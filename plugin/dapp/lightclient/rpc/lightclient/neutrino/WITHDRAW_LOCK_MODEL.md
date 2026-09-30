# 提现「锁定模型」设计文档

分支：`feat-rgb-usdt-integration`　　仓库：`33cn/plugin`
本文只做设计，不含任何代码改动。所有事实均标注 `file:line`，全部来自静态阅读（见文末「覆盖边界」）。

---

## 0. 前提修正：锁定模型已经建了一半，缺的是「解锁」

任务描述里的前提是「现在 `rgbx withdraw` 是**直接销毁**」。**这条与代码不符**，必须先纠正，否则后面所有设计都会建在错的起点上。

实际情况：

- 用户提交 `rgbx withdraw` 时，链上**不销毁**，而是把资产从 `tx.From()` **转进一个锁定地址**（`crosschain.go:166`，`accDB.Transfer(tx.From(), lockAddr, withdraw.GetAmount())`）。
- 锁定地址是确定性的：`accDB.ExecAddress(RgbxX + "-crosschain-lock")`（`account.go:12-14`）。
- 真正的销毁发生在**结算**时：桥提交 `Confirm` 之后，`confirmWithdrawSettlement` 从锁定地址 `Burn`（`exec.go:256-257`）。

所以今天的形态是：**锁定是单向的**。资产确实离开了用户余额（用户视角就是「没了」），但它躺在锁定地址里，而**从锁定地址出来的路只有一条 —— `Burn`**。

全仓 grep 锁定地址的引用（`crosschain.go:150,166`、`exec.go:256,257`、以及测试），除 `Burn` 外没有任何一条把额度转回用户的路；全仓 grep `Unlock|Refund|Cancel|abandon` 在 rgbx 与 lightclient 的生产代码里**零命中**（命中的全是 `sync.RWMutex.Unlock`）。

因此本任务的准确定义是：

> **不是"从销毁改成锁定"，而是"给已经存在的锁定补上解锁这条路"，并把"什么时候允许解锁"这个判据做成可判定的。**

这个修正对设计有实质影响：状态机、防重放键、台账语义、E2E 断言全都要围绕「锁定地址的额度只能被 `Burn` 消耗一次」这条既有不变式来写，而不是从零引入一个锁。

---

## 1. 现状：一笔提现的完整生命周期

### 1.1 链上（chain33 / rgbx 执行器）

| 步骤 | 位置 | 做什么 | 落在哪 |
|---|---|---|---|
| ① 提交 | `checktx.go:335` `checkWithdraw` | 先验检查（见 §5 表） | 无 |
| ② 锁定 | `crosschain.go:142` `Exec_Withdraw` | 派生 `BurnOperationID(symbol, chain33TxHash)`（`crosschain.go:154`） | — |
| ③ 记台账 | `crosschain.go:160` → `operation.go:104-141` `recordBurnOperation` | 写不可变记录 + 下标 + 供应量索引（**不动 `burned`**，`operation.go:110-112`） | stateDB（共识） |
| ④ 转账 | `crosschain.go:166` | `tx.From()` → 锁定地址 | stateDB（账户余额） |
| ⑤ 存 payload | `crosschain.go:176-179` | `formatPayloadKey(txHash)` = 整笔 `WithdrawAsset` | stateDB（共识） |
| ⑥ 发 pending 日志 | `crosschain.go:180-194` | `TyPendingTxLog` + `PendingTx{...}` | 回执 log |
| ⑦ 落 pending | `exec_local.go:50-73` `ExecLocal_Withdraw` | 由 ⑥ 的 log 写 pendingTx + 按 from 建索引 | **LocalDB（节点私有）** |
| ⑧ 桥拉取 | `rgbx.go:109-170` `pullPendingTx`（每 10s） | `Query_ListPendingTx` → `withdrawReqChan`（`rgbx.go:142-145`） | — |
| ⑨ 桥构造+签名+广播 | 见 §1.2 | — | 桥本地 bolt（`neutrino.db`） |
| ⑩ 桥提交确认 | `bitcoin.go:716-728` `commitWithdrawConfirm` | 提交 `ConfirmTx{ActionType: TyWithdrawAsset}` | chain33 交易 |
| ⑪ 确认校验 | `checktx.go:495` `checkConfirm` → `validate_proof.go:67` `checkWithdrawConfirm` | S3 去重 → SPV 证明 → （非 RGB20）OP_RETURN 承诺 + 金额脚本 → 台账 → 确认深度 | — |
| ⑫ 销毁 | `exec.go:221` `confirmWithdrawSettlement` | 复验台账（`exec.go:238-251`）→ **从锁定地址 Burn**（`exec.go:256-257`）→ 写已消费键（`exec.go:265-268`）→ `burned += amount`（`exec.go:271`） | stateDB（共识） |

关键点：

- **⑤ 的 payload 存在共识状态**，⑫ 靠它拿到金额（`exec.go:222-226`）；`validate_proof.go:103-108` 也读同一份。所以「这笔提现的金额」在锁定时就被钉死，结算时不可篡改 —— 这是 `S2` 台账设计的原意（`operation.go:104-112`、`operation.go:162-174`）。
- **⑦ 的 pendingTx 在 LocalDB**，`checkConfirm` 读的就是它（`checktx.go:508`）。**任何新的解锁判据都不能依赖它**（`validate_proof.go:68-72` 已确立这条原则：CheckTx 在出块执行阶段被每个节点重跑，读节点私有数据会让"区块是否合法"依赖各节点本地库）。
- **⑪ 显式拒绝 `Timeout`**（`checktx.go:541-546`，错误码 `ErrWithdrawConfirmTimeoutNotAllowed`，`checktx.go:63`），且 `Exec_Confirm` 对 `Timeout` 直接返回空回执（`exec.go:142-144`）。也就是说**今天提现这条路上完全没有"超时即处理"的机制** —— 这正是问题所在：桥不推进，资产永远躺在锁定地址里。

### 1.2 桥侧（neutrino）

