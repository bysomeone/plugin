//! Sidecar configuration.

use std::path::{Path, PathBuf};

use anyhow::{anyhow, Context, Result};
use bitcoin::hex::FromHex;
use bitcoin::Network;
use serde::Deserialize;

/// Runtime configuration of the RGB sidecar.
///
/// The TSS group's *bare compressed* secp256k1 pubkey is the single descriptor key
/// (`wpkh(<pubkey>)`), so the sidecar never holds a private key.
#[derive(Clone, Debug)]
pub struct Config {
    /// Directory for persisted state (Stock bin files + `ledger.json`).
    pub data_dir: PathBuf,
    /// btcd JSON-RPC endpoint, `host:port` (e.g. `btcd:18443`). TLS is used when
    /// `btc_rpc_cert` is set (btcd serves a self-signed cert, CA:TRUE).
    pub btc_rpc_host: String,
    /// btcd RPC user.
    pub btc_rpc_user: String,
    /// btcd RPC password.
    pub btc_rpc_pass: String,
    /// Optional path to btcd's `rpc.cert` (PEM) to trust for TLS. `None` => plain HTTP
    /// (test-only convenience; the production path always sets the cert).
    pub btc_rpc_cert: Option<PathBuf>,
    /// Bitcoin network (regtest/testnet/mainnet).
    pub network: Network,
    /// Compressed pubkey hex of the TSS key. `wpkh(<this>)` is the descriptor.
    pub tss_pubkey_hex: String,
    /// gRPC listen address, e.g. `0.0.0.0:50061`.
    pub grpc_listen: String,
    /// Contracts this deployment declares. Adopted during `RgbEngine::open`, and a declaration
    /// that does not hold up **fails the start** (see `engine::apply_declared_contracts`).
    pub contracts: Vec<ContractDecl>,
}

/// One contract a deployment declares, from the file `RGB_SIDECAR_CONTRACTS` points at:
///
/// ```json
/// { "contracts": [
///     { "symbol": "USDT", "assetId": "rgb:...", "genesisConsignment": "<hex>" },
///     { "symbol": "USDT2", "assetId": "rgb:...", "genesisConsignmentFile": "/etc/rgb/usdt.genesis.hex" }
/// ] }
/// ```
///
/// The file (rather than an env list) because a genesis consignment is ~12 KB of hex per
/// contract — far past what an env var should carry, and a mounted file is the natural shape
/// for something an operator has to be able to diff and rotate.
#[derive(Clone, Debug, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct ContractDecl {
    /// The sidecar's own name for the asset — the `asset_symbol` every other RPC takes.
    pub symbol: String,
    /// The contract id the operator expects these bytes to be (`rgb:...`). Verified against the
    /// consignment at adopt time, so a stale or mixed-up declaration fails loudly instead of
    /// silently registering the wrong asset.
    pub asset_id: String,
    /// The genesis consignment, hex-encoded inline. Exclusive with `genesis_consignment_file`.
    #[serde(default)]
    pub genesis_consignment: Option<String>,
    /// Path to a file holding that same hex. Exclusive with `genesis_consignment`.
    #[serde(default)]
    pub genesis_consignment_file: Option<PathBuf>,
}

impl ContractDecl {
    /// The declared consignment bytes, from whichever source this declaration used.
    pub fn genesis_bytes(&self) -> Result<Vec<u8>> {
        let hex = match (&self.genesis_consignment, &self.genesis_consignment_file) {
            (Some(_), Some(_)) => {
                return Err(anyhow!(
                    "contract {}: set either genesisConsignment or genesisConsignmentFile, not both",
                    self.symbol
                ))
            }
            (None, None) => {
                return Err(anyhow!(
                    "contract {}: one of genesisConsignment or genesisConsignmentFile is required",
                    self.symbol
                ))
            }
            (Some(inline), None) => inline.clone(),
            (None, Some(path)) => std::fs::read_to_string(path).with_context(|| {
                format!(
                    "contract {}: read genesisConsignmentFile {}",
                    self.symbol,
                    path.display()
                )
            })?,
        };
        // Whitespace is stripped rather than only trimmed: a wrapped or newline-terminated hex
        // file is a normal thing for an operator to produce, and failing on it would be a
        // config puzzle, not a safety property.
        let compact: String = hex.chars().filter(|c| !c.is_whitespace()).collect();
        if compact.is_empty() {
            return Err(anyhow!("contract {}: the genesis consignment is empty", self.symbol));
        }
        Vec::<u8>::from_hex(&compact)
            .map_err(|e| anyhow!("contract {}: genesis consignment is not valid hex: {e}", self.symbol))
    }
}

/// Read the declarations file `RGB_SIDECAR_CONTRACTS` points at.
pub fn load_contract_declarations(path: &Path) -> Result<Vec<ContractDecl>> {
    #[derive(Deserialize)]
    #[serde(rename_all = "camelCase", deny_unknown_fields)]
    struct Declarations {
        #[serde(default)]
        contracts: Vec<ContractDecl>,
    }

    let raw = std::fs::read_to_string(path)
        .with_context(|| format!("read RGB_SIDECAR_CONTRACTS file {}", path.display()))?;
    let parsed: Declarations = serde_json::from_str(&raw)
        .with_context(|| format!("parse RGB_SIDECAR_CONTRACTS file {}", path.display()))?;
    for c in &parsed.contracts {
        if c.symbol.is_empty() {
            return Err(anyhow!("{}: a contract entry has an empty symbol", path.display()));
        }
        if c.asset_id.is_empty() {
            return Err(anyhow!("{}: contract {} has an empty assetId", path.display(), c.symbol));
        }
    }
    Ok(parsed.contracts)
}

