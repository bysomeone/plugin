package executor

import (
	"testing"

	"github.com/33cn/chain33/types"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/stretchr/testify/require"
)

func queryEffectiveBridgeConfig(t *testing.T, r *rgbx, symbol string) *rtypes.EffectiveBridgeConfig {
	t.Helper()
	msg, err := r.Query_GetEffectiveBridgeConfig(&types.ReqString{Data: symbol})
	require.NoError(t, err)
	return msg.(*rtypes.EffectiveBridgeConfig)
}

// Missing records are not an error: the bridge is unrestricted, and the caller can tell
// "never configured" apart from "configured to zero" via the configured flag.
func TestEffectiveBridgeConfig_MissingRecords(t *testing.T) {
	r, _, _ := newBridgeConfigTestExec(t, bridgeConfigTestChainCfg())

	reply := queryEffectiveBridgeConfig(t, r, "usdt")
	require.False(t, reply.GetConfigured())
	require.Equal(t, "USDT", reply.GetAssetSymbol())
	require.NotNil(t, reply.GetConfig())
	require.False(t, reply.GetConfig().GetLimitsEnabled())
	require.False(t, reply.GetConfig().GetPauseDeposit())
	require.False(t, reply.GetConfig().GetPauseWithdraw())
	require.Zero(t, reply.GetPendingActivationHeight())
	require.Zero(t, reply.GetWithdrawFeeMinUnits())
}

// The merge follows enforcement: numericLimitsEnabled is the asset switch gated by the
// global master switch, pauses are independent and OR-ed, shared knobs come from global.
func TestEffectiveBridgeConfig_MergesGlobalAndAsset(t *testing.T) {
	r, state, _ := newBridgeConfigTestExec(t, bridgeConfigTestChainCfg())
	setCrossChainInfoForTest(t, state, "RGB20_USDT", rtypes.CrossChainAssetRgb20, 6, []byte{0x02})
	setBridgeConfigForTest(t, state, "", &rtypes.BridgeConfigApply{
		LimitsEnabled:                 true,
		PauseWithdraw:                 true,
		WithdrawMaxFeeRateSatPerVbyte: 50,
		Rgb20WithdrawFeeLockVbytes:    700,
	})
	setBridgeConfigForTest(t, state, "RGB20_USDT", &rtypes.BridgeConfigApply{
		AssetSymbol:    "RGB20_USDT",
		LimitsEnabled:  true,
		MaxPerTx:       1000,
		MaxPerWindow1H: 2000,
		PauseDeposit:   true,
		Rgb20WithdrawFee: []*rtypes.Rgb20WithdrawFee{
			{Symbol: "RGB20_USDT", FeeMinUnits: 99},
		},
	})

	reply := queryEffectiveBridgeConfig(t, r, "XRGB20_USDT")
	require.True(t, reply.GetConfigured())
	require.Equal(t, "RGB20_USDT", reply.GetAssetSymbol())
	cfg := reply.GetConfig()
	require.True(t, cfg.GetLimitsEnabled())
	require.Equal(t, int64(1000), cfg.GetMaxPerTx())
	require.Equal(t, int64(2000), cfg.GetMaxPerWindow1H())
	require.True(t, cfg.GetPauseDeposit(), "the asset pause is independent")
	require.True(t, cfg.GetPauseWithdraw(), "the global pause also applies to the asset")
	require.Equal(t, uint32(50), cfg.GetWithdrawMaxFeeRateSatPerVbyte())
	require.Equal(t, uint32(700), cfg.GetRgb20WithdrawFeeLockVbytes())
	require.Equal(t, int64(99), reply.GetWithdrawFeeMinUnits())
	require.Zero(t, reply.GetPendingActivationHeight())
}

// The global record's limitsEnabled is the master kill switch for the numeric limits.
func TestEffectiveBridgeConfig_GlobalKillSwitch(t *testing.T) {
	r, state, _ := newBridgeConfigTestExec(t, bridgeConfigTestChainCfg())
	setBridgeConfigForTest(t, state, "", &rtypes.BridgeConfigApply{LimitsEnabled: false})
	setBridgeConfigForTest(t, state, "BTC", &rtypes.BridgeConfigApply{
		AssetSymbol: "BTC", LimitsEnabled: true, MaxPerTx: 1000,
	})

	cfg := queryEffectiveBridgeConfig(t, r, "BTC").GetConfig()
	require.False(t, cfg.GetLimitsEnabled())
	require.Equal(t, int64(1000), cfg.GetMaxPerTx(), "the configured value is still reported")
}

