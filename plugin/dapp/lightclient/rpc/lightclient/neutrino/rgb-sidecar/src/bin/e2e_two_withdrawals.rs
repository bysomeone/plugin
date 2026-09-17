//! Regression E2E: two **consecutive** RGB20 withdrawals on regtest btcd.
//!
//! The chain33 bridge was not needed: the "TSS" is a plain test key, so the PSBTs are signed
//! locally with the same key the watch-only sidecar watches. This isolates the property under
//! test - a withdrawal's change seal must be usable as the input of the *next* withdrawal:
//!
//!   1. fund the single-script TSS address on btcd (no wallet: spend a mature coinbase)
//!   2. issue 100 USDT at a TSS UTXO
//!   3. deposit: transfer 1.0 USDT to a receive invoice, settle it
//!   4. withdrawal #1: 0.5 USDT -> user invoice; the change seal (0.5) must be recorded
//!   5. withdrawal #2: 0.5 USDT -> user invoice; it must select the change seal of #1
//!
//! Without the fix the withdrawal path never merges its own RGB transition into the Stock, so
//! step 5 fails in `build_transfer` with
//! `outpoint <txid>:2 has no rgb:... state`.
//!
//! Env: `RGB_BITCOIND_RPC` (default 127.0.0.1:18443), `RGB_BITCOIND_USER`/`_PASS`,
//! `RGB_BITCOIND_CERT` (PEM path => TLS, required for btcd), `BTC_FUNDING_WIF` (mining key WIF),
//! `RGB_SIDECAR_DATA_DIR` (default /tmp/rgb-spike/sidecar-two-withdrawals).

use std::str::FromStr;
use std::sync::Arc;

use anyhow::{anyhow, Result};
use bitcoin::absolute::LockTime;
use bitcoin::key::PrivateKey;
use bitcoin::script::PushBytesBuf;
use bitcoin::sighash::{EcdsaSighashType, SighashCache};
use bitcoin::{Address, CompressedPublicKey, Network, OutPoint, Psbt, ScriptBuf, Sequence,
             Transaction, TxIn, TxOut, Witness};
use rgb_sidecar::config::Config;
use rgb_sidecar::engine::RgbEngine;
use rgb_sidecar::invoice::build_address_invoice;
use rgb_sidecar::rpc::BtcdRpc;
use rgbstd::contract::IssuerWrapper;
use schemata::InflatableFungibleAsset;

const TSS_SECRET: [u8; 32] = [0x11; 32];
const USER_SECRET: [u8; 32] = [0x22; 32];
const COINBASE_MATURITY_CONFS: i64 = 101;
const FUND_AMOUNT_SAT: u64 = 1_000_000_000; // 10.0 BTC
const FUND_FEE_SAT: u64 = 2_000;
/// Fund the TSS again unless it already holds a UTXO at least this large (the genesis seal must
/// cover the deposit's dust output and fee); keeps repeated runs on a shared regtest chain
/// deterministic instead of reusing small change left by the previous run.
const TSS_MIN_FUNDING_SAT: u64 = 100_000_000; // 1.0 BTC
const DEPOSIT_AMOUNT: i64 = 100_000_000; // 1.0 USDT
const WITHDRAW_AMOUNT: i64 = 50_000_000; // 0.5 USDT

fn env_or(key: &str, default: &str) -> String {
    std::env::var(key).unwrap_or_else(|_| default.to_string())
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

/// Sign a PSBT with an external private key (stands in for the chain33 TSS group). Every input
/// must be a P2WPKH output of this key.
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
        let sh = cache.p2wpkh_signature_hash(i, &txout.script_pubkey, txout.value, EcdsaSighashType::All)?;
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

/// Fund `dest_script` from a mature coinbase paying the fixed regtest mining key. btcd has no
/// wallet, so the coinbase is spent with a raw signed tx (same approach as `issue_usdt`).
fn fund_address(rpc: &BtcdRpc, mining_wif: &str, dest_script: &ScriptBuf) -> Result<String> {
    let secp = bitcoin::secp256k1::Secp256k1::new();
    let key = PrivateKey::from_wif(mining_wif).map_err(|e| anyhow!("bad BTC_FUNDING_WIF: {e}"))?;
    let pk = key.public_key(&secp);
    let miner_script = Address::p2pkh(&pk, Network::Regtest).script_pubkey();

    let best = rpc.get_block_count()?;
    if best < COINBASE_MATURITY_CONFS as u64 {
        let need = COINBASE_MATURITY_CONFS as u64 + 4 - best;
        println!("mining {need} warm-up blocks (best={best})");
        rpc.mine_blocks(need as u32)?;
    }
    let txs = rpc.search_txs_for_script(&miner_script)?;
    let mut chosen: Option<(OutPoint, u64)> = None;
    'outer: for t in &txs {
        if t.confirmations < COINBASE_MATURITY_CONFS {
            continue;
        }
        for (vout, o) in &t.outputs {
            if o.value > FUND_AMOUNT_SAT + FUND_FEE_SAT {
                let outpoint = OutPoint { txid: t.txid, vout: *vout };
                if rpc.get_txout(&outpoint)?.is_some() {
                    chosen = Some((outpoint, o.value));
                    break 'outer;
                }
            }
        }
    }
    let (prevout, prev_value) = chosen
        .ok_or_else(|| anyhow!("no mature coinbase to the mining key; check BTC_FUNDING_WIF"))?;

    let unsigned = Transaction {
        version: bitcoin::transaction::Version::TWO,
        lock_time: LockTime::ZERO,
        input: vec![TxIn {
            previous_output: prevout,
            script_sig: ScriptBuf::new(),
            sequence: Sequence::MAX,
            witness: Witness::new(),
        }],
        output: vec![
            TxOut {
                value: bitcoin::Amount::from_sat(FUND_AMOUNT_SAT),
                script_pubkey: dest_script.clone(),
            },
            TxOut {
                value: bitcoin::Amount::from_sat(prev_value - FUND_AMOUNT_SAT - FUND_FEE_SAT),
                script_pubkey: miner_script.clone(),
            },
        ],
    };
    let sighash = SighashCache::new(&unsigned)
        .legacy_signature_hash(0, &miner_script, EcdsaSighashType::All as u32)?;
    let msg: bitcoin::secp256k1::Message = sighash.into();
    let sig = secp.sign_ecdsa(&msg, &key.inner);
    let mut sig_der = sig.serialize_der().to_vec();
    sig_der.push(EcdsaSighashType::All as u8);
    let mut tx = unsigned;
    tx.input[0].script_sig = ScriptBuf::builder()
        .push_slice(PushBytesBuf::try_from(sig_der)?)
        .push_key(&pk)
        .into_script();
    let txid = rpc.send_raw_transaction(&tx)?;
    rpc.mine_blocks(1)?;
    Ok(txid.to_string())
}

