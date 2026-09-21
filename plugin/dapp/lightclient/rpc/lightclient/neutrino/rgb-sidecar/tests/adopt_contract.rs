//! Batch 1: adopting a contract that **somebody else issued**.
//!
//! The bridge has to serve Tether's USDT — a contract it did not create and cannot re-create.
//! So a contract reaches a sidecar in exactly one way: it is handed the contract's **genesis
//! consignment** and adopts it (`RgbEngine::adopt_contract`). Issuance is no longer a second,
//! privileged path: it builds a contract, exports those very bytes and adopts them, which is
//! what the first half of this test pins.
//!
//! The invariant that makes adoption safe is stated negatively: **adopting never invents a
//! balance**. The genesis seal belongs to the issuer — for Tether's contract it is an outpoint
//! this bridge has never owned — so recording it would count someone else's money as ours.
//! Only the issuer path claims a seal, and it proves the outpoint against the TSS wallet first
//! (see `claim_genesis_seal`); the second test pins that refusal.

mod common;

use anyhow::Result;
use bitcoin::hex::FromHex;
use common::{data_dir, funding_tx, open_engine, pubkey_hex, p2wpkh, start_mock_btcd};
use rgbcore::ContractId;
use std::str::FromStr;

const TSS_SECRET: [u8; 32] = [0x11; 32];
const SYMBOL: &str = "USDT";
const ISSUED: i64 = 100_000_000;
/// The genesis seal's BTC value: irrelevant to RGB, just needs to exist on the mock chain.
const GENESIS_BTC: u64 = 700;

#[test]
fn a_published_genesis_is_adopted_by_a_fresh_engine_without_a_balance() -> Result<()> {
    let (rpc, chain) = start_mock_btcd();
    let tss_pubkey = pubkey_hex(&TSS_SECRET);
    let (_tss_addr, tss_script) = p2wpkh(&TSS_SECRET);
    let (genesis_funding, genesis_outpoint) = funding_tx(&tss_script, GENESIS_BTC, 1);
    chain.lock().unwrap().add_tx(genesis_funding);

    // ---- the issuer: builds a contract and publishes its genesis consignment ----
    let mut issuer = open_engine(&data_dir("adopt-issuer"), &rpc, &tss_pubkey)?;
    let published = issuer.issue_asset_at(SYMBOL, "Tether USD", 8, ISSUED as u64, genesis_outpoint)?;
    let genesis_bytes = Vec::<u8>::from_hex(&published.genesis_consignment_hex)?;
    // The issuer does hold the genesis UTXO, so its balance is the supply it minted.
    assert_eq!(
        issuer.get_balance(SYMBOL),
        (ISSUED, 0),
        "the issuer's own genesis seal must be a spendable balance"
    );

    // ---- a completely fresh sidecar (empty ledger, empty Stock) adopts it ----
    let mut adopter = open_engine(&data_dir("adopt-fresh"), &rpc, &tss_pubkey)?;
    assert!(adopter.ledger.assets.is_empty(), "adopter must start with no assets");
    assert!(adopter.list_seals(SYMBOL).is_empty(), "adopter must start with no seals");

    let adopted = adopter.adopt_contract(&genesis_bytes, SYMBOL)?;

    // (a) the asset is known, and identical to what the issuer published
    assert_eq!(adopted.asset_id, published.asset_id, "adopted contract id");
    assert_eq!(adopted.symbol, SYMBOL);
    assert_eq!(adopted.precision, 8, "precision read from the contract itself");
    assert_eq!(adopted.issued_supply, ISSUED, "genesis allocation read from the contract");
    assert_eq!(
        adopted.schema, "InflatableFungibleAsset",
        "schema name comes from the contract, not from a hard-coded string"
    );
    assert_eq!(
        adopter.ledger.asset(SYMBOL).map(|a| a.asset_id.clone()),
        Some(published.asset_id.clone()),
        "the adopter's ledger carries the asset under the requested symbol"
    );

    // (b) no balance was invented out of the issuer's genesis seal
    assert_eq!(
        adopter.get_balance(SYMBOL),
        (0, 0),
        "adopting must not claim the issuer's genesis seal as the bridge's balance"
    );
    assert!(
        adopter.list_seals(SYMBOL).is_empty(),
        "adopting must not write any seal: the balance can only come from a deposit"
    );

    // (c) the contract is usable in the Stock (a transfer transition can be built for it)
    let contract_id = ContractId::from_str(&adopted.asset_id)?;
    adopter
        .stock
        .transition_builder(contract_id, "transfer")
        .expect("the adopted contract must be usable for a transfer");

    // ---- adoption is idempotent, and one contract maps to exactly one symbol ----
    let again = adopter.adopt_contract(&genesis_bytes, SYMBOL)?;
    assert_eq!(again.asset_id, adopted.asset_id, "re-adopting the same contract is a no-op");
    assert_eq!(adopter.ledger.assets.len(), 1, "re-adopting must not duplicate the record");

    let same_contract_other_symbol = adopter.adopt_contract(&genesis_bytes, "USDT-COPY");
    assert!(
        same_contract_other_symbol.is_err(),
        "one contract must not be split across two sidecar symbols"
    );
    assert!(
        format!("{:#}", same_contract_other_symbol.unwrap_err()).contains("already registered"),
        "the refusal must say the contract is already registered"
    );

    // A *different* contract may not take over the symbol either (that would strand every seal
    // and receive already recorded under it).
    let (other_funding, other_outpoint) = funding_tx(&tss_script, GENESIS_BTC, 2);
    chain.lock().unwrap().add_tx(other_funding);
    let other = issuer.issue_asset_at("FAKEUSDT", "Not Tether", 8, 1, other_outpoint)?;
    assert_ne!(other.asset_id, adopted.asset_id, "the two issuances must be different contracts");
    let other_bytes = Vec::<u8>::from_hex(&other.genesis_consignment_hex)?;
    let stolen = adopter.adopt_contract(&other_bytes, SYMBOL);
    assert!(stolen.is_err(), "another contract must not take a symbol already in use");
    assert!(
        format!("{:#}", stolen.unwrap_err()).contains("already registered"),
        "the refusal must name the symbol collision"
    );

    Ok(())
}