// A registered asset can be queried with either spelling; both address its one record.
func TestEffectiveBridgeConfig_AcceptsBothSpellings(t *testing.T) {
	r, state, _ := newBridgeConfigTestExec(t, bridgeConfigTestChainCfg())
	setCrossChainInfoForTest(t, state, "BTC", rtypes.CrossChainAssetBtc, rtypes.BTCChainPrecision, []byte{0x02})
	setBridgeConfigForTest(t, state, "BTC", &rtypes.BridgeConfigApply{LimitsEnabled: true, MaxPerTx: 123})

	for _, spelling := range []string{"BTC", "XBTC", "btc"} {
		reply := queryEffectiveBridgeConfig(t, r, spelling)
		require.True(t, reply.GetConfigured(), "symbol=%s", spelling)
		require.Equal(t, int64(123), reply.GetConfig().GetMaxPerTx(), "symbol=%s", spelling)
		require.Equal(t, "BTC", reply.GetAssetSymbol(), "symbol=%s", spelling)
	}
}

// A scheduled loosening is visible before it activates: the effective values stay old and
// pendingActivationHeight tells the caller when the new ones will take over.
func TestEffectiveBridgeConfig_PendingLooseningExposesActivationHeight(t *testing.T) {
	r, state, _ := newBridgeConfigTestExec(t, bridgeConfigTestChainCfg())
	r.SetEnv(10, 0, 0)
	setBridgeConfigForTest(t, state, "BTC", &rtypes.BridgeConfigApply{
		AssetSymbol: "BTC", LimitsEnabled: true, MaxPerTx: 100,
	})
	require.NoError(t, state.Set(formatBridgeConfigPendingKey("BTC"), types.Encode(&bridgePendingConfig{
		Snapshot:         types.Encode(&rtypes.BridgeConfigApply{AssetSymbol: "BTC", LimitsEnabled: true, MaxPerTx: 500}),
		ActivationHeight: 100,
		ApplyID:          "apply",
	})))

	reply := queryEffectiveBridgeConfig(t, r, "BTC")
	require.Equal(t, int64(100), reply.GetPendingActivationHeight())
	require.Equal(t, int64(100), reply.GetConfig().GetMaxPerTx(),
		"the loosening is not effective before its activation height")

	r.SetEnv(100, 0, 0)
	reply = queryEffectiveBridgeConfig(t, r, "BTC")
	require.Zero(t, reply.GetPendingActivationHeight())
	require.Equal(t, int64(500), reply.GetConfig().GetMaxPerTx(),
		"at the activation height the scheduled config is effective")
}

// The empty symbol addresses the global record, which is the effective config there (there
// is no master switch above it).
func TestEffectiveBridgeConfig_GlobalSymbol(t *testing.T) {
	r, state, _ := newBridgeConfigTestExec(t, bridgeConfigTestChainCfg())
	setBridgeConfigForTest(t, state, "", &rtypes.BridgeConfigApply{
		PauseDeposit:                  true,
		WithdrawMaxFeeRateSatPerVbyte: 80,
	})

	reply := queryEffectiveBridgeConfig(t, r, "")
	require.True(t, reply.GetConfigured())
	require.True(t, reply.GetConfig().GetPauseDeposit())
	require.Equal(t, uint32(80), reply.GetConfig().GetWithdrawMaxFeeRateSatPerVbyte())
}

// Query_GetBridgeConfig keeps returning the raw stored record (no merge, no derived flags).
func TestEffectiveBridgeConfig_LegacyQueryUnchanged(t *testing.T) {
	r, state, _ := newBridgeConfigTestExec(t, bridgeConfigTestChainCfg())
	setBridgeConfigForTest(t, state, "", &rtypes.BridgeConfigApply{PauseWithdraw: true})
	setBridgeConfigForTest(t, state, "BTC", &rtypes.BridgeConfigApply{AssetSymbol: "BTC"})

	msg, err := r.Query_GetBridgeConfig(&types.ReqString{Data: "BTC"})
	require.NoError(t, err)
	cfg := msg.(*rtypes.BridgeConfigApply)
	require.False(t, cfg.GetPauseWithdraw(), "the raw asset record does not include the global pause")
	require.False(t, cfg.GetLimitsEnabled())
}