fn main() -> Result<()> {
    let data_dir = env_or("RGB_SIDECAR_DATA_DIR", "/tmp/rgb-spike/sidecar-two-withdrawals");
    std::fs::remove_dir_all(&data_dir).ok();

    let btc_host = env_or("RGB_BITCOIND_RPC", "127.0.0.1:18443");
    let btc_user = env_or("RGB_BITCOIND_USER", "root");
    let btc_pass = env_or("RGB_BITCOIND_PASS", "1314");
    let btc_cert = env_or("RGB_BITCOIND_CERT", "");
    let cert_path = (!btc_cert.is_empty()).then(|| std::path::PathBuf::from(&btc_cert));

    let tss_pubkey_hex = pubkey_hex(&TSS_SECRET)?;
    let cfg = Config {
        data_dir: data_dir.clone().into(),
        btc_rpc_host: btc_host.clone(),
        btc_rpc_user: btc_user.clone(),
        btc_rpc_pass: btc_pass.clone(),
        btc_rpc_cert: cert_path.clone(),
        network: Network::Regtest,
        tss_pubkey_hex,
        grpc_listen: "0.0.0.0:0".into(),
    };
    let rpc = Arc::new(BtcdRpc::connect(
        &btc_host,
        &btc_user,
        &btc_pass,
        cert_path.as_deref(),
        Network::Regtest,
    )?);
    let mut engine = RgbEngine::open(cfg)?;

    // btcd's regtest activates segwit through BIP9 (MinerConfirmationWindow=144,
    // RuleChangeActivationThreshold=108). Every TSS output is P2WPKH, and until the deployment
    // reports `active` btcd rejects the spends with
    // "has witness data, but segwit isn't active yet". Measured: still `lockedin` at height 290,
    // `active` at 490, so mine well past the tallest candidate window boundary (432).
    const SEGWIT_ACTIVE_HEIGHT: u64 = 500;
    let best = rpc.get_block_count()?;
    if best < SEGWIT_ACTIVE_HEIGHT {
        let need = SEGWIT_ACTIVE_HEIGHT - best;
        println!("mining {need} blocks so segwit activates (best={best})");
        rpc.mine_blocks(need as u32)?;
    }

    // 1. Fund the TSS address (btcd has no wallet: spend a mature coinbase from the fixed
    //    mining key).
    let tss_addr = engine.tss_address().to_string();
    let tss_script = engine.tss_script().clone();
    let funding_wif = env_or("BTC_FUNDING_WIF", "");
    if funding_wif.is_empty() {
        return Err(anyhow!("BTC_FUNDING_WIF env required to fund the TSS address on btcd"));
    }
    engine.sync()?;
    // Fund unless the TSS already holds a coin big enough to be the genesis seal *and* to carry
    // the deposit's BTC output: dust change left behind by an earlier run must not be reused as
    // the genesis seal.
    let largest = engine.list_unspent_btc()?.iter().map(|u| u.value).max().unwrap_or(0);
    if largest < TSS_MIN_FUNDING_SAT {
        let fund_txid = fund_address(&rpc, &funding_wif, &tss_script)?;
        println!("funded TSS={tss_addr} txid={fund_txid} (largest utxo was {largest})");
        engine.sync()?;
    }

    // 2. Issue 100 USDT.
    let asset = engine.issue_asset("USDT", "Tether USD", 8, 10_000_000_000)?;
    println!("issued USDT asset_id={}", asset.asset_id);

    // 3. Deposit: the "user" pays 1.0 USDT to a receive invoice (TSS script).
    let user_addr = Address::p2wpkh(
        &CompressedPublicKey::from_slice(&hex_decode(&pubkey_hex(&USER_SECRET)?)?)?,
        Network::Regtest,
    );
    let (invoice, receive_id) = engine.create_receive("USDT", DEPOSIT_AMOUNT)?;
    let genesis = engine
        .ledger
        .seals
        .values()
        .filter(|s| s.asset_symbol == "USDT")
        .max_by_key(|s| s.amount)
        .cloned()
        .ok_or_else(|| anyhow!("no genesis seal"))?;
    let genesis_outpoint = OutPoint::from_str(&genesis.outpoint)?;
    let pay = engine.build_transfer(
        "USDT",
        &[genesis_outpoint],
        &invoice,
        &user_addr.script_pubkey(),
        2,
        100_000,
    )?;
    let signed_pay = sign_psbt(&pay.psbt, &TSS_SECRET)?;
    engine.broadcast(&signed_pay.clone().extract_tx()?)?;
    rpc.mine_blocks(1)?;
    engine.wait_tx(pay.txid, std::time::Duration::from_secs(15))?;
    let rec = engine.provide_consignment(&pay.consignment, Some(&receive_id))?;
    assert_eq!(rec.status, "settled", "deposit did not settle");
    engine.sync()?;
    let (settled, _) = engine.get_balance("USDT");
    assert_eq!(settled, DEPOSIT_AMOUNT, "unexpected balance after deposit");
    println!("deposit settled: {receive_id} balance={settled}");

    // 4/5. Two consecutive withdrawals of 0.5 USDT each: the second one must spend the change
    //      seal opened by the first.
    let user_invoice = build_address_invoice(
        Network::Regtest,
        &user_addr.script_pubkey(),
        asset.asset_id.parse()?,
        InflatableFungibleAsset::schema().schema_id(),
        WITHDRAW_AMOUNT as u64,
    )?;

    let mut prev_change: Option<String> = None;
    let mut first_signed: Option<Psbt> = None;
    let mut first_result: Option<(String, String, Option<String>)> = None;
    for round in 1..=2u32 {
        let before = engine.get_balance("USDT").0;
        let w = engine.build_withdrawal("USDT", WITHDRAW_AMOUNT, &user_invoice, &tss_addr, 2, &[])?;
        let inputs: Vec<String> = w
            .psbt
            .unsigned_tx
            .input
            .iter()
            .map(|i| i.previous_output.to_string())
            .collect();
        println!("withdrawal #{round}: txid={} inputs={inputs:?}", w.txid);

        if round == 2 {
            let want = prev_change
                .clone()
                .expect("withdrawal #1 produced no change seal");
            assert!(
                inputs.contains(&want),
                "withdrawal #2 did not spend the change seal {want} of withdrawal #1 (inputs={inputs:?})"
            );
        }

        let signed_w = sign_psbt(&w.psbt, &TSS_SECRET)?;
        engine.broadcast(&signed_w.clone().extract_tx()?)?;
        rpc.mine_blocks(1)?;
        engine.wait_tx(w.txid, std::time::Duration::from_secs(15))?;
        let (txid, recip, change) = engine.finalize_withdrawal(&signed_w)?;
        assert_eq!(txid, w.txid, "finalize txid mismatch");
        if round == 1 {
            first_signed = Some(signed_w.clone());
            first_result = Some((txid.to_string(), recip.clone(), change.clone()));
        }

        engine.sync()?;
        let after = engine.get_balance("USDT").0;
        println!("withdrawal #{round} finalized: change={change:?} balance {before} -> {after}");
        assert_eq!(after, before - WITHDRAW_AMOUNT, "unexpected balance after withdrawal");

        prev_change = change;
        if round == 1 {
            assert!(
                prev_change.is_some(),
                "withdrawal #1 must leave a change seal to spend in #2"
            );
        }
    }

    // Re-finalizing an already finalized withdrawal must be a no-op returning the same values
    // (the bridge may retry), not a second application of the transition.
    if let (Some(psbt), Some(want)) = (first_signed, first_result) {
        let again = engine.finalize_withdrawal(&psbt)?;
        let got = (again.0.to_string(), again.1.clone(), again.2.clone());
        assert_eq!(got, want, "repeated finalize must be idempotent");
        println!("repeated finalize is idempotent: {got:?}");
    }

    let (settled_final, _) = engine.get_balance("USDT");
    assert_eq!(settled_final, DEPOSIT_AMOUNT - 2 * WITHDRAW_AMOUNT);
    println!(
        "E2E-TWO-WITHDRAWALS-PASS: change seal of withdrawal #1 spent by withdrawal #2 (final balance={settled_final})"
    );
    Ok(())
}

fn hex_decode(s: &str) -> Result<Vec<u8>> {
    let mut out = Vec::with_capacity(s.len() / 2);
    for i in (0..s.len()).step_by(2) {
        out.push(u8::from_str_radix(&s[i..i + 2], 16)?);
    }
    Ok(out)
}
