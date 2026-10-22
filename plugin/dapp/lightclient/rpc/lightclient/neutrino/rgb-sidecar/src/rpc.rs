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
use rustls::client::danger::{HandshakeSignatureValid, ServerCertVerified, ServerCertVerifier};
use rustls::pki_types::pem::PemObject;
use rustls::pki_types::{CertificateDer, ServerName, UnixTime};
use rustls::{DigitallySignedStruct, SignatureScheme};

/// Pins the btcd RPC self-signed cert. btcd's `rpc.cert` has CA:TRUE, so webpki rejects it as an
/// end-entity (`CaUsedAsEndEntity`) when added to a root store; pinning the exact DER is the
/// standard way to trust a self-signed server cert.
#[derive(Debug)]
struct PinnedCertVerifier {
    pinned: CertificateDer<'static>,
    provider: Arc<rustls::crypto::CryptoProvider>,
}

impl ServerCertVerifier for PinnedCertVerifier {
    fn verify_server_cert(
        &self,
        end_entity: &CertificateDer<'_>,
        _intermediates: &[CertificateDer<'_>],
        _server_name: &ServerName<'_>,
        _ocsp_response: &[u8],
        _now: UnixTime,
    ) -> Result<ServerCertVerified, rustls::Error> {
        if end_entity.as_ref() == self.pinned.as_ref() {
            Ok(ServerCertVerified::assertion())
        } else {
            Err(rustls::Error::General(
                "btcd RPC cert does not match pinned rpc.cert".into(),
            ))
        }
    }

    fn verify_tls12_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, rustls::Error> {
        rustls::crypto::verify_tls12_signature(
            message,
            cert,
            dss,
            &self.provider.signature_verification_algorithms,
        )
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, rustls::Error> {
        rustls::crypto::verify_tls13_signature(
            message,
            cert,
            dss,
            &self.provider.signature_verification_algorithms,
        )
    }

    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        self.provider
            .signature_verification_algorithms
            .supported_schemes()
    }
}

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
            let pinned = CertificateDer::from_pem_file(cert_path)
                .with_context(|| format!("read btcd cert {}", cert_path.display()))?;
            let provider = Arc::new(rustls::crypto::aws_lc_rs::default_provider());
            let verifier = PinnedCertVerifier {
                pinned,
                provider: provider.clone(),
            };
            let tls = rustls::ClientConfig::builder_with_provider(provider)
                .with_safe_default_protocol_versions()
                .context("rustls protocol versions")?
                .dangerous()
                .with_custom_certificate_verifier(Arc::new(verifier))
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

    /// Send several calls in one HTTP request (JSON-RPC batch) and return their results in the
    /// request order.
    ///
    /// Batching is a *transport* optimisation only: every call keeps its own method, params and
    /// error. Any per-item error (or a node that answers a batch with a single object / not at
    /// all) fails the whole batch, and the callers below fall back to the sequential per-call
    /// path — so an old or stricter node stays correct, just slower.
    fn call_batch(&self, calls: &[(&str, serde_json::Value)]) -> Result<Vec<serde_json::Value>> {
        if calls.is_empty() {
            return Ok(Vec::new());
        }
        let bodies: Vec<serde_json::Value> = calls
            .iter()
            .enumerate()
            .map(|(id, (method, params))| {
                serde_json::json!({
                    "jsonrpc": "1.0",
                    "id": id,
                    "method": method,
                    "params": params,
                })
            })
            .collect();
        let auth = base64::engine::general_purpose::STANDARD
            .encode(format!("{}:{}", self.user, self.pass));
        let resp = self
            .agent
            .post(&self.url)
            .set("Content-Type", "application/json")
            .set("Authorization", &format!("Basic {auth}"))
            .send_string(&serde_json::Value::Array(bodies).to_string())
            .context("btcd rpc batch")?;
        let text = resp.into_string().context("btcd rpc batch read")?;
        let v: serde_json::Value =
            serde_json::from_str(&text).context("btcd rpc batch json parse")?;
        let arr = v
            .as_array()
            .ok_or_else(|| anyhow!("btcd rpc batch: expected an array reply"))?;
        let mut out: Vec<Option<serde_json::Value>> = (0..calls.len()).map(|_| None).collect();
        for item in arr {
            let id = item
                .get("id")
                .and_then(|v| v.as_u64())
                .ok_or_else(|| anyhow!("btcd rpc batch: response without a numeric id"))?
                as usize;
            let slot = out
                .get_mut(id)
                .ok_or_else(|| anyhow!("btcd rpc batch: unknown response id {id}"))?;
            if let Some(err) = item.get("error") {
                if !err.is_null() {
                    return Err(anyhow!("btcd rpc batch item {id} error: {err}"));
                }
            }
            *slot = Some(
                item.get("result")
                    .cloned()
                    .ok_or_else(|| anyhow!("btcd rpc batch item {id}: no result"))?,
            );
        }
        out.into_iter()
            .enumerate()
            .map(|(id, slot)| slot.ok_or_else(|| anyhow!("btcd rpc batch: no reply for id {id}")))
            .collect()
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
        let res = self.call("gettxout", txout_params(outpoint))?;
        parse_txout(res)
    }

