//! Repeated deposits into the same contract: the recipient of a consignment must be the output of
//! the transition it **delivers**, never an output of a historical transition it carries along.
//!
//! Every RGB transfer consignment includes the full ancestry of the transition back to the
//! contract genesis. When a second deposit spends a seal created by the first one, the first
//! deposit's transition travels in the same consignment — and its recipient output also pays the
//! TSS script. Picking "the first opened seal at the TSS script" therefore read the **first**
//! deposit's amount as the second deposit's amount (#77): a second deposit of a different amount
//! was rejected with `consignment does not correspond to hinted receive`, and a second deposit of
//! the *same* amount settled onto the first deposit's outpoint.
//!
//! The tests drive a real engine against the mock btcd node from `tests/common`, build each
//! deposit with the engine's own payer-side builder (`build_transfer`, what the test-sim driver
//! uses), and settle it through `provide_consignment` — the production attribution path.

mod common;

use std::sync::{Arc, Mutex};

use anyhow::Result;
use bitcoin::{OutPoint, ScriptBuf, Txid};
use common::{data_dir, funding_tx, open_engine, p2wpkh, pubkey_hex, start_mock_btcd, MockChain};
use rgb_sidecar::engine::RgbEngine;
use rgb_sidecar::invoice::consignment_from_bytes;
use rgbstd::containers::ConsignmentExt;

const TSS_SECRET: [u8; 32] = [0x11; 32];
const SYMBOL: &str = "USDT";
const ISSUED: i64 = 100_000_000;
/// BTC carried by the RGB recipient output (dust: the value is irrelevant, RGB state rides on it).
const RECIPIENT_BTC: u64 = 546;
/// The genesis seal's BTC value: enough to fund the first deposit's carrier tx.
const GENESIS_BTC: u64 = 200_000;
const FEE_RATE: u64 = 2;

struct Harness {
    engine: RgbEngine,
    chain: Arc<Mutex<MockChain>>,
    tss_script: ScriptBuf,
    genesis: OutPoint,
}

/// A funded engine with one issued asset.
fn harness(name: &str) -> Result<Harness> {
    let (rpc, chain) = start_mock_btcd();
    let tss_pubkey = pubkey_hex(&TSS_SECRET);
    let (_tss_addr, tss_script) = p2wpkh(&TSS_SECRET);
    let (genesis_funding, genesis) = funding_tx(&tss_script, GENESIS_BTC, 1);
    chain.lock().unwrap().add_tx(genesis_funding);

    let mut engine = open_engine(&data_dir(name), &rpc, &tss_pubkey)?;
    engine.issue_asset_at(SYMBOL, "Tether USD", 8, ISSUED as u64, genesis)?;
    Ok(Harness {
        engine,
        chain,
        tss_script,
        genesis,
    })
}

impl Harness {
    /// Build a deposit of `amount` from `input` to a fresh receive and settle it. The carrier tx is
    /// published on the mock node so the next deposit can use its outputs (a real node would have
    /// it on chain; the engine also caches it as a local anchor).
    fn deposit(&mut self, input: OutPoint, amount: i64) -> Result<(OutPoint, Txid)> {
        let created = self.engine.create_receive(SYMBOL, amount, None)?;
        let (invoice, receive_id) = (created.invoice, created.receive_id);
        let pay = self.engine.build_transfer(
            SYMBOL,
            &[input],
            &invoice,
            &self.tss_script,
            FEE_RATE,
            RECIPIENT_BTC,
        )?;
        self.chain
            .lock()
            .unwrap()
            .add_tx(pay.psbt.unsigned_tx.clone());
        let rec = self
            .engine
            .provide_consignment(&pay.consignment, Some(&receive_id))?;
        let recipient = OutPoint {
            txid: pay.txid,
            vout: pay.recipient_vout,
        };
        assert_eq!(rec.amount_requested, amount, "receive amount");
        assert_eq!(rec.settled_amount, amount, "settled amount");
        assert_eq!(rec.txid, pay.txid.to_string(), "settled anchor txid");
        assert_eq!(rec.vout, pay.recipient_vout, "settled vout");
        Ok((recipient, pay.txid))
    }
}

