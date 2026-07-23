# RGB20 USDT 跨链集成技术方案

**版本**: v2.0  
**适用对象**: 开发者、架构师  
**最后更新**: 2026-07-23  
**前置依赖**: [BTC 跨链桥技术架构文档](./TECHNICAL.md)

---

## 重要前提：rgbx ≠ RGB 标准协议

**rgbx 是 Chain33 生态自定义的资产协议**，借用了 RGB 的核心思想（UTXO 封印、客户端验证、OP_RETURN 承诺），但使用了完全独立的数据格式和验证规则。rgbx **不是** RGB 标准协议的实现，两者的 commitment 格式、状态编码、Schema 定义均**互不兼容**。

本方案的策略是：在 rgbx 合约层保持不动的情况下，于 neutrino 链下节点新增一套 **RGB20 适配层**，专门解析和验证标准 RGB 协议的数据，将 RGB20 USDT 的充提翻译为 rgbx 合约能理解的 Deposit/Withdraw 操作。

```
┌─────────────────────────────────────────────────────────────────┐
│  rgbx: 自定义协议       RGB 标准协议: LNP/BP 协会维护              │
│                                                                  │
│  mintAsset               RGB20 Schema (同质化代币)               │
│  transferAsset           Contractum 语言定义                      │
│  confirmTx               strict_types 编码                       │
│  OP_RETURN: rgbx:xxx     Opret/Tapret commitment                 │
│                                                                  │
│         ≠ 不兼容 → 需要适配层桥接                                  │
└─────────────────────────────────────────────────────────────────┘
```

---

## 目录

