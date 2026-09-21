# P2WSH 多地址并发充值：性能指标与基准

> **这份文档是什么**：P2WSH 多地址充值方案的性能口径基线——测了哪些维度、判据是什么、实测数字
> 是多少、哪条路径是真瓶颈、哪些还没测。用于① 容量规划与拐点判断；② 后续优化（#52）的回归尺子。
>
> **数据来源与口径（重要）**：本文**所有数字转录自 #48 评估报告**
> `~/.claude/plans/p2wsh-multi-address-perf.md`（41 KB，2026-09-17 产出），**本文没有重新测量任何数字**。
> 报告原生标注被保留：
> - `[实测]` = 该次评估真实跑出来的（局部基准 / 容器内只读采样）；
> - `[推导]` = 由实测数据 + 代码结构推出来的，**不是直接测的**；
> - `[日志]` = 从既有 E2E 日志里读出来的。
>
> 凡报告没测的，本文写「**未实测**」或「**未记录**」，**不补数字、不外推充数**。
>
> **基准文件**（两份，探针性质，不进产品调用路径）：
> - Go：`perf_p2wsh_bench_test.go`（本目录，10 个 `Benchmark*`）
> - Rust：`rgb-sidecar/benches/perf_p2wsh.rs`（3 个 `#[test]` 形式的 perf 用例）

---

## 0. 一分钟结论

1. **单机天花板 ≈ 1e4 量级**，卡在两处**性质完全不同**的地方：
   - **充值识别（BTC 侧）**：上游 neutrino 在「过滤器命中的块」上跑 **O(输出数 × watch 集)**，
     实测 **M=1e4 → 3.78 s/命中块；M=1e5 → 39.5 s/命中块**。这是**单核串行 CPU**，
     纵向加机器基本无效（已吃满一核），横向也得先改代码。
   - **入账/账本（侧车）**：`ledger.json` 每笔提现记录 **19.5 KB**，而 `Ledger::save` 每次变更
     **重写整个文件 + fsync**，且在**全局 `Mutex<RgbEngine>` 内** ⇒ 它同时是「侧车所有 RPC 的总吞吐上限」。
2. **这套设计里「几笔充值」从来不是并发问题，「多少用户 / 多少历史笔数」才是。**
   所有成本随**存量规模**涨，不随瞬时并发涨：watch 集**只增不减**（上游 `Rescan.Update` 只有
   `AddAddrs`、没有 remove；`chain.NeutrinoClient.scanning` 又是粘性 `true`），账本没有裁剪/分层。
3. **本仓自己的数据结构是对的**：`depositScriptSet` 是双向 map，反查 **O(1)** 且实测与 N 无关
   （127–139 ns）。**贵的是上游没有用同类结构**——所以优化要么 patch/vendor 上游，要么绕开上游。
4. **默认配置上限与实测拐点重合**：`maxWatchedDepositScripts` 默认 **1e4**
   （`deposit_address.go:49`），而实测 M=1e4 时命中块已经要 3.78 s ⇒ 这个默认值是「现在还能跑」的
   边界，不是余量。

---

## 1. 指标体系（测了哪些维度 + 各自的判据）

九个维度，按「一笔充值从 BTC 上链到 XBTC 入账」的路径顺序：

| # | 维度 | 判据（怎么算好/坏） | 对应基准 |
|---|---|---|---|
| 1 | watch 集本身的增删查（本仓 `depositScriptSet`） | 反查应是 **O(1)** 且不随 N 变；用线性扫做对照，证明「贵不在本仓」 | `BenchmarkDepositScriptSetLookup` |
| 2 | 每地址**派生**本身（纯函数） | 单价恒定、与规模无关；用来证明派生**不是**瓶颈 | `BenchmarkDeriveDepositPkScript` |
| 3 | 批量 import N 个脚本（waddrmgr 写库 + 加密 + 订阅） | 关键问「**每脚本边际成本是否恒定**」（有没有隐藏 O(N²)）；必须把**固定开销**与**边际成本**分开看 | `BenchmarkImportDepositWitnessScripts` |
| 4 | 发放一个新地址的**全路径** | 单次端到端耗时 → **地址发放 QPS 上限 = 1/耗时** | `BenchmarkEnsureUserDepositScript` |
| 5 | 上游 neutrino **每命中块**的匹配（复刻） | 随 watch 集 M 的增长曲线；与哈希表版的**倍率**（判「硬件能否救」） | `BenchmarkNeutrinoPaysWatchedAddr` / `...Map` |
| 6 | 上游 `spendsWatchedInput`（复刻） | 随 `watchInputs` 的曲线；注意该集合**进程内只增不减** ⇒ 曲线还随**运行时长**变差 | `BenchmarkNeutrinoSpendsWatchedInput` |
| 7 | GCS filter 匹配（每块都跑） | 每块 **O(M log M + N_filter)**，不是常数；判「相对块间隔 600 s 是否可忽略」 | `BenchmarkGCSFilterMatchAny` |
| 8 | 跨节点 watch 集下发/合并（每 30 s 一轮，所有节点） | 稳态（只有去重判断）vs 冷启动（每条要派生 + 落库）**分开量**；判是否成为常态开销 | `BenchmarkSyncDepositScriptsRound` |
| 9 | 内层循环**单价**（每个 (输出, watch 地址) pair） | 用于把「命中块总耗时」拆成单价 × pair 数，方便外推到别的块形状 | `BenchmarkPayToAddrScript` |

