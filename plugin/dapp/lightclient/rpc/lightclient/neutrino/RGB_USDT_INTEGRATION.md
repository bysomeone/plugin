# RGB USDT 跨链集成技术方案

**版本**: v1.0  
**适用对象**: 开发者、架构师  
**最后更新**: 2026-07-23  
**前置依赖**: [BTC 跨链桥技术架构文档](./TECHNICAL.md)

---

## 目录

1. [背景与动机](#1-背景与动机)
2. [RGB 协议概要](#2-rgb-协议概要)
3. [架构设计](#3-架构设计)
4. [核心流程设计](#4-核心流程设计)
5. [数据结构变更](#5-数据结构变更)
6. [Neutrino 官方节点改造](#6-neutrino-官方节点改造)
7. [安全模型](#7-安全模型)
8. [配置与部署](#8-配置与部署)
9. [代码变更清单](#9-代码变更清单)

---

## 1. 背景与动机

### 1.1 现状

当前跨链桥仅支持 BTC 原生币的充提。rgbx 合约已具备多资产管理框架（`CrossChainInfo` 按 `assetSymbol` 索引），为扩展新资产预留了接口。

### 1.2 目标

支持 RGB 协议上发行的 USDT 在 Chain33 与 Bitcoin 网络之间双向流通：

- **充值**：用户将 RGB USDT 转入 TSS 控制的 UTXO → Chain33 铸造对应数量的 wrapped USDT
- **提现**：用户在 Chain33 销毁 wrapped USDT → TSS 构造 BTC 交易将 RGB USDT 转出到用户地址

### 1.3 核心差异：BTC vs RGB USDT

| 维度 | BTC | RGB USDT |
|------|-----|----------|
| **资产载体** | BTC UTXO 的 value 字段 | RGB 状态机（client-side validated） |
| **转账语义** | BTC 输出金额 = 转账金额 | BTC 输出金额仅覆盖手续费，RGB 状态转移携带实际金额 |
| **所有权证明** | 控制 UTXO 私钥 | 控制密封 RGB 状态的 UTXO 私钥 + RGB 状态转移历史 |
| **交易大小** | 标准 P2WPKH (~140 vBytes) | 额外 RGB 承诺数据（OP_RETURN ~50-200 bytes） |
| **验证方式** | 仅需 BTC SPV 证明 | BTC SPV 证明 + RGB 客户端验证 |
| **地址隔离** | 可共用 TSS 地址 | 建议独立 TSS 地址（UTXO 管理隔离） |

---

## 2. RGB 协议概要

### 2.1 核心概念

RGB 是 Bitcoin 上的客户端验证智能合约系统，关键概念：

```
┌─────────────────────────────────────────────────────────────────┐
│                    RGB 资产状态转移模型                           │
│                                                                  │
│  ┌─────────────────┐          ┌─────────────────┐               │
│  │   BTC Tx #N     │          │   BTC Tx #N+1   │               │
│  │                 │          │                 │               │
│  │ Inputs:         │          │ Inputs:         │               │
│  │  [UTXO-A]  ◄─── Single-Use Seal ──── 关闭旧封印              │
│  │                 │          │                 │               │
│  │ Outputs:        │          │ Outputs:        │               │
│  │  [UTXO-B]  ──── Single-Use Seal ────► 打开新封印              │
│  │  OP_RETURN      │          │  OP_RETURN      │               │
│  │  (RGB commitment)│          │  (RGB commitment)│              │
│  └─────────────────┘          └─────────────────┘               │
│                                                                  │
│  RGB State:                                                     │
│  Owner=UTXO-A             RGB State:                            │
│  Amount=100 USDT    →     Owner=UTXO-B                          │
│                           Amount=100 USDT (or split)             │
└─────────────────────────────────────────────────────────────────┘
```

### 2.2 与跨链桥的关系

| RGB 概念 | 跨链桥中的含义 |
|----------|---------------|
| **Single-Use Seal** | BTC UTXO 作为"封印"，锁定 RGB 状态 |
| **State Transition** | RGB USDT 转账 = 打开旧封印 + 创建新封印 |
| **Commitment** | OP_RETURN 中嵌入 RGB 状态转移的承诺哈希 |
| **Client-Side Validation** | 官方节点需运行 RGB 客户端验证每次转移 |

### 2.3 RGB 客户端验证的必要性

与 BTC 不同，RGB 资产转账的"有效性"不能仅通过 BTC 链上数据判断：

```
BTC 交易已确认
      │
      ▼
┌─────────────────┐     ┌─────────────────┐     ┌─────────────────┐
│ 检查 BTC 层面   │     │ 检查 RGB 层面   │     │ 验证结论         │
│ • 交易在区块中  │     │ • 封印UTXO正确  │     │                 │
│ • 矿工费合理    │     │ • 状态转移合法  │     │ ✅ 充值有效     │
│ • 确认数足够    │     │ • 金额正确      │───▶│ ❌ 拒绝充值     │
│                 │     │ • 无双重支付    │     │                 │
└─────────────────┘     └─────────────────┘     └─────────────────┘
```

---

## 3. 架构设计

### 3.1 整体架构

```
┌───────────────────────────────────────────────────────────────────────────────┐
│                              Chain33 主链                                      │
│                                                                               │
│  ┌──────────────────────┐  ┌──────────────────────┐  ┌──────────────────────┐ │
│  │ lightclient 合约      │  │ rgbx 合约             │  │ TSS 网络 (P2P)       │ │
│  │                      │  │                      │  │                      │ │
│  │ BTC区块头 (共用)      │  │ CrossChainInfo:      │  │ DKG-1: BTC 密钥组    │ │
│  │ SPV 验证 (共用)       │  │  ├─ BTC → X.BTC     │  │ DKG-2: RGB_USDT 密钥 │ │
│  │                      │  │  └─ RGB_USDT → X.RGB_USDT│                  │ │
│  └──────────────────────┘  └──────────────────────┘  └──────────────────────┘ │
└───────────────────────────────────────────────────────────────────────────────┘

┌───────────────────────────────────────────────────────────────────────────────┐
│                              Bitcoin 主链                                      │
│                                                                               │
│  ┌──────────────────────────────────────────────────────────────────────┐    │
│  │                    Neutrino 轻客户端 (官方节点)                         │    │
│  │                                                                       │    │
│  │  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐                 │    │
│  │  │ BTC 监听器   │  │ RGB 客户端   │  │ 交易构造器   │                 │    │
│  │  │ • BTC 充值    │  │ • 状态验证   │  │ • BTC 提现   │                 │    │
│  │  │ • 区块同步    │  │ • 封印追踪   │  │ • RGB USDT   │                 │    │
│  │  │              │  │ • Schema 验证│  │   提现构造   │                 │    │
│  │  └──────────────┘  └──────────────┘  └──────────────┘                 │    │
│  └──────────────────────────────────────────────────────────────────────┘    │
│                                                                               │
│  ┌─────────────────────┐     ┌──────────────────────────┐                     │
│  │ BTC TSS 地址 (BTC)  │     │ BTC TSS 地址 (RGB_USDT)  │                     │
│  │ • 存储 BTC 价值     │     │ • 仅存储 BTC 粉尘/手续费 │                     │
│  │ • 无 RGB 状态       │     │ • 密封 RGB USDT 状态     │                     │
│  └─────────────────────┘     └──────────────────────────┘                     │
└───────────────────────────────────────────────────────────────────────────────┘
```

### 3.2 模块职责扩展

| 模块 | 原有职责 | 新增职责 |
|------|---------|---------|
| **rgbx 合约** | BTC Deposits/Withdraw | 通用化资产类型，支持 RGB_USDT 充提 |
| **lightclient 合约** | BTC 区块头存储 | 无需变更 |
| **neutrino 官方节点** | BTC 监听、交易构造、SPV 提交 | RGB 客户端验证、RGB USDT 沉积检测、RGB 提现构造 |
| **TSS 网络** | BTC 提现签名 | 新增 RGB_USDT 密钥组 DKG，RGB 提现签名 |

### 3.3 资产符号体系

```
跨链资产命名规则: <CrossChainAssetPrefix>.<Symbol>

配置: CrossChainAssetPrefix = "X" (默认)

┌──────────────────┬──────────────────┬──────────────────────────────┐
│ 原始资产          │ Chain33 封装资产  │ 说明                          │
├──────────────────┼──────────────────┼──────────────────────────────┤
│ BTC              │ X.BTC            │ 已有，BTC 原生币               │
│ RGB_USDT         │ X.RGB_USDT       │ 新增，RGB 协议上的 USDT        │
│ (未来扩展)        │ X.XXX            │ 按相同模式扩展                │
└──────────────────┴──────────────────┴──────────────────────────────┘
```

---

## 4. 核心流程设计

### 4.1 充值流程 (RGB USDT → Chain33)

```
用户在 RGB 钱包操作
      │
      ▼
┌─────────────────────────────────────────────────────────────────────┐
│ 1. 构造 BTC 交易 (RGB USDT 转账)                                     │
│    • Input: 用户当前封印 RGB USDT 的 UTXO                            │
│    • Output[0]: TSS-RGB_USDT 地址 (新封印，金额=粉尘 546 sat)        │
│    • Output[1]: OP_RETURN "rgbx:deposit:<assetSymbol>:<chain33Addr>"│
│    • Output[N]: 找零 (如有，剩余 USDT 的新封印)                       │
│    • RGB 承诺: 嵌入 RGB 状态转移数据                                  │
│    • BTC 手续费: 从独立的 BTC UTXO 提供                               │
└─────────────────────────────────────────────────────────────────────┘
      │
      ▼
┌──────────────┐    ┌──────────────┐    ┌──────────────┐
│ BTC 网络广播  │───▶│ Neutrino     │───▶│ RGB 客户端   │
│              │    │ 监听 BTC 交易 │    │ 验证状态转移 │
└──────────────┘    └──────────────┘    └──────┬───────┘
                                              │
                                    ┌─────────▼─────────┐
                                    │ RGB 验证通过?      │
                                    │ • Schema 合规     │
                                    │ • 封印UTXO 正确    │
                                    │ • 金额正确         │
                                    │ • 无双重支付       │
                                    └────────┬──────────┘
                                             │
                                    ┌────────▼────────┐
                                    │ 等待 6 个区块确认│
                                    └────────┬────────┘
                                             │
                                    ┌────────▼──────────────┐
                                    │ 构造 BTC SPV 证明      │
                                    │ + 提取 OP_RETURN 数据  │
                                    │ + 对 TSS 输出金额求和  │
                                    └────────┬──────────────┘
                                             │
                                    ┌────────▼──────────────┐
                                    │ rgbx.Deposit           │
                                    │ • assetSymbol=RGB_USDT│
                                    │ • amount=USDT 金额    │
                                    │ • depositAddress=     │
                                    │   chain33地址          │
                                    │ • BtcTxProof           │
                                    └────────┬──────────────┘
                                             │
                                    ┌────────▼──────────────┐
                                    │ 铸币 X.RGB_USDT       │
                                    │ 到指定 Chain33 地址    │
                                    └──────────────────────┘
```

#### 4.1.1 OP_RETURN 数据格式

```
充值的 OP_RETURN (兼容原有格式):
┌──────────┬──────────┬─────────────────────────┐
│ prefix   │ action   │ payload                 │
│ "rgbx:"  │"deposit:"│ "<chain33Address>"      │
└──────────┴──────────┴─────────────────────────┘

带资产类型的 OP_RETURN (新格式):
┌──────────┬──────────┬──────────────────────────────────────────────┐
│ prefix   │ action   │ payload                                      │
│ "rgbx:"  │"deposit:"│ "<assetSymbol>:<chain33Address>"             │
└──────────┴──────────┴──────────────────────────────────────────────┘

示例:
  BTC 充值:      "rgbx:deposit:1ABC..."   (兼容旧格式，assetSymbol 默认 BTC)
  RGB_USDT 充值: "rgbx:deposit:RGB_USDT:1ABC..."
```

#### 4.1.2 充值金额提取

与 BTC 充值不同，RGB USDT 的金额**不在 BTC 交易输出中**，而在 RGB 状态转移数据中。

```
BTC 充值 (现有):
  遍历 btcTx.TxOut → 匹配 PkScript == TSS PkScript → 累加 Value

RGB USDT 充值 (新增):
  BTC Tx 确认 + SPV 验证通过后
    → RGB 客户端解析状态转移数据
    → 提取转入 TSS 封印的 USDT 金额
    → amount = RGB 状态转移金额 (非 BTC 金额)
```

**链上验证策略**：由于 Chain33 合约无法运行 RGB 客户端进行完整验证，采用以下分层验证：

| 层级 | 验证内容 | 执行方 |
|------|---------|--------|
| **BTC 层 (链上)** | BTC tx 在区块中、Merkle 证明、确认数 | rgbx 合约 |
| **承诺层 (链上)** | OP_RETURN 格式正确、目标地址有效 | rgbx 合约 |
| **RGB 层 (链下)** | RGB 状态转移合法、金额正确、无双重支付 | 官方节点 RGB 客户端 |
| **共识层 (链下)** | 多 guardian 对 RGB 状态转移结果达成共识 | Guardian 网络 |

### 4.2 提现流程 (Chain33 → RGB USDT)

```
用户调用 rgbx.Withdraw
      │
      ▼
┌──────────────┐    ┌──────────────────────────────────────────────────┐
│ rgbx 合约     │───▶│ 创建 PendingTx                                     │
│ 检查余额      │    │ • assetSymbol = "RGB_USDT" (或缩写)               │
│ 锁定资产      │    │ • amount = 提现 USDT 数量                          │
│              │    │ • destinationAddr = 目标 BTC 地址 (接收 RGB 封印)   │
│              │    │ • feeRate = BTC 手续费率 (sat/vByte)                │
└──────────────┘    └──────────────────────────────────────────────────┘
      │
      ▼
┌──────────────────────────────────────────────────────────────────────┐
│ 官方节点监听到 PendingTx                                               │
│                                                                      │
│ 1. 读取 CrossChainInfo 获取 RGB_USDT 的 TSS 地址和 PkScript           │
│ 2. RGB 客户端查询 TSS 地址下可用的 RGB USDT 封印 UTXO                  │
│ 3. 选择封印 UTXO 作为 BTC 交易输入                                     │
│ 4. 构造 BTC 交易:                                                     │
│                                                                      │
│    Inputs:                                                           │
│      [封印 UTXO-A]  -- 当前持有 RGB USDT 的 UTXO (sticky 保护)        │
│      [手续费 UTXO]  -- 独立的 BTC UTXO，用于支付矿工费                 │
│                                                                      │
│    Outputs:                                                          │
│      [用户 BTC 地址]  -- 新封印，RGB USDT 金额 = 提现金额              │
│      [找零 TSS 地址]  -- 新封印，剩余 USDT (如有)                      │
│      [OP_RETURN]      -- "rgbx:withdraw:<chain33TxHash>"             │
│                                                                      │
│    RGB 承诺:                                                         │
│      嵌入 RGB 状态转移数据 (schema + state transition + proof)        │
│                                                                      │
│    Sticky 末输入:                                                     │
│      末输入 = 封印 UTXO (必须保持，防止更换 RGB 封印)                   │
└──────────────────────────────────────────────────────────────────────┘
      │
      ▼
┌──────────────┐    ┌──────────────────────────────────┐
│ TSS 阈值签名 │───▶│ 非官方节点验证                     │
│ (RGB_USDT    │    │ • Sticky 末输入一致               │
│  密钥组)     │    │ • 输出金额正确                    │
│              │    │ • OP_RETURN 承诺正确              │
└──────────────┘    └──────────────────────────────────┘
      │
      ▼
┌──────────────┐    ┌──────────────┐    ┌──────────────────┐
│ BTC 网络广播  │───▶│ Neutrino     │───▶│ rgbx.Confirm     │
│              │    │ 监听确认     │    │ • 销毁 X.RGB_USDT │
│              │    │ (6 个区块)   │    │ • 清理 sticky    │
└──────────────┘    └──────────────┘    └──────────────────┘
```

#### 4.2.1 BTC 交易费用处理

RGB USDT 提现交易需要 BTC 手续费，但 RGB USDT 封印 UTXO 的 BTC 价值通常仅含粉尘：

```
┌──────────────────────────────────────────────────────────────┐
│                 RGB USDT 提现 — BTC 手续费来源                  │
│                                                              │
│  方案: 独立手续费 UTXO                                        │
│                                                              │
│  BTC Inputs:                                                 │
│  ┌─────────────────────┐  ┌─────────────────────┐            │
│  │ 封印 UTXO (sticky)  │  │ 手续费 UTXO          │            │
│  │ value: 546 sat      │  │ value: ≥ 手续费+粉尘 │            │
│  │ 密封: 1000 USDT     │  │ 无 RGB 状态          │            │
│  └─────────────────────┘  └─────────────────────┘            │
│                                                              │
│  BTC Outputs:                                                │
│  ┌─────────────────────┐  ┌─────────────────────┐            │
│  │ 用户 BTC 地址        │  │ TSS 找零             │            │
│  │ value: 546 sat      │  │ value: 剩余 - fee   │            │
│  │ 新封印: 500 USDT    │  │ 新封印: 500 USDT    │            │
│  └─────────────────────┘  └─────────────────────┘            │
│                                                              │
│  需要维护一个独立的 "手续费 UTXO 池"                           │
│  定期从 TSS-BTC 地址向 TSS-RGB_USDT 地址转入小额 BTC           │
└──────────────────────────────────────────────────────────────┘
```

### 4.3 DKG 初始化流程

RGB USDT 需要独立的 TSS 密钥组：

```
┌─────────────────────────────────────────────────────────────────┐
│                 RGB_USDT DKG 初始化流程                           │
│                                                                  │
│  1. Guardian 节点协商新的 DKG 会话                                 │
│     • assetSymbol = "RGB_USDT"                                  │
│     • 独立的随机种子 (不与 BTC 密钥组关联)                          │
│                                                                  │
│  2. DKG 协议执行 (GG18/GG20)                                      │
│     • 生成 t-of-n 共享公钥                                       │
│     • 派生 P2WPKH 地址 (TSS-RGB_USDT 收款地址)                    │
│                                                                  │
│  3. 各 Guardian 提交 CommitDKG 到 rgbx 合约                       │
│     rgbx.CommitDKG {                                            │
│       assetSymbol: "RGB_USDT"                                   │
│       dkgAddress:  "bc1q..."  // TSS-RGB_USDT 地址               │
│       pkScript:    0x0014... // P2WPKH 锁定脚本                  │
│     }                                                            │
│                                                                  │
│  4. 全部 Guardian 提交后，合约自动创建 CrossChainInfo              │
│     CrossChainInfo {                                            │
│       assetSymbol:   "RGB_USDT"                                 │
│       wrappedSymbol: "X.RGB_USDT"                               │
│       tssAddress:    "bc1q..."                                  │
│       pkScript:      0x0014...                                  │
│     }                                                            │
│                                                                  │
│  5. 跨链桥就绪，可以处理 RGB_USDT 充提                             │
└─────────────────────────────────────────────────────────────────┘
```

---

## 5. 数据结构变更

### 5.1 Proto 定义 (rgbx.proto)

#### 5.1.1 新增 RGB 证明结构

```protobuf
// 新增: RGB 状态转移证明 (链下验证使用，不上链完整数据)
message rgbStateProof {
    string contractId    = 1;  // RGB 合约 ID (如 USDT 合约)
    string schemaId      = 2;  // RGB Schema ID
    uint32 transitionIdx = 3;  // 状态转移索引
    bytes  stateData     = 4;  // RGB 状态转移序列化数据
    bytes  attachment    = 5;  // RGB 附件/证明数据
    string sealUtxo      = 6;  // 旧封印 UTXO (被关闭的封印)
    string newSealUtxo   = 7;  // 新封印 UTXO (目标地址的封印)
}
```

#### 5.1.2 Deposit 结构扩展 (可选，向后兼容)

```protobuf
// depositAsset 已有 assetSymbol 字段，无需变更结构
// 但建议在 OP_RETURN 解析中兼容新旧格式
message depositAsset {
    int64      amount         = 1;
    string     depositAddress = 2;
    string     assetSymbol    = 3;  // 已有字段: "BTC" | "RGB_USDT"
    btcTxProof txProof        = 4;
}
```

#### 5.1.3 新增 RGB 充值验证辅助消息

```protobuf
// 新增: Guardian 对 RGB 充值的验证确认
message rgbDepositConfirmation {
    string btcTxHash       = 1;  // 对应 BTC 交易哈希
    string assetSymbol     = 2;  // 资产符号
    int64  confirmedAmount = 3;  // 确认的 RGB USDT 金额
    string sealUtxo        = 4;  // 新封印 UTXO 位置
    int64  timestamp       = 5;  // 确认时间戳
}
```

### 5.2 类型常量 (types/asset.go)

```go
// 新增跨链资产符号常量
const (
    BTCSymbol     = "BTC"       // 已有
    RGBUSDTSymbol = "RGB_USDT"  // 新增: RGB 协议上的 USDT
)
```

### 5.3 状态存储扩展 (executor/kv.go)

```
新增数据库桶:
├── rgb-deposit-confirmations-    # RGB 充值 guardian 确认
│   └── key: btcTxHash, value: []rgbDepositConfirmation
├── rgb-seal-utxo-                # RGB 封印 UTXO 索引
│   └── key: outPoint, value: {assetSymbol, amount}
└── rgb-withdraw-sticky-seal      # RGB 提现 sticky 封印
    └── key: chain33TxHash, value: sealOutPoint
```

### 5.4 Key 格式化函数

```go
// 新增 key 格式化
func formatRgbDepositConfirmationKey(btcTxHash string) []byte {
    return []byte(rgbDepositConfirmationsPrefix + btcTxHash)
}

func formatRgbSealUtxoKey(outPoint string) []byte {
    return []byte(rgbSealUtxoPrefix + outPoint)
}

func formatRgbStickySealKey(chain33TxHash []byte) []byte {
    return append([]byte(rgbStickySealPrefix), chain33TxHash...)
}
```

---

## 6. Neutrino 官方节点改造

### 6.1 新增 RGB 客户端模块

```
plugin/dapp/lightclient/rpc/lightclient/neutrino/
├── btcwallet.go        # 已有: BTC 钱包管理
├── bitcoin.go          # 已有: BTC 官方节点逻辑
├── tss.go              # 已有: TSS 服务
├── rgbx.go             # 已有: Pending 交易管理
├── rpc.go              # 已有: Chain33 RPC
├── cache.go            # 已有: 内存缓存
├── client.go           # 已有: 客户端初始化
├── config.go           # 修改: 添加 RGB 配置
├── wallet.go           # 已有: 钱包 DB 工具
│
├── rgb/                # 新增: RGB 客户端模块
│   ├── client.go       # RGB 客户端封装 (连接 RGB proxy/库)
│   ├── validate.go     # RGB 状态转移验证
│   ├── seal.go         # 封印 UTXO 管理与追踪
│   └── schema.go       # RGB Schema 定义 (USDT)
│
├── rgbdeposit.go       # 新增: RGB USDT 充值处理
└── rgbwithdraw.go      # 新增: RGB USDT 提现处理
```

### 6.2 RGB 客户端接口设计

```go
// rgb/client.go — RGB 客户端核心接口
package rgb

import (
    "github.com/btcsuite/btcd/wire"
    "github.com/btcsuite/btcd/chaincfg/chainhash"
)

// RgbClient RGB 客户端接口
type RgbClient interface {
    // ValidateDeposit 验证 RGB 充值交易
    // 返回: 转入 TSS 封印的 USDT 金额、新封印位置、错误
    ValidateDeposit(btcTx *wire.MsgTx, tssScript []byte) (*DepositResult, error)

    // BuildWithdrawal 构造 RGB 提现交易 (返回 BTC tx 模板和 RGB 承诺数据)
    BuildWithdrawal(params *WithdrawalParams) (*WithdrawalResult, error)

    // GetSealBalance 获取指定 UTXO 封印的 RGB 资产余额
    GetSealBalance(outPoint wire.OutPoint, contractID string) (int64, error)

    // ListSealUtxos 列出 TSS 控制的可用封印 UTXO 及其余额
    ListSealUtxos(contractID string) ([]SealUtxo, error)

    // VerifyStateTransition 验证 RGB 状态转移的合法性
    VerifyStateTransition(btcTx *wire.MsgTx, contractID string) error
}

type DepositResult struct {
    Amount      int64           // 转入的 USDT 金额
    NewSeal     wire.OutPoint   // 新封印 UTXO
    ContractID  string          // RGB 合约 ID
}

type WithdrawalParams struct {
    Amount         int64             // 提现 USDT 金额
    RecipientAddr  string            // 用户 BTC 地址
    SealUtxo       wire.OutPoint     // 当前封印 UTXO
    FeeUtxo        wire.OutPoint     // 手续费 UTXO
    FeeRate        int64             // 费率 (sat/vByte)
    Chain33TxHash  []byte            // chain33 交易哈希 (用于 OP_RETURN)
    AssetSymbol    string            // 资产符号
}

type WithdrawalResult struct {
    UnsignedTx  *wire.MsgTx     // 未签名的 BTC 交易
    RgbCommitment []byte        // RGB 承诺数据 (嵌入 OP_RETURN)
    NewSeal     wire.OutPoint   // 用户侧新封印
    ChangeSeal  wire.OutPoint   // TSS 找零封印
}

type SealUtxo struct {
    OutPoint    wire.OutPoint
    Amount      int64           // RGB 资产余额
    BtcValue    int64           // BTC 粉尘金额
}
```

### 6.3 充值处理流程 (rgbdeposit.go)

```go
// rgbdeposit.go — RGB USDT 充值处理
package neutrino

// processRgbDeposit 处理 RGB USDT 充值
func (n *Neutrino) processRgbDeposit(btcTx *wire.MsgTx, blockInfo *BlockInfo) error {
    // 1. 解析 OP_RETURN，提取 assetSymbol 和 depositAddress
    assetSymbol, depositAddr, err := parseRgbDepositOpReturn(btcTx)
    if err != nil || assetSymbol == "" {
        return nil // 不是 RGB 充值
    }

    // 2. 获取该资产的 crossChainInfo (TSS PkScript)
    info, err := n.getCrossChainInfo(assetSymbol)
    if err != nil {
        return err
    }

    // 3. 检查是否有输出到 TSS 地址 (封印位置)
    sealOutput := findOutputToScript(btcTx, info.PkScript)
    if sealOutput < 0 {
        return fmt.Errorf("no output to TSS address")
    }

    // 4. RGB 客户端验证状态转移
    result, err := n.rgbClient.ValidateDeposit(btcTx, info.PkScript)
    if err != nil {
        n.log.Error("RGB validation failed", "btcTxHash", btcTx.TxHash(), "err", err)
        return err
    }

    // 5. 等待确认数达标
    if blockInfo.Confirmations < n.cfg.BlockConfirmations {
        n.cachePendingRgbDeposit(btcTx, blockInfo, result)
        return nil
    }

    // 6. 构造 SPV 证明
    proof, err := n.buildBtcTxProof(btcTx, blockInfo)
    if err != nil {
        return err
    }

    // 7. 提交 deposit 交易到 Chain33
    deposit := &rtypes.DepositAsset{
        Amount:         result.Amount,
        DepositAddress: depositAddr,
        AssetSymbol:    assetSymbol,
        TxProof:        proof,
    }
    return n.submitDepositTx(deposit)
}
```

### 6.4 提现处理流程 (rgbwithdraw.go)

```go
// rgbwithdraw.go — RGB USDT 提现处理
package neutrino

// processRgbWithdraw 处理 RGB USDT 提现
func (n *Neutrino) processRgbWithdraw(pending *rtypes.PendingTx) error {
    // 1. 获取跨链信息
    info, err := n.getCrossChainInfo(pending.AssetSymbol)
    if err != nil {
        return err
    }

    // 2. 读取 sticky 记录
    stickySeal, err := n.loadRgbStickySeal(pending.TxHash)
    isRetry := (err == nil && stickySeal != nil)

    // 3. 选择封印 UTXO
    var sealUtxo *SealUtxo
    if isRetry {
        // 重试: 使用 sticky 封印
        sealUtxo = stickySeal
    } else {
        // 首次: 选择可用封印 UTXO
        sealUtxos, err := n.rgbClient.ListSealUtxos(info.AssetSymbol)
        // 选择封印余额 >= pending.Amount 的 UTXO
        sealUtxo = selectBestSealUtxo(sealUtxos, pending.Amount)
    }

    // 4. 选择手续费 UTXO
    feeUtxo, err := n.selectFeeUtxo(info, pending.FeeRate, estimatedTxSize)
    if err != nil {
        return err
    }

    // 5. 构造提现交易
    result, err := n.rgbClient.BuildWithdrawal(&rgb.WithdrawalParams{
        Amount:        pending.Amount,
        RecipientAddr: pending.TargetAddress,
        SealUtxo:      sealUtxo.OutPoint,
        FeeUtxo:       feeUtxo.OutPoint,
        FeeRate:       pending.FeeRate,
        Chain33TxHash: pending.TxHash,
        AssetSymbol:   pending.AssetSymbol,
    })
    if err != nil {
        return err
    }

    // 6. 记录 sticky 封印 (首次)
    if !isRetry {
        n.saveRgbStickySeal(pending.TxHash, sealUtxo)
    }

    // 7. TSS 签名 (使用 RGB_USDT 密钥组)
    signedTx, err := n.tssSign(result.UnsignedTx, info.AssetSymbol)
    if err != nil {
        n.releaseUtxosExcept(feeUtxo.OutPoint, sealUtxo.OutPoint)
        return err
    }

    // 8. 广播到 BTC 网络
    return n.broadcastTx(signedTx)
}
```

### 6.5 配置扩展 (config.go)

```toml
# 新增 RGB 相关配置
[rgb]
# RGB 客户端类型: "rgbproxy" | "rgblib" | "mock" (开发用)
ClientType = "rgbproxy"

# RGB Proxy 地址 (当使用 rgbproxy 时)
ProxyAddr = "localhost:8080"

# RGB 合约注册
[rgb.contracts]
# USDT 合约
[rgb.contracts.usdt]
ContractID = "rgb:2dwGxY...-USDT"  # RGB USDT 合约 ID
SchemaID = "rgb:2dwGxY...-Schema"  # RGB Schema ID
Precision = 6                      # 精度
AssetSymbol = "RGB_USDT"           # 对应 Chain33 上的符号

# 手续费 UTXO 池
[rgb.feePool]
# 最小手续费 UTXO 数量
MinFeeUtxos = 10
# 单个手续费 UTXO 推荐金额 (sat)
TargetUtxoAmount = 100000
# 从 BTC TSS 转入的阈值 (低于此数量自动补充)
RefillThreshold = 5
```

---

## 7. 安全模型

### 7.1 扩展的威胁矩阵

在原 BTC 跨链桥威胁矩阵基础上新增:

| 威胁 | 攻击者 | 影响 | 防护措施 | 残余风险 |
|------|--------|------|---------|---------|
| **RGB 状态伪造** | 充值用户 | 无成本获得 wrapped USDT | 官方节点 RGB 客户端验证 | RGB 客户端实现漏洞 |
| **封印UTXO 替换** | 官方节点 | 提现双花 | Sticky 封印机制+TSS 多节点验证 | 官方+TSS 多数共谋 |
| **RGB Schema 不兼容** | 合约升级 | 状态无法解析 | Schema 版本锁定+版本协商 | Schema 迁移期不一致 |
| **BTC 手续费不足** | 运营失误 | 提现交易无法确认 | 手续费 UTXO 池监控+自动补充 | 极端费率波动 |
| **封印 UTXO 粉尘被花费** | 第三方矿工 | RGB 封印破裂 | 封印 UTXO 金额≥546 sat，标记不要花费 | BTC 全节点策略变更 |

### 7.2 充值安全分层

```
┌─────────────────────────────────────────────────────────────────┐
│                   RGB USDT 充值验证分层                           │
│                                                                  │
│  Layer 1: BTC 链上验证 (rgbx 合约)                                │
│  ├─ BTC 交易在区块中 (SPV Merkle 证明)                            │
│  ├─ 区块头已提交到 lightclient                                    │
│  ├─ 确认数 ≥ 6 个区块                                             │
│  └─ OP_RETURN 格式正确, 目标地址有效                               │
│                                                                  │
│  Layer 2: RGB 链下验证 (官方节点)                                  │
│  ├─ RGB 状态转移的封印UTXO正确                                      │
│  ├─ RGB Schema 合规                                              │
│  ├─ 转移金额符合 RGB 合约逻辑                                      │
│  └─ 封印 UTXO 未被前置花费 (无双重封印)                             │
│                                                                  │
│  Layer 3: Guardian 共识 (多节点)                                   │
│  ├─ 多个 guardian 独立运行 RGB 客户端验证                          │
│  ├─ 充值需要 ≥ t 个 guardian 确认 (与 TSS 阈值一致)                │
│  └─ 任一 guardian 拒绝 → 充值被阻止                               │
│                                                                  │
│  全部通过 → 提交 deposit 交易 → 铸币                              │
└─────────────────────────────────────────────────────────────────┘
```

### 7.3 Sticky 封印机制 (类比 Sticky 末输入)

```
┌─────────────────────────────────────────────────────────────────┐
│                 RGB Sticky 封印机制                               │
│                                                                  │
│  关键原则: 同一提现请求的所有重试必须复用同一个封印 UTXO            │
│                                                                  │
│  首次构造:                                                        │
│  ┌────────────────────────────────────────────┐                 │
│  │ 1. 从可用封印 UTXO 中选择封印余额 ≥ 提现金额  │                 │
│  │ 2. 记录 sticky 封印:                        │                 │
│  │    chain33TxHash → {sealOutPoint, amount}   │                 │
│  │ 3. 锁定该封印 UTXO (其他提现不可用)          │                 │
│  └────────────────────────────────────────────┘                 │
│                                                                  │
│  重试/失败:                                                       │
│  ┌────────────────────────────────────────────┐                 │
│  │ 1. 读取 sticky 封印记录                      │                 │
│  │ 2. 必须使用相同的封印 UTXO 作为输入           │                 │
│  │ 3. 其他输入 (手续费 UTXO) 可更换              │                 │
│  │ 4. OR_RETURN 输出必须包含相同的 chain33Hash  │                 │
│  └────────────────────────────────────────────┘                 │
│                                                                  │
│  TSS 签名验证 (非官方节点):                                        │
│  ┌────────────────────────────────────────────┐                 │
│  │ 1. 检查待签名交易的 input[last] == sticky封印│                 │
│  │ 2. 检查封印 UTXO 未被其他交易花费             │                 │
│  │ 3. 检查 OP_RETURN 承诺数据正确               │                 │
│  │ 4. 不一致 → 拒绝签名                         │                 │
│  └────────────────────────────────────────────┘                 │
└─────────────────────────────────────────────────────────────────┘
```

### 7.4 BTC 费用管理安全

RGB USDT 封印 UTXO 的 BTC 价值通常仅含 546 sat (粉尘限制)。提现交易的手续费需要独立的 BTC 来源：

```
手续费 UTXO 池管理:
┌─────────────────────────────────────────────────────────────────┐
│  1. 资金来源:                                                     │
│     • 初始: 从 TSS-BTC 地址转入一批小额 BTC UTXO 到 TSS-RGB_USDT  │
│     • 持续: 定期监控 UTXO 池余额，低于阈值自动补充                 │
│                                                                  │
│  2. 安全约束:                                                     │
│     • 手续费 UTXO 与封印 UTXO 严格区分 (标记 + 独立密钥可选)       │
│     • 单次提现最多使用 1 个手续费 UTXO + 1 个封印 UTXO             │
│     • 手续费 UTXO 不可作为封印 UTXO (无 RGB 状态)                  │
│                                                                  │
│  3. 监控告警:                                                     │
│     • 手续费 UTXO 数量 < MinFeeUtxos → 告警                       │
│     • 所有手续费 UTXO 总额 < 预期月消耗 → 严重告警                 │
└─────────────────────────────────────────────────────────────────┘
```

---

## 8. 配置与部署

### 8.1 官方节点配置

```toml
[rgbx]
# 通用配置
CommitAddress = "1CcmeX..."

# 跨链资产前缀
CrossChainAssetPrefix = "X"

# Guardian 平行链
GuardianParachainTitle = "user.p.rgbxguardians."

[neutrino]
# 已有配置
IsOfficialNode = true
BtcHeaderStartHeight = 840000
BlockConfirmations = 6

# RGB 配置
[neutrino.Rgb]
# RGB 客户端类型
ClientType = "rgbproxy"    # rgbproxy | rgblib | mock
# RGB Proxy 地址
ProxyAddr = "127.0.0.1:8080"
# RGB 数据目录 (当使用内置 RGB 库时)
DataDir = "./rgb_data"

# RGB 合约配置
[neutrino.Rgb.Contracts.USDT]
ContractID = "rgb:2dwGxY...-USDT"
SchemaID = "rgb:2dwGxY...-USDT-Schema"
Precision = 6
AssetSymbol = "RGB_USDT"
MinConfirmations = 6

# 手续费 UTXO 池
[neutrino.Rgb.FeePool]
MinFeeUtxos = 10
TargetUtxoAmount = 100000
RefillThreshold = 5

# RGB_USDT TSS 配置 (可以复用 BTC TSS 的对等节点)
[neutrino.Tss.RGB_USDT]
Peers = ["node1", "node2", "node3", "node4", "node5"]
Threshold = 3
Rank = 0
```

### 8.2 部署前提

1. **RGB Proxy 部署**: 每个官方节点需运行 RGB Proxy (或内置 RGB 库)
2. **RGB 合约注册**: 确认 RGB USDT 的 ContractID 和 SchemaID
3. **手续费 UTXO 初始化**: 首次部署需从 TSS-BTC 转入至少 0.01 BTC 到 TSS-RGB_USDT 地址
4. **DKG 完成**: 全部 guardian 完成 RGB_USDT 的 DKG 流程

### 8.3 部署拓扑

```
┌─────────────────────────────────────────────────────────────────┐
│                   RGB USDT 桥部署拓扑                             │
│                                                                  │
│  每个官方节点运行:                                                 │
│  ┌──────────────────────────────────────────┐                   │
│  │  Chain33 节点                             │                   │
│  │  ├─ rgbx 合约                             │                   │
│  │  ├─ lightclient 合约                      │                   │
│  │  └─ TSS 服务 (BTC + RGB_USDT 两组密钥)     │                   │
│  │                                          │                   │
│  │  Neutrino 服务                            │                   │
│  │  ├─ BTC 监听器                            │                   │
│  │  ├─ RBG 客户端 (连接 RGB Proxy)            │                   │
│  │  ├─ 充值处理器 (BTC + RGB_USDT)           │                   │
│  │  └─ 提现处理器 (BTC + RGB_USDT)           │                   │
│  │                                          │                   │
│  │  RGB Proxy (独立进程)                      │                   │
│  │  ├─ RGB 合约引擎                           │                   │
│  │  ├─ 状态存储 (本地文件)                     │                   │
│  │  └─ gRPC API                             │                   │
│  └──────────────────────────────────────────┘                   │
│                                                                  │
│  推荐: 3 官方节点 + 4 非官方节点, 阈值 = 5                         │
│  RGB 客户端容量: 每个官方节点独立运行 RGB Proxy                    │
└─────────────────────────────────────────────────────────────────┘
```

---

## 9. 代码变更清单

### 9.1 Phase 1: 基础框架 (合约层通用化)

| 文件 | 变更类型 | 内容 |
|------|---------|------|
| `types/asset.go` | 修改 | 添加 `RGBUSDTSymbol` 常量 |
| `executor/checktx.go` | 修改 | `checkDeposit` 支持 `RGB_USDT` 资产符号的 OP_RETURN 解析 |
| `executor/validate_proof.go` | 修改 | `validateDepositTxContent` 支持非 BTC 资产的金额验证逻辑分支 |
| `executor/kv.go` | 新增 | RGB 相关 key 格式化函数 |
| `proto/rgbx.proto` | 新增 | `rgbStateProof`、`rgbDepositConfirmation` 消息 |

### 9.2 Phase 2: Neutrino 节点改造

| 文件 | 变更类型 | 内容 |
|------|---------|------|
| `neutrino/config.go` | 修改 | 添加 `RgbConfig` 结构体 |
| `neutrino/client.go` | 修改 | 初始化 RGB 客户端 |
| `neutrino/rgb/client.go` | 新增 | RGB 客户端接口与实现 |
| `neutrino/rgb/validate.go` | 新增 | RGB 状态转移验证逻辑 |
| `neutrino/rgb/seal.go` | 新增 | 封印 UTXO 管理与追踪 |
| `neutrino/rgbdeposit.go` | 新增 | RGB USDT 充值监听与处理 |
| `neutrino/rgbwithdraw.go` | 新增 | RGB USDT 提现构造与处理 |
| `neutrino/bitcoin.go` | 修改 | 区分 BTC 和 RGB USDT 的充值/提现处理 |

### 9.3 Phase 3: TSS 集成

| 文件 | 变更类型 | 内容 |
|------|---------|------|
| `neutrino/tss.go` | 修改 | 支持多密钥组 (按 assetSymbol 索引) |
| `neutrino/rpc.go` | 修改 | 新增 RGB 相关 RPC 接口 |

### 9.4 Phase 4: 测试与部署

| 文件 | 变更类型 | 内容 |
|------|---------|------|
| `executor/crosschain_test.go` | 修改 | 添加 RGB_USDT 充提测试用例 |
| `executor/validate_proof_test.go` | 修改 | 添加 RGB 充值验证测试 |
| `cmd/ci/docker-compose.yml` | 修改 | 添加 RGB Proxy 容器 |
| `cmd/ci/Dockerfile` | 修改 | 添加 RGB 客户端依赖 |

### 9.5 兼容性

- **向后兼容**：现有 BTC 桥功能完全不受影响
- **Proto 兼容**：新增字段使用 protobuf 标准扩展，不影响已有消息解析
- **API 兼容**：现有 RPC 接口不变，新增 RGB 专用接口
- **数据库兼容**：新增存储桶，不修改已有桶结构

---

## 附录 A: OP_RETURN 数据格式规范

### 充值 OP_RETURN

```
格式: 0x6a <len> "rgbx:deposit:[<assetSymbol>:]<chain33Address>"

BTC 充值 (兼容现有):
  OP_RETURN = 0x6a 0x30 726762783a6465706f7369743a31416263...
  解码: "rgbx:deposit:1Abc..."

RGB_USDT 充值 (新格式):
  OP_RETURN = 0x6a 0x3a 726762783a6465706f7369743a5247425f555344543a31416263...
  解码: "rgbx:deposit:RGB_USDT:1Abc..."

解析规则:
  1. 检查前缀 "rgbx:deposit:"
  2. 按 ":" 分割 payload
  3. 如果 segments == 1: assetSymbol = "BTC", depositAddr = segments[0]
  4. 如果 segments == 2: assetSymbol = segments[0], depositAddr = segments[1]
```

### 提现 OP_RETURN

```
格式: 0x6a <len> "rgbx:withdraw:<chain33TxHash>"

统一格式 (所有资产):
  chain33TxHash = 32 字节交易哈希的十六进制编码

注意: 提现的 assetSymbol 由 chain33TxHash 对应的 PendingTx 记录确定，
      不需要在 OP_RETURN 中重复编码。
```

---

## 附录 B: 参考资源

- RGB 协议规范: [https://rgb.tech/](https://rgb.tech/)
- RGB 开发者文档: [https://docs.rgb.tech/](https://docs.rgb.tech/)
- RGB Proxy: [https://github.com/RGB-Tools/rgb-proxy](https://github.com/RGB-Tools/rgb-proxy)
- 现有 BTC 桥技术文档: [TECHNICAL.md](./TECHNICAL.md)
- GG18/GG20 阈值签名论文: "Fast Multiparty Threshold ECDSA"

---

*文档结束*
