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
// （chain33 的 1e8 最小单位），不是某个资产的属性，所以它可以是个常量。
//
// 曾经这个常量被当成**所有**资产的提现口径（`withdrawAmountDecimals`），于是精度 6 的 RGB20
// 资产（如 USDT）会被按 1e8 换算：用户输入 1，链上得到 1e8 个最小单位 = 100 个 USDT 的量，
// 且全程静默。资产的口径只能来自资产自己（链上资产记录 / 合约 / 显式声明），见
// resolveWithdrawDecimals。
const btcWithdrawDecimals = 8

// resolveWithdrawDecimals 决定 withdraw 的 -a 十进制口径（最小单位 = 10^decimals）。
//
// 选择顺序，每一档都有出处；**没有出处就报错，绝不回落成 8**：
//
//  1. 显式 `-p/--precision`：调用方声明（对桥接资产，这个值就是合约声明的精度）；
//  2. BTC：见 btcWithdrawDecimals（固定 8，合法且无需询问）；
//  3. 链上该资产的记录：本地发行/铸造的 rgbx 资产，精度由发行方在 MintAsset 里声明
//     （chainSymbol 非空表示节点认识这个资产，chainPrecision 是它的精度）；
//  4. 以上都没有 → 报错。
//
// 为什么 3 之前要先挡掉 RGB20：桥接符号（`rtypes.IsRgb20Symbol`）的资产记录**不由充值铸造写**
// （见 executor/crosschain.go 的 Exec_Deposit：只动账户余额，不写 RgbxAsset），链上口径无从查证；
// 而它的真实口径是**合约**的精度，CLI 看不到合约。这时唯一诚实的做法是要调用方给出 `-p`，而不是
// 挑一个数。
//
// 链上记录的精度也必须 <= rtypes.MaxPrecision：超过链上口径能表达的范围（或干脆是个哨兵值）时
// 拒绝，而不是交给 parseDecimalAmount 去截断。
func resolveWithdrawDecimals(symbol string, flagPrecision int32, chainSymbol string, chainPrecision uint32) (int, error) {
	if flagPrecision >= 0 {
		if flagPrecision > int32(rtypes.MaxPrecision) {
			return 0, fmt.Errorf("invalid precision %d: must be in [0, %d]", flagPrecision, rtypes.MaxPrecision)
		}
		return int(flagPrecision), nil
	}
	if strings.EqualFold(symbol, rtypes.BTCSymbol) {
		return btcWithdrawDecimals, nil
	}
	if rtypes.IsRgb20Symbol(symbol) {
		return 0, fmt.Errorf("cannot determine the amount scale of %s: it is a bridged RGB20 asset, whose "+
			"precision is declared by its RGB contract (chain33 keeps no asset record for a bridged symbol, "+
			"so it cannot be read from here). Pass -p/--precision <n> with the precision the contract "+
			"declares (the bridge's rgb20.contracts[].precision, which the bridge checks against the "+
			"contract at startup). Refusing to assume 8: the amount would be off by 10^n and nothing "+
			"downstream would notice", symbol)
	}
	if chainSymbol != "" {
		if chainPrecision > rtypes.MaxPrecision {
			return 0, fmt.Errorf("asset %s declares precision %d on chain, above the maximum %d",
				symbol, chainPrecision, rtypes.MaxPrecision)
		}
		return int(chainPrecision), nil
	}
	return 0, fmt.Errorf("cannot determine the precision of asset %q: the node returned no asset record for it "+
		"(or the query to the node failed), and none was given. Pass -p/--precision <n> stating how many "+
		"decimal places the asset's amounts carry; the conversion of -a into minimum units has no correct "+
		"default", symbol)
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
