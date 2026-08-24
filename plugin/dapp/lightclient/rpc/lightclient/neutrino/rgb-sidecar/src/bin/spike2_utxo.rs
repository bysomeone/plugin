//! Spike 2: UTXO recognition via bdk with a SINGLE-SCRIPT watch-only wallet synced from
//! a regtest indexer (electrs serving the Electrum protocol on 127.0.0.1:60401).
//!
//! Proves the full "watch-only single-script wallet" loop end-to-end:
//!   1. Build `wpkh(<TSS compressed pubkey>)` single-script wallet (zero derivation).
//!   2. Sync from the regtest Electrum indexer (bdk_electrum 0.24.0).
//!   3. Confirm the wallet sees the funded UTXO at the shared TSS script (balance / list_unspent).
//!
//! Prerequisites (see SPIKE.md):
//!   - bitcoind regtest running with data dir /tmp/rgb-spike/bitcoin
//!   - electrs 0.11.1 (built from source) serving Electrum on 127.0.0.1:60401
//!   - 1.5 BTC sent to bcrt1qw508d6qejxtdg4y5r3zarvary0c5xw7kygt080 and confirmed
//!     (sendtoaddress + generatetoaddress 3)

use std::str::FromStr;

use bdk_electrum::{electrum_client, BdkElectrumClient};
use bdk_wallet::bitcoin::{Address, CompressedPublicKey, Network, ScriptBuf};
use bdk_wallet::KeychainKind;
use bdk_wallet::Wallet;

const TSS_PUBKEY_HEX: &str =
    "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798";
const ELECTRUM_URL: &str = "127.0.0.1:60401";

fn main() -> anyhow::Result<()> {
    // --- 1. Single-script watch-only wallet (same as Spike 1) ---
    let pubkey = CompressedPublicKey::from_str(TSS_PUBKEY_HEX)?;
    let descriptor_str = format!("wpkh({})", TSS_PUBKEY_HEX);
    let mut wallet = Wallet::create_single(descriptor_str.clone())
        .network(Network::Regtest)
        .create_wallet_no_persist()?;

    let expected = Address::p2wpkh(&pubkey, Network::Regtest);
    println!("descriptor:      {}", descriptor_str);
    println!("shared address:  {}", expected);

    // --- 2. Connect to the regtest Electrum indexer ---
    let electrum = electrum_client::Client::new(ELECTRUM_URL)
        .map_err(|e| anyhow::anyhow!("electrum connect {ELECTRUM_URL}: {e}"))?;
    let client = BdkElectrumClient::new(electrum);
    println!("electrum:        {}", ELECTRUM_URL);

    // --- 3. Reveal the shared script (single-script: every index == the same script),
    // then sync all revealed spks. ---
    wallet.reveal_addresses_to(KeychainKind::External, 5).for_each(drop);
    let request = wallet.start_sync_with_revealed_spks().build();
    let response = client.sync(request, 10, true)?;
    wallet.apply_update(response)?;

    // --- 4. Assert the UTXO is recognized as owned ---
    let balance = wallet.balance();
    println!("balance:         total={} confirmed={} trusted_pending={} immature={}",
        balance.total(), balance.confirmed, balance.trusted_pending, balance.immature);
    assert!(balance.total().to_sat() > 0, "SPIKE2-FAIL: no balance recognized");

    let unspents: Vec<_> = wallet.list_unspent().collect();
    println!("list_unspent:    {} UTXOs", unspents.len());
    let shared_spk = ScriptBuf::from(expected.script_pubkey());
    for u in &unspents {
        println!(
            "  outpoint={} amount={} is_spent={} spk==shared={}",
            u.outpoint,
            u.txout.value,
            u.is_spent,
            u.txout.script_pubkey == shared_spk
        );
        assert_eq!(
            u.txout.script_pubkey, shared_spk,
            "SPIKE2-FAIL: UTXO not at the shared TSS script"
        );
        assert!(!u.is_spent);
    }
    assert!(!unspents.is_empty(), "SPIKE2-FAIL: no unspent outputs found");

    // --- 5. Peek index 0 and confirm derivation stays on the same script ---
    let addr0 = wallet.peek_address(KeychainKind::External, 0).address;
    assert_eq!(addr0, expected, "SPIKE2-FAIL: derivation index 0 changed script");
    println!("derivation:      all indexes still resolve to the same shared script");

    println!("SPIKE2-PASS: single-script watch-only wallet synced and owns the UTXO");
    Ok(())
}
