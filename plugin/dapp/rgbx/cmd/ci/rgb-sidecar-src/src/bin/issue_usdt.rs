//! One-shot helper: fund the TSS address and issue RGB20 USDT into a sidecar
//! data dir (env `RGB_SIDECAR_DATA_DIR`, default `/tmp/rgb-spike/sidecar-interop`),
//! so a fresh gRPC sidecar started on that dir has a real asset for Go<->Rust interop.
//!
//! - TSS pubkey: env `RGB_SIDECAR_TSS_PUBKEY`（chain33 GG18 阈值公钥），缺省用开发测试密钥。
//! - bitcoind RPC：env `RGB_BITCOIND_RPC` / `RGB_BITCOIND_USER` / `RGB_BITCOIND_PASS`（默认 regtest 本地）。
//!   通过 JSON-RPC 交互（不依赖本机 bitcoin-cli），可在任意容器/宿主运行。

use anyhow::{anyhow, Result};
use bitcoin::Network;
use rgb_sidecar::config::Config;
use rgb_sidecar::engine::RgbEngine;

const TSS_SECRET: [u8; 32] = [0x11; 32];

fn env_or(key: &str, default: &str) -> String {
    std::env::var(key).unwrap_or_else(|_| default.to_string())
}

fn basic_auth(user: &str, pass: &str) -> String {
    use base64::Engine;
    base64::engine::general_purpose::STANDARD.encode(format!("{user}:{pass}"))
}

fn rpc_url() -> String {
    env_or("RGB_BITCOIND_RPC", "http://127.0.0.1:18443")
}

fn rpc(method: &str, params: serde_json::Value) -> Result<serde_json::Value> {
    let user = env_or("RGB_BITCOIND_USER", "rgb");
    let pass = env_or("RGB_BITCOIND_PASS", "rgbpass123");
    let body = serde_json::json!({"jsonrpc": "1.0", "id": "issue-usdt", "method": method, "params": params});
    let resp = ureq::post(&rpc_url())
        .set("Content-Type", "application/json")
        .set("Authorization", &basic_auth(&user, &pass))
        .send_string(&body.to_string())
        .map_err(|e| anyhow!("rpc {method}: {e}"))?;
    let text = resp
        .into_string()
        .map_err(|e| anyhow!("rpc {method} read: {e}"))?;
    let v: serde_json::Value = serde_json::from_str(&text)?;
    if let Some(err) = v.get("error") {
        if !err.is_null() {
            return Err(anyhow!("rpc {method} error: {err}"));
        }
    }
    v.get("result").cloned().ok_or_else(|| anyhow!("rpc {method} no result"))
}

fn ensure_wallet() -> Result<()> {
    // 钱包可能已存在；忽略 "already exists"。
    if let Err(e) = rpc("createwallet", serde_json::json!(["spike"])) {
        let msg = format!("{e:#}");
        if msg.contains("already exists") {
            return Ok(());
        }
        // "Database already exists" 同样可忽略
        if msg.contains("Database already exists") {
            return Ok(());
        }
        // 部分实现 createwallet 后返回 error 但钱包已建；再探测一次
        let _ = rpc("getwalletinfo", serde_json::json!([]));
        return Ok(());
    }
    Ok(())
}

fn mine_blocks(n: u32) -> Result<()> {
    let addr = rpc("getnewaddress", serde_json::json!([{}]))?
        .as_str()
        .unwrap_or_default()
        .to_string();
    rpc("generatetoaddress", serde_json::json!([n, addr]))?;
    Ok(())
}

fn fund(address: &str, btc: f64) -> Result<()> {
    rpc("sendtoaddress", serde_json::json!([address, btc]))?;
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

#[tokio::main]
async fn main() -> Result<()> {
    let data_dir = env_or("RGB_SIDECAR_DATA_DIR", "/tmp/rgb-spike/sidecar-interop");
    // 若给定 GG18 公钥则用之（chain33 TSS 组的阈值公钥），否则用开发默认测试密钥。
    let tss_pubkey_hex = env_or("RGB_SIDECAR_TSS_PUBKEY", "");
    let tss_pubkey_hex = if tss_pubkey_hex.is_empty() {
        pubkey_hex(&TSS_SECRET)?
    } else {
        tss_pubkey_hex
    };
    let cfg = Config {
        data_dir: data_dir.clone().into(),
        electrum_url: env_or("RGB_SIDECAR_ELECTRUM", "127.0.0.1:60401").into(),
        network: Network::Regtest,
        tss_pubkey_hex,
        grpc_listen: "0.0.0.0:0".into(),
    };
    let mut engine = RgbEngine::open(cfg)?;

    ensure_wallet()?;
    let tss_addr = engine.tss_address().to_string();
    println!("TSS address: {tss_addr}");
    fund(&tss_addr, 10.0)?;
    engine.sync()?;

    if engine.ledger.asset("USDT").is_some() {
        println!("USDT already issued, skipping");
    } else {
        let asset = engine.issue_asset("USDT", "Tether USD", 8, 10_000_000_000)?; // 100.0 USDT
        println!("issued USDT asset_id={}", asset.asset_id);
    }
    engine.save()?;
    let assets = engine.list_assets();
    for a in &assets {
        println!("asset: symbol={} precision={} issued={}", a.symbol, a.precision, a.issued_supply);
    }
    let seals = engine.list_seals("USDT");
    for s in &seals {
        println!("seal: outpoint={} amount={} status={:?}", s.outpoint, s.amount, s.status);
    }
    println!("ISSUE-DONE ledger={data_dir}");
    Ok(())
}
