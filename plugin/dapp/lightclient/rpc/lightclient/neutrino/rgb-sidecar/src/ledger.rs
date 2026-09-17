//! Sidecar application ledger: persistent bookkeeping of assets, RGB seals and receives.
//!
//! The RGB consensus state lives in the rgb-ops `Stock` (persisted as bin files by
//! `FsBinStore`). This JSON ledger is the *sidecar's own* bookkeeping that the Stock does
//! not track: symbol→asset mapping, seal lifecycle, receive_id↔SecretSeal mapping and the
//! incoming-transfer (receive) records surfaced by the gRPC API.
//!
//! # O3：热账本 / 冷归档分层
//!
//! 这里只放**热状态**（`assets` / `seals` / `receives` / `settled_by_outpoint` /
//! `finalized_withdrawals`），约 360–500 B/条；提现构建的重放存档（`RecordedWithdrawal`，
//! 约 19.5 KB/条）搬到 [`crate::build_store`] 的 `builds/` 冷层，**每笔一个文件**。
//!
//! 改前两者同住一个文件，于是**每次状态变更都要重写全部提现历史**（W=1e4 时 204 MB /
//! 337 ms，且在全局 engine 锁内 ⇒ 侧车状态变更速率 ~3 次/秒）。分层后热账本体量只跟
//! "活跃 seal/收据数"有关，与提现笔数解耦。
//!
//! ## 崩溃语义
//!
//! 热账本仍是**单文件原子快照**：`<tmp>` 写入 → `fsync` → `rename` → 目录 `fsync`。
//! 崩溃后磁盘上要么是上一份完整账本、要么是新的完整账本，**不存在半份**。
//! 与冷层的一致性靠**写入顺序**保证（见 `build_store` 模块文档）：归档先落盘，热账本后落盘。
//!
//! `fsync` 失败现在是**硬错误**（改前被 `.ok()` 吞掉）：dirty page 没落盘就 rename，崩溃后可能
//! 留下一个被截断的账本 —— 对一个管钱的状态文件，宁可当场报错也不要留下这种可能。

use std::collections::BTreeMap;
use std::fs;
use std::io::Write;
use std::path::Path;

use anyhow::{anyhow, Context, Result};
use serde::{Deserialize, Serialize};

use crate::types::{recv_status, SealStatus, SealTxOut};

/// An RGB20/IFA asset known to the sidecar.
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct AssetRec {
    pub asset_id: String,
    pub symbol: String,
    pub schema: String,
    pub precision: u8,
    pub issued_supply: i64,
    /// Hex of the serialized genesis contract consignment (so the contract can be
    /// re-imported if the Stock is rebuilt).
    pub genesis_consignment_hex: String,
}

/// An incoming RGB transfer (a receive) tracked by the sidecar.
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct ReceiveRec {
    pub receive_id: String,
    pub invoice: String,
    pub asset_symbol: String,
    pub asset_id: String,
    pub amount_requested: i64,
    pub status: String,
    pub settled_amount: i64,
    pub txid: String,
    pub vout: u32,
    /// Hex of the `SecretSeal` used for a blinded receive (None for witness receives).
    pub secret_seal_hex: Option<String>,
}

/// A withdrawal whose RGB transition has already been merged into the Stock.
///
/// Persisted (not just kept in memory) so `finalize_withdrawal` stays idempotent **across a
/// sidecar restart**: the bridge retries a withdrawal whose broadcast failed by rebuilding the
/// very same transaction (same seals ⇒ same txid), and that retry calls finalize again. With an
/// in-memory-only record the retry would hit "no pending withdrawal" — or worse, re-merge the
/// same fascia into the Stock — and the on-chain burn would stay stuck forever.
///
/// **不裁剪**：这条记录是 finalize 的幂等凭据，只有几十字节，删掉它换不来什么，却能让一笔
/// "链上已发生"的提现在重试时报 "no pending withdrawal"（旧代码里那正是"burn 永远卡住"的根因）。
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct FinalizedWithdrawalRec {
    pub txid: String,
    pub recipient_outpoint: String,
    pub change_outpoint: Option<String>,
    /// [`crate::build_store::key_digest`] of the build's seal set: points at the archived
    /// [`RecordedWithdrawal`], so the archive can be pruned **without scanning `builds/`**
    /// (the hot ledger already lists every finalized withdrawal).
    ///
    /// `None` for records written before O3 (or a legacy ledger that has not been migrated);
    /// such a build is simply never pruned — the conservative direction.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub seal_set_digest: Option<String>,
}

