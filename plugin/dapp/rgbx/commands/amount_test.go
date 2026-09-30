package commands

import (
	"fmt"
	"math"
	"strconv"
	"testing"

	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
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
		got, err := parseDecimalAmount(arg, btcWithdrawDecimals)
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
	require.Equal(t, 8, btcWithdrawDecimals)
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

		got, err := parseDecimalAmount(arg, btcWithdrawDecimals)
		require.NoError(t, err, "arg=%q", arg)
		require.Equal(t, want, got, "arg=%q", arg)
	}

	// 带空格的非法输入必须报错，而不是被当成 0.0006。
	_, err := parseDecimalAmount("0.0006 0000", btcWithdrawDecimals)
	require.Error(t, err)
}

// Test_WithdrawCmd_AmountFlagIsExact 走真实命令的 flag 读取路径：amount 必须按字符串接收，
// 否则 0.0006 在进入换算前就已经变成 0.00059999999999999994744。
func Test_WithdrawCmd_AmountFlagIsExact(t *testing.T) {
	cmd := withdrawAssetCMD()
	require.NoError(t, cmd.Flags().Set("amount", "0.0006"))

	argv, err := cmd.Flags().GetString("amount")
	require.NoError(t, err, "amount flag must be a string flag")
	amount, err := parseDecimalAmount(argv, btcWithdrawDecimals)
	require.NoError(t, err)
	require.Equal(t, int64(60000), amount)

	// harness 的 %.8f 口径同样精确
	require.NoError(t, cmd.Flags().Set("amount", "0.00060000"))
	argv, err = cmd.Flags().GetString("amount")
	require.NoError(t, err)
	amount, err = parseDecimalAmount(argv, btcWithdrawDecimals)
	require.NoError(t, err)
	require.Equal(t, int64(60000), amount)
}

func Test_pow10(t *testing.T) {
	for n := 0; n <= 8; n++ {
		require.Equal(t, int64(math.Pow(10, float64(n))), pow10(n))
	}
}

// resolveWithdrawDecimals 的每一档都必须有出处；**没有出处就报错**，绝不回落成 8。
// 这一组测试钉住的正是"静默 10^n 倍金额错误"那个缺口。
func Test_resolveWithdrawDecimals(t *testing.T) {
	const usdt = "RGB20_USDT" // 桥接符号（rtypes.IsRgb20Symbol）

	t.Run("显式精度优先于一切", func(t *testing.T) {
		// 即便链上有资产记录且精度不同，也以显式声明为准（调用方读的是合约）。
		got, err := resolveWithdrawDecimals(usdt, 6, "RGB20_USDT", 8)
		require.NoError(t, err)
		require.Equal(t, 6, got)
	})

	t.Run("BTC 固定 8 位小数", func(t *testing.T) {
		for _, s := range []string{"BTC", "btc", "Btc"} {
			got, err := resolveWithdrawDecimals(s, -1, "", 0)
			require.NoError(t, err, "symbol=%s", s)
			require.Equal(t, 8, got)
		}
	})

	t.Run("非 8 精度的链上资产按其自身精度换算", func(t *testing.T) {
		got, err := resolveWithdrawDecimals("MYTOKEN", -1, "MYTOKEN", 6)
		require.NoError(t, err)
		require.Equal(t, 6, got, "精度 6 必须照用，不能取 8")
	})

	t.Run("精度 0 的链上资产是合法的", func(t *testing.T) {
		got, err := resolveWithdrawDecimals("NFTLIKE", -1, "NFTLIKE", 0)
		require.NoError(t, err)
		require.Equal(t, 0, got)
	})

	t.Run("桥接的 RGB20 资产没给 -p 就拒绝", func(t *testing.T) {
		err := func() error { _, e := resolveWithdrawDecimals(usdt, -1, "", 0); return e }()
		require.Error(t, err, "must not silently assume 8")
		require.Contains(t, err.Error(), "RGB20")
		require.Contains(t, err.Error(), "-p/--precision")
		// 即使链上碰巧有一条该符号的资产记录也不能信：桥接符号的口径只在合约里。
		_, err = resolveWithdrawDecimals(usdt, -1, "RGB20_USDT", 8)
		require.Error(t, err, "a chain record must not be trusted for a bridged symbol")
	})

	t.Run("既没 -p 也没链上记录时拒绝", func(t *testing.T) {
		_, err := resolveWithdrawDecimals("UNKNOWN", -1, "", 0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "UNKNOWN")
	})

	t.Run("超出链上口径的精度拒绝", func(t *testing.T) {
		_, err := resolveWithdrawDecimals("WEIRD", -1, "WEIRD", rtypes.MaxPrecision+1)
		require.Error(t, err)
		require.Contains(t, err.Error(), "above the maximum")

		_, err = resolveWithdrawDecimals("WEIRD", rtypes.MaxPrecision+1, "", 0)
		require.Error(t, err)
	})

	t.Run("显式精度上界", func(t *testing.T) {
		got, err := resolveWithdrawDecimals("X", rtypes.MaxPrecision, "", 0)
		require.NoError(t, err)
		require.Equal(t, int(rtypes.MaxPrecision), got)
	})
}

// 端到端口径：精度 6 的资产，-a 1.5 必须换算成 1_500_000 个最小单位（而不是 1.5e8）。
func Test_resolveWithdrawDecimals_DrivesTheConversion(t *testing.T) {
	decimals, err := resolveWithdrawDecimals("RGB20_USDT", 6, "", 0)
	require.NoError(t, err)

	amount, err := parseDecimalAmount("1.5", decimals)
	require.NoError(t, err)
	require.Equal(t, int64(1_500_000), amount, "精度 6 下 1.5 个 USDT = 1.5e6 最小单位")

	// 同一串在 8 位口径下是 1.5e8 —— 相差 100 倍，这正是"静默算错钱"的形态。
	amount8, err := parseDecimalAmount("1.5", btcWithdrawDecimals)
	require.NoError(t, err)
	require.Equal(t, int64(150_000_000), amount8)
	require.Equal(t, int64(100), amount8/amount)

	// 超出精度的输入必须报错，而不是静默截断成另一个金额。
	_, err = parseDecimalAmount("1.5000001", decimals)
	require.Error(t, err)
}

// withdraw 命令的精度 flag 必须存在、默认 -1（"未指定"），且不与其他 flag 的简写冲突
// （`-a/-f/-d/-s/-p`；`send` 是 DisableFlagParsing 的手工解析，`-k` 由它自己摘掉）。
func Test_WithdrawCmd_PrecisionFlag(t *testing.T) {
	cmd := withdrawAssetCMD()
	require.NotNil(t, cmd.Flags().Lookup("precision"))
	p := cmd.Flags().ShorthandLookup("p")
	require.NotNil(t, p, "-p 是 harness 与运维要用的简写")
	require.Equal(t, "precision", p.Name)

	got, err := cmd.Flags().GetInt32("precision")
	require.NoError(t, err)
	require.Equal(t, int32(-1), got, "默认必须是 -1：0 是合法精度，不能拿它当'未指定'")

	require.NoError(t, cmd.Flags().Set("precision", "6"))
	got, err = cmd.Flags().GetInt32("precision")
	require.NoError(t, err)
	require.Equal(t, int32(6), got)
}
