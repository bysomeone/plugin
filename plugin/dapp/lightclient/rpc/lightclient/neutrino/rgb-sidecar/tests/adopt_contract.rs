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
use common::{data_dir, engine_config, funding_tx, open_engine, pubkey_hex, p2wpkh, start_mock_btcd};
use rgb_sidecar::config::load_contract_declarations;
use rgb_sidecar::engine::RgbEngine;
use rgbcore::ContractId;
use serde_json::json;
use std::path::PathBuf;
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

    let adopted = adopter.adopt_contract(&genesis_bytes, SYMBOL, None)?;

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
    let again = adopter.adopt_contract(&genesis_bytes, SYMBOL, None)?;
    assert_eq!(again.asset_id, adopted.asset_id, "re-adopting the same contract is a no-op");
    assert_eq!(adopter.ledger.assets.len(), 1, "re-adopting must not duplicate the record");

    let same_contract_other_symbol = adopter.adopt_contract(&genesis_bytes, "USDT-COPY", None);
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
    let stolen = adopter.adopt_contract(&other_bytes, SYMBOL, None);
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

/// A declarations file (`RGB_SIDECAR_CONTRACTS`), written next to the test's data dirs.
fn declarations_file(name: &str, contracts: serde_json::Value) -> PathBuf {
    let dir = std::env::temp_dir().join(format!("rgb-sidecar-decls-{name}-{}", std::process::id()));
    std::fs::create_dir_all(&dir).expect("create declarations dir");
    let path = dir.join("contracts.json");
    std::fs::write(&path, json!({ "contracts": contracts }).to_string()).expect("write declarations");
    path
}

/// Batch 2a: the contract is declared in configuration, and **startup** adopts it. A declared
/// contract that does not hold up must fail the start — degrading to "the asset is not there"
/// would disguise a deployment mistake as a bridge bug.
#[test]
fn a_declared_contract_is_adopted_at_startup() -> Result<()> {
    let (rpc, chain) = start_mock_btcd();
    let tss_pubkey = pubkey_hex(&TSS_SECRET);
    let (_tss_addr, tss_script) = p2wpkh(&TSS_SECRET);
    let (genesis_funding, genesis_outpoint) = funding_tx(&tss_script, GENESIS_BTC, 1);
    chain.lock().unwrap().add_tx(genesis_funding);

    // The issuer publishes a contract…
    let mut issuer = open_engine(&data_dir("decl-issuer"), &rpc, &tss_pubkey)?;
    let published = issuer.issue_asset_at(SYMBOL, "Tether USD", 8, ISSUED as u64, genesis_outpoint)?;

    // …and the deployment declares it (the shape `RGB_SIDECAR_CONTRACTS` points at).
    let decl_path = declarations_file(
        "good",
        json!([{
            "symbol": SYMBOL,
            "assetId": published.asset_id,
            "genesisConsignment": published.genesis_consignment_hex,
        }]),
    );

    let dir = data_dir("decl-sidecar");
    let deploy = |dir: &std::path::Path| -> Result<RgbEngine> {
        let mut cfg = engine_config(dir, &rpc, &tss_pubkey);
        cfg.contracts = load_contract_declarations(&decl_path)?;
        RgbEngine::open(cfg)
    };

    let adopter = deploy(&dir)?;
    assert_eq!(
        adopter.ledger.asset(SYMBOL).map(|a| a.asset_id.clone()),
        Some(published.asset_id.clone()),
        "a declared contract must be adopted during startup"
    );
    assert_eq!(adopter.get_balance(SYMBOL), (0, 0), "adoption still grants no balance");
    assert!(adopter.list_seals(SYMBOL).is_empty(), "adoption still writes no seal");
    adopter
        .stock
        .transition_builder(ContractId::from_str(&published.asset_id)?, "transfer")
        .expect("the declared contract must be usable in the Stock");
    drop(adopter);

    // A restart re-applies the same declaration: it must be a no-op, not an error (this is what
    // every production restart does).
    let restarted = deploy(&dir)?;
    assert_eq!(restarted.get_balance(SYMBOL), (0, 0));
    assert_eq!(restarted.ledger.assets.len(), 1, "a restart must not duplicate the asset");

    Ok(())
}

/// The tamper case: the bytes are one contract, the declaration names another. That is the
/// mistake a deployment actually makes (a stale file, a swapped mount, a copy-paste between
/// entries) and it must stop the start, naming both sides.
#[test]
fn a_tampered_declaration_fails_the_start() -> Result<()> {
    let (rpc, chain) = start_mock_btcd();
    let tss_pubkey = pubkey_hex(&TSS_SECRET);
    let (_tss_addr, tss_script) = p2wpkh(&TSS_SECRET);
    let (f1, op1) = funding_tx(&tss_script, GENESIS_BTC, 1);
    let (f2, op2) = funding_tx(&tss_script, GENESIS_BTC, 2);
    {
        let mut c = chain.lock().unwrap();
        c.add_tx(f1);
        c.add_tx(f2);
    }

    let mut issuer = open_engine(&data_dir("tamper-issuer"), &rpc, &tss_pubkey)?;
    let mine = issuer.issue_asset_at(SYMBOL, "Tether USD", 8, ISSUED as u64, op1)?;
    let other = issuer.issue_asset_at("OTHER", "Other USD", 8, 1, op2)?;
    assert_ne!(mine.asset_id, other.asset_id);

    // (1) mismatched assetId: our consignment, but declared as the other contract.
    let mismatch = declarations_file(
        "mismatch",
        json!([{
            "symbol": SYMBOL,
            "assetId": other.asset_id,
            "genesisConsignment": mine.genesis_consignment_hex,
        }]),
    );
    let mut cfg = engine_config(&data_dir("tamper-mismatch"), &rpc, &tss_pubkey);
    cfg.contracts = load_contract_declarations(&mismatch)?;
    let err = match RgbEngine::open(cfg) {
        Ok(_) => panic!("a declaration that names another contract must fail the start"),
        Err(e) => e,
    };
    let msg = format!("{err:#}");
    assert!(msg.contains("declared contract USDT"), "must name the declaration: {msg}");
    assert!(
        msg.contains("is not the contract in the consignment"),
        "must say the bytes are not that contract: {msg}"
    );
    assert!(msg.contains(&other.asset_id) && msg.contains(&mine.asset_id), "must show both ids: {msg}");

    // (2) the mounted hex file is missing: also a start failure, not an empty asset list.
    let missing = declarations_file(
        "missing-file",
        json!([{
            "symbol": SYMBOL,
            "assetId": mine.asset_id,
            "genesisConsignmentFile": "/nonexistent/usdt.genesis.hex",
        }]),
    );
    let mut cfg = engine_config(&data_dir("tamper-missing"), &rpc, &tss_pubkey);
    cfg.contracts = load_contract_declarations(&missing)?;
    let err = match RgbEngine::open(cfg) {
        Ok(_) => panic!("an unreadable consignment file must fail the start"),
        Err(e) => e,
    };
    let msg = format!("{err:#}");
    assert!(msg.contains("read genesisConsignmentFile"), "got: {msg}");
    assert!(msg.contains("declared contract USDT"), "got: {msg}");

    Ok(())
}
