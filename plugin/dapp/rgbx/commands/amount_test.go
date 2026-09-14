package commands

import (
	"fmt"
	"math"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// Test_parseDecimalAmount_Exact 精确换算：任何合法位数都不许出现"少 1 / 多 1"。
func Test_parseDecimalAmount_Exact(t *testing.T) {
	cases := []struct {
		in       string
		decimals int
		want     int64
	}{
		// 浮点截断的经典反例：0.0006 * 1e8 = 59999.99999999999 → int64 得 59999（少 1）。
		{"0.0006", 8, 60000},
		// harness 的 awk "%.8f" 口径（末尾补零），必须与上面等价。
		{"0.00060000", 8, 60000},
		{"0.00003000", 8, 3000},
		// harness 现用金额（恰好落在安全区），作为回归保护保留。
		{"0.005", 8, 500000},
		{"0.002", 8, 200000},
		{"0.1", 8, 10000000},
		{"1", 8, 100000000},
		{"0.00000001", 8, 1},
		{"0.5", 8, 50000000},
		{".5", 8, 50000000},
		{"5.", 8, 500000000},
		{"0000.0006", 8, 60000},
		{" 0.0006 ", 8, 60000},
		{"+0.0006", 8, 60000},
		{"-0.0006", 8, -60000},
		{"0", 8, 0},
		{"0.0000000", 8, 0},
		{"12345678.00000000", 8, 1234567800000000},
		// 非 8 位精度（transfer 按资产精度换算）
		{"0.0006", 6, 600},
		{"1.234567", 6, 1234567},
		{"100", 0, 100},
		{"1.0", 0, 1},
	}
	for _, c := range cases {
		got, err := parseDecimalAmount(c.in, c.decimals)
		require.NoError(t, err, "in=%q decimals=%d", c.in, c.decimals)
		require.Equal(t, c.want, got, "in=%q decimals=%d", c.in, c.decimals)
	}
}

// Test_parseDecimalAmount_Invalid 非法 / 超出精度的输入必须报错，不许静默等价成别的金额。
func Test_parseDecimalAmount_Invalid(t *testing.T) {
	cases := []struct {
		in       string
		decimals int
	}{
		{"", 8},
		{"   ", 8},
		{"abc", 8},
		{"1.2.3", 8},
		{"1e8", 8},
		{"0x10", 8},
		{"0.1.0", 8},
		{"-", 8},
		{".", 8},
		{"+", 8},
		// 超出精度且末位非零：静默截断会得到与用户意图不同的金额，必须报错。
		{"0.000000001", 8},
		{"0.123456789", 8},
		{"0.0000001", 6},
		// int64 溢出
		{"99999999999999999999", 8},
		// 非法精度
		{"1", 9},
		{"1", -1},
	}
	for _, c := range cases {
		_, err := parseDecimalAmount(c.in, c.decimals)
		require.Error(t, err, "in=%q decimals=%d should be rejected", c.in, c.decimals)
	}
	// 超出精度但多余位全为 0 属于合法输入（等价于按精度截断）。
	got, err := parseDecimalAmount("0.1000000000", 8)
	require.NoError(t, err)
	require.Equal(t, int64(10000000), got)
}

// Test_parseDecimalAmount_NoOffByOne_Sweep 用 harness 的渲染口径（awk "%.8f" = min units/1e8）
// 把 1..n 个最小单位转成 -a 参数，再换算回来，必须逐个精确。
//
// 同时用旧的浮点实现（float64 相乘 + int64 截断）统计本区间内会少 1 的个数，断言它非零——
// 保证这个区间确实覆盖了"易截断"的值，而不是恰好全落在安全区（这正是 harness 用
// 0.005/0.002 时没暴露该 bug 的原因）。
func Test_parseDecimalAmount_NoOffByOne_Sweep(t *testing.T) {
	const n = 200000
	var floatLost, exactLost int
	for i := int64(1); i <= n; i++ {
		arg := fmt.Sprintf("%.8f", float64(i)/1e8) // 与 harness 的 awk 渲染一致

		// 旧实现（回归反证用）
		f, err := strconv.ParseFloat(arg, 64)
		require.NoError(t, err)
		if int64(f*math.Pow(10, 8)) != i {
			floatLost++
			if floatLost <= 5 {
				t.Logf("old float path: min unit %d -> %d (arg %q)", i, int64(f*math.Pow(10, 8)), arg)
			}
		}

		// 新实现
		got, err := parseDecimalAmount(arg, withdrawAmountDecimals)
		require.NoError(t, err, "arg=%q", arg)
		if got != i {
			exactLost++
			if exactLost <= 10 {
				t.Errorf("min unit %d round-trips to %d (arg %q)", i, got, arg)
			}
		}
	}
	require.Zero(t, exactLost, "%d/%d min units lost by 1", exactLost, n)
	require.NotZero(t, floatLost, "sweep must cover truncation-prone values, otherwise it proves nothing")
	t.Logf("sweep 1..%d: old float path loses %d, exact parse loses 0", n, floatLost)
}

// Test_WithdrawAmount_TruncationProneValues 直接钉住提现入口的换算：-a 口径是 8 位小数，
// 下表每个值在旧实现（float64 相乘 + int64 截断）下都会少 1（0.00060000 -> 59999、
// 0.00000003 -> 2），精确换算必须给出原值。
func Test_WithdrawAmount_TruncationProneValues(t *testing.T) {
	require.Equal(t, 8, withdrawAmountDecimals)
	prone := map[string]int64{
		"0.00060000":        60000,
		"0.00000003":        3,
		"0.00000006":        6,
		"0.00000012":        12,
		"0.00000024":        24,
		"0.00000029":        29,
		"99999999.99999999": 9999999999999999,
	}
	for arg, want := range prone {
		// 确认这个值确实会踩雷（否则本用例没有意义）。
		f, err := strconv.ParseFloat(arg, 64)
		require.NoError(t, err)
		require.NotEqual(t, want, int64(f*math.Pow(10, 8)),
			"arg=%q: old float path was already exact; pick a truncation-prone value", arg)

		got, err := parseDecimalAmount(arg, withdrawAmountDecimals)
		require.NoError(t, err, "arg=%q", arg)
		require.Equal(t, want, got, "arg=%q", arg)
	}

	// 带空格的非法输入必须报错，而不是被当成 0.0006。
	_, err := parseDecimalAmount("0.0006 0000", withdrawAmountDecimals)
	require.Error(t, err)
}

// Test_WithdrawCmd_AmountFlagIsExact 走真实命令的 flag 读取路径：amount 必须按字符串接收，
// 否则 0.0006 在进入换算前就已经变成 0.00059999999999999994744。
func Test_WithdrawCmd_AmountFlagIsExact(t *testing.T) {
	cmd := withdrawAssetCMD()
	require.NoError(t, cmd.Flags().Set("amount", "0.0006"))

	argv, err := cmd.Flags().GetString("amount")
	require.NoError(t, err, "amount flag must be a string flag")
	amount, err := parseDecimalAmount(argv, withdrawAmountDecimals)
	require.NoError(t, err)
	require.Equal(t, int64(60000), amount)

	// harness 的 %.8f 口径同样精确
	require.NoError(t, cmd.Flags().Set("amount", "0.00060000"))
	argv, err = cmd.Flags().GetString("amount")
	require.NoError(t, err)
	amount, err = parseDecimalAmount(argv, withdrawAmountDecimals)
	require.NoError(t, err)
	require.Equal(t, int64(60000), amount)
}

func Test_pow10(t *testing.T) {
	for n := 0; n <= 8; n++ {
		require.Equal(t, int64(math.Pow(10, float64(n))), pow10(n))
	}
}
