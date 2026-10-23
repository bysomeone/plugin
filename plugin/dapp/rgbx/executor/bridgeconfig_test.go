package executor

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/33cn/chain33/client/mocks"
	"github.com/33cn/chain33/common/db"
	"github.com/33cn/chain33/types"
	"github.com/33cn/chain33/util"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

/*
 * Covers the governance-driven bridge config:
 *   - apply stores a pending snapshot; approve promotes it only after autonomy says yes;
 *   - the effective config drives pause flags and the rolling numeric limits;
 *   - with no config at all nothing changes (unlimited).
 */

// bridgeConfigTestChainCfg enables the manage-side autonomy executor, which is where the
// bridge reads the autonomy executor name from (mirroring chain33's manage executor).
func bridgeConfigTestChainCfg() *types.Chain33Config {
	cfgStr := strings.Replace(types.GetDefaultCfgstring(), `autonomyExec=""`, `autonomyExec="autonomy"`, 1)
	return types.NewChain33Config(cfgStr)
}

func newBridgeConfigTestExec(t *testing.T, cfg *types.Chain33Config) (*rgbx, db.DB, *mocks.QueueProtocolAPI) {
	t.Helper()
	dir, state, local := util.CreateTestDB()
	t.Cleanup(func() { util.CloseTestDB(dir, state) })
	api := &mocks.QueueProtocolAPI{}
	api.On("GetConfig").Return(cfg)
	r := newRgbx().(*rgbx)
	r.SetAPI(api)
	r.SetStateDB(state)
	r.SetLocalDB(local)
	return r, state, api
}

func signedTestTx(payload string) *types.Transaction {
	tx := &types.Transaction{Payload: []byte(payload)}
	tx.Sign(types.SECP256K1, testPriv)
	return tx
}

func setBridgeConfigForTest(t *testing.T, state kvSetter, symbol string, cfg *rtypes.BridgeConfigApply) {
	t.Helper()
	require.NoError(t, state.Set(formatBridgeConfigKey(symbol), types.Encode(cfg)))
}

func setBridgeWindowForTest(t *testing.T, state kvSetter, symbol string, kind bridgeWindowKind, bucket, amount int64) {
	t.Helper()
	require.NoError(t, state.Set(formatBridgeWindowKey(symbol, kind, bucket), types.Encode(&types.Int64{Data: amount})))
}

// newWithdrawConfigFixture registers an RGB20 asset and funds fromAddr, so checkWithdraw
// gets past the asset/balance branches and reaches the config verdict.
func newWithdrawConfigFixture(t *testing.T, cfg *types.Chain33Config) (*rgbx, db.DB, string) {
	t.Helper()
	r, state, _ := newBridgeConfigTestExec(t, cfg)
	from, _ := util.Genaddress()
	setCrossChainInfoForTest(t, state, "usdt", rtypes.CrossChainAssetRgb20, 6, []byte{0x02})
	acc, err := r.newAccount(ensureCrossChainSymbol("usdt"))
	require.NoError(t, err)
	_, err = acc.Mint(from, 1_000_000)
	require.NoError(t, err)
	return r, state, from
}

func rgb20Withdraw(amount int64) *rtypes.WithdrawAsset {
	return &rtypes.WithdrawAsset{
		Amount:          amount,
		FeeRate:         10,
		DestinationAddr: "rgb:utxob-test-invoice",
		AssetSymbol:     "usdt",
	}
}

func TestBridgeConfig_ApplyApprove(t *testing.T) {
	r, state, api := newBridgeConfigTestExec(t, bridgeConfigTestChainCfg())
	r.SetEnv(100, 0, 0)
	setCrossChainInfoForTest(t, state, "usdt", rtypes.CrossChainAssetRgb20, 6, []byte{0x02})
	api.On("QueryChain", mock.Anything).Return(&types.ReplyString{}, nil)

	global := &rtypes.BridgeConfigApply{LimitsEnabled: true, PauseDeposit: true}
	globalTx := signedTestTx("apply-global")
	receipt, err := r.Exec_BridgeConfigApply(global, globalTx, 0)
	require.NoError(t, err)
	require.Len(t, receipt.GetKV(), 1)
	applyStateKV(t, state, receipt.GetKV())

	// The record is stored, but nothing is effective yet.
	msg, err := r.Query_GetBridgeConfig(&types.ReqString{})
	require.NoError(t, err)
	require.False(t, msg.(*rtypes.BridgeConfigApply).GetPauseDeposit())

	// Approve without autonomy approval is rejected by the autonomy query and the record
	// stays pending.
	rejectExec, rejectState, rejectAPI := newBridgeConfigTestExec(t, bridgeConfigTestChainCfg())
	rejectAPI.On("QueryChain", mock.Anything).Return(nil, errors.New("not approved"))
	rejectReceipt, err := rejectExec.Exec_BridgeConfigApply(global, globalTx, 0)
	require.NoError(t, err)
	applyStateKV(t, rejectState, rejectReceipt.GetKV())
	_, err = rejectExec.Exec_BridgeConfigApprove(&rtypes.BridgeConfigApprove{
		ApplyConfigId: hex.EncodeToString(globalTx.Hash()), AutonomyItemId: "item-1",
	}, signedTestTx("approve"), 0)
	require.Error(t, err)
	msg, err = rejectExec.Query_GetBridgeConfig(&types.ReqString{})
	require.NoError(t, err)
	require.False(t, msg.(*rtypes.BridgeConfigApply).GetPauseDeposit())

	// Approve with autonomy approval promotes the snapshot.
	approveReceipt, err := r.Exec_BridgeConfigApprove(&rtypes.BridgeConfigApprove{
		ApplyConfigId: hex.EncodeToString(globalTx.Hash()), AutonomyItemId: "item-1",
	}, signedTestTx("approve-global"), 0)
	require.NoError(t, err)
	require.Len(t, approveReceipt.GetKV(), 2)
	applyStateKV(t, state, approveReceipt.GetKV())

	msg, err = r.Query_GetBridgeConfig(&types.ReqString{})
	require.NoError(t, err)
	effective := msg.(*rtypes.BridgeConfigApply)
	require.True(t, effective.GetLimitsEnabled())
	require.True(t, effective.GetPauseDeposit())

	// The same apply cannot be approved twice.
	_, err = r.Exec_BridgeConfigApprove(&rtypes.BridgeConfigApprove{
		ApplyConfigId: hex.EncodeToString(globalTx.Hash()), AutonomyItemId: "item-1",
	}, signedTestTx("approve-again"), 0)
	require.ErrorIs(t, err, ErrBridgeConfigNotPending)

	// Asset record approves into its own effective key.
	asset := &rtypes.BridgeConfigApply{AssetSymbol: "usdt", LimitsEnabled: true, MaxPerTx: 500}
	assetTx := signedTestTx("apply-asset")
	assetReceipt, err := r.Exec_BridgeConfigApply(asset, assetTx, 0)
	require.NoError(t, err)
	applyStateKV(t, state, assetReceipt.GetKV())
	assetApproveReceipt, err := r.Exec_BridgeConfigApprove(&rtypes.BridgeConfigApprove{
		ApplyConfigId: hex.EncodeToString(assetTx.Hash()), AutonomyItemId: "item-2",
	}, signedTestTx("approve-asset"), 0)
	require.NoError(t, err)
	applyStateKV(t, state, assetApproveReceipt.GetKV())
	assetEffective, err := r.loadBridgeConfig("usdt")
	require.NoError(t, err)
	require.Equal(t, int64(500), assetEffective.GetMaxPerTx())
}