/// A withdrawal build, recorded so that a retry can replay the **same transaction** byte for byte.
///
/// Rebuilding from the same seals is *not* enough to reproduce a txid: each build draws fresh
/// random blinding factors for the recipient/change seals (`BlindSeal::new_random_vout`), so the
/// RGB commitment written into the carrier tx — and with it the txid — differs every time. Only
/// the built PSBT itself can be replayed; anything else is a second, conflicting spend of the same
/// seal (and, if it lands first, a consignment handed to the user that does not match the tx the
/// network accepted).
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct RecordedWithdrawal {
    /// txid of the built (and, on the first attempt, broadcast) transaction.
    pub txid: String,
    pub asset_id: String,
    pub asset_symbol: String,
    /// The seal inputs of the build, in the order the tx spends them.
    pub input_outpoints: Vec<String>,
    #[serde(default)]
    pub change_vout: Option<u32>,
    pub change_amount: i64,
    /// Hex of the built (unsigned) PSBT — the object that is replayed.
    pub psbt_hex: String,
    /// Hex of the build's consignment (what the user is handed with the withdrawal).
    pub consignment_hex: String,
    /// Per-input asset amounts (`BuildWithdrawalResponse.input_amounts`).
    pub input_amounts: Vec<i64>,
    /// Hex of the build's RGB fascia: merging it into the Stock is what registers the transition's
    /// output seals. Needed to finalize a replayed build whose transition was never merged (the
    /// first attempt failed before finalize, or the sidecar restarted in between).
    pub fascia_hex: String,
}

/// The whole persistent bookkeeping state (**hot layer only** — withdrawal builds live in
/// [`crate::build_store`], see the module docs).
///
/// Deliberately **not** `Serialize`/`Deserialize`: the on-disk shape is [`LedgerFile`], and a caller
/// serializing `Ledger` directly would silently drop [`Ledger::legacy_builds`]. Go through
/// [`Ledger::load`] / [`Ledger::save`].
#[derive(Clone, Debug, Default)]
pub struct Ledger {
    pub assets: BTreeMap<String, AssetRec>,
    pub seals: BTreeMap<String, SealTxOut>,
    pub receives: BTreeMap<String, ReceiveRec>,
    /// index receive_id -> outpoint for settled receives (reverse lookup for attribution).
    pub settled_by_outpoint: BTreeMap<String, String>,
    /// Finalized withdrawals by txid.
    pub finalized_withdrawals: BTreeMap<String, FinalizedWithdrawalRec>,
    /// Pre-O3 `withdrawal_builds` read back from an old `ledger.json`.
    ///
    /// [`Ledger::load`] parks them here so [`crate::engine::RgbEngine::open`] can move them into the
    /// cold archive *before* anything else happens. [`Ledger::save`] refuses to run while they are
    /// still here, so a pre-O3 deployment cannot silently lose its replay archive by saving first.
    pub legacy_builds: BTreeMap<String, RecordedWithdrawal>,
}

/// The **on-disk** shape of `ledger.json`, read side.
///
/// Separate from [`Ledger`] on purpose: the file is a frozen format (other tools, backups and the
/// Go bridge's docs refer to it) and it is the only place the pre-O3 `withdrawal_builds` field is
/// still understood.
#[derive(Deserialize)]
struct LedgerFile {
    #[serde(default)]
    assets: BTreeMap<String, AssetRec>,
    #[serde(default)]
    seals: BTreeMap<String, SealTxOut>,
    #[serde(default)]
    receives: BTreeMap<String, ReceiveRec>,
    #[serde(default)]
    settled_by_outpoint: BTreeMap<String, String>,
    #[serde(default)]
    finalized_withdrawals: BTreeMap<String, FinalizedWithdrawalRec>,
    /// Legacy: pre-O3 the build archive lived here. Read for migration, never written back.
    #[serde(default)]
    withdrawal_builds: BTreeMap<String, RecordedWithdrawal>,
}

