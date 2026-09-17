//! P2WSH 多地址并发的**局部基准**（性能评估用探针，非产品代码）。
//!
//! 量三处与"并发充值/提现"直接相关的写放大：
//!   1. **账本 save/load**（O3 的验收尺子）。同一台机器、同一份数据形状，一次跑出**改前 vs 改后**
//!      两列：改前 = 单文件 `ledger.json`（含 `withdrawal_builds`），改后 = 热账本 +
//!      `builds/` 冷层归档。改后的代码里已经没有"单文件含 builds"这个形态了，所以这里用
//!      [`LegacyLedger`]（字段与改前的 `Ledger` 逐一对应）复刻它，保证**对照始终可复现**，
//!      不依赖"跑之前先 git stash 一下"。
//!   2. **冷层归档本身**：单笔的写/读耗时与体积（hex → deflate+b64），以及 W 笔时的总占用
//!      （未裁剪 / 已裁剪成墓碑）。
//!   3. `BtcWallet::register_user_script` —— 注册表每次插入都重写**整个 JSON 文件**
//!      （`save_user_scripts`），所以 N 个用户 = O(N²) 字节。这条是 O5，本文件只做基线记录。
//!
//! 记录体积用**实测量级**（来自一次真实 E2E 运行后容器内的 `/data/ledger.json`）：
//!   `withdrawal_builds` 一条 = psbt_hex 1994 + consignment_hex 15926 + fascia_hex 1552
//!   字符 ≈ 19.5 KB；`genesis_consignment_hex` 11630 字符；seal 359 B / finalized 327 B。
//!
//! 跑法：`cargo test --release --bench perf_p2wsh -- --nocapture --test-threads=1`
//! **必须 `--test-threads=1`**：三个用例并行跑会互相抢 fsync（本机实测能差 2–5×），那样对照数字
//! 就是噪声而不是结论。
//! （用 `#[test]` 而不是 libtest 的 bench harness —— 后者要改 Cargo.toml 加
//! `harness = false`，那是产品文件，本次评估不动。）

use std::collections::BTreeMap;
use std::sync::Arc;
use std::time::Instant;

use rgb_sidecar::build_store::BuildStore;
use rgb_sidecar::ledger::{FinalizedWithdrawalRec, Ledger, RecordedWithdrawal};
use rgb_sidecar::rpc::BtcdRpc;
use rgb_sidecar::types::SealStatus;
use rgb_sidecar::wallet::{deposit_witness_script, BtcWallet};
use rgb_sidecar::SealTxOut;
use serde::{Deserialize, Serialize};

/// 确定性伪随机（xorshift64）——基准要可复现，不能用系统随机源。
struct Rng(u64);

impl Rng {
    fn next(&mut self) -> u64 {
        self.0 ^= self.0 << 13;
        self.0 ^= self.0 >> 7;
        self.0 ^= self.0 << 17;
        self.0
    }
    fn bytes(&mut self, n: usize) -> Vec<u8> {
        (0..n).map(|_| (self.next() & 0xff) as u8).collect()
    }
}

/// 造一段"像真实 consignment"的字节：RGB 的 strict encoding 里塞满了重复出现的 32 字节
/// contract id / txid 与 16 字节 seal，中间夹少量真随机字段。这样 deflate 的压缩比
/// （实测 ≈1.97×）与**真实 consignment 从 E2E 账本里量到的 1.93×** 基本一致。
///
/// 这一点必须较真：用 `"cd".repeat(7963)` 那种周期性数据测，压缩比会被吹到上千倍，验收数字
/// 就成了假的。psbt/fascia 是签名与见证数据（接近随机，压缩比 ~1.3–1.45×），这里用同一档
/// 数据不会高估它们的收益。
fn realistic_bytes(seed: u64, n: usize) -> Vec<u8> {
    let mut r = Rng(seed | 1);
    let ids: Vec<Vec<u8>> = (0..24).map(|_| r.bytes(32)).collect();
    let seals: Vec<Vec<u8>> = (0..8).map(|_| r.bytes(16)).collect();
    let mut out = Vec::with_capacity(n + 64);
    while out.len() < n {
        out.extend_from_slice(&ids[(r.next() % ids.len() as u64) as usize]);
        out.extend_from_slice(&seals[(r.next() % seals.len() as u64) as usize]);
        out.extend_from_slice(&r.bytes(24));
    }
    out.truncate(n);
    out
}

fn realistic_hex(seed: u64, n: usize) -> String {
    use bitcoin::hex::{Case, DisplayHex};
    realistic_bytes(seed, n).to_hex_string(Case::Lower)
}