- 主循环 `withdrawalProcessor`（`bitcoin.go:636-714`），三类队列（BTC 提现 / RGB20 提现 / confirm）在同一个 `retryTicker` 上重试（`bitcoin.go:650-671`）。
- **BTC 提现**：`processWithdrawRequest`（`bitcoin.go:341-411`）
  - 构建 → 选 UTXO 并锁定 → 首次写 `stickyUTXO`（`bitcoin.go:371-377`，之后不可改）
  - 自校验 → `processSignBtcTx` → 广播（`bitcoin.go:390`）
  - 广播后落盘 `withdrawStatusSent`（`bitcoin.go:405`）
  - 已有快照非空但 sticky 为空 ⇒ 直接跳过重试（`bitcoin.go:705-708`，fail-closed）
- **RGB20 提现**：`retryRgb20Withdraw`（`bitcoin.go:617-633`）→ `rgb20.Withdraw`（`rgb20/withdraw.go:255-391`）
  - `BuildWithdrawal`（侧车）→ `ValidateWithdrawPsbt` 交叉核对 → `persistStickySeal`（`rgb20/withdraw.go:330`）→ `SignPsbt`（TSS）→ `FinalizeWithdrawal` → txid↔pending 映射 → `BroadcastTx`（`rgb20/withdraw.go:380`）
  - `BroadcastTx` 成功后落盘 `withdrawStatusSent`（`rgb20withdraw.go:198-203`）
- **不可恢复的失败**：`retryRgb20Withdraw` 把 `withdrawStatusUnrecoverable` 落盘并**停止重试**（`bitcoin.go:623-630`）。注释明确写死："链上 pending 不动 —— 桥不做自动退款，已锁仓资产如何处置属产品决策"（`bitcoin.go:615-616`；`rgb20/withdraw.go:110-112` 同义）。**这就是本任务要解决的那个洞。**
- 桥本地状态桶（全部在 `n.neutrinoCfg.Database`，即 `neutrino.db`，`bitcoin.go:434`）：
  - `rgbx-withdraw-state`：`broadcasted` / `confirmed` / `unrecoverable`（`bitcoin.go:419-425`）
  - `rgbx-withdraw-sticky-utxo`（`bitcoin.go:426`）
  - `rgb20-withdraw-sticky-seal` / `rgb20-withdraw-txid`（`rgb20/withdraw.go:22-24`）
  - **这些全是节点私有的，不是共识状态，且不随 chain33 重组回滚。**

### 1.3 TSS 签名轮次（门限）

- 协调者发起：`signPsbt`（`tss.go:859-900`）→ 挑签名者 → 广播 `rgb20-withdraw` 签名通知（带完整提现上下文，`tss.go:879-886`）→ `signPsbtWithSigners` → 每个输入一次 `signMsg`。
- `signMsg`（`tss.go:750-775`）直接调 `cggmp.ProcessSign(signers, t.threshold, msg, dkgResult, refreshResult, sessionName)`（`tss.go:758`）。
- 签名者选取：`waitForSufficientSigners`（`tss.go:624-631`）→ `pickSigners`（`tss.go:642-658`）→ `selectSignerCombination`（`tss.go:668-686`），**返回恰好 `threshold` 个**，并要求 Birkhoff rank 升序满足 `rank_i <= i`（`tss.go:694-700` 的 `validateSignerCombination`）。

**门限数核实**（任务里要求核 `TSS_THRESHOLD`）：

- 代码里**没有** `TSS_THRESHOLD` 这个 Go 常量；门限是配置项 `Threshold uint32`（`config.go:182`）。
- 部署形态是 **4 节点 + threshold = 3**，rank = `{0,1,1,1}`（`config.go:177-186`、`tss.go:259-262`、`tss.go:660-667`）。
- CI 里 `TSS_THRESHOLD` 是 shell 变量，默认 3，渲染进 TSS 配置：`plugin/dapp/rgbx/cmd/ci/docker-compose.sh:98`（`TSS_THRESHOLD="${TSS_THRESHOLD:-3}"`）与 `:333`（`threshold=${TSS_THRESHOLD}`）。

**结论：门限 = 3/4，主会话结论 3 里"部分签名 1–2 份无威胁"成立。**

---

## 2. 目标状态机

### 2.1 状态

一笔提现（以 **chain33 burn 交易哈希** 为唯一身份，也就是 `BurnOperationID` 的原像之一，`types/operation.go:87`）落在一个终态三选一上：

| 状态 | 含义 | 资产在哪 | 能否再变 |
|---|---|---|---|
| `Locked` | 已锁定，等待桥推进 | 锁定地址 | 能 |
| `Settled` | 已销毁放款 | 已销毁（`burned` 已累加） | **终态** |
| `Released` | 已解锁退回 | 已回到原主 | **终态** |

### 2.2 允许的迁移

| from | to | 触发者 | 授权要求 | 链上判据 |
|---|---|---|---|---|
| — | `Locked` | 用户本人 | 用户签名 + `checkWithdraw` 全部先验检查 | `checktx.go:335-427` |
| `Locked` | `Settled` | 桥（任何人可提交，但…） | `tx.From() == rgbxCfg.CommitAddress`（`checktx.go:500-505`）+ 有效 SPV/merkle 证明 + OP_RETURN 承诺（非 RGB20）+ 金额脚本（非 RGB20）+ 台账一致 + 确认深度 | `validate_proof.go:67-114` |
| `Locked` | `Released` | **待定，见 §8-Q1** | **待定** | **待新增** |

### 2.3 禁止的迁移

- `Settled → Released`、`Released → Settled`：两者互斥，必须由同一个终态键挡住（见 §4）。
- `Locked → Locked`：同一 burn 哈希不可能被 `Exec_Withdraw` 执行两次（交易身份唯一）。
- **`Locked → Released` 不得由「超时」单独触发**。`Confirm.Timeout` 这条路已经被显式关掉（`checktx.go:541-546`），解锁若复用"超时即退"等于把它从后门打开，与主会话结论 1/2 冲突。

---

## 3. 「签名是否达成」这个判据怎么落地

### 3.1 这个事实今天存在哪：**只存在于进程内，重启即丢，且不可被第三方验证**

逐条核实：