func TestBridgeConfig_ApplyValidation(t *testing.T) {
	r, state, _ := newBridgeConfigTestExec(t, bridgeConfigTestChainCfg())

	// Empty symbol is the global record: numeric per-asset limits are not allowed.
	_, err := r.Exec_BridgeConfigApply(&rtypes.BridgeConfigApply{MaxPerTx: 1}, signedTestTx("a"), 0)
	require.ErrorIs(t, err, ErrInvalidBridgeConfig)

	// Negative values are rejected.
	_, err = r.Exec_BridgeConfigApply(&rtypes.BridgeConfigApply{AssetSymbol: "usdt", MaxPerTx: -1}, signedTestTx("b"), 0)
	require.ErrorIs(t, err, ErrInvalidBridgeConfig)

	// Non-empty symbol must be a registered cross-chain asset.
	_, err = r.Exec_BridgeConfigApply(&rtypes.BridgeConfigApply{AssetSymbol: "usdt", MaxPerTx: 1}, signedTestTx("c"), 0)
	require.ErrorIs(t, err, ErrGetCrossChainInfo)

	setCrossChainInfoForTest(t, state, "usdt", rtypes.CrossChainAssetRgb20, 6, []byte{0x02})
	_, err = r.Exec_BridgeConfigApply(&rtypes.BridgeConfigApply{AssetSymbol: "usdt", MaxPerTx: 1}, signedTestTx("d"), 0)
	require.NoError(t, err)
}

func TestBridgeConfig_ApproveWithoutAutonomyConfigured(t *testing.T) {
	// The default config leaves autonomyExec empty: approval must fail closed.
	r, state, _ := newBridgeConfigTestExec(t, types.NewChain33Config(types.GetDefaultCfgstring()))
	applyTx := signedTestTx("apply-no-autonomy")
	receipt, err := r.Exec_BridgeConfigApply(&rtypes.BridgeConfigApply{PauseWithdraw: true}, applyTx, 0)
	require.NoError(t, err)
	applyStateKV(t, state, receipt.GetKV())
	_, err = r.Exec_BridgeConfigApprove(&rtypes.BridgeConfigApprove{
		ApplyConfigId: hex.EncodeToString(applyTx.Hash()), AutonomyItemId: "item",
	}, signedTestTx("approve"), 0)
	require.ErrorIs(t, err, types.ErrNotFound)
}

func TestBridgeConfig_WithdrawLimits(t *testing.T) {
	cfg := bridgeConfigTestChainCfg()
	r, state, from := newWithdrawConfigFixture(t, cfg)

	// No config at all: unlimited.
	require.NoError(t, r.checkWithdraw(from, "tx", rgb20Withdraw(500_000)))

	// Pause is independent of limitsEnabled.
	setBridgeConfigForTest(t, state, "", &rtypes.BridgeConfigApply{PauseWithdraw: true})
	require.ErrorIs(t, r.checkWithdraw(from, "tx", rgb20Withdraw(1)), ErrBridgeWithdrawPaused)

	// Master switch off: per-asset limits are not enforced.
	setBridgeConfigForTest(t, state, "", &rtypes.BridgeConfigApply{LimitsEnabled: false})
	setBridgeConfigForTest(t, state, "usdt", &rtypes.BridgeConfigApply{LimitsEnabled: true, MaxPerTx: 10})
	require.NoError(t, r.checkWithdraw(from, "tx", rgb20Withdraw(100)))

	// Per-asset pause is honored independently of the global record.
	setBridgeConfigForTest(t, state, "usdt", &rtypes.BridgeConfigApply{PauseWithdraw: true})
	require.ErrorIs(t, r.checkWithdraw(from, "tx", rgb20Withdraw(1)), ErrBridgeWithdrawPaused)
}

