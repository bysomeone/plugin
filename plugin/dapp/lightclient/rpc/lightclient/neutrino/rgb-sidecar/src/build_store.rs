//! 提现构建（[`RecordedWithdrawal`]）的**冷层归档**：分层 / 压缩 / 裁剪（O3）。
//!
//! # 为什么要分层
//!
//! 改前整个账本是一个 `ledger.json`，**每次状态变更全量重写 + fsync**，而且发生在
//! `service.rs` 的全局 `Mutex<RgbEngine>` 里。而 `withdrawal_builds`（提现构建的重放存档）
//! 是里面唯一的巨物：实测一条 ≈ 19.5 KB（psbt 1994 + consignment 15926 + fascia 1552 字符），
//! 于是 W 笔提现 = 一次 save 重写 `W × 19.5 KB`：
//!
//! | W | 单次 save | 单次 load | 侧车状态变更速率 |
//! |---|---|---|---|
//! | 1e3 | 38 ms | 12 ms | ~26 /s |
//! | 1e4 | 337 ms | 145 ms | **~3 /s** |
//! | 5e4 | 1493 ms | 983 ms | ~0.7 /s |
//!
//! 分层后热账本（seals/receives/finalized，~360 B/条）与提现笔数解耦，存档只在**重放**时按
//! seal 集合读回单个文件。
//!
//! # 归档窗口（哪些 build 可以裁剪，裁剪成什么样）
//!
//! [`RecordedWithdrawal`] 存在的唯一理由是 **E9-A 幂等重放**：桥广播失败后拿同一组 seal 重试，
//! 侧车必须回同一份 PSBT（同 txid），否则就是同一笔链上销毁的**第二笔支付**。所以：
//!
//! - **可以裁剪**：构建已 finalize（`ledger.finalized_withdrawals` 有记录 ⇒ 转账已并入 Stock、
//!   seal 状态已落盘）**且**锚定交易已有 `retain_confirmations` 个确认。判据是**本地可证的**
//!   确认高度：change seal 的 `maturity_height`（`sync()` 从钱包 UTXO 视图刷新，0 = 未确认）。
//!   一笔已确认的提现不需要重放——广播没失败才会上链；重试窗口是"分钟"级，确认窗口是"块"级。
//! - **裁剪 = 留墓碑，不是删除**：原文件重写成"同一个 seal 集合 / 同一个 txid，但不再有 PSBT
//!   载荷"（约 400 B，比 19.5 KB 小约 50×）。这样重放请求会撞上一条**指名道姓的**错误
//!   （"构建已于高度 H 裁剪"），而不是含混的"查不到"。
//! - **绝不裁剪**：未 finalize 的构建（哪怕链上已经有它的交易——那正是最需要重放的情形）、
//!   没有 change seal 的构建（拿不到确认高度的证明）、以及 change seal 的 `maturity_height`
//!   仍为 0 的构建。宁可留着归档，不可让重放取不回。
//!
//! 拿不回时**一律响亮失败**（[`BuildStore::get`] 返回 `Err`，绝不静默重建）：静默重建正是 E9-A
//! 要堵的那个洞。
//!
//! # 压缩
//!
//! 三个大字段是 hex（本身 2× 膨胀）。落盘时先 hex→raw，再把三段拼成一个 deflate 帧、base64 回
//! 文本（JSON 可读）。实测一条 19,472 字符 → 4,542 B 压缩 + base64 ≈ 6 KB，**3.2×**。
//!
//! 选了 `flate2` 的纯 Rust 后端（miniz_oxide）：在同一份真实数据上，deflate-9 与 zstd-3/19、
//! lz4、xz 的差距都在 7% 以内（4542 / 4578 / 4481 / 4871 / 4416 B），而 zstd 要拖进 C 构建链
//! （`zstd-sys`）。consignment 是 RGB 私密数据，压缩只落本地盘，**不上传不外发**。
//!
//! # 崩溃语义
//!
//! - 每个归档文件：写 `<name>.tmp` → `fsync` → `rename`（覆盖式）→ 目录 `fsync`。
//!   任何时刻磁盘上要么是旧版本、要么是新版本，**不存在半份归档**。
//! - 跨文件顺序：`build_withdrawal` 里**先**落归档（并对 IO 错误硬失败），**再** `save()` 热账本
//!   + Stock，最后才把结果返回给调用方。⇒ 调用方拿到的那份 PSBT 一定已经可重放。
//! - 裁剪是"同一个文件的重写"，同样是 tmp+rename 原子替换。裁剪前崩溃 = 归档还在（可重放），
//!   裁剪后崩溃 = 墓碑（重放响亮失败）。两种都不会出现"半份"。
//!
//! `builds/` 目录里全是**只写一次、极少读**的文件（除裁剪那一次重写），所以它不参与热路径的
//! 全量重写——这正是 O3 要拿掉的写放大。