| 相关事实 | 存放位置 | 落盘？ | 重启后？ | 第三方可验证？ |
|---|---|---|---|---|
| 本轮选了哪些签名节点 | `tssService.signRoundSigners`，**内存 map**（定义 `tss.go:205-214`；写 `tss.go:1010-1014`；读 `tss.go:1017-1021`） | ❌ | ❌ 丢 | ❌ |
| 确定性拒签回执 | `tssService.signRejections`，**内存 map**（定义 `tss.go:210-213`；读 `tss.go:1024-1031`） | ❌ | ❌ 丢 | ❌ |
| 组签名的**产物** | `signResult.sig`（`tss.go:179-182`），函数局部 → 返回给 `Adapter.Withdraw`（`rgb20/withdraw.go:338`）→ Finalize（`rgb20/withdraw.go:356`）→ 广播（`rgb20/withdraw.go:380`） | ❌ **从不落盘** | ❌ | ❌ |
| 「桥已经推进到广播」 | `withdrawStatusSent`（常量 `bitcoin.go:421`；写 BTC 侧 `bitcoin.go:405`、RGB20 侧 `rgb20withdraw.go:199`；读 `bitcoin.go:432-446`） | ✅ bolt（`neutrino.db`） | ✅ | ❌ **节点私有** |
| 「链上是否已结算」 | `formatWithdrawUsedKey(burnTxHash)`（`kv.go:77-80`；写 `exec.go:265-268`；读 `validate_proof.go:73-78`） | ✅ stateDB | ✅ | ✅（共识） |
| 「链上是否登记过这笔提现」 | 台账记录 `formatOperationRecordKey(opID)`（`kv.go:121-123`；写 `operation.go:56-65`） | ✅ stateDB | ✅ | ✅（共识） |

**链上完全没有"这笔提现签过名"的任何痕迹。** 链上与提现相关的共识状态只有三样：payload（`crosschain.go:176`）、台账记录（`operation.go:104`）、已结算键（`exec.go:265`）。

注意 `withdrawStatusSent` 的**位置很尴尬**：它写在**广播成功之后**，也就是说「签名已达成但广播失败/还没广播」这个窗口里它是**空**的。而 `rgb20.Withdraw` 里的那条护栏（`rgb20/withdraw.go:282-287`）恰恰把「state 非空但 sticky 为空」当成致命不一致处理 —— 说明作者也认为「有 state 就一定签过」，但反向（「无 state 就没签过」）**不成立**。

### 3.2 结论：让它变成可判定，只有一条路 —— 把它做成一次门限签名的**产物**

「签名达成了」**可以**被证明（出示签名即可验证）；「签名**没有**达成」**无法**被证明（否认事实，要穷举所有可能的签名轮次）。这与主会话结论 1 是同一个道理，只是换到了签名这一层。

所以解锁条件**不能**定义成「证明桥没签名」，而必须定义成：

> **门限组对「放弃这笔提现」这件事本身出具一次签名。出示不出这个签名，就不允许解锁。**

这样：
- 「放弃」需要一个 3/4 门限签名；
- 「付款」需要同一个 3/4 门限签名（`signMsg` 用同一把 DKG 群钥，`tss.go:758` → `tss.go:765-771`）；
- 二者绑在同一笔 pending 上 ⇒ **单节点两边都做不了**（这是主会话结论 3 的落地形态）。

### 3.3 链上已经有现成的验证原语，不需要新造

充值侧早就实现了「链上验一个 TSS 门限签名」：

```go
// threshold.go:33-58
func verifyThresholdSig(pub []byte, dep *rtypes.DepositAsset) error {
    ...
    pubKey, err := btcec.ParsePubKey(pub)          // pub = CrossChainInfo.Pubkey
    sig, err := ecdsa.ParseDERSignature(dep.GetThresholdSig())
    msg := computeDepositSignMessage(dep)          // threshold.go:23-29
    if !sig.Verify(msg, pubKey) { return ErrInvalidThresholdSig }
    ...
}
```

配套事实：

- 群公钥存在共识状态：`Exec_CommitDKG` 写 `CrossChainInfo.Pubkey`（`crosschain.go:52-64`，字段在 `:59`）。
- **所有 symbol 都强制带公钥**，包括 BTC：`checkCommitDKG` 的注释与实现明确写死（`checktx.go:296-302`：*"所有 symbol（含 BTC/XBTC）都必须提交 TSS 组公钥，并校验 hash160(pubkey) == pkScript[2:]，… 原 RGB20 分支 BL-1 提升为全局"*），实现见 `checktx.go:304-313`（`ParseDepositTssPubKey` + `pkScript[2:]` 逐字节比对，无 symbol 分支）。
- 消息口径是 `sha256(types.Encode(X{thresholdSig: nil}))`（`threshold.go:20-29`），桥侧同形实现见 `tss.go:839-846`。

**所以「放弃证书」可以直接复用这条路径**：链上按同一套口径算消息、用同一把公钥验签。BTC 与 RGB20 两条提现路径都能用。

### 3.4 需要新增什么（具体到字段）

**需要新增的**：

1. 一个 proto 消息（`plugin/dapp/rgbx/proto/rgbx.proto`），语义 = 一笔**放弃授权**：
   ```
   message UnlockAsset {
       string asset_symbol       = 1;  // 与 withdraw 同一归一化口径
       bytes  chain33_tx_hash    = 2;  // burn 的 chain33 哈希（唯一身份）
       int64  amount             = 3;  // 必须等于锁定时写下的台账金额
       bytes  threshold_sig      = 4;  // 门限组对 (1)(2)(3) 的签名；进消息原像时置空
   }
   ```
   消息原像口径与充值同形：`sha256(types.Encode(&UnlockAsset{..., ThresholdSig: nil}))`。

2. 一个 action 类型 `TyUnlockAsset` + `NameUnlockAssetAction = "Unlock"` + `ActionName` 映射（现表在 `types/rgbx.go:26-62`，`GetActionName` 在 `:116-134`）。

3. **一个终态键**（共识状态）—— 这是防重放的核心，见 §4。

**不需要新增的**：

- **不需要**新增"是否已签名"的状态字段。签名本身就是判据，链上验签即可，不需要在链上维护一个"签没签过"的布尔量（那个量链上本来就拿不到真值）。
- **不需要**动 `CrossChainInfo` 的结构（公钥已经在里面了）。
- **不需要**动 `PendingTx` / localdb 结构。

