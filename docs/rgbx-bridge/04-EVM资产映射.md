# 跨链资产的 EVM 映射方案

把桥接进来的跨链资产（XBTC、RGB20 USDT 及其他已登记的 RGB20 合约资产）在 chain33 自带的
`evm` 上暴露为标准 ERC20。本文是设计文档，尚未实现；实现进度以仓库 issue 为准。

## 1. 结论与边界

采用**同链预编译 + ERC20 包装合约**方案：EVM 里的余额就是 `rgbx` 账户库里那笔余额，不另铸一份。

| 维度 | 结论 |
| --- | --- |
| 账本 | 单一，仍是 `rgbx` 执行器下的 `account.DB` |
| 跨链证明 | 不需要（同一条链内读写） |
| 新增信任边界 | 无（只新增一个受限的合约可调用入口） |
| 共识改动 | 有：新预编译 + fork 高度 |
| 反向（EVM → 比特币） | 第二阶段，见 §6 |

不适用外部公链 EVM。若目标是外部 EVM，需要 TSS 组签名 + 目标链验签 + burn 去重的跨链证明桥，
是另一套系统，不在本文范围内。

## 2. 现状

- 桥接资产以 `rgbx` 执行器的账户余额形式存在：
  `account.NewAccountDB(cfg, rtypes.RgbxX, formatSymbol(symbol), stateDB)`
  （`plugin/dapp/rgbx/executor/account.go`）。symbol 大写归一化，是资产唯一身份。
- 资产元信息在链上 `CrossChainInfo`（`plugin/dapp/rgbx/types/asset.go`），含
  `assetKind`（`btc` / `rgb20`）与 `precision`。
- chain33 已有同构的成熟范式：`token` 预编译
  （`plugin/dapp/evm/executor/vm/runtime/token.go`）在 EVM 中直接读写 `token` 执行器的账户库。

## 3. 架构

| 组件 | 位置 | 职责 |
| --- | --- | --- |
| 预编译 | `plugin/dapp/evm/executor/vm/runtime/rgbx.go`，地址 `0x0000000000000000000000000000000000200003` | `balanceOf` / `totalSupply` / `decimals` / `transfer` |
| 库合约 | `plugin/dapp/evm/contracts/rgbx/PreRgbxToken.sol` | staticcall 封装，供包装合约引用 |
| 包装合约 | `plugin/dapp/evm/contracts/rgbx/RgbxToken.sol` | `name` / `symbol` / `allowance` 等 EVM 侧语义，转账落到预编译 |
| 绑定 | `plugin/dapp/evm/contracts/rgbx/generated/` | go binding |
| 授权注册表 | EVM 状态：包装合约地址 → symbol | 只有注册过的合约能调预编译 |

地址顺延现有分配：`0x…200001` 为 `token`，`0x…200002` 为 `comflix`。

### 3.1 数据来源

| 方法 | 来源 |
| --- | --- |
| `balanceOf` | `rgbx` 账户库 `LoadAccount(addr).Balance` |
| `decimals` | `CrossChainInfo.precision` |
| `totalSupply` | `rgbx` 供应量台账（`operationSupplyKeyPrefix`，mint/burn 单调累计） |
| `transfer` | `rgbx` 账户库 `Transfer`，先过 EVM 账户黑名单 `isBlockedAccount` |

金额口径与 `token` 预编译一致：账户库是 `int64`，`decimals` 只作为展示与换算依据，不做缩放。
`totalSupply` 与余额来自同一本账，恒等式 `Σbalance == totalSupply` 在 rgbx 侧已成立。

## 4. 安全设计

`token` 预编译的 `from` 取自 calldata，依赖"包装合约由 SuperManager 部署 + Solidity 里
`from = msg.sender`"兜住。本方案收紧为：

1. **授权注册表**：包装合约必须先注册并与唯一 symbol 绑定；未注册的调用直接 revert。
   恶意合约既不能自建通道，也不能抢注他人 symbol。
2. **`from` 由包装合约填入 `msg.sender`**，预编译校验其为有效地址，不接受外部指定转账方。
3. **资产白名单**：只在 `CrossChainInfo` 中登记的 symbol 可被映射，内部记账符号不可暴露。
4. **fork 门控**：`Height < fork` 时预编译返回空成功，模拟"调用空地址"的语义，否则从创世
   重放会与历史状态分歧。
5. **黑名单一致**：与 `token` 路径共用同一份账户黑名单判定。

## 5. 分期

| 阶段 | 内容 | 交付判据 |
| --- | --- | --- |
| P0 | 前置核验 | 见 §7 的三项全部通过 |
| P1 | 只读预编译 + 包装合约 + 单测 | `balanceOf` / `decimals` / `totalSupply` 与 rgbx 状态逐个对上 |
| P2 | `transfer` + 注册表 + fork 门控 + E2E | E2E 场景：BTC 充值 → EVM 内转账 → 转回普通地址 → 提现 |
| P3 | 反向：EVM 内 burn 直接发起提现 | 见 §6 |

P1、P2 自足可交付。P2 完成前，用户若需要提现，走"EVM 转回普通链地址 → 现有提现接口"，
不涉及额外风险。

## 6. 反向路径（P3）

EVM 里销毁后要直接得到比特币，需要把销毁落成 rgbx 的提现记录（锁定 + 操作台账 + pending
payload，见 `executor/crosschain.go` 的 `Exec_Withdraw`）。两个约束：

- **终态键必须扩成三值**。`formatWithdrawFinalKey` 现在只有 `settled` / `released` 两个值，
  多一个结算域后同一笔 burn 可能被两个域各消费一次。扩为三值后所有读取点（结算、解锁、
  加价、放弃、查询）都要互查。
- **限额与熔断复用**：EVM 侧销毁同样计入提现窗口限额与熔断判定，不能成为绕过通道。

实现上不建议在预编译里重写这套写态逻辑，应把提现记录的构造与写入抽成执行器与预编译共用的
包级辅助函数，避免两份实现漂移。

## 7. 前置核验（P0）

1. **地址驱动**：`TransferToToken` 直接把 20 字节 hex 当账户名使用，只在 eth 地址驱动下成立。
   需确认本链 EVM 的地址驱动类型；非 eth 驱动时必须做地址格式转换。
2. **跨执行器写态**：EVM 的 `MemoryStateDB` 能否写 `mavl-rgbx-*` 键。`token` 已验证同一机制，
   需在 rgbx 上再确认一次。
3. **精度上限**：账户库为 `int64`，`precision` 过大时 `10^precision` 换算溢出；确认已登记的
   资产精度均在可用范围内。

## 8. 尚未决定

- 注册表由 evm 执行器管理员动作写入，还是由 rgbx 治理流程写入。
- 包装合约是每个资产一份（便于各自命名与元数据），还是全资产共用一个工厂。
- `name` / `symbol` 的取值来源：包装合约构造参数、`CrossChainInfo`，或两者交叉校验。
