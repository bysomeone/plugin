# 关键状态的备份与恢复

本文是备份范围、恢复路径与缺口的技术依据：说明哪些状态必须备份、丢了会怎样、如何恢复，以及已知缺口。

> **证据标记**（全文遵守，请勿把推断当事实引用）：
>
> | 标记 | 含义 |
> |---|---|
> | ✅ | **读代码确认**：本文件给出 `文件:行`，可直接复核 |
> | ⚠️ | **推断**：由确认的事实推出，但推导链的一段没有直接代码证据 |
> | ❓ | **未确认**：本文没有查清，写在这里是为了不让人误以为已结论 |
>
> 相关文档（不重复，只引用）：`CONFIG.md` §4.3.1–4.3.3 已详述「share ↔ 链上组公钥自检」与
> 「只许 refresh、不许 re-DKG」；本文只补**备份/恢复的运维面**。

---

## 0. 摘要

**必须备份的只有 4 类东西**，其余都能从链或从钱包重建：

| # | 状态 | 为什么不可放弃 |
|---|---|---|
| **B1** | 每节点 TSS 密钥材料（`neutrino.db` 的 `rgbx-tss` bucket） | 丢够份额（`threshold=3`）＝ **旧地址下的资金永久花不出去**。re-DKG 换群公钥，链上 `CrossChainInfo` 写死不可改 ⇒ 修不回来 |
| **B2** | 侧车 `/data`（`ledger.json` + `stock/`） | RGB 是**客户端验证**，资产状态链上查不出来 ⇒ 侧车状态**不可从链重建** |
| **B3** | 每节点中继本地库（`neutrino.db` 的 `rgb20-*` / `rgbx-*` bucket） | 业务台账；**多数可自愈或重建**，但 `rgb20-receive` 是 consignment 原文的全仓唯一留存 ⇒ 单独列为必须 |
| **B4** | 主链（`main`）自己的 `datadir` | `main` 在 compose 里**只有 1 个节点**：它是 `CrossChainInfo` / 已铸供应量 / 操作台账的**唯一持有者** |
| **B5** | 每节点的 **libp2p 身份私钥**（`system/p2p/dht/addrbook.go` 的 `privkey` 记录，落在节点 datadir 的 addrbook 库里） | TSS 认的身份就是 **libp2p peer id**，且由传输层认证（`cggmp/errors.go:16` 的 `errMissingPeerID`）。**只恢复 share、不恢复这个私钥 ⇒ 节点回来时 peer id 变了，组内不认它**，恢复就不是"换台机器"而是"换了个成员"。详见下方 §2.1 |

**§2.1 为什么 B5 必须进清单**

TSS 协议层一切寻址都用 peer id：`pm.SelfID()` / `pm.PeerIDs()`（`cggmp/api.go:105/131`），消息里带的 peer id 由**传输层认证**（不是发送方自称）。所以：

- **换机器 = 无损**，只要同时恢复两样：**(a) TSS share**（B1）与 **(b) libp2p 身份私钥**（B5）。peer id 由私钥派生（`addrbook.go:122-142`），私钥不变则 peer id 不变 ⇒ 对 TSS 而言**组网配置根本没变**（同一批 ID、同一份 bks、同样的份额），不需要任何重新组网动作。
- **只恢复 (a)、丢了 (b) ⇒ 节点回来是个新成员**，旧组不认它，得走加节点/重组流程（见 `docs/rgbx-bridge/01-系统设计.md` 的 TSS 生命周期说明）。
- ⇒ **备份清单把这两样按同一等级对待**：它们是"恢复一个节点"的两个必需件，缺一不可。

**风险分级一句话**：B1 丢 = 完蛋（资金锁死，不可逆）；B2 丢 = 完蛋（资产认知与 consignment 消失，
不可逆）；B3 丢 = 麻烦（可自愈 + 需重扫，服务降级）；B4 丢 = **完蛋**（单主节点时 `main-data` 是主链唯一样本）。

**主要缺口**（第 4 节展开；其中 stock 一项已由 G2 修复）：

- ~~侧车 `stock/*.dat` 的写不是原子的，`Stock::load` 失败会静默降级成空 Stock~~ —— **已修复
  （缺口 G2，2026-10-24）**：三份文件改为 tmp → `fsync` → `rename` → 目录 `fsync` 原子写入，
  载入失败或文件集不完整会**拒绝启动**并报出路径、大小与原因（`store.rs`、`engine.rs`）。见 §4 G2。
- `AssetRec.genesis_consignment_hex` **只写不读**（写：`engine.rs:728`；读：只有测试），
  且 `issue_usdt` 只打印 `asset_id` 不打印 genesis hex（`bin/issue_usdt.rs:196`）。

---

## 1. 部署拓扑（先说清楚东西在哪，否则表的路径没有意义）

`cmd/ci/docker-compose.yml` + `docker-compose-rgb20.yml`（**CI 拓扑**；生产拓扑不同会直接改变风险分级，
见 §2.5 与 §6 ❓3）：

```
main    (1 个)  卷 main-data   →  /root/datadir     主链（rgbx/lightclient 合约状态在这里）
para1   (官方)  卷 para1-data  →  /root/paradatadir 官方节点：btcwallet + 充值 watch 集 + 发地址
                                                       + 扫集 + rgb20 HTTP(17000) + 发地址 HTTP(17001)
para2..4(验证)  卷 paraN-data  →  /root/paradatadir 只参与 DKG/签名
btcd    (1 个)  bind ./btcd-data → /root/.btcd      regtest，**每次启动无条件清链**
rgb-sidecar(1)  卷 rgb-sidecar-data → /data         全桥**唯一一份**侧车状态（4 个 para 共用）
```

**关键点 1**：TSS 中继（`[rpc.sub.light.neutrino]`）**只在 para1–4 上跑**（`enableTSS=true` 只出现在
`chain33.paraN.toml:14`；`chain33.test.toml` 只有 `[exec.sub.lightclient]`）。⇒ **share 是每 para 一份，
共 4 份，不在一台机器上**（✅ `chain33.para1.toml:14`、`:618-670`）。

**关键点 2**：**侧车只有一份**（`rgb-sidecar` 单实例，`RGB_SIDECAR_DATA_DIR=/data`，
卷 `rgb-sidecar-data`）。它不随 para 复制 ⇒ **B2 是单点**（✅ `docker-compose-rgb20.yml:47,51,62`）。

### 1.1 每 para 节点的落盘路径（`neutrino.db` 是「一文件多用途」）

`config.go:223-225`：