### 3.5 反向的一半（"已达成则不可放弃"）在链上不可判，只能靠桥侧策略

「付款签名已达成 ⇒ 不可放弃」这一半，链上做不到：付款签名存在与否，链上无从知道（桥可以把签名藏起来）。

可落地的策略护栏：

- 新增签名轮次类型 `rgb20-withdraw-abandon`（现有类型定义见 `tss.go:37-48`，`transactionTypeRgb20Withdraw` / `...Reject` / `...TestSign`）。
- 签名节点在参与放弃签名**之前**，先查自己的 `WithdrawState(burnHash)`（`rgb20withdraw.go:106-110` → `bitcoin.go:432-446`）：**非空（= `broadcasted`）即拒签放弃**。理由是广播意味着签名已达成且付款可能已在路上。
- 这条是**策略**（各桥节点本地行为），不是共识判据 ⇒ 它挡的是"诚实节点被误导"，挡不住"3/4 节点合谋"（合谋超出整个桥的信任假设，可接受）。

**残余风险见 §8-Q2**：桥侧失联/作恶时，用户仍可能拿到一个合法的放弃证书，而桥手里同时握着一个有效的付款签名。这是本设计最大的未闭合面。

---

## 4. 防重放：同一笔 pending 不能既解锁又销毁

### 4.1 今天的机制（结算侧）

- `formatWithdrawUsedKey(burnTxHash)`（`kv.go:77-80`，`sha256` 定长化后拼前缀），写点在 `exec.go:265-268`（**只在结算成功时随回执 KV 写**），读点在 `validate_proof.go:73-78`。
- 读的语义是 fail-closed：**只有 `ErrNotFound` 才放行**（`validate_proof.go:74`：`if !errors.Is(err, types.ErrNotFound) { return ErrWithdrawAlreadyConfirmed }`）。
- 随状态库一起提交/回滚，所以重组后重放同一笔确认不会被误判（`kv.go:82-105` 的 E14 注释把这条原则写透了）。
- 台账记录另有第二层：`newOperationRecordKV` 只允许写一次，读到任何已存在的键就拒（`operation.go:56-65`），且**没有更新路径**。

### 4.2 竞态清单与结论

**C1 —— 同区块内 `Confirm` 与 `Unlock` 同时提交（最核心的一条）**

- chain33 在一个区块内**串行**执行交易，后一笔看得到前一笔写入的 stateDB。因此互斥是**可达成的**，但**前提是两边都查**。
- 只查一边就会漏：今天 `markConfirmUsed`（`exec.go:209-219`）**只写不查**，查在 CheckTx 的另一侧（`validate_proof.go:73`）。解锁如果只在 CheckTx 里查「已结算键」、而结算侧不查「已解锁键」，那么在一个块里 `Confirm` 排在 `Unlock` 后面时，`Confirm` 会照常销毁 —— 同一笔 burn 既解锁又销毁。
- **结论：必须两边互查。** 具体两种形态，见 §8-Q3。
- **建议（更稳）**：把两条终态收敛到**一把键**、**写一次**：
  ```
  formatWithdrawFinalKey(burnTxHash) -> "settled" | "released"
  ```
  第一笔写者赢，第二笔读到即拒。少一处漂移、语义单一、天然幂等。代价是动了既有的 `formatWithdrawUsedKey` 口径（本链未上线，`validate_proof.go:122-124` 与 `kv.go:110-113` 都已确立"不留兼容分支"的先例）。

**C2 —— 同一笔 `Unlock` 重复提交**

- 靠 C1 的终态键挡住：第二次读到键即拒（幂等键 = burn 哈希，天然唯一，不需要额外序号）。
- 链上也天然防一层：必须重新发一笔交易，而状态已经变了。

**C3 —— 时序竞态：解锁先上链，桥随后才广播付款**

- **链上不可判**（§3.2）。
- 后果：用户拿回锁定额度 **且** 收到 RGB/BTC 资产 = 桥净亏。
- 缓解只能在桥侧：签名节点拒签放弃（§3.5）。**残余风险必须让用户拍板**（§8-Q2）。

**C4 —— 重组（reorg）**

- 链上：所有终态键都在 stateDB ⇒ 随重组回滚 ⇒ "回滚后重放同一笔"不会被误判（既有原则，`kv.go:82-105`）。
- 桥侧：`withdrawStatusSent` / `confirmed` / sticky 都在 `neutrino.db`，**不随 chain33 回滚**。重组后链上回到 `Locked`，桥侧仍是 `broadcasted`。解锁引入后，这个既有偏差的新表现是"链上允许解锁、桥侧仍认为已付款"。
- **结论：不改结构，但要在运维文档里明确**，且 §3.5 的护栏按"桥侧状态偏保守"设计（sticky/state 非空一律不当成可放弃）。

**C5 —— 跨节点一致性**

- 终态键在共识状态 ⇒ CheckTx 各节点读到同一结论（`validate_proof.go:68-72` 已确立）。
- **解锁路径必须同样不读 LocalDB**，尤其不能读 `pendingTx`（那是节点私有的，`exec_local.go:50-73`）；金额与归属一律取共识状态里的台账记录（`operation.go:104-141`）与 payload（`crosschain.go:176`）。

**C6 —— `Timeout` 路径不得复用**

- `checkConfirm` 对 withdraw 显式拒绝 `Timeout`（`checktx.go:541-546`）；`Exec_Confirm` 对 `Timeout` 直接返回空回执（`exec.go:142-144`）。
- 解锁**不能**挂在 `Confirm.Timeout` 上，否则等于从后门恢复"超时即退"，与主会话结论 1/2 冲突。**结论：解锁必须是独立的 action。**

**C7 —— 解锁金额与接收方**