use std::fs;
use std::io::{Read, Write};
use std::path::{Path, PathBuf};
use std::time::{SystemTime, UNIX_EPOCH};

use anyhow::{anyhow, bail, Context, Result};
use base64::Engine as _;
use bitcoin::hashes::{sha256, Hash as _};
use flate2::read::DeflateDecoder;
use flate2::write::DeflateEncoder;
use flate2::Compression;
use serde::{Deserialize, Serialize};

use crate::ledger::RecordedWithdrawal;

/// 归档格式标识：读到别的值一律报错（不猜、不降级）。
pub const FORMAT: &str = "rgb-sidecar-build/1";

/// 载荷编码方式。只认 `deflate`；未知值报错。
const CODEC_DEFLATE: &str = "deflate";

/// deflate 级别。实测（真实 consignment，9,736 B raw）：级别 6 → 4,742 B，级别 9 → 4,542 B
/// （差 4%，代价约 40% 时间）。每笔提现只压一次、且在全局锁内，取快的那档。
const COMPRESSION_LEVEL: u32 = 6;

/// 归档文件的后缀（JSON 文本，`head -c 400` 可直接看头部）。
pub const EXT: &str = "json";

/// 载荷上限（解压后的原始字节）。真实一条构建 ≈10 KB 量级；这条界只为拦住损坏的长度字段
/// 导致的天量分配，不是业务约束。
const MAX_PAYLOAD_BYTES: usize = 64 * 1024 * 1024;

/// seal 集合的稳定摘要（归档文件名）。集合 = 排序去重后的 outpoint 列表，所以**与顺序无关**：
/// 同一次重放无论 seal 以什么顺序传来，都落到同一个文件。
pub fn key_digest(seal_set_key: &str) -> String {
    sha256::Hash::hash(seal_set_key.as_bytes()).to_string()
}

/// 裁剪标记：构建的载荷已被丢弃，只剩"它曾经存在、属于哪个 txid、为什么被裁"。
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Pruned {
    /// 裁剪时的链尖高度。
    pub btc_height: u32,
    /// 锚定交易的确认高度（change seal 的 `maturity_height`）。
    pub confirm_height: u32,
    /// 裁剪理由（写给人看）。
    pub reason: String,
}

/// 归档文件的磁盘形态。
#[derive(Debug, Serialize, Deserialize)]
struct BuildFile {
    format: String,
    /// 该构建记录的 seal 集合键（`engine::seal_set_key`）。读回时重新校验。
    seal_set_key: String,
    txid: String,
    asset_id: String,
    asset_symbol: String,
    input_outpoints: Vec<String>,
    #[serde(default)]
    change_vout: Option<u32>,
    change_amount: i64,
    input_amounts: Vec<i64>,
    /// 载荷三段各自的**原始字节长度**（用于切分与校验），顺序固定。
    psbt_len: u64,
    consignment_len: u64,
    fascia_len: u64,
    codec: String,
    /// `deflate(psbt || consignment || fascia)` 的 base64；裁剪后为 `None`。
    #[serde(default, skip_serializing_if = "Option::is_none")]
    payload_b64: Option<String>,
    /// 裁剪信息；`None` = 载荷完整。
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pruned: Option<Pruned>,
    /// 归档写入时间（审计用；旧文件可能没有）。
    #[serde(default)]
    built_at_unix: u64,
}

