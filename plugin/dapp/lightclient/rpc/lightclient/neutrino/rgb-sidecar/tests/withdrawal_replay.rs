//! E9-A: an RGB20 withdrawal whose broadcast failed must be **replayed**, not rebuilt.
//!
//! The bridge's retry path rebuilds the withdrawal. By then the sidecar ledger has advanced
//! (first attempt's seal → `Consumed`, its change seal → `Minted`), so selecting seals by status
//! picks a *different* set and produces a second, independently valid transfer: the user is paid
//! twice for one on-chain burn while the bridge settles the burn once.
//!
//! `BuildWithdrawalRequest.input_seals` closes that hole: the bridge sends back the seals the first
//! attempt spent, the sidecar spends exactly those (status filter bypassed, state still verified)
//! and reuses the recorded input list, so the retry reproduces the very same transaction — same
//! txid — i.e. an idempotent replay.
//!
//! The test drives a real engine against a **mock btcd JSON-RPC node** (no bitcoind/btcd needed),
//! and pins, in order:
//!   1. build + finalize a withdrawal (seal = a dust-ish seal, plus a bridge-owned fee input);
//!   2. the accident: the withdrawal lands on chain, the ledger advances, *and* a new, larger
//!      bridge UTXO appears (so fee-input re-selection would differ too);
//!   3. a naive rebuild picks another seal and another fee input ⇒ a different txid (the hazard);
//!   4. after a **sidecar restart**, the replay build (input_seals = the recorded seals) produces
//!      the identical txid and the identical input list;
//!   5. re-finalizing the replay stays idempotent across the restart (the finalized record is
//!      persisted in the ledger) — it must not fail with "no pending withdrawal" nor re-apply the
//!      transition, either of which would leave the on-chain burn stuck forever;
//!   6. with the record gone, the same seals still rebuild into a *different* transaction — the
//!      reason the replay re-issues the recorded PSBT instead of re-building it;
//!   7. the seals are not trusted: an outpoint that carries no RGB state is rejected.

use std::collections::HashMap;
use std::io::{BufRead, BufReader, Read, Write};
use std::net::{TcpListener, TcpStream};
use std::str::FromStr;
use std::sync::{Arc, Mutex};

use anyhow::{anyhow, Result};
use bitcoin::absolute::LockTime;
use bitcoin::consensus::encode;
use bitcoin::hex::{Case, DisplayHex};
use bitcoin::sighash::{EcdsaSighashType, SighashCache};
use bitcoin::{
    Address, CompressedPublicKey, Network, OutPoint, Psbt, ScriptBuf, Sequence, Transaction, TxIn,
    TxOut, Witness,
};
use rgb_sidecar::config::Config;
use rgb_sidecar::engine::RgbEngine;
use rgb_sidecar::invoice::build_address_invoice;
use rgbstd::contract::IssuerWrapper;
use schemata::InflatableFungibleAsset;
use serde_json::{json, Value};

const TSS_SECRET: [u8; 32] = [0x11; 32];
const USER_SECRET: [u8; 32] = [0x22; 32];
const SYMBOL: &str = "USDT";
const ISSUED: i64 = 100_000_000;
const WITHDRAW: i64 = 40_000_000;
const FEE_RATE: u64 = 2;
/// The bridge's RGB seal: dust-ish on purpose, so the carrier tx needs a bridge-owned fee input.
const SEAL_BTC: u64 = 700; // below recipient dust + fee ⇒ the carrier tx needs a fee input
/// Bridge-owned BTC fee UTXO used by the first build.
const FEE_UTXO_BTC: u64 = 200_000;
/// A larger bridge UTXO that appears *after* the first build: re-selecting fee inputs would now
/// prefer this one, which is exactly why the replay must pin the recorded input list.
const NEW_UTXO_BTC: u64 = 300_000;

// =====================================================================
// Mock btcd JSON-RPC node
// =====================================================================