**没进这份基准的维度（见 §5）**：充值入账（`commitDepositTx` 那一段）的端到端时延与吞吐、
链上执行器侧的派生校验、多节点下发的**网络**成本、以及提现/扫集侧的 **seal 查询**
（`CheckStickySeal` @ `tss.go:1729`、`btcwallet.listUnspent` @ `btcwallet.go:829`）——
这些在**提现/扫集**路径上，不在充值识别路径上，本基准文件不覆盖。

---

## 2. 实测数据与测试条件

### 2.1 测试条件（必须连着数字一起引用）

| 项 | 值 |
|---|---|
| 跑法（Go） | `go test ./plugin/dapp/lightclient/rpc/lightclient/neutrino/ -run '^$' -bench '<name>' -benchtime=...` |
| 跑在哪（Go） | **干净 worktree** `/private/tmp/worktrees/plugin-p2wsh-perf`（detached HEAD = `049eb9a66`）。**原因**：评估当时主工作树编译不过（另一个 agent 的 CGGMP WIP 引了尚不存在的 `chain33/system/crypto/tss/cggmp`） |
| 跑法（Rust） | `cargo test --release --bench perf_p2wsh -- --nocapture --test-threads=1`（**必须单线程**：三个用例并行会互抢 fsync，本机实测能差 2–5×，那样数字是噪声不是结论） |
| 机器 | macOS 本机（Docker Desktop 环境当时正跑着 E2E） |
| 规模 | watch 集 M / 过滤器条目数 / 提现笔数 W = 1e2 ~ 1e5 若干档；命中块按 **2000 个输出**建模（≈ 满块量级） |

**口径警告（照抄报告）**：§2.5 的 btcd RPC 延迟（3.4–6.7 ms）是 **macOS + Docker Desktop + E2E 并发**
下测的，生产 Linux 同网应低一个量级。**只用于横向比较，不用于绝对容量规划。**

### 2.2 Go 桥侧（neutrino 包）

| 项目 | 数字 | 来源 |
|---|---|---|
| `depositScriptSet.lookupUser`（map 反查） | **127–139 ns**，N=1e2…1e5 **不变** | [实测] |
| 对照：同一件事线性扫 `entries` | N=1e3: 996 ns；1e4: 19.6 µs；**1e5: 619 µs** | [实测] |
| `DeriveDepositPkScript`（纯派生） | **20.7 µs/次**（大头是 `btcec.ParsePubKey`） | [实测] |
| `importDepositWitnessScripts` **固定开销** | **~10.5 ms/次调用**（bolt RW 提交 + scope 取用） | [实测] |
| 每脚本**边际成本** | **~16 µs/脚本** | [实测] |
| 批量 import N=1 / 10 / 100 / 1000 | **10.6 / 11.0 / 12.2 / 26.8 ms** | [实测] |
| `ensureUserDepositScript`（发放一个地址全路径） | **15.9 ms** ⇒ **~63 地址/秒**（单线程） | [实测] |
| `mergeRemoteDepositScripts` **冷启动** 1e3 / 1e4 | 41.6 ms / **422 ms** | [实测] |
| `mergeRemoteDepositScripts` **稳态** 1e3 / 1e4 | 65.7 µs / 709 µs | [实测] |
| 下发侧组装请求 1e3 / 1e4 | 164 µs / 1.6 ms（不含网络；1e4 条 ≈ **680 KB** 负载） | [实测] |

**读法（这几条决定了优化方向）**：

- **固定 10.5 ms vs 边际 16 µs 差 ~600×** ⇒ 「N 个用户」必须**批量 import**
  （N 个用户分散发放 = N × 10.5 ms，1e4 用户 = **105 s**；一次批量 = 10.5 ms + 1e4×16 µs = **170 ms**）。
  **当前代码已经是批量**（`importDepositWitnessScripts` 收 `[][]byte`，`mergeRemoteDepositScripts` /
  `loadDepositScripts` 都走批量），这一点是对的、不要改回去。
- 单用户按需发放 **15.9 ms/个** = **两次 bolt 提交**（import 一次 + `saveDepositScript` 一次）
  ⇒ 这是地址发放的 QPS 上限，也是 O6 的收益来源。