/// build 冷层归档：`<data_dir>/builds/<sha256(seal_set_key)>.json`。
#[derive(Clone, Debug)]
pub struct BuildStore {
    dir: PathBuf,
}

impl BuildStore {
    pub fn new(dir: PathBuf) -> Self {
        Self { dir }
    }

    pub fn dir(&self) -> &Path {
        &self.dir
    }

    /// 某个 seal 集合的归档文件路径。
    pub fn path_for(&self, seal_set_key: &str) -> PathBuf {
        self.path_for_digest(&key_digest(seal_set_key))
    }

    /// 按摘要定位归档文件（热账本的 `finalized_withdrawals` 只记摘要，不记 seal 集合本身）。
    pub fn path_for_digest(&self, digest: &str) -> PathBuf {
        self.dir.join(format!("{digest}.{EXT}"))
    }

    /// 取回某 seal 集合的构建记录。
    ///
    /// - `Ok(None)`：**没有**该集合的归档（从未构建过，或文件被外部删了）。调用方按"记录不存在"
    ///   走 fail-closed 路径。
    /// - `Err`：归档**存在但取不回**（已裁剪、格式不认识、载荷损坏、seal 集合对不上）。这类情况
    ///   必须炸出来，绝不能退化成 `Ok(None)` —— 那正是 E9-A 的"重建出另一笔交易"。
    pub fn get(&self, seal_set_key: &str) -> Result<Option<RecordedWithdrawal>> {
        let path = self.path_for(seal_set_key);
        if !path.exists() {
            return Ok(None);
        }
        let raw = fs::read(&path).with_context(|| format!("read build archive {}", path.display()))?;
        let file: BuildFile = serde_json::from_slice(&raw)
            .map_err(|e| anyhow!("build archive {}: {e}", path.display()))?;
        if file.format != FORMAT {
            bail!(
                "build archive {}: unknown format {:?} (expected {FORMAT})",
                path.display(),
                file.format
            );
        }
        // 文件名是 hash，字节内容才是真相：两边必须自洽，否则宁可报错也不要交出别人的构建。
        if file.seal_set_key != seal_set_key {
            bail!(
                "build archive {}: seal set mismatch (file holds {:?}, lookup was {seal_set_key:?})",
                path.display(),
                file.seal_set_key
            );
        }
        let recomputed = crate::engine::seal_set_key_of_strs(&file.input_outpoints);
        if recomputed != seal_set_key {
            bail!(
                "build archive {}: input outpoints do not hash to the lookup key ({recomputed:?} != {seal_set_key:?})",
                path.display()
            );
        }
        if let Some(p) = &file.pruned {
            bail!(
                "build archive {}: the build for this seal set (txid {}) was pruned at BTC height {} \
                 (confirmed at {}, {}) — the original PSBT is no longer replayable",
                path.display(),
                file.txid,
                p.btc_height,
                p.confirm_height,
                p.reason
            );
        }
        let Some(payload_b64) = file.payload_b64.as_ref() else {
            bail!(
                "build archive {}: no payload and no prune marker (corrupt archive for txid {})",
                path.display(),
                file.txid
            );
        };
        if file.codec != CODEC_DEFLATE {
            bail!(
                "build archive {}: unknown codec {:?}",
                path.display(),
                file.codec
            );
        }
        let comp = base64::engine::general_purpose::STANDARD
            .decode(payload_b64)
            .map_err(|e| anyhow!("build archive {}: base64: {e}", path.display()))?;
        let want = (file.psbt_len + file.consignment_len + file.fascia_len) as usize;
        // 一个损坏的（或被人手改过的）长度字段不该变成一次几十 GB 的分配：真实的一条构建 ≈10 KB
        // 量级，这里给足余量即可。
        if want > MAX_PAYLOAD_BYTES {
            bail!(
                "build archive {}: header declares a {want}-byte payload (max {MAX_PAYLOAD_BYTES})",
                path.display()
            );
        }
        let raw_payload = inflate(&comp, want)
            .map_err(|e| anyhow!("build archive {}: inflate: {e}", path.display()))?;
        if raw_payload.len() != want {
            bail!(
                "build archive {}: payload is {} bytes, header declares {want}",
                path.display(),
                raw_payload.len()
            );
        }
        let (psbt, rest) = raw_payload.split_at(file.psbt_len as usize);
        let (consignment, fascia) = rest.split_at(file.consignment_len as usize);
        Ok(Some(RecordedWithdrawal {
            txid: file.txid,
            asset_id: file.asset_id,
            asset_symbol: file.asset_symbol,
            input_outpoints: file.input_outpoints,
            change_vout: file.change_vout,
            change_amount: file.change_amount,
            psbt_hex: to_hex(psbt),
            consignment_hex: to_hex(consignment),
            input_amounts: file.input_amounts,
            fascia_hex: to_hex(fascia),
        }))
    }

