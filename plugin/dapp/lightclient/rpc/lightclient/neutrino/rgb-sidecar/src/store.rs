//! Atomic, fail-loud persistence for the RGB `Stock`.
//!
//! rgb-ops ships `FsBinStore`, and both of its halves are unusable for a system whose state is
//! supposed to be backable-up:
//!
//! - **Write**: `strict_serialize_to_file` is `File::create` + a streaming encode — the target is
//!   truncated *in place* and then written, with no tmp file, no `fsync` and no `rename`. A crash
//!   or a failed write leaves a torn file that has already replaced the good one.
//! - **Read**: `Stock::load`'s error was swallowed by `RgbEngine::open` (which fell back to
//!   `Stock::in_memory()`), so a torn file came back as an *empty* Stock — silently, next to a
//!   `ledger.json` that still listed the assets.
//!
//! This module keeps the crate's file layout (`stock/{stash,state,index}.dat`, so existing data
//! dirs keep working) and replaces both halves: writes go through `ledger::write_atomic`
//! (tmp → fsync → rename → dir fsync), and reads either decode or fail with the file, its size and
//! the reason. Nothing here degrades to "empty": an absent file is an error, and the one legitimate
//! "nothing to load yet" case (a brand-new data dir) is an explicit, separately answered question
//! ([`StockDirState`]).

use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

use amplify::confinement::U32 as U32MAX;
use nonasync::persistence::{PersistenceError, PersistenceProvider};
use rgbstd::persistence::fs::FsBinStore;
use rgbstd::persistence::{MemIndex, MemStash, MemState};
use strict_encoding::{StrictDeserialize, StrictSerialize};

use crate::ledger::write_atomic;

/// What a `stock/` directory holds.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum StockDirState {
    /// No stock file exists at all: a first start, the caller creates an empty Stock.
    Fresh,
    /// All three files exist: load them, and fail loudly if any of them does not decode.
    Initialized,
}

/// A `stock/` file — or the directory itself — that cannot be used, with the reason.
///
/// The size is included because it is what makes a truncation self-evident in a log line ("0
/// bytes", or 40% of what the last healthy backup had).
#[derive(Debug)]
pub struct StoreError {
    path: PathBuf,
    size: Option<u64>,
    reason: String,
}

impl StoreError {
    fn at(path: &Path, reason: impl Into<String>) -> Self {
        Self {
            path: path.to_path_buf(),
            size: std::fs::metadata(path).map(|m| m.len()).ok(),
            reason: reason.into(),
        }
    }
}

impl std::fmt::Display for StoreError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self.size {
            Some(n) => write!(f, "{} ({n} bytes): {}", self.path.display(), self.reason),
            None => write!(f, "{}: {}", self.path.display(), self.reason),
        }
    }
}

impl std::error::Error for StoreError {}

/// `FsBinStore` with atomic writes and non-swallowing reads. Same file layout.
#[derive(Clone, Debug)]
pub struct AtomicFsBinStore {
    inner: FsBinStore,
    /// The first write failure that has not been reported yet.
    ///
    /// [`PersistenceProvider::store`] has to return `Ok` even when the write failed, because
    /// rgb-ops performs a write *inside its transactions* and turns any error into
    /// `rollback_transaction()` — which the in-memory providers implement as `unreachable!()`.
    /// So a disk error there aborts the process with "entered unreachable code" instead of a
    /// diagnosable failure. The error is recorded here instead, and
    /// [`Self::take_write_failure`] hands it to the engine at its persistence boundaries — which
    /// is where a failed write can be reported to the caller as an ordinary error, and where the
    /// next successful write converges the disk with the in-memory state (a retry after a
    /// transient failure loses nothing).
    ///
    /// Reporting is deliberately conservative: the flag is kept until it is read, never cleared by
    /// a later success, so a write that failed is never silently forgotten.
    pending: Arc<Mutex<Option<String>>>,
}