```go
dbPath := filepath.Join(chainCfg.GetModuleConfig().BlockChain.DbPath, "lightclient")
db, err := openWalletDB(dbPath, "neutrino.db")
```

`BlockChain.DbPath` 在 para 上是 `paradatadir`（✅ `chain33.para1.toml:44`），
被 chain33 拼成 `<datadir>/paradatadir`（✅ `chain33/util/util.go:593`），容器里即 `/root/paradatadir`。
⇒ 每 para 的落盘是：

| 文件 | 绝对路径（CI 容器内） | 内容 |
|---|---|---|
| `neutrino.db` | `/root/paradatadir/lightclient/neutrino.db` | ① **`rgbx-tss` bucket（share，明文 JSON）** ② 全部 `rgb20-*`/`rgbx-*` 业务 bucket ③ **neutrino 自己的 SPV 头/过滤器/waddrmgr**（同一个 `walletdb.DB`，✅ `config.go:234-236` 把同一个 `db` 传进 `neutrino.Config`） |
| `btcwallet.db` | `/root/paradatadir/lightclient/btcwallet.db` | **watch-only 钱包**（`wallet.CreateWatchingOnly`，无任何私钥 ✅ `btcwallet.go:165-178`）：地址管理器 + tx store + **生日** |

> ⚠️ **`neutrino.db` 不是「小 key-value 库」**：SPV 头链与过滤器同住其中。主网体量会是 GB 级，
> 而其中**只有 `rgbx-tss` 那两条记录不可再生**。这是「直接整文件备份」这个朴素方案的核心痛点
> （第 4 节缺口 G1）。

### 1.2 侧车 `/data` 的完整文件清单（✅ `rgb-sidecar/src/config.rs:135-152`）

| 路径 | 内容 | 写入方式 |
|---|---|---|
| `/data/ledger.json` | 热账本：`assets` / `seals` / `receives` / `settled_by_outpoint` / `finalized_withdrawals`（✅ `ledger.rs:130-143`） | **原子**：tmp → `fsync` → `rename` → 目录 `fsync`（✅ `ledger.rs:202,317-331`） |
| `/data/stock/stash.dat` | `MemStash`：schemata / **geneses** / bundles / witnesses / **secret_seals** / type_system / libs（✅ rgb-ops `src/persistence/memory.rs:80-92`） | **原子**：tmp → `fsync` → `rename` → 目录 `fsync`（✅ `store.rs` `AtomicFsBinStore` → `ledger::write_atomic`） |
| `/data/stock/state.dat` | `MemState`：witnesses / invalid_ops / **contracts（合约状态本体）**（✅ 同上 `:289-297`） | 同上，原子 |
| `/data/stock/index.dat` | `MemIndex`：op↔bundle↔contract 索引 / **terminal_index（SecretSeal → Opout）**（✅ 同上 `:913-925`） | 同上，原子（三份文件逐个提交，彼此之间不是事务；中途崩溃会留下不完整文件集 → **拒绝启动**，见 G2） |
| `/data/builds/<sha256(seal集)>.json` | 提现构建归档（PSBT + consignment + fascia 的 deflate），**重放同一笔提现的唯一依据**（✅ `build_store.rs:138,160`） | 每笔一个文件，一次写、可裁剪 |
| `/data/user_scripts.json` | 用户 P2WSH 充值脚本 watch 集（program ↔ witnessScript + userID）（✅ `config.rs:147`、`wallet.rs:259,290`） | 非原子（`fs::write` 语义，未细查） |

侧车 gRPC 契约（`proto/rgb_sidecar.proto`）**没有** Stock/ledger 的 export/import：
可用的只有 `AdoptContract`（导 genesis）、`RegisterDepositScripts` 等（✅ 通读 proto 的 16 个 rpc）。

### 1.3 主链与 btcd

- **`main`**：卷 `main-data:/root/datadir`，`dbPath="datadir"`（✅ `chain33.test.toml:47`）。
  `mavl-rgbx-*`（`CrossChainInfo`、`deposited-txid-`、`withdrawn-`、`oprec-`、`opsupply-`、`opidx-`）
  与 `mavl-lightclient-*` 都在这里（✅ `rgbx/executor/kv.go:26-41`）。
  **compose 里 `main` 只有 1 个实例** ⇒ 这份数据没有副本。
- **btcd（regtest）**：`./btcd-data` 是 **bind mount**（✅ `docker-compose.yml:108`），
  且 btcd v0.24.2 在 regtest 下**每次启动无条件清链**（`removeRegressionDB()` 只以 `cfg.RegressionTest`
  为门，`--datadir` 挡不住；✅ `docker-compose.yml:89-96` 的注释 + `docker-compose.sh:341-348`）。
  ⇒ **CI 的 BTC 链本就设计成可丢弃**，不备份。
- **btcd（生产）**：生产应接真实 Bitcoin 网络，链数据可从网络重同步 ⇒ 也不需要备份
  （⚠️ 推断，以生产实际部署为准）。

---

## 2. 状态总表

「必须备份」一列里：**P0** = 丢了不可逆且直接等于资金损失；**P1** = 必须，但可从别处重建/自愈。

### 2.1 B1 · TSS 密钥材料（每节点独立，4 份）

| 项 | 内容 |
|---|---|
| **存放位置** | `<paraN-data>/paradatadir/lightclient/neutrino.db` → bucket **`rgbx-tss`**，两条 key：<br>• **`cggmp-dkg-result`**（群公钥、本节点 share、各节点 Birkhoff 参数、DKG rid、partial pubkey）<br>• **`cggmp-refresh-result`**（refresh 后 share、**Paillier 私钥素数 `paillierP/Q`**、`ySecret`、Pedersen/ppk 参数）<br>✅ `tss.go:59,60,69,481,506,517,542` |
| **格式 / 加密** | **明文 JSON**，无额外加密（✅ `CONFIG.md:412-413`、`tss.go:65-68` 注释）。**敏感度 = 私钥** |
| **是否独立** | 每节点一份，互不相同，缺一不可拼（✅ 4 个 para 各自 `initNeutrinoConfig` 开自己的 db） |
| **丢失后果** | **不可逆**。丢 1–3 份：剩余 ≥ `threshold=3` 仍能签（可用性下降）；**丢到只剩 2 份 ⇒ 旧 TSS 地址下的 BTC 与 RGB20 的 `threshold_sig` 永久无法产出**。re-DKG 换群公钥，而链上 `CrossChainInfo` 每 symbol 一份、写死不可改（`ErrDuplicateDKGCommit`，✅ `rgbx/executor/checktx.go:327-330`、`:78`）⇒ **资金永久锁死** |
| **能否靠重跑修复** | ❌ **不能**。唯一「换材料不换公钥」的机制是 CGGMP `ProcessRefresh`（= 删掉 `cggmp-refresh-result` 后全员重启补跑），但它**只能换 refresh 那一半**，`cggmp-dkg-result` 本身（含 share 与群公钥）丢了就没了 |
| **必须备份** | **P0，最高优先级** |
| **建议方式 / 频率** | DKG+refresh 落盘后**立刻一次**（这是一次性关键资产，之后不再变，除非跑 refresh）；变更（走 refresh）后重做一次 |
| **由谁持有** | **每个 para 的运维方各自持有自己的那份**，且**不得三份同处**（`threshold=3`，3 份 = 直接可花）。⚠️ 建议至少一份离线冷备 |