func TestBridgeConfig_AssetRecordWithoutGlobalRecord(t *testing.T) {
	// An asset record can carry limits on its own; a missing global record is "not
	// configured", not "master switch off".
	r, state, from := newWithdrawConfigFixture(t, bridgeConfigTestChainCfg())
	setBridgeConfigForTest(t, state, "usdt", &rtypes.BridgeConfigApply{LimitsEnabled: true, MaxPerTx: 10})
	require.ErrorIs(t, r.checkWithdraw(from, "tx", rgb20Withdraw(100)), ErrBridgeWithdrawLimitExceeded)
	require.NoError(t, r.checkWithdraw(from, "tx", rgb20Withdraw(10)))
}

func TestBridgeConfig_WithdrawMaxPerTx(t *testing.T) {
	r, state, from := newWithdrawConfigFixture(t, bridgeConfigTestChainCfg())
	setBridgeConfigForTest(t, state, "", &rtypes.BridgeConfigApply{LimitsEnabled: true})
	setBridgeConfigForTest(t, state, "usdt", &rtypes.BridgeConfigApply{LimitsEnabled: true, MaxPerTx: 100})

	require.ErrorIs(t, r.checkWithdraw(from, "tx", rgb20Withdraw(101)), ErrBridgeWithdrawLimitExceeded)
	require.NoError(t, r.checkWithdraw(from, "tx", rgb20Withdraw(100)))
}

func TestBridgeConfig_WithdrawWindow1h(t *testing.T) {
	r, state, from := newWithdrawConfigFixture(t, bridgeConfigTestChainCfg())
	setBridgeConfigForTest(t, state, "", &rtypes.BridgeConfigApply{LimitsEnabled: true})
	setBridgeConfigForTest(t, state, "usdt", &rtypes.BridgeConfigApply{LimitsEnabled: true, MaxPerWindow1H: 100})

	height := blocksPerHour*3 + 7
	r.SetEnv(height, 0, 0)
	setBridgeWindowForTest(t, state, "usdt", bridgeWindowWithdraw, bridgeWindowBucket(height), 90)

	require.ErrorIs(t, r.checkWithdraw(from, "tx", rgb20Withdraw(20)), ErrBridgeWithdrawLimitExceeded)
	require.NoError(t, r.checkWithdraw(from, "tx", rgb20Withdraw(10)))

	// Crossing into the next hourly bucket resets the 1h window.
	r.SetEnv(height+blocksPerHour, 0, 0)
	require.NoError(t, r.checkWithdraw(from, "tx", rgb20Withdraw(100)))
}

func TestBridgeConfig_WithdrawWindow24h(t *testing.T) {
	r, state, from := newWithdrawConfigFixture(t, bridgeConfigTestChainCfg())
	setBridgeConfigForTest(t, state, "", &rtypes.BridgeConfigApply{LimitsEnabled: true})
	setBridgeConfigForTest(t, state, "usdt", &rtypes.BridgeConfigApply{LimitsEnabled: true, MaxPerWindow24H: 100})

	height := blocksPerHour * 100
	r.SetEnv(height, 0, 0)
	bucket := bridgeWindowBucket(height)
	setBridgeWindowForTest(t, state, "usdt", bridgeWindowWithdraw, bucket, 40)
	setBridgeWindowForTest(t, state, "usdt", bridgeWindowWithdraw, bucket-1, 60)

	require.ErrorIs(t, r.checkWithdraw(from, "tx", rgb20Withdraw(1)), ErrBridgeWithdrawLimitExceeded)

	// The 24h window is rolling: the earlier bucket only drops out after 24 hours.
	r.SetEnv(height+blocksPerHour, 0, 0)
	require.ErrorIs(t, r.checkWithdraw(from, "tx", rgb20Withdraw(1)), ErrBridgeWithdrawLimitExceeded)
	r.SetEnv(height+24*blocksPerHour, 0, 0)
	require.NoError(t, r.checkWithdraw(from, "tx", rgb20Withdraw(1)))
}

func TestBridgeConfig_DepositPauseAndMintLimit(t *testing.T) {
	cfg := bridgeConfigTestChainCfg()
	r, state, _ := newBridgeConfigTestExec(t, cfg)
	addr, _ := util.Genaddress()
	deposit := func(amount int64) *rtypes.DepositAsset {
		return &rtypes.DepositAsset{Amount: amount, DepositAddress: addr, AssetSymbol: "usdt"}
	}

	// No config: pause does not trigger.
	require.Error(t, r.checkDeposit("tx", deposit(1))) // stops later at proof validation
	require.NotErrorIs(t, r.checkDeposit("tx", deposit(1)), ErrBridgeDepositPaused)

	setBridgeConfigForTest(t, state, "", &rtypes.BridgeConfigApply{PauseDeposit: true})
	require.ErrorIs(t, r.checkDeposit("tx", deposit(1)), ErrBridgeDepositPaused)

	setBridgeConfigForTest(t, state, "", &rtypes.BridgeConfigApply{LimitsEnabled: true})
	setBridgeConfigForTest(t, state, "usdt", &rtypes.BridgeConfigApply{LimitsEnabled: true, MaxMintPerWindow: 100})
	height := blocksPerHour * 2
	r.SetEnv(height, 0, 0)
	setBridgeWindowForTest(t, state, "usdt", bridgeWindowMint, bridgeWindowBucket(height), 60)

	require.ErrorIs(t, r.checkDeposit("tx", deposit(50)), ErrBridgeMintLimitExceeded)
	// Within the limit the config lets it through; the dummy proof then fails later.
	require.NotErrorIs(t, r.checkDeposit("tx", deposit(40)), ErrBridgeMintLimitExceeded)

	// Pause wins over the mint limit.
	setBridgeConfigForTest(t, state, "", &rtypes.BridgeConfigApply{LimitsEnabled: true, PauseDeposit: true})
	require.ErrorIs(t, r.checkDeposit("tx", deposit(50)), ErrBridgeDepositPaused)

	// A per-asset pause is honored as well.
	setBridgeConfigForTest(t, state, "", &rtypes.BridgeConfigApply{LimitsEnabled: true})
	setBridgeConfigForTest(t, state, "usdt", &rtypes.BridgeConfigApply{LimitsEnabled: true, PauseDeposit: true})
	require.ErrorIs(t, r.checkDeposit("tx", deposit(50)), ErrBridgeDepositPaused)
}