    /// `gettxout` for many outpoints in batched requests. The returned vector is index-aligned with
    /// `outpoints`; an outpoint the node reports an error for is `None` (the pre-existing "not a
    /// live UTXO" reading), and a batch the node cannot serve falls back to single calls.
    pub fn get_txouts(&self, outpoints: &[OutPoint]) -> Vec<Option<TxOutInfo>> {
        let mut out = Vec::with_capacity(outpoints.len());
        for chunk in outpoints.chunks(TXOUT_BATCH) {
            let calls: Vec<(&str, serde_json::Value)> = chunk
                .iter()
                .map(|op| ("gettxout", txout_params(op)))
                .collect();
            match self.call_batch(&calls) {
                Ok(results) => {
                    for result in results {
                        out.push(parse_txout(result).unwrap_or(None));
                    }
                }
                Err(_) => {
                    // Batch unsupported or failed: the pre-O4 per-outpoint path. An error is a
                    // "not live" answer here, exactly as before.
                    for op in chunk {
                        out.push(self.get_txout(op).unwrap_or(None));
                    }
                }
            }
        }
        out
    }

    /// `searchrawtransactions` (btcd + `--addrindex`): every confirmed/mempool tx paying to
    /// `script`, with the outputs that pay to `script`.
    pub fn search_txs_for_script(&self, script: &ScriptBuf) -> Result<Vec<AddrTx>> {
        let address = Address::from_script(script, self.network)
            .map_err(|_| anyhow!("script not an address for network"))?;
        let res = self.call(
            "searchrawtransactions",
            serde_json::json!([address.to_string()]),
        )?;
        Ok(self.parse_search_result(script, res))
    }

    /// Batched [`Self::search_txs_for_script`]. Results are index-aligned with `scripts`; a script
    /// that cannot be encoded as an address, whose call fails, or whose batch failed to be served
    /// keeps the per-script error (callers treat that as an empty set, as before).
    pub fn search_txs_for_scripts(&self, scripts: &[ScriptBuf]) -> Vec<Result<Vec<AddrTx>>> {
        let mut slots: Vec<Option<Result<Vec<AddrTx>>>> = scripts.iter().map(|_| None).collect();
        let mut pending: Vec<(usize, String)> = Vec::with_capacity(scripts.len());
        for (i, script) in scripts.iter().enumerate() {
            match Address::from_script(script, self.network) {
                Ok(address) => pending.push((i, address.to_string())),
                Err(_) => {
                    slots[i] = Some(Err(anyhow!("script not an address for network")));
                }
            }
        }
        for chunk in pending.chunks(SEARCH_BATCH) {
            let calls: Vec<(&str, serde_json::Value)> = chunk
                .iter()
                .map(|(_, address)| ("searchrawtransactions", serde_json::json!([address])))
                .collect();
            match self.call_batch(&calls) {
                Ok(results) => {
                    for ((index, _), result) in chunk.iter().zip(results) {
                        slots[*index] =
                            Some(Ok(self.parse_search_result(&scripts[*index], result)));
                    }
                }
                Err(_) => {
                    for (index, _) in chunk {
                        slots[*index] = Some(self.search_txs_for_script(&scripts[*index]));
                    }
                }
            }
        }
        slots
            .into_iter()
            .map(|slot| {
                slot.unwrap_or_else(|| Err(anyhow!("searchrawtransactions: no reply for script")))
            })
            .collect()
    }

    fn parse_search_result(&self, script: &ScriptBuf, res: serde_json::Value) -> Vec<AddrTx> {
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
            let confirmations = item
                .get("confirmations")
                .and_then(|v| v.as_i64())
                .unwrap_or(0);
            let mut outputs = Vec::new();
            if let Some(vouts) = item.get("vout").and_then(|v| v.as_array()) {
                for vo in vouts {
                    let Some(n) = vo.get("n").and_then(|v| v.as_u64()) else {
                        continue;
                    };
                    let Some(hex) = vo.pointer("/scriptPubKey/hex").and_then(|v| v.as_str()) else {
                        continue;
                    };
                    let Ok(bytes) = Vec::<u8>::from_hex(hex) else {
                        continue;
                    };
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
        out
    }
}

/// JSON-RPC batch size for `searchrawtransactions`. A response can carry every matching tx, so the
/// cap bounds peak memory (round-trips are already ~one per chunk).
const SEARCH_BATCH: usize = 64;
/// `gettxout` replies are tiny; the chunk cap only exists to bound request/response size.
const TXOUT_BATCH: usize = 1_000;

fn txout_params(outpoint: &OutPoint) -> serde_json::Value {
    serde_json::json!([outpoint.txid.to_string(), outpoint.vout])
}

fn parse_txout(res: serde_json::Value) -> Result<Option<TxOutInfo>> {
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
    let confirmations = res
        .get("confirmations")
        .and_then(|v| v.as_u64())
        .unwrap_or(0);
    let script_bytes = Vec::<u8>::from_hex(script_hex).context("gettxout script hex")?;
    Ok(Some(TxOutInfo {
        value: (value_btc * 100_000_000.0).round() as u64,
        script_pubkey: ScriptBuf::from_bytes(script_bytes),
        confirmations,
    }))
}