- 解锁量必须**恰好等于锁定时登记的金额**，取台账记录的 `rec.Amount`（`operation.go:118-130`），不能从锁定地址余额反推（锁定地址服务多笔提现，余额是聚合量）。
- `confirmWithdrawSettlement` 已经是这个口径（`exec.go:244-251` 逐项比对 kind / symbol / amount），解锁照抄同一份判据即可，**复用 `checkWithdrawOperationLedger`（`operation.go:175-221`）是自然选择**。
- 接收方建议取 `rec.Owner`（= burn 时的 `tx.From()`，`operation.go:127`），**不接受调用方指定地址** —— 否则放弃证书的语义就从"退回"变成"授权转账"。

**C8 —— 锁定地址的额度守恒**

- 锁定地址是一个共享账户（`account.go:12-14` 是确定性地址，不带用户维度）。所以「这笔提现的额度」与「锁定地址的总余额」是两个量，任何解锁逻辑都必须用前者。这与 C7 是同一条，单列因为它是这个模型最容易写错的地方。

---

## 5. 失败分类表

三栏：**先验拒绝**（提交时就该拒，根本不该锁定）／**可放弃**（后验、但可判定为"未达成签名"）／**只能人工**。

| # | 失败 | 可判定性 | 今天的行为（file:line） | 目标行为 | 判据 |
|---|---|---|---|---|---|
| 1 | BTC 金额 < 546 sat | 先验 | 已拒（`checktx.go:369`） | 不变 | `minBtcWithdrawAmount`（`checktx.go:119`） |
| 2 | RGB20 金额 < 1 最小单位 | 先验 | 已拒（`checktx.go:343`） | 不变 | `minRgb20WithdrawAmount`（`checktx.go:121`） |
| 3 | 付款地址（BTC）无法解码 | 先验 | 已拒（`checktx.go:374-378`） | 不变 | `decodeBtcAddressScript` |
| 4 | RGB20 invoice 为空 | 先验 | 已拒（`checktx.go:347-350`） | 不变 | — |
| 5 | **RGB20 invoice 语法/网络非法** | 先验（理论上） | **链上不判**（只查非空）。桥侧 `BuildWithdrawal` 失败后经 `classifyWithdrawSidecarError`（`rgb20/withdraw.go:178-200`）归 `permanent` → 最终 `withdrawStatusUnrecoverable`（`bitcoin.go:623-630`） | 提交时就拒，**不该锁定** | **今天链上没有解析能力**（解析器只在 Rust 侧车：`rgb-sidecar/src/invoice.rs:57`）→ 见 §8-Q4 |
| 6 | **RGB20 合约身份不符（asset_id ≠ 配置）** | 先验（理论上） | 桥侧 `asset-contract-mismatch`（`rgb20/withdraw.go:188`，判定依据 `classifyWithdrawSidecarError`）→ 不可恢复 | 提交时就拒 | 链上 `CrossChainInfo` **只有 pubkey、没有 asset_id** ⇒ 需要新增字段，见 §8-Q4 |
| 7 | feeRate 越界 | 先验 | 已拒（BTC `checktx.go:379`；RGB20 `checktx.go:351`） | 不变 | `maxBtcFeeRate = 1000`（`checktx.go:118`） |
| 8 | 余额不足 | 先验 | 已拒（BTC `checktx.go:423`；RGB20 `checktx.go:361`） | 不变 | — |
| 9 | 提现目标 = 自己的充值脚本 | 先验 | 已拒（`checktx.go:411-416`，E15-a） | 不变 | `rtypes.IsDepositPkScript` |
| 10 | 侧车永久错误：资产未发行 / 合约不符 / 账本重建后 asset_id 变化 | 后验，**可判** | 归 `UnrecoverableWithdrawError`（`rgb20/withdraw.go:105-133`，标记来源 `:147-170`）→ 停重试、留链上 pending（`bitcoin.go:617-633`） | **可放弃** | `IsUnrecoverableWithdraw`（`rgb20/withdraw.go:127-133`）+ 签名未达成 |
| 11 | sticky seal 不一致（E9） | 后验，**可判** | 归 `sticky-seal-mismatch` → 不可恢复（`rgb20/withdraw.go:172-176`、`:803-822`） | **可放弃**（但这是防双付护栏，放弃前必须确认该 seal 集合确实没被签过） | 同上 |
| 12 | 组签名凑不齐（< 3 个节点在线 / 候选不足） | 后验，**可判** | 可重试、无限等（`tss.go:624-631` 每 3s 一轮；`selectSignerCombination` 凑不出返回 nil，`tss.go:668-671`） | **可放弃**（签名未达成） | 门限判据 |
| 13 | 组签名已达成，但 Finalize / 提取失败 | 后验 | 可重试（`rgb20/withdraw.go:356-359`） | **不可放弃**（签名已达成） | 门限判据 |
| 14 | 广播失败（网络 / 被拒） | 后验 | 可重试；若节点已知该 txid 则算成功（`rgb20withdraw.go:133-141`，`rgb20withdraw.go:174-185`） | 重播 / 替换；都无效才人工 | 桥侧 |
| 15 | 广播成功但长时间不上链 | 后验 | 可重试 | 重播（RBF/替换费率不在本设计范围） | 桥侧 |
| 16 | BTC 确认数不足 / 确认延迟 | 后验 | 可重试，`ErrInsufficientBtcConfirmations`（`validate_proof.go:385-415`） | 只能等；**不可放弃** | 门限 + 桥侧广播状态 |
| 17 | 桥侧数据丢失（`neutrino.db` 被清） | 后验 | sticky 为空 + state 非空 ⇒ 停下（`rgb20/withdraw.go:282-287`）；BTC 侧同样跳过（`bitcoin.go:705-708`） | **只能人工** | 桥侧 |
| 18 | 桥整体失联 / 全部节点下线 | 后验 | 卡住 | **只能人工**（或由 Q1 的授权形态决定） | — |

**分界线**：#1–#9 是**先验可判**的 —— 它们全部依赖"提交时就能拿到的信息"。今天链上只覆盖了 #1–#4、#7–#9；**#5、#6 是缺口**（见 §8-Q4）。
**#10–#12 是本设计要打通的那一段**（今天全部落在"不可恢复、不退款"这个死胡同里）。
**#16–#18 明确不自动退款**（与主会话结论 4 一致）。

---

## 6. 改动清单

### A. 链上（**动共识路径**）

