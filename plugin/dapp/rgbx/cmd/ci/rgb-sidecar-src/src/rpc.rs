//! Minimal btcd JSON-RPC client (TLS-capable) — the sidecar's Bitcoin chain/indexer backend.
//!
//! Replaces the electrum-indexer dependency: the RGB sidecar reads the single btcd full node
//! directly (watch-only), matching production where BTC + RGB share one btcd chain.
//!
//! TLS semantics mirror the chain33 CLI's `btcDepositTx`: the node speaks TLS with a self-signed
//! cert (btcd auto-generates `rpc.cert`, SAN=hostname). The caller passes the cert path and it is
//! added as a trust root (btcd's cert is CA:TRUE), so no global TLS verification is disabled.

use std::path::Path;
use std::str::FromStr;
use std::sync::Arc;

use anyhow::{anyhow, Context, Result};
use base64::Engine;
use bitcoin::consensus::encode;
use bitcoin::hashes::hex::FromHex;
use bitcoin::{Address, Network, OutPoint, ScriptBuf, Transaction, Txid};
use rustls::pki_types::pem::PemObject;

/// A btcd JSON-RPC connection. Cheap to clone (shares the underlying ureq agent).
#[derive(Clone)]
pub struct BtcdRpc {
    url: String,
    user: String,
    pass: String,
    network: Network,
    agent: ureq::Agent,
}

/// One output view returned by `gettxout` / an entry of `searchrawtransactions`.
#[derive(Debug, Clone)]
pub struct TxOutInfo {
    pub value: u64,
    pub script_pubkey: ScriptBuf,
    /// Confirmation count at query time (0 == mempool).
    pub confirmations: u64,
}

/// One transaction touching a script returned by `searchrawtransactions` (btcd `--addrindex`).
#[derive(Debug, Clone)]
pub struct AddrTx {
    pub txid: Txid,
    pub confirmations: i64,
    pub outputs: Vec<(u32, TxOutInfo)>,
}

impl BtcdRpc {
    /// `host` is `host:port` (e.g. `btcd:18443`). When `cert` is set the client speaks HTTPS and
    /// trusts that PEM cert as a root; otherwise plain HTTP is used (test-only convenience, not
    /// the production path).
    pub fn connect(
        host: &str,
        user: &str,
        pass: &str,
        cert: Option<&Path>,
        network: Network,
    ) -> Result<Self> {
        let scheme = if cert.is_some() { "https" } else { "http" };
        let mut builder = ureq::AgentBuilder::new();
        if let Some(cert_path) = cert {
            let cert_der = rustls::pki_types::CertificateDer::from_pem_file(cert_path)
                .with_context(|| format!("read btcd cert {}", cert_path.display()))?;
            let mut roots = rustls::RootCertStore::empty();
            roots
                .add(cert_der)
                .context("add btcd self-signed cert to trust roots")?;
            let provider = Arc::new(rustls::crypto::aws_lc_rs::default_provider());
            let tls = rustls::ClientConfig::builder_with_provider(provider)
                .with_safe_default_protocol_versions()
                .context("rustls protocol versions")?
                .with_root_certificates(roots)
                .with_no_client_auth();
            builder = builder.tls_config(Arc::new(tls));
        }
        let agent = builder.build();
        Ok(Self {
            url: format!("{scheme}://{host}"),
            user: user.to_string(),
            pass: pass.to_string(),
            network,
            agent,
        })
    }

    fn call(&self, method: &str, params: serde_json::Value) -> Result<serde_json::Value> {
        let body = serde_json::json!({
            "jsonrpc": "1.0",
            "id": "rgb-sidecar",
            "method": method,
            "params": params,
        });
        let auth =
            base64::engine::general_purpose::STANDARD.encode(format!("{}:{}", self.user, self.pass));
        let resp = self
            .agent
            .post(&self.url)
            .set("Content-Type", "application/json")
            .set("Authorization", &format!("Basic {auth}"))
            .send_string(&body.to_string())
            .with_context(|| format!("btcd rpc {method}"))?;
        let text = resp.into_string().with_context(|| format!("btcd rpc {method} read"))?;
        let v: serde_json::Value = serde_json::from_str(&text).context("btcd rpc json parse")?;
        if let Some(err) = v.get("error") {
            if !err.is_null() {
                return Err(anyhow!("btcd rpc {method} error: {err}"));
            }
        }
        v.get("result")
            .cloned()
            .ok_or_else(|| anyhow!("btcd rpc {method}: no result"))
    }

    /// Best-chain height (`getblockcount`).
    pub fn get_block_count(&self) -> Result<u64> {
        self.call("getblockcount", serde_json::json!([]))?
            .as_u64()
            .ok_or_else(|| anyhow!("getblockcount not an int"))
    }

