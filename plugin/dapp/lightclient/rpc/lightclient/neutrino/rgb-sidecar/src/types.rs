//! Internal record types for the sidecar's application layer.

use serde::{Deserialize, Serialize};

/// Lifecycle of a single RGB seal (a UTXO carrying RGB state) owned by the sidecar.
///
/// - `PendingMint`: a tx that creates this seal was built/signed/broadcast but not yet
///   observed as confirmed at the sidecar's synced height.
/// - `Minted`: the seal's tx is confirmed and the seal is available to be spent.
/// - `Consumed`: the seal was spent as an input of a newer state transition.
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
pub enum SealStatus {
    PendingMint,
    Minted,
    Consumed,
}

impl SealStatus {
    pub fn as_str(&self) -> &'static str {
        match self {
            SealStatus::PendingMint => "pending-mint",
            SealStatus::Minted => "minted",
            SealStatus::Consumed => "consumed",
        }
    }
    pub fn from_str(s: &str) -> Self {
        match s {
            "pending-mint" => SealStatus::PendingMint,
            "consumed" => SealStatus::Consumed,
            _ => SealStatus::Minted,
        }
    }
}

/// An RGB seal (outpoint + assigned asset state) owned by the sidecar.
///
/// All seals live under the *same* TSS script (single-script constraint); they are
/// distinguished by `outpoint` (and, when the seal is confidential, by `secret_seal_hex`).
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct SealTxOut {
    /// "txid:vout" of the UTXO carrying the RGB state.
    pub outpoint: String,
    /// `rgb:...` contract id.
    pub asset_id: String,
    /// Bridge-facing symbol (e.g. "USDT").
    pub asset_symbol: String,
    /// Asset amount in the asset's minimum units.
    pub amount: i64,
    /// Bitcoin value of the UTXO in satoshis.
    pub btc_value: i64,
    /// Block height at which the carrying tx was confirmed (0 = unconfirmed).
    pub maturity_height: u32,
    pub status: SealStatus,
    /// Hex-encoded `SecretSeal` (blinding factor) when this seal was created via a blinded
    /// receive; `None` for witness/explicit receives.
    pub secret_seal_hex: Option<String>,
}

/// Receive status strings (mirrors proto `TransferState.status`).
pub mod recv_status {
    pub const WAITING_COUNTERPARTY: &str = "waiting-counterparty";
    pub const SETTLED: &str = "settled";
    pub const FAILED: &str = "failed";
    pub const TIMEOUT: &str = "timeout";
}