/// 一条提现构建记录，字段长度按实测值填（997 / 7963 / 776 **字节** ⇒ 19,472 字符）。
fn withdrawal_rec(i: usize) -> RecordedWithdrawal {
    RecordedWithdrawal {
        txid: format!("{:064x}", i),
        asset_id: "rgb:_hOjIJ24-vPAowEi-jNlYCBx-VK6Yrlf-TrU7DnG-JW31WBU".into(),
        asset_symbol: "USDT".into(),
        input_outpoints: vec![format!("{:064x}:2", i)],
        change_vout: Some(2),
        change_amount: 100_000,
        psbt_hex: realistic_hex(i as u64 * 3 + 1, 997),
        consignment_hex: realistic_hex(i as u64 * 5 + 2, 7_963),
        input_amounts: vec![300_000],
        fascia_hex: realistic_hex(i as u64 * 7 + 3, 776),
    }
}

fn finalized_rec(i: usize) -> FinalizedWithdrawalRec {
    FinalizedWithdrawalRec {
        txid: format!("{:064x}", i),
        recipient_outpoint: format!("{:064x}:2", i),
        change_outpoint: Some(format!("{:064x}:2", i + 1)),
        seal_set_digest: Some(format!("{:064x}", i + 7)),
    }
}

/// 一笔提现留在热账本里的东西：它花掉/产生的 seal + finalized 记录（实测 359 B + 327 B）。
fn seal_rec(i: usize) -> SealTxOut {
    SealTxOut {
        outpoint: format!("{:064x}:2", i),
        asset_id: "rgb:_hOjIJ24-vPAowEi-jNlYCBx-VK6Yrlf-TrU7DnG-JW31WBU".into(),
        asset_symbol: "USDT".into(),
        amount: 300_000,
        btc_value: 999_987_824,
        maturity_height: 524,
        status: SealStatus::Consumed,
        secret_seal_hex: None,
    }
}

/// **改前的磁盘形态**：一切（热状态 + 全部提现构建）住在同一个 `ledger.json` 里，每次变更全量重写。
/// 字段与改前的 `Ledger` 完全一致（含 `seal_set_digest` 为 `None` 时被跳过），所以它的体积/耗时
/// 就是改前的基线，可以随时重跑复现。
#[derive(Serialize, Deserialize)]
struct LegacyLedger {
    assets: BTreeMap<String, serde_json::Value>,
    seals: BTreeMap<String, SealTxOut>,
    receives: BTreeMap<String, serde_json::Value>,
    settled_by_outpoint: BTreeMap<String, String>,
    finalized_withdrawals: BTreeMap<String, FinalizedWithdrawalRec>,
    withdrawal_builds: BTreeMap<String, RecordedWithdrawal>,
}

impl LegacyLedger {
    /// 与改前的 `Ledger::save` 逐行等价：写 `.tmp` → `sync_all` → `rename`。
    /// **必须带上 fsync**，否则这一列会白捡一个"不用刷盘"的便宜，对照就不成立了。
    fn save(&self, path: &std::path::Path) {
        use std::io::Write as _;
        let tmp = path.with_extension("json.tmp");
        let mut f = std::fs::File::create(&tmp).unwrap();
        f.write_all(&serde_json::to_vec_pretty(self).unwrap()).unwrap();
        f.sync_all().unwrap();
        std::fs::rename(&tmp, path).unwrap();
    }
    fn load(path: &std::path::Path) -> Self {
        serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap()
    }
}