| # | 文件 | 改动 |
|---|---|---|
| A1 | `plugin/dapp/rgbx/proto/rgbx.proto` | 新增 `UnlockAsset` 消息（§3.4 第 1 条） |
| A2 | `plugin/dapp/rgbx/types/rgbx.go` | 新增 `TyUnlockAsset` + `NameUnlockAssetAction = "Unlock"`，注册进 `ActionName` 表（现表 `:26-62`）与 `GetActionName`（`:116-134`） |
| A3 | `plugin/dapp/rgbx/executor/kv.go` | 新增终态键 `formatWithdrawFinalKey`（§4.2-C1 建议形态） |
| A4 | `plugin/dapp/rgbx/executor/crosschain.go` | 新增 `Exec_Unlock`：验门限签名 → 读台账 → 查终态键 → `Transfer(lockAddr, rec.Owner, rec.Amount)` → 写终态键 |
| A5 | `plugin/dapp/rgbx/executor/checktx.go` | 新增 `checkUnlock`；在 `CheckTx` 分派里加分支（现分派 `:135-151`） |
| A6 | `plugin/dapp/rgbx/executor/validate_proof.go` | `checkWithdrawConfirm` 增加"未解锁"检查（现函数 `:67-114`，去重段 `:68-78`） |
| A7 | `plugin/dapp/rgbx/executor/exec.go` | `confirmWithdrawSettlement` 改为写终态键（现 `:265-268`） |
| A8 | `plugin/dapp/rgbx/executor/query.go` | `isOperationSettled`（`:228-234`）纳入 `released` 终态；`OperationRecord` 的结算视图需要区分 settled / released（见文末自决项） |
| A9 | `plugin/dapp/rgbx/executor/threshold.go` | 复用 `verifyThresholdSig` 的 btcec 验签路径；为其抽出一个"通用消息"重载（现签名依赖 `DepositAsset` 类型，`:33-58`） |
| （可选）A10 | `proto` + `executor` | 若 §8-Q4 选 (a)：`CrossChainInfo` 增加 `asset_id`，`checkCommitDKG` 强制提交，`checkWithdraw` 增加 invoice 解析与合约比对 |

**A1–A9 全部落在共识路径上**（新 action、新共识键、CheckTx/Exec 判据变化）。因为本链未上线（`validate_proof.go:122-124`、`kv.go:110-113` 两处都明确写"不留兼容分支"），可以硬切，不引入 fork 高度。

### B. 桥侧（策略，不动共识）

| # | 文件 | 改动 |
|---|---|---|
| B1 | `neutrino/tss.go` | 新增签名通知类型 `transactionTypeRgb20WithdrawAbandon`（现类型定义 `:37-48`）+ 协调者侧 `signAbandon`（复用 `signMsg`，`:750-775`）+ 签名节点侧 handler（仿 `handleRgb20WithdrawSign`，`:1651-1678`） |
| B2 | `neutrino/tss.go` | 签名节点参与放弃签名前查 `WithdrawState`（`rgb20withdraw.go:106-110`），非空即拒签（§3.5） |
| B3 | `neutrino/bitcoin.go` | 终态集合新增 `withdrawStatusAbandonSigned` / `withdrawStatusReleased`（现集合 `:419-430`；读写函数 `:432-457`） |
| B4 | `neutrino/bitcoin.go` | `retryRgb20Withdraw`（`:617-633`）在 `unrecoverable` 分支后**发起放弃签名轮次**，而不是只落盘停住 |
| B5 | `neutrino/rgb20/withdraw.go` | `UnrecoverableWithdrawError`（`:105-133`）的处置语义从"停"扩成"停 + 可发起放弃授权" |
| B6 | 新增提交点 | 桥侧把 `UnlockAsset` 交易提交到 chain33（复用 `submitMainChainTx` 家族，如 `bitcoin.go:718`） |
| B7 | `neutrino/config.go` | 若放弃签名需要独立开关/超时配置，加在 `rgb20` 配置段 |

### C. harness / E2E

| # | 文件 | 改动 |
|---|---|---|
| C1 | `build/ci/rgbx-ci/scripts/rgb20_test.sh` | 新增「拒绝后解锁」场景（§7-1）；新增防重放与互斥负例（§7-3/4/5） |
| C2 | `build/ci/rgbx-ci/scripts/btc_test.sh` | 扩 `scenario_user_withdraw_auto_confirm`（`:197-224`）的断言：锁定地址余额归零 + 终态键 = settled |
| C3 | `build/ci/rgbx-ci/scripts/assertions.sh` | 若需要：加一个"锁定地址余额"断言（今天**没有锁定地址的查询入口**，可能需要新增 query 或直接用 CLI 查该地址的资产余额） |
| C4 | `build/ci/rgbx-ci/testcase.sh` | 把新场景挂进 `testcase_entry`（`:15-74`） |

---

## 7. E2E 用例清单

前置说明：锁定地址是 `ExecAddress(RgbxX + "-crosschain-lock")`（`account.go:12-14`）。**今天没有查询它的 RPC 入口**，用例 1/2/4/5 需要它的余额断言 —— 要么新增一个 query，要么用既有 `chain33-cli` 按地址查该 symbol 的余额（C3 待补这一条）。