#[derive(Default)]
struct MockChain {
    height: u64,
    /// txid -> serialized tx (hex served by `getrawtransaction`).
    txs: HashMap<String, Transaction>,
    /// live (unspent) UTXOs.
    utxos: HashMap<OutPoint, (u64, ScriptBuf)>,
}

impl MockChain {
    fn add_tx(&mut self, tx: Transaction) {
        let txid = tx.compute_txid().to_string();
        for (vout, out) in tx.output.iter().enumerate() {
            self.utxos
                .insert(OutPoint { txid: tx.compute_txid(), vout: vout as u32 }, (out.value.to_sat(), out.script_pubkey.clone()));
        }
        self.txs.insert(txid, tx);
    }

    fn spend(&mut self, outpoint: OutPoint) {
        self.utxos.remove(&outpoint);
    }
}

fn to_hex(bytes: &[u8]) -> String {
    bytes.to_hex_string(Case::Lower)
}

fn handle_request(chain: &Arc<Mutex<MockChain>>, req: &Value) -> Value {
    let method = req.get("method").and_then(|m| m.as_str()).unwrap_or("");
    let params = req.get("params").cloned().unwrap_or_else(|| json!([]));
    let id = req.get("id").cloned().unwrap_or_else(|| json!(1));
    let (result, error): (Value, Value) = match method {
        "getblockcount" => (json!(chain.lock().unwrap().height), Value::Null),
        "getrawtransaction" => {
            let txid = params.get(0).and_then(|v| v.as_str()).unwrap_or("");
            match chain.lock().unwrap().txs.get(txid) {
                Some(tx) => (json!(encode::serialize_hex(tx)), Value::Null),
                None => (
                    Value::Null,
                    json!({"code": -5, "message": "No such mempool or blockchain transaction"}),
                ),
            }
        }
        "gettxout" => {
            let txid = params.get(0).and_then(|v| v.as_str()).unwrap_or("");
            let vout = params.get(1).and_then(|v| v.as_u64()).unwrap_or(0) as u32;
            let op = bitcoin::Txid::from_str(txid).ok().map(|txid| OutPoint { txid, vout });
            match op.and_then(|op| chain.lock().unwrap().utxos.get(&op).cloned()) {
                Some((value, script)) => (
                    json!({
                        "value": value as f64 / 100_000_000.0,
                        "scriptPubKey": {"hex": to_hex(script.as_bytes())},
                        "confirmations": 100,
                    }),
                    Value::Null,
                ),
                // Unknown/spent: `gettxout` returns null (not an error).
                None => (Value::Null, Value::Null),
            }
        }
        "searchrawtransactions" => {
            let addr = params.get(0).and_then(|v| v.as_str()).unwrap_or("");
            let script = Address::from_str(addr)
                .ok()
                .and_then(|a| a.require_network(Network::Regtest).ok())
                .map(|a| a.script_pubkey());
            let chain = chain.lock().unwrap();
            let mut out = Vec::new();
            for (txid, tx) in chain.txs.iter() {
                let Some(script) = script.as_ref() else { continue };
                let outputs: Vec<Value> = tx
                    .output
                    .iter()
                    .enumerate()
                    .filter(|(_, o)| &o.script_pubkey == script)
                    .map(|(n, o)| {
                        json!({
                            "n": n,
                            "value": o.value.to_sat() as f64 / 100_000_000.0,
                            "scriptPubKey": {"hex": to_hex(o.script_pubkey.as_bytes())},
                        })
                    })
                    .collect();
                if outputs.is_empty() {
                    continue;
                }
                out.push(json!({
                    "txid": txid,
                    "confirmations": 100,
                    "vout": outputs,
                }));
            }
            (Value::Array(out), Value::Null)
        }
        _ => (
            Value::Null,
            json!({"code": -32601, "message": format!("method {method} not found")}),
        ),
    };
    json!({"result": result, "error": error, "id": id})
}

