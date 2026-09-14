package commands

import (
	"fmt"
	"strconv"
	"strings"

	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
)

// 提现额的最小单位口径固定为 1e8（BTC 的 8 位小数）。RGB20 资产也走同一个入口
// （见 plugin/dapp/rgbx/cmd/ci/scripts/rgb20_test.sh：「-a 口径 *1e8 = min units」），
// 所以这里不能用资产自身的 precision 代替。
const withdrawAmountDecimals = 8

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