| # | 用例 | 断言 |
|---|---|---|
| 1 | `scenario_unlock_after_rejection` | 构造一笔必然被侧车永久拒的提现（例如 invoice 的 asset_id 与配置不符）→ **断言**：a) 锁定发生（用户余额 −amount，锁定地址 +amount）；b) 桥归为不可恢复（`withdrawStatusUnrecoverable` 落盘）；c) 完成放弃签名并提交 `Unlock` 后，**用户余额回到提现前**、**锁定地址余额归零**；d) `Query_GetOperation.settled == false` 且终态 = released；e) `Query_GetOperationSupply` 的 `burned` **不变**（从未销毁）；f) `minted − burned` 回到提现前的值 |
| 2 | `scenario_withdraw_settle_then_burn`（在既有场景上扩断言） | 正常提现走完 → **断言**：a) 锁定地址余额归零；b) 终态 = settled；c) `burned += amount`；d) 用户余额 −amount |
| 3 | `scenario_duplicate_unlock_rejected` | 同一 burn 连提两次 `Unlock` → **断言**：第一笔成功；第二笔**交易失败**（预期错误码 `ErrWithdrawAlreadyReleased`），且**用户余额只恢复一次**、锁定地址余额为 0 |
| 4 | `scenario_unlock_after_settle_rejected` | 对**已结算**的 burn 提交 `Unlock` → **断言**：被拒（预期 `ErrWithdrawAlreadyConfirmed`），用户余额与锁定地址余额**都不变** |
| 5 | `scenario_settle_after_release_rejected` | 对**已解锁**的 burn 提交 `Confirm` → **断言**：被拒，锁定地址余额保持 0，`burned` **不增加**（关键：不能出现"已解锁的额度又被销毁一次"） |
| 6 | `scenario_unlock_signature_invalid` | 提交 `UnlockAsset` 但 `thresholdSig` 伪造/为空/被改过金额 → **断言**：被拒（`ErrInvalidThresholdSig` 家族），状态不变 |
| 7 | `scenario_broadcast_failure_then_retry` | 让一次广播失败（btcd 不可达或注入一个已知 txid）→ **断言**：a) 桥保持 `broadcasted` 前的重试状态；b) 恢复后重播成功；c) 最终正常确认销毁；d) **期间从未产生解锁**（这是"广播失败走重播"这条的正例） |
| 8 | （待补）`scenario_no_unlock_after_signed` | 签名已达成但故意不广播/不确认 → **断言**：`Unlock` 被拒。**当前 harness 无此能力**（没有"签名成功但拦住广播"的注入点），要实现需先在 harness 上开一个注入口 |

---

## 8. 待拍板问题

以下都是主会话那 4 条结论**没有覆盖**的设计分叉。每条给出选项与取舍，**不自行拍板**。

**Q1 —— 解锁的授权者是谁？（决定 §2.3 的迁移授权与 §3 的落地形态，影响面最大）**

- **(a) 链上验「门限放弃证书」**：`UnlockAsset` 必须携带组公钥可验证的 `thresholdSig`。与主会话结论 3 自洽，单节点作不了恶。代价：桥侧要新增一个签名轮次类型（B1）+ 一个把证书送到用户手里的渠道（链上事件？侧车 API？CLI 自取？）——这条渠道本身是新增的攻击面与新增的失败点。
- **(b) 只认 `CommitAddress` 提交**：授权判据直接用现成的 `from == rgbxCfg.CommitAddress`（现成代码 `checktx.go:500-505`）。实现最省、无新渠道。代价：**持该地址者 = 单点 = 可以先解锁再付款**，与"单节点作不了恶"的目标直接冲突。
- **(c) 超时后用户自提**：链上只查"pending 已超过 N 个高度"。**与主会话结论 1/2 冲突**（超时不等于未签名），会把"已签但确认慢"的提现误放，等于把 `ErrWithdrawConfirmTimeoutNotAllowed`（`checktx.go:63,541-546`）这条刻意关掉的路重新打开。**建议排除，但请确认。**

**Q2 —— "已达成签名"的判定放在链上还是桥侧？（决定 C3 残余风险归属）**

- **(a) 链上只验放弃证书**，"已签名则不可放弃"由签名节点拒签保证（§3.5）。共识判据简单，无新增交易。代价：桥侧失联/作恶时，用户仍可能拿到合法放弃证书而桥手里同时握着有效付款签名 ⇒ **桥净亏**，且链上无从发现。
- **(b) 链上同时接收并记录「付款证书」**：桥在签名成功后立刻提交一笔"付款已签名"的上链记录；`Unlock` 检查该记录不存在。更严，把 C3 从"桥侧策略"抬成"共识判据"。代价：多一笔链上交易、多一个失败点（签名成功但上链失败 ⇒ 又回到不可判），且要求桥在广播前先上链，引入新的时序约束。
- 这两条不是"哪个更好"，而是**残余风险放在哪一侧**的取舍。

**Q3 —— 终态键：一把还是两把？（动的是既有共识判据）**

- **(a) 收敛成一把** `formatWithdrawFinalKey(burnTxHash) -> settled|released`，写一次。语义单一、天然幂等、少一处漂移。代价：改掉既有 `formatWithdrawUsedKey`（`kv.go:77-80`）的口径 —— 而它今天已经被 `validate_proof.go:73` 读、被 `exec.go:265` 写，且 `S3` 的注释把它的理由写得很足。
- **(b) 保留 `formatWithdrawUsedKey` + 新增 `formatWithdrawReleasedKey`，两边互查**。完全不动既有路径、风险局部。代价：两条真值来源，任何未来的第三条终态都要再加一轮互查 —— 这正是 C1 那个"只查一边就漏"的坑的温床。
- 本链未上线，我倾向 (a)；但这是既有共识判据的改动，请确认。

**Q4 —— 先验拒绝（§5 的 #5 / #6）要不要把 invoice 校验搬到链上？**

- **(a) 搬**：新增 Go 侧 RGB invoice 解析 + `CrossChainInfo` 增加 `asset_id` 字段 + `checkCommitDKG` 强制提交 + `checkWithdraw` 做合约身份比对。收益：把"invoice 解析失败 / 合约不符"从"锁仓后走解锁"变成"提交时就拒"，从根上减少锁仓量与解锁轮次。代价：**链上要长期维护一份 RGB invoice 解析器**（今天只有 Rust 侧车有，`rgb-sidecar/src/invoice.rs:57`），invoice 格式演进时链上跟着改 = 共识层跟进第三方协议格式，风险与维护成本都不低。
- **(b) 不搬**，只做弱校验（前缀 `rgb:` / 长度上界），语义校验仍留桥侧。代价：这类失败仍会锁仓，靠 §7-1 的"拒绝后解锁"兜底。

**Q5 —— 锁定期间的产品语义（时限与费）**：锁定是否有上限时长？长期无人认领的额度怎么办？是否需要给"永久无人认领"一个治理出口（例如锁定超过 X 高度后由治理销毁）？—— 若答案是"需要自动处置"，则与 Q1(c) 是同一个坑，必须先明确"自动"的判据从哪来。

---

