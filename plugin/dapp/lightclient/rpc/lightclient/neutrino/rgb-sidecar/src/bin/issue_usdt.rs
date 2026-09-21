//! One-shot helper: fund the TSS address and issue RGB20 USDT into a sidecar
//! data dir (env `RGB_SIDECAR_DATA_DIR`, default `/tmp/rgb-spike/sidecar-interop`),
//! so a fresh gRPC sidecar started on that dir has a real asset for Go<->Rust interop.
//!
//! - TSS pubkey: env `RGB_SIDECAR_TSS_PUBKEY` (chain33 GG18 threshold pubkey), falls back to a
//!   dev test key.
//! - btcd RPC: env `RGB_BITCOIND_RPC` (`host:port`) / `RGB_BITCOIND_USER` / `RGB_BITCOIND_PASS` /
//!   `RGB_BITCOIND_CERT` (PEM path => HTTPS, btcd self-signed). The sidecar reads the SAME btcd
//!   node, so the genesis seal is on the same chain the bridge's neutrino syncs.
//! - Funding on btcd (no wallet): the test harness mines to the fixed mining key and passes its WIF
//!   via `BTC_FUNDING_WIF`; this helper spends a mature mining-address coinbase to the TSS address
//!   with a raw signed tx, then mines to confirm.

use std::path::PathBuf;
use std::sync::Arc;

use anyhow::{anyhow, Result};
use bitcoin::absolute::LockTime;
use bitcoin::key::PrivateKey;
use bitcoin::script::PushBytesBuf;
use bitcoin::sighash::{EcdsaSighashType, SighashCache};
use bitcoin::{Address, Network, OutPoint, ScriptBuf, Sequence, Transaction, TxIn, TxOut, Witness};
use rgb_sidecar::config::Config;
use rgb_sidecar::engine::RgbEngine;
use rgb_sidecar::rpc::BtcdRpc;

const TSS_SECRET: [u8; 32] = [0x11; 32];
const COINBASE_MATURITY_CONFS: i64 = 101; // regtest coinbase spendable after 100 blocks on top
const FUND_AMOUNT_SAT: u64 = 1_000_000_000; // 10.0 BTC to the TSS address
const FUND_FEE_SAT: u64 = 2_000;

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

/// Ensure the chain is deep enough to contain a mature coinbase to the mining key, then spend one
/// to `dest_script`. Returns the funding txid.
fn fund_tss_address(
    rpc: &BtcdRpc,
    mining_wif: &str,
    dest_script: &ScriptBuf,
) -> Result<String> {
    let secp = bitcoin::secp256k1::Secp256k1::new();
    let key = PrivateKey::from_wif(mining_wif).map_err(|e| anyhow!("bad BTC_FUNDING_WIF: {e}"))?;
    // The test harness mines regtest to the key derived from privHex=...0001 (btcctl generate);
    // that WIF must be the one passed via BTC_FUNDING_WIF or no coinbase is spendable.
    let pk = key.public_key(&secp);
    let miner_addr = Address::p2pkh(&pk, Network::Regtest);
    let miner_script = miner_addr.script_pubkey();

    // Find a mature coinbase output paying the mining key; warm the chain up first if needed.
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
                // Skip already-spent coinbases (a previous run may have used them).
                if rpc.get_txout(&outpoint)?.is_some() {
                    chosen = Some((outpoint, o.value));
                    break 'outer;
                }
            }
        }
    }
    let (prevout, prev_value) = chosen.ok_or_else(|| {
        anyhow!(
            "no mature coinbase to mining key after warm-up; check --miningaddr matches BTC_FUNDING_WIF"
        )
    })?;

    // Build 1-in/2-out funding tx: coinbase -> TSS + change back to the miner.
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
        .legacy_signature_hash(0, &miner_script, EcdsaSighashType::All as u32)
        .map_err(|e| anyhow!("sighash: {e}"))?;
    let msg: bitcoin::secp256k1::Message = sighash.into();
    let sig = secp.sign_ecdsa(&msg, &key.inner);
    let mut sig_der = sig.serialize_der().to_vec();
    sig_der.push(EcdsaSighashType::All as u8);
    let script_sig = ScriptBuf::builder()
        .push_slice(PushBytesBuf::try_from(sig_der)?)
        .push_key(&pk)
        .into_script();
    let mut tx = unsigned;
    tx.input[0].script_sig = script_sig;

    let txid = rpc.send_raw_transaction(&tx)?;
    rpc.mine_blocks(1)?;
    Ok(txid.to_string())
}