- **稳态下发不是问题**（1e4 条合并 709 µs + 组装 1.6 ms，每 30 s 一轮）；**冷启动才是**（422 ms）。

### 2.3 上游 neutrino：每块成本（**这是头号瓶颈**）

「每块」要分两个阶段量，曲线完全不同：

**(a) 过滤器匹配（每个新块都跑）** — `rescanState.notifyBlock` → `matchBlockFilter` →
`gcs.Filter.MatchAny(key, ro.watchList)`（`neutrino@v0.16.0/rescan.go:1157`）：
每块 **O(M log M + N_filter)**，**不是常数**。
实测 filterN=3000：M=1e3 / 1e4 / 1e5 → **0.25 / 0.89 / 6.62 ms/块** [实测]。
⇒ 块间隔 600 s，**这部分可忽略**。

**(b) 命中块的处理（过滤器命中才跑）** — `extractBlockMatches` 里的
`rescanOptions.paysWatchedAddr`（`neutrino@v0.16.0/rescan.go:1303`）：**O(输出数 × watch 集)**，
**没有任何索引**，每个 pair 重算一次 `txscript.PayToAddrScript`，且**命中后不 break**（内层跑满）。

实测（复刻该循环，2000 输出）[实测]：

| watch 集 M | 每命中块耗时 | 对照：同样工作用哈希表 | 倍率 |
|---|---|---|---|
| 100 | 46.8 ms | 81 µs | ~580× |
| 1,000 | 393 ms | 85 µs | ~4,600× |
| **10,000** | **3.78 s** | 95 µs | **~40,000×** |
| **100,000** | **39.5 s** | 104 µs | **~380,000×** |

配套的 `spendsWatchedInput`（`rescan.go:1273`）是 **O(输入数 × watchInputs)**，而 `watchInputs`
**每命中一个输出就 append 一条、进程内永不回收** ⇒ 曲线随时间单调变差。
实测（10 输入）：watchInputs=1e3 / 1e4 / 1e5 → **36 µs / 0.36 ms / 3.6 ms** [实测]。

> **这条是本文最重要的数字**：M=1e4 时单块 3.78 s 是**单核串行**、且期间所有块通知排队。
> 纵向扩展救不了（已吃满一核），横向也得先改代码。修法见 §4 的 O2。

### 2.4 GCS filter 匹配（真库）

`gcs.Filter.MatchAny`（`btcutil@v1.1.5/gcs/gcs.go:344`）按 `len(data) >= f.N()/2` 二选一：
`ZipMatchAny`（对 M 个查询项各做 siphash + **排序** O(M log M)）或
`HashMatchAny`（先把过滤器 N 个值建 map O(N)，再逐个查表）。

已记录数字：**filterN=3000，M=1e3 / 1e4 / 1e5 → 0.25 / 0.89 / 6.62 ms/块** [实测]。
基准本身扫的是 3×4 矩阵（filterN=1000/3000/10000 × watch=100/1e3/1e4/1e5），
**其余 9 格报告未记录**（见 §5）。

### 2.5 侧车（Rust）：RGB20 充值路径的吞吐上限

> 这一段决定的是**带 RGB20 的充值**（第 ④ 段）上限，不是纯 BTC/XBTC 充值。
> `rgb-sidecar/**` 正在被另一条线改动，**数字可能已漂移**；以 `rgb-sidecar/benches/perf_p2wsh.rs`
> 的现跑为准，下表是 #48 评估当时的记录。

| 项目 | 数字 | 来源 |
|---|---|---|
| `Ledger::save` W=1e2/1e3/1e4/5e4 | 文件 2.0/20.4/**203.9**/1019 MB → **8.6 / 27.2 / 225.5 / 1587.8 ms** | [实测] |
| `Ledger::load` 同上 | **1.2 / 12.4 / 122.8 / 872.2 ms** | [实测] |
| 真实 `ledger.json`（一次 E2E 后） | **112,352 B**（1 asset + 6 seals + 2 receives + 4 finalized + 5 builds） | [实测]（容器内只读） |
| 一条 `withdrawal_build` | psbt 1,994 + consignment 15,926~16,976 + fascia 1,552 字符 ≈ **19.5 KB**（consignment 占 82%） | [实测] |
| `genesis_consignment_hex` | 11,630 字符 | [实测] |
| 注册表 `register_user_script`（每次重写整个 JSON） | n=1e2: 0.299 ms/条均；n=1e3: 首100 0.569→末100 1.837 ms，总 0.98 s；**n=5e3: 首100 0.162→末100 6.692 ms，总 19.3 s** | [实测] |
| 注册表文件大小 | 1e2: 32.5 KB；1e3: 325 KB；5e3: 1.6 MB（≈325 B/条） | [实测] |
| 侧车 `list_unspent_*` 逐脚本 O(N) RPC | N 个脚本 ⇒ (N+1) × (`searchrawtransactions` + `getblockcount` + k × `gettxout`)；`get_block_count()` **每脚本调一次**（纯冗余） | [代码推导] |
| ⇒ 一次 `build_sweep` 的 RPC 数 | N=1e2/1e3/1e4 → ~300 / ~3,000 / **~30,000** | [推导] |

