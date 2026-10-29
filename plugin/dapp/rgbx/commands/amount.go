package commands

import (
	"fmt"
	"strconv"
	"strings"

	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
)

// btcWithdrawDecimals 是 BTC 提现的 -a 口径（8 位小数）。
//
// 它**只对 BTC 成立**，而且成立的理由不是"历史上都这么写"：BTC 的小数位是链上口径本身
// （chain33 的 1e8 最小单位），不是某个资产的属性，所以它可以是个常量。链上那侧的同一个值由
// rtypes.BTCChainPrecision 定义（checkCommitDKG 强制 BTC 类资产的精度只能是它），两边不可能
// 对不上 —— 因此"链上还没有该资产的跨链记录"时回落到这个常量是安全的，不是猜测。
const btcWithdrawDecimals = 8

// defaultCrossChainAssetPrefix is the mapping prefix documented for cross-chain symbols
// ("BTC" -> "XBTC"). The executor's authoritative prefix lives in node config; the CLI uses
// this constant only to derive the other spelling of a caller-supplied symbol when a lookup
// that came back empty is retried.
const defaultCrossChainAssetPrefix = "X"

// crossChainSymbolAliases returns the spellings to try for a caller-supplied symbol, most
// exact first: the symbol as typed, then the other spelling (original <-> mapped). It mirrors
// the executor's lookup order, which also prefers the exact spelling, so a query for either
// spelling finds the same record and never redirects an exact hit.
func crossChainSymbolAliases(symbol string) []string {
	normalized := strings.ToUpper(strings.TrimSpace(symbol))
	if normalized == "" {
		return nil
	}
	prefix := defaultCrossChainAssetPrefix
	if original := strings.TrimPrefix(normalized, prefix); original != normalized && original != "" {
		return []string{normalized, original}
	}
	return []string{normalized, prefix + normalized}
}

// isBtcSymbol reports whether either spelling of symbol is BTC. BTC's precision is a chain
// constant, so the fallback must also recognize the mapped spelling "XBTC" used by balances.
func isBtcSymbol(symbol string) bool {
	for _, spelling := range crossChainSymbolAliases(symbol) {
		if spelling == rtypes.BTCSymbol {
			return true
		}
	}
	return false
}

// crossChainInfoAbsent 判断节点是否**没有**该符号的跨链资产记录。
//
// 判据落在记录内容上：执行器的 Query_GetCrossChainInfo 对不存在的 symbol 返回空结构体 +
// nil error（既有契约），所以"查得到"与"链上有"是两件事。
func crossChainInfoAbsent(info *rtypes.CrossChainInfo) bool {
	return info.GetTssAddress() == "" && len(info.GetPubkey()) == 0
}

// resolveWithdrawDecimals 决定 withdraw 的 -a 十进制口径（最小单位 = 10^decimals）。
//
// 精度**只从链上读**：调用方（withdrawAsset）已按符号把两种链上记录查了出来，
// 这里只做选择与校验，不猜、也没有 `-p` 这种"让人填一个数"的入口。
//
// 选择顺序，每一档都有出处；**没有出处就报错，绝不回落成 8**：
//
//  1. 跨链资产记录（crossChainInfo）：桥接资产（BTC 类 / RGB20 类）的精度在这里。类别与精度由
//     桥在 CommitDKG 时登记（BTC 类固定 8），RGB20 类的精度由桥核对出合约声明的值后回填
//     （见 executor/crosschain.go 的 Exec_CrossChainMeta）。回填尚未发生（哨兵值）时报错 ——
//     那说明桥还没核对出这个资产的合约精度，此时的正确行为是不服务，而不是挑一个数；
//  2. 本地发行/铸造资产的记录（rgbxAsset）：精度由发行方在 MintAsset 里声明；
//  3. BTC 且链上还没有跨链记录（DKG 尚未提交）：见 btcWithdrawDecimals（协议常量）；
//  4. 以上都没有 → 报错。
//
// 为什么以前需要 `-p`、现在不需要了：桥接符号的资产记录**不由充值铸造写**
// （executor/crosschain.go 的 Exec_Deposit 只动账户余额，不写 RgbxAsset），链上查不到口径，
// 而它的真实口径在合约里、CLI 又看不到合约 —— 于是只能要求调用方手填。现在桥把核对过的合约精度
// 写进了链上的跨链资产记录，CLI 读得到，手填那个入口（连同它带来的"填错就静默差 10^n 倍"）
// 随之删掉。
//
// 链上记录的精度也必须 <= rtypes.MaxPrecision：超过链上口径能表达的范围（或干脆是个哨兵值）时
// 拒绝，而不是交给 parseDecimalAmount 去截断。
func resolveWithdrawDecimals(symbol string, crossChain *rtypes.CrossChainInfo, asset *rtypes.RgbxAsset) (int, error) {
	if !crossChainInfoAbsent(crossChain) {
		kind := rtypes.CrossChainAssetKind(crossChain.GetAssetKind())
		if !kind.Valid() {
			return 0, fmt.Errorf("asset %s is registered on chain with an unknown cross chain asset kind "+
				"(%d): cannot tell how its amounts are scaled; this node's rgbx executor is probably "+
				"older than the chain state", symbol, crossChain.GetAssetKind())
		}
		precision := crossChain.GetPrecision()
		if !rtypes.IsKnownPrecision(precision) {
			return 0, fmt.Errorf("cannot determine the amount scale of %s: the bridge has not recorded its "+
				"precision on chain yet (crossChainInfo.precision is the \"not backfilled\" sentinel). For an "+
				"RGB20 asset the bridge backfills the precision of its contract automatically once the sidecar "+
				"has verified it; retry once the bridge is up and the contract is issued. Refusing to assume a "+
				"scale: the amount would be off by 10^n and nothing downstream would notice", symbol)
		}
		if precision > rtypes.MaxPrecision {
			return 0, fmt.Errorf("asset %s declares precision %d on chain, above the maximum %d",
				symbol, precision, rtypes.MaxPrecision)
		}
		return int(precision), nil
	}
	if asset.GetSymbol() != "" {
		if asset.GetPrecision() > rtypes.MaxPrecision {
			return 0, fmt.Errorf("asset %s declares precision %d on chain, above the maximum %d",
				symbol, asset.GetPrecision(), rtypes.MaxPrecision)
		}
		return int(asset.GetPrecision()), nil
	}
	if isBtcSymbol(symbol) {
		return btcWithdrawDecimals, nil
	}
	return 0, fmt.Errorf("cannot determine the precision of asset %q: the node returned no cross chain record "+
		"and no asset record for it (or the queries to the node failed). A bridged asset's precision is "+
		"recorded on chain by the bridge (crossChainInfo.precision), a locally minted asset's precision is "+
		"declared at mint time; if neither exists, the conversion of -a into minimum units has no correct "+
		"default (is this node on the same chain as the asset, and has the bridge committed its DKG?)", symbol)
}

