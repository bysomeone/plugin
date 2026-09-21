# 关键状态的备份与恢复

> **任务 #27**（`LAUNCH_READINESS.md` 二「上线前必须」第一项）。本轮只做**现状盘点 + 规划**，不含实现。
>
> 基线：plugin `feat-rgb-usdt-integration`。盘点开始时 HEAD = `38a6cc0f5`，**盘点期间仓库在并发推进**
> （另有 agent 在提交，HEAD 已到 `1b65b6ff2`）；本文所有关键事实在 `1b65b6ff2` 上**复核过一遍**
> —— 期间落地的提交（`b5e7af71c` 声明式合约、`1b65b6ff2` 充值 consignment 合约身份校验）
> **没有新增任何持久化 bucket**（✅ `git show 1b65b6ff2 | grep -i bucket` 无命中）。
> chain33 `feat/tss-alice-cggmp` @ `36fb0fd21`。
> 盘点方式：只读浏览代码/配置（`neutrino/**`、`rgb20/**`、`plugin/dapp/rgbx/**`、chain33 `system/crypto/tss/**`、
> `cmd/ci/docker-compose*.yml`，以及 cargo registry 里的 `rgb-ops-0.11.1-rc.11` / `rgb-strict-encoding-1.0.2`）。
>
> **证据标记**（全文遵守，请勿把推断当事实引用）：
>
> | 标记 | 含义 |
> |---|---|
> | ✅ | **读代码确认**：本文件给出 `文件:行`，可直接复核 |
> | ⚠️ | **推断**：由确认的事实推出，但推导链的一段没有直接代码证据 |
> | ❓ | **未确认**：本轮没查清，写在这里是为了不让人误以为已结论 |
>
> 相关文档（不重复，只引用）：`CONFIG.md` §4.3.1–4.3.3 已详述「share ↔ 链上组公钥自检」与
> 「只许 refresh、不许 re-DKG」；本文只补**备份/恢复的运维面**。

---

## 0. 摘要

**必须备份的只有 4 类东西**，其余都能从链或从钱包重建：

| # | 状态 | 为什么不可放弃 |
|---|---|---|
| **B1** | 每节点 TSS 密钥材料（`neutrino.db` 的 `rgbx-tss` bucket） | 丢够份额（当前 `threshold=3`）＝ **旧地址下的资金永久花不出去**。re-DKG 换群公钥，链上 `CrossChainInfo` 写死不可改 ⇒ 修不回来 |
| **B2** | 侧车 `/data`（`ledger.json` + `stock/`） | RGB 是**客户端验证**，资产状态链上查不出来 ⇒ 侧车状态**不可从链重建** |
| **B3** | 每节点中继本地库（`neutrino.db` 的 `rgb20-*` / `rgbx-*` bucket） | 业务台账；**多数可自愈或重建**，但 `rgb20-receive` 是 consignment 原文的全仓唯一留存 ⇒ 单独列为必须 |
| **B4** | 主链（`main`）自己的 `datadir` | `main` 在 compose 里**只有 1 个节点**：它是 `CrossChainInfo` / 已铸供应量 / 操作台账的**唯一持有者** |

**风险分级一句话**：B1 丢 = 完蛋（资金锁死，不可逆）；B2 丢 = 完蛋（资产认知与 consignment 消失，
不可逆）；B3 丢 = 麻烦（可自愈 + 需重扫，服务降级）；B4 丢 = **完蛋**（生产与 CI 同为单主节点，`main-data` 是主链唯一样本）。

**最刺眼的两条现状问题**（第 4 节展开）：

- 侧车 `stock/*.dat` 的写**不是原子的**（`File::create` 直接写目标文件，无 tmp/rename/fsync），
  而 `stock.dat` 损坏时 `Stock::load` 失败会被**静默降级成空 Stock**——不报错、不 panic、
  侧车照常起来（`engine.rs:230-236`）。
- `AssetRec.genesis_consignment_hex` **只写不读**（写：`engine.rs:728`；读：只有测试），
  且 `issue_usdt` 只打印 `asset_id` 不打印 genesis hex（`bin/issue_usdt.rs:196`）。

---

## 1. 部署拓扑（先说清楚东西在哪，否则表的路径没有意义）