func TestBridgeConfig_ExecDepositEnforcesConfig(t *testing.T) {
	r, state, _ := newBridgeConfigTestExec(t, bridgeConfigTestChainCfg())
	setBridgeConfigForTest(t, state, "", &rtypes.BridgeConfigApply{PauseDeposit: true})
	_, err := r.Exec_Deposit(&rtypes.DepositAsset{Amount: 1, AssetSymbol: "btc"}, signedTestTx("d0"), 0)
	require.ErrorIs(t, err, ErrBridgeDepositPaused)

	setBridgeConfigForTest(t, state, "", &rtypes.BridgeConfigApply{LimitsEnabled: true})
	setBridgeConfigForTest(t, state, "btc", &rtypes.BridgeConfigApply{LimitsEnabled: true, MaxMintPerWindow: 10})
	setBridgeWindowForTest(t, state, "btc", bridgeWindowMint, bridgeWindowBucket(0), 10)
	_, err = r.Exec_Deposit(&rtypes.DepositAsset{Amount: 1, AssetSymbol: "btc"}, signedTestTx("d1"), 0)
	require.ErrorIs(t, err, ErrBridgeMintLimitExceeded)
}

func TestBridgeConfig_ExecDepositTracksMintWindow(t *testing.T) {
	r, state, _ := newBridgeConfigTestExec(t, bridgeConfigTestChainCfg())
	setCrossChainInfoForTest(t, state, "btc", rtypes.CrossChainAssetBtc, rtypes.BTCChainPrecision,
		newP2WSHDepositFixture(t, frozenVectorA).tssPub)
	setBridgeConfigForTest(t, state, "", &rtypes.BridgeConfigApply{LimitsEnabled: true})
	setBridgeConfigForTest(t, state, "btc", &rtypes.BridgeConfigApply{LimitsEnabled: true, MaxMintPerWindow: 10_000})

	height := int64(1234)
	r.SetEnv(height, 0, 0)
	deposit := &rtypes.DepositAsset{
		Amount:         100,
		DepositAddress: testCommitAddr,
		AssetSymbol:    "btc",
		TxProof:        &rtypes.BtcTxProof{TxData: canonicalBtcTxData(t, "bridge-mint-window", 100)},
	}
	receipt, err := r.Exec_Deposit(deposit, signedTestTx("deposit"), 0)
	require.NoError(t, err)
	applyStateKV(t, state, receipt.GetKV())

	counter, err := r.loadBridgeWindowCounter("btc", bridgeWindowMint, bridgeWindowBucket(height))
	require.NoError(t, err)
	require.Equal(t, int64(100), counter)
}

func TestBridgeConfig_ExecWithdrawTracksWindow(t *testing.T) {
	r, state, _ := newBridgeConfigTestExec(t, bridgeConfigTestChainCfg())
	require.NoError(t, state.Set(formatCrossChainInfoKey("BTC"), types.Encode(&rtypes.CrossChainInfo{AssetSymbol: "BTC"})))
	acc, err := r.newAccount("xBTC")
	require.NoError(t, err)
	_, err = acc.Mint(testCommitAddr, 1000)
	require.NoError(t, err)
	setBridgeConfigForTest(t, state, "", &rtypes.BridgeConfigApply{LimitsEnabled: true})
	setBridgeConfigForTest(t, state, "BTC", &rtypes.BridgeConfigApply{LimitsEnabled: true, MaxPerWindow1H: 1000})

	height := int64(77)
	r.SetEnv(height, 0, 0)
	withdraw := &rtypes.WithdrawAsset{
		Amount:          600,
		FeeRate:         10,
		DestinationAddr: "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kygt080",
		AssetSymbol:     "BTC",
	}
	receipt, err := r.Exec_Withdraw(withdraw, signedTestTx("withdraw"), 0)
	require.NoError(t, err)
	applyStateKV(t, state, receipt.GetKV())

	counter, err := r.loadBridgeWindowCounter("BTC", bridgeWindowWithdraw, bridgeWindowBucket(height))
	require.NoError(t, err)
	require.Equal(t, int64(600), counter)
}

