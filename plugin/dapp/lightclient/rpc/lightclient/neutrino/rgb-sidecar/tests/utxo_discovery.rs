//! UTXO discovery (O4-lite).
//!
//! Discovery stays on the node's address index (`searchrawtransactions` + `gettxout`) — that is
//! the authoritative liveness view. The optimisation is transport-only: all scripts are searched
//! in batched JSON-RPC requests, `getblockcount` is read once per sweep instead of once per script,
//! and every candidate output's `gettxout` is batched too. These tests pin the properties that must
//! not change while that happens:
//!
//! * every deposit is listed, including several in one sweep;
//! * a spent output is not listed, and a new live output of the spending tx is;
//! * registering an address whose deposit is already on chain lists that outpoint exactly once
//!   (idempotent registration, no duplicate accounting);
//! * a sweep is a bounded number of HTTP round-trips independent of the watch-set size, with
//!   exactly one `getblockcount`.

mod common;

use std::collections::BTreeSet;
use std::sync::Arc;

use anyhow::Result;
use bitcoin::absolute::LockTime;
use bitcoin::{
    Amount, CompressedPublicKey, Network, OutPoint, ScriptBuf, Sequence, Transaction, TxIn, TxOut,
    Witness,
};
use common::{funding_tx, p2wpkh, pubkey_hex, start_mock_btcd};
use rgb_sidecar::rpc::BtcdRpc;
use rgb_sidecar::wallet::{deposit_witness_script, BtcWallet};

const TSS_SECRET: [u8; 32] = [0x11; 32];

fn open_wallet(rpc: &str, tss_pubkey: &str) -> BtcWallet {
    let rpc = Arc::new(BtcdRpc::connect(rpc, "u", "p", None, Network::Regtest).unwrap());
    BtcWallet::new(rpc, tss_pubkey, Network::Regtest).unwrap()
}

/// The `pkScript` the wallet will accept for `user_id`: derived from the same TSS pubkey, which is
/// exactly what `register_deposit_script` re-derives and verifies.
fn user_pk_script(tss_pubkey: &str, user_id: &str) -> ScriptBuf {
    let pubkey: CompressedPublicKey = tss_pubkey.parse().unwrap();
    let witness_script = deposit_witness_script(user_id, &pubkey).unwrap();
    ScriptBuf::new_p2wsh(&witness_script.wscript_hash())
}

fn outpoints(utxos: &[rgb_sidecar::wallet::WalletUtxo]) -> BTreeSet<OutPoint> {
    utxos.iter().map(|u| u.outpoint).collect()
}

/// Several deposits discovered in one sweep must all be listed; none may be lost by batching.
#[test]
fn multiple_deposits_in_one_sweep_are_all_listed() -> Result<()> {
    let (rpc, chain) = start_mock_btcd();
    let tss = pubkey_hex(&TSS_SECRET);
    let mut wallet = open_wallet(&rpc, &tss);

    let pk_a = user_pk_script(&tss, "user-a");
    let pk_b = user_pk_script(&tss, "user-b");
    assert!(wallet.register_deposit_script("user-a", &pk_a)?);
    assert!(wallet.register_deposit_script("user-b", &pk_b)?);

    // Two deposits to A and one to B.
    let (tx_a1, op_a1) = funding_tx(&pk_a, 10_000, 1);
    let (tx_a2, op_a2) = funding_tx(&pk_a, 20_000, 2);
    let (tx_b1, op_b1) = funding_tx(&pk_b, 30_000, 3);
    {
        let mut c = chain.lock().unwrap();
        c.add_tx(tx_a1);
        c.add_tx(tx_a2);
        c.add_tx(tx_b1);
    }

    let utxos = wallet.list_unspent_user_deposits();
    assert_eq!(
        outpoints(&utxos),
        BTreeSet::from([op_a1, op_a2, op_b1]),
        "every deposit of the watch set must be listed"
    );
    // The main pool is a separate view: none of the user deposits leaks into it.
    assert!(wallet.list_unspent().is_empty());
    Ok(())
}

/// A spent output disappears from the list; the spending transaction's own output is live.
#[test]
fn spent_outputs_are_not_listed() -> Result<()> {
    let (rpc, chain) = start_mock_btcd();
    let tss = pubkey_hex(&TSS_SECRET);
    let (_addr, tss_script) = p2wpkh(&TSS_SECRET);
    let wallet = open_wallet(&rpc, &tss);

    let (tx1, op1) = funding_tx(&tss_script, 50_000, 1);
    let (tx2, op2) = funding_tx(&tss_script, 60_000, 2);
    {
        let mut c = chain.lock().unwrap();
        c.add_tx(tx1);
        c.add_tx(tx2);
    }
    assert_eq!(
        outpoints(&wallet.list_unspent()),
        BTreeSet::from([op1, op2])
    );

    // Spend op1, paying change back to the same script.
    let spend = Transaction {
        version: bitcoin::transaction::Version::TWO,
        lock_time: LockTime::ZERO,
        input: vec![TxIn {
            previous_output: op1,
            script_sig: ScriptBuf::new(),
            sequence: Sequence::MAX,
            witness: Witness::new(),
        }],
        output: vec![TxOut {
            value: Amount::from_sat(49_000),
            script_pubkey: tss_script.clone(),
        }],
    };
    let change = OutPoint {
        txid: spend.compute_txid(),
        vout: 0,
    };
    {
        let mut c = chain.lock().unwrap();
        c.add_tx(spend);
        c.spend(op1);
    }

    let utxos = wallet.list_unspent();
    let set = outpoints(&utxos);
    assert!(!set.contains(&op1), "a spent output must never be listed");
    assert!(set.contains(&op2), "the untouched output must stay");
    assert!(
        set.contains(&change),
        "the spending tx's own output is live"
    );
    assert_eq!(utxos.len(), 2);
    Ok(())
}

