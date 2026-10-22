package commands

import (
	"strings"
	"testing"

	"github.com/33cn/chain33/types"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// 本文件覆盖新增的治理 / 放弃提现 / 提现加价 / 充值地址命令：命令是否挂上、参数是否在
// 发 RPC 之前构造正确、可重复 flag 与字段掩码的语义是否与链上一致。

func setFlags(t *testing.T, cmd *cobra.Command, values map[string]string) {
	t.Helper()
	for name, value := range values {
		require.NoErrorf(t, cmd.Flags().Set(name, value), "set --%s", name)
	}
}

// captureCreateTx replaces the create-transaction seam and returns a getter for the last
// (actionName, payload) pair. The original is restored when the test finishes.
func captureCreateTx(t *testing.T) func() (string, types.Message) {
	t.Helper()
	old := createTxRPC
	var action string
	var payload types.Message
	createTxRPC = func(_ *cobra.Command, actionName string, req types.Message) {
		action = actionName
		payload = req
	}
	t.Cleanup(func() { createTxRPC = old })
	return func() (string, types.Message) { return action, payload }
}

func Test_newCommandsAreRegistered(t *testing.T) {
	root := Cmd()
	for _, name := range []string{
		"bridge-config-apply",
		"bridge-config-approve",
		"abandon-withdraw",
		"bump-withdraw-fee",
		"register-deposit-script",
		"get-bridge-config",
		"list-abandon-requests",
		"list-bump-withdraw-fee-requests",
		"get-deposit-address",
		"list-deposit-registrations",
	} {
		require.NotNilf(t, findSubCmd(t, root, name), "subcommand %q", name)
	}
}

func Test_buildBridgeConfigApply_FullSnapshot(t *testing.T) {
	cmd := bridgeConfigApplyCMD()
	setFlags(t, cmd, map[string]string{
		"asset-symbol":                        "usdt",
		"limits-enabled":                      "true",
		"max-per-tx":                          "1000",
		"max-per-window-1h":                   "2000",
		"max-per-window-24h":                  "3000",
		"max-mint-per-window":                 "4000",
		"large-deposit-threshold":             "5000",
		"large-deposit-extra-confirmations":   "6",
		"pause-deposit":                       "true",
		"pause-withdraw":                      "false",
		"withdraw-max-fee-rate-sat-per-vbyte": "80",
		"rgb20-withdraw-fee-lock-vbytes":      "600",
		"rgb20-withdraw-fee":                  "USDT=1000000",
	})

	apply, err := buildBridgeConfigApply(cmd)
	require.NoError(t, err)
	require.Equal(t, "usdt", apply.GetAssetSymbol())
	require.True(t, apply.GetLimitsEnabled())
	require.Equal(t, int64(1000), apply.GetMaxPerTx())
	require.Equal(t, int64(2000), apply.GetMaxPerWindow1H())
	require.Equal(t, int64(3000), apply.GetMaxPerWindow24H())
	require.Equal(t, int64(4000), apply.GetMaxMintPerWindow())
	require.Equal(t, int64(5000), apply.GetLargeDepositThreshold())
	require.Equal(t, int64(6), apply.GetLargeDepositExtraConfirmations())
	require.True(t, apply.GetPauseDeposit())
	require.False(t, apply.GetPauseWithdraw())
	require.Equal(t, uint32(80), apply.GetWithdrawMaxFeeRateSatPerVbyte())
	require.Equal(t, uint32(600), apply.GetRgb20WithdrawFeeLockVbytes())
	require.Len(t, apply.GetRgb20WithdrawFee(), 1)
	require.Equal(t, "USDT", apply.GetRgb20WithdrawFee()[0].GetSymbol())
	require.Equal(t, int64(1000000), apply.GetRgb20WithdrawFee()[0].GetFeeMinUnits())
	// 未传掩码 = 全量快照，掩码必须为空。
	require.Empty(t, apply.GetFields())
}

// 只改一部分字段却不给 --fields 时，必须拒绝——否则全量快照会把其它字段静默清零。
func Test_buildBridgeConfigApply_PartialRequiresFields(t *testing.T) {
	cmd := bridgeConfigApplyCMD()
	setFlags(t, cmd, map[string]string{"max-per-tx": "1000"})

	_, err := buildBridgeConfigApply(cmd)
	require.Error(t, err)
	require.Contains(t, err.Error(), "--fields")
	require.Contains(t, err.Error(), "--max-per-tx")

	// 一个字段 flag 都不给时，空掩码就是显式的全量快照（全部归零），允许。
	cmd = bridgeConfigApplyCMD()
	apply, err := buildBridgeConfigApply(cmd)
	require.NoError(t, err)
	require.Empty(t, apply.GetFields())
	require.Zero(t, apply.GetMaxPerTx())
}

func Test_buildBridgeConfigApply_FieldMaskRepeatedAndCommaSeparated(t *testing.T) {
	// StringSlice 同时接受重复 flag 与逗号分隔。
	cmd := bridgeConfigApplyCMD()
	setFlags(t, cmd, map[string]string{
		"max-per-tx":     "1000",
		"pause-deposit":  "true",
		"pause-withdraw": "true",
		"fields":         " maxPerTx , pauseDeposit ",
	})
	setFlags(t, cmd, map[string]string{"fields": "pauseWithdraw"})

	apply, err := buildBridgeConfigApply(cmd)
	require.NoError(t, err)
	require.Equal(t, []string{"maxPerTx", "pauseDeposit", "pauseWithdraw"}, apply.GetFields())
	require.Equal(t, int64(1000), apply.GetMaxPerTx())
	require.True(t, apply.GetPauseDeposit())
	require.True(t, apply.GetPauseWithdraw())
}

func Test_buildBridgeConfigApply_RejectsUnknownField(t *testing.T) {
	cmd := bridgeConfigApplyCMD()
	setFlags(t, cmd, map[string]string{"fields": "maxPerTx,notAField"})

	_, err := buildBridgeConfigApply(cmd)
	require.Error(t, err)
	require.Contains(t, err.Error(), "notAField")
	require.Contains(t, err.Error(), "maxPerTx")
}

func Test_buildBridgeConfigApply_RejectsNegativeLimits(t *testing.T) {
	for _, flag := range []string{
		"max-per-tx",
		"max-per-window-1h",
		"max-per-window-24h",
		"max-mint-per-window",
		"large-deposit-threshold",
		"large-deposit-extra-confirmations",
	} {
		cmd := bridgeConfigApplyCMD()
		setFlags(t, cmd, map[string]string{flag: "-1"})
		_, err := buildBridgeConfigApply(cmd)
		require.Errorf(t, err, "flag %s", flag)
		require.Contains(t, err.Error(), flag)
	}
}

func Test_parseRgb20WithdrawFees(t *testing.T) {
	t.Run("repeated entries", func(t *testing.T) {
		fees, err := parseRgb20WithdrawFees([]string{"USDT=1000000", " usdc = 200 "})
		require.NoError(t, err)
		require.Len(t, fees, 2)
		require.Equal(t, "USDT", fees[0].GetSymbol())
		require.Equal(t, int64(1000000), fees[0].GetFeeMinUnits())
		require.Equal(t, "usdc", fees[1].GetSymbol())
		require.Equal(t, int64(200), fees[1].GetFeeMinUnits())
	})

	t.Run("zero fee is legal", func(t *testing.T) {
		fees, err := parseRgb20WithdrawFees([]string{"USDT=0"})
		require.NoError(t, err)
		require.Equal(t, int64(0), fees[0].GetFeeMinUnits())
	})

	for _, tc := range []struct {
		name string
		in   []string
		want string
	}{
		{"missing amount", []string{"USDT="}, "SYMBOL=AMOUNT"},
		{"missing symbol", []string{"=100"}, "SYMBOL=AMOUNT"},
		{"missing equals", []string{"USDT"}, "SYMBOL=AMOUNT"},
		{"empty entry", []string{" "}, "SYMBOL=AMOUNT"},
		{"decimal amount", []string{"USDT=1.5"}, "integer"},
		{"negative amount", []string{"USDT=-1"}, "negative"},
		{"duplicate symbol", []string{"USDT=1", "usdt=2"}, "duplicate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseRgb20WithdrawFees(tc.in)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

func Test_buildBridgeConfigApprove(t *testing.T) {
	applyHash := strings.Repeat("ab", 32)

	cmd := bridgeConfigApproveCMD()
	setFlags(t, cmd, map[string]string{"apply-id": "0x" + applyHash, "item-id": "item-1"})
	approve, err := buildBridgeConfigApprove(cmd)
	require.NoError(t, err)
	require.Equal(t, "0x"+applyHash, approve.GetApplyConfigId())
	require.Equal(t, "item-1", approve.GetAutonomyItemId())

	cmd = bridgeConfigApproveCMD()
	setFlags(t, cmd, map[string]string{"apply-id": "zz", "item-id": "item-1"})
	_, err = buildBridgeConfigApprove(cmd)
	require.Error(t, err)
	require.Contains(t, err.Error(), "apply-id")

	cmd = bridgeConfigApproveCMD()
	setFlags(t, cmd, map[string]string{"apply-id": applyHash, "item-id": " "})
	_, err = buildBridgeConfigApprove(cmd)
	require.Error(t, err)
	require.Contains(t, err.Error(), "item-id")
}

func Test_buildAbandonWithdraw(t *testing.T) {
	burnHash := strings.Repeat("cd", 32)

	cmd := abandonWithdrawCMD()
	setFlags(t, cmd, map[string]string{
		"burn-tx-hash": "0x" + burnHash,
		"owner":        "1Owner",
		"reason":       "stuck",
	})
	abandon, err := buildAbandonWithdraw(cmd)
	require.NoError(t, err)
	require.Len(t, abandon.GetBurnTxHash(), 32)
	require.Equal(t, "1Owner", abandon.GetOwner())
	require.Equal(t, "stuck", abandon.GetReason())

	// owner 是退回对象，执行器要求与台账逐字相等；CLI 不能省略。
	cmd = abandonWithdrawCMD()
	setFlags(t, cmd, map[string]string{"burn-tx-hash": burnHash})
	_, err = buildAbandonWithdraw(cmd)
	require.Error(t, err)
	require.Contains(t, err.Error(), "owner")

	cmd = abandonWithdrawCMD()
	setFlags(t, cmd, map[string]string{"burn-tx-hash": "xyz", "owner": "1Owner"})
	_, err = buildAbandonWithdraw(cmd)
	require.Error(t, err)
	require.Contains(t, err.Error(), "burn-tx-hash")
}

func Test_buildBumpWithdrawFee(t *testing.T) {
	txHash := strings.Repeat("ef", 32)

	cmd := bumpWithdrawFeeCMD()
	setFlags(t, cmd, map[string]string{"withdrawal-tx-hash": "0x" + txHash, "fee-rate": "12"})
	bump, err := buildBumpWithdrawFee(cmd)
	require.NoError(t, err)
	require.Len(t, bump.GetWithdrawalTxHash(), 32)
	require.Equal(t, int64(12), bump.GetFeeRate())

	for _, feeRate := range []string{"0", "-1"} {
		cmd = bumpWithdrawFeeCMD()
		setFlags(t, cmd, map[string]string{"withdrawal-tx-hash": txHash, "fee-rate": feeRate})
		_, err = buildBumpWithdrawFee(cmd)
		require.Errorf(t, err, "fee-rate=%s", feeRate)
		require.Contains(t, err.Error(), "fee-rate")
	}
}

// 每个交易命令都必须把构造好的 payload 交给 CreateTransaction，action 名与 proto oneof
// 字段一致（名字写错链上会返回 unknown action）。
func Test_newTxCommandsDispatchExpectedAction(t *testing.T) {
	txHash := "0x" + strings.Repeat("11", 32)
	cases := []struct {
		name       string
		cmd        func() *cobra.Command
		flags      map[string]string
		wantAction string
		check      func(t *testing.T, payload types.Message)
	}{
		{
			name:       "bridge-config-apply",
			cmd:        bridgeConfigApplyCMD,
			flags:      map[string]string{"pause-deposit": "true", "fields": "pauseDeposit"},
			wantAction: rtypes.NameBridgeConfigApplyAction,
			check: func(t *testing.T, payload types.Message) {
				apply, ok := payload.(*rtypes.BridgeConfigApply)
				require.True(t, ok)
				require.True(t, apply.GetPauseDeposit())
				require.Equal(t, []string{"pauseDeposit"}, apply.GetFields())
			},
		},
		{
			name:       "bridge-config-approve",
			cmd:        bridgeConfigApproveCMD,
			flags:      map[string]string{"apply-id": txHash, "item-id": "item-1"},
			wantAction: rtypes.NameBridgeConfigApproveAction,
			check: func(t *testing.T, payload types.Message) {
				approve, ok := payload.(*rtypes.BridgeConfigApprove)
				require.True(t, ok)
				require.Equal(t, "item-1", approve.GetAutonomyItemId())
			},
		},
		{
			name:       "abandon-withdraw",
			cmd:        abandonWithdrawCMD,
			flags:      map[string]string{"burn-tx-hash": txHash, "owner": "1Owner"},
			wantAction: rtypes.NameAbandonWithdrawAction,
			check: func(t *testing.T, payload types.Message) {
				abandon, ok := payload.(*rtypes.AbandonWithdraw)
				require.True(t, ok)
				require.Equal(t, "1Owner", abandon.GetOwner())
			},
		},
		{
			name:       "bump-withdraw-fee",
			cmd:        bumpWithdrawFeeCMD,
			flags:      map[string]string{"withdrawal-tx-hash": txHash, "fee-rate": "9"},
			wantAction: rtypes.NameBumpWithdrawFeeAction,
			check: func(t *testing.T, payload types.Message) {
				bump, ok := payload.(*rtypes.BumpWithdrawFee)
				require.True(t, ok)
				require.Equal(t, int64(9), bump.GetFeeRate())
			},
		},
		{
			name:       "register-deposit-script",
			cmd:        registerDepositScriptCMD,
			wantAction: rtypes.NameRegisterDepositScriptAction,
			check: func(t *testing.T, payload types.Message) {
				_, ok := payload.(*rtypes.RegisterDepositScript)
				require.True(t, ok)
				require.Empty(t, payload.String())
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			captured := captureCreateTx(t)
			cmd := tc.cmd()
			setFlags(t, cmd, tc.flags)
			cmd.Run(cmd, nil)

			gotAction, gotPayload := captured()
			require.Equal(t, tc.wantAction, gotAction)
			require.NotNil(t, gotPayload)
			tc.check(t, gotPayload)
		})
	}
}

func Test_validateListPendingReq(t *testing.T) {
	req, err := validateListPendingReq(100, 2, 200, 20)
	require.NoError(t, err)
	require.Equal(t, int64(100), req.GetStartHeight())
	require.Equal(t, int64(2), req.GetStartIndex())
	require.Equal(t, int64(200), req.GetEndHeight())
	require.Equal(t, int32(20), req.GetCount())

	// endHeight = 0 表示"到当前高度"。
	req, err = validateListPendingReq(0, 0, 0, 1)
	require.NoError(t, err)
	require.Equal(t, int64(0), req.GetEndHeight())

	_, err = validateListPendingReq(0, 0, 0, 0)
	require.Error(t, err)
	require.Contains(t, err.Error(), "count")

	_, err = validateListPendingReq(0, 0, 0, maxListCount+1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "count")

	_, err = validateListPendingReq(10, 0, 9, 1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "endHeight")
}

// 查询命令的 flag 默认值与必填约束（请求体本身由 sendQueryRPC 构造，这里只钉住 CLI 契约）。
func Test_queryCommandFlags(t *testing.T) {
	get := findSubCmd(t, Cmd(), "get-bridge-config")
	symbol, err := get.Flags().GetString("asset-symbol")
	require.NoError(t, err)
	require.Empty(t, symbol)

	deposit := findSubCmd(t, Cmd(), "get-deposit-address")
	userID, err := deposit.Flags().GetString("user-id")
	require.NoError(t, err)
	require.Empty(t, userID)
	require.NoError(t, deposit.MarkFlagRequired("user-id"))

	for _, name := range []string{
		"list-abandon-requests",
		"list-bump-withdraw-fee-requests",
		"list-deposit-registrations",
	} {
		cmd := findSubCmd(t, Cmd(), name)
		count, err := cmd.Flags().GetUint32("count")
		require.NoErrorf(t, err, "%s count", name)
		require.Equalf(t, uint32(20), count, "%s default count", name)
		endHeight, err := cmd.Flags().GetUint64("endHeight")
		require.NoErrorf(t, err, "%s endHeight", name)
		require.Equalf(t, uint64(0), endHeight, "%s default endHeight", name)
	}
}
