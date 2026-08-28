//! Test-only simulation driver (NOT part of the shared gRPC contract).
//!
//! Exposes a minimal HTTP JSON endpoint (`RGB_SIDECAR_TEST_LISTEN`, default disabled) that
//! drives the E2E **without touching `proto/rgb_sidecar.proto`**:
//!   - `POST /sim/user_pay`          build an UNSIGNED transfer (bridge USDT -> receive invoice)
//!   - `POST /sim/user_pay_submit`   broadcast a TSS-signed PSBT + provide consignment (settle)
//!   - `POST /sim/user_invoice`      create a user-side RGB invoice (withdraw E2E)
//!   - `GET  /sim/status`            debug status (assets / balance / seal count)
//!
//! The sidecar NEVER signs: the deposit-payment PSBT is signed by the chain33 TSS group
//! (via the bridge's test `sign-psbt` endpoint), preserving the "no keys in sidecar" rule.

use std::net::SocketAddr;
use std::str::FromStr;
use std::sync::Arc;

use anyhow::{anyhow, Result};
use bitcoin::OutPoint;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpListener;
use tokio::sync::Mutex;

use crate::engine::RgbEngine;
use crate::invoice::parse_invoice;

fn env_or(key: &str, default: &str) -> String {
    std::env::var(key).unwrap_or_else(|_| default.to_string())
}

fn basic_auth(user: &str, pass: &str) -> String {
    use base64::Engine;
    // 修复：必须带 "Basic " scheme，否则 bitcoind RPC 返回 401（issue_usdt.rs 同款实现带前缀）。
    format!(
        "Basic {}",
        base64::engine::general_purpose::STANDARD.encode(format!("{user}:{pass}"))
    )
}

/// bitcoind JSON-RPC（test-sim 用，不依赖本机 bitcoin-cli）。
fn rpc(method: &str, params: serde_json::Value) -> Result<serde_json::Value> {
    let url = env_or("RGB_BITCOIND_RPC", "http://127.0.0.1:18443");
    let user = env_or("RGB_BITCOIND_USER", "rgb");
    let pass = env_or("RGB_BITCOIND_PASS", "rgbpass123");
    let body = serde_json::json!({"jsonrpc": "1.0", "id": "test-sim", "method": method, "params": params});
    let resp = ureq::post(&url)
        .set("Content-Type", "application/json")
        .set("Authorization", &basic_auth(&user, &pass))
        .send_string(&body.to_string())
        .map_err(|e| anyhow!("rpc {method}: {e}"))?;
    let text = resp.into_string().map_err(|e| anyhow!("rpc {method} read: {e}"))?;
    let v: serde_json::Value = serde_json::from_str(&text)?;
    if let Some(err) = v.get("error") {
        if !err.is_null() {
            return Err(anyhow!("rpc {method} error: {err}"));
        }
    }
    v.get("result").cloned().ok_or_else(|| anyhow!("rpc {method} no result"))
}

/// Mine regtest blocks via bitcoind JSON-RPC. Test-only.
fn mine_blocks(n: u32) -> Result<()> {
    // 修复：getnewaddress 不接收参数对象，空参即可（[{}] 会被 bitcoind 判为 label 类型错误 -3，
    // 导致挖块失败 → deposit settle 卡住）。
    let addr = rpc("getnewaddress", serde_json::json!([]))?
        .as_str()
        .unwrap_or_default()
        .to_string();
    rpc("generatetoaddress", serde_json::json!([n, addr]))?;
    Ok(())
}

fn hex_encode(bytes: &[u8]) -> String {
    use bitcoin::hex::DisplayHex;
    bytes.to_lower_hex_string()
}

fn hex_decode(s: &str) -> Vec<u8> {
    let mut out = Vec::with_capacity(s.len() / 2);
    let s = s.trim();
    let mut i = 0;
    while i + 1 < s.len() {
        if let Ok(b) = u8::from_str_radix(&s[i..i + 2], 16) {
            out.push(b);
        }
        i += 2;
    }
    out
}