/// An address whose deposit already landed before registration is listed exactly once, and
/// re-registering the same address adds nothing.
#[test]
fn newly_registered_address_is_listed_once() -> Result<()> {
    let (rpc, chain) = start_mock_btcd();
    let tss = pubkey_hex(&TSS_SECRET);
    let mut wallet = open_wallet(&rpc, &tss);
    let pk = user_pk_script(&tss, "late-user");

    // The deposit is on chain before the sidecar learns the address.
    let (tx, op) = funding_tx(&pk, 77_000, 9);
    chain.lock().unwrap().add_tx(tx);
    assert!(
        wallet.list_unspent_user_deposits().is_empty(),
        "an unregistered script is not part of the watch set"
    );

    assert!(wallet.register_deposit_script("late-user", &pk)?);
    let utxos = wallet.list_unspent_user_deposits();
    assert_eq!(utxos.len(), 1);
    assert_eq!(utxos[0].outpoint, op);
    assert_eq!(utxos[0].value, 77_000);

    // Idempotent registration must not create a second accounting entry for the same outpoint.
    assert!(!wallet.register_deposit_script("late-user", &pk)?);
    let all = wallet.list_unspent_all();
    assert_eq!(
        all.iter().filter(|u| u.outpoint == op).count(),
        1,
        "the same outpoint must appear at most once in the union view"
    );
    Ok(())
}

/// O4-lite cost shape: one `getblockcount` per sweep, and a bounded number of HTTP round-trips
/// regardless of the watch-set size (all searches and all `gettxout`s are batched).
#[test]
fn sweep_batches_rpcs_and_reads_the_tip_once() -> Result<()> {
    const N: usize = 5;
    let (rpc, chain) = start_mock_btcd();
    let tss = pubkey_hex(&TSS_SECRET);
    let mut wallet = open_wallet(&rpc, &tss);

    for i in 0..N {
        let user_id = format!("user-{i}");
        let pk = user_pk_script(&tss, &user_id);
        wallet.register_deposit_script(&user_id, &pk)?;
        let (tx, _outpoint) = funding_tx(&pk, 10_000 + i as u64, i as u32 + 1);
        chain.lock().unwrap().add_tx(tx);
    }

    chain.lock().unwrap().reset_counters();
    let utxos = wallet.list_unspent_all();
    assert_eq!(utxos.len(), N);

    {
        let c = chain.lock().unwrap();
        assert_eq!(
            c.call_count("getblockcount"),
            1,
            "the tip must be read once per sweep, not once per script"
        );
        assert_eq!(c.call_count("searchrawtransactions"), N + 1);
        assert_eq!(c.call_count("gettxout"), N);
        // main + N scripts fit in one search batch; the N gettxout calls fit in one batch; plus
        // the single getblockcount request. The pre-O4 shape was (N+1) searches + (N+1) tips + N
        // outputs = 3N+2 round-trips here.
        assert_eq!(
            c.http_requests, 3,
            "sweep must be 3 HTTP requests (search batch, tip, gettxout batch): {:?}",
            c.calls
        );
    }

    // Same set on a second sweep (batched calls are stateless and repeatable).
    chain.lock().unwrap().reset_counters();
    let again = wallet.list_unspent_all();
    assert_eq!(outpoints(&again), outpoints(&utxos));
    {
        let c = chain.lock().unwrap();
        assert_eq!(c.call_count("getblockcount"), 1);
        assert_eq!(c.http_requests, 3);
    }
    Ok(())
}

/// A node that does not implement JSON-RPC batches must still produce exactly the same set: the
/// client falls back to the pre-O4 sequential calls. The tip is still read once (that part is not
/// a transport optimisation).
#[test]
fn batch_unsupported_node_falls_back_to_the_same_set() -> Result<()> {
    const N: usize = 3;
    let (rpc, chain) = start_mock_btcd();
    let tss = pubkey_hex(&TSS_SECRET);
    let mut wallet = open_wallet(&rpc, &tss);
    for i in 0..N {
        let user_id = format!("user-{i}");
        let pk = user_pk_script(&tss, &user_id);
        wallet.register_deposit_script(&user_id, &pk)?;
        let (tx, _outpoint) = funding_tx(&pk, 5_000 + i as u64, i as u32 + 1);
        chain.lock().unwrap().add_tx(tx);
    }

    let batched = wallet.list_unspent_all();
    {
        let mut c = chain.lock().unwrap();
        c.batch_supported = false;
        c.reset_counters();
    }
    let fallback = wallet.list_unspent_all();
    assert_eq!(
        outpoints(&fallback),
        outpoints(&batched),
        "the fallback transport must not change the discovered set"
    );
    let c = chain.lock().unwrap();
    assert_eq!(c.call_count("searchrawtransactions"), N + 1);
    assert_eq!(
        c.call_count("getblockcount"),
        1,
        "the tip hoist applies to the sequential path too"
    );
    assert_eq!(c.call_count("gettxout"), N);
    // Two rejected batch attempts + (N+1) single searches + 1 tip + N single gettxout calls.
    assert_eq!(c.http_requests, 2 + (N + 1) + 1 + N);
    Ok(())
}