impl AtomicFsBinStore {
    /// Create (if needed) and open the stock directory.
    pub fn open(dir: &Path) -> Result<Self, PersistenceError> {
        let inner = FsBinStore::new(dir.to_path_buf()).map_err(PersistenceError::with)?;
        Ok(Self { inner, pending: Arc::new(Mutex::new(None)) })
    }

    /// The first write failure since the last call, if any. Callers must treat it as fatal for the
    /// operation they were performing: the write did **not** happen.
    pub fn take_write_failure(&self) -> Option<String> {
        self.pending.lock().ok().and_then(|mut p| p.take())
    }

    fn record_write_failure(&self, e: PersistenceError) {
        if let Ok(mut pending) = self.pending.lock() {
            if pending.is_none() {
                *pending = Some(format!("{e}"));
            }
        }
    }

    /// Classify the directory, without decoding anything.
    ///
    /// A *partial* set of files is an error rather than a fresh start: three files are written
    /// together and a restore that brought back only some of them must not be read as "nothing
    /// here yet" — that would start an empty Stock next to a populated ledger, which is precisely
    /// the silent divergence this module exists to prevent.
    pub fn state(&self) -> Result<StockDirState, PersistenceError> {
        let all = [
            (&self.inner.stash, "stash.dat"),
            (&self.inner.state, "state.dat"),
            (&self.inner.index, "index.dat"),
        ];
        let missing: Vec<&str> = all
            .iter()
            .filter(|(p, _)| !p.exists())
            .map(|(_, name)| *name)
            .collect();
        match missing.len() {
            0 => Ok(StockDirState::Initialized),
            n if n == all.len() => Ok(StockDirState::Fresh),
            _ => Err(PersistenceError::with(StoreError::at(
                self.inner.stash.parent().unwrap_or(&self.inner.stash),
                format!(
                    "incomplete RGB stock: {} missing while the rest are present (a partial \
                     restore, or a first write that never completed) — refusing to read it as an \
                     empty state",
                    missing.join(", ")
                ),
            ))),
        }
    }

    pub fn stash_path(&self) -> &Path {
        &self.inner.stash
    }

    pub fn state_path(&self) -> &Path {
        &self.inner.state
    }

    pub fn index_path(&self) -> &Path {
        &self.inner.index
    }
}

fn load_file<T: StrictDeserialize>(path: &Path) -> Result<T, PersistenceError> {
    T::strict_deserialize_from_file::<U32MAX>(path).map_err(|e| {
        PersistenceError::with(StoreError::at(
            path,
            format!("cannot be decoded ({e}) — the file is corrupt or truncated"),
        ))
    })
}

fn write_file<T: StrictSerialize>(path: &Path, object: &T) -> Result<(), PersistenceError> {
    let bytes = object
        .to_strict_serialized::<U32MAX>()
        .map_err(|e| {
            PersistenceError::with(StoreError::at(path, format!("cannot be encoded ({e})")))
        })?
        .release();
    write_atomic(path, &bytes).map_err(|e| {
        PersistenceError::with(StoreError::at(
            path,
            format!("cannot be written atomically ({e:#}); the previous file is untouched"),
        ))
    })
}

/// Store, recording (not raising) the failure — see `AtomicFsBinStore::pending`.
fn store_file<T: StrictSerialize>(
    store: &AtomicFsBinStore,
    path: &Path,
    object: &T,
) -> Result<(), PersistenceError> {
    if let Err(e) = write_file(path, object) {
        store.record_write_failure(e);
    }
    Ok(())
}

impl PersistenceProvider<MemStash> for AtomicFsBinStore {
    fn load(&self) -> Result<MemStash, PersistenceError> {
        load_file(&self.inner.stash)
    }

    fn store(&self, object: &MemStash) -> Result<(), PersistenceError> {
        store_file(self, &self.inner.stash, object)
    }
}

impl PersistenceProvider<MemState> for AtomicFsBinStore {
    fn load(&self) -> Result<MemState, PersistenceError> {
        load_file(&self.inner.state)
    }

    fn store(&self, object: &MemState) -> Result<(), PersistenceError> {
        store_file(self, &self.inner.state, object)
    }
}