func TestBridgeConfig_ExecWithdrawEnforcesWindow(t *testing.T) {
	// CheckTx only guards the mempool; Exec must reject the second withdrawal packed into
	// the same block when the pair exceeds the window.
	r, state, _ := newBridgeConfigTestExec(t, bridgeConfigTestChainCfg())
	require.NoError(t, state.Set(formatCrossChainInfoKey("BTC"), types.Encode(&rtypes.CrossChainInfo{AssetSymbol: "BTC"})))
	acc, err := r.newAccount("xBTC")
	require.NoError(t, err)
	_, err = acc.Mint(testCommitAddr, 1000)
	require.NoError(t, err)
	setBridgeConfigForTest(t, state, "BTC", &rtypes.BridgeConfigApply{LimitsEnabled: true, MaxPerWindow1H: 500})
	r.SetEnv(10, 0, 0)

	withdraw := func(amount int64) *rtypes.WithdrawAsset {
		return &rtypes.WithdrawAsset{
			Amount:          amount,
			FeeRate:         10,
			DestinationAddr: "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kygt080",
			AssetSymbol:     "BTC",
		}
	}
	first, err := r.Exec_Withdraw(withdraw(400), signedTestTx("withdraw-first"), 0)
	require.NoError(t, err)
	applyStateKV(t, state, first.GetKV())

	_, err = r.Exec_Withdraw(withdraw(400), signedTestTx("withdraw-second"), 1)
	require.ErrorIs(t, err, ErrBridgeWithdrawLimitExceeded)
}

func TestBridgeConfig_QueryDefaultsToUnlimited(t *testing.T) {
	r, _, _ := newBridgeConfigTestExec(t, bridgeConfigTestChainCfg())
	msg, err := r.Query_GetBridgeConfig(&types.ReqString{Data: "usdt"})
	require.NoError(t, err)
	cfg := msg.(*rtypes.BridgeConfigApply)
	require.Equal(t, "USDT", cfg.GetAssetSymbol())
	require.False(t, cfg.GetLimitsEnabled())
	require.False(t, cfg.GetPauseWithdraw())
}

// TestBridgeConfig_ActionDispatch proves the action names in the frozen oneof are wired to
// the Exec methods (DriverBase dispatches "Exec_" + oneof field name).
func TestBridgeConfig_ActionDispatch(t *testing.T) {
	r, state, api := newBridgeConfigTestExec(t, bridgeConfigTestChainCfg())
	api.On("QueryChain", mock.Anything).Return(&types.ReplyString{}, nil)

	apply := &rtypes.BridgeConfigApply{PauseWithdraw: true}
	applyTx, err := r.GetExecutorType().CreateTransaction(rtypes.NameBridgeConfigApplyAction, apply)
	require.NoError(t, err)
	applyTx.Sign(types.SECP256K1, testPriv)
	applyReceipt, err := r.Exec(applyTx, 0)
	require.NoError(t, err)
	require.Len(t, applyReceipt.GetKV(), 1)
	applyStateKV(t, state, applyReceipt.GetKV())

	approve := &rtypes.BridgeConfigApprove{
		ApplyConfigId:  hex.EncodeToString(applyTx.Hash()),
		AutonomyItemId: "item",
	}
	approveTx, err := r.GetExecutorType().CreateTransaction(rtypes.NameBridgeConfigApproveAction, approve)
	require.NoError(t, err)
	approveTx.Sign(types.SECP256K1, testPriv)
	_, err = r.Exec(approveTx, 0)
	require.NoError(t, err)

	// CheckTx dispatch uses the same action names.
	badApply, err := r.GetExecutorType().CreateTransaction(rtypes.NameBridgeConfigApplyAction,
		&rtypes.BridgeConfigApply{MaxPerTx: 1})
	require.NoError(t, err)
	require.ErrorIs(t, r.CheckTx(badApply, 0), ErrInvalidBridgeConfig)
	badApprove, err := r.GetExecutorType().CreateTransaction(rtypes.NameBridgeConfigApproveAction,
		&rtypes.BridgeConfigApprove{})
	require.NoError(t, err)
	require.ErrorIs(t, r.CheckTx(badApprove, 0), ErrInvalidBridgeConfig)
}

// TestBridgeConfig_DepositExtraConfirmations pins the large-deposit confirmation rule: the
// extra depth applies only at or above the configured threshold, only to that asset, and
// nothing changes when no config exists.
func TestBridgeConfig_DepositExtraConfirmations(t *testing.T) {
	r, state, _ := newBridgeConfigTestExec(t, bridgeConfigTestChainCfg())

	extra, err := r.depositExtraConfirmations(&rtypes.DepositAsset{AssetSymbol: "BTC", Amount: 1_000_000})
	require.NoError(t, err)
	require.Zero(t, extra, "no config means no extra wait")

	setBridgeConfigForTest(t, state, formatSymbol("BTC"), &rtypes.BridgeConfigApply{
		AssetSymbol:                    "BTC",
		LargeDepositThreshold:          1_000_000,
		LargeDepositExtraConfirmations: 12,
	})

	below, err := r.depositExtraConfirmations(&rtypes.DepositAsset{AssetSymbol: "BTC", Amount: 999_999})
	require.NoError(t, err)
	require.Zero(t, below, "below the threshold uses the base confirmation count")

	at, err := r.depositExtraConfirmations(&rtypes.DepositAsset{AssetSymbol: "BTC", Amount: 1_000_000})
	require.NoError(t, err)
	require.Equal(t, int64(12), at, "at the threshold the extra depth applies")

	other, err := r.depositExtraConfirmations(&rtypes.DepositAsset{AssetSymbol: "RGB20_USDT", Amount: 1_000_000})
	require.NoError(t, err)
	require.Zero(t, other, "a different asset is unaffected")
}

