package neutrino

import (
	"errors"
	"testing"
	"time"

	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/stretchr/testify/require"
)

// 本文件覆盖链上桥配置（rgbx 治理的 BridgeConfig）在桥侧的读取口径：
//   - 链上值生效；
//   - TTL 缓存命中（不重复查询）；
//   - 查询失败且从未成功过 → 回退内置默认值（签名端不因此拒签合法交易）；
//   - 曾成功过、之后失败 → 用最后一次成功值；
//   - 链上值为 0（治理未设置）→ 回退内置默认值；
//   - RGB20 手续费按 symbol 取值。

// bridgeConfigFixture 构造一个带注入查询与可控时钟的 client，并统计查询次数。
type bridgeConfigFixture struct {
	client *neutrinoClient
	now    time.Time
	calls  int
}

func newBridgeConfigFixture(t *testing.T, query func(symbol string) (*rtypes.BridgeConfigApply, error)) *bridgeConfigFixture {
	t.Helper()
	f := &bridgeConfigFixture{client: &neutrinoClient{}, now: time.Unix(1_700_000_000, 0)}
	reader := newBridgeConfigReader(func(symbol string) (*rtypes.BridgeConfigApply, error) {
		f.calls++
		return query(symbol)
	}, 30*time.Second)
	reader.now = func() time.Time { return f.now }
	f.client.bridgeConfig = reader
	return f
}

func (f *bridgeConfigFixture) advance(d time.Duration) { f.now = f.now.Add(d) }

func TestBridgeConfigReader_OnChainValueWins(t *testing.T) {
	f := newBridgeConfigFixture(t, func(symbol string) (*rtypes.BridgeConfigApply, error) {
		return &rtypes.BridgeConfigApply{
			AssetSymbol:                   symbol,
			WithdrawMaxFeeRateSatPerVbyte: 42,
			Rgb20WithdrawFeeLockVbytes:    500,
			Rgb20WithdrawFee: []*rtypes.Rgb20WithdrawFee{
				{Symbol: "RGB20_USDT", FeeMinUnits: 1000},
			},
		}, nil
	})

	require.Equal(t, uint32(42), f.client.withdrawMaxFeeRateSatPerVbyte())
	require.Equal(t, uint32(500), f.client.rgb20WithdrawFeeLockVbytes())
	require.Equal(t, int64(1000), f.client.rgb20WithdrawFeeForSymbol("RGB20_USDT"))
}

func TestBridgeConfigReader_TTLCacheHit(t *testing.T) {
	f := newBridgeConfigFixture(t, func(string) (*rtypes.BridgeConfigApply, error) {
		return &rtypes.BridgeConfigApply{WithdrawMaxFeeRateSatPerVbyte: 42}, nil
	})

	require.Equal(t, uint32(42), f.client.withdrawMaxFeeRateSatPerVbyte())
	require.Equal(t, 1, f.calls)

	// TTL 内：命中缓存，不再查询。
	f.advance(29 * time.Second)
	require.Equal(t, uint32(42), f.client.withdrawMaxFeeRateSatPerVbyte())
	require.Equal(t, 1, f.calls, "TTL 内必须命中缓存")

	// 超过 TTL：重新查询。
	f.advance(2 * time.Second)
	require.Equal(t, uint32(42), f.client.withdrawMaxFeeRateSatPerVbyte())
	require.Equal(t, 2, f.calls, "TTL 过期后必须重新查询")
}

func TestBridgeConfigReader_FirstFailureFallsBackToDefault(t *testing.T) {
	queryErr := errors.New("main chain unreachable")
	f := newBridgeConfigFixture(t, func(string) (*rtypes.BridgeConfigApply, error) {
		return nil, queryErr
	})

	require.Equal(t, uint32(defaultSweepMaxFeeRate), f.client.withdrawMaxFeeRateSatPerVbyte())
	require.Equal(t, int64(0), f.client.rgb20WithdrawFeeForSymbol("RGB20_USDT"),
		"从未成功过 ⇒ 手续费按 0（不校验）处理，签名端不拒签合法提现")
	require.Equal(t, uint32(0), f.client.rgb20WithdrawFeeLockVbytes())

	// 失败结果也缓存一个 TTL，避免主链不可用时反复打查询。
	calls := f.calls
	f.advance(time.Second)
	require.Equal(t, uint32(defaultSweepMaxFeeRate), f.client.withdrawMaxFeeRateSatPerVbyte())
	require.Equal(t, calls, f.calls, "TTL 内的重复失败不应再查询")
}

