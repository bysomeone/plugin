package types

import "math"

const (
	// MaxAssetSymbolLength is the maximum byte length of an asset's symbol.
	// This byte length is equivalent to character count for single-byte
	// UTF-8 characters.
	MaxAssetSymbolLength = 16

	// MetaHashLen is the length of the metadata hash.
	MetaHashLen = 32
	// MaxPrecision max Precision
	MaxPrecision = 8
	// MaxAssetAmount max amount
	MaxAssetAmount = 1e8 * 900 * 1e8 // 900 亿

	// BTCChainPrecision BTC 类跨链资产的精度（链上口径）。
	//
	// 为什么是常量而不是"从别处读"：BTC 的最小单位（聪）与 BTC 的关系是协议定义的 1e8，桥里所有
	// BTC 金额（充值证明、提现、扫集）也都以聪为单位。所以它不依赖任何外部读数，是 checkCommitDKG
	// 强制的链上不变量（BTC 类资产只能提交这个值），而不是提交者随手填的一个数。
	BTCChainPrecision uint32 = 8

	// PrecisionUnknown 链上"精度尚未回填"的哨兵值。
	//
	// 为什么需要哨兵：proto3 的 `uint32` **没有 presence** —— 不写与写 0 在线上是同一串字节，
	// 而 0 是合法精度（不可分割资产）。跨链资产的精度在 CommitDKG 那一刻未必已知（RGB20 的精度
	// 只能来自合约，而合约可能还没发行 / 侧车还没核对），所以"还不知道"必须用一个精度空间之外的
	// 值表示。真实精度不可能到这个值（RGB 的 Precision 上限是 Atto=18，链上口径上限是
	// MaxPrecision），因此不会与任何真精度混淆。
	//
	// **两侧共享的约定**：Rust 侧 `rgb_sidecar::service::PRECISION_UNKNOWN`、Go 侧
	// `rgb20.PrecisionUnknown`（由本常量赋值）都取同一个值，`proto/rgb_sidecar.proto` 的
	// `AssetInfo.precision` 也写明了。改一个就必须同时改另一个。
	PrecisionUnknown uint32 = math.MaxUint32
)

// AssetType denotes the asset types supported by the Taproot RgbxAsset protocol.
type AssetType uint8

const (
	// Normal is an asset that can be represented in multiple units,
	// resembling a divisible asset.
	Normal AssetType = 0

	// Collectible is a unique asset, one that cannot be represented in
	// multiple units.
	Collectible AssetType = 1
)

// String returns a human-readable description of the type.
func (t AssetType) String() string {
	switch t {
	case Normal:
		return "Normal"
	case Collectible:
		return "Collectible"
	default:
		return "<Unknown>"
	}
}

// CrossChainAssetKind 跨链资产**类别**，由 CommitDKG 登记在链上（CrossChainInfo.assetKind）。
//
// 为什么类别必须上链、且必须与 symbol 字面量解耦：充值/提现对两条类别走的是**完全不同的门禁**
// （BTC 类按 P2WSH 派生脚本判定金额与归属；RGB20 类的金额在 consignment 内，链上只做 TSS 阈值
// 签名门禁）。此前执行器用 `strings.ToUpper(symbol) == "RGB20_USDT"` 来分叉 —— 那个函数名叫
// "IsRgb20Symbol"（听起来是"是不是 RGB20 资产"），实际是"是不是这**一个**符号"：接入第二个
// RGB20 资产时它会返回 false，交易被静默按 BTC 分支处理。类别是桥在 CommitDKG 里声明的链上
// 事实（提交者受 guardian 白名单约束），判定只读它，不再看符号。
type CrossChainAssetKind uint32

const (
	// CrossChainAssetBtc BTC 及其映射资产（如 XBTC）。
	CrossChainAssetBtc CrossChainAssetKind = 0
	// CrossChainAssetRgb20 RGB20 协议资产（如 RGB20 USDT）：充值金额在 consignment 内，
	// 链上按 TSS thresholdSig 门禁。
	CrossChainAssetRgb20 CrossChainAssetKind = 1
)

// String returns a human-readable description of the kind.
func (k CrossChainAssetKind) String() string {
	switch k {
	case CrossChainAssetBtc:
		return "BTC"
	case CrossChainAssetRgb20:
		return "RGB20"
	default:
		return "<Unknown>"
	}
}

// Valid 判断类别是否为链上已知类别（未知类别一律拒绝，避免"新类别被旧执行器当 BTC 处理"）。
func (k CrossChainAssetKind) Valid() bool {
	return k == CrossChainAssetBtc || k == CrossChainAssetRgb20
}

// IsKnownPrecision 判断精度值是否为真实精度（而不是"尚未回填"的哨兵）。
//
// 读取方（前端 / CLI / 桥）必须先过这一关再用精度换算金额：拿哨兵去算 10^precision 会溢出，
// 或者更糟 —— 被当成"某个很大的精度"而静默算错钱。
func IsKnownPrecision(precision uint32) bool {
	return precision != PrecisionUnknown
}

// IsRgb20CrossChainAsset 按**链上登记的资产类别**判断该跨链资产是否走 RGB20 分支。
//
// 记录缺失（info 为 nil / 空的零值）时返回 false：调用方必须先自行确认记录存在
// （getCrossChainInfo 的错误分支），否则会把"链上还没有这条记录"当成 BTC 处理。
func IsRgb20CrossChainAsset(info *CrossChainInfo) bool {
	return CrossChainAssetKind(info.GetAssetKind()) == CrossChainAssetRgb20
}

// cross chain asset symbol
const (
	// BTCSymbol bitcoin
	BTCSymbol = "BTC"

	// RGB20USDTSymbol 当前部署使用的 RGB20 资产符号（UTEXO 发行的 Bitcoin 原生 USDT）。
	// 与标准 RGB（v0.11.1）RGB20/IFA 协议对接，链上只做 TSS 阈值签名门禁。
	//
	// 注意：它只是一个**默认符号常量**（配置 / E2E / 文档里用），不参与任何类别判定 ——
	// 类别判定一律看链上登记的 CrossChainInfo.assetKind。
	RGB20USDTSymbol = "RGB20_USDT"
)