/// The other half of the same invariant: the issuer path claims a genesis seal **only** for an
/// outpoint the TSS wallet actually holds. Without that check, pointing an issuance at an
/// outpoint the bridge does not own (an adopted issuer's genesis, say) would fabricate a balance.
#[test]
fn claiming_a_genesis_seal_the_wallet_does_not_hold_is_refused() -> Result<()> {
    let (rpc, chain) = start_mock_btcd();
    let tss_pubkey = pubkey_hex(&TSS_SECRET);
    let (_tss_addr, tss_script) = p2wpkh(&TSS_SECRET);
    let (_user_addr, user_script) = p2wpkh(&[0x22; 32]);

    // A UTXO that exists on chain but is NOT the bridge's (paying a user script).
    let (foreign_funding, foreign_outpoint) = funding_tx(&user_script, GENESIS_BTC, 1);
    chain.lock().unwrap().add_tx(foreign_funding);
    // ...and one the bridge does own, for the control case.
    let (own_funding, own_outpoint) = funding_tx(&tss_script, GENESIS_BTC, 2);
    chain.lock().unwrap().add_tx(own_funding);

    let mut engine = open_engine(&data_dir("adopt-claim-guard"), &rpc, &tss_pubkey)?;

    let refused = engine.issue_asset_at(SYMBOL, "Tether USD", 8, ISSUED as u64, foreign_outpoint);
    assert!(
        refused.is_err(),
        "an outpoint the TSS wallet does not hold must never become the bridge's balance"
    );
    assert!(
        format!("{:#}", refused.unwrap_err()).contains("not a TSS wallet UTXO"),
        "the refusal must name the ownership check"
    );
    assert_eq!(
        engine.get_balance(SYMBOL),
        (0, 0),
        "the refused issuance must leave no balance behind"
    );
    assert!(
        engine.ledger.assets.is_empty(),
        "the refusal must leave no contract registered either — the check runs before anything \
         is written, so a misconfigured issuance does not poison the symbol"
    );

    // Control: the same call with a bridge-owned outpoint succeeds.
    let issued = engine.issue_asset_at(SYMBOL, "Tether USD", 8, ISSUED as u64, own_outpoint)?;
    assert!(!issued.asset_id.is_empty());
    assert_eq!(engine.get_balance(SYMBOL), (ISSUED, 0));

    Ok(())
}
