//! What a sidecar does with the state files it is handed at startup (task #60, the first step of
//! backup/restore).
//!
//! A backup is only worth taking if the state on disk cannot be *torn*, and if a torn one is
//! impossible to mistake for a healthy one. Two failure modes made that false:
//!
//! 1. `Stock::load`'s error was swallowed by `RgbEngine::open` (fallback: `Stock::in_memory()`), so
//!    a corrupt `stock/*.dat` came up as an *empty* RGB state — silently, next to a `ledger.json`
//!    that still listed its assets.
//! 2. The stock's writes were `File::create` + stream-encode: the target was truncated in place, so
//!    a crash mid-write left exactly that corrupt file. (Both are now handled — see `src/store.rs`.)
//!
//! These tests drive the real files on disk: they adopt a contract so the state is non-trivial,
//! close the engine, then damage the directory the way a crash, a disk rollback or a partial
//! restore would, and pin what the next start does.

mod common;

use anyhow::Result;
use bitcoin::hex::FromHex;
use common::{data_dir, declarations_file, engine_config, funding_tx, open_engine, pubkey_hex, p2wpkh, start_mock_btcd};
use rgb_sidecar::engine::RgbEngine;
use rgbcore::ContractId;
use serde_json::json;
use std::str::FromStr;

const TSS_SECRET: [u8; 32] = [0x11; 32];
const SYMBOL: &str = "USDT";
const ISSUED: i64 = 100_000_000;
const GENESIS_BTC: u64 = 700;

/// An engine with one adopted contract and a non-empty stock, plus its contract's genesis bytes
/// and asset id. The engine is dropped so the caller starts from what is on disk.
struct Fixture {
    rpc: String,
    tss_pubkey: String,
    dir: std::path::PathBuf,
    asset_id: String,
    genesis_hex: String,
    /// A second contract, issued but **not** adopted: adopting it is a real Stock mutation.
    other_asset_id: String,
    other_genesis_hex: String,
}

fn fixture(name: &str) -> Result<Fixture> {
    let (rpc, chain) = start_mock_btcd();
    let tss_pubkey = pubkey_hex(&TSS_SECRET);
    let (_addr, tss_script) = p2wpkh(&TSS_SECRET);
    let (funding, outpoint) = funding_tx(&tss_script, GENESIS_BTC, 1);
    let (other_funding, other_outpoint) = funding_tx(&tss_script, GENESIS_BTC, 2);
    {
        let mut c = chain.lock().unwrap();
        c.add_tx(funding);
        c.add_tx(other_funding);
    }

    let dir = data_dir(name);
    let mut issuer = open_engine(&data_dir(&format!("{name}-issuer")), &rpc, &tss_pubkey)?;
    let published = issuer.issue_asset_at(SYMBOL, "Tether USD", 8, ISSUED as u64, outpoint)?;
    let other = issuer.issue_asset_at("USDT2", "Second USD", 8, ISSUED as u64, other_outpoint)?;

    // …and a sidecar that only *adopts* it, so its ledger and its stock are both populated.
    let mut engine = RgbEngine::open(engine_config(&dir, &rpc, &tss_pubkey))?;
    engine.adopt_contract(
        &Vec::<u8>::from_hex(&published.genesis_consignment_hex)?,
        SYMBOL,
        Some(&published.asset_id),
    )?;
    drop(engine);

    Ok(Fixture {
        rpc,
        tss_pubkey,
        dir,
        asset_id: published.asset_id,
        genesis_hex: published.genesis_consignment_hex,
        other_asset_id: other.asset_id,
        other_genesis_hex: other.genesis_consignment_hex,
    })
}

impl Fixture {
    fn open(&self) -> Result<RgbEngine> {
        RgbEngine::open(engine_config(&self.dir, &self.rpc, &self.tss_pubkey))
    }

    fn stock_file(&self, name: &str) -> std::path::PathBuf {
        self.dir.join("stock").join(name)
    }
}

/// The control case: a brand-new data directory is not "damage", it is a first start.
#[test]
fn a_fresh_data_dir_starts() -> Result<()> {
    let (rpc, _chain) = start_mock_btcd();
    let dir = data_dir("startup-fresh");
    let engine = RgbEngine::open(engine_config(&dir, &rpc, &pubkey_hex(&TSS_SECRET)))?;
    assert!(engine.ledger.assets.is_empty(), "a fresh sidecar has no assets");
    assert!(engine.list_seals(SYMBOL).is_empty(), "and no seals");
    eprintln!("[probe] fresh data dir {}: sidecar started", dir.display());
    Ok(())
}

/// A truncated stock file is the shape a crash-mid-write or a disk rollback leaves behind. The
/// start must fail, and the error must say which file and why.
#[test]
fn a_truncated_stock_file_fails_the_start() -> Result<()> {
    let fx = fixture("startup-truncated")?;
    let stash = fx.stock_file("stash.dat");
    let whole = std::fs::read(&stash)?;
    std::fs::write(&stash, &whole[..whole.len() / 2])?;

    let err = match fx.open() {
        Ok(_) => panic!("a truncated stock must not start"),
        Err(e) => e,
    };
    let msg = format!("{err:#}");
    eprintln!("[probe] startup with a truncated stash.dat refused with:\n{msg}");
    assert!(msg.contains("stash.dat"), "must name the file: {msg}");
    assert!(msg.contains(&format!("{} bytes", whole.len() / 2)), "must report its size: {msg}");
    assert!(msg.contains("corrupt or truncated"), "must give the reason: {msg}");
    assert!(msg.contains("refusing to start"), "must refuse, not degrade: {msg}");
    Ok(())
}