    /// 落盘一条构建记录（覆盖同名旧记录）。返回后**保证已 fsync**：调用方可以放心把它交出去。
    pub fn put(&self, seal_set_key: &str, rec: &RecordedWithdrawal) -> Result<()> {
        let computed = crate::engine::seal_set_key_of_strs(&rec.input_outpoints);
        if computed != *seal_set_key {
            bail!(
                "build archive: record for txid {} hashes to {computed:?}, refusing to file it under {seal_set_key:?}",
                rec.txid
            );
        }
        let psbt = from_hex(&rec.psbt_hex)?;
        let consignment = from_hex(&rec.consignment_hex)?;
        let fascia = from_hex(&rec.fascia_hex)?;
        let mut raw = Vec::with_capacity(psbt.len() + consignment.len() + fascia.len());
        raw.extend_from_slice(&psbt);
        raw.extend_from_slice(&consignment);
        raw.extend_from_slice(&fascia);
        let file = BuildFile {
            format: FORMAT.to_string(),
            seal_set_key: seal_set_key.to_string(),
            txid: rec.txid.clone(),
            asset_id: rec.asset_id.clone(),
            asset_symbol: rec.asset_symbol.clone(),
            input_outpoints: rec.input_outpoints.clone(),
            change_vout: rec.change_vout,
            change_amount: rec.change_amount,
            input_amounts: rec.input_amounts.clone(),
            psbt_len: psbt.len() as u64,
            consignment_len: consignment.len() as u64,
            fascia_len: fascia.len() as u64,
            codec: CODEC_DEFLATE.to_string(),
            payload_b64: Some(
                base64::engine::general_purpose::STANDARD.encode(deflate(&raw)?),
            ),
            pruned: None,
            built_at_unix: now_unix(),
        };
        let bytes = serde_json::to_vec(&file)?;
        write_atomic(&self.path_for(seal_set_key), &bytes)
    }

    /// 按**摘要**裁剪（热账本侧的调用点：`finalized_withdrawals` 里只有摘要）。
    ///
    /// 先读回文件、核对"文件名摘要 == `key_digest(文件里的 seal 集合)`"，再走 [`Self::prune`]。
    /// 不自洽就报错，不裁。
    pub fn prune_by_digest(&self, digest: &str, pruned: Pruned) -> Result<bool> {
        let path = self.path_for_digest(digest);
        if !path.exists() {
            return Ok(false);
        }
        let raw = fs::read(&path).with_context(|| format!("read build archive {}", path.display()))?;
        let file: BuildFile = serde_json::from_slice(&raw)
            .map_err(|e| anyhow!("build archive {}: {e}", path.display()))?;
        if key_digest(&file.seal_set_key) != digest {
            bail!(
                "build archive {}: filename digest does not match its seal set, refusing to prune",
                path.display()
            );
        }
        self.prune(&file.seal_set_key, pruned)
    }