    /// Fetch a transaction by txid (btcd `--txindex` required). `Ok(None)` when unknown —
    /// mirrors electrum `transaction_get` semantics callers rely on for polling/optional lookups.
    pub fn get_transaction(&self, txid: &Txid) -> Result<Option<Transaction>> {
        let hex = match self.call("getrawtransaction", serde_json::json!([txid.to_string(), 0])) {
            Ok(serde_json::Value::String(s)) => s,
            Ok(_) => return Err(anyhow!("getrawtransaction {}: unexpected reply", txid)),
            Err(e) => {
                let msg = format!("{e:#}");
                if msg.contains("-5") || msg.contains("No Tx") || msg.contains("not found") {
                    return Ok(None);
                }
                return Err(e);
            }
        };
        let bytes = Vec::<u8>::from_hex(&hex).context("getrawtransaction hex decode")?;
        let tx: Transaction = encode::deserialize(&bytes).context("getrawtransaction tx decode")?;
        Ok(Some(tx))
    }

    /// Ask the node to mine `n` regtest blocks (btcd mines to `--miningaddr`, no wallet needed).
    pub fn mine_blocks(&self, n: u32) -> Result<()> {
        self.call("generate", serde_json::json!([n]))?;
        Ok(())
    }

    /// Broadcast a raw transaction, returning its txid.
    pub fn send_raw_transaction(&self, tx: &Transaction) -> Result<Txid> {
        let hex = encode::serialize_hex(tx);
        let res = self.call("sendrawtransaction", serde_json::json!([hex]))?;
        let s = res.as_str().ok_or_else(|| anyhow!("sendrawtransaction: unexpected reply"))?;
        Txid::from_str(s).context("sendrawtransaction txid parse")
    }

    /// `gettxout` for a single outpoint; `Ok(None)` when spent or never existed.
    pub fn get_txout(&self, outpoint: &OutPoint) -> Result<Option<TxOutInfo>> {
        let res = self.call(
            "gettxout",
            serde_json::json!([outpoint.txid.to_string(), outpoint.vout]),
        )?;
        if res.is_null() {
            return Ok(None);
        }
        let value_btc = res
            .get("value")
            .and_then(|v| v.as_f64())
            .ok_or_else(|| anyhow!("gettxout: no value"))?;
        let script_hex = res
            .pointer("/scriptPubKey/hex")
            .and_then(|v| v.as_str())
            .ok_or_else(|| anyhow!("gettxout: no scriptPubKey.hex"))?;
        let confirmations = res.get("confirmations").and_then(|v| v.as_u64()).unwrap_or(0);
        let script_bytes = Vec::<u8>::from_hex(script_hex).context("gettxout script hex")?;
        Ok(Some(TxOutInfo {
            value: (value_btc * 100_000_000.0).round() as u64,
            script_pubkey: ScriptBuf::from_bytes(script_bytes),
            confirmations,
        }))
    }

    /// `searchrawtransactions` (btcd + `--addrindex`): every confirmed/mempool tx paying to
    /// `script`, with the outputs that pay to `script`.
    pub fn search_txs_for_script(&self, script: &ScriptBuf) -> Result<Vec<AddrTx>> {
        let address = Address::from_script(script, self.network)
            .map_err(|_| anyhow!("script not an address for network"))?;
        let res = self.call("searchrawtransactions", serde_json::json!([address.to_string()]))?;
        let arr = res.as_array().cloned().unwrap_or_default();
        let mut out = Vec::with_capacity(arr.len());
        for item in arr {
            let Some(txid) = item
                .get("txid")
                .and_then(|v| v.as_str())
                .and_then(|s| Txid::from_str(s).ok())
            else {
                continue;
            };
            let confirmations = item.get("confirmations").and_then(|v| v.as_i64()).unwrap_or(0);
            let mut outputs = Vec::new();
            if let Some(vouts) = item.get("vout").and_then(|v| v.as_array()) {
                for vo in vouts {
                    let Some(n) = vo.get("n").and_then(|v| v.as_u64()) else { continue };
                    let Some(hex) = vo.pointer("/scriptPubKey/hex").and_then(|v| v.as_str()) else {
                        continue;
                    };
                    let Ok(bytes) = Vec::<u8>::from_hex(hex) else { continue };
                    if bytes == script.as_bytes() {
                        let value_btc = vo.get("value").and_then(|v| v.as_f64()).unwrap_or(0.0);
                        outputs.push((
                            n as u32,
                            TxOutInfo {
                                value: (value_btc * 100_000_000.0).round() as u64,
                                script_pubkey: ScriptBuf::from_bytes(bytes),
                                confirmations: confirmations.max(0) as u64,
                            },
                        ));
                    }
                }
            }
            out.push(AddrTx {
                txid,
                confirmations,
                outputs,
            });
        }
        Ok(out)
    }
}