> ✅ **链上不含 share**：`CrossChainInfo` 只写 `tssAddress` / `pubkey`（✅ `rgbx/executor/crosschain.go:52-62`），
> 链上只有「群公钥」与「DKG 确认集合」。⇒ **share 在链上没有任何副本**。
>
> ✅ **chain33 侧不落盘**：`system/crypto/tss/**` 是纯算法包，`DKGResult`/`RefreshResult`
> （`cggmp/result.go:73,202`）只在内存里传递，**没有 DB 写**；持久化是调用方（plugin `lightclient/tss.go`）
> 的责任。⇒ 备份范围**不涉及 chain33 仓库**。

#### 2.1.1 启动自检能救什么、不能救什么

`client.go:399-460` 在启动最早期做「本地 share 的组公钥 ↔ 链上 `CrossChainInfo`」比对，
不一致即 **ERROR + 拒绝启动**（逃生阀 `tss.allowShareMismatch`，默认 `false`，✅ `client.go:202`）。

- **能救**：揪出「share 与链不是同一代」（re-DKG 后、从别的环境恢复后）——避免节点「参与签名但不被认」的静默失灵。
- **救不了**：它读的是 `cggmp-dkg-result` 里的**组公钥**（✅ `client.go:473-491` `loadTssGroupPubKeyFromDB`），
  并**不校验 share 本身是否完好**。⇒ 一条 `cggmp-dkg-result` 记录**JSON 可解析、share 字段被截断/改坏**，
  自检会放过（⚠️ 推断：`groupPubKeyFromDKG` 只解群公钥字段）。**没有「share 可用性」的自检**（不属于 G2；share 可用性至今无自动检查）。

### 2.2 B2 · 侧车 `/data`（全桥唯一一份）

| 项 | 内容 |
|---|---|
| **存放位置** | 卷 `rgb-sidecar-data` → `/data`（`RGB_SIDECAR_DATA_DIR`）。文件清单见 §1.2 |
| **丢失后果** | **不可逆**。RGB 是客户端验证：链上**只有**金额/承诺，没有资产状态 ⇒ 侧车状态**不可从链重建**。丢了以后：① 桥不知道任何 seal 的归属与余额；② 提现无法构造（`build_transfer` 要 `asset(symbol)` + Stock 里的合约状态）；③ 用户手上的 consignment 仍有效，但桥这边对不上账 |
| **必须备份** | **P0**。且**必须成套**（`ledger.json` + `stock/` + `builds/` + `user_scripts.json`）—— ⚠️ 单独拿一份没有意义，因为 `stock/*` 与 `ledger.json` 之间有跨文件一致性（§4 G4） |
| **建议方式 / 频率** | 状态随每笔充值/提现变更 ⇒ **每日 + 每次大额操作前**；侧车不提供在线一致性快照，必须用「停侧车 → 打包 → 起」的冷备 |
| **由谁持有** | 桥上运维方。因为是单点，**必须至少保留一份异地或离线副本**（见 `docs/rgbx-bridge/02-2-备份与恢复.md` §2） |

#### 2.2.1 `genesis_consignment_hex`：只写不读 —— **明确的缺口**

`genesis_consignment_hex` 存的是「合约创世 consignment 的原始字节」，注释明说它存在的目的就是
「Stock 重建时用它重新 import」（✅ `ledger.rs:45-47`）。

- **写**：`engine.rs:728`（`adopt_contract` 里 `hex_encode(genesis_bytes)`）。
- **读**：**生产代码里零处**。全仓 grep 只有测试读它（`rgb-sidecar/tests/adopt_contract.rs:44,113`）。
- **⇒ 缺口**：有「为重建而存」的数据，**没有「用它重建」的代码**。

**清了 `/data` = 永久丢失合约认知**（在下面这个前提下）：

- **自发行资产**（`issue_asset` → `issue_asset_at`）：genesis bytes 只在 `engine.rs:800-801` 生成一次，
  落进 `AssetRec` 后进 `ledger.json`；`issue_usdt` bin **只 `println!("issued USDT asset_id={}")`，
  不输出 genesis hex**（✅ `bin/issue_usdt.rs:196`）⇒ 数据目录清掉后**无处可寻**。
  （⚠️ 注意：genesis 也存在于 `stock/stash.dat` 的 `geneses` 映射里，且 rgb-ops 有
  `Stock::export_schema` / `export_contract`——✅ `rgb-ops src/persistence/stock.rs:635,647`。
  但**侧车没有把它们暴露成任何入口**（proto 里没有），所以「有数据、没工具」，与「没数据」在运维上等价。）
- **声明式引入的外部资产**（`RGB_SIDECAR_CONTRACTS` 文件里的 `genesisConsignment` /
  `genesisConsignmentFile`，✅ `config.rs:39-60`）：genesis 在**运维的配置文件**里还有一份。
  该文件本身**不在 `/data` 里**，但已通过 `cold_backup.sh` 的 `BACKUP_EXTRA_FILES` 纳入备份清单
  （按 `extra/<label>` 打包并校验，恢复时用 `RESTORE_EXTRA_TARGETS` 放回；运维口径见
  `docs/rgbx-bridge/02-2-备份与恢复.md` §1/§3）。G3 已按此关闭。

### 2.3 B3 · 中继业务 bucket（每节点一份，都在同一个 `neutrino.db` 里）

全部经 `rgb20.NewWalletStore(n.neutrinoCfg.Database)` 落在 `neutrino.db`（✅ `client.go:183`）。