**拟合与拐点**：`save_ms ≈ 1.1 × file_MB`、`file_MB ≈ 0.0195 × W` ⇒ **`save_ms ≈ 0.021 × W`**。
⇒ W=1e4 时侧车最大状态变更速率 **~4.6 次/秒**，W=1e5 时 **~0.47 次/秒**；且它在全局锁内，
所以**同时是所有侧车 RPC 的总吞吐上限**。

**扫集功能级失效（关键结论）**：N=1e4 时一次 `build_sweep` ≈ 30,000 RPC。
本机口径（4.2–6.7 ms/RPC）**≈144 s > Go 侧 `sweepOnce` 的 2 分钟 ctx 超时**
（`sweep.go:95`）⇒ **N≈1e4 时扫集必然超时**，而且这 144 s 全程占着侧车全局锁。
报告另给了生产口径（按 0.5 ms/RPC 估）**≈15 s**——**仍接近但不到超时**，所以这条的严重性
**依赖部署形态**，属「需要实测确认」（见 §5）。

**后续进展（与上表不是同一次测量，勿混用）**：O3（账本分层 + 归档压缩）**已落地**（`e8d24558b`），
报告口径：**1e4 规模 save 384.4→22.0 ms（17×）、文件 208→8.65 MB（24×）**；
**5e4 规模 save 1586.7→74.0 ms（21×）、load 2543.6→88.9 ms（29×）**；侧车状态变更速率
**~2.6/s → ~45/s**。来源：`reactive-brewing-sedgewick.md` #O3 段落。
> 注意：1e4 规模下 #48 报告记 save **225.5 ms**、O3 段落记 save **384.4 ms**（文件体积两者一致：
> 204 vs 208 MB）⇒ 同机同形状的两次独立测量差 ~1.7×，**未归因**（Rust 基准自己的注释也标注单次
> fsync 抖动可达 0.2–15 ms）。引用时按**区间 225–384 ms** 计，别当两个不同规模。
> **仍未解**：热账本随 seal + finalized 线性增长（**686 B/笔**，W=1e5 ≈ 70 MB ⇒ save 回到百 ms 量级）。

### 2.6 E2E 真实链路时延（**watch 集 = 1 个脚本，regtest**）

| 场景 | 数字 | 来源 |
|---|---|---|
| BTC 广播 → XBTC 入账 | 15:33:53 → 15:34:02 = **~9 s** | [日志] `/tmp/rgbx-c4-e2e.log:392-393` |
| 充值广播 → 扫集上链 | 15:34:03 → 15:34:31 = **~28 s**（CI 把 `sweep.intervalSeconds` 压到 3） | [日志] `/tmp/rgbx-c4-e2e.log:396-397` |

> **这两条不能外推到 N 个用户**——它们是 watch 集 = 1 的单脚本数字。N 之后的曲线见 §2.2 / §2.3。

---

## 3. 基准 ↔ 代码路径对照

行号：Go 侧为本文写作时的当前树；上游为 `go.mod` 锁定的版本
（`neutrino v0.16.0`、`btcwallet v0.16.17`、`btcutil v1.1.5`）。

