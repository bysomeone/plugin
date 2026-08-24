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
    /// Electrum server (electrs) URL, e.g. `127.0.0.1:60401`.
    pub electrum_url: String,
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

    pub fn stock_dir(&self) -> PathBuf {
        self.data_dir.join("stock")
    }
}