// applyAndApproveBridgeConfig runs the two-step governance flow and applies the resulting
// receipt KVs, so the test observes what the chain would store.
func applyAndApproveBridgeConfig(t *testing.T, r *rgbx, state kvSetter,
	cfg *rtypes.BridgeConfigApply, label string) {
	t.Helper()
	applyTx := signedTestTx("apply-" + label)
	applyReceipt, err := r.Exec_BridgeConfigApply(cfg, applyTx, 0)
	require.NoError(t, err)
	applyStateKV(t, state, applyReceipt.GetKV())

	approveReceipt, err := r.Exec_BridgeConfigApprove(&rtypes.BridgeConfigApprove{
		ApplyConfigId:  hex.EncodeToString(applyTx.Hash()),
		AutonomyItemId: "item-" + label,
	}, signedTestTx("approve-"+label), 0)
	require.NoError(t, err)
	applyStateKV(t, state, approveReceipt.GetKV())
}

// advancePastBridgeTimelock moves execution past the activation height of a scheduled
// loosening so the test can observe the promoted config.
func advancePastBridgeTimelock(r *rgbx) {
	r.SetEnv(r.GetHeight()+bridgeConfigTimelockBlocks+1, 0, 0)
}

func TestBridgeConfig_LooseningWaitsForTimelock(t *testing.T) {
	r, state, api := newBridgeConfigTestExec(t, bridgeConfigTestChainCfg())
	api.On("QueryChain", mock.Anything).Return(&types.ReplyString{}, nil)
	r.SetEnv(1000, 0, 0)
	// Start from an effective config that pauses withdrawals.
	setBridgeConfigForTest(t, state, "", &rtypes.BridgeConfigApply{PauseWithdraw: true})

	// Unpausing is a loosening: approval stores a pending record, the pause stays in force.
	applyAndApproveBridgeConfig(t, r, state, &rtypes.BridgeConfigApply{PauseWithdraw: false}, "unpause")
	effective, err := r.loadBridgeConfig("")
	require.NoError(t, err)
	require.True(t, effective.GetPauseWithdraw(), "an unpause must not take effect before the timelock")

	pending, err := r.loadBridgePendingConfig("")
	require.NoError(t, err)
	require.NotNil(t, pending)
	require.Equal(t, int64(1000)+bridgeConfigTimelockBlocks, pending.ActivationHeight)

	// At the activation height the scheduled config becomes effective without another tx.
	r.SetEnv(pending.ActivationHeight, 0, 0)
	effective, err = r.loadBridgeConfig("")
	require.NoError(t, err)
	require.False(t, effective.GetPauseWithdraw())
}

func TestBridgeConfig_TighteningAppliesImmediatelyAndCancelsPending(t *testing.T) {
	r, state, api := newBridgeConfigTestExec(t, bridgeConfigTestChainCfg())
	api.On("QueryChain", mock.Anything).Return(&types.ReplyString{}, nil)
	r.SetEnv(2000, 0, 0)
	setBridgeConfigForTest(t, state, "usdt", &rtypes.BridgeConfigApply{
		AssetSymbol: "usdt", LimitsEnabled: true, MaxPerTx: 100,
	})
	setCrossChainInfoForTest(t, state, "usdt", rtypes.CrossChainAssetRgb20, 6, []byte{0x02})

	// Schedule a loosening (raise the limit).
	applyAndApproveBridgeConfig(t, r, state, &rtypes.BridgeConfigApply{
		AssetSymbol: "usdt", MaxPerTx: 500, Fields: []string{rtypes.BridgeConfigFieldMaxPerTx},
	}, "raise")
	require.NotNil(t, mustPending(t, r, "usdt"))

	// A later tightening takes effect immediately and removes the scheduled loosening.
	applyAndApproveBridgeConfig(t, r, state, &rtypes.BridgeConfigApply{
		AssetSymbol: "usdt", PauseWithdraw: true, Fields: []string{rtypes.BridgeConfigFieldPauseWithdraw},
	}, "pause")
	effective, err := r.loadBridgeConfig("usdt")
	require.NoError(t, err)
	require.True(t, effective.GetPauseWithdraw())
	require.Equal(t, int64(100), effective.GetMaxPerTx(), "the scheduled raise was cancelled")
	require.Nil(t, mustPending(t, r, "usdt"))
}

func mustPending(t *testing.T, r *rgbx, symbol string) *bridgePendingConfig {
	t.Helper()
	pending, err := r.loadBridgePendingConfig(symbol)
	require.NoError(t, err)
	return pending
}

func TestBridgeConfig_PartialMaskOnlyTouchesListedFields(t *testing.T) {
	r, state, api := newBridgeConfigTestExec(t, bridgeConfigTestChainCfg())
	api.On("QueryChain", mock.Anything).Return(&types.ReplyString{}, nil)
	setCrossChainInfoForTest(t, state, "usdt", rtypes.CrossChainAssetRgb20, 6, []byte{0x02})

	setBridgeConfigForTest(t, state, "usdt", &rtypes.BridgeConfigApply{
		AssetSymbol:                   "usdt",
		LimitsEnabled:                 true,
		MaxPerTx:                      100,
		PauseDeposit:                  true,
		WithdrawMaxFeeRateSatPerVbyte: 50,
	})

	// The apply names maxPerTx only; its other payload values must not leak into the
	// effective config.
	applyAndApproveBridgeConfig(t, r, state, &rtypes.BridgeConfigApply{
		AssetSymbol:   "usdt",
		MaxPerTx:      200,
		PauseWithdraw: true,
		Fields:        []string{rtypes.BridgeConfigFieldMaxPerTx},
	}, "mask")
	// Raising a limit is a loosening: it waits out the timelock before it is visible.
	advancePastBridgeTimelock(r)

	effective, err := r.loadBridgeConfig("usdt")
	require.NoError(t, err)
	require.Equal(t, int64(200), effective.GetMaxPerTx(), "listed field is updated")
	require.True(t, effective.GetLimitsEnabled(), "unlisted field keeps its on-chain value")
	require.True(t, effective.GetPauseDeposit(), "unlisted field keeps its on-chain value")
	require.False(t, effective.GetPauseWithdraw(), "an unlisted payload value is not applied")
	require.Equal(t, uint32(50), effective.GetWithdrawMaxFeeRateSatPerVbyte())
	require.Empty(t, effective.GetFields(), "the mask itself is never persisted")
}

