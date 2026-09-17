//! Sidecar configuration.

use std::path::PathBuf;

use bitcoin::Network;

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
