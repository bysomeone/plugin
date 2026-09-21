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
`crypto/birkhoffinterpolation/birkhoffinterpolation.go:188` 的 `ensureRankAndOrder`：

```go
if uint32(bks.Len()) < threshold {
    return ErrEqualOrLargerThreshold
}
```

即**不是按权重加权**；`rank` 决定的是插值的数学结构（该方贡献几阶导数点），不是「必需性」。

> ⚠️ 这与「权重 0 表示必须参与」的说法**不一致**。那可能是早期 GG18 版本、或另一份设计稿的语义 —— 需要单独核实后才能写进备份方案，此处不下结论。

**能否动态变更（改门限 / 加节点）**：协议层**支持**。`ProcessRefresh`（alice 的 resharing）可在**保持公钥不变**的前提下更换参与方集合与门限，正是「动态加入新节点」所需的机制。**但是** plugin 侧目前只在 DKG 流程里调用 refresh，**没有**把「增删节点后重新分享」做成运维入口 —— 要用得单独做。

**对备份方案的直接含义**：在门限语义完全确认前，按**最保守假设**设计 —— 任意份额丢失都可能致命，因此每个节点各自独立备份，且**不放在同一处**。