`cmd/ci/docker-compose.yml` + `docker-compose-rgb20.yml`（**CI 拓扑**；生产拓扑未定稿，
差异会直接改变风险分级，见 §2.5 与 §6 ❓3）：

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
| `/data/stock/stash.dat` | `MemStash`：schemata / **geneses** / bundles / witnesses / **secret_seals** / type_system / libs（✅ rgb-ops `src/persistence/memory.rs:80-92`） | **非原子**：`fs::File::create` + 流式 encode（✅ rgb-strict-encoding `src/traits.rs:397-406`） |
| `/data/stock/state.dat` | `MemState`：witnesses / invalid_ops / **contracts（合约状态本体）**（✅ 同上 `:289-297`） | 同上，非原子 |
| `/data/stock/index.dat` | `MemIndex`：op↔bundle↔contract 索引 / **terminal_index（SecretSeal → Opout）**（✅ 同上 `:913-925`） | 同上，非原子 |
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
  （⚠️ 推断：生产拓扑未定稿）。

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
| **由谁持有** | **每个 para 的运维方各自持有自己的那份**，且**不得三份同处**（当前 `threshold=3`，3 份 = 直接可花，✅ `LAUNCH_READINESS.md:70`）。⚠️ 建议至少一份离线冷备 |

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
  自检会放过（⚠️ 推断：`groupPubKeyFromDKG` 只解群公钥字段）。**没有「share 可用性」的自检**（缺口 G2）。

### 2.2 B2 · 侧车 `/data`（全桥唯一一份）

| 项 | 内容 |
|---|---|
| **存放位置** | 卷 `rgb-sidecar-data` → `/data`（`RGB_SIDECAR_DATA_DIR`）。文件清单见 §1.2 |
| **丢失后果** | **不可逆**。RGB 是客户端验证：链上**只有**金额/承诺，没有资产状态 ⇒ 侧车状态**不可从链重建**。丢了以后：① 桥不知道任何 seal 的归属与余额；② 提现无法构造（`build_transfer` 要 `asset(symbol)` + Stock 里的合约状态）；③ 用户手上的 consignment 仍有效，但桥这边对不上账 |
| **必须备份** | **P0**。且**必须成套**（`ledger.json` + `stock/` + `builds/` + `user_scripts.json`）—— ⚠️ 单独拿一份没有意义，因为 `stock/*` 与 `ledger.json` 之间有跨文件一致性（§4 G4） |
| **建议方式 / 频率** | 状态随每笔充值/提现变更 ⇒ **每日 + 每次大额操作前**；若做在线快照须先解决 G4 |
| **由谁持有** | 桥上运维方。因为是单点，**必须异地复制**（⚠️ 建议 3-2-1：本地 + 异地，至少一份离线） |

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
  但那份文件本身**不在 `/data` 里、也没被任何东西备份**（缺口 G3）——抄一遍配置能救，前提是它还在。

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
| **丢失后果** | **生日丢失**。钱包生日 = `btcwallet.db` 创建时刻，neutrino 的 rescan 用 `StartTime=生日` 截断 ⇒ **生日之前的 BTC 块不会被下载 filter ⇒ 生日之前的充值永远看不见**。⚠️ 且旧 TSS UTXO 对钱包不可见，那部分 BTC 桥上花不出去。**没有任何配置能改变这一点**（✅ `CONFIG.md:259-266,659`） |
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

> ✅ **已确认（用户 2026-09-21）**：**生产同样是单主节点** ⇒ `main-data` 与 B1/B2 同属
> 「不可重建」一档，必须备份。上面的「主链 ≥2 个独立节点」仍是有价值的**拓扑级加固建议**，
> 但它是备份的补充、不是替代。好消息：CI 与生产的拓扑一致，备份方案**不需要分两套**。

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

**今天缺什么**：②③ 全靠人工开 bbolt 文件比对，**没有工具**（缺口 G1）。

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
     - 声明式资产：运维的 RGB_SIDECAR_CONTRACTS 文件里还有（前提：它被备份了 —— 缺口 G3）
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

### 3.4 恢复后的自检清单（今天能做的）

| 检查 | 怎么做 | 谁做 |
|---|---|---|
| share 与链同代 | 启动日志无 panic + `CONFIG.md §4.3.1` 的 fail-closed | 自动 |
| share 自检覆盖不到的「share 完好性」 | **没有自动检查**（缺口 G2） | ❌ 无 |
| 侧车账 ↔ 链上供应量对账 | 人工：`chain33-cli` 查 `opsupply-` vs 侧车 `GetBalance` | 人工 |
| 侧车 Stock ↔ ledger 一致 | **没有检查**（缺口 G4） | ❌ 无 |
| `btcwallet.db` 生日是否覆盖历史 | **没有检查**（缺口 G7） | ❌ 无 |