func TestBridgeConfigReader_FailureAfterSuccessUsesLastValue(t *testing.T) {
	var fail bool
	f := newBridgeConfigFixture(t, func(string) (*rtypes.BridgeConfigApply, error) {
		if fail {
			return nil, errors.New("main chain unreachable")
		}
		return &rtypes.BridgeConfigApply{
			WithdrawMaxFeeRateSatPerVbyte: 42,
			Rgb20WithdrawFeeLockVbytes:    500,
			Rgb20WithdrawFee: []*rtypes.Rgb20WithdrawFee{
				{Symbol: "RGB20_USDT", FeeMinUnits: 1000},
			},
		}, nil
	})

	require.Equal(t, uint32(42), f.client.withdrawMaxFeeRateSatPerVbyte())
	require.Equal(t, int64(1000), f.client.rgb20WithdrawFeeForSymbol("RGB20_USDT"))

	// 成功之后查询开始失败：TTL 过期后仍必须用最后一次成功值。
	fail = true
	f.advance(31 * time.Second)
	require.Equal(t, uint32(42), f.client.withdrawMaxFeeRateSatPerVbyte())
	require.Equal(t, uint32(500), f.client.rgb20WithdrawFeeLockVbytes())
	require.Equal(t, int64(1000), f.client.rgb20WithdrawFeeForSymbol("RGB20_USDT"))
	require.Greater(t, f.calls, 1, "TTL 过期后应真的重新尝试过查询")
}

func TestBridgeConfigReader_ZeroFallsBackToDefault(t *testing.T) {
	f := newBridgeConfigFixture(t, func(string) (*rtypes.BridgeConfigApply, error) {
		// 治理尚未设置这些字段：protobuf 解出来就是零值。
		return &rtypes.BridgeConfigApply{}, nil
	})

	require.Equal(t, uint32(defaultSweepMaxFeeRate), f.client.withdrawMaxFeeRateSatPerVbyte(),
		"链上值为 0 ⇒ 内置默认上限")
	require.Equal(t, int64(0), f.client.rgb20WithdrawFeeForSymbol("RGB20_USDT"),
		"链上未设置手续费 ⇒ 0 = 不收取")
	require.Equal(t, uint32(0), f.client.rgb20WithdrawFeeLockVbytes())
}

func TestBridgeConfigReader_Rgb20WithdrawFeeBySymbol(t *testing.T) {
	f := newBridgeConfigFixture(t, func(string) (*rtypes.BridgeConfigApply, error) {
		return &rtypes.BridgeConfigApply{
			Rgb20WithdrawFee: []*rtypes.Rgb20WithdrawFee{
				{Symbol: "RGB20_USDT", FeeMinUnits: 1000},
				{Symbol: "RGB20_XBTC", FeeMinUnits: 0},
			},
		}, nil
	})

	require.Equal(t, int64(1000), f.client.rgb20WithdrawFeeForSymbol("RGB20_USDT"))
	require.Equal(t, int64(0), f.client.rgb20WithdrawFeeForSymbol("RGB20_XBTC"),
		"显式配置为 0 与未配置同义：不收取")
	require.Equal(t, int64(0), f.client.rgb20WithdrawFeeForSymbol("RGB20_UNKNOWN"))
	require.Equal(t, int64(0), f.client.rgb20WithdrawFeeForSymbol(""))
}

func TestBridgeConfig_QueryUsesGlobalRecordForSharedFields(t *testing.T) {
	var symbols []string
	f := newBridgeConfigFixture(t, func(symbol string) (*rtypes.BridgeConfigApply, error) {
		symbols = append(symbols, symbol)
		return &rtypes.BridgeConfigApply{WithdrawMaxFeeRateSatPerVbyte: 42}, nil
	})

	f.client.withdrawMaxFeeRateSatPerVbyte()
	f.client.rgb20WithdrawFeeLockVbytes()
	require.Equal(t, []string{""}, symbols,
		"费率上限与锁定体积是全局旋钮：查询空 symbol 的全局记录（同一个 TTL 内共用一次查询）")
}

// 未初始化 reader（纯单测/非官方节点路径）不能 panic，一律回退默认值。
func TestBridgeConfig_NilReaderFallsBack(t *testing.T) {
	n := &neutrinoClient{}
	require.Equal(t, uint32(defaultSweepMaxFeeRate), n.withdrawMaxFeeRateSatPerVbyte())
	require.Equal(t, int64(0), n.rgb20WithdrawFeeForSymbol("RGB20_USDT"))
	require.Equal(t, uint32(0), n.rgb20WithdrawFeeLockVbytes())
}
