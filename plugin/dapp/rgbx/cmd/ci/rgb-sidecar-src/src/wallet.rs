//! Watch-only single-script Bitcoin wallet (bdk) synced from an Electrum indexer.
//!
//! The descriptor is `wpkh(<TSS compressed pubkey>)` — zero BIP32 derivation, every
//! index resolves to the same P2WPKH script. The wallet holds NO private keys.

use std::str::FromStr;

use anyhow::{anyhow, Result};
use bdk_electrum::{electrum_client, BdkElectrumClient};
use bdk_wallet::bitcoin::{Address, CompressedPublicKey, Network, OutPoint, ScriptBuf};
use bdk_wallet::{KeychainKind, Wallet};

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
    wallet: Wallet,
    client: BdkElectrumClient<electrum_client::Client>,
    address: Address,
    script: ScriptBuf,
}

impl BtcWallet {
    pub fn new(electrum_url: &str, tss_pubkey_hex: &str, network: Network) -> Result<Self> {
        let pubkey = CompressedPublicKey::from_str(tss_pubkey_hex)
            .map_err(|e| anyhow!("invalid TSS pubkey {tss_pubkey_hex}: {e}"))?;
        let descriptor = format!("wpkh({tss_pubkey_hex})");
        let wallet = Wallet::create_single(descriptor)
            .network(network)
            .create_wallet_no_persist()?;
        let address = Address::p2wpkh(&pubkey, network);
        let script = ScriptBuf::from(address.script_pubkey());
        let electrum = electrum_client::Client::new(electrum_url)
            .map_err(|e| anyhow!("electrum connect {electrum_url}: {e}"))?;
        let client = BdkElectrumClient::new(electrum);
        Ok(Self {
            wallet,
            client,
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

    pub fn sync(&mut self) -> Result<()> {
        self.wallet
            .reveal_addresses_to(KeychainKind::External, 1)
            .for_each(drop);
        let request = self.wallet.start_sync_with_revealed_spks().build();
        let response = self.client.sync(request, 10, true)?;
        self.wallet.apply_update(response)?;
        Ok(())
    }

    pub fn list_unspent(&self) -> Vec<WalletUtxo> {
        self.wallet
            .list_unspent()
            .filter(|u| !u.is_spent)
            .map(|u| WalletUtxo {
                outpoint: u.outpoint,
                value: u.txout.value.to_sat(),
                script_pubkey: u.txout.script_pubkey.clone(),
                height: match u.chain_position {
                    bdk_chain::ChainPosition::Confirmed { anchor, .. } => Some(anchor.block_id.height),
                    _ => None,
                },
            })
            .collect()
    }
}