func TestBridgeConfig_PartialMaskOnMissingBaseUsesZeroDefaults(t *testing.T) {
	r, state, api := newBridgeConfigTestExec(t, bridgeConfigTestChainCfg())
	api.On("QueryChain", mock.Anything).Return(&types.ReplyString{}, nil)
	setCrossChainInfoForTest(t, state, "usdt", rtypes.CrossChainAssetRgb20, 6, []byte{0x02})

	applyAndApproveBridgeConfig(t, r, state, &rtypes.BridgeConfigApply{
		AssetSymbol:  "usdt",
		PauseDeposit: true,
		MaxPerTx:     999,
		Fields:       []string{rtypes.BridgeConfigFieldPauseDeposit},
	}, "first-mask")

	effective, err := r.loadBridgeConfig("usdt")
	require.NoError(t, err)
	require.True(t, effective.GetPauseDeposit())
	require.Zero(t, effective.GetMaxPerTx(), "unlisted fields default to zero on first apply")
}

func TestBridgeConfig_UnknownMaskFieldRejected(t *testing.T) {
	r, state, _ := newBridgeConfigTestExec(t, bridgeConfigTestChainCfg())
	setCrossChainInfoForTest(t, state, "usdt", rtypes.CrossChainAssetRgb20, 6, []byte{0x02})

	_, err := r.Exec_BridgeConfigApply(&rtypes.BridgeConfigApply{
		AssetSymbol: "usdt", Fields: []string{"notAField"},
	}, signedTestTx("unknown-field"), 0)
	require.ErrorIs(t, err, ErrInvalidBridgeConfig)

	// assetSymbol identifies the record and cannot be changed through a mask.
	_, err = r.Exec_BridgeConfigApply(&rtypes.BridgeConfigApply{
		AssetSymbol: "usdt", Fields: []string{"assetSymbol"},
	}, signedTestTx("mask-asset-symbol"), 0)
	require.ErrorIs(t, err, ErrInvalidBridgeConfig)

	// The same check runs in CheckTx.
	bad, txErr := r.GetExecutorType().CreateTransaction(rtypes.NameBridgeConfigApplyAction,
		&rtypes.BridgeConfigApply{AssetSymbol: "usdt", Fields: []string{"notAField"}})
	require.NoError(t, txErr)
	require.ErrorIs(t, r.CheckTx(bad, 0), ErrInvalidBridgeConfig)
}

func TestBridgeConfig_Rgb20WithdrawFeeValidation(t *testing.T) {
	r, state, _ := newBridgeConfigTestExec(t, bridgeConfigTestChainCfg())
	setCrossChainInfoForTest(t, state, "usdt", rtypes.CrossChainAssetRgb20, 6, []byte{0x02})
	setCrossChainInfoForTest(t, state, "btc", rtypes.CrossChainAssetBtc,
		rtypes.BTCChainPrecision, []byte{0x02})

	// fee = 0 is legal.
	_, err := r.Exec_BridgeConfigApply(&rtypes.BridgeConfigApply{
		Rgb20WithdrawFee: []*rtypes.Rgb20WithdrawFee{{Symbol: "usdt", FeeMinUnits: 0}},
	}, signedTestTx("fee-zero"), 0)
	require.NoError(t, err)

	// Negative fee is rejected.
	_, err = r.Exec_BridgeConfigApply(&rtypes.BridgeConfigApply{
		Rgb20WithdrawFee: []*rtypes.Rgb20WithdrawFee{{Symbol: "usdt", FeeMinUnits: -1}},
	}, signedTestTx("fee-negative"), 0)
	require.ErrorIs(t, err, ErrInvalidBridgeConfig)

	// Duplicate symbols are rejected after normalization.
	_, err = r.Exec_BridgeConfigApply(&rtypes.BridgeConfigApply{
		Rgb20WithdrawFee: []*rtypes.Rgb20WithdrawFee{
			{Symbol: "usdt", FeeMinUnits: 1},
			{Symbol: "USDT", FeeMinUnits: 2},
		},
	}, signedTestTx("fee-duplicate"), 0)
	require.ErrorIs(t, err, ErrInvalidBridgeConfig)

	// Empty symbol is invalid.
	_, err = r.Exec_BridgeConfigApply(&rtypes.BridgeConfigApply{
		Rgb20WithdrawFee: []*rtypes.Rgb20WithdrawFee{{Symbol: "", FeeMinUnits: 1}},
	}, signedTestTx("fee-empty-symbol"), 0)
	require.ErrorIs(t, err, ErrInvalidAssetSymbol)

	// A registered BTC-kind asset is not allowed.
	_, err = r.Exec_BridgeConfigApply(&rtypes.BridgeConfigApply{
		Rgb20WithdrawFee: []*rtypes.Rgb20WithdrawFee{{Symbol: "btc", FeeMinUnits: 1}},
	}, signedTestTx("fee-btc"), 0)
	require.ErrorIs(t, err, ErrInvalidAssetKind)

	// An unregistered asset is rejected rather than assumed to be RGB20.
	_, err = r.Exec_BridgeConfigApply(&rtypes.BridgeConfigApply{
		Rgb20WithdrawFee: []*rtypes.Rgb20WithdrawFee{{Symbol: "unknown", FeeMinUnits: 1}},
	}, signedTestTx("fee-unknown"), 0)
	require.ErrorIs(t, err, ErrGetCrossChainInfo)
}