| bucket | 内容 | 丢失后果 | 备份 |
|---|---|---|---|
| `rgbx-tss` | 见 §2.1 | 不可逆 | **P0** |
| `rgb20-receive` | receive ↔ chain33 请求映射，**含 `Consignment []byte`（用户上传的 consignment 原文）**（✅ `receive.go:18,27-33`） | **全仓唯一留存 consignment 原文的地方**。丢了：该笔充值无法重新验证/重放；⚠️ 若该笔尚未 mint 则永久卡住（需重新向用户索要 consignment）。**不可从链重建** | **P0**（列在这里是因为它是 B3 里唯一「不可再生」的一项） |
| `rgb20-seal` | seal 索引状态机（`pending-mint`/`minted`/`consumed`）（✅ `seal.go:17-27`） | 待铸集合丢失 ⇒ 可能重复铸造或漏铸（⚠️ 链上 `deposited-txid-` 去重兜底，故多半是「漏」而非「重」） | P1 |
| `rgb20-known-txid` | 已知 RGB txid 集合（避免 BTC 充值路径误判）（✅ `receive.go:19-21`） | 分类误判；可从 `rgb20-receive` 重建 | P1（可重建） |
| `rgb20-deposit-sig` | 已签充值产物（**性能缓存**） | **可自愈**：丢了就重签一轮，`C` 逐字节相同、链上按 btc-txid 去重（✅ `CONFIG.md:470-484` 明确说明） | P2（不必备份） |
| `rgb20-withdraw-txid` | BTC 提现 txid → chain33 提现哈希 | 提现关联丢失，需人工核对 | P1 |
| `rgb20-withdraw-sticky-seal` | chain33 提现哈希 → 绑定的 sticky seal | 重试可能换 seal ⇒ 与已广播交易冲突 | P1 |
| `rgbx-withdraw-state` | 提现状态（`broadcasted`/`confirmed`/**`unrecoverable`**）（✅ `bitcoin.go:420-424`） | 重试语义错乱；`unrecoverable` 是「停在这里」的凭据，丢了会重新尝试必败路径 | P1 |
| `rgbx-withdraw-sticky-utxo` / `rgbx-deposit-state` | 提现 sticky UTXO / 充值已处理标记（✅ `bitcoin.go:426,428`） | 同上 | P1 |
| `rgbx-deposit-scripts` | 用户充值脚本 watch 集（userID → pkScript）（✅ `deposit_address.go:52`） | **可重建**：`witnessScript = DeriveDepositWitnessScript(userID, tssPub)` 是**冻结的纯函数**（✅ `deposit_address.go:23-33,191`）；丢了只会漏认充值，用户再要一次地址即恢复（会触发一次 rescan） | P1（可重建） |
| `rgbx-btcwallet-monitor` | `min-pending-height` 水位线（⚠️ 注意它在 `neutrino.db`，不是 `btcwallet.db`：✅ `btcwallet.go:295,319` 用 `neutrinoCfg.Database`） | 丢失后从钱包自己的最早点重放（✅ `btcwallet.go:395-413`）⇒ 可重建，只是慢 | P2 |
| — | neutrino 自己的 SPV 头/过滤器/waddrmgr | 重同步（regtest 下**不可**恢复，见 §2.4） | P2（不必） |

`btcwallet.db`（独立文件）：

| 项 | 内容 |
|---|---|
| **丢失后果** | **生日丢失**。钱包生日 = `btcwallet.db` **创建时刻 − 48 小时**（不是创建时刻本身：waddrmgr 留了 2 天安全宽限，`waddrmgr/manager.go:1994-1995`，连 `btcwallet.go:187` 打印出来的也是这个减过的值），neutrino 的 rescan 用 `StartTime=生日` 截断 ⇒ **生日之前的 BTC 块不会被下载 filter ⇒ 生日之前的充值永远看不见**。⚠️ 且旧 TSS UTXO 对钱包不可见，那部分 BTC 桥上花不出去。**没有任何配置能改变这一点**（✅ `CONFIG.md:259-266,659`） |
| **必须备份** | **P0**。这是「不备份 B3 也无所谓」论调的反例：`btcwallet.db` 无任何私钥（watch-only），但**丢了就永久看不见历史充值** |
| **频率** | 同 `neutrino.db`（每日）。⚠️ **必须与 `neutrino.db` 同一时刻取样**（两者有跨文件状态，见 G4） |

### 2.4 可从链重建、**不必备份**的状态

| 状态 | 在链上吗 | 结论 |
|---|---|---|
| `rgbx` 合约状态（`CrossChainInfo`、`deposited-txid-`、`withdrawn-`、`confirm-used-`、`oprec-`、`opsupply-`、`opidx-`） | ✅ `mavl-rgbx-*` 在链上（✅ `kv.go:26-41`） | 不必备份，**但依赖「链还在」（= B4）** |
| `lightclient` 合约状态（`btc-lastheader`、`btc-chainstate` 窗口） | ✅ `mavl-lightclient-*`（✅ `CONFIG.md` 与 plan 已核） | 不必备份 |
| dapp **localdb**（`LODB-lightclient-*` 逐高度头、`LODB-rgbx-*`） | ❌ 节点私有、非共识 | 理论上可从链重放，但 ✅ **chain33 启动不重放历史 `ExecLocal`，现有重建通道都是「版本升级触发」，运维无法自行触发**（`CONFIG.md:56-66`）⇒ **不是「不必备份」，而是「没有重建入口」**（缺口 G5） |
| para 链数据（`paradatadir` 的链部分） | ✅ 由主链 paracross 派生 | 可从主链重建；⚠️ 需要能重跑 nodegroup apply（推断） |
| btcd/regtest 链数据 | ❌ | **测试环境专属：不备份**。regtest 每次启动无条件清链（✅ §1.3）⇒ CI 里「BTC 链」本就当可丢弃。**生产接真实 Bitcoin 网络，可从网络重同步，同样不备份** |
| 侧车 `builds/` 归档（已裁剪的） | ❌ | ⚠️ 裁剪窗口由 `RGB_SIDECAR_BUILD_RETAIN_CONFIRMATIONS` 控制（✅ `engine.rs:2456-2461`）。**裁剪后只保留元数据，重放载荷永久消失** ⇒ 若某笔提现的广播失败且归档已裁剪，**无法重放出同一 txid**（✅ `build_store.rs:150-165` 的 `Ok(None)` / `Err` 语义）⇒ 建议**裁剪窗口设得保守**，且**备份须包含 `builds/`** |

### 2.5 B4 · 主链（`main`）

**CI 拓扑下 `main` 只有 1 个节点，`main-data` 是主链的唯一样本。** 丢了以后：

- ⚠️ **不可从 para 链重建**（para 是主链的派生，方向相反）。若有其他主链节点，可从 P2P 同步 ——
  但 CI compose 里没有。
- ⇒ **生产必须保证主链至少有 ≥2 个独立节点**，否则「备份主链数据」本身就成为 P0。
  这属于**拓扑要求**，不是备份工具能解决的（缺口 G6）。

> ✅ **若生产为单主节点**，`main-data` 与 B1/B2 同属「不可重建」一档，必须备份。上面的「主链 ≥2 个独立
> 节点」仍是有价值的**拓扑级加固建议**，但它是备份的补充、不是替代；多节点时 `main-data` 可从 P2P 同步。

### 2.6 网关数据（`gateway-data`）

网关是 gateway 架构新增的组件，`--data-dir` 下有四类数据：

| 数据 | 内容 | 丢失后果 | 结论 |
| --- | --- | --- | --- |
| `blobs/` + `index/` | 用户投递的 consignment 与 ACK 状态 | 未交付到桥、且用户只上传过一次的那份交付物丢失；桥看不到该笔充值，用户需重传 | **可丢**：协议是 at-least-once，钱包侧可重传；不是权威账本 |
| `deposit-requests/` | 签名充值请求与已出 invoice | 用户可重新提交同一 `request_id`（签名不变）找回；桥按 request id 幂等 | **可丢，可重放** |
| `bridge-events/events.jsonl` | 审计事件镜像（默认 90 天） | 统计/对账缺一段；链上数据仍在 | **可丢**，但建议纳入备份以便审计连续 |
| `--ttl` 控制 | consignment 默认 7 天、充值请求同 TTL | —— | 过期即删 |

**备份范围不含网关**（`cold_backup.sh` 的源清单只覆盖 main/para/sidecar）。这是有意的：网关不是权威，
丢失后的恢复动作是"让用户重传 / 用户用同一请求 id 重新取 invoice"。若审计要求事件连续，请把
`<gateway-data-dir>` 作为额外源加入备份（它很小），并在恢复后接受最多 90 天的历史窗口。

---

## 3. 恢复流程

### 3.1 单节点恢复（同代还原 —— **唯一可靠路径**）

**前提**：链上 `CrossChainInfo` 已存在（即该组已 commit DKG）。此时**绝不允许 re-DKG**
（✅ `tss.go:114-143` `errRedkgRefusedOnChainWithoutLocalShare` 会 panic 拒绝启动 —— 这是**特性不是 bug**）。

```
① 停掉该 para 容器（不要动其他节点）
② 判断「备份的哪一代」：备份里的 cggmp-dkg-result 必须与链上 CrossChainInfo 同代
     - 判据：取备份里 dkg-result 的群公钥，与链上 pubkey 比（BTC 走 hash160(群公钥)==pkScript[2:]，
       见 CONFIG.md §4.3.1）
③ 还原两样东西，且必须是同一时刻的样本：
     - <paraN-data>/paradatadir/lightclient/neutrino.db     ← 含 rgbx-tss
     - <paraN-data>/paradatadir/lightclient/btcwallet.db    ← 生日 + tx store
④ 起容器，看启动日志：
     - "ensureDKG load from local db"        = share 载入成功
     - share 自检未 panic                    = 与链同代
     - 若 refresh 记录缺失：会自动补跑一轮 refresh（✅ tss.go:569-583；需**缺记录的节点一起重启**）
⑤ 验证：该节点参与一笔真实签名（或 E2E 场景）
```

> **G1 已实现（2026-10-24）**：`cmd/rgb-tss-tool` 可以在节点停止时把 `rgbx-tss`
> 单独导出成 JSON（`export --db ... --out ...`），并导入到目标 `neutrino.db`
> （`import --file ... --db ... [--force]`）。导出文件含群公钥、rid、share id，
> 导入后逐字节读回校验；覆盖已有值时先写 `<db>.tss-backup-<unix>`。用法与限制见
> `cmd/rgb-tss-tool/README.md`。②的「哪一代」比对仍要靠群公钥对链上记录，但不再需要
> 打开 bbolt 手工找记录。导出的仍只是**单节点 share**，恢复集群依旧需要 ≥threshold 份。

### 3.2 侧车从零恢复

```
① 停 rgb-sidecar（避免半写状态被 tar）
② 还原整个 /data（ledger.json + stock/ + builds/ + user_scripts.json）
   或：还原 /data + 重新声明合约（用 RGB_SIDECAR_CONTRACTS 文件，见下）
③ 起侧车，看启动日志：
     - "RGB_SIDECAR_CONTRACTS: N contract(s) declared"
     - apply_declared_contracts 失败即启动失败（✅ engine.rs:300-303，这是设计：坏声明不能看起来像「资产没了」）
④ 对账：拿链上 opsupply-（已铸总量）与侧车 get_balance 比
```

**如果要「从零重建」而不是「还原备份」**（即 `/data` 真的没了）：

```
① 拿回每个合约的 genesis consignment 原始字节
     - 声明式资产：运维的 RGB_SIDECAR_CONTRACTS 文件里还有（已由 `BACKUP_EXTRA_FILES` 纳入备份，
       见 §4 G3）
     - 自发行资产：**没有出口**（缺口 G0，本文件的核心缺口）
② 起一个空 /data 的侧车 + 声明文件 → 合约会被 adopt 进 Stock
③ **但 historical state 不会回来**：seals / receives / 已发生的 transfer 全部丢失
     - 需要从链上重建「哪些 seal 属于桥、多少量」——链上只有 opsupply-（累计铸/毁）
       与 oprec- 台账（✅ kv.go:33-41），**没有 per-seal 明细**
     - ⇒ 只能得到一个「合约存在但余额为 0」的侧车（⚠️ 推断：未验证 adopt 后 Stock 的实际可操作状态）
④ 结论：**这不是一条真正可用的恢复路径**，只是「让服务能起来」
```

### 3.3 全组从零（灾难：share 全丢或链重建）

**这是设计上必须避免的状态**（✅ `CONFIG.md:427-429`）：唯一出路是**整组 + 链一起重建**
（全新链上没有旧 `CrossChainInfo`）。代价：**旧地址下的一切 BTC 与已铸 RGB20 全部作废**。
⇒ 这是「备份没做好」的终局，不是恢复方案。

### 3.4 恢复后的自检清单

| 检查 | 怎么做 | 谁做 |
|---|---|---|
| share 与链同代 | 启动日志无 panic + `CONFIG.md §4.3.1` 的 fail-closed | 自动 |
| share 自检覆盖不到的「share 完好性」 | **没有自动检查**（与 G2 无关，见 §6 ❓1） | ❌ 无 |
| 侧车账 ↔ 链上供应量对账 | 人工：`chain33-cli` 查 `opsupply-` vs 侧车 `GetBalance` | 人工 |
| 侧车 Stock ↔ ledger 一致 | **有检查**：启动时逐个核对 ledger 资产在 Stock 中有合约，缺失即拒启（`verify_stock_matches_ledger`）；已声明合约可重新 adopt 修复。跨文件的快照一致性靠停机冷备保证（G4 已按最小方案关闭），在线快照未实现 | 自动 |
| `btcwallet.db` 生日是否覆盖历史 | **没有检查**（缺口 G7） | ❌ 无 |

---

## 4. 缺口清单

按「会不会导致资金不可逆损失」排序。工作量是**粗估**（人日，含测试与一次 E2E 验证）。

| # | 缺口 | 后果 | 粗估 | 依赖 |
|---|---|---|---|---|
| **G0** | **`genesis_consignment_hex` 只写不读 + 无 Stock 导出入口**；自发行资产的 genesis 无处可寻 | 侧车 `/data` 全丢 = **合约认知永久丢失**，不可逆 | **1–2 人日**（加一个 `ExportContract` RPC 或 CLI：`Stock::export_contract` 已有库函数）+ 0.5 人日（`issue_usdt` 把 genesis hex 落盘 / 打印） | 侧车 proto 改动 |
| ~~G1~~ | ~~**没有「从 `neutrino.db` 单独导出/导入 `rgbx-tss`」的工具**~~ —— **已落地（2026-10-24）**：`cmd/rgb-tss-tool export/import` 在节点停止时只导出 `rgbx-tss` bucket（JSON、0600），导入前写回滚副本、无 `--force` 时拒绝覆盖，并校验群公钥/rid；用法见该目录 `README.md` | 备份不再必须整库拷贝；恢复仍要求 ≥3/4 份且同代 | 已完成 | — |
| ~~G2~~ | ~~**`stock/*.dat` 写非原子 + 载入失败静默降级成空 Stock**~~ —— **已落地（2026-10-24）**：三份文件改为 tmp → `fsync` → `rename` → 目录 `fsync`；载入/完整性失败一律**拒绝启动**，不再有 in-memory 空 Stock（`store.rs`、`engine.rs`） | 现在损坏的 `stock/*.dat` 或残缺的文件集会让启动**带路径、大小与原因失败**；`ledger` 有资产而 Stock 没有时也拒启（除非该合约已声明，可重新 adopt 修复）。写失败在持久化边界上报为错误，ledger 不会先于 Stock 前进 | 已完成（见下方【订正 2026-10-24】） | 已修复 |
| ~~G3~~ | ~~**`RGB_SIDECAR_CONTRACTS` 声明文件不在 `/data`、无备份约定**~~ —— **已关闭（纳入备份清单）**：`cold_backup.sh` 的 `BACKUP_EXTRA_FILES` 把它按 `extra/<label>` 打包并计入清单校验，`restore_backup.sh` 用 `RESTORE_EXTRA_TARGETS` 放回；运维口径见 `docs/rgbx-bridge/02-2-备份与恢复.md` §1/§3 | 声明文件随包保存，风险降到与其它备份成员相同 | 已完成 | — |
| ~~G4~~ | ~~**`/data` 无一致性快照点**~~ —— **已按最小方案关闭**：冷备流程是「停 → 打包 → 起」，侧车在打包期间不写；`docs/rgbx-bridge/02-2-备份与恢复.md` §3 已明确「备份前必须停侧车」。**注意**：`Snapshot` RPC / 在线一致性快照**未实现**，在线热备仍不可用 | 一致性由停机窗口保证；在线快照仍缺失 | 已完成（文档方案，0.5 人日） | 侧车 |
| **G5** | **dapp localdb 无重建入口** | localdb 丢 ⇒ 掉队停机，只能等一次带版本升级的发布或临时开逃生阀 | （已在 `CONFIG.md:56-66` 记为后续项） | — |
| **G6** | **生产主链拓扑**（单主节点或多节点） | 单主节点时 `main-data` 丢 = 链丢 = 全部合约状态丢，**与 B1/B2 同级、必须备份**；多节点时可从 P2P 同步 | 拓扑要求 | — |
| ~~G7~~ | ~~**`btcwallet.db` 生日没有自检**~~ —— **已落地**：启动时 `checkWalletBirthday` 比对钱包生日与头链起点，覆盖不到历史时打印 `ALERT wallet birthday cannot cover chain history`；E2E 探针会删除 `btcwallet.db` 并断言告警出现（`backup_test.sh` 的 `scenario_wallet_birthday_guard`）。触发条件是「`btcwallet.db` 丢了/被重建」，**不是**「从旧备份恢复」（原措辞方向错了，见下方【订正 2026-09-22】） | 重建钱包会被立即发现，不再静默漏充值 | 已完成 | — |
| ~~G8~~ | ~~**无恢复演练**~~ —— **已落地**：`rgbx/cmd/ci/scripts/backup_test.sh`（备 → 真的销毁卷 → 还原 → 起 → 断言 → 跑业务）。覆盖面与不模拟的场景见 §4.1 | 每次 E2E 都执行完整恢复链路 | 已完成 | — |

**合计**：G0 ≈ **1.5–2.5 人日**（G1/G2/G3/G4/G7 已关闭；G5 为后续项）；G8 的恢复演练已落地为 E2E 场景。
G4 若要做在线快照（`Snapshot` RPC），仍需另计 2–3 人日。
→ 与 `docs/rgbx-bridge/02-运维手册.md` 的备份章节是同一批工作。

**【订正 2026-10-24】G2 已修复：stock 的写现在是原子的，载入失败是 fail-closed**

本节上半部分与 §0/§1.2 的原文描述的是**修复前**的行为（`File::create` 直接写目标文件、`Stock::load`
失败回退到 `Stock::in_memory()`）。现行行为以代码为准（`rgb-sidecar/src/store.rs`、`engine.rs`）：

- **写**：三份 `stock/*.dat` 都经 `ledger::write_atomic`（`<name>.dat.tmp` → 写 → `fsync` → `rename`
  → 目录 `fsync`）。任何时刻目标文件要么是旧完整版本、要么是新完整版本。`rgb-ops` 在事务内要求
  `store()` 返回 `Ok`，所以写失败被记录并由 `RgbEngine::save()` 在持久化边界的**下一次调用**上报为
  错误——此时 `ledger.json` **不会**先于 Stock 前进。
- **读**：只有 `stock/` 下三份文件**全都不存在**才视为首次启动（空 Stock，行为与原来一致）；任何一份
  缺失（部分还原/首个写没写完）、或存在但解码失败/读失败，启动**直接失败**，错误带文件路径、字节数与
  原因。不再有"静默空 Stock"。
- **ledger ↔ Stock 一致性**：启动末尾逐个核对 ledger 里的资产在 Stock 中都有合约，缺一个就拒启；若该
  合约在 `RGB_SIDECAR_CONTRACTS` 里声明了，则先按 genesis consignment 重新 adopt（这是"只有
  `ledger.json` 被还原"时的修复路径）。
- **仍未解决**：三份文件是**逐个**提交的，文件之间不是事务；中断会留下不完整文件集——现在它表现为
  **拒绝启动**（可诊断），而不是静默错乱。彻底消除需要 journal（属 G4 的一致性快照范畴）。
- 由此，§6 的 ❓4（`adopt_contract` 在 Stock 空/ledger 有时的行为）与 ❓8（文件不存在与文件损坏是否
  同一种 `Err`）都已由上述实现回答。

**【订正 2026-09-22】G7 的方向（原文写反了，会误导运维）**

原文说"从旧备份恢复后**可能**看不见某些历史充值"。**恰恰相反**：

- **从旧备份恢复 → 看得见**。还原回来的 `btcwallet.db` 里生日仍是原始 T0，而 `SyncedTo` 是备份那刻的 tip；启动后钱包从 `SyncedTo` 向前回扫（`wallet/rescan.go:305-308` 取 `startStamp = SyncedTo()`），覆盖 `[备份 tip, now]` —— 这一段完全在生日之后，不会被 `StartTime` 截断。也就是说**备份齐全时，停机期间的充值恢复后能补回来**。
- **`btcwallet.db` 丢了 / 被重建 → 才静默漏**。此时走 `wallet.CreateWatchingOnly(..., types.Now())`（`btcwallet.go:172-179`，**这一步没有任何日志**），生日变成"现在 − 48h"，**更早的充值永远看不见**；而且症状是"账面上一切正常、钱不在账上" —— 没有 ERROR、没有 WARN，只有一行 `newBtcWallet open wallet success ... birthtime` 的 INFO。

**对备份清单的直接含义**：`btcwallet.db` 里**没有任何私钥**（watch-only），很容易被人当成"可重建"而排除在备份之外 —— **但它是不可替代的 P0**。丢了它等于此后所有早于"重建时刻 − 48h"的充值静默不入账。已在必需清单里（`scripts/cold_backup.sh` 的 `para1:lightclient/btcwallet.db`），此处的订正是为了不让"可重建"的直觉把它从清单里划掉。

**判定强度（如实标注）**：以上是**静态读码**（上游 pinned 源码 + 我们零覆盖）推出来的结论，**未实测**。要实测用 G8 演练加两臂：A 臂=停桥前备份 → 停桥后打一笔充值 → 销毁还原 → 起桥，预期日志出现该笔 `detected deposit`（**若不出现，则上面"恢复后能补回"的判断错**）；B 臂=只删 `btcwallet.db` 保留其余再起，预期**静默**漏该笔。regtest 下也可用 `setmocktime` 把链时间推到 T0 来免等 3 天。

### 4.1 G8 演练的覆盖范围

**自动演练断言**（每轮 E2E 最后一个场景，`RGBX_BACKUP_DRILL=0` 可跳过）：

1. 备份包的**自校验** —— 清单里每个文件的大小 + sha256 在还原时逐个核对；`restore_backup.sh --dry-run`
   **在动手销毁之前**先拿同一组参数跑一遍，参数写错时数据还在。
2. 销毁是**真的**（6 个卷，`find -delete`，断言剩 0 个文件），不是"假装清掉"。
3. 还原后**状态逐项相等**，不是"起来一个空壳"：
   - chain33 侧余额；
   - 侧车账本指纹（资产 id + seal 数 + 可支配额）；
   - **TSS peer 名单**（这条才是 B5 的证明 —— libp2p 身份私钥回来了；不一致意味着节点以新成员身份回归，老 share 全部作废）；
   - 4 个 para 的钱包身份（authAccount 地址在位）。
4. 还原后**业务仍可用**：跑一笔提现 —— 走完 TSS 签名 → 侧车 BuildWithdrawal → signPsbt → BTC 广播 → 链上销毁。

**自动演练不模拟的场景（上线前必须单独验证）**：

- **还原后的充值路径不在自动演练内**。演练最后那笔"充值"只在还原后的账本**仍有 `minted` seal 时**才跑得起来；
  而 test-sim 的"用户付款"是**用桥自己的 seal 模拟**的（`simulate_user_pay` 花掉桥的 seal、找零回 TSS 脚本），
  主套件的充值+提现循环会把这些 seal 耗尽（实测备份那一刻：5 consumed + 1 pending-mint，**零 minted**）。
  这是 **fixture 限制，生产里没有对应物**（真实用户带自己的 USDT 付款，不花桥的余额）。
  演练遇到这种情况会**显式打出 SKIPPED 并说明边界**，不静默通过。
- **单节点恢复已在自动演练中实现**（`scenario_single_node_restore`，每轮 E2E 在整组场景之后跑）：从整组包里用
  `restore_backup.sh --only para2` 只还原一个 para（validator），断言
  ① 该节点的 **TSS peer 名单与还原前完全一致**（B5：libp2p 身份私钥随包回来，不是"新成员"）；
  ② 钱包身份在位（authAccount 私钥 = para 共识签名钥 + TSS 提交钥，且是在任何 seed/import 之前断言的）；
  ③ 节点追平主链后参与一笔真实提现（TSS 签名 → 侧车 BuildWithdrawal → BTC 广播 → 链上销毁）。
  **不模拟**：跨机器/跨属主（同机同用户）、启动日志行（`ensureDKG load from local db` 等）的断言、
  refresh 协调步骤、以及 ≤ 30 分钟用时统计。
- **全组从零不在自动演练内**（`/data` 全丢 + 重新 adopt），见 §3.3；上线前必须单独演练。
- **备份年龄永远是 0**：演练在停服后立刻打包、立刻还原，btcd 不前进，所以"用一份 N 小时前的包恢复"没有被测到。
  若生产主链是单节点（§2.5），还原旧包会回滚停机窗口内的 `CreateReceive`/consignment，而 BTC 侧付款真实发生
  ⇒ 这批充值需要用户重传；这个场景必须在上线前用非 0 年龄的包单独验证。
- **备份源本身是否健康不验**：`btcwallet.db` 只验存在与 sha256，无法证明其生日覆盖历史（若该文件此前被误删重建，
  备份照常 PASS，但更早的充值永久不可见，见 G7 与 `checkWalletBirthday` 的启动告警）；四份 share 只验"与活文件一致"，
  不证明每份都可用（share 可用性，见 §6 ❓1）；`sidecar:stock` 只验存在，语义一致性靠停机冷备（G4 最小方案）
  与提现腿间接覆盖，**没有**在线快照检查。
- **不模拟**：跨机器/跨属主恢复（tar 保留 uid/gid；演练同机同用户）、备份包异地留存/传输损坏、
  备份或还原中途断电与网络分区。以上场景上线前必须单独验证。
- **网关数据不在演练范围内**（见 §2.6）：六个卷不包含 `gateway-data`，销毁/还原都不涉及它。

**金额口径（踩过的坑）**：演练里"还原后跑业务"的金额**必须跟着还原后的实际状态走**，不能用默认值 ——
默认充值是 `RGB20_DEPOSIT_AMOUNT=1000000`、默认提现是 `RGB20_WITHDRAW_AMOUNT=500000`，
而还原后的账本/余额是前面场景的**残余**（实测用户余额 0.0004 = 40000 最小单位）。
直接用默认值会得到 `insufficient USDT ... have 0` 和 `ErrInsufficientBalance`，
**看起来像"还原失败"，实则金额选错**。换算口径：display 余额 × 1e8 = 最小单位。

---

## 5. 风险分级

### 🔴 丢了就完蛋（不可逆，直接影响资金）

| 项 | 位置 | 一句后果 |
|---|---|---|
| `rgbx-tss/cggmp-dkg-result` × 4（**丢到 < 3 份**） | `<paraN>/paradatadir/lightclient/neutrino.db` | 旧 TSS 地址下的 BTC + RGB20 签名能力**永久丧失**；re-DKG 修不了（链上写死） |
| `rgbx-tss/cggmp-refresh-result` × 4 | 同上 | 同上（Paillier 素数 + share 即可签名，敏感度等同 share） |
| 侧车 `/data` 全套 | 卷 `rgb-sidecar-data` | 资产状态**不可从链重建**；合约认知丢失（G0） |
| `rgb20-receive`（含 consignment 原文） | `<paraN>/.../neutrino.db` | 未 mint 的充值**永久卡住**（consignment 无第二份） |
| `btcwallet.db` | 同上目录（独立文件） | 生日丢失 ⇒ **历史充值永久看不见**（无配置可救） |
| `main-data`（**若生产为单主节点**） | 卷 `main-data` | 链丢 = 全部合约状态丢（G6） |

### 🟡 只是麻烦（可自愈 / 可重建 / 服务降级）

| 项 | 恢复方式 |
|---|---|
| `rgb20-deposit-sig` | 自动重签（✅ `CONFIG.md:481-484`） |
| `rgbx-deposit-scripts` / 侧车 `user_scripts.json` | 用户再要一次地址即重建（纯函数派生，✅ `deposit_address.go:191`） |
| `min-pending-height` | 从钱包最早点重放（✅ `btcwallet.go:395-413`） |
| dapp **localdb** | ⚠️ 名义上可从链重放，但**没有重建入口**（G5）⇒ 实际是「等一次版本升级」 |
| neutrino SPV 头/过滤器 | 重同步（**真实 Bitcoin 网络下可行；regtest 下不可**） |
| para 链数据 | 从主链重建（⚠️ 推断） |
| 侧车 `builds/`（**未裁剪**的） | 重放可用；**已裁剪**的载荷永久消失（§2.4） |

### ⚪ 不必备份

| 项 | 为什么 |
|---|---|
| btcd / regtest 数据 | 测试环境专属；regtest 每次启动无条件清链，**生产接真实网络可重同步** |
| `rgbx` / `lightclient` 合约状态（`mavl-*`） | 在链上（**前提是链还在** —— 见 B4） |
| 链上 `CrossChainInfo` / `oprec-` / `opsupply-` | 同上 |
| 侧车二进制、chain33 二进制、toml（除 `RGB_SIDECAR_CONTRACTS`） | 可从仓库重建 |

---

## 6. **未确认**、不得当结论引用的项

| # | 未确认的事 | 影响 |
|---|---|---|
| ❓1 | **门限语义**：`threshold=3` + 4 节点的**确切数学含义**。历史清单已指出「rank 决定插值结构，不是必需性」，且 alice 只校验「参与方个数 ≥ threshold」。⇒ 「丢 1 份是否仍可签」「哪几份组合能签」**我没有独立验证**（其中引用的 `crypto/birkhoffinterpolation/birkhoffinterpolation.go:188` **不在本地 chain33 检出里** —— 本文未复核到源码） | 若「任意 3 份即可签」，则**3 份备份同处 = 单点**（🔴）；若语义更严，分级会变 |
| ❓2 | 「权重 0 表示必须参与」的说法来自哪一版设计 | 同上；本文按**最保守假设**（任意份额丢失都可能致命）设计 |
| ❓3 | 生产拓扑（几个主链节点？侧车是否单实例？是否共用一个 btcd？） | 直接决定 B4 / B2 的风险等级 |
| ❓4 | ~~`adopt_contract` 在「Stock 空但 ledger 有 asset」时的最终可操作状态~~ —— **已随 G2 解决（2026-10-24）**：合约缺失时从 genesis consignment 重新 import；未声明则启动拒启（启动自检 `verify_stock_matches_ledger`） | 已不影响分级 |
| ❓5 | `MemState`/`MemStash` 各字段对「哪些操作会因此失效」的精确映射 | 只影响表述精度，不影响分级 |
| ❓6 | `user_scripts.json` 的写是否原子 | 只影响它自己的损坏概率 |
| ❓7 | 侧车 `stock.store()` 是否在**全局 engine 锁**内（若在，快照 RPC 的实现风险更低） | 影响 G4 的实现方式 |
| ❓8 | ~~`Stock::load` 对「文件不存在」与「文件损坏」是否返回同一种 `Err`~~ —— **已随 G2 解决（2026-10-24）**：三份文件全缺 = 首次启动；任何一份缺失或损坏 = 启动失败（`StockDirState`） | 已不影响分级 |

---

## 附：与既有文档的关系

| 文档 | 关系 |
|---|---|
| `CONFIG.md` §4.3.1–4.3.3 | **详述** share↔链自检、CGGMP 两份材料、refresh 语义。本文**不重复**，只补运维面（备份范围/频率/持有者/恢复步骤） |
| `CONFIG.md` §4.1.2 | `btcwallet.db` 生日与「只能从备份还原」的**权威说明** |
| `docs/rgbx-bridge/02-运维手册.md` | 本文是备份章节的设计依据 |
| `docs/rgbx-bridge/01-系统设计.md` | 侧车/中继的实现细节与安全模型 |