| # | 基准（`perf_p2wsh_bench_test.go`） | 测什么 | 对应代码路径 |
|---|---|---|---|
| 1 | `BenchmarkDepositScriptSetLookup:48` | watch 集反查归属：map（现状）vs 线性扫（对照） | `deposit_address.go:81` `lookupUser`（map `byProgram`：`deposit_address.go:65`）；对照走 `deposit_address.go:131` `entries` |
| 2 | `BenchmarkImportDepositWitnessScripts:124` | 批量 import N 个脚本（waddrmgr 写库 + 加密 + 订阅）；问「每脚本成本是否恒定」 | `deposit_address.go:266` `importDepositWitnessScripts` → 一个 `walletdb.Update` 里逐条 `ScopedKeyManager.ImportWitnessScript` |
| 3 | `BenchmarkEnsureUserDepositScript:161` | 发放一个新地址的**全路径** = 地址发放 QPS 上限的倒数 | `deposit_address.go:178` `ensureUserDepositScript` → import（`:266`）→ `notifyAddrs`/`NotifyReceived` → `deposit_address.go:538` `saveDepositScript`（第二次 bolt 提交） |
| 4 | `BenchmarkDeriveDepositPkScript:175` | 纯派生单价（证明派生不是瓶颈） | `rgbx/types/p2wsh_deposit.go:130` `DeriveDepositPkScript`（同文件 `:107` 是 `DeriveDepositWitnessScript`） |
| 5 | `BenchmarkNeutrinoPaysWatchedAddr:194` | **上游命中块匹配的逐字复刻**：每输出 × 每 watch 地址重算 `PayToAddrScript`，命中不 break | `neutrino@v0.16.0/rescan.go:1303` `rescanOptions.paysWatchedAddr`（上游函数未导出 ⇒ 只能复刻循环结构，被调函数是真库） |
| 6 | `BenchmarkNeutrinoPaysWatchedAddrMap:233` | 同一件事用哈希表做的对照（**本仓已有形态**） | 对照 `depositScriptSet.byProgram`（`deposit_address.go:65`）的查表语义 |
| 7 | `BenchmarkNeutrinoSpendsWatchedInput:261` | 上游 `spendsWatchedInput` 复刻：O(输入数 × watched inputs)，且该集合**只增不减** | `neutrino@v0.16.0/rescan.go:1273` `rescanOptions.spendsWatchedInput`（`watchInputs` 在 `paysWatchedAddr` 命中时 append） |
| 8 | `BenchmarkGCSFilterMatchAny:294` | 真 `gcs.Filter.MatchAny`，每块一次 | `btcutil@v1.1.5/gcs/gcs.go:344` `MatchAny`（分派到 `:362` `ZipMatchAny` / `:451` `HashMatchAny`）；调用点 `neutrino@v0.16.0/rescan.go:1157` `matchBlockFilter` |
| 9 | `BenchmarkPayToAddrScript:352` | 上游内层循环的**单价**（每个 (输出, watch 地址) pair） | 同 #5 的内层一行 `txscript.PayToAddrScript` |
| 10 | `BenchmarkSyncDepositScriptsRound:372` | 每 30 s 一轮的 watch 集下发/合并：`cold` / `steady` / `push` 三段分开量 | `deposit_address.go:433` `depositScriptSyncWorker` → `:410` `syncDepositScriptsWithSidecar` → `:468` `mergeRemoteDepositScripts`；下发侧走 `:131` `entries()` + 组请求 |

辅助（非基准）：`benchNewWallet:85`（建 watching-only 钱包 + neutrino db 的基准夹具）、
`chain33Addr:331`（造**合法** chain33 base58 地址——随便造的串会被 `address.CheckAddress` 跳过、
测不到真实成本）、`randPkScript:341`（造形态正确的 P2WSH pkScript）。

**上游复刻的可信边界**：`paysWatchedAddr` / `spendsWatchedInput` 是包内私有函数，**复刻的只有循环结构**，
被调用的 `txscript.PayToAddrScript` / `bytes.Equal` 都是真库真函数。
误差来源：假设「命中块有 2000 个输出」（真实块输出数会变）；复刻里没做上游命中后的
`watchInputs` append（那只是一个 append，量级不影响）。

---

## 4. 瓶颈排序与优化方向

### 4.1 排序（按 ROI）

| # | 瓶颈 | 量级 | 为什么 ROI 最高 |
|---|---|---|---|
| **1** | 上游 neutrino `paysWatchedAddr` 的 **O(输出 × watch 集)** | **3.78 s/命中块 @M=1e4；39.5 s @M=1e5** [实测] | 唯一一个 **10⁴~10⁵ 倍**的差距；单核串行，**硬件救不了** |
| **2** | **watch 集只增不减（无回收路径）** | 它是 #1 的**乘数**；上游 `Update` 只有 `AddAddrs`、无 remove，`scanning` 粘性 true ⇒ 进程内无法收缩 | 不清这个 #1 迟早踩线；清了 #1 的爆炸半径小一个量级 |
| **3** | 侧车账本**全量重写**（19.5 KB/笔，全局锁内） | W=1e4：204 MB / **225 ms/次** / **~4.6 次·秒⁻¹**；W=5e4：1.0 GB / 1.59 s | 直接决定侧车 RPC 总吞吐；纵向只线性缓解 |
| **4** | 侧车 `list_unspent_*` 逐脚本 **O(N) RPC** | N=1e4 → 一次 `build_sweep` ~30,000 RPC **≈144 s > Go 侧 2 分钟超时** [推导] | **功能级失效**（1e4 脚本下扫集永远超时），不是变慢 |
| **5** | 侧车注册表每次重写整个 JSON | n=5e3：末条 **6.7 ms**、总 19.3 s；1e4 用户 ≈78 s [实测] | 修复成本最低，但只在冷启动/大批量发放时痛 |
| 6 | Go 桥发放地址 15.9 ms/个（两次 bolt 提交） | ~**63 个/秒** [实测] | 1e5 用户冷启动 26 分钟；可合并成一次提交 |
| 7 | 充值入账串行 + 每笔重复下载整块 | [推导] ~20–30 笔/秒；`depositWatcher` 单 goroutine | 短期够用；块级缓存是低垂果实 |
| 8 | GCS filter 每块 O(M log M) | M=1e4 → 0.89 ms/块；1e5 → 6.6 ms/块 [实测] | 相对可忽略（块间隔 600 s） |