    /// 裁剪一条归档：丢掉载荷，留下"曾经有过什么、为什么被裁"。文件**不删除**。
    ///
    /// 返回 `false` 表示没有该归档（或已经是墓碑），调用方无需区分。
    pub fn prune(&self, seal_set_key: &str, pruned: Pruned) -> Result<bool> {
        let path = self.path_for(seal_set_key);
        if !path.exists() {
            return Ok(false);
        }
        let raw = fs::read(&path).with_context(|| format!("read build archive {}", path.display()))?;
        let mut file: BuildFile = serde_json::from_slice(&raw)
            .map_err(|e| anyhow!("build archive {}: {e}", path.display()))?;
        if file.format != FORMAT {
            bail!(
                "build archive {}: unknown format {:?}, refusing to prune",
                path.display(),
                file.format
            );
        }
        if file.seal_set_key != seal_set_key {
            bail!(
                "build archive {}: seal set mismatch while pruning (file holds {:?})",
                path.display(),
                file.seal_set_key
            );
        }
        if file.pruned.is_some() || file.payload_b64.is_none() {
            return Ok(false);
        }
        file.payload_b64 = None;
        file.pruned = Some(pruned);
        let bytes = serde_json::to_vec(&file)?;
        write_atomic(&path, &bytes)?;
        Ok(true)
    }

    /// 单条归档的磁盘占用（裁剪后应显著变小）；不存在 = `None`。
    pub fn file_len(&self, seal_set_key: &str) -> Option<u64> {
        fs::metadata(self.path_for(seal_set_key)).ok().map(|m| m.len())
    }
}

fn now_unix() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

fn deflate(raw: &[u8]) -> Result<Vec<u8>> {
    let mut enc = DeflateEncoder::new(Vec::new(), Compression::new(COMPRESSION_LEVEL));
    enc.write_all(raw)?;
    Ok(enc.finish()?)
}

fn inflate(comp: &[u8], expected: usize) -> Result<Vec<u8>> {
    let mut out = Vec::with_capacity(expected);
    DeflateDecoder::new(comp).read_to_end(&mut out)?;
    Ok(out)
}

fn from_hex(s: &str) -> Result<Vec<u8>> {
    use bitcoin::hashes::hex::FromHex;
    Vec::<u8>::from_hex(s).map_err(|e| anyhow!("bad hex in recorded withdrawal: {e}"))
}

fn to_hex(bytes: &[u8]) -> String {
    use bitcoin::hex::{Case, DisplayHex};
    bytes.to_hex_string(Case::Lower)
}