/// Half a stock is ambiguous — is that a fresh start or a broken one? It is a broken one, and it
/// must be refused: reading it as "empty" would put an empty RGB state next to a populated ledger.
#[test]
fn a_missing_stock_file_fails_the_start() -> Result<()> {
    let fx = fixture("startup-missing-file")?;
    std::fs::remove_file(fx.stock_file("state.dat"))?;

    let err = match fx.open() {
        Ok(_) => panic!("a partial stock must not start"),
        Err(e) => e,
    };
    let msg = format!("{err:#}");
    eprintln!("[probe] startup with state.dat missing refused with:\n{msg}");
    assert!(msg.contains("incomplete RGB stock"), "got: {msg}");
    assert!(msg.contains("state.dat"), "must name the missing file: {msg}");
    assert!(msg.contains("partial restore"), "must say what it looks like: {msg}");
    Ok(())
}

/// The whole `stock/` directory gone while `ledger.json` survived — a restore that brought back
/// only part of the backup. Today nothing notices: the sidecar comes up with an empty RGB state
/// and a ledger that lists the asset, and only fails much later, at the first withdrawal.
#[test]
fn a_ledger_asset_without_its_stock_fails_the_start() -> Result<()> {
    let fx = fixture("startup-lost-stock")?;
    std::fs::remove_dir_all(fx.dir.join("stock"))?;

    let err = match fx.open() {
        Ok(_) => panic!("a ledger asset with no stock must not start"),
        Err(e) => e,
    };
    let msg = format!("{err:#}");
    eprintln!("[probe] startup with the stock/ directory gone refused with:\n{msg}");
    assert!(msg.contains(&format!("ledger asset {SYMBOL}")), "must name the asset: {msg}");
    assert!(msg.contains(&fx.asset_id), "must name the contract: {msg}");
    assert!(msg.contains("not in the RGB stock"), "must say what is wrong: {msg}");
    assert!(msg.contains("RGB_SIDECAR_CONTRACTS"), "must say how to repair it: {msg}");
    Ok(())
}

/// …and the repair it points at works: the same damaged directory starts once the contract is
/// declared, because startup re-adopts it from its genesis consignment. This is the mechanism a
/// restore of "ledger + contract declarations" leans on.
#[test]
fn a_declared_contract_repairs_a_lost_stock() -> Result<()> {
    let fx = fixture("startup-repair")?;
    std::fs::remove_dir_all(fx.dir.join("stock"))?;
    assert!(fx.open().is_err(), "without the declaration the same directory must fail");

    let decl_path = declarations_file(
        "startup-repair",
        json!([{ "symbol": SYMBOL, "assetId": fx.asset_id, "genesisConsignment": fx.genesis_hex }]),
    );
    let mut cfg = engine_config(&fx.dir, &fx.rpc, &fx.tss_pubkey);
    cfg.contracts = rgb_sidecar::config::load_contract_declarations(&decl_path)?;
    let repaired = RgbEngine::open(cfg)?;

    assert_eq!(
        repaired.ledger.asset(SYMBOL).map(|a| a.asset_id.clone()),
        Some(fx.asset_id.clone()),
        "the ledger asset is still there"
    );
    assert_eq!(
        repaired.get_balance(SYMBOL),
        (0, 0),
        "repairing the Stock must not invent a balance (adoption never writes a seal)"
    );
    repaired
        .stock
        .transition_builder(ContractId::from_str(&fx.asset_id)?, "transfer")
        .expect("the re-adopted contract must be usable again");
    Ok(())
}

/// The write path itself: a write is staged through a tmp file, so a write that cannot complete
/// leaves the previous file byte-identical (no torn file, no half-replaced state) — and the
/// failure is reported, not swallowed.
///
/// The mutation matters: `Stock::store()` is a no-op when nothing is dirty, so a *real* change is
/// what makes the store actually write.
#[test]
fn a_failed_stock_write_leaves_the_previous_file_intact() -> Result<()> {
    let fx = fixture("startup-atomic")?;
    let stash = fx.stock_file("stash.dat");
    let ledger = fx.dir.join("ledger.json");
    let before_stash = std::fs::read(&stash)?;
    let before_ledger = std::fs::read(&ledger)?;

    // Block the staging path, then adopt a second contract: the write cannot be staged, so it must
    // fail before the target is touched.
    let tmp = std::path::PathBuf::from(format!("{}.tmp", stash.display()));
    std::fs::create_dir(&tmp)?;
    let mut engine = fx.open()?;
    let err = engine
        .adopt_contract(
            &Vec::<u8>::from_hex(&fx.other_genesis_hex)?,
            "USDT2",
            Some(&fx.other_asset_id),
        )
        .expect_err("a blocked staging file must fail the save");
    let msg = format!("{err:#}");
    eprintln!("[probe] saving after blocking the staging path failed with:\n{msg}");
    assert!(msg.contains("cannot be written atomically"), "got: {msg}");

    assert_eq!(
        std::fs::read(&stash)?,
        before_stash,
        "the previous stock file must be byte-identical (the write never reached it)"
    );
    assert_eq!(
        std::fs::read(&ledger)?,
        before_ledger,
        "and the ledger must not have been advanced either"
    );
    assert!(
        !std::fs::metadata(&stash)?.is_dir(),
        "the target must still be a file, not a stub"
    );

    drop(engine);
    std::fs::remove_dir(&tmp)?;
    // The directory it left behind still starts, with exactly the state it had before.
    let reopened = fx.open()?;
    assert_eq!(reopened.get_balance(SYMBOL), (0, 0));
    assert_eq!(reopened.ledger.assets.len(), 1, "the failed write left no trace");
    reopened
        .stock
        .transition_builder(ContractId::from_str(&fx.asset_id)?, "transfer")
        .expect("and the healthy state is still usable");
    Ok(())
}