impl Config {
    pub fn tss_descriptor(&self) -> String {
        format!("wpkh({})", self.tss_pubkey_hex)
    }

    pub fn ledger_path(&self) -> PathBuf {
        self.data_dir.join("ledger.json")
    }

    /// 提现构建的冷层归档目录（O3）：`builds/<sha256(seal 集合)>.json`，每笔提现一个文件。
    /// 与热账本分开，好让热账本的每次全量重写不再拖着全部提现历史。
    pub fn builds_dir(&self) -> PathBuf {
        self.data_dir.join("builds")
    }

    /// 用户 P2WSH 充值脚本注册表（program → witnessScript + userID）。必须持久化：重启后
    /// watch 集一丢，已经打进用户充值地址的 BTC 就没有 witnessScript 可签（花不掉）。
    pub fn user_scripts_path(&self) -> PathBuf {
        self.data_dir.join("user_scripts.json")
    }

    pub fn stock_dir(&self) -> PathBuf {
        self.data_dir.join("stock")
    }
}

#[cfg(test)]
mod test {
    use super::*;

    fn decl_file(name: &str, body: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!(
            "rgb-sidecar-decl-test-{}-{:?}",
            std::process::id(),
            std::thread::current().id()
        ));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join(name);
        std::fs::write(&path, body).unwrap();
        path
    }

    #[test]
    fn declarations_load_from_inline_hex_and_from_a_file() {
        let path = decl_file(
            "inline.json",
            r#"{"contracts":[
                 {"symbol":"USDT","assetId":"rgb:v_TEST-1","genesisConsignment":"deadbeef"}
               ]}"#,
        );
        let decls = load_contract_declarations(&path).unwrap();
        assert_eq!(decls.len(), 1);
        assert_eq!(decls[0].symbol, "USDT");
        assert_eq!(decls[0].asset_id, "rgb:v_TEST-1");
        assert_eq!(decls[0].genesis_bytes().unwrap(), vec![0xde, 0xad, 0xbe, 0xef]);

        // A file source, with the newlines + indent a wrapped hex invites.
        let hex_path = decl_file("consignment.hex", "dead\n  beef\n");
        let path = decl_file(
            "file.json",
            &format!(
                r#"{{"contracts":[{{"symbol":"USDT","assetId":"rgb:v_TEST-1",
                   "genesisConsignmentFile":{}}}]}}"#,
                serde_json::to_string(&hex_path).unwrap()
            ),
        );
        let decls = load_contract_declarations(&path).unwrap();
        assert_eq!(
            decls[0].genesis_bytes().unwrap(),
            vec![0xde, 0xad, 0xbe, 0xef],
            "whitespace inside a hex file must not be a config puzzle"
        );
    }

    #[test]
    fn a_declaration_must_name_exactly_one_source() {
        let both = ContractDecl {
            symbol: "USDT".into(),
            asset_id: "rgb:v_X".into(),
            genesis_consignment: Some("deadbeef".into()),
            genesis_consignment_file: Some("/tmp/whatever".into()),
        };
        let err = both.genesis_bytes().unwrap_err();
        assert!(format!("{err:#}").contains("not both"), "got: {err:#}");

        let neither = ContractDecl {
            symbol: "USDT".into(),
            asset_id: "rgb:v_X".into(),
            genesis_consignment: None,
            genesis_consignment_file: None,
        };
        let err = neither.genesis_bytes().unwrap_err();
        assert!(format!("{err:#}").contains("is required"), "got: {err:#}");

        let missing = ContractDecl {
            symbol: "USDT".into(),
            asset_id: "rgb:v_X".into(),
            genesis_consignment: None,
            genesis_consignment_file: Some("/nonexistent/consignment.hex".into()),
        };
        let err = missing.genesis_bytes().unwrap_err();
        assert!(format!("{err:#}").contains("read genesisConsignmentFile"), "got: {err:#}");
    }

    #[test]
    fn malformed_declarations_are_rejected() {
        // A typo'd key is a config error, not a silently ignored field.
        let path = decl_file(
            "typo.json",
            r#"{"contracts":[{"symbol":"USDT","assetID":"rgb:v_X","genesisConsignment":"00"}]}"#,
        );
        let err = load_contract_declarations(&path).unwrap_err();
        assert!(format!("{err:#}").contains("parse RGB_SIDECAR_CONTRACTS file"), "got: {err:#}");

        let path = decl_file(
            "empty-symbol.json",
            r#"{"contracts":[{"symbol":"","assetId":"rgb:v_X","genesisConsignment":"00"}]}"#,
        );
        assert!(format!("{:#}", load_contract_declarations(&path).unwrap_err()).contains("empty symbol"));

        let path = decl_file(
            "empty-asset.json",
            r#"{"contracts":[{"symbol":"USDT","assetId":"","genesisConsignment":"00"}]}"#,
        );
        assert!(format!("{:#}", load_contract_declarations(&path).unwrap_err()).contains("empty assetId"));

        let path = decl_file(
            "not-hex.json",
            r#"{"contracts":[{"symbol":"USDT","assetId":"rgb:v_X","genesisConsignment":"zz"}]}"#,
        );
        assert!(format!("{:#}", load_contract_declarations(&path).unwrap()[0].genesis_bytes().unwrap_err())
            .contains("not valid hex"));
    }
}