fn handle_conn(mut stream: TcpStream, chain: Arc<Mutex<MockChain>>) {
    let mut reader = BufReader::new(stream.try_clone().expect("clone stream"));
    loop {
        let mut request_line = String::new();
        if reader.read_line(&mut request_line).unwrap_or(0) == 0 {
            return;
        }
        let mut len = 0usize;
        loop {
            let mut header = String::new();
            if reader.read_line(&mut header).unwrap_or(0) == 0 {
                return;
            }
            let header = header.trim().to_ascii_lowercase();
            if header.is_empty() {
                break;
            }
            if let Some(v) = header.strip_prefix("content-length:") {
                len = v.trim().parse().unwrap_or(0);
            }
        }
        let mut body = vec![0u8; len];
        if len > 0 && reader.read_exact(&mut body).is_err() {
            return;
        }
        let Ok(req) = serde_json::from_slice::<Value>(&body) else {
            return;
        };
        let text = handle_request(&chain, &req).to_string();
        let response = format!(
            "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{text}",
            text.len()
        );
        let _ = stream.write_all(response.as_bytes());
        let _ = stream.flush();
        return; // Connection: close
    }
}

/// Start the mock node; returns its `host:port` and the shared chain state.
fn start_mock_btcd() -> (String, Arc<Mutex<MockChain>>) {
    let listener = TcpListener::bind("127.0.0.1:0").expect("bind mock btcd");
    let host = listener.local_addr().expect("local addr").to_string();
    let chain = Arc::new(Mutex::new(MockChain { height: 200, ..Default::default() }));
    let chain_for_thread = chain.clone();
    std::thread::spawn(move || {
        for stream in listener.incoming() {
            let Ok(stream) = stream else { continue };
            let chain = chain_for_thread.clone();
            std::thread::spawn(move || handle_conn(stream, chain));
        }
    });
    (host, chain)
}

// =====================================================================
// Helpers
// =====================================================================

fn pubkey_hex(secret: &[u8; 32]) -> String {
    let secp = bitcoin::secp256k1::Secp256k1::new();
    let sk = bitcoin::secp256k1::SecretKey::from_slice(secret).expect("secret key");
    let pk = bitcoin::secp256k1::PublicKey::from_secret_key(&secp, &sk);
    to_hex(&pk.serialize())
}

fn p2wpkh(secret: &[u8; 32]) -> (Address, ScriptBuf) {
    let secp = bitcoin::secp256k1::Secp256k1::new();
    let sk = bitcoin::secp256k1::SecretKey::from_slice(secret).expect("secret key");
    let pk = CompressedPublicKey(bitcoin::secp256k1::PublicKey::from_secret_key(&secp, &sk));
    let addr = Address::p2wpkh(&pk, Network::Regtest);
    let script = addr.script_pubkey();
    (addr, script)
}

/// A stand-in "coinbase" funding tx: `(tx, outpoint of vout 0)`.
fn funding_tx(script: &ScriptBuf, value: u64, marker: u32) -> (Transaction, OutPoint) {
    let tx = Transaction {
        version: bitcoin::transaction::Version::TWO,
        lock_time: LockTime::ZERO,
        input: vec![TxIn {
            previous_output: OutPoint::null(),
            script_sig: ScriptBuf::from_bytes(marker.to_le_bytes().to_vec()),
            sequence: Sequence::MAX,
            witness: Witness::new(),
        }],
        output: vec![TxOut {
            value: bitcoin::Amount::from_sat(value),
            script_pubkey: script.clone(),
        }],
    };
    let outpoint = OutPoint { txid: tx.compute_txid(), vout: 0 };
    (tx, outpoint)
}

/// Sign a PSBT with an external key (stands in for the chain33 TSS group); every input must be a
/// P2WPKH output of that key.
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
        let sh =
            cache.p2wpkh_signature_hash(i, &txout.script_pubkey, txout.value, EcdsaSighashType::All)?;
        let msg = bitcoin::secp256k1::Message::from(sh);
        let sig = secp.sign_ecdsa(&msg, &sk);
        let btc_sig = bitcoin::ecdsa::Signature::sighash_all(sig);
        psbt.inputs[i].partial_sigs.insert(bitcoin::PublicKey::new(pk), btc_sig);
        let mut w = Witness::new();
        w.push(btc_sig.to_vec());
        w.push(pk.serialize());
        psbt.inputs[i].final_script_witness = Some(w);
        psbt.inputs[i].final_script_sig = Some(ScriptBuf::new());
    }
    Ok(psbt)
}