/// Engine-side simulation helpers (extension trait; uses only public engine/ledger APIs).
pub trait SimulateExt {
    fn simulate_user_pay_begin(&mut self, invoice: &str) -> Result<SimPayOutcome>;
    fn simulate_user_pay_submit(
        &mut self,
        psbt_hex: &str,
        consignment_hex: &str,
        receive_id: &str,
    ) -> Result<SimPayResult>;
    fn build_user_invoice(&mut self, asset_symbol: &str, amount: i64) -> Result<String>;
}

pub struct SimPayOutcome {
    pub psbt: bitcoin::Psbt,
    pub consignment: Vec<u8>,
    pub txid: bitcoin::Txid,
    pub symbol: String,
}

pub struct SimPayResult {
    pub receive_id: String,
    pub status: String,
    pub txid: String,
}

impl SimulateExt for RgbEngine {
    fn simulate_user_pay_begin(&mut self, invoice: &str) -> Result<SimPayOutcome> {
        // 同步 TSS watch-only 钱包：否则 bdk list_unspent 看不到 TSS 地址的 BTC UTXO，
        // build_transfer 报 "BTC inputs (0) cannot cover output+fee"。
        self.wallet.sync()?;
        let parsed = parse_invoice(invoice)?;
        let amount = parsed
            .amount
            .ok_or_else(|| anyhow!("invoice has no amount"))?
            .value() as i64;
        let asset_id = parsed
            .contract_id
            .ok_or_else(|| anyhow!("invoice has no contract"))?
            .to_string();
        let symbol = self
            .ledger
            .assets
            .values()
            .find(|a| a.asset_id == asset_id)
            .map(|a| a.symbol.clone())
            .ok_or_else(|| anyhow!("asset {asset_id} not in ledger"))?;

        let (seals, _total) = self.ledger.select_seals(&symbol, amount)?;
        let input_outpoints: Vec<OutPoint> = seals
            .iter()
            .map(|s| OutPoint::from_str(&s.outpoint))
            .collect::<Result<_, _>>()?;
        drop(seals); // release the borrow on self.ledger before build_transfer(&mut self)
        let change_script = self.tss_script().clone();
        let outcome = self.build_transfer(
            &symbol,
            &input_outpoints,
            invoice,
            &change_script,
            2,  // sat/vB
            546, // dust to the receive output
        )?;
        Ok(SimPayOutcome {
            psbt: outcome.psbt,
            consignment: outcome.consignment,
            txid: outcome.txid,
            symbol,
        })
    }

    fn simulate_user_pay_submit(
        &mut self,
        psbt_hex: &str,
        consignment_hex: &str,
        receive_id: &str,
    ) -> Result<SimPayResult> {
        let psbt = bitcoin::psbt::Psbt::deserialize(&hex_decode(psbt_hex))
            .map_err(|e| anyhow!("decode signed psbt: {e}"))?;
        let tx = psbt.extract_tx().map_err(|e| anyhow!("extract tx: {e}"))?;
        let txid = self.broadcast(&tx)?;
        mine_blocks(1)?;
        self.wait_tx(txid, std::time::Duration::from_secs(15))?;
        let rec = self.provide_consignment(&hex_decode(consignment_hex), Some(receive_id))?;
        self.sync()?;
        Ok(SimPayResult {
            receive_id: rec.receive_id,
            status: rec.status,
            txid: txid.to_string(),
        })
    }

    fn build_user_invoice(&mut self, asset_symbol: &str, amount: i64) -> Result<String> {
        let asset = self
            .ledger
            .asset(asset_symbol)
            .cloned()
            .ok_or_else(|| anyhow!("asset {asset_symbol} not issued"))?;
        let net = self.network();
        let user_addr = crate::wallet::user_test_address(net)?;
        crate::invoice::build_address_invoice(
            net,
            &user_addr.script_pubkey(),
            asset.asset_id.parse()?,
            crate::invoice::ifa_schema_id(),
            amount as u64,
        )
        .map_err(Into::into)
    }
}

// =====================================================================
// Minimal HTTP JSON server
// =====================================================================

