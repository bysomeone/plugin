//! Shared harness for the sidecar's integration tests: a **mock btcd JSON-RPC node** (no
//! bitcoind/btcd needed) plus the small builders the tests drive an engine with.
//!
//! Not every test uses every helper, hence the module-wide `dead_code` allowance.

#![allow(dead_code)]

use std::collections::HashMap;
use std::io::{BufRead, BufReader, Read, Write};
use std::net::{TcpListener, TcpStream};
use std::str::FromStr;
use std::sync::{Arc, Mutex};

use anyhow::Result;
use bitcoin::consensus::encode;
use bitcoin::hex::{Case, DisplayHex};
use bitcoin::{
    Address, Network, OutPoint, ScriptBuf, Sequence, Transaction, TxIn, TxOut,
};
use rgb_sidecar::config::Config;
use rgb_sidecar::engine::RgbEngine;
use serde_json::{json, Value};

// =====================================================================
// Mock btcd JSON-RPC node
// =====================================================================

#[derive(Default)]
pub struct MockChain {
    pub height: u64,
    /// `gettxout` 报的确认数（0 = 还没确认；决定钱包认到的 `height`/`maturity_height`）。
    pub confirmations: u64,
    /// txid -> serialized tx (hex served by `getrawtransaction`).
    pub txs: HashMap<String, Transaction>,
    /// live (unspent) UTXOs.
    pub utxos: HashMap<OutPoint, (u64, ScriptBuf)>,
}

impl MockChain {
    pub fn add_tx(&mut self, tx: Transaction) {
        let txid = tx.compute_txid().to_string();
        for (vout, out) in tx.output.iter().enumerate() {
            self.utxos
                .insert(OutPoint { txid: tx.compute_txid(), vout: vout as u32 }, (out.value.to_sat(), out.script_pubkey.clone()));
        }
        self.txs.insert(txid, tx);
    }

    pub fn spend(&mut self, outpoint: OutPoint) {
        self.utxos.remove(&outpoint);
    }
}

pub fn to_hex(bytes: &[u8]) -> String {
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
pub fn start_mock_btcd() -> (String, Arc<Mutex<MockChain>>) {
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

pub fn pubkey_hex(secret: &[u8; 32]) -> String {
    let secp = bitcoin::secp256k1::Secp256k1::new();
    let sk = bitcoin::secp256k1::SecretKey::from_slice(secret).expect("secret key");
    let pk = bitcoin::secp256k1::PublicKey::from_secret_key(&secp, &sk);
    to_hex(&pk.serialize())
}

pub fn p2wpkh(secret: &[u8; 32]) -> (Address, ScriptBuf) {
    let secp = bitcoin::secp256k1::Secp256k1::new();
    let sk = bitcoin::secp256k1::SecretKey::from_slice(secret).expect("secret key");
    let pk = bitcoin::CompressedPublicKey(bitcoin::secp256k1::PublicKey::from_secret_key(&secp, &sk));
    let addr = Address::p2wpkh(&pk, Network::Regtest);
    let script = addr.script_pubkey();
    (addr, script)
}

/// A stand-in "coinbase" funding tx: `(tx, outpoint of vout 0)`.
pub fn funding_tx(script: &ScriptBuf, value: u64, marker: u32) -> (Transaction, OutPoint) {
    let tx = Transaction {
        version: bitcoin::transaction::Version::TWO,
        lock_time: bitcoin::absolute::LockTime::ZERO,
        input: vec![TxIn {
            previous_output: OutPoint::null(),
            script_sig: ScriptBuf::from_bytes(marker.to_le_bytes().to_vec()),
            sequence: Sequence::MAX,
            witness: bitcoin::Witness::new(),
        }],
        output: vec![TxOut {
            value: bitcoin::Amount::from_sat(value),
            script_pubkey: script.clone(),
        }],
    };
    let outpoint = OutPoint { txid: tx.compute_txid(), vout: 0 };
    (tx, outpoint)
}

pub fn open_engine(data_dir: &std::path::Path, rpc: &str, tss_pubkey: &str) -> Result<RgbEngine> {
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

pub fn data_dir(name: &str) -> std::path::PathBuf {
    let dir = std::env::temp_dir().join(format!("rgb-sidecar-test-{name}-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    dir
}