/// The reported defect: the second deposit has a different amount than the first.
#[test]
fn second_deposit_of_a_different_amount_settles_its_own_amount() -> Result<()> {
    let mut h = harness("repeated-deposit-different")?;
    let first_amount = 1_000_000;
    let second_amount = 44_000;

    let (first_seal, _first_txid) = h.deposit(h.genesis, first_amount)?;
    // The second consignment carries the first deposit's transition; the fixture is only useful if
    // that history is really there.
    let created = h.engine.create_receive(SYMBOL, second_amount, None)?;
    let (second_invoice, second_receive) = (created.invoice, created.receive_id);
    let pay = h.engine.build_transfer(
        SYMBOL,
        &[first_seal],
        &second_invoice,
        &h.tss_script,
        FEE_RATE,
        RECIPIENT_BTC,
    )?;
    assert!(
        consignment_from_bytes(&pay.consignment)?
            .bundled_witnesses()
            .count()
            >= 2,
        "the fixture must carry the first deposit as history, or it cannot reproduce #77"
    );
    // The signing nodes' path (`ValidateConsignment` with the deposit's amount) must see the same
    // delivery: the bridge compares this amount against the on-chain deposit before signing.
    let expected_outpoint = format!("{}:{}", pay.txid, pay.recipient_vout);
    let insp = h
        .engine
        .validate_consignment(&pay.consignment, Some(second_amount))?;
    assert!(insp.valid, "{:?}", insp.error);
    assert_eq!(insp.recipient_amount, second_amount);
    assert_eq!(
        insp.recipient_seal.as_deref(),
        Some(expected_outpoint.as_str())
    );

    let rec = h
        .engine
        .provide_consignment(&pay.consignment, Some(&second_receive))?;

    assert_eq!(rec.settled_amount, second_amount);
    assert_eq!(
        rec.txid,
        pay.txid.to_string(),
        "must settle the new deposit's txid"
    );
    assert_eq!(rec.vout, pay.recipient_vout);
    let seal = h
        .engine
        .list_seals(SYMBOL)
        .into_iter()
        .find(|s| s.outpoint == format!("{}:{}", pay.txid, pay.recipient_vout))
        .expect("the new recipient seal must be recorded");
    assert_eq!(seal.amount, second_amount);
    Ok(())
}

/// The same defect with an amount collision: the second deposit's amount equals the first's, so an
/// amount-only match would silently settle the second receive onto the **first** deposit's outpoint.
#[test]
fn second_deposit_of_the_same_amount_does_not_reuse_the_first_outpoint() -> Result<()> {
    let mut h = harness("repeated-deposit-same")?;
    let amount = 44_000;

    let (first_seal, first_txid) = h.deposit(h.genesis, amount)?;
    let created = h.engine.create_receive(SYMBOL, amount, None)?;
    let (second_invoice, second_receive) = (created.invoice, created.receive_id);
    let pay = h.engine.build_transfer(
        SYMBOL,
        &[first_seal],
        &second_invoice,
        &h.tss_script,
        FEE_RATE,
        RECIPIENT_BTC,
    )?;
    let rec = h
        .engine
        .provide_consignment(&pay.consignment, Some(&second_receive))?;

    assert_ne!(
        first_txid, pay.txid,
        "the two deposits are different transactions"
    );
    assert_eq!(
        rec.txid,
        pay.txid.to_string(),
        "the second receive must not be settled onto the first deposit's outpoint"
    );
    assert_eq!(rec.vout, pay.recipient_vout);
    // The first deposit's seal was spent by the second deposit, so it must be consumed — not
    // resurrected as pending-mint by re-recording it as the second receive's recipient.
    let first = h
        .engine
        .list_seals(SYMBOL)
        .into_iter()
        .find(|s| s.outpoint == first_seal.to_string())
        .expect("the first deposit's seal is still in the ledger");
    assert_eq!(first.status.as_str(), "consumed");
    Ok(())
}
