//! RGB sidecar — Phase 2a: gRPC service (tonic) + application layer.
//!
//! The sidecar provides RGB consensus to the Go bridge (chain33) in watch-only,
//! single-script mode. The TSS group holds the only signing key (a bare secp256k1
//! pubkey, no chain code); this crate never holds private keys.

/// Generated gRPC types + service trait from `proto/rgb_sidecar.proto`.
pub mod pb {
    tonic::include_proto!("rgb_sidecar");
}

pub mod config;
pub mod engine;
pub mod invoice;
pub mod ledger;
pub mod service;
pub mod test_sim;
pub mod types;
pub mod wallet;

pub use engine::RgbEngine;
pub use types::{SealStatus, SealTxOut};
