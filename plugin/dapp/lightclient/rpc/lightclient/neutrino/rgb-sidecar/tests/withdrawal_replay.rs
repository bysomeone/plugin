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
    /// `gettxout` 报的确认数（0 = 还没确认；决定钱包认到的 `height`/`maturity_height`）。
    confirmations: u64,
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
            // NOTE: the guard from `lock()` lives until the end of the enclosing statement (the whole
            // `match`), so read `confirmations` *before* it — locking twice would deadlock.
            let confirmations = chain.lock().unwrap().confirmations;
            let live = op.and_then(|op| chain.lock().unwrap().utxos.get(&op).cloned());
            match live {
                Some((value, script)) => (
                    json!({
                        "value": value as f64 / 100_000_000.0,
                        "scriptPubKey": {"hex": to_hex(script.as_bytes())},
                        "confirmations": confirmations,
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
    let chain = Arc::new(Mutex::new(MockChain {
        height: 200,
        confirmations: 100,
        ..Default::default()
    }));
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

    // ---- 6. without the record there is no second transaction at all --------------
    // The build is not a pure function of its inputs: every run draws fresh random blinding
    // factors for the recipient/change seals (and picks fee inputs from the live wallet), so
    // rebuilding from the same seals used to yield a *different* transaction — a second,
    // independently valid payment for one on-chain burn, i.e. the E9 hazard this record exists to
    // close. C4 把它彻底堵死（fail-closed）：重放要花的那个 seal 早在第一笔上链时就被花掉了，
    // 钱包里查不到它的脚本与面额，而 BIP143 把 prevout 面额算进被签的消息 —— 猜一个值只会签出
    // 一笔必然无效的交易（改前那条"退化路径"产出的正是这种东西）。所以记录丢了不再退化成
    // "另造一笔"，而是直接报错，由调用方按不可恢复处理。
    // 记录没了（等同归档被外部删掉）：重放必须 fail-closed。O3 之后记录住在 `<data>/builds/`，
    // 整个目录就是"重放存档"，删掉它等价于改前的 `withdrawal_builds.clear()`。
    std::fs::remove_dir_all(dir.join("builds")).unwrap();
    let err = engine
        .build_withdrawal(
            SYMBOL,
            WITHDRAW,
            &user_invoice,
            &tss_addr,
            FEE_RATE,
            &[seal_outpoint.to_string()],
        )
        .expect_err("without the record the retry must fail closed, not build a second payment");
    assert!(
        format!("{err:#}").contains("not in the wallet's watch set"),
        "unexpected error: {err:#}"
    );

    let _ = std::fs::remove_dir_all(&dir);
    Ok(())
}

/// O3：提现构建的重放存档（`<data>/builds/`）的生命周期。
///
/// 归档窗口是"**已 finalize 且锚定交易确认满 N 块**"，缺一不可：
///   - 未 finalize ⇒ 不裁（哪怕链上已经有那笔交易——那正是最需要重放的情形）；
///   - 未确认 / 没有 change seal ⇒ 不裁（拿不到确认高度的证明）；
///   - 满足条件 ⇒ 裁成墓碑（丢掉 PSBT 载荷，留下 txid），此后重放**响亮失败**，绝不静默重建。
///
/// 反面（"取回同一份 PSBT"的能力被改坏）：把 `BuildStore::get` 的裁剪/损坏分支改成返回
/// `Ok(None)`，下面第 4 步就会去走重建路径；把 `put` 改成空操作，`failed_broadcast_is_replayed_
/// not_rebuilt` 会红。
#[test]
fn build_archive_is_pruned_only_after_finalize_and_confirmation() -> Result<()> {
    let (rpc, chain) = start_mock_btcd();
    let tss_pubkey = pubkey_hex(&TSS_SECRET);
    let (_tss_addr, tss_script) = p2wpkh(&TSS_SECRET);

    let (seal_funding, seal_outpoint) = funding_tx(&tss_script, SEAL_BTC, 11);
    let (fee_funding, fee_outpoint) = funding_tx(&tss_script, FEE_UTXO_BTC, 12);
    {
        let mut c = chain.lock().unwrap();
        c.add_tx(seal_funding);
        c.add_tx(fee_funding);
    }

    let dir = data_dir("archive");
    let mut engine = open_engine(&dir, &rpc, &tss_pubkey)?;
    let asset = engine.issue_asset_at(SYMBOL, "Tether USD", 8, ISSUED as u64, seal_outpoint)?;
    let user_invoice = build_address_invoice(
        Network::Regtest,
        &p2wpkh(&USER_SECRET).1,
        asset.asset_id.parse()?,
        InflatableFungibleAsset::schema().schema_id(),
        WITHDRAW as u64,
    )?;
    let tss_addr = engine.tss_address().to_string();
    // 保留窗口设成 0：确认即可裁 —— 让测试不必造 144 个块。
    engine.set_build_retain_confirmations(0);
    let archive_dir = dir.join("builds");

    // ---- 1. 构建：归档落盘，且**不是**在热账本里 ----
    let first = engine.build_withdrawal(SYMBOL, WITHDRAW, &user_invoice, &tss_addr, FEE_RATE, &[])?;
    let first_txid = first.txid.to_string();
    let seal_key = seal_outpoint.to_string();
    let files: Vec<_> = std::fs::read_dir(&archive_dir)?.map(|e| e.unwrap().path()).collect();
    assert_eq!(files.len(), 1, "one build ⇒ exactly one archive file: {files:?}");
    let full_len = std::fs::metadata(&files[0])?.len();
    let hot: serde_json::Value =
        serde_json::from_str(&std::fs::read_to_string(dir.join("ledger.json"))?)?;
    assert!(
        hot.get("withdrawal_builds").is_none() && !hot.to_string().contains("psbt_hex"),
        "the hot ledger must not carry the build payload any more: {}",
        hot.to_string().chars().take(200).collect::<String>()
    );

    // ---- 2. 未 finalize ⇒ 不裁（哪怕链上已经有它的交易） ----
    let broadcast = {
        let signed = sign_psbt(&first.psbt, &TSS_SECRET)?;
        chain.lock().unwrap().add_tx(signed.clone().extract_tx()?);
        signed
    };
    engine.sync()?;
    assert_eq!(engine.prune_build_archives()?, 0, "an unfinalized build must never be pruned");

    // ---- 3. finalize 但**未确认** ⇒ 仍然不裁 ----
    engine.finalize_withdrawal(&broadcast)?;
    {
        let mut c = chain.lock().unwrap();
        c.confirmations = 0; // 交易还在内存池里
        c.spend(seal_outpoint);
        c.spend(fee_outpoint);
    }
    engine.sync()?;
    assert_eq!(
        engine.prune_build_archives()?,
        0,
        "an unprovable confirmation height must block pruning"
    );

    // ---- 4. 已 finalize + 已确认 ⇒ 裁成墓碑；重放响亮失败 ----
    chain.lock().unwrap().confirmations = 100;
    engine.sync()?;
    assert_eq!(engine.prune_build_archives()?, 1, "a confirmed, finalized build must be pruned");
    let tomb = std::fs::metadata(&files[0])?.len();
    assert!(tomb * 4 < full_len, "tombstone should be far smaller: {tomb} vs {full_len}");
    // 幂等：再裁一次什么都不做。
    assert_eq!(engine.prune_build_archives()?, 0);

    let err = engine
        .build_withdrawal(
            SYMBOL,
            WITHDRAW,
            &user_invoice,
            &tss_addr,
            FEE_RATE,
            &[seal_key],
        )
        .expect_err("a pruned build must fail loudly, never silently rebuild");
    let msg = format!("{err:#}");
    assert!(msg.contains("was pruned"), "unexpected error: {msg}");
    assert!(msg.contains(&first_txid), "the error must name the txid: {msg}");

    // ---- 5. 重启后墓碑仍在：裁剪结果活得比进程久 ----
    drop(engine);
    let mut engine = open_engine(&dir, &rpc, &tss_pubkey)?;
    engine.set_build_retain_confirmations(0);
    let err = engine
        .build_withdrawal(SYMBOL, WITHDRAW, &user_invoice, &tss_addr, FEE_RATE, &[format!("{}", seal_outpoint)])
        .expect_err("the tombstone must survive a restart");
    assert!(format!("{err:#}").contains("was pruned"), "unexpected: {err:#}");

    let _ = std::fs::remove_dir_all(&dir);
    Ok(())
}

/// O3 迁移：pre-O3 的 `ledger.json` 把构建存档存在热账本里。启动时必须**搬到冷层**（否则那台
/// 机器的重放能力会静默消失），且迁移只做一次。
#[test]
fn legacy_ledger_builds_are_migrated_to_the_archive() -> Result<()> {
    let (rpc, chain) = start_mock_btcd();
    let tss_pubkey = pubkey_hex(&TSS_SECRET);
    let (_tss_addr, tss_script) = p2wpkh(&TSS_SECRET);
    let (seal_funding, seal_outpoint) = funding_tx(&tss_script, SEAL_BTC, 21);
    let (fee_funding, _fee_outpoint) = funding_tx(&tss_script, FEE_UTXO_BTC, 22);
    {
        let mut c = chain.lock().unwrap();
        c.add_tx(seal_funding);
        c.add_tx(fee_funding);
    }

    let dir = data_dir("legacy-migrate");
    let mut engine = open_engine(&dir, &rpc, &tss_pubkey)?;
    let asset = engine.issue_asset_at(SYMBOL, "Tether USD", 8, ISSUED as u64, seal_outpoint)?;
    let user_invoice = build_address_invoice(
        Network::Regtest,
        &p2wpkh(&USER_SECRET).1,
        asset.asset_id.parse()?,
        InflatableFungibleAsset::schema().schema_id(),
        WITHDRAW as u64,
    )?;
    let tss_addr = engine.tss_address().to_string();
    let first = engine.build_withdrawal(SYMBOL, WITHDRAW, &user_invoice, &tss_addr, FEE_RATE, &[])?;
    let seal_key = seal_outpoint.to_string();
    drop(engine);
    // 模拟一台 **pre-O3** 的机器：还没有 `builds/`，构建存档躺在热账本里。
    std::fs::remove_dir_all(dir.join("builds")).unwrap();

    let legacy_rec = serde_json::json!({
        "txid": first.txid.to_string(),
        "asset_id": asset.asset_id,
        "asset_symbol": SYMBOL,
        "input_outpoints": [seal_outpoint.to_string()],
        "change_vout": 2,
        "change_amount": 100_000,
        "psbt_hex": "abcd",          // 内容不是本测试的重点：只验证"旧记录被搬进冷层"
        "consignment_hex": "abcd",
        "input_amounts": [SEAL_BTC as i64],
        "fascia_hex": "abcd",
    });
    let ledger_path = dir.join("ledger.json");
    let mut hot: serde_json::Value =
        serde_json::from_str(&std::fs::read_to_string(&ledger_path)?)?;
    hot["withdrawal_builds"] = serde_json::json!({ seal_key.clone(): legacy_rec });
    std::fs::write(&ledger_path, serde_json::to_vec_pretty(&hot)?)?;

    // 启动即迁移：不是报错，而是把旧记录搬进冷层。
    let _engine = open_engine(&dir, &rpc, &tss_pubkey)?;
    let hot_after = std::fs::read_to_string(&ledger_path)?;
    assert!(
        !hot_after.contains("withdrawal_builds"),
        "the hot ledger must be rewritten without the build archive"
    );
    let files: Vec<_> = std::fs::read_dir(dir.join("builds"))?.map(|e| e.unwrap().path()).collect();
    assert_eq!(files.len(), 1, "the legacy record must land in the archive: {files:?}");
    let rec: serde_json::Value = serde_json::from_slice(&std::fs::read(&files[0])?)?;
    assert_eq!(rec["txid"].as_str().unwrap(), first.txid.to_string());
    assert_eq!(rec["seal_set_key"].as_str().unwrap(), seal_key);

    let _ = std::fs::remove_dir_all(&dir);
    Ok(())
}

/// The replay path must also be reachable through the exact wire shape the bridge uses/// (`rgb20/pb`: `input_seals` is field 7 of `BuildWithdrawalRequest`), and unknown fields must not
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