## 附 A：本文自行决定的项（低风险，供复核，不单独打断）

- 新 action 命名 `UnlockAsset` / `NameUnlockAssetAction = "Unlock"`；错误码沿用 `ErrInvalidThresholdSig`（`threshold.go:17`）+ 新增 `ErrWithdrawAlreadyReleased`。
- 解锁金额取台账记录的 `rec.Amount`，接收方取 `rec.Owner`（= burn 的 `tx.From()`），**不接受调用方指定地址**（理由见 §4.2-C7）。
- **解锁不改 `burned`**（从未销毁），不改 `minted`，不改台账记录本身（不可变原则，`operation.go:16-20`）；只新增终态键。
- `OperationRecord.Settled`（`query.go:228-234` 补算）保持"已结算放款"的既有 bool 语义，released 用**新增字段/新查询**表达，不复用 `Settled`。
- 「放弃证书」的消息原像口径与充值同形（`sha256(types.Encode(X{sig:nil}))`，对标 `threshold.go:20-29`），复用 `verifyThresholdSig` 的 btcec 路径（`threshold.go:33-58`），禁用 chain33 `VerifyBytes`（双重哈希陷阱，`threshold.go:31`）。
- 文档语言用中文（任务明确要求）。

## 附 B：覆盖边界（**不含糊**）

**已读**：

- rgbx 执行器：`crosschain.go`（全文）、`exec.go`（全文）、`exec_local.go`（全文）、`kv.go`（全文）、`operation.go`（全文）、`query.go`（全文）、`account.go`（全文）、`threshold.go`（全文）、`checktx.go` 的 `:55-160` / `:280-660`、`validate_proof.go` 的 `:67-230` / `:385-415`。
- rgbx 类型：`types/operation.go` `:1-80`、`types/asset.go` 片段、`types/rgbx.go` 片段（action 表）、`proto/rgbx.proto`（grep，未见 `WithdrawAsset` 定义 —— 该消息不在 `rgbx.proto` 里，可能由别处生成，**未追**）。
- 桥侧：`neutrino/bitcoin.go` `:300-800`、`neutrino/rgb20withdraw.go`（全文）、`neutrino/rgb20/withdraw.go`（全文）、`neutrino/tss.go` `:160-240` / `:605-700` / `:740-1030`、`neutrino/config.go` `:177-186`、`neutrino/rgbx.go` `:100-180`。
- E2E：`build/ci/rgbx-ci/testcase.sh`（全文）、`scripts/assertions.sh`（全文）、`scripts/btc_test.sh` `:197-239`、`scripts/rgb20_test.sh`（函数清单 + `:464-500` / `:220-255` 片段）。
- `plugin/dapp/rgbx/cmd/ci/docker-compose.sh` 的 `:98` / `:333`（grep）。

**没读（因此本文任何涉及它们的论断都不成立）**：

- `btcwallet.go`（42KB）**完全没读** —— BTC 侧 `buildWithdrawTx`、UTXO 锁定/释放（`releaseUTXOsExcept`）、`addPendingTx` 的细节均未核实。
- `validate_proof.go` 的 `:230-385` 与 `:415-end`（`validateBtcTxProof` 主体、`hasWithdrawCommitment`、`getBtcCanonicalTip`）。
- `checktx.go` 的 `:160-280` 与 `:660-end`。
- `tss.go` 的 `:240-605` / `:1030-1387` / `:1387-end`（含 `handleRgb20WithdrawSign` / `validateRgb20WithdrawSign` 主体、BTC 侧 `handleSignNotify` 完整实现、`processSignBtcTx`）。
- `neutrino/client.go`、`deposit_address.go`、`sweep.go`、`btcwallet.go`；`rgb20/` 下的 `adapter.go` / `deposit.go` / `precision.go` / `seal.go` / `receive.go`。
- Rust 侧车全部（只 grep 到 `invoice.rs:57` 的 `parse_invoice` 存在，**没读实现**）。
- `build/ci/rgbx-ci/docker-compose.sh`（harness 主体，未读）。
- **另一个 worktree**（`~/git/worktree/plugin-btc-crosschain-demo`，分支 `demo/btc-crosschain`）**完全没碰**。

**方法边界**：

- **没有跑任何测试、没有任何实跑验证**。本文全部结论来自静态阅读 + grep。
- 没有跑 E2E，没有起任何容器，没有做任何代码改动，没有 commit。

## 附 C：未核实 / 不确定项

1. **「组签名可被链上 `verifyThresholdSig` 验证」未实测**。我据 `ToBtcecSignature`（`tss.go:765`）产出 DER 签名、且充值路径已用同一套 `btcec.ParsePubKey` + `ecdsa.ParseDERSignature` + `Verify`（`threshold.go:42-53`）验证同源签名，推断成立；但**没有实跑**一次"CGGMP 签出来的签名能否通过 `verifyThresholdSig`"。这是 §3 整节的技术前提，**建议实现前先写一个探针验证**。
2. `WithdrawAsset` 这个 proto 消息**不在** `plugin/dapp/rgbx/proto/rgbx.proto` 里（grep 无命中），我没找到它的定义文件。因此 §3.4 里 `UnlockAsset` 该加在哪个 proto 文件、以及新消息的编号分配，**未最终确定**。
3. 锁定地址**当前没有任何查询入口**（`query.go` 全文里没有），§7 的几条余额断言怎么落地未定。
4. BTC 侧 `minChangeAmount`、`stickyUTXO` 的完整生命周期（`btcwallet.go` 未读）未核实，§5 的 #14/#15 只按 RGB20 侧的行为描述。
5. 桥侧 `broadcastOutcomeIsSuccess` / `txKnownToNode`（`rgb20withdraw.go:118-141`）在纯 neutrino 部署（无全节点 RPC）下退化为"按广播结果判定"（`rgb20withdraw.go:118-120` 注释），这对 §5-#14 的判定有影响，**未展开**。
6. 「桥不能作恶」的信任假设我只按主会话结论 3 复述（3/4 门限），**没有独立论证** 3/4 之外的合谋面（例如 guardian 节点与桥运营方是否同一实体）—— 那需要看部署形态，不在代码里。
