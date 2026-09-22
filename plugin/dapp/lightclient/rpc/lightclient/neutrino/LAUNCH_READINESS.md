# RGB20 USDT 跨链桥 · 上线就绪清单

> 快照 2026-09-21。基线：plugin `feat-rgb-usdt-integration` @ `55410ffd9`，chain33 `feat/tss-alice-cggmp` @ `36fb0fd21`。
> 分档判据：**上线必须**＝缺了会阻塞上线或造成不可逆损失；**可延后**；**流程**。

## 一、核心开发已闭环

| 能力 | 证据 |
|---|---|
| 充值：P2WSH 每用户地址 → 侧车结算 → TSS 签名 → 链上铸币 | E2E 充值场景 + P2WSH 扫码充值 + sweep 归集 |
| 提现：链上发起 → BuildWithdrawal → TSS signPsbt → 广播 → 确认 → 销毁 | E2E 三条提现场景（含连续提现、截断金额 60000 最小单位） |
| native asset mint / burn | E2E mint 场景 |
| 双花 / 重复领取护栏（E1·E9·E11·E14·E15） | 单测 + E2E 负例 |
| BTC 头查询的共识权威 E6(b) | E2E 读节点日志取证（护栏自身诊断） |
| DKG 会话韧性（超时后可重试） | 回归测试 + chain33 `36fb0fd21` |

**端到端**：2026-09-19 全量 E2E 10 场景 `E2E_EXIT=0`，零 ERROR/FAIL/panic。

**覆盖边界（不够的地方如实标出，别当已覆盖）**

- E14 的 stateDB 去重键在健康节点上**不可达**（localdb 层先命中）。E2E 负例证明的是「去重机制整体」，该键单独的判别性只有单测。
- #55 那一轮 E2E **没有触发超时路径**（DKG 一次成功），其证据是回归测试，不是 E2E。
- Taproot 未做（见下）。

## 二、上线前必须

| 项 | 为什么是必须 | 状态 |
|---|---|---|
| **备份 / 恢复规划** | TSS 是门限签名，share 丢失**不可逆** —— 丢够份额＝永久失去对资金的签名能力。这是运维前提，不是加固 | 规划中 |
| **最小备份 / 恢复入口** | 上一条的可执行部分：share 与侧车 `/data` 怎么落盘、怎么恢复 | 未开始 |
| **S5 确认深度可配** | 源链事件 / BTC-RGB 锚定的确认深度要做成配置项；硬编码会在上线调参时卡住 | 未开始 |
| S6 费账户与锁仓资金分离记账 | 原设计即标「可选」，不阻塞上线 | 未开始 |

## 三、可延后

| 项 | 性质 |
|---|---|
| 性能基准 + 指标文档 | 文档化；基准文件已写好待提交 |
| 性能优化批次（O1–O8 等） | 已明确推迟到系统开发完成之后 |
| TSS Taproot / FROST | 前瞻性：当前方案是 **P2WSH 支付到脚本**，Taproot 属后续升级 |
| chain33-cli 退出码缺陷 | 已决定**不改底层**（会打坏按旧标准写的合约测试脚本），只在我们自己的脚本侧规避 |

## 四、流程

| 项 | 说明 |
|---|---|
| chain33 提 PR | `feat/tss-alice-cggmp` → `33cn/chain33`。合入并发版后，删掉 plugin `go.mod` 里的 fork `replace`、把 require 指到发行版（MVS 自动接上） |

## 附：门限、权重与「动态加节点」

**机制**：alice CGGMP + Birkhoff 插值。每个节点有**自己的 `rank`**，`threshold` 是门限；包装器签名 `ProcessDKG(peers, threshold, rank, …)`。

**实测配置**（E2E）：para1 official `rank=0`、para2–4 validator `rank=1`、`threshold=3`。

**「谁必须参与」由什么决定**：alice 的校验只检查**参与方个数 ≥ threshold** ——
`getamis/alice@v1.0.7` 的 `crypto/birkhoffinterpolation/birkhoffinterpolation.go:188`（**在 alice 依赖里，不在 chain33 检出里**，要看得去 `$(go env GOMODCACHE)/github.com/getamis/alice@v1.0.7/`）的 `ensureRankAndOrder`：

```go
if uint32(bks.Len()) < threshold {
    return ErrEqualOrLargerThreshold
}
```

即**不是按权重加权**；`rank` 决定的是插值的数学结构（该方贡献几阶导数点），不是「必需性」。

> ⚠️ 这与「权重 0 表示必须参与」的说法**不一致**。那可能是早期 GG18 版本、或另一份设计稿的语义 —— 需要单独核实后才能写进备份方案，此处不下结论。

**改门限与加节点是两件不同的事，走的是两套协议**（此前本文件把两者混为一谈，已订正）：