/// Write side of the same format: borrows the maps instead of cloning them (the hot ledger is
/// rewritten on **every** state change, so an extra full copy would be a real cost).
#[derive(Serialize)]
struct LedgerFileRef<'a> {
    assets: &'a BTreeMap<String, AssetRec>,
    seals: &'a BTreeMap<String, SealTxOut>,
    receives: &'a BTreeMap<String, ReceiveRec>,
    settled_by_outpoint: &'a BTreeMap<String, String>,
    finalized_withdrawals: &'a BTreeMap<String, FinalizedWithdrawalRec>,
}

impl Ledger {
    pub fn load(path: &Path) -> Result<Self> {
        if !path.exists() {
            return Ok(Self::default());
        }
        let data = fs::read(path)
            .with_context(|| format!("read ledger {}", path.display()))?;
        let file: LedgerFile = serde_json::from_slice(&data)
            .with_context(|| format!("parse ledger {}", path.display()))?;
        Ok(Self {
            assets: file.assets,
            seals: file.seals,
            receives: file.receives,
            settled_by_outpoint: file.settled_by_outpoint,
            finalized_withdrawals: file.finalized_withdrawals,
            legacy_builds: file.withdrawal_builds,
        })
    }

    /// Take the pre-O3 build archive so the caller can move it to the cold layer.
    pub fn take_legacy_builds(&mut self) -> BTreeMap<String, RecordedWithdrawal> {
        std::mem::take(&mut self.legacy_builds)
    }

    pub fn save(&self, path: &Path) -> Result<()> {
        if !self.legacy_builds.is_empty() {
            return Err(anyhow!(
                "refusing to save ledger {}: it still holds {} un-migrated pre-O3 withdrawal \
                 builds (they belong in the build archive; see RgbEngine::open)",
                path.display(),
                self.legacy_builds.len()
            ));
        }
        let file = LedgerFileRef {
            assets: &self.assets,
            seals: &self.seals,
            receives: &self.receives,
            settled_by_outpoint: &self.settled_by_outpoint,
            finalized_withdrawals: &self.finalized_withdrawals,
        };
        let json = serde_json::to_vec_pretty(&file)?;
        write_atomic(path, &json)
    }

    // ---- assets ----

    pub fn asset(&self, symbol: &str) -> Option<&AssetRec> {
        self.assets.values().find(|a| a.symbol == symbol)
    }

    pub fn upsert_asset(&mut self, rec: AssetRec) {
        self.assets.insert(rec.asset_id.clone(), rec);
    }

    // ---- seals ----

    pub fn seal(&self, outpoint: &str) -> Option<&SealTxOut> {
        self.seals.get(outpoint)
    }

    pub fn upsert_seal(&mut self, seal: SealTxOut) {
        self.seals.insert(seal.outpoint.clone(), seal);
    }

