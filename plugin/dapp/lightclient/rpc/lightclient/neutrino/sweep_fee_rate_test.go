package neutrino

import (
	"errors"
	"testing"
	"time"

	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/stretchr/testify/require"
)

// withSweepFeeEstimateFn 替换动态费率估算实现，测试结束后还原。
func withSweepFeeEstimateFn(t *testing.T,
	fn func(*neutrinoClient) (*btcjson.EstimateSmartFeeResult, error)) {
	t.Helper()
	old := sweepFeeRateEstimateFn
	sweepFeeRateEstimateFn = fn
	t.Cleanup(func() { sweepFeeRateEstimateFn = old })
}

func feeRateResult(rate float64) *btcjson.EstimateSmartFeeResult {
	return &btcjson.EstimateSmartFeeResult{FeeRate: &rate}
}

// TestSweepSettings_MaxFeeRate 上限未配置链上值时用内置默认值，配置时取链上值。
func TestSweepSettings_MaxFeeRate(t *testing.T) {
	n := &neutrinoClient{}
	s := n.sweepSettings()
	require.Equal(t, uint32(defaultSweepMaxFeeRate), s.maxFeeRate)
	require.Equal(t, uint32(defaultSweepFeeRate), s.feeRate)

	n.bridgeConfig = newBridgeConfigReader(func(string) (*rtypes.BridgeConfigApply, error) {
		return &rtypes.BridgeConfigApply{WithdrawMaxFeeRateSatPerVbyte: 80}, nil
	}, time.Minute)
	n.cfg.UserDepositSweep.FeeRate = 5
	s = n.sweepSettings()
	require.Equal(t, uint32(80), s.maxFeeRate)
	require.Equal(t, uint32(5), s.feeRate)
}

// TestSweepFeeRate 估算成功取用、向上取整、超上限跳过、失败/无结果回退、低于配置取下限。
func TestSweepFeeRate(t *testing.T) {
	n := &neutrinoClient{}
	settings := sweepSettings{feeRate: 2, maxFeeRate: 50}

	tests := []struct {
		name       string
		estimate   func(*neutrinoClient) (*btcjson.EstimateSmartFeeResult, error)
		feeRate    uint32
		maxFeeRate uint32
		wantRate   uint32
		wantSweep  bool
	}{
		{
			name:       "estimate used when above configured feeRate",
			estimate:   func(*neutrinoClient) (*btcjson.EstimateSmartFeeResult, error) { return feeRateResult(0.00003), nil }, // 3 sat/vB
			feeRate:    2,
			maxFeeRate: 50,
			wantRate:   3,
			wantSweep:  true,
		},
		{
			name:       "estimate is rounded up",
			estimate:   func(*neutrinoClient) (*btcjson.EstimateSmartFeeResult, error) { return feeRateResult(0.0000301), nil }, // 3.01 sat/vB
			feeRate:    2,
			maxFeeRate: 50,
			wantRate:   4,
			wantSweep:  true,
		},
		{
			name:       "estimate above cap skips the round",
			estimate:   func(*neutrinoClient) (*btcjson.EstimateSmartFeeResult, error) { return feeRateResult(0.001), nil }, // 100 sat/vB
			feeRate:    2,
			maxFeeRate: 50,
			wantRate:   0,
			wantSweep:  false,
		},
		{
			name: "rpc failure falls back to configured feeRate",
			estimate: func(*neutrinoClient) (*btcjson.EstimateSmartFeeResult, error) {
				return nil, errors.New("Insufficient data or no feerate found")
			},
			feeRate:    2,
			maxFeeRate: 50,
			wantRate:   2,
			wantSweep:  true,
		},
		{
			name: "missing feerate falls back to configured feeRate",
			estimate: func(*neutrinoClient) (*btcjson.EstimateSmartFeeResult, error) {
				return &btcjson.EstimateSmartFeeResult{}, nil
			},
			feeRate:    2,
			maxFeeRate: 50,
			wantRate:   2,
			wantSweep:  true,
		},
		{
			name:       "zero feerate falls back to configured feeRate",
			estimate:   func(*neutrinoClient) (*btcjson.EstimateSmartFeeResult, error) { return feeRateResult(0), nil },
			feeRate:    2,
			maxFeeRate: 50,
			wantRate:   2,
			wantSweep:  true,
		},
		{
			name:       "estimate below configured feeRate keeps the lower bound",
			estimate:   func(*neutrinoClient) (*btcjson.EstimateSmartFeeResult, error) { return feeRateResult(0.00001), nil }, // 1 sat/vB
			feeRate:    5,
			maxFeeRate: 50,
			wantRate:   5,
			wantSweep:  true,
		},
		{
			name:       "estimate equal to configured feeRate",
			estimate:   func(*neutrinoClient) (*btcjson.EstimateSmartFeeResult, error) { return feeRateResult(0.00005), nil }, // 5 sat/vB
			feeRate:    5,
			maxFeeRate: 50,
			wantRate:   5,
			wantSweep:  true,
		},
		{
			name:       "configured feeRate above cap skips instead of exceeding the hard cap",
			estimate:   func(*neutrinoClient) (*btcjson.EstimateSmartFeeResult, error) { return feeRateResult(0.00001), nil },
			feeRate:    80,
			maxFeeRate: 50,
			wantRate:   0,
			wantSweep:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withSweepFeeEstimateFn(t, tt.estimate)
			local := settings
			local.feeRate = tt.feeRate
			local.maxFeeRate = tt.maxFeeRate
			rate, ok := n.sweepFeeRate(local)
			require.Equal(t, tt.wantSweep, ok)
			require.Equal(t, tt.wantRate, rate)
		})
	}
}

// TestSweepFeeRateEstimator_NoRPCClient 未配置 btcRPC 时估算返回错误（由调用方回退到配置费率）。
func TestSweepFeeRateEstimator_NoRPCClient(t *testing.T) {
	_, err := sweepFeeRateEstimateFn(&neutrinoClient{})
	require.Error(t, err)
}

// TestSweepSignFeeRate 签名核对口径：读链上 withdrawMaxFeeRateSatPerVbyte；
// 未配置/查询失败时回退到内置默认上限（不再取本地 feeRate）。
func TestSweepSignFeeRate(t *testing.T) {
	// nil service / 未初始化的 client 仍返回 0（跳过区间检查）。
	require.Equal(t, uint32(0), (*tssService)(nil).sweepSignFeeRate())
	require.Equal(t, uint32(0), (&tssService{}).sweepSignFeeRate())

	// 没有 reader（未初始化）时回退到内置默认值。
	require.Equal(t, uint32(defaultSweepMaxFeeRate),
		(&tssService{client: &neutrinoClient{}}).sweepSignFeeRate())

	// 链上值优先生效。
	client := &neutrinoClient{}
	client.bridgeConfig = newBridgeConfigReader(func(string) (*rtypes.BridgeConfigApply, error) {
		return &rtypes.BridgeConfigApply{WithdrawMaxFeeRateSatPerVbyte: 42}, nil
	}, time.Minute)
	require.Equal(t, uint32(42), (&tssService{client: client}).sweepSignFeeRate())
}