impl PersistenceProvider<MemIndex> for AtomicFsBinStore {
    fn load(&self) -> Result<MemIndex, PersistenceError> {
        load_file(&self.inner.index)
    }

    fn store(&self, object: &MemIndex) -> Result<(), PersistenceError> {
        store_file(self, &self.inner.index, object)
    }
}

#[cfg(test)]
mod test {
    use super::*;

    fn dir(name: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!(
            "rgb-sidecar-store-test-{name}-{}-{:?}",
            std::process::id(),
            std::thread::current().id()
        ));
        let _ = std::fs::remove_dir_all(&dir);
        dir
    }

    #[test]
    fn a_missing_dir_is_fresh_and_a_full_one_is_initialized() {
        let dir = dir("classify");
        let store = AtomicFsBinStore::open(&dir).unwrap();
        assert_eq!(store.state().unwrap(), StockDirState::Fresh, "no files yet = first start");

        store.store(&MemStash::in_memory()).unwrap();
        store.store(&MemState::in_memory()).unwrap();
        store.store(&MemIndex::in_memory()).unwrap();
        assert_eq!(
            store.state().unwrap(),
            StockDirState::Initialized,
            "all three files present = load them"
        );

        // A partial set is neither fresh nor loadable: it must be refused, not read as empty.
        std::fs::remove_file(store.state_path()).unwrap();
        let err = store.state().unwrap_err();
        let msg = format!("{err}");
        assert!(msg.contains("incomplete RGB stock"), "got: {msg}");
        assert!(msg.contains("state.dat"), "must name the missing file: {msg}");
    }

    /// A write is only useful if what comes back is the same state: compare the decoded objects
    /// with what was stored, for all three files. (The non-empty case — a real contract and its
    /// seals surviving a restart — is covered end to end by `tests/startup_state.rs`.)
    #[test]
    fn a_written_stock_file_reads_back_identical() {
        fn encoded<T: StrictSerialize>(object: &T) -> Vec<u8> {
            object.to_strict_serialized::<U32MAX>().unwrap().release()
        }

        let path = dir("roundtrip");
        let store = AtomicFsBinStore::open(&path).unwrap();
        store.store(&MemStash::in_memory()).unwrap();
        store.store(&MemState::in_memory()).unwrap();
        store.store(&MemIndex::in_memory()).unwrap();
        assert!(store.take_write_failure().is_none());

        let stash: MemStash = PersistenceProvider::<MemStash>::load(&store).unwrap();
        let state: MemState = PersistenceProvider::<MemState>::load(&store).unwrap();
        let index: MemIndex = PersistenceProvider::<MemIndex>::load(&store).unwrap();
        assert_eq!(encoded(&stash), encoded(&MemStash::in_memory()));
        assert_eq!(encoded(&state), encoded(&MemState::in_memory()));
        assert_eq!(encoded(&index), encoded(&MemIndex::in_memory()));
    }

    #[test]
    fn an_absent_file_is_an_error_not_an_empty_state() {
        let dir = dir("absent");
        let store = AtomicFsBinStore::open(&dir).unwrap();
        // The trait's `load` must never invent an empty object: `state()` is the only place that
        // may answer "there is nothing here", and it answers it for the *whole* directory.
        let err = PersistenceProvider::<MemStash>::load(&store).unwrap_err();
        let msg = format!("{err}");
        assert!(msg.contains("stash.dat"), "must name the file: {msg}");
        assert!(msg.contains("cannot be decoded"), "must give the reason: {msg}");
    }

    #[test]
    fn a_corrupt_or_truncated_file_fails_with_the_file_and_its_size() {
        let dir = dir("corrupt");
        let store = AtomicFsBinStore::open(&dir).unwrap();
        store.store(&MemStash::in_memory()).unwrap();

        // Half a file: keep the first bytes, drop the rest.
        let good = std::fs::read(store.stash_path()).unwrap();
        std::fs::write(store.stash_path(), &good[..good.len() / 2]).unwrap();
        let err = PersistenceProvider::<MemStash>::load(&store).unwrap_err();
        let msg = format!("{err}");
        assert!(msg.contains("stash.dat"), "must name the file: {msg}");
        assert!(msg.contains(&format!("{} bytes", good.len() / 2)), "must report the size: {msg}");
        assert!(msg.contains("corrupt or truncated"), "must give the reason: {msg}");

        // Total garbage of the same length is caught the same way.
        std::fs::write(store.stash_path(), vec![0xffu8; good.len()]).unwrap();
        assert!(format!("{}", PersistenceProvider::<MemStash>::load(&store).unwrap_err())
            .contains("stash.dat"));
    }

    /// The property that makes a backup meaningful: a write either lands completely or leaves the
    /// previous file in place. Sabotaging the staging path is the cheapest way to prove no write
    /// ever touches the target before the rename.
    #[test]
    fn a_failed_write_leaves_the_previous_file_intact_and_is_reported() {
        let dir = dir("atomic");
        let store = AtomicFsBinStore::open(&dir).unwrap();
        store.store(&MemStash::in_memory()).unwrap();
        let before = std::fs::read(store.stash_path()).unwrap();
        assert!(store.take_write_failure().is_none());

        // Block the staging file: creating it now fails, so the store must fail *before* the
        // target is touched.
        let tmp = store.stash_path().with_file_name(format!(
            "{}.tmp",
            store.stash_path().file_name().unwrap().to_string_lossy()
        ));
        std::fs::create_dir(&tmp).unwrap();
        // `store` returns Ok by contract (rgb-ops would panic on an error inside a transaction),
        // but the failure must be waiting for the engine.
        PersistenceProvider::<MemStash>::store(&store, &MemStash::in_memory()).unwrap();
        let reported = store.take_write_failure().expect("the failure must be reported, not lost");
        assert!(reported.contains("cannot be written atomically"), "got: {reported}");
        assert!(reported.contains("previous file is untouched"), "got: {reported}");
        assert!(store.take_write_failure().is_none(), "reported once, not repeated forever");

        assert_eq!(
            std::fs::read(store.stash_path()).unwrap(),
            before,
            "the target must be byte-identical to what it was before the failed write"
        );
        PersistenceProvider::<MemStash>::load(&store).expect("and it must still decode");
        std::fs::remove_dir(&tmp).unwrap();

        // A later success must work normally (the flag does not poison the store).
        PersistenceProvider::<MemStash>::store(&store, &MemStash::in_memory()).unwrap();
        assert!(store.take_write_failure().is_none(), "a successful write reports nothing");
    }

    /// The flag is shared with the copies the crate keeps inside the `Stock` — otherwise the
    /// engine would never see a failure raised by the provider the Stock writes through.
    #[test]
    fn the_reported_failure_is_visible_through_a_clone() {
        let dir = dir("clone");
        let store = AtomicFsBinStore::open(&dir).unwrap();
        let crate_copy = store.clone();
        let tmp = store.stash_path().with_file_name(format!(
            "{}.tmp",
            store.stash_path().file_name().unwrap().to_string_lossy()
        ));
        std::fs::create_dir(&tmp).unwrap();
        // The clone writes (as the Stock's own provider does)…
        PersistenceProvider::<MemStash>::store(&crate_copy, &MemStash::in_memory()).unwrap();
        // …and the engine's handle sees it.
        assert!(store.take_write_failure().is_some());
        std::fs::remove_dir(&tmp).unwrap();
    }

    #[test]
    fn a_successful_write_leaves_no_staging_file_behind() {
        let dir = dir("clean");
        let store = AtomicFsBinStore::open(&dir).unwrap();
        store.store(&MemStash::in_memory()).unwrap();
        let leftovers: Vec<String> = std::fs::read_dir(&dir)
            .unwrap()
            .filter_map(|e| e.ok())
            .map(|e| e.file_name().to_string_lossy().into_owned())
            .filter(|n| n.ends_with(".tmp"))
            .collect();
        assert!(leftovers.is_empty(), "staging files must not accumulate: {leftovers:?}");
    }
}