    pub fn seals_for<'a>(&'a self, symbol: &'a str) -> impl Iterator<Item = &'a SealTxOut> + 'a {
        self.seals
            .values()
            .filter(move |s| s.asset_symbol == symbol)
    }

    /// Select spendable (minted, not consumed) seals for `symbol` covering at least `need`.
    /// Returns (chosen seals, total amount, excess=change).
    pub fn select_seals<'a>(&'a self, symbol: &'a str, need: i64) -> Result<(Vec<&'a SealTxOut>, i64)> {
        let mut chosen = Vec::new();
        let mut total = 0i64;
        for s in self.seals_for(symbol) {
            if s.status == SealStatus::Minted {
                chosen.push(s);
                total = total.saturating_add(s.amount);
                if total >= need {
                    break;
                }
            }
        }
        if total < need {
            return Err(anyhow!(
                "insufficient {}: need {need}, have {total} (across {} minted seals)",
                symbol,
                chosen.len()
            ));
        }
        Ok((chosen, total))
    }

    // ---- receives ----

    pub fn upsert_receive(&mut self, rec: ReceiveRec) {
        self.receives.insert(rec.receive_id.clone(), rec);
    }

    pub fn receive(&self, id: &str) -> Option<&ReceiveRec> {
        self.receives.get(id)
    }

    pub fn mark_settled(&mut self, id: &str, amount: i64, asset_id: &str, txid: &str, vout: u32) -> Result<()> {
        let rec = self
            .receives
            .get_mut(id)
            .ok_or_else(|| anyhow!("receive {id} not found"))?;
        rec.status = recv_status::SETTLED.to_string();
        rec.settled_amount = amount;
        rec.asset_id = asset_id.to_string();
        rec.txid = txid.to_string();
        rec.vout = vout;
        self.settled_by_outpoint
            .insert(format!("{txid}:{vout}"), id.to_string());
        Ok(())
    }

    /// Reverse lookup: which receive_id settled the given outpoint.
    pub fn receive_for_outpoint(&self, outpoint: &str) -> Option<&str> {
        self.settled_by_outpoint.get(outpoint).map(String::as_str)
    }
}

/// `<path>.tmp` 写入 → `fsync` → `rename` → 目录 `fsync`。
///
/// 崩溃语义：任何时刻磁盘上的目标文件要么是**旧的完整版本**、要么是**新的完整版本**，
/// 不会出现半份。`fsync` 失败是**硬错误**（不是 best-effort）：跳过 fsync 直接 rename 的实现，
/// 崩溃后可能留下一个内容被截断、却已经顶替掉旧版本的文件。
pub(crate) fn write_atomic(path: &Path, bytes: &[u8]) -> Result<()> {
    if let Some(dir) = path.parent() {
        fs::create_dir_all(dir).with_context(|| format!("create {}", dir.display()))?;
    }
    let tmp = path.with_extension("json.tmp");
    {
        let mut f = fs::File::create(&tmp).with_context(|| format!("create {}", tmp.display()))?;
        f.write_all(bytes)
            .with_context(|| format!("write {}", tmp.display()))?;
        f.sync_all()
            .with_context(|| format!("fsync {}", tmp.display()))?;
    }
    fs::rename(&tmp, path).with_context(|| format!("rename to {}", path.display()))?;
    sync_dir(path.parent());
    Ok(())
}