fn http_response(body: &[u8]) -> Vec<u8> {
    let mut out = Vec::new();
    out.extend_from_slice(b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: ");
    out.extend_from_slice(body.len().to_string().as_bytes());
    out.extend_from_slice(b"\r\nConnection: close\r\n\r\n");
    out.extend_from_slice(body);
    out
}

fn http_error(code: u16, msg: &str) -> Vec<u8> {
    let body = serde_json::to_vec(&serde_json::json!({"error": msg})).unwrap_or_default();
    let mut out = Vec::new();
    out.extend_from_slice(
        format!(
            "HTTP/1.1 {code} ERROR\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
            body.len()
        )
        .as_bytes(),
    );
    out.extend_from_slice(&body);
    out
}

/// Locate `needle` inside `haystack`; return the byte offset or `None`.
fn find_subsequence(haystack: &[u8], needle: &[u8]) -> Option<usize> {
    haystack
        .windows(needle.len())
        .position(|win| win == needle)
}

/// Read one HTTP request from the socket using Content-Length framing
/// (header until `\r\n\r\n`, then exactly `Content-Length` body bytes).
///
/// Previously this used `read_to_end` which only returns once the *client*
/// closes the connection (EOF) — curl never does, so every endpoint hung.
/// Returns the full request bytes (header + separator + body) for `handle`.
async fn read_http_request(sock: &mut tokio::net::TcpStream) -> std::io::Result<Vec<u8>> {
    const MAX_HEADER: usize = 64 * 1024;
    let mut buf = Vec::with_capacity(1 << 20);
    let header_end;
    loop {
        let mut chunk = [0u8; 4096];
        let n = sock.read(&mut chunk).await?;
        if n == 0 {
            // Peer closed the connection before a complete header arrived.
            if buf.is_empty() {
                return Ok(buf); // empty connection — caller should close
            }
            header_end = None;
            break;
        }
        buf.extend_from_slice(&chunk[..n]);
        if let Some(pos) = find_subsequence(&buf, b"\r\n\r\n") {
            header_end = Some(pos);
            break;
        }
        if buf.len() > MAX_HEADER {
            return Err(std::io::Error::new(
                std::io::ErrorKind::InvalidData,
                "request header too large",
            ));
        }
    }

    let Some(header_end) = header_end else {
        // No body separator — pass through what we got (handle tolerates it).
        return Ok(buf);
    };

    // Parse Content-Length (absent -> 0, e.g. a plain GET).
    let header = String::from_utf8_lossy(&buf[..header_end]);
    let content_length = header
        .lines()
        .find_map(|l| {
            let (k, v) = l.split_once(':')?;
            if k.trim().eq_ignore_ascii_case("content-length") {
                v.trim().parse::<usize>().ok()
            } else {
                None
            }
        })
        .unwrap_or(0);

    // Bytes already received past the separator are the start of the body.
    let mut body = buf.split_off(header_end + 4);
    if body.len() < content_length {
        let mut remaining = vec![0u8; content_length - body.len()];
        sock.read_exact(&mut remaining).await?;
        body.extend_from_slice(&remaining);
    }
    // Rebuild the full request buffer: header (+separator) + body.
    buf.extend_from_slice(&body);
    Ok(buf)
}

/// Spawn the test-sim HTTP server (only when `RGB_SIDECAR_TEST_LISTEN` is set).
pub fn spawn(engine: Arc<Mutex<RgbEngine>>, listen: &str) -> Result<()> {
    let addr: SocketAddr = listen.parse()?;
    tokio::spawn(async move {
        let listener = TcpListener::bind(addr).await.expect("bind test-sim");
        loop {
            let (mut sock, _) = match listener.accept().await {
                Ok(v) => v,
                Err(_) => continue,
            };
            let engine = engine.clone();
            tokio::spawn(async move {
                let buf = match read_http_request(&mut sock).await {
                    Ok(buf) if !buf.is_empty() => buf,
                    _ => return, // read error / empty connection — just close
                };
                let resp = handle(&buf, engine).await;
                let _ = sock.write_all(&resp).await;
                let _ = sock.flush().await;
            });
        }
    });
    Ok(())
}

async fn handle(buf: &[u8], engine: Arc<Mutex<RgbEngine>>) -> Vec<u8> {
    let s = String::from_utf8_lossy(buf);
    let mut lines = s.lines();
    let request_line = lines.next().unwrap_or_default();
    let mut parts = request_line.split_whitespace();
    let method = parts.next().unwrap_or("");
    let path = parts.next().unwrap_or("");
    let _version = parts.next().unwrap_or("");
    let body = match s.find("\r\n\r\n") {
        Some(idx) => buf.get(idx + 4..).unwrap_or_default().to_vec(),
        None => Vec::new(),
    };
    let body_str = String::from_utf8_lossy(&body);

    let mut eng = engine.lock().await;
    match (method, path) {
        ("POST", "/sim/user_pay") => {
            let req: serde_json::Value = serde_json::from_str(&body_str).unwrap_or(serde_json::json!({}));
            let invoice = req["invoice"].as_str().unwrap_or("");
            if invoice.is_empty() {
                return http_error(400, "invoice required");
            }
            match eng.simulate_user_pay_begin(invoice) {
                Ok(out) => http_response(
                    &serde_json::to_vec(&serde_json::json!({
                        "psbt": hex_encode(&out.psbt.serialize()),
                        "consignment": hex_encode(&out.consignment),
                        "txid": out.txid.to_string(),
                        "symbol": out.symbol,
                    }))
                    .unwrap_or_default(),
                ),
                Err(e) => http_error(500, &format!("{e:#}")),
            }
        }
        ("POST", "/sim/user_pay_submit") => {
            let req: serde_json::Value = serde_json::from_str(&body_str).unwrap_or(serde_json::json!({}));
            let psbt_hex = req["psbt"].as_str().unwrap_or("");
            let cons_hex = req["consignment"].as_str().unwrap_or("");
            let receive_id = req["receive_id"].as_str().unwrap_or("");
            if psbt_hex.is_empty() || cons_hex.is_empty() || receive_id.is_empty() {
                return http_error(400, "psbt, consignment, receive_id required");
            }
            match eng.simulate_user_pay_submit(psbt_hex, cons_hex, receive_id) {
                Ok(res) => http_response(
                    &serde_json::to_vec(&serde_json::json!({
                        "receive_id": res.receive_id, "status": res.status, "txid": res.txid,
                    }))
                    .unwrap_or_default(),
                ),
                Err(e) => http_error(500, &format!("{e:#}")),
            }
        }
        ("POST", "/sim/user_invoice") => {
            let req: serde_json::Value = serde_json::from_str(&body_str).unwrap_or(serde_json::json!({}));
            let symbol = req["asset_symbol"].as_str().unwrap_or("");
            let amount = req["amount"].as_i64().unwrap_or(0);
            match eng.build_user_invoice(symbol, amount) {
                Ok(invoice) => http_response(
                    &serde_json::to_vec(&serde_json::json!({"invoice": invoice})).unwrap_or_default(),
                ),
                Err(e) => http_error(500, &format!("{e:#}")),
            }
        }
        ("GET", "/sim/status") => {
            let mut seals = 0;
            let mut balance = 0i64;
            let mut assets = Vec::new();
            for a in eng.list_assets() {
                assets.push(serde_json::json!({"symbol": a.symbol, "asset_id": a.asset_id, "precision": a.precision}));
                let (settled, _) = eng.get_balance(&a.symbol);
                balance += settled;
                seals += eng.list_seals(&a.symbol).len();
            }
            http_response(
                &serde_json::to_vec(&serde_json::json!({
                    "assets": assets, "total_balance_usdt": balance, "total_seals": seals,
                    "synced_height": eng.synced_height(),
                }))
                .unwrap_or_default(),
            )
        }
        _ => http_error(404, "not found"),
    }
}

// Ensure the extension trait is considered used even if the HTTP server is disabled at runtime.
#[allow(dead_code)]
fn _assert_trait<E: SimulateExt>(_e: &E) {}