### 4.2 优化项与**当前状态**

| 项 | 做法（一句话） | 收益 | 状态 |
|---|---|---|---|
| **O1** watch 集回收 | 只 watch「已发放且未归零」的地址；归零即从持久化集合 + 内存集移除，配合重启重建（**上游无 remove，进程内缩不了**） | 把 M 从「历史用户数」降回「活跃用户数」 | **未做**（#52） |
| **O2** 命中块匹配 O(输出×M) → O(输出) | (a) 上游打补丁：`ro.watchAddrs` 换 map 结构；(b) 绕开上游：本仓自己消费区块做 program 匹配（`depositScriptSet` 已是 O(1)） | (a) M=1e4：**3.78 s → ~0.1 ms（~4×10⁴ 倍）** [实测对照] | **未做**（#52，报告列为**最该先做**） |
| **O3** 侧车账本分层 + 压缩 + 裁剪 | `withdrawal_builds` 拆成每笔一文件（`builds/<sha256(seal集合)>.json`）+ 大字段压缩 + 已 finalize 且确认 ≥144 块才裁（留墓碑）+ 注册表 append | 1e4 save **384.4→22.0 ms**、文件 **208→8.65 MB**；状态变更速率 **2.6/s → ~45/s** | **已完成** `e8d24558b` |
| **O4** 侧车 UTXO 发现改「逐块扫输出 + 本地索引」 | 主路径读块做 program 匹配（O(1)/块），`searchrawtransactions` 只用于新注册地址的**限定窗口回填**；顺手把 `get_block_count()` 提出循环 | 一次 `build_sweep` **~30,000 RPC（144 s）→ ~0**；全局锁占用分钟级→毫秒级 | **未做**（#52） |
| **O5** 侧车注册表改增量写 | `user_scripts/` 每用户一个文件（或 append-only 日志），替代每次全量 `serde_json::to_vec_pretty` | n=5e3 注册 **19.3 s → ~0.1 s**（O(N²)→O(1)） | **未做**（#52，最便宜） |
| **O6** Go 桥地址发放合并写 | `importDepositWitnessScripts` + `saveDepositScript` 合成**一次** bolt 提交 | 15.9 ms → 预计 ~8–10 ms/个（1.6–2×） | **未做**（#52） |
| **O7** 充值入账：块级缓存 + 并发化 | 按 blockHash 缓存整块；`depositWatcher` 改小并发池 | [推导] ~20–30 → 100+ 笔/秒 | **未做**（#52） |
| **O8** 禁 native P2WSH 作提现目标 | 既有计划项，**非性能**（资金安全） | — | 提醒：**别因为性能排期把它往后挤** |

**排期建议（报告结论，主会话认同）**：先 **O2 + O1**（唯一 10⁴~10⁵ 倍差距、硬件救不了）→
再 **O3**（已做）→ O4 → O5 → O6/O7。**排在 CGGMP 之后**（CGGMP 在改主树，且基准文件需在能编译的树上才能提交）。
来源：`reactive-brewing-sedgewick.md:352`（#52 推迟到系统开发完成后，顺序 O1+O2 → O4 → O5 → O6/O7）。

### 4.3 硬件扩展拐点

| 维度 | 拐点 | 依据 |
|---|---|---|
| 提现累积笔数 W | **~1e4**（侧车状态变更 ~4.6 次/秒）→ 1e5 时 0.47 次/秒（不可用） | [实测] `save_ms ≈ 0.021 × W` |
| watch 集 M | **~1e4 脚本**（命中块 3.78 s）→ 1e5 脚本 39.5 s/命中块 | [实测] |
| 地址发放突刺 | **~63 个/秒**（1e5 用户 = 26 min 冷启动） | [实测] |
| BTC 充值入账 | **~20–30 笔/秒**（单 goroutine） | [推导] |
| 扫集 | **~1e3 脚本**可用（14 s/次）；**1e4 脚本超时**（144 s > 2 min） | [推导] |

**关键判断**：这三个拐点里**只有「提现笔数」能靠换硬件线性推后**（更快的 SSD → 保存更快，
但仍是 O(N) 全量重写，只是常数变小）。**watch 集那条是单核 CPU 的 O(outputs × M)，纵向几乎无收益、
横向也没法拆（一个 btcwallet 实例一个 rescan）⇒ 到拐点必须改代码，不是加机器。**
纵向只能把拐点从 1e4 推到 3–5e4；横向分片（watch 集按 userID 哈希分桶到 K 个监听节点）能**直接线性拆掉**
瓶颈 #1，但会把复杂度转移到「扫集/提现跨桶」——**只有到 1e5~1e6 量级才值得**。