/// 目录 fsync：让 `rename` 本身也持久。best-effort —— 有些文件系统不支持对目录 fsync，
/// 那不是错误（文件内容已经 fsync 过，最坏情况是回退到上一份完整版本）。
pub(crate) fn sync_dir(dir: Option<&Path>) {
    #[cfg(unix)]
    if let Some(dir) = dir {
        if let Ok(d) = fs::File::open(dir) {
            let _ = d.sync_all();
        }
    }
    #[cfg(not(unix))]
    let _ = dir;
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::types::SealStatus;

    fn temp_dir(name: &str) -> std::path::PathBuf {
        let dir = std::env::temp_dir().join(format!("o3-ledger-{name}-{}", std::process::id()));
        let _ = fs::remove_dir_all(&dir);
        dir
    }

    fn seal(i: usize) -> SealTxOut {
        SealTxOut {
            outpoint: format!("{:064x}:2", i),
            asset_id: "rgb:test".into(),
            asset_symbol: "USDT".into(),
            amount: 300_000,
            btc_value: 999_987_824,
            maturity_height: 524,
            status: SealStatus::Minted,
            secret_seal_hex: None,
        }
    }

    fn legacy_build(i: usize) -> RecordedWithdrawal {
        RecordedWithdrawal {
            txid: format!("{:064x}", i),
            asset_id: "rgb:test".into(),
            asset_symbol: "USDT".into(),
            input_outpoints: vec![format!("{:064x}:2", i)],
            change_vout: Some(2),
            change_amount: 100_000,
            psbt_hex: "ab".repeat(10),
            consignment_hex: "cd".repeat(10),
            input_amounts: vec![300_000],
            fascia_hex: "ef".repeat(10),
        }
    }

    #[test]
    fn round_trips_the_hot_state() {
        let dir = temp_dir("roundtrip");
        let path = dir.join("ledger.json");
        let mut l = Ledger::default();
        l.upsert_seal(seal(1));
        l.finalized_withdrawals.insert(
            format!("{:064x}", 1),
            FinalizedWithdrawalRec {
                txid: format!("{:064x}", 1),
                recipient_outpoint: format!("{:064x}:1", 1),
                change_outpoint: Some(format!("{:064x}:2", 1)),
                seal_set_digest: Some("d".repeat(64)),
            },
        );
        l.save(&path).unwrap();
        let back = Ledger::load(&path).unwrap();
        assert_eq!(back.seals.len(), 1);
        assert_eq!(back.seals.get(&seal(1).outpoint).map(|s| s.status), Some(SealStatus::Minted));
        assert_eq!(
            back.finalized_withdrawals.values().next().unwrap().seal_set_digest,
            Some("d".repeat(64))
        );
        // The hot file must not mention the build archive at all any more.
        let text = fs::read_to_string(&path).unwrap();
        assert!(!text.contains("withdrawal_builds"), "hot ledger: {text}");
        let _ = fs::remove_dir_all(&dir);
    }

    /// O3 migration: an old `ledger.json` still holding `withdrawal_builds` must load, expose them
    /// for migration, and refuse to be saved until they have been moved out.
    #[test]
    fn legacy_ledger_loads_and_is_migrated_not_dropped() {
        let dir = temp_dir("legacy");
        let path = dir.join("ledger.json");
        fs::create_dir_all(&dir).unwrap();
        let node = serde_json::json!({
            "assets": {},
            "seals": { seal(1).outpoint: serde_json::to_value(seal(1)).unwrap() },
            "receives": {},
            "settled_by_outpoint": {},
            "finalized_withdrawals": {},
            "withdrawal_builds": { "k": serde_json::to_value(legacy_build(1)).unwrap() },
        });
        fs::write(&path, serde_json::to_vec_pretty(&node).unwrap()).unwrap();

        let mut l = Ledger::load(&path).unwrap();
        assert_eq!(l.seals.len(), 1, "the hot state must still load");
        assert_eq!(l.legacy_builds.len(), 1, "the pre-O3 archive must be picked up");
        let err = l.save(&path).expect_err("an un-migrated ledger must refuse to save");
        assert!(format!("{err:#}").contains("un-migrated"), "unexpected: {err:#}");

        let legacy = l.take_legacy_builds();
        assert_eq!(legacy.len(), 1);
        l.save(&path).unwrap();
        let back = Ledger::load(&path).unwrap();
        assert!(back.legacy_builds.is_empty());
        assert_eq!(back.seals.len(), 1);
        let _ = fs::remove_dir_all(&dir);
    }

    /// A record written before O3 (no `seal_set_digest`) must still load — `serde(default)`.
    #[test]
    fn finalized_record_without_digest_still_loads() {
        let dir = temp_dir("oldfin");
        let path = dir.join("ledger.json");
        fs::create_dir_all(&dir).unwrap();
        let node = serde_json::json!({
            "assets": {}, "seals": {}, "receives": {}, "settled_by_outpoint": {},
            "finalized_withdrawals": {
                "aa": {"txid": "aa", "recipient_outpoint": "aa:1", "change_outpoint": "aa:2"}
            }
        });
        fs::write(&path, serde_json::to_vec_pretty(&node).unwrap()).unwrap();
        let l = Ledger::load(&path).unwrap();
        let rec = l.finalized_withdrawals.get("aa").unwrap();
        assert_eq!(rec.seal_set_digest, None);
        assert_eq!(rec.change_outpoint.as_deref(), Some("aa:2"));
        let _ = fs::remove_dir_all(&dir);
    }
}