fn inputs_of(psbt: &Psbt) -> Vec<String> {
    psbt.unsigned_tx.input.iter().map(|i| i.previous_output.to_string()).collect()
}

/// (outpoint, status, amount) of every seal of the asset — `SealTxOut` has no `PartialEq`.
fn seal_view(engine: &RgbEngine) -> Vec<(String, String, i64)> {
    let mut v: Vec<(String, String, i64)> = engine
        .list_seals(SYMBOL)
        .into_iter()
        .map(|s| (s.outpoint, s.status.as_str().to_string(), s.amount))
        .collect();
    v.sort();
    v
}

fn open_engine(data_dir: &std::path::Path, rpc: &str, tss_pubkey: &str) -> Result<RgbEngine> {
    RgbEngine::open(Config {
        data_dir: data_dir.to_path_buf(),
        btc_rpc_host: rpc.to_string(),
        btc_rpc_user: "root".into(),
        btc_rpc_pass: "1314".into(),
        btc_rpc_cert: None, // plain HTTP against the mock
        network: Network::Regtest,
        tss_pubkey_hex: tss_pubkey.to_string(),
        grpc_listen: "127.0.0.1:0".into(),
    })
}

fn data_dir(name: &str) -> std::path::PathBuf {
    let dir = std::env::temp_dir().join(format!("rgb-sidecar-test-{name}-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    dir
}

// =====================================================================
// The test
// =====================================================================

#[test]
fn failed_broadcast_is_replayed_not_rebuilt() -> Result<()> {
    let (rpc, chain) = start_mock_btcd();
    let tss_pubkey = pubkey_hex(&TSS_SECRET);
    let (_tss_addr, tss_script) = p2wpkh(&TSS_SECRET);
    let (_user_addr, user_script) = p2wpkh(&USER_SECRET);

    // Funding: the bridge's RGB seal (dust-ish) + a bridge-owned BTC fee UTXO.
    let (seal_funding, seal_outpoint) = funding_tx(&tss_script, SEAL_BTC, 1);
    let (fee_funding, fee_outpoint) = funding_tx(&tss_script, FEE_UTXO_BTC, 2);
    {
        let mut chain = chain.lock().unwrap();
        chain.add_tx(seal_funding);
        chain.add_tx(fee_funding);
    }

    let dir = data_dir("replay");
    let mut engine = open_engine(&dir, &rpc, &tss_pubkey)?;
    let asset = engine.issue_asset_at(SYMBOL, "Tether USD", 8, ISSUED as u64, seal_outpoint)?;

    let user_invoice = build_address_invoice(
        Network::Regtest,
        &user_script,
        asset.asset_id.parse()?,
        InflatableFungibleAsset::schema().schema_id(),
        WITHDRAW as u64,
    )?;
    let tss_addr = engine.tss_address().to_string();

    // ---- 1. first build + finalize ------------------------------------------------
    let first = engine.build_withdrawal(SYMBOL, WITHDRAW, &user_invoice, &tss_addr, FEE_RATE, &[])?;
    let first_inputs = inputs_of(&first.psbt);
    assert_eq!(
        first_inputs,
        vec![seal_outpoint.to_string(), fee_outpoint.to_string()],
        "the fresh build must spend the RGB seal plus the bridge-owned fee UTXO"
    );
    let first_txid = first.txid.to_string();
    let signed = sign_psbt(&first.psbt, &TSS_SECRET)?;
    let (txid, _recipient, change_outpoint) = engine.finalize_withdrawal(&signed)?;
    assert_eq!(txid.to_string(), first_txid, "finalize keeps the built txid (segwit)");
    let change_outpoint = change_outpoint.expect("withdrawal leaves a change seal");

    // ---- 2. the accident: the tx lands on chain, the ledger advances, the bridge
    //         receives a new (larger) BTC UTXO --------------------------------
    let broadcast_tx = signed.clone().extract_tx()?;
    let (new_deposit, _new_outpoint) = funding_tx(&tss_script, NEW_UTXO_BTC, 3);
    {
        let mut chain = chain.lock().unwrap();
        chain.add_tx(broadcast_tx.clone());
        chain.spend(seal_outpoint);
        chain.spend(fee_outpoint);
        chain.add_tx(new_deposit);
    }
    engine.sync()?;
    assert_eq!(
        engine.ledger.seal(&change_outpoint).map(|s| s.status),
        Some(rgb_sidecar::SealStatus::Minted),
        "the change seal must be spendable again now that its tx is on chain"
    );

    // ---- 3. the hazard: a naive rebuild picks another seal (and another fee input)
    let naive = engine.build_withdrawal(SYMBOL, WITHDRAW, &user_invoice, &tss_addr, FEE_RATE, &[])?;
    assert_ne!(
        naive.txid.to_string(),
        first_txid,
        "without input_seals the rebuild picks the change seal ⇒ a second, different payment"
    );
    assert_eq!(
        inputs_of(&naive.psbt)[0], change_outpoint,
        "the naive rebuild spends the change seal instead of the original one"
    );

    // ---- 4. sidecar restart, then the replay build --------------------------------
    drop(engine);
    let mut engine = open_engine(&dir, &rpc, &tss_pubkey)?;
    let replay = engine.build_withdrawal(
        SYMBOL,
        WITHDRAW,
        &user_invoice,
        &tss_addr,
        FEE_RATE,
        &[seal_outpoint.to_string()], // what the bridge records as the sticky seals
    )?;
    assert_eq!(
        replay.txid.to_string(),
        first_txid,
        "the replay must re-issue the recorded build: same transaction, same txid"
    );
    assert_eq!(
        replay.psbt.serialize(),
        first.psbt.serialize(),
        "the replayed PSBT must be byte-identical to the recorded one (only then is the txid stable)"
    );
    assert_eq!(
        inputs_of(&replay.psbt),
        first_inputs,
        "the replay must keep the recorded input list, not re-select from the live wallet"
    );
    assert_eq!(
        replay.consignment, first.consignment,
        "the replay reproduces the same consignment (same RGB transition)"
    );
    // The bridge re-validates what it is about to sign, including its own sticky-seal comparison
    // (PSBT inputs ∩ the consignment's closed seals). That comparison only works if the sidecar
    // still resolves the transition's closed seal *after* the first finalize consumed it in the
    // Stock — otherwise the retry would come back empty and be judged unrecoverable.
    let insp = engine.validate_consignment(&replay.consignment)?;
    assert!(insp.valid, "the replayed consignment must still validate: {:?}", insp.error);
    assert!(
        insp.closed_seals.contains(&seal_outpoint.to_string()),
        "closed seals must still resolve after the transition was consumed: {:?}",
        insp.closed_seals
    );

    // ---- 5. finalize the replay: idempotent across the restart --------------------
    let seals_before = seal_view(&engine);
    let (txid2, recipient2, change2) = engine.finalize_withdrawal(&signed)?;
    assert_eq!(txid2.to_string(), first_txid);
    assert_eq!(change2.as_deref(), Some(change_outpoint.as_str()));
    assert!(recipient2.ends_with(":1"), "recipient outpoint: {recipient2}");
    assert_eq!(
        seal_view(&engine),
        seals_before,
        "a repeated finalize must not re-apply the transition or add a second change seal"
    );

    // ---- 6. without the record, "the same seals" is NOT the same transaction ------
    // The build is not a pure function of its inputs: every run draws fresh random blinding
    // factors for the recipient/change seals (and picks fee inputs from the live wallet), so
    // re-building from the same seals yields a different RGB commitment — and therefore a
    // different txid. This is why the replay has to re-issue the recorded PSBT, and why the
    // fallback below is a degraded path (same seals, different transaction).
    engine.ledger.withdrawal_builds.clear();
    let rebuilt = engine.build_withdrawal(
        SYMBOL,
        WITHDRAW,
        &user_invoice,
        &tss_addr,
        FEE_RATE,
        &[seal_outpoint.to_string()],
    )?;
    assert_ne!(
        rebuilt.txid.to_string(),
        first_txid,
        "a rebuild of the same seals is a different transaction ⇒ the record is what makes a replay a replay"
    );

    // ---- 7. the seals are verified, not trusted ----------------------------------
    let err = engine
        .build_withdrawal(SYMBOL, WITHDRAW, &user_invoice, &tss_addr, FEE_RATE, &[fee_outpoint.to_string()])
        .expect_err("an outpoint carrying no RGB state must be rejected");
    assert!(
        format!("{err:#}").contains("has no rgb:"),
        "unexpected error: {err:#}"
    );
    let unknown = "0202020202020202020202020202020202020202020202020202020202020202:0";
    let err = engine
        .build_withdrawal(SYMBOL, WITHDRAW, &user_invoice, &tss_addr, FEE_RATE, &[unknown.to_string()])
        .expect_err("an unknown outpoint must be rejected");
    assert!(format!("{err:#}").contains("has no rgb:"), "unexpected error: {err:#}");
    // The replay list is only meaningful for the asset it was recorded for.
    let err = engine
        .build_withdrawal("OTHER", WITHDRAW, &user_invoice, &tss_addr, FEE_RATE, &[seal_outpoint.to_string()])
        .expect_err("an unknown asset must be rejected");
    assert!(format!("{err:#}").contains("not issued"), "unexpected error: {err:#}");

    // A record is never handed out for a request it does not match: the same seals with a
    // different invoice (i.e. a different withdrawal that happens to share the seal set) is an
    // error, not a quiet substitution with somebody else's build.
    let other_invoice = build_address_invoice(
        Network::Regtest,
        &p2wpkh(&[0x33; 32]).1,
        asset.asset_id.parse()?,
        InflatableFungibleAsset::schema().schema_id(),
        WITHDRAW as u64,
    )?;
    let err = engine
        .build_withdrawal(SYMBOL, WITHDRAW, &other_invoice, &tss_addr, FEE_RATE, &[seal_outpoint.to_string()])
        .expect_err("a recorded build must not be re-issued for a different recipient");
    assert!(
        format!("{err:#}").contains("differs from the request"),
        "unexpected error: {err:#}"
    );

    let _ = std::fs::remove_dir_all(&dir);
    Ok(())
}

/// The replay path must also be reachable through the exact wire shape the bridge uses
/// (`rgb20/pb`: `input_seals` is field 7 of `BuildWithdrawalRequest`), and unknown fields must not
/// break the server. This guards the Go↔Rust contract shared by the two generated stubs.
#[test]
fn proto_carries_input_seals() -> Result<()> {
    use rgb_sidecar::pb::BuildWithdrawalRequest;
    let req = BuildWithdrawalRequest {
        asset_symbol: SYMBOL.into(),
        asset_id: "rgb:test".into(),
        amount: WITHDRAW,
        recipient_invoice: "rgb:invoice".into(),
        change_address: "bcrt1qtest".into(),
        fee_rate: FEE_RATE as u32,
        input_seals: vec!["aa:0".into(), "bb:1".into()],
    };
    let bytes = prost::Message::encode_to_vec(&req);
    let back = <BuildWithdrawalRequest as prost::Message>::decode(&bytes[..])?;
    assert_eq!(back.input_seals, vec!["aa:0".to_string(), "bb:1".to_string()]);
    // Field 7 on the wire, as declared in proto/rgb_sidecar.proto.
    assert!(bytes.windows(2).any(|w| w == [0x3a, 0x04]), "tag for field 7 not found");
    Ok(())
}