1. [背景与动机](#1-背景与动机)
2. [rgbx 与 RGB 标准协议的关系](#2-rgbx-与-rgb-标准协议的关系)
3. [架构设计](#3-架构设计)
4. [RGB20 适配层设计](#4-rgb20-适配层设计)
5. [核心流程设计](#5-核心流程设计)
6. [数据流与格式转换](#6-数据流与格式转换)
7. [Neutrino 节点改造](#7-neutrino-节点改造)
8. [安全模型](#8-安全模型)
9. [配置与部署](#9-配置与部署)
10. [代码变更清单](#10-代码变更清单)

---

## 1. 背景与动机

### 1.1 现状

当前跨链桥通过 rgbx 合约支持 BTC 原生币的充提。rgbx 合约已具备多资产管理框架（`CrossChainInfo` 按 `assetSymbol` 索引，`DepositAsset`/`WithdrawAsset` 均带资产类型标识），为扩展新资产预留了接口。

### 1.2 目标

接入 **RGB 标准协议**上发行的 USDT（RGB20 同质化代币），实现与 Chain33 的双向跨链：

- **充值**：用户用 RGB 钱包将 RGB20 USDT 转入桥的 TSS 封印 → Chain33 铸造 wrapped USDT
- **提现**：用户在 Chain33 销毁 wrapped USDT → 桥构造 BTC 交易将 RGB20 USDT 转出

### 1.3 为什么需要适配层

rgbx 合约无法直接理解标准 RGB 协议的数据。需要一个链下适配层：

| 层次 | 职责 |
|------|------|
| **rgbx 合约 (链上)** | BTC SPV 验证、OP_RETURN 承诺验证、资产发行/销毁（不变） |
| **RGB20 适配器 (链下)** | 解析 RGB 标准协议数据、验证状态转移、提取金额 → 翻译为 rgbx 能理解的参数 |

这和 Ethereum 跨链桥（cross2eth）的架构一致——链上合约不解析 EVM 交易，只验证 Merkle Proof；EVM 事件解析全在 relayer 里做。

---

## 2. rgbx 与 RGB 标准协议的关系

### 2.1 设计思想的共通点

两者都基于以下核心概念：

| 概念 | 说明 |
|------|------|
| **Single-Use Seal** | BTC UTXO 作为"封印"，锁定资产所有权 |
| **Client-Side Validation** | 验证逻辑在客户端执行，BTC 链上只记录 commitment |
| **State Transition** | 资产转移 = 关闭旧封印 + 打开新封印 |
| **Commitment** | BTC 交易中嵌入状态转移的承诺数据 |

### 2.2 具体实现的不兼容

| | rgbx 协议 | RGB 标准协议 |
|---|---|---|
| **代币定义** | `MintAsset` (proto 自定义) | RGB20 Schema (Contractum 语言) |
| **状态编码** | Protobuf 序列化 | strict_types 二进制编码 |
| **承诺方式** | OP_RETURN `rgbx:deposit:xxx` | Opret (OP_RETURN) / Tapret (Taproot) 标准格式 |
| **Schema 语言** | 无（代码硬编码验证规则） | Contractum + AluVM 虚拟机 |
| **验证引擎** | Chain33 合约 Go 代码 | AluVM 字节码解释器 |
| **资产转移** | `TransferAsset` + `ConfirmTx` | RGB State Transition (Schema 定义) |
| **证明数据** | `UtxoSpendingProof` (BTC tx + 索引) | Consignment (完整状态转移链 + 附件) |

### 2.3 适配层的边界

**适配层需要做的**（接触标准 RGB 协议）：

- 解析 RGB Consignment 数据（strict_types 解码）
- 识别 RGB20 Schema 的状态转移（提取 USDT 金额和封印 UTXO）
- 验证 Opret/Tapret commitment 格式
- 调用 RGB Core 库验证状态转移合法性
- 跟踪 TSS 地址下的 RGB 封印状态

**适配层不需要做的**（由 rgbx 合约处理）：

- BTC SPV 证明验证
- OP_RETURN 广播承诺格式验证（rgbx 自己的 `rgbx:withdraw:xxx` 格式）
- 封装资产的发行、锁定、销毁
- 跨链状态机管理（Pending → Confirmed）

---

## 3. 架构设计

### 3.1 整体架构

```
┌──────────────────────────────────────────────────────────────────────────────┐
│                              Chain33 主链                                     │
│                                                                              │
│  ┌──────────────────────┐  ┌──────────────────────┐  ┌──────────────────────┐│
│  │ lightclient 合约      │  │ rgbx 合约 (不变)      │  │ TSS 网络 (P2P)       ││
│  │ BTC区块头 (复用)      │  │ BTC SPV验证 (复用)    │  │ DKG-1: BTC 密钥组    ││
│  │                      │  │ OP_RETURN 承诺 (复用) │  │ DKG-2: RGB20 USDT    ││
│  └──────────────────────┘  └──────────────────────┘  └──────────────────────┘│
└──────────────────────────────────────────────────────────────────────────────┘
                                    ▲
                                    │ Deposit/Withdraw (与 BTC 桥相同的格式)
                                    │
┌───────────────────────────────────┴──────────────────────────────────────────┐
│                         Neutrino 官方节点 (链下)                               │
│                                                                              │
│  ┌──────────────────────────────────────────────────────────────────────┐   │
│  │                    RGB20 USDT 适配层 (新增)                             │   │
│  │                                                                       │   │
│  │  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐                 │   │
│  │  │ commitment   │  │ state        │  │ seal         │                 │   │
│  │  │ 解析器       │  │ 验证器       │  │ 追踪器       │                 │   │
│  │  │ • Opret 解析 │  │ • RGB Core   │  │ • 封印UTXO   │                 │   │
│  │  │ • Tapret 解析│  │ • RGB20      │  │   索引管理   │                 │   │
│  │  │ • strict_    │  │   Schema     │  │ • 余额同步   │                 │   │
│  │  │   types 解码 │  │ • 双重支付   │  │ • 过期检测   │                 │   │
│  │  └──────┬───────┘  └──────┬───────┘  └──────┬───────┘                 │   │
│  │         └─────────────────┼─────────────────┘                         │   │
│  │                           ▼                                           │   │
│  │                ┌───────────────────┐                                  │   │
│  │                │ 翻译为 rgbx 格式   │                                  │   │
│  │                │ • 金额 (BTC→USDT) │                                  │   │
│  │                │ • 地址映射        │                                  │   │
│  │                │ • SPV证明构造     │                                  │   │
│  │                └───────────────────┘                                  │   │
│  └──────────────────────────────────────────────────────────────────────┘   │
│                                                                              │
│  ┌─────────────────────┐     ┌──────────────────────────┐                    │
│  │ 已有: BTC 监听/构造  │     │ 已有: TSS 签名服务        │                    │
│  │ (btcwallet.go,      │     │ (tss.go, 支持多密钥组)   │                    │
│  │  bitcoin.go)        │     │                          │                    │
│  └─────────────────────┘     └──────────────────────────┘                    │
└──────────────────────────────────────────────────────────────────────────────┘
                                │
                                ▼
┌──────────────────────────────────────────────────────────────────────────────┐
│                           Bitcoin 主链                                        │
│                                                                              │
│  ┌─────────────────────────┐     ┌───────────────────────────┐              │
│  │ BTC TSS 地址            │     │ BTC TSS 地址 (RGB20 USDT)  │              │
│  │ • 存储 BTC 价值         │     │ • 仅 BTC 粉尘 (封印载体)    │              │
│  │ • 无 RGB 状态           │     │ • 密封 RGB20 USDT 状态     │              │
│  └─────────────────────────┘     └───────────────────────────┘              │
│                                                                              │
│  RGB20 USDT 的 commitment 格式 (标准 RGB 协议):                               │
│  ┌─────────────────────────────────────────────────────────────┐            │
│  │ Opret:  OP_RETURN <rgb_commitment_hash>                     │            │
│  │ Tapret: Taproot script-path spend with tweaked commitment   │            │
│  └─────────────────────────────────────────────────────────────┘            │
└──────────────────────────────────────────────────────────────────────────────┘
```

### 3.2 模块职责

| 模块 | 类型 | 变更 | 职责 |
|------|------|------|------|
| **rgbx 合约** | 链上 | 极小改动 | 复用 BTC SPV 验证 + OP_RETURN 广播承诺 + 资产铸造/销毁 |
| **lightclient 合约** | 链上 | 无变更 | BTC 区块头存储与查询 |
| **TSS 网络** | P2P | 扩展 | 新增 RGB20 USDT 密钥组 DKG |
| **RGB20 适配层** | 链下 (新增) | 全新 | 标准 RGB 协议解析、验证、格式翻译 |
| **neutrino BTC 模块** | 链下 | 微调 | 区分 BTC 和 RGB20 充值的处理分支 |

### 3.3 数据流全景

```
充值方向 (RGB20 USDT → Chain33):

BTC 交易 (含 RGB20 commitment)
    │
    ▼
[RGB20 适配层]
  • 解析 Opret/Tapret commitment
  • strict_types 解码 Consignment
  • 比对 RGB20 Schema → 提取 USDT 金额 + 封印 UTXO
  • 调用 RGB Core 验证库
    │
    ▼ (翻译为)
    │
[rgbx.Deposit]
  assetSymbol = "RGB20_USDT"
  amount      = (从 RGB 状态提取的 USDT 金额)
  depositAddr = (用户在 OP_RETURN 广播中指定的 Chain33 地址)
  txProof     = (BTC SPV 证明，与 BTC 桥相同)

提现方向 (Chain33 → RGB20 USDT):

rgbx.Withdraw → PendingTx
    │
    ▼
[RGB20 适配层]
  • 查询 TSS 封印 UTXO 列表
  • 选择封印余额 ≥ 提现金额 的 UTXO
  • 构造 BTC 交易模板
  • 生成 RGB20 State Transition (调用 RGB Core)
  • 嵌入 Opret/Tapret commitment
  • 嵌入 rgbx 广播承诺 (rgbx:withdraw:xxx，用于链上结算)
    │
    ▼
TSS 签名 → 广播 → 确认 → rgbx.Confirm 结算
```

---

## 4. RGB20 适配层设计

### 4.1 适配层的定位

适配层是一个**纯链下模块**，运行在 neutrion 官方节点进程中。它的职责是架起标准 RGB 协议和 rgbx 合约之间的桥梁：

```
标准 RGB 协议世界                     rgbx 合约世界
══════════════════     适配层      ══════════════════
Consignment (strict)  ──解析──▶   DepositAsset (proto)
RGB20 Schema          ──匹配──▶   assetSymbol = "RGB20_USDT"
State Transition      ──提取──▶   amount = 提取的 USDT 数量
Opret/Tapret commit   ──识别──▶   触发充值处理
Seal UTXO             ──追踪──▶   对应 TSS PkScript
```

### 4.2 核心数据结构（适配层内部）

```
RGB20 Consignment (标准 RGB 协议):
┌──────────────────────────────────────────────┐
│ Transition Bundle                             │
│  ├─ Transition #0                            │
│  │   ├─ ContractID: rgb:xxx-USDT             │
│  │   ├─ SchemaID:   rgb:xxx-USDT-Schema      │
│  │   ├─ Inputs:                              │
│  │   │   └─ Seal UTXO: txid:vout (旧封印)    │
│  │   ├─ Assignments:                         │
│  │   │   ├─ OwnedState (转入的金额)           │
│  │   │   └─ FungibleToken (USDT 数量)        │
│  │   └─ Valencies: (新封印 UTXO 列表)         │
│  │       ├─ {output:0, amount: 500 USDT}     │
│  │       ├─ {output:1, amount: 300 USDT}     │
│  │       └─ {output:2, amount: 200 USDT}     │
│  └─ ...更多 Transitions                       │
│                                               │
│  Attachment: (RGB 附件/证明)                   │
│  └─ 前置 State Transitions 的证明             │
└──────────────────────────────────────────────┘

适配层提取结果:
┌──────────────────────────────────────────────┐
│  • 找到新封印在 output[0] (TSS PkScript)      │
│  • 该封印对应的 USDT 金额: 500 USDT          │
│  • 旧封印 UTXO: txid_A:vout_0               │
│  • 新封印 UTXO: txid_B:vout_0               │
│  • 广播承诺数据: (用户指定的 chain33 地址)     │
└──────────────────────────────────────────────┘
```

### 4.3 RGB Core 集成方式

```
方案选择:

选项 A: 调用 RGB Proxy (独立进程，gRPC)
  ✅ 隔离性好，升级 RGB 协议不触及 neutrino
  ✅ 复用社区 RGB Core 库
  ❌ 额外运维成本 (多一个进程)

选项 B: 内嵌 RGB Core (通过 CGO/FFI)
  ✅ 部署简单
  ❌ 编译复杂 (Rust + Go)
  ❌ RGB 版本升级需重新编译 neutrino

建议: 选项 A — RGB Proxy 方式

neutrino (Go)                    RGB Proxy (Rust/gRPC)
─────────────                    ─────────────────────
rgb20/adapter.go                 rgb-core 库
  │                                 │
  ├─ ValidateDeposit() ──gRPC──▶   │
  │  (传入 BTC tx bytes)          ├─ strict_types 解码
  │                               ├─ RGB20 Schema 验证
  │  ◀──gRPC── {valid, amount,   ├─ Seal 一致性检查
  │             sealUTXO}         └─ 返回验证结果
```

### 4.4 适配层接口定义

```go
// rgb20/adapter.go — RGB20 适配层核心接口

// RGB20Adapter 标准 RGB20 协议适配器
type RGB20Adapter interface {
    // ParseDepositFromBtcTx 从 BTC 交易中解析 RGB20 USDT 充值
    // 输入: 已确认的 BTC 交易 + TSS PkScript
    // 输出: 充值金额 + 新封印 UTXO + 充值目标地址
    // 返回 error 如果:
    //   - 交易中无 RGB20 commitment
    //   - RGB20 Schema 不匹配
    //   - 状态转移非法
    //   - 封印不是到 TSS 地址的
    ParseDepositFromBtcTx(btcTx *wire.MsgTx, tssPkScript []byte) (*RGB20Deposit, error)

    // BuildWithdrawalTx 构造 RGB20 USDT 提现的 BTC 交易
    // 输入: 提现参数
    // 输出: 未签名的 BTC 交易 + RGB Consignment + 封印映射
    BuildWithdrawalTx(params *RGB20WithdrawParams) (*RGB20WithdrawResult, error)

    // GetSealBalance 查询指定 UTXO 封印的 RGB20 USDT 余额
    GetSealBalance(outPoint wire.OutPoint) (int64, error)

    // ListAvailableSeals 列出所有可用的 TSS 封印及其余额
    ListAvailableSeals() ([]RGB20Seal, error)
}

// RGB20Deposit 充值解析结果
type RGB20Deposit struct {
    Amount       int64          // USDT 金额 (带精度)
    NewSealUtxo  wire.OutPoint  // 新封印 UTXO 位置
    OldSealUtxo  wire.OutPoint  // 被关闭的旧封印
    DepositAddr  string         // Chain33 目标地址 (从广播承诺提取)
    ContractID   string         // RGB 合约 ID
}

// RGB20Seal 封印 UTXO
type RGB20Seal struct {
    OutPoint    wire.OutPoint
    UsdtAmount  int64  // USDT 余额
    BtcValue    int64  // BTC 粉尘金额
    Age         int64  // 封印年龄 (区块高度)
}

// RGB20WithdrawParams 提现参数
type RGB20WithdrawParams struct {
    Amount         int64          // 提现 USDT 金额
    RecipientAddr  string         // 用户 BTC 地址
    SealUtxo       wire.OutPoint  // 选择的封印 UTXO
    FeeUtxo        wire.OutPoint  // 手续费 UTXO
    FeeRate        int64          // sat/vByte
    Chain33TxHash  []byte         // rgbx 广播承诺数据
}

// RGB20WithdrawResult 提现构造结果
type RGB20WithdrawResult struct {
    UnsignedTx     *wire.MsgTx    // BTC 交易
    Consignment    []byte         // RGB Consignment 数据
    BroadcastOpRet []byte         // rgbx 广播承诺 OP_RETURN
    NewSeal        wire.OutPoint  // 用户侧新封印
    ChangeSeal     wire.OutPoint  // 找零封印
}
```

---

## 5. 核心流程设计

### 5.1 充值流程 (RGB20 USDT → Chain33)

```
用户在 RGB 钱包操作
      │
      ▼
┌─────────────────────────────────────────────────────────────┐
│ BTC 交易 (标准 RGB20 USDT 转账)                               │
│                                                              │
│   Inputs:                                                    │
│     [用户封印 UTXO] + [BTC 手续费 UTXO]                       │
│                                                              │
│   Outputs:                                                   │
│     [TSS-P2WPKH 封印 UTXO]  ← 桥的新封印 (BtcValue=546 sat)  │
│     [OP_RETURN 广播承诺]     ← 用户补充的 chain33 地址         │
│     [找零封印 UTXO]          ← 如有                           │
│                                                              │
│   Commitment: Opret/Tapret (RGB 标准格式)                     │
│     → 嵌入 RGB20 State Transition + Consignment              │
└──────────────────────────────────────────────────────────────┘
      │
      ▼ BTC 交易广播到网络
      │
┌─────▼──────────────────────────────────────────────────────┐
│ neutrino 节点处理                                            │
│                                                              │
│ 1. BTC 监听器检测到 TSS 地址收到新的 UTXO                     │
│                                                              │
│ 2. RGB20 适配器解析 BTC 交易:                                 │
│    ├─ ParseDepositFromBtcTx()                               │
│    ├─ 检测 commitment 类型 (Opret/Tapret)                     │
│    ├─ strict_types 解码 Consignment                         │
│    ├─ 比对 RGB20 Schema: 确认为 USDT 合约                     │
│    ├─ 提取 Assignment: 转入的 USDT 金额                       │
│    ├─ 提取 Valency: 新封印 UTXO 对应哪个 output               │
│    └─ 验证: Schema 合规 + 无双重封印 + 金额一致               │
│                                                              │
│ 3. 等待 6 个区块确认                                          │
│                                                              │
│ 4. 构造 BTC SPV 证明 (复用现有逻辑)                            │
│                                                              │
│ 5. 提交 rgbx.Deposit:                                        │
│    {                                                         │
│      assetSymbol:    "RGB20_USDT"                            │
│      amount:         (RGB 适配器提取的 USDT 金额)             │
│      depositAddress: (广播承诺中用户指定的 chain33 地址)       │
│      txProof:        (BTC SPV 证明)                          │
│    }                                                         │
│                                                              │
│ 6. 链上铸币 X.RGB20_USDT 到用户地址                           │
└──────────────────────────────────────────────────────────────┘
```

#### 5.1.1 充值 commitment 的双层设计

RGB20 充值交易实际包含两层 commitment：

```
BTC 交易
├── RGB 层 commitment (Opret/Tapret)
│   └── RGB20 Consignment 的 commitment hash
│       → RGB 适配器解析，获取 USDT 金额和封印信息
│
└── 广播层 commitment (OP_RETURN，rgbx 已有格式)
    └── "rgbx:deposit:RGB20_USDT:<chain33Addr>"
        → rgbx 合约验证，确定资产类型和收款地址
        → 格式与 BTC 充值兼容，仅 assetSymbol 不同
```

### 5.2 提现流程 (Chain33 → RGB20 USDT)

```
用户调用 rgbx.Withdraw
      │
      ▼
┌──────────────┐    ┌────────────────────────────────────────┐
│ rgbx 合约     │───▶│ 创建 PendingTx (与 BTC 提现相同结构)     │
│ 锁定 X.RGB20_ │    │ assetSymbol = "RGB20_USDT"            │
│ USDT          │    │ amount = 提现 USDT 数量               │
│              │    │ destinationAddr = 用户 BTC 地址          │
└──────────────┘    └────────────────────────────────────────┘
      │
      ▼
┌──────────────────────────────────────────────────────────────┐
│ neutrino 节点处理                                              │
│                                                               │
│ 1. 拉取 PendingTx，识别为 RGB20_USDT 提现                     │
│                                                               │
│ 2. RGB20 适配器: ListAvailableSeals()                         │
│    → 列出所有封印余额 ≥ 提现金额的 UTXO                        │
│                                                               │
│ 3. 选择封印 UTXO + 手续费 UTXO                                 │
│                                                               │
│ 4. RGB20 适配器: BuildWithdrawalTx()                          │
│    ├─ 调用 RGB Core 构造 State Transition                    │
│    │   (Contractum → AluVM → strict_types 编码)              │
│    ├─ 生成 Opret/Tapret commitment                            │
│    ├─ 构造 BTC 交易模板:                                      │
│    │   Inputs:  [封印 UTXO] + [手续费 UTXO]                   │
│    │   Outputs: [用户封印] + [找零封印] + [OP_RETURN 广播]    │
│    └─ 生成 rgbx 广播承诺: rgbx:withdraw:chain33Hash           │
│                                                               │
│ 5. Sticky 封印保护 (类比 Sticky 末输入):                       │
│    封印 UTXO 必须固定，重试不可更换                             │
│                                                               │
│ 6. TSS 签名 → 广播 BTC 交易 → 等待确认                         │
│                                                               │
│ 7. 确认后: 构造 SPV 证明 → 提交 rgbx.Confirm 结算              │
│    → 合约销毁锁定的 X.RGB20_USDT                               │
└──────────────────────────────────────────────────────────────┘
```

### 5.3 rgbx 合约侧的变更

合约层需要的改动非常小：

| 变更点 | 说明 |
|--------|------|
| `checkDeposit` 增加资产分支 | `RGB20_USDT` 时，金额验证不检查 BTC 输出金额（USDT 金额来自适配层），仅校验 SPV 证明 + 广播承诺格式 |
| `validateDepositTxContent` 扩展 | 对 `RGB20_USDT` 跳过 "对 TSS 地址输出累加求和" 的验证（金额已在适配层确认） |
| 新增 `RGBUSDTSymbol` 常量 | `types/asset.go` 中定义 |
| **其他均复用** | SPV 验证、OP_RETURN 广播承诺解析、Pending 状态机、Confirm 结算逻辑全部不变 |

---

## 6. 数据流与格式转换

### 6.1 核心转换矩阵

```
┌──────────────────────────────────────────────────────────────────┐
│              适配层：RGB 标准协议 → rgbx 格式的转换                 │
│                                                                   │
│  ┌─────────────────────┐          ┌─────────────────────┐         │
│  │ RGB 标准协议        │          │ rgbx 合约格式        │         │
│  ├─────────────────────┤          ├─────────────────────┤         │
│  │ 充值金额来源:        │  转换    │ amount:              │         │
│  │ Consignment →       │ ──────▶ │  RGB 状态转移提取的   │         │
│  │ OwnedState →        │         │  USDT 数量            │         │
│  │ FungibleToken       │         │                      │         │
│  ├─────────────────────┤          ├─────────────────────┤         │
│  │ 目标地址:            │  转换    │ depositAddress:      │         │
│  │ 广播 OP_RETURN 中的  │ ──────▶ │  提取的 chain33 地址   │         │
│  │ chain33 地址         │         │                      │         │
│  ├─────────────────────┤          ├─────────────────────┤         │
│  │ 资产类型识别:         │  转换    │ assetSymbol:         │         │
│  │ RGB ContractID       │ ──────▶ │  "RGB20_USDT"        │         │
│  │ == USDT 合约ID       │         │                      │         │
│  ├─────────────────────┤          ├─────────────────────┤         │
│  │ 封印 UTXO:           │  转换    │ 链下 Sticky 记录      │         │
│  │ Valency[0] → vout   │ ──────▶ │ (链上不存储封印信息)   │         │
│  └─────────────────────┘          └─────────────────────┘         │
│                                                                   │
│  rgbx 合约不关心字节是怎么转换的 —— 它只验证 BTC SPV 无误即可       │
└──────────────────────────────────────────────────────────────────┘
```

### 6.2 Opret vs Tapret commitment

RGB 标准协议支持两种 commitment 方式：

| | Opret | Tapret |
|---|---|---|
| **实现方式** | `OP_RETURN <hash>` | Taproot script-path 调整 |
| **链上可见性** | 显式 OP_RETURN 输出 | 只在见证数据中出现 |
| **BTC 交易大小** | ~50 bytes 额外 | ~16 bytes 额外 (更省) |
| **适配器解析** | 遍历 TxOut 查找 OP_RETURN | 遍历 witness 查找 tapscript |
| **当前主流** | 兼容性最好 | 隐私性和费用更优 |

适配器需同时支持两种方式，优先检测 Opret，再检测 Tapret。

### 6.3 广播承诺（用户自定 OP_RETURN）

RGB20 充值交易的 BTC 部分还有一个额外的 OP_RETURN（用户自己加的广播承诺），用于指定资产类型和 Chain33 收款地址：

```
格式:
  OP_RETURN "rgbx:deposit:<assetSymbol>:<chain33Address>"

示例:
  OP_RETURN "rgbx:deposit:RGB20_USDT:1ABC..."

  ┌──────────┬────────────┬───────────────────────────────┐
  │ "rgbx:"  │ "deposit:" │ "RGB20_USDT" : "1ABC..."     │
  │ 协议前缀 │ 操作类型    │ 资产符号      目标 chain33 地址 │
  └──────────┴────────────┴───────────────────────────────┘

与 BTC 充值兼容:
  BTC 充值:      "rgbx:deposit:1ABC..."   (assetSymbol 默认 "BTC")
  RGB20 充值:    "rgbx:deposit:RGB20_USDT:1ABC..."

解析规则:
  1. 找到所有 OP_RETURN 输出
  2. 一个是以 rgbx:deposit: 开头的 → 广播承诺 (rgbx 合约验证用)
  3. 另一个是 Opret 格式的 → RGB 协议 commitment (适配器解析用)
```

---

## 7. Neutrino 节点改造

### 7.1 新增文件结构

```
plugin/dapp/lightclient/rpc/lightclient/neutrino/
├── btcwallet.go        # 已有: BTC 交易监听、OP_RETURN 分类
├── bitcoin.go          # 修改: 充值处理增加 RGB20 分支
├── tss.go              # 修改: 签名支持多密钥组
├── rgbx.go             # 已有: Pending 交易拉取
├── config.go           # 修改: 添加 RGB20 配置
├── client.go           # 修改: 初始化 RGB20 适配器
│
├── rgb20/              # 新增: RGB20 适配层
│   ├── adapter.go      # RGB20Adapter 接口 + 实现
│   ├── commitment.go   # Opret/Tapret commitment 解析
│   ├── consignment.go  # strict_types 解码 + Consignment 解析
│   ├── seal.go         # TSS 封印 UTXO 索引管理
│   └── proxy.go        # RGB Proxy gRPC 客户端
│
├── rgb20deposit.go     # 新增: RGB20 充值处理器
├── rgb20withdraw.go    # 新增: RGB20 提现处理器
└── rgbx/rpc/types.go   # 修改: 新增 RGB20 相关 RPC 类型

### 7.2 充值处理分支

```
btcwallet.monitorTransactions()
      │
      ▼
analyzeTransaction()
      │
      ├── 有输出到 TSS-BTC 地址? → BTC 充值 (已有逻辑)
      │
      ├── 有输出到 TSS-RGB20 地址? → 进入 RGB20 分支
      │       │
      │       ▼
      │   rgb20deposit.processRgb20Deposit()
      │       │
      │       ├─ rgb20.ParseDepositFromBtcTx()  ← RGB Proxy 调用
      │       │    • 解析 Opret/Tapret commitment
      │       │    • 解码 Consignment
      │       │    • 验证 RGB20 Schema
      │       │    • 提取 USDT 金额 + 封印 UTXO
      │       │
      │       ├─ parseBroadcastOpReturn()       ← 已有的 OP_RETURN 解析
      │       │    • 提取 assetSymbol 和 chain33 目标地址
      │       │
      │       ├─ 等待 6 确认 → buildBtcTxProof() ← 已有逻辑
      │       │
      │       └─ submitRgbxDeposit()            ← 已有逻辑(复用)
      │            deposit.AssetSymbol = "RGB20_USDT"
      │            deposit.Amount      = 适配器提取的金额
      │
      └── 其他 → 忽略或内部操作
```

### 7.3 提现处理分支

```
rgbx.pullPendingTx()
      │
      ▼
pendingTx.AssetSymbol == "RGB20_USDT"?
      │
      ├── 是 → rgb20withdraw.processRgb20Withdraw()
      │       │
      │       ├─ rgb20.ListAvailableSeals()     ← 查询可用封印
      │       ├─ 选择封印 + 手续费UTXO
      │       ├─ rgb20.BuildWithdrawalTx()      ← RGB Proxy 构造
      │       │    • 生成 RGB20 State Transition
      │       │    • 生成 Opret/Tapret commitment
      │       │    • 生成广播 OP_RETURN
      │       ├─ Sticky 封印记录
      │       ├─ TSS 签名 → 广播
      │       └─ 确认后: rgbx.Confirm 结算
      │
      └── 否 → 进入 BTC 提现逻辑 (已有)
```

### 7.4 配置扩展

```toml
[neutrino]
IsOfficialNode = true
BlockConfirmations = 6

# BTC 桥 (已有)
[neutrino.Tss.BTC]
Peers = ["node1", "node2", "node3", "node4", "node5"]
Threshold = 3

# RGB20 USDT 桥 (新增)
[neutrino.Tss.RGB20_USDT]
Peers = ["node1", "node2", "node3", "node4", "node5"]
Threshold = 3

# RGB20 适配层配置 (新增)
[neutrino.RGB20]
# RGB Proxy 地址 (每个官方节点运行一个 RGB Proxy)
ProxyAddr = "127.0.0.1:8080"
# 或使用内嵌库 (需 CGO/Rust 交叉编译)
# LibraryPath = "/usr/local/lib/librgb.so"

# 注册要支持的 RGB 合约
[[neutrino.RGB20.Contracts]]
ContractID = "rgb:2dwGxY...-USDT"       # RGB 合约 ID
SchemaType = "RGB20"                     # Schema 类型
AssetSymbol = "RGB20_USDT"               # Chain33 上的符号
Precision   = 6                          # USDT 精度
# 该合约对应的链上 TSS 信息 (需和 CommitDKG 一致)
TssCrossChainKey = "RGB20_USDT"

# 手续费 UTXO 池
[neutrino.RGB20.FeePool]
MinUtxos = 10
TargetAmount = 100000
RefillThreshold = 5
```

---

## 8. 安全模型

### 8.1 威胁矩阵

| 威胁 | 攻击者 | 影响 | 防护 | 残余风险 |
|------|--------|------|------|---------|
| **伪造 RGB20 充值** | 用户 | 骗取 wrapped USDT | RGB Core 验证 + Guardian 多节点确认 | RGB Core 0day |
| **篡改 USDT 金额** | 用户 | 少转多报 | 适配器严格从 Consignment 提取 | 适配器解析 bug |
| **双花同一封印** | 用户 | 同一 RGB 状态充值两次 | Seal 追踪器查重 + BTC SPV | 链重组 |
| **封印 UTXO 替换** | 官方节点 | 提现双花 | Sticky 封印 + TSS 多节点验证 | 官方+TSS 多数共谋 |
| **RGB Schema 升级** | 协议演进 | 旧版适配器无法解析 | Schema 版本白名单 | 过渡期不一致 |
| **Commitment 伪造** | 用户 | 非 RGB20 交易被识别为充值 | contractID 精确匹配 | 适配器配置错误 |
| **手续费不足** | 运营 | 提现无法确认 | 手续费 UTXO 池自动补充 | 极端费率 |

### 8.2 分层验证模型

```
Layer 1 (链上, rgbx 合约):
  ✅ BTC SPV Merkle 证明
  ✅ 广播 OP_RETURN 格式
  ✅ 确认数 ≥ 6
  ✅ 防重放 (depositUsedKey)

Layer 2 (链下, RGB20 适配器):
  ✅ Opret/Tapret commitment 解析成功
  ✅ Consignment strict_types 解码成功
  ✅ ContractID 匹配已注册的 USDT 合约
  ✅ RGB20 Schema 验证通过 (AluVM)
  ✅ 封印 UTXO 到 TSS 地址
  ✅ 封印 UTXO 未被其他充值使用

Layer 3 (链下, Guardian 共识):
  ✅ 多个 Guardian 独立运行适配器验证
  ✅ ≥ t 个确认才提交 Deposit
```

### 8.3 与 BTC 桥的安全差异

| | BTC 桥 | RGB20 桥 |
|---|---|---|
| **链上金额验证** | ✅ 验证 TSS 输出金额 | ⚠️ 链上无法验证（金额在 RGB 状态） |
| **链下金额验证** | 不需要 | ✅ RGB Core 验证（适配层） |
| **多签确认充值** | 不需要（链上已验证） | ✅ 需要 ≥ t 个 guardian 确认 |
| **总信任模型** | 信任 BTC PoW | 信任 BTC PoW + RGB Core + Guardian 多数 |

关键差异：BTC 桥的**金额是链上可验证的**，RGB20 桥的**金额只能在链下验证**。因此 RGB20 桥**必须额外依赖 Guardian 多签确认充值金额**。

---

## 9. 配置与部署

### 9.1 部署前提

1. **RGB Proxy 部署**：每个官方节点服务器上运行 RGB Proxy（连接 Bitcoin Core + RGB Core 库）
2. **RGB 合约注册**：配置要支持的 RGB20 USDT 的 ContractID、SchemaID、精度
3. **RGB20 DKG 完成**：Guardian 完成 RGB20_USDT 的 TSS 密钥生成
4. **手续费 UTXO 初始化**：从 TSS-BTC 地址转入一笔 BTC 到 TSS-RGB20 地址作为手续费池
5. **广播承诺规范约定**：用户充值时需附带 `rgbx:deposit:RGB20_USDT:<addr>` 的 OP_RETURN

### 9.2 部署拓扑

```
每个官方节点:
┌──────────────────────────────────────────────┐
│                                              │
│  Chain33 节点                                 │
│  ├─ rgbx 合约                                 │
│  ├─ lightclient 合约                          │
│  └─ TSS 服务 (BTC 密钥组 + RGB20 密钥组)       │
│                                              │
│  Neutrino 服务                                │
│  ├─ BTC 监听器 (已有)                          │
│  ├─ BTC 交易构造器 (已有)                       │
│  ├─ RGB20 适配层 (新增) ───gRPC───▶           │
│  │                                    RGB Proxy
│  ├─ RGB20 充值处理器 (新增)             (独立进程)
│  └─ RGB20 提现处理器 (新增)              Rust 实现
│                                          ├─ RGB Core 库
│  Bitcoin Core / Neutrino                 ├─ strict_types 编解码
│  (BTC P2P 网络同步)                       ├─ AluVM 虚拟机
│                                          └─ RGB20 Schema
└──────────────────────────────────────────────┘

推荐: 3 官方节点 + 4 非官方节点, 阈值 = 5
每个官方节点独立运行 RGB Proxy
```

---

## 10. 代码变更清单

### 10.1 Phase 1: rgbx 合约层微调

| 文件 | 变更 | 内容 |
|------|------|------|
| `types/asset.go` | 修改 | 添加 `RGB20USDTSymbol = "RGB20_USDT"` |
| `executor/checktx.go` | 修改 | `checkDeposit` 对 `RGB20_USDT` 跳过 BTC 输出金额校验（金额在适配层验证） |
| `executor/validate_proof.go` | 修改 | `validateDepositTxContent` 增加资产类型分支 |
| `proto/rgbx.proto` | 修改 | OP_RETURN 解析兼容 `assetSymbol:addr` 格式 |

### 10.2 Phase 2: RGB20 适配层（全新）

| 文件 | 内容 |
|------|------|
| `neutrino/rgb20/adapter.go` | RGB20Adapter 接口 + RGB Proxy 调用实现 |
| `neutrino/rgb20/commitment.go` | Opret/Tapret commitment 检测与解析 |
| `neutrino/rgb20/consignment.go` | strict_types 解码、Consignment 遍历、金额提取 |
| `neutrino/rgb20/seal.go` | TSS 封印 UTXO 索引管理（DB 存储） |
| `neutrino/rgb20/proxy.go` | RGB Proxy gRPC 客户端封装 |
| `neutrino/rgb20deposit.go` | RGB20 充值监听、验证、Deposit 提交 |
| `neutrino/rgb20withdraw.go` | RGB20 提现构造、Sticky 封印、Confirm 结算 |

### 10.3 Phase 3: 已有模块扩展

| 文件 | 变更 | 内容 |
|------|------|------|
| `neutrino/config.go` | 修改 | 添加 `RGB20Config` |
| `neutrino/client.go` | 修改 | 初始化 RGB20 适配器 |
| `neutrino/bitcoin.go` | 修改 | 充值处理增加 RGB20 分支调度 |
| `neutrino/tss.go` | 修改 | 多密钥组索引（按 assetSymbol 区分签名会话） |
| `neutrino/btcwallet.go` | 修改 | OP_RETURN 分类增加 `RGB20_USDT` 识别 |

### 10.4 Phase 4: 测试与部署

| 文件 | 内容 |
|------|------|
| `executor/crosschain_test.go` | RGB20 充提测试 |
| `neutrino/rgb20/adapter_test.go` | 适配器单元测试 (mock RGB Proxy) |
| `cmd/ci/docker-compose.yml` | 测试环境增加 RGB Proxy 容器 |

---

## 附录 A: RGB 协议参考资料

- RGB 协议规范: [https://rgb.tech/](https://rgb.tech/)
- LNP/BP 标准协会: [https://www.lnp-bp.org/](https://www.lnp-bp.org/)
- RGB Core (Rust): [https://github.com/RGB-WG/rgb-core](https://github.com/RGB-WG/rgb-core)
- RGB20 Schema 规范: RGB 同质化代币标准接口
- RGB Proxy: 社区维护的 RGB Core gRPC 服务封装
- AluVM: RGB 智能合约虚拟机 [https://www.aluvm.org/](https://www.aluvm.org/)

## 附录 B: 与 rgbx 协议的差异总结

```
             rgbx (已有)              RGB 标准协议 (需适配)
             ─────────                ───────────────────
代币定义       proto MintAsset           Contractum RGB20 Schema
状态编码       protobuf                  strict_types
承诺格式       rgbx:deposit:xxx          Opret / Tapret
验证引擎       Go 合约定制逻辑            AluVM 字节码
资产溯源       genesisOut UTXO           Genesis State Transition
转移证明       UtxoSpendingProof         Consignment + Attachment

适配层的本质: 做 strict_types → protobuf 的语义翻译
```

---

*文档结束*