### 4.4 要补的指标（可回归）

本仓 neutrino/rgb20 与侧车**都没有 prometheus 使用先例**；建议先**结构化日志 + 时长字段**
（与现有 `log.Info("sweepOnce swept ...", "inputs", ...)` 同风格，零依赖），需要 Grafana 时再引 prometheus。
核心 5 条：

| 指标 | 定义 | 落点 |
|---|---|---|
| `deposit_watch_set_size` | 当前 watch 集条目数（已有日志 `watchSetSize`，**升级为指标**） | `deposit_address.go` |
| `deposit_address_issue_seconds` | 发放一个地址的耗时 | `ensureUserDepositScript:178` |
| `block_matched_seconds` | **命中块**处理耗时（瓶颈 #1 的直接观测） | 需包一层 `chain.NeutrinoClient` 或改上游 |
| `deposit_credit_latency_seconds` | BTC 块时间/首次看见 → chain33 上账成功（**p50/p99**） | `handleTransaction` + `commitDepositTx` 各打一点，按 txid 关联 |
| `sweep_build_seconds{watch_set_bucket}` / `sidecar_ledger_save_seconds` | 扫集构建耗时与输入数 / 侧车 save 耗时 | `sweepOnce`、侧车 `engine.rs` 的 `save()` |

**回归守门信号（便宜且灵敏）**：`BenchmarkNeutrinoPaysWatchedAddr` 的复刻若被优化掉，
会立刻从 **3.78 s 掉到 ~0.1 ms** —— 是个很好的「O2 到底做没做」的二进制判据。
建议不进主 CI（太慢），放进 `make perf-p2wsh` 按需/发版前跑，与本文基线对比。

---

## 5. 未实测 / 未记录 / 外推（缺口清单）

**照实说，别把这些当已验证。**

1. **没跑 E2E harness**（评估当时的约束）。§2.6 两条端到端时延来自既有日志，且 **watch 集 = 1**，
   **不能外推到 N 个用户**。
2. **「上游 O(输出 × M)」是复刻实测，不是对真 neutrino 的实测**。复刻了循环结构 + 真库函数；
   误差：假设命中块 2000 输出、未复刻 `watchInputs` 的 append。**建议补**：在多 watch 集规模下
   跑一次真 E2E（先灌 1e2/1e3 个假地址再走一笔真实充值），否则永远测不到瓶颈 #1。
3. **btcd RPC 延迟是 macOS + Docker Desktop + E2E 并发下测的**（3.4–6.7 ms/次），比生产 Linux 同网高
   约一个量级。§2.5 的扫集超时结论报告**同时给了两个口径**（本机 144 s / 生产 ~15 s）——
   **严重性依赖部署形态，需实测确认**。
4. **充值入账 ~20–30 笔/秒是推导，未实测**（没测 `submitMainChainTx` 的真实时延，
   它含 chain33 的 `getProperFeeRate` 查询 + 签名 + gRPC 发送）。
5. **「上游 `scanning` 粘性 true」是代码推导，无运行时验证**：
   `grep "scanning = false"` 在 `btcwallet@v0.16.17/chain/neutrino.go` 无命中（只有 `:420` / `:524`
   两处 `= true`），但**没有用运行时日志验证「第二次 NotifyReceived 不触发 rescan」**。
   这条**如果错了，§2.2 的成本模型要整体乘 N**。
6. **「不回放历史」也是代码推导，无运行时验证**：`NotifyReceived` 不传 `StartBlock`
   ⇒ `neutrino@v0.16.0/rescan.go:392-397` 取 `chain.BestBlock()`（当前链尖）**向前**扫。
   这**可能是个真 bug**（也影响恢复/备份场景），建议单独验证（见 §6）。
7. **没测链上执行器侧**（`checkDeposit` 的派生校验、`ValidateWithdrawPsbt`）：每笔一次 O(1) 派生
   （20.7 µs），相对可忽略，**无单独基准**。
8. **没测多节点下发的网络成本**：`syncDepositScriptsWithSidecar` 的 1e4 条 ≈ 680 KB / 30 s / 节点
   只测了组装与合并的 CPU 部分，**网络未测**。
9. **`BenchmarkPayToAddrScript` 与 `BenchmarkGCSFilterMatchAny` 的多数格子报告未记录**：
   前者的单价数字 #48 报告里没有（只有总耗时）；后者的 3×4 矩阵只记了 `filterN=3000` 一行三格，
   **其余 9 格未记录**。基准已就位，需要时按 §7 命令补跑即可（本机负载敏感，别随手全跑）。
10. **`BenchmarkSyncDepositScriptsRound` 的 `steady`/`push` 两段在报告里只有 1e4 一个规模有数**
    （709 µs / 1.6 ms），1e2/1e3 未记录。