/// tmp → fsync → rename → 目录 fsync（与热账本同一个 [`crate::ledger::write_atomic`]）。
///
/// 与热账本不同，这里**任何一步失败都是硬错误**：归档的契约就是"返回前已在盘上"，悄悄失败等于
/// 让调用方以为能重放、实际重放不了。
fn write_atomic(path: &Path, bytes: &[u8]) -> Result<()> {
    crate::ledger::write_atomic(path, bytes)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 确定性伪随机 hex（**不可压**，代表归档的最坏情况：只有 hex→raw 这一档 1.5× 的收益）。
    fn random_hex(seed: u64, bytes: usize) -> String {
        let mut s = seed | 1;
        let mut out = String::with_capacity(bytes * 2);
        for _ in 0..bytes {
            s ^= s << 13;
            s ^= s >> 7;
            s ^= s << 17;
            out.push_str(&format!("{:02x}", (s & 0xff) as u8));
        }
        out
    }

    /// 一条提现构建记录，字段长度按实测值填（997 / 7963 / 776 字节 ⇒ 19,472 字符）。
    fn rec(i: usize) -> RecordedWithdrawal {
        RecordedWithdrawal {
            txid: format!("{:064x}", i),
            asset_id: "rgb:test".into(),
            asset_symbol: "USDT".into(),
            input_outpoints: vec![format!("{:064x}:2", i)],
            change_vout: Some(2),
            change_amount: 100_000,
            psbt_hex: random_hex(i as u64 * 3 + 1, 997),
            consignment_hex: random_hex(i as u64 * 5 + 2, 7_963),
            input_amounts: vec![300_000],
            fascia_hex: random_hex(i as u64 * 7 + 3, 776),
        }
    }

    /// 高度可压的同类记录（用来证明"压缩这一环真的在工作"）。
    fn compressible_rec(i: usize) -> RecordedWithdrawal {
        let mut r = rec(i);
        r.psbt_hex = "ab".repeat(997);
        r.consignment_hex = "cd".repeat(7_963);
        r.fascia_hex = "ef".repeat(776);
        r
    }

    /// The key the record's own outpoints hash to (the store refuses anything else).
    fn key_of(i: usize) -> String {
        format!("{:064x}:2", i)
    }

    fn temp_dir(name: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!("o3-buildstore-{name}-{}", std::process::id()));
        let _ = fs::remove_dir_all(&dir);
        dir
    }

    #[test]
    fn digest_is_order_independent() {
        let a = key_digest("x:1,y:2");
        let b = key_digest("x:1,y:2");
        assert_eq!(a, b);
        assert_ne!(a, key_digest("y:2,x:1"));
        assert_eq!(a.len(), 64);
    }

    #[test]
    fn round_trips_byte_for_byte() {
        let dir = temp_dir("roundtrip");
        let store = BuildStore::new(dir.clone());
        let r = rec(7);
        let key = key_of(7);
        store.put(&key, &r).unwrap();
        let back = store.get(&key).unwrap().expect("archive must be found");
        assert_eq!(back.txid, r.txid);
        assert_eq!(back.psbt_hex, r.psbt_hex);
        assert_eq!(back.consignment_hex, r.consignment_hex);
        assert_eq!(back.fascia_hex, r.fascia_hex);
        assert_eq!(back.input_amounts, r.input_amounts);
        assert_eq!(back.change_amount, r.change_amount);
        let _ = fs::remove_dir_all(&dir);
    }

    /// 键序回归：同一笔 tx 的多个 seal（vout 2 与 vout 10）下，**字符串序**（`seal_set_key`
    /// 先 `to_string()` 再 `sort()`）给出 `:10` 在 `:2` 之前，与 `OutPoint` 的数值序相反。
    /// 归档的写入键与读回重算键必须逐字节一致，否则记录写得进去、却再也取不回来
    /// （`get` 会报"do not hash to the lookup key"）。这条用例把两者钉在一起。
    #[test]
    fn multi_seal_same_txid_round_trips() {
        let dir = temp_dir("multi-seal");
        let store = BuildStore::new(dir.clone());
        let txid = format!("{:064x}", 0xabc);
        let mut r = rec(9);
        r.input_outpoints = vec![format!("{txid}:2"), format!("{txid}:10")];
        let key = crate::engine::seal_set_key_of_strs(&r.input_outpoints);
        assert_eq!(key, format!("{txid}:10,{txid}:2"), "字符串序，不是数值序");
        // 引擎侧（OutPoint 入参）必须算出同一个键。
        let ops: Vec<bitcoin::OutPoint> = r
            .input_outpoints
            .iter()
            .map(|s| s.parse().unwrap())
            .collect();
        assert_eq!(crate::engine::seal_set_key(&ops), key);
        store.put(&key, &r).unwrap();
        let back = store.get(&key).unwrap().expect("归档必须能按同一个键取回");
        assert_eq!(back.input_outpoints, r.input_outpoints);
        let _ = fs::remove_dir_all(&dir);
    }

    #[test]
    fn missing_archive_is_none_not_an_error() {
        let dir = temp_dir("missing");
        let store = BuildStore::new(dir);
        assert!(store.get("nope:0").unwrap().is_none());
    }

    /// 压缩真的接上了：可压数据上归档比 hex 小一个数量级以上（`payload_b64` 出现 = 压缩被
    /// 关掉/退化成"只做 base64"，这时只有 1.5×）。
    #[test]
    fn compression_is_actually_enabled() {
        let dir = temp_dir("ratio");
        let store = BuildStore::new(dir.clone());
        let r = compressible_rec(1);
        let key = key_of(1);
        store.put(&key, &r).unwrap();
        let on_disk = store.file_len(&key).unwrap();
        let hex_bytes = (r.psbt_hex.len() + r.consignment_hex.len() + r.fascia_hex.len()) as u64;
        assert!(
            on_disk * 10 < hex_bytes,
            "deflate should crush repetitive payload: {on_disk} vs {hex_bytes}"
        );
        let _ = fs::remove_dir_all(&dir);
    }

    /// 最坏情况（不可压数据）：只剩 hex→raw（2×）再 base64（1.33×）这一档，理论 ~1.5×，
    /// JSON 头部再吃掉一点 —— 断言放到 1.4×。
    #[test]
    fn incompressible_payload_still_shrinks_via_hex_to_raw() {
        let dir = temp_dir("incompressible");
        let store = BuildStore::new(dir.clone());
        let r = rec(2);
        let key = key_of(2);
        store.put(&key, &r).unwrap();
        let on_disk = store.file_len(&key).unwrap();
        let hex_bytes = (r.psbt_hex.len() + r.consignment_hex.len() + r.fascia_hex.len()) as u64;
        assert!(
            on_disk * 7 < hex_bytes * 5,
            "worst case must still be >=1.4x smaller: {on_disk} vs {hex_bytes}"
        );
        let _ = fs::remove_dir_all(&dir);
    }

    /// 损坏的归档必须**响亮失败**，绝不能退化成 `Ok(None)`（那会让重放退化成重建另一笔交易）。
    #[test]
    fn corrupt_payload_fails_loudly() {
        let dir = temp_dir("corrupt");
        let store = BuildStore::new(dir.clone());
        let key = key_of(3);
        store.put(&key, &rec(3)).unwrap();
        let path = store.path_for(&key);
        let mut file: BuildFile =
            serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
        file.payload_b64 = Some(base64::engine::general_purpose::STANDARD.encode(b"not deflate"));
        fs::write(&path, serde_json::to_vec(&file).unwrap()).unwrap();
        let err = store.get(&key).expect_err("corrupt payload must be an error");
        assert!(format!("{err:#}").contains("inflate"), "unexpected: {err:#}");

        // 载荷被截短（deflate 流本身合法，但长度对不上）也要炸。
        store.put(&key, &rec(3)).unwrap();
        let mut file: BuildFile =
            serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
        file.consignment_len = 1;
        fs::write(&path, serde_json::to_vec(&file).unwrap()).unwrap();
        let err = store.get(&key).expect_err("length mismatch must be an error");
        assert!(format!("{err:#}").contains("declares"), "unexpected: {err:#}");
        let _ = fs::remove_dir_all(&dir);
    }

    /// 文件名与内容不自洽（hash 撞了、文件被挪了）→ 报错，不交出别人的构建。
    #[test]
    fn seal_set_mismatch_fails_loudly() {
        let dir = temp_dir("mismatch");
        let store = BuildStore::new(dir.clone());
        store.put(&key_of(4), &rec(4)).unwrap();
        let path = store.path_for(&key_of(4));
        let mut file: BuildFile =
            serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
        file.input_outpoints = vec!["9999:9".into()];
        fs::write(&path, serde_json::to_vec(&file).unwrap()).unwrap();
        let err = store.get(&key_of(4)).expect_err("mismatch must be an error");
        assert!(
            format!("{err:#}").contains("do not hash to the lookup key"),
            "unexpected: {err:#}"
        );
        let _ = fs::remove_dir_all(&dir);
    }

    #[test]
    fn pruned_archive_keeps_the_txid_and_fails_loudly() {
        let dir = temp_dir("prune");
        let store = BuildStore::new(dir.clone());
        let key = key_of(5);
        store.put(&key, &rec(5)).unwrap();
        let full = store.file_len(&key).unwrap();
        assert!(store
            .prune(
                &key,
                Pruned {
                    btc_height: 900,
                    confirm_height: 700,
                    reason: "finalized + 200 confirmations".into(),
                }
            )
            .unwrap());
        let pruned_len = store.file_len(&key).unwrap();
        assert!(
            pruned_len * 4 < full,
            "the tombstone must be much smaller than the payload: {pruned_len} vs {full}"
        );
        // 幂等：再裁一次无副作用。
        assert!(!store
            .prune(
                &key,
                Pruned { btc_height: 901, confirm_height: 700, reason: "again".into() }
            )
            .unwrap());
        let err = store.get(&key).expect_err("a pruned build must fail loudly");
        let msg = format!("{err:#}");
        assert!(msg.contains("was pruned"), "unexpected: {msg}");
        assert!(msg.contains(&rec(5).txid), "the error must name the txid: {msg}");
        let _ = fs::remove_dir_all(&dir);
    }
}