---

## 4. 缺口清单

按「会不会导致资金不可逆损失」排序。工作量是**粗估**（人日，含测试与一次 E2E 验证）。

| # | 缺口 | 后果 | 粗估 | 依赖 |
|---|---|---|---|---|
| **G0** | **`genesis_consignment_hex` 只写不读 + 无 Stock 导出入口**；自发行资产的 genesis 无处可寻 | 侧车 `/data` 全丢 = **合约认知永久丢失**，不可逆 | **1–2 人日**（加一个 `ExportContract` RPC 或 CLI：`Stock::export_contract` 已有库函数）+ 0.5 人日（`issue_usdt` 把 genesis hex 落盘 / 打印） | 侧车 proto 改动 |
| **G1** | **没有「从 `neutrino.db` 单独导出/导入 `rgbx-tss`」的工具** | 备份只能整文件（GB 级），运维会因此**不做备份**；恢复要人工开 bbolt 比对代次 | **2–3 人日**（新增 CLI：`rgbx tss-export --db <neutrino.db> --out <file>` / `tss-import`；bbolt 单 bucket 读写 + 群公钥/rid 打印） | chain33/plugin CLI 侧 |
| **G2** | **`stock/*.dat` 写非原子 + 载入失败静默降级成空 Stock** | 崩溃/磁盘满 → `stock.dat` 截断 → 下次启动**静默空 Stock**（不报错），且 `apply_declared_contracts` 因 ledger 已登记而**提前 return、不重新 import**（✅ `engine.rs:683-685`）⇒ 服务照常启动、余额（读 ledger）看着正常，但提现/结算在用到 Stock 时才失败 | **1 人日**（① `stock.store()` 前 tmp+rename+fsync；② `Stock::load` 失败**必须 fail-closed**，不允许静默 in-memory） | 侧车（`engine.rs`/`config.rs`） |
| **G3** | **`RGB_SIDECAR_CONTRACTS` 声明文件不在 `/data`、无备份约定** | 丢了它 + 丢了 `/data` ⇒ 连「重新 adopt 声明式合约」这条不完整的路都断了 | **0.5 人日**（约定：声明文件放进 `/data`，或纳入备份清单 + 在 `CONFIG.md` 写明） | 无 |
| **G4** | **`/data` 无一致性快照点**：`ledger.json` 原子写、`stock/*` 非原子写、`btcwallet.db` 是另一个文件 ⇒ 在线 `tar` 可能拿到**撕裂的组合** | 恢复出一份互相矛盾的备份，且**没有工具能发现** | **2–3 人日**（最小方案：加 `Snapshot` RPC —— 在 engine 全局锁内把 `/data` 打成一致 tar / 或 `Quiesce` 后由外部拷贝）；**或** 0.5 人日（文档化「备份前必须停侧车」，代价是停机窗口） | 侧车 |
| **G5** | **dapp localdb 无重建入口** | localdb 丢 ⇒ 掉队停机，只能等一次带版本升级的发布或临时开逃生阀 | （已在 `CONFIG.md:56-66` 记为后续项，**不在本轮范围**） | — |
| ~~G6~~ | ~~生产主链拓扑未确认~~ —— **已确认（用户 2026-09-21）：生产同为单主节点** | ⇒ `main-data` 丢 = 链丢 = 全部合约状态丢，**与 B1/B2 同级、必须备份** | 已决策，不再是缺口；且 CI 与生产拓扑一致，备份方案无需分两套 | 已决策 |
| **G7** | **`btcwallet.db` 生日没有自检** | 从旧备份恢复后可能「看不见」某些历史充值，**静默** | **1 人日**（启动时打印生日 + 与 neutrino 的 earliest 比对，不匹配则 WARN/拒启） | 中继 |
| **G8** | **无恢复演练**：CI 的 `scenario_restart_recovery` 已被屏蔽且不覆盖 TSS/侧车状态 | 备份从没被验证过 = 等于没有备份 | **2–3 人日**（新增 scenario：清 `/data` → 还原 → 跑一笔充值+提现；清一个 para 的 neutrino.db → 还原 → 参与一次签名） | harness |