func TestBridgeConfig_FeeRelatedUpperBounds(t *testing.T) {
	r, _, _ := newBridgeConfigTestExec(t, bridgeConfigTestChainCfg())

	// The fee-rate cap uses the executor's existing hard bound (1000 sat/vB).
	_, err := r.Exec_BridgeConfigApply(&rtypes.BridgeConfigApply{
		WithdrawMaxFeeRateSatPerVbyte: uint32(maxBtcFeeRate) + 1,
	}, signedTestTx("rate-over"), 0)
	require.ErrorIs(t, err, ErrInvalidBridgeConfig)
	_, err = r.Exec_BridgeConfigApply(&rtypes.BridgeConfigApply{
		WithdrawMaxFeeRateSatPerVbyte: uint32(maxBtcFeeRate),
	}, signedTestTx("rate-max"), 0)
	require.NoError(t, err)

	// The conservative transaction size is bounded by maxRgb20WithdrawFeeLockVbytes.
	_, err = r.Exec_BridgeConfigApply(&rtypes.BridgeConfigApply{
		Rgb20WithdrawFeeLockVbytes: maxRgb20WithdrawFeeLockVbytes + 1,
	}, signedTestTx("vbytes-over"), 0)
	require.ErrorIs(t, err, ErrInvalidBridgeConfig)
	_, err = r.Exec_BridgeConfigApply(&rtypes.BridgeConfigApply{
		Rgb20WithdrawFeeLockVbytes: maxRgb20WithdrawFeeLockVbytes,
	}, signedTestTx("vbytes-max"), 0)
	require.NoError(t, err)
}

func TestBridgeConfig_QueryReturnsFeeFields(t *testing.T) {
	r, state, api := newBridgeConfigTestExec(t, bridgeConfigTestChainCfg())
	api.On("QueryChain", mock.Anything).Return(&types.ReplyString{}, nil)
	setCrossChainInfoForTest(t, state, "usdt", rtypes.CrossChainAssetRgb20, 6, []byte{0x02})
	setCrossChainInfoForTest(t, state, "other", rtypes.CrossChainAssetRgb20, 6, []byte{0x02})

	applyAndApproveBridgeConfig(t, r, state, &rtypes.BridgeConfigApply{
		AssetSymbol:                   "usdt",
		WithdrawMaxFeeRateSatPerVbyte: 25,
		Rgb20WithdrawFeeLockVbytes:    500,
		Rgb20WithdrawFee: []*rtypes.Rgb20WithdrawFee{
			{Symbol: "usdt", FeeMinUnits: 100},
			{Symbol: "other", FeeMinUnits: 0},
		},
	}, "query")

	msg, err := r.Query_GetBridgeConfig(&types.ReqString{Data: "usdt"})
	require.NoError(t, err)
	cfg := msg.(*rtypes.BridgeConfigApply)
	require.Equal(t, uint32(25), cfg.GetWithdrawMaxFeeRateSatPerVbyte())
	require.Equal(t, uint32(500), cfg.GetRgb20WithdrawFeeLockVbytes())
	require.Len(t, cfg.GetRgb20WithdrawFee(), 2)
	require.Equal(t, "usdt", cfg.GetRgb20WithdrawFee()[0].GetSymbol())
	require.Equal(t, int64(100), cfg.GetRgb20WithdrawFee()[0].GetFeeMinUnits())
	require.Equal(t, "other", cfg.GetRgb20WithdrawFee()[1].GetSymbol())
	require.Zero(t, cfg.GetRgb20WithdrawFee()[1].GetFeeMinUnits())
}

func TestBridgeConfig_MaskReplacesFeeListWholesale(t *testing.T) {
	r, state, api := newBridgeConfigTestExec(t, bridgeConfigTestChainCfg())
	api.On("QueryChain", mock.Anything).Return(&types.ReplyString{}, nil)
	setCrossChainInfoForTest(t, state, "usdt", rtypes.CrossChainAssetRgb20, 6, []byte{0x02})
	setCrossChainInfoForTest(t, state, "other", rtypes.CrossChainAssetRgb20, 6, []byte{0x02})

	setBridgeConfigForTest(t, state, "", &rtypes.BridgeConfigApply{
		Rgb20WithdrawFee: []*rtypes.Rgb20WithdrawFee{
			{Symbol: "usdt", FeeMinUnits: 5},
			{Symbol: "other", FeeMinUnits: 7},
		},
	})

	applyAndApproveBridgeConfig(t, r, state, &rtypes.BridgeConfigApply{
		Rgb20WithdrawFee: []*rtypes.Rgb20WithdrawFee{{Symbol: "usdt", FeeMinUnits: 9}},
		Fields:           []string{rtypes.BridgeConfigFieldRgb20WithdrawFee},
	}, "list-replace")
	// Dropping "other" from the list lowers the fees collected: a loosening.
	advancePastBridgeTimelock(r)

	effective, err := r.loadBridgeConfig("")
	require.NoError(t, err)
	require.Len(t, effective.GetRgb20WithdrawFee(), 1, "the list is replaced, not merged")
	require.Equal(t, "usdt", effective.GetRgb20WithdrawFee()[0].GetSymbol())
	require.Equal(t, int64(9), effective.GetRgb20WithdrawFee()[0].GetFeeMinUnits())
}
