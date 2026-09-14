//! Watch-only single-script Bitcoin wallet backed by the btcd JSON-RPC node.
//!
//! The descriptor is `wpkh(<TSS compressed pubkey>)` — zero BIP32 derivation, every index
//! resolves to the same P2WPKH script. The wallet holds NO private keys; it discovers UTXOs by
//! querying btcd's `searchrawtransactions` (address index) + `gettxout`.

use std::str::FromStr;
use std::sync::Arc;

use anyhow::{anyhow, Result};
use bitcoin::{Address, CompressedPublicKey, Network, OutPoint, ScriptBuf};

use crate::rpc::BtcdRpc;

/// Test-only: derive the E2E "user" P2WPKH address from a fixed test secret ([0x22;32]).
/// Used by the test-sim driver to build user-side RGB invoices; the sidecar still holds NO keys.
pub fn user_test_address(network: Network) -> Result<Address> {
    use bitcoin::secp256k1::Secp256k1;
    let secp = Secp256k1::new();
    let sk = bitcoin::secp256k1::SecretKey::from_slice(&[0x22u8; 32])?;
    let pk = bitcoin::secp256k1::PublicKey::from_secret_key(&secp, &sk);
    let compressed = CompressedPublicKey(pk);
    Ok(Address::p2wpkh(&compressed, network))
}

/// A confirmed/unconfirmed UTXO of the single-script wallet.
#[derive(Clone, Debug)]
pub struct WalletUtxo {
    pub outpoint: OutPoint,
    pub value: u64,
    pub script_pubkey: ScriptBuf,
    pub height: Option<u32>,
}

pub struct BtcWallet {
    rpc: Arc<BtcdRpc>,
    address: Address,
    script: ScriptBuf,
}

impl BtcWallet {
    pub fn new(rpc: Arc<BtcdRpc>, tss_pubkey_hex: &str, network: Network) -> Result<Self> {
        // chain33 getCrossChainInfo returns pubkey with a "0x" prefix; strip it before hex parsing.
        let hex = tss_pubkey_hex.strip_prefix("0x").unwrap_or(tss_pubkey_hex);
        let pubkey = CompressedPublicKey::from_str(hex)
            .map_err(|e| anyhow!("invalid TSS pubkey {tss_pubkey_hex}: {e}"))?;
        let address = Address::p2wpkh(&pubkey, network);
        let script = ScriptBuf::from(address.script_pubkey());
        Ok(Self {
            rpc,
            address,
            script,
        })
    }

    pub fn address(&self) -> &Address {
        &self.address
    }

    pub fn script(&self) -> &ScriptBuf {
        &self.script
    }

    /// btcd is queried live on `list_unspent`, so there is nothing to sync. Kept for API parity.
    pub fn sync(&mut self) -> Result<()> {
        Ok(())
    }

    /// Live UTXO set of the single TSS script, discovered via btcd's address index.
    pub fn list_unspent(&self) -> Vec<WalletUtxo> {
        let Ok(txs) = self.rpc.search_txs_for_script(&self.script) else {
            return Vec::new();
        };
        let best = self.rpc.get_block_count().unwrap_or(0);
        let mut out = Vec::new();
        for tx in txs {
            for (vout, txout) in tx.outputs {
                let outpoint = OutPoint {
                    txid: tx.txid,
                    vout,
                };
                // Still unspent?
                let Ok(Some(live)) = self.rpc.get_txout(&outpoint) else {
                    continue;
                };
                let height = if live.confirmations > 0 {
                    // gettxout confirmations is relative to the chain tip at query time.
                    Some((best.saturating_sub(live.confirmations - 1)) as u32)
                } else {
                    None
                };
                out.push(WalletUtxo {
                    outpoint,
                    value: live.value,
                    script_pubkey: txout.script_pubkey.clone(),
                    height,
                });
            }
        }
        out
    }
}