11. **提现/扫集侧的 seal 查询未单独基准**：`CheckStickySeal`（`tss.go:1729`，一次侧车 RPC）与
    `btcwallet.listUnspent`（`btcwallet.go:829`）都在**提现/扫集**路径上，不在充值识别路径上，
    本基准文件不覆盖；只有「一条 seal 的字节成本」（**359 B**，来自 Rust 基准注释）与
    「`SealIndex` 是内存 map + 落盘」这个结构事实。
12. **侧车数字可能已漂移**：`rgb-sidecar/**` 正在被另一条线改动，§2.5 的记录是评估当时的快照。
13. **O3 的两次测量不一致**（225.5 ms vs 384.4 ms，同机同形状）**未归因**，按区间引用。

---

## 6. 与源码注释不一致之处（**待修正**，本文只记录不改代码）

`deposit_address.go` 的文件头与行内注释里，有三处论断被 #48 的实测/上游核对推翻。
**注意修订时要连带改结论**，不能只改措辞：

| 位置 | 注释说的 | #48 核对的结果 |
|---|---|---|
| `deposit_address.go:39-41` | 「rescan 未在跑时 `NotifyReceived` 会**从钱包生日起回扫一遍**（在跑时只是 AddAddrs）。所以按需发放的隐含成本 = 每个新用户一次 rescan；**N 个用户分散发放 = N 次 rescan**」 | **与上游实现不符**：① `scanning` 是**粘性 true**（`btcwallet@v0.16.17/chain/neutrino.go` 只有 `:420`/`:524` 两处 `= true`，`= false` 无命中）⇒ 进程内**第一次** `NotifyReceived` 之后全走 `Update(AddAddrs)`，**不重扫**、更不是 N 次 rescan；② 首次那次 rescan 从 **`chain.BestBlock()`（链尖）向前**扫，**不是从钱包生日**。（均为代码推导，见 §5 第 5/6 条） |
| `deposit_address.go:43-44` | 「1e4 个 program 的 watch 列表对 neutrino 的 filter 匹配是**常数级开销**，瓶颈在 rescan 次数而非集合大小」 | **两处都不准**：① filter 匹配是 **O(M log M + N_filter)**（实测 M=1e5 → 6.62 ms/块），不是常数；② 真正的瓶颈是**命中块**路径的 **O(输出 × M)**（M=1e4 → **3.78 s/块**），**既不是 filter 匹配、也不是 rescan 次数** |
| `deposit_address.go:249-250` | 「历史充值由 `NotifyReceived` 触发的 neutrino rescan（**从钱包生日起**）覆盖，不依赖这里」 | 同上：无 `StartBlock` 的 rescan 取 `chain.BestBlock()` 向前扫、**不回放历史** ⇒ 恢复场景下可能看不到历史充值。**报告判为疑似真 bug**，建议单独验证 |
| `deposit_address.go:263-265` | 「为什么批量：rescan 未在跑时**每次 `NotifyReceived` 都会触发一次回扫**，N 个脚本分 N 次调用就是 N 次回扫」 | 与 `:39-41` 同源同错：`scanning` 粘性 true ⇒ 只有**第一次**会 `Start()`，之后都是 `Update(AddAddrs)`。**结论（必须批量）仍然成立、代码也没错**，但**理由不对**——真正的理由是 §2.2 的**固定 10.5 ms/次 bolt 提交**（N 次 = N × 10.5 ms，1e4 用户 105 s），以及「一次 `NotifyReceived` 一批」的订阅语义 |

另：`deposit_address.go:36-38`「每 import 一个脚本 = 加一个 34 字节 program … 不增加任何逐地址的 RPC」
——**这部分是对的**（Go 侧走本地 filter 匹配，不是侧车那种逐脚本 `searchrawtransactions`），保留。

---

## 附：复现命令

```bash
# Go 基准（10 个）。单条跑，别一次全跑——本机负载敏感。
cd <repo>/plugin/dapp/lightclient/rpc/lightclient/neutrino
go test . -run '^$' -bench 'BenchmarkNeutrinoPaysWatchedAddr$' -benchtime=1x
go test . -run '^$' -bench 'BenchmarkDepositScriptSetLookup$'
go test . -run '^$' -bench 'BenchmarkSyncDepositScriptsRound$'

# Rust 基准（3 个 perf 用例）。必须单线程：并行会互抢 fsync，数字变噪声。
cd <repo>/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb-sidecar
cargo test --release --bench perf_p2wsh -- --nocapture --test-threads=1
```

> 评估当时主工作树编译不过（CGGMP WIP），基准跑在一个 detached HEAD = `049eb9a66` 的干净 worktree
> `/private/tmp/worktrees/plugin-p2wsh-perf`。**当前树已可编译**（`go vet` 通过），无需再建 worktree。
>
> 报告提到的 perf 基线文件名是 `docs/perf-baseline.md`（尚未建立）——本文可作该基线的第一版内容。