/// 改后的热账本 + 冷层归档，按**同样的**数据形状铺开。
fn new_shape(n: usize) -> (Ledger, BuildStore, Vec<String>) {
    let mut l = Ledger::default();
    for i in 0..n {
        l.upsert_seal(seal_rec(i));
        l.finalized_withdrawals
            .insert(format!("{:064x}", i), finalized_rec(i));
    }
    let dir = std::env::temp_dir().join(format!("perf-o3-builds-{n}-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    let store = BuildStore::new(dir);
    let mut keys = Vec::with_capacity(n);
    for i in 0..n {
        let rec = withdrawal_rec(i);
        let key = rgb_sidecar::engine::seal_set_key_of_strs(&rec.input_outpoints);
        store.put(&key, &rec).unwrap();
        keys.push(key);
    }
    (l, store, keys)
}

#[test]
fn perf_ledger_save_load() {
    println!("=== O3 对照：账本 save/load（W 笔提现；同一台机器、同一份数据形状）===");
    println!(
        "{:>7} | {:>28} | {:>26} | {:>22}",
        "W", "改前（单文件含 builds）", "改后（热账本）", "改后（冷层归档）"
    );
    println!("{}", "-".repeat(96));
    for n in [100usize, 1_000, 10_000, 50_000] {
        // ---- 改前 ----
        let legacy_dir = std::env::temp_dir().join(format!("perf-legacy-{n}-{}", std::process::id()));
        std::fs::create_dir_all(&legacy_dir).unwrap();
        let legacy_path = legacy_dir.join("ledger.json");
        let legacy = LegacyLedger {
            assets: BTreeMap::new(),
            seals: (0..n).map(|i| (seal_rec(i).outpoint.clone(), seal_rec(i))).collect(),
            receives: BTreeMap::new(),
            settled_by_outpoint: BTreeMap::new(),
            finalized_withdrawals: (0..n)
                .map(|i| {
                    let mut f = finalized_rec(i);
                    f.seal_set_digest = None; // 改前没有这个字段
                    (f.txid.clone(), f)
                })
                .collect(),
            withdrawal_builds: (0..n)
                .map(|i| {
                    let rec = withdrawal_rec(i);
                    (format!("{}:2", rec.txid), rec)
                })
                .collect(),
        };
        legacy.save(&legacy_path); // 预热
        let legacy_bytes = std::fs::metadata(&legacy_path).unwrap().len();
        let rounds = if n >= 10_000 { 3 } else { 10 };
        let t = Instant::now();
        for _ in 0..rounds {
            legacy.save(&legacy_path);
        }
        let legacy_save = t.elapsed() / rounds;
        let t = Instant::now();
        let legacy_loaded = LegacyLedger::load(&legacy_path);
        let legacy_load = t.elapsed();
        assert_eq!(legacy_loaded.withdrawal_builds.len(), n);

        // ---- 改后 ----
        let (ledger, store, keys) = new_shape(n);
        let hot_path = legacy_dir.join("hot.json");
        ledger.save(&hot_path).unwrap(); // 预热
        let hot_bytes = std::fs::metadata(&hot_path).unwrap().len();
        let t = Instant::now();
        for _ in 0..rounds {
            ledger.save(&hot_path).unwrap();
        }
        let hot_save = t.elapsed() / rounds;
        let t = Instant::now();
        let hot_loaded = Ledger::load(&hot_path).unwrap();
        let hot_load = t.elapsed();
        assert_eq!(hot_loaded.finalized_withdrawals.len(), n);

        let archive_bytes: u64 = keys.iter().filter_map(|k| store.file_len(k)).sum();
        // 单笔归档的写 + 读（重放路径的真实成本）。
        let t = Instant::now();
        for _ in 0..rounds {
            store.put(&keys[0], &withdrawal_rec(0)).unwrap();
        }
        let one_write = t.elapsed() / rounds;
        let t = Instant::now();
        for _ in 0..rounds {
            store.get(&keys[0]).unwrap().expect("archived");
        }
        let one_read = t.elapsed() / rounds;

        println!(
            "{:>7} | file {:>7.2} MB save {:>7.1} ms load {:>7.1} ms | file {:>6.2} MB save {:>6.1} ms load {:>5.1} ms | {:>7.2} MB 单笔写 {:>5.2} ms 读 {:>5.2} ms",
            n,
            legacy_bytes as f64 / 1e6,
            legacy_save.as_secs_f64() * 1e3,
            legacy_load.as_secs_f64() * 1e3,
            hot_bytes as f64 / 1e6,
            hot_save.as_secs_f64() * 1e3,
            hot_load.as_secs_f64() * 1e3,
            archive_bytes as f64 / 1e6,
            one_write.as_secs_f64() * 1e3,
            one_read.as_secs_f64() * 1e3,
        );
        println!(
            "{:>7} | 每条 {:>6.1} us | 每条 {:>6.1} us（热账本与提现笔数解耦，只剩 seal+finalized） | 归档 {:.0} B/笔（hex 19472 B）",
            "",
            legacy_save.as_secs_f64() * 1e6 / n as f64,
            hot_save.as_secs_f64() * 1e6 / n as f64,
            archive_bytes as f64 / n as f64,
        );
        let _ = std::fs::remove_dir_all(&legacy_dir);
    }

    println!();
    println!("=== 写放大外推（一次状态变更 = 一次全量重写）===");
    let per_rec = 19_500f64; // 实测 ≈19.5 KB/笔（hex）
    for n in [1_000usize, 10_000, 100_000] {
        let bytes_at_n = per_rec * n as f64;
        let total_written = per_rec * (n as f64) * (n as f64) / 2.0;
        println!(
            "  改前 N={n:<7} 单文件≈{:>8.1} MB   累计写入≈{:>9.1} GB   ≥{:.0} s 纯 I/O@500MB/s",
            bytes_at_n / 1e6,
            total_written / 1e9,
            total_written / 5e8
        );
    }
    println!(
        "  改后：热账本 ~360 B/条（seal）+ 327 B/条（finalized），与提现笔数**解耦**；\
         归档 6 KB/笔**只写一次**（裁剪后 ~400 B）"
    );
}

/// 冷层归档本身：压缩比、裁剪前后的体积。
#[test]
fn perf_build_archive_footprint() {
    println!();
    println!("=== 提现构建 archived（hex → deflate+b64）===");
    let dir = std::env::temp_dir().join(format!("perf-archive-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    let store = BuildStore::new(dir.clone());
    let rec = withdrawal_rec(0);
    let key = rgb_sidecar::engine::seal_set_key_of_strs(&rec.input_outpoints);
    let hex_bytes =
        (rec.psbt_hex.len() + rec.consignment_hex.len() + rec.fascia_hex.len()) as u64;
    // 取 20 次平均：单次 put 里 fsync/rename 的抖动很大（本机实测 0.2–15 ms），一次采样说明不了问题。
    const ROUNDS: u32 = 20;
    let t = Instant::now();
    for _ in 0..ROUNDS {
        store.put(&key, &rec).unwrap();
    }
    let write = t.elapsed() / ROUNDS;
    let t = Instant::now();
    let mut back = store.get(&key).unwrap().unwrap();
    for _ in 0..ROUNDS {
        back = store.get(&key).unwrap().unwrap();
    }
    let read = t.elapsed() / ROUNDS;
    assert_eq!(back.consignment_hex, rec.consignment_hex, "归档必须逐字节可逆");
    let archived = store.file_len(&key).unwrap();
    println!(
        "  1 笔：热账本内联（改前）{:>6} B → 归档 {:>5} B（{:.1}× 小）| 写 {:>5.2} ms 读 {:>5.2} ms",
        hex_bytes,
        archived,
        hex_bytes as f64 / archived as f64,
        write.as_secs_f64() * 1e3,
        read.as_secs_f64() * 1e3,
    );
    for n in [1_000usize, 10_000] {
        let (_, store, keys) = new_shape(n);
        let total: u64 = keys.iter().filter_map(|k| store.file_len(k)).sum();
        // 裁成墓碑（已 finalize + 已确认）。
        for k in &keys {
            store
                .prune(
                    k,
                    rgb_sidecar::build_store::Pruned {
                        btc_height: 900,
                        confirm_height: 700,
                        reason: "bench".into(),
                    },
                )
                .unwrap();
        }
        let pruned: u64 = keys.iter().filter_map(|k| store.file_len(k)).sum();
        println!(
            "  W={n:<7} 归档总计 {:>6.1} MB（全量）→ {:>5.1} MB（裁剪后墓碑）| 改前热账本内联 {:>6.1} MB",
            total as f64 / 1e6,
            pruned as f64 / 1e6,
            hex_bytes as f64 * n as f64 / 1e6
        );
        let _ = std::fs::remove_dir_all(store.dir());
    }
    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn perf_user_script_registry_write_amplification() {
    println!();
    println!("=== BtcWallet 注册表：每次插入重写整个 JSON 文件（O5，本文件只记录基线）===");
    let rpc = Arc::new(
        BtcdRpc::connect("127.0.0.1:1", "u", "p", None, bitcoin::Network::Regtest).unwrap(),
    );
    let pubkey_hex = "02c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5";
    for n in [100usize, 1_000, 5_000] {
        let dir = std::env::temp_dir().join(format!("perf-uscripts-{n}"));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("user_scripts.json");
        let mut w = BtcWallet::new(rpc.clone(), pubkey_hex, bitcoin::Network::Regtest)
            .unwrap()
            .with_user_scripts_path(path.clone());
        let pubkey: bitcoin::CompressedPublicKey = pubkey_hex.parse().unwrap();

        let mut per_insert = Vec::with_capacity(n);
        let t_all = Instant::now();
        for i in 0..n {
            let user_id = format!("1PerfUser{i:0>24}");
            let ws = deposit_witness_script(&user_id, &pubkey).unwrap();
            let pk = bitcoin::ScriptBuf::new_p2wsh(&ws.wscript_hash());
            let t = Instant::now();
            w.register_user_script(pk, user_id, ws).unwrap();
            per_insert.push(t.elapsed().as_secs_f64() * 1e3);
        }
        let total = t_all.elapsed();
        let size = std::fs::metadata(&path).unwrap().len();
        let first100: f64 = per_insert.iter().take(100).sum::<f64>() / 100.0;
        let last100: f64 = per_insert.iter().rev().take(100).sum::<f64>() / 100.0;
        println!(
            "  n={n:<6} 总耗时 {:>8.1} ms | 首100条均值 {:>6.3} ms → 末100条均值 {:>6.3} ms | 文件 {:>6.1} KB",
            total.as_secs_f64() * 1e3,
            first100,
            last100,
            size as f64 / 1e3
        );
        let _ = std::fs::remove_dir_all(&dir);
    }
}