// parseDecimalAmount 把十进制字符串精确换算为最小单位整数，全程定点运算，不经过 float64。
//
// 为什么不能用浮点：float64 无法精确表示大多数十进制小数，相乘再截断会丢 1 个最小单位。
// 实测 0.0006 * 1e8 = 59999.99999999999（int64 截断得 59999）、0.00000003 得 2（应为 3）；
// 1..2,000,000 最小单位中 159879 个（7.99%）会被少换算 1 个单位。
//
// 后果链：链上 pending 的提现额 = 本函数的返回值，而提现意图（invoice / 侧车发放额）用精确值。
// 两者差 1 时，签名节点的覆盖校验会（正确地）以
// "withdraw payout exceeds pending amount" 拒绝放款，且该 pending 不属于不可恢复类别，
// 于是桥每秒重试刷 ERROR——用户资产已 burn，却永远拿不到款。
//
// decimals 是小数位数：整数部分左移 decimals 位，小数部分右补零到 decimals 位。
// 小数位数的**来源**见 resolveWithdrawDecimals（它是资产属性，不是常量）。
// 小数位数超过 decimals 时，只有多余位全为 0 才接受；否则属于超出精度的输入，
// 直接报错，而不是静默截断成另一个金额。
func parseDecimalAmount(s string, decimals int) (int64, error) {
	if decimals < 0 || decimals > rtypes.MaxPrecision {
		return 0, fmt.Errorf("invalid decimals: %d", decimals)
	}
	raw := strings.TrimSpace(s)
	if raw == "" {
		return 0, fmt.Errorf("empty amount")
	}
	negative := false
	switch raw[0] {
	case '+':
		raw = raw[1:]
	case '-':
		negative = true
		raw = raw[1:]
	}
	intPart, fracPart, _ := strings.Cut(raw, ".")
	if !isAllDigits(intPart) || !isAllDigits(fracPart) {
		return 0, fmt.Errorf("invalid amount: %q", s)
	}
	if intPart == "" && fracPart == "" {
		return 0, fmt.Errorf("invalid amount: %q", s)
	}
	if len(fracPart) > decimals {
		for _, c := range fracPart[decimals:] {
			if c != '0' {
				return 0, fmt.Errorf("amount %q has more than %d decimal places", s, decimals)
			}
		}
		fracPart = fracPart[:decimals]
	}
	// 整数部分 + 小数部分补零到 decimals 位 = 最小单位的十进制表示，直接整数解析。
	digits := intPart + fracPart + strings.Repeat("0", decimals-len(fracPart))
	amount, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid amount %q: %w", s, err)
	}
	if negative {
		amount = -amount
	}
	return amount, nil
}

func isAllDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// pow10 返回 10^n（n >= 0）的整数形式，替代 math.Pow 的浮点路径。
func pow10(n int) int64 {
	v := int64(1)
	for i := 0; i < n; i++ {
		v *= 10
	}
	return v
}