- **改门限（同一组人）**：走 CGGMP 的 `refresh`（`getamis/alice@v1.0.7` 的 `crypto/tss/ecdsa/cggmp/refresh`）。我们已经在用（每次签名前强制跑一轮，见 `tss.go:573-590`），但调用时传的是**配置里同一份** `Peers`/`Threshold`，即「原地换 share」。之所以不能靠它加人：`NewRefresh(oldShare, …)` 要求每个参与者提供**自己的旧份额** —— 新节点没有旧份额，构造不出来。
- **加节点**：走的是**另一个包** `crypto/tss/ecdsa/addshare`（`newpeer` + `oldpeer`）—— `newpeer.NewAddShare(peerManager, pubkey, threshold, newPeerRank, listener)` **不需要旧 share**，`oldpeer.NewAddShare(…, share, bks, newPeerID, …)` 由老节点用自己的旧份额 + 当前 Birkhoff 参数参与。它在 **Birkhoff 参数（bks）** 这一层工作，与 CGGMP 同源，因此是**方案无关**的加节点协议。（GG18 另有专属的 `gg18/reshare`。）

**我们的状态**：chain33 与 plugin 对 `addshare` 的引用是 **0** —— **底层有、我们没接**。所以「加节点」不是"底层做不到"，而是**没接线**。

**接线可行性与所需改法（2026-09-22 已查实）**

- **产出物**：两个 `Result` **同构**（`addshare/{newpeer,oldpeer}/addshare.go:34-39`）：`{PartialPublicKeys, PublicKey, Share, Bks}`。老节点的 share 不变（底层多项式没动，addshare 只是给新节点算一个新增值点），Bks 只**多一项**新 bk。
- **不能直接喂 CGGMP sign**：`cggmp/sign/sign.go:40` 除上述之外还要 **`paillierKey`（本节点 Paillier 私钥）** 与 **`ped`（全体 Pedersen 参数）**，而 `ped` 不是可选项（`round_1.go:124,150` 与 `round_3/4` 直接解引用 ⇒ 缺项即 nil deref；我们的 wrapper 已挡这一条，见 `system/crypto/tss/cggmp/result.go:248` 的 `validateSignMaterial`）。
- **但能直接喂 GG18 sign** —— 这是**库自带集成测试实测**的：`gg18/integration/tss_test.go` 的 Step 5（:168-241）跑 newpeer+oldpeer addshare 并把 share/bks 写回，Step 6（:243-248）用新集合继续签名（`-ginkgo.focus='3 of \(0,0,0\)$'` 通过）。⇒ addshare 的 (share, bks) 在 Birkhoff 层**本来就是合法签名输入**，缺的纯粹是 CGGMP 额外那两样。
- **正解：`addshare → refresh → sign`**（**不重跑 DKG，群公钥不变**）。refresh 就是 CGGMP 的 Paillier/Pedersen provisioning 阶段，且它接受**任意内部自洽的旧状态**作输入（`refresh/round_3.go:202,225-236`）。三处断言的是同一个 `ValidatePublicKey`（DKG `dkg/3_result_handler.go:110`、refresh `refresh/round_3.go:233`、addshare `newpeer/addshare_test.go:143`）。
- **落到我们这边**：用 addshare 结果**重建一个 `DKGResult`**（`PubX/PubY/Rid` 沿用，`Share/Bks/PartialPubKeys` 换掉），再原样调现有的 `cggmp.ProcessRefresh` → `ProcessSign`。**两个 wrapper 都不用改** —— 已核：`ProcessRefresh(peers, threshold, dkgRes *DKGResult, …)`（`api.go:147`），而 `aliceResult()`（`result.go:105-116`）正是从 `DKGResult` 取 `Share`/`Bks`/`Rid`/公钥，另加 `validatePartialPubKeys`。**真正要新写的是 addshare 本身的接线**（newpeer/oldpeer + 消息通道），两仓引用数确实为 0。
- **两条硬约束**：① `newPeerRank` 必须 **< threshold**（求和是 `for i := newrank; i < threshold`，`birkhoffinterpolation.go:251`；rank ≥ threshold 时系数恒 0、新 share=0，端点自校验会**报错**而非静默）；② Bks **有两份** —— 库里的那份与我们的 `selectSignerCombination`/`validateSignerCombination`（`tss.go:668/694`）读的那份，重建 `DKGResult` 时**要一起换**。新节点 rank 建议 **0**。

> ⚠️ **仍未实测的部分**：`addshare → refresh → sign` 的**完整串联**没跑过 —— **库没有这条测试**，上面是从入参契约 + 三处同一断言推得的，不是实测。上线前应补一条可执行探针（在 regtest 上加第 5 个节点，走完 addshare→refresh 后用新集合签一笔）。

**对上线的含义不变**：这件事牵扯密钥生命周期（改错了直接锁死资金），**上线前不做**。但理由从"底层不支持"更正为"底层支持、我们没接、且接线后需实测"。

**对备份方案的直接含义**：在门限语义完全确认前，按**最保守假设**设计 —— 任意份额丢失都可能致命，因此每个节点各自独立备份，且**不放在同一处**。