#[tokio::main]
async fn main() -> Result<()> {
    let data_dir = env_or("RGB_SIDECAR_DATA_DIR", "/tmp/rgb-spike/sidecar-interop");
    // Given GG18 pubkey else dev default test key.
    let tss_pubkey_hex = env_or("RGB_SIDECAR_TSS_PUBKEY", "");
    let tss_pubkey_hex = if tss_pubkey_hex.is_empty() {
        pubkey_hex(&TSS_SECRET)?
    } else {
        tss_pubkey_hex
    };

    // btcd endpoint (host:port). TLS when the cert path is set.
    let btc_host = env_or("RGB_BITCOIND_RPC", "127.0.0.1:18443");
    let btc_user = env_or("RGB_BITCOIND_USER", "root");
    let btc_pass = env_or("RGB_BITCOIND_PASS", "1314");
    let btc_cert = env_or("RGB_BITCOIND_CERT", "");
    let cert_path = if btc_cert.is_empty() {
        None
    } else {
        Some(PathBuf::from(btc_cert))
    };

    let cfg = Config {
        data_dir: data_dir.clone().into(),
        btc_rpc_host: btc_host.clone(),
        btc_rpc_user: btc_user.clone(),
        btc_rpc_pass: btc_pass.clone(),
        btc_rpc_cert: cert_path.clone(),
        network: Network::Regtest,
        tss_pubkey_hex: tss_pubkey_hex.clone(),
        grpc_listen: "0.0.0.0:0".into(),
        contracts: Vec::new(),
    };
    let rpc = Arc::new(BtcdRpc::connect(
        &btc_host,
        &btc_user,
        &btc_pass,
        cert_path.as_deref(),
        Network::Regtest,
    )?);
    let mut engine = RgbEngine::open(cfg)?;

    let tss_addr = engine.tss_address().to_string();
    let tss_script = engine.tss_script().clone();
    println!("TSS address: {tss_addr}");

    let funding_wif = env_or("BTC_FUNDING_WIF", "");
    if funding_wif.is_empty() {
        return Err(anyhow!("BTC_FUNDING_WIF env required to fund the TSS address on btcd"));
    }
    engine.sync()?;
    let unspents = engine.list_unspent_btc()?;
    if unspents.is_empty() {
        let fund_txid = fund_tss_address(&rpc, &funding_wif, &tss_script)?;
        println!("funded TSS address, txid={fund_txid}");
        engine.sync()?;
        println!("TSS BTC unspents after fund: {}", engine.list_unspent_btc()?.len());
    } else {
        println!("TSS already funded: {} unspent UTXO(s)", unspents.len());
    }

    if engine.ledger.asset("USDT").is_some() {
        println!("USDT already issued, skipping");
    } else {
        let asset = engine.issue_asset("USDT", "Tether USD", 8, 10_000_000_000)?; // 100.0 USDT
        println!("issued USDT asset_id={}", asset.asset_id);
    }
    engine.save()?;
    let assets = engine.list_assets();
    for a in &assets {
        println!(
            "asset: symbol={} precision={} issued={}",
            a.symbol, a.precision, a.issued_supply
        );
    }
    let seals = engine.list_seals("USDT");
    for s in &seals {
        println!(
            "seal: outpoint={} amount={} status={:?}",
            s.outpoint, s.amount, s.status
        );
    }
    println!("ISSUE-DONE ledger={data_dir}");
    Ok(())
}
