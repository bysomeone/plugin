//! Phase 2a standalone regtest E2E.
//!
//! Flow (the "user" is simulated by the same sidecar, using a second test key for its change):
//!   1. bridge funds its single-script TSS address + a "user" address, mines
//!   2. bridge issues RGB20 (IFA) USDT (100) at a TSS-script UTXO
//!   3. CreateReceive(1 USDT) -> address-based invoice at the TSS script
//!   4. simulated user pays: build_transfer(genesis 100 -> invoice 1, change 99 -> user script),
//!      sign (external test key), broadcast, mine
//!   5. ProvideConsignment -> validate + settle + attribute to the receive
//!   6. Sync -> receive seal minted; bridge balance = 1 USDT
//!   7. BuildWithdrawal(0.5 USDT -> user invoice) -> unsigned PSBT + consignment
//!   8. sign PSBT externally (the TSS seam), broadcast, mine
//!   9. FinalizeWithdrawal -> input seal consumed, change seal (0.5) recorded
//!   10. Sync -> bridge balance = 0.5 USDT
//!
//! Requires bitcoind regtest + electrs (see spike/regtest_env.sh).

use std::process::Command;
use std::str::FromStr;

use anyhow::{anyhow, Result};
use bitcoin::sighash::{EcdsaSighashType, SighashCache};
use bitcoin::{Address, CompressedPublicKey, Network, OutPoint, Psbt, ScriptBuf, Witness};
use rgb_sidecar::config::Config;
use rgb_sidecar::engine::RgbEngine;
use rgb_sidecar::invoice::build_address_invoice;
use rgbstd::contract::IssuerWrapper;
use schemata::InflatableFungibleAsset;

const ELECTRUM: &str = "127.0.0.1:60401";
const DATA_DIR: &str = "/tmp/rgb-spike/sidecar-e2e";
const TSS_SECRET: [u8; 32] = [0x11; 32];
const USER_SECRET: [u8; 32] = [0x22; 32];

fn bitcoin_cli(args: &[&str]) -> Result<String> {
    let out = Command::new("/opt/bitcoin-27.2/bin/bitcoin-cli")
        .arg("-datadir=/tmp/rgb-spike/bitcoin")
        .arg("-rpcport=18443")
        .arg("-regtest")
        .args(args)
        .output()
        .map_err(|e| anyhow!("bitcoin-cli spawn: {e}"))?;
    if !out.status.success() {
        return Err(anyhow!(
            "bitcoin-cli {} failed: {}",
            args.join(" "),
            String::from_utf8_lossy(&out.stderr)
        ));
    }
    Ok(String::from_utf8_lossy(&out.stdout).trim().to_string())
}

fn mine_blocks(n: u32) -> Result<()> {
    let addr = bitcoin_cli(&["-rpcwallet=spike", "getnewaddress"])?;
    bitcoin_cli(&["generatetoaddress", &n.to_string(), &addr])?;
    Ok(())
}

fn fund(address: &str, btc: f64) -> Result<()> {
    bitcoin_cli(&["-rpcwallet=spike", "sendtoaddress", address, &btc.to_string()])?;
    mine_blocks(1)?;
    Ok(())
}

fn pubkey_hex(secret: &[u8; 32]) -> Result<String> {
    let secp = bitcoin::secp256k1::Secp256k1::new();
    let sk = bitcoin::secp256k1::SecretKey::from_slice(secret)?;
    let pk = bitcoin::secp256k1::PublicKey::from_secret_key(&secp, &sk);
    Ok(bitcoin::hex::DisplayHex::to_hex_string(
        &pk.serialize(),
        bitcoin::hex::Case::Lower,
    ))
}

/// Sign a PSBT with an external private key (stands in for TSS). Every input must be a
/// P2WPKH output of this key.
fn sign_psbt(psbt: &Psbt, secret: &[u8; 32]) -> Result<Psbt> {
    let secp = bitcoin::secp256k1::Secp256k1::new();
    let sk = bitcoin::secp256k1::SecretKey::from_slice(secret)?;
    let pk = bitcoin::secp256k1::PublicKey::from_secret_key(&secp, &sk);
    let mut psbt = psbt.clone();
    for i in 0..psbt.inputs.len() {
        let txout = psbt.inputs[i]
            .witness_utxo
            .clone()
            .ok_or_else(|| anyhow!("input {i}: no witness_utxo"))?;
        let mut cache = SighashCache::new(&psbt.unsigned_tx);
        let sh = cache.p2wpkh_signature_hash(
            i,
            &txout.script_pubkey,
            txout.value,
            EcdsaSighashType::All,
        )?;
        let msg = bitcoin::secp256k1::Message::from(sh);
        let sig = secp.sign_ecdsa(&msg, &sk);
        let btc_sig = bitcoin::ecdsa::Signature::sighash_all(sig);
        psbt.inputs[i]
            .partial_sigs
            .insert(bitcoin::PublicKey::new(pk), btc_sig);
        let mut w = Witness::new();
        w.push(btc_sig.to_vec());
        w.push(pk.serialize());
        psbt.inputs[i].final_script_witness = Some(w);
        psbt.inputs[i].final_script_sig = Some(ScriptBuf::new());
    }
    Ok(psbt)
}