**合计**：G0–G4 + G7 ≈ **7–10 人日**（不含 G5/G6）；加 G8 演练 ≈ **9–13 人日**。
→ 与 `LAUNCH_READINESS.md` 的「最小备份/恢复入口」（任务 #30）是同一批工作，**建议合并成一个批次**。

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
| `main-data`（**若生产仍是单主节点**） | 卷 `main-data` | 链丢 = 全部合约状态丢（G6 待确认） |

### 🟡 只是麻烦（可自愈 / 可重建 / 服务降级）

| 项 | 恢复方式 |
|---|---|
| `rgb20-deposit-sig` | 自动重签（✅ `CONFIG.md:481-484`） |
| `rgbx-deposit-scripts` / 侧车 `user_scripts.json` | 用户再要一次地址即重建（纯函数派生，✅ `deposit_address.go:191`） |
| `min-pending-height` | 从钱包最早点重放（✅ `btcwallet.go:395-413`） |
| dapp **localdb** | ⚠️ 名义上可从链重放，但**今天没有入口**（G5）⇒ 实际是「等一次版本升级」 |
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

## 6. 本轮**未确认**、不得当结论引用的项

| # | 未确认的事 | 影响 |
|---|---|---|
| ❓1 | **门限语义**：`threshold=3` + 4 节点的**确切数学含义**。`LAUNCH_READINESS.md:60-70` 已指出「rank 决定插值结构，不是必需性」，且 alice 只校验「参与方个数 ≥ threshold」。⇒ 「丢 1 份是否仍可签」「哪几份组合能签」**我没有独立验证**（该结论引自 `LAUNCH_READINESS.md`，其中引用的 `crypto/birkhoffinterpolation/birkhoffinterpolation.go:188` **不在本地 chain33 检出里** —— 本轮未复核到源码） | 若「任意 3 份即可签」，则**3 份备份同处 = 单点**（🔴）；若语义更严，分级会变 |
| ❓2 | 「权重 0 表示必须参与」的说法来自哪一版设计 | 同上；本轮按**最保守假设**（任意份额丢失都可能致命）设计 |
| ❓3 | 生产拓扑（几个主链节点？侧车是否单实例？是否共用一个 btcd？） | 直接决定 B4 / B2 的风险等级 |
| ❓4 | `adopt_contract` 在「Stock 空但 ledger 有 asset」时的最终可操作状态（§3.2 步骤③）—— 我只确认了它会**提前 return 不 import**（`engine.rs:683-685`），没跑过 | 决定 G2 的严重度 |
| ❓5 | `MemState`/`MemStash` 各字段对「哪些操作会因此失效」的精确映射 | 只影响表述精度，不影响分级 |
| ❓6 | `user_scripts.json` 的写是否原子 | 只影响它自己的损坏概率 |
| ❓7 | 侧车 `stock.store()` 是否在**全局 engine 锁**内（若在，快照 RPC 的实现风险更低） | 影响 G4 的实现方式 |
| ❓8 | `Stock::load` 对「文件不存在」与「文件损坏」是否返回同一种 `Err`（`engine.rs:230` 的 `Err(_)` 一视同仁） | 影响 G2 的修法（是否要区分首次启动与损坏） |

---

## 附：与既有文档的关系

| 文档 | 关系 |
|---|---|
| `CONFIG.md` §4.3.1–4.3.3 | **详述** share↔链自检、CGGMP 两份材料、refresh 语义。本文**不重复**，只补运维面（备份范围/频率/持有者/恢复步骤） |
| `CONFIG.md` §4.1.2 | `btcwallet.db` 生日与「只能从备份还原」的**权威说明** |
| `LAUNCH_READINESS.md` | 本文是「二、上线前必须」第 1 项的产出；第 2 项「最小备份/恢复入口」= 本文第 4 节的缺口 G0–G4 |
| `TECHNICAL.md` | 侧车/中继的实现细节 |
| plan `reactive-brewing-sedgewick.md`（2026-09-14 督查结论） | 那次盘点的**部分结论已过时**，本文以代码为准修正了两处：① TSS 是**两条**记录（`cggmp-dkg-result` + `cggmp-refresh-result`），不是一条 `dkg-result`；② 「仓内两份侧车源码已分化」**已不复存在**（全仓只有一个 `neutrino/rgb-sidecar/`） |