#[tokio::main]
async fn main() -> Result<()> {
    std::fs::remove_dir_all(DATA_DIR).ok();

    let tss_pubkey_hex = pubkey_hex(&TSS_SECRET)?;
    let user_pubkey_hex = pubkey_hex(&USER_SECRET)?;

    let cfg = Config {
        data_dir: DATA_DIR.into(),
        electrum_url: ELECTRUM.into(),
        network: Network::Regtest,
        tss_pubkey_hex,
        grpc_listen: "0.0.0.0:0".into(),
    };
    let mut engine = RgbEngine::open(cfg)?;

    // 1. Fund bridge (TSS) + user addresses.
    let tss_addr = engine.tss_address().to_string();
    fund(&tss_addr, 10.0)?;
    let user_addr = Address::p2wpkh(
        &CompressedPublicKey::from_slice(&hex_decode(&user_pubkey_hex)?)?,
        Network::Regtest,
    );
    fund(&user_addr.to_string(), 5.0)?;
    engine.sync()?;
    println!("E2E: funded TSS={tss_addr} user={user_addr}");

    // 2. Issue 100 USDT at the first TSS UTXO.
    let asset = engine.issue_asset("USDT", "Tether USD", 8, 10_000_000_000)?; // 100.0 USDT
    let genesis = engine
        .ledger
        .seals
        .values()
        .find(|s| s.asset_symbol == "USDT")
        .cloned()
        .ok_or_else(|| anyhow!("no genesis seal"))?;
    let genesis_outpoint = OutPoint::from_str(&genesis.outpoint)?;
    println!("E2E: issued USDT {} genesis_seal={}", asset.asset_id, genesis.outpoint);

    // 3. CreateReceive 1 USDT.
    let (invoice, receive_id) = engine.create_receive("USDT", 100_000_000)?; // 1.0 USDT
    println!("E2E: create_receive {receive_id}");

    // 4. Simulated user pays: spend genesis (100) -> 1 to invoice, 99 change to user script.
    //    The user sends 100_000 sat with the RGB output (so the bridge can later fund fees).
    let outcome = engine.build_transfer(
        "USDT",
        &[genesis_outpoint],
        &invoice,
        &user_addr.script_pubkey(),
        2,
        100_000,
    )?;
    let signed = sign_psbt(&outcome.psbt, &TSS_SECRET)?;
    let tx = signed.clone().extract_tx()?;
    engine.broadcast(&tx)?;
    mine_blocks(1)?;
    engine.wait_tx(outcome.txid, std::time::Duration::from_secs(15))?;
    println!("E2E: user paid, txid={}", outcome.txid);

    // 5. ProvideConsignment -> settle + attribute.
    let rec = engine.provide_consignment(&outcome.consignment, Some(&receive_id))?;
    println!("E2E: provide_consignment -> status={} amount={}", rec.status, rec.settled_amount);
    assert_eq!(rec.status, "settled");
    assert_eq!(rec.settled_amount, 100_000_000);

    // 6. Sync -> receive seal minted.
    engine.sync()?;
    let (settled, pending) = engine.get_balance("USDT");
    println!("E2E: balance after deposit settled={settled} pending={pending}");
    assert_eq!(settled, 100_000_000);

    // 7. BuildWithdrawal 0.5 USDT to the user invoice (user's address).
    let user_invoice = build_address_invoice(
        Network::Regtest,
        &user_addr.script_pubkey(),
        asset.asset_id.parse()?,
        InflatableFungibleAsset::schema().schema_id(),
        50_000_000, // 0.5 USDT
    )?;
    let w = engine.build_withdrawal("USDT", 50_000_000, &user_invoice, &tss_addr, 2)?;
    println!("E2E: build_withdrawal txid={} inputs={}", w.txid, w.input_amounts.len());

    // 8. Sign (external test key = TSS seam), broadcast, mine.
    let signed_w = sign_psbt(&w.psbt, &TSS_SECRET)?;
    let tx_w = signed_w.clone().extract_tx()?;
    engine.broadcast(&tx_w)?;
    mine_blocks(1)?;
    engine.wait_tx(w.txid, std::time::Duration::from_secs(15))?;

    // 9. FinalizeWithdrawal.
    let (txid, recip, change) = engine.finalize_withdrawal(&signed_w)?;
    println!("E2E: finalize txid={txid} recipient={recip} change={change:?}");
    assert_eq!(txid.to_string(), w.txid.to_string());

    // 10. Sync -> change seal minted; balance 0.5.
    engine.sync()?;
    let (settled2, pending2) = engine.get_balance("USDT");
    println!("E2E: balance after withdrawal settled={settled2} pending={pending2}");
    assert_eq!(settled2, 50_000_000);

    // Bonus: list seals.
    let seals = engine.list_seals("USDT");
    println!("E2E: seals:");
    for s in &seals {
        println!(
            "  {} amount={} status={}",
            s.outpoint,
            s.amount,
            s.status.as_str()
        );
    }

    println!("E2E-PASS: deposit (issue->receive->settle->attribute) + withdrawal (build->sign->finalize) OK");
    Ok(())
}

fn hex_decode(s: &str) -> Result<Vec<u8>> {
    let mut out = Vec::with_capacity(s.len() / 2);
    for i in (0..s.len()).step_by(2) {
        out.push(u8::from_str_radix(&s[i..i + 2], 16)?);
    }
    Ok(out)
}
