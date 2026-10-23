package rgb20

import (
	"context"
	"errors"
	"testing"

	pb "github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20/pb"
	"github.com/stretchr/testify/require"
)

// 本文件覆盖「提现手续费」（链上 rgbx 治理配置，按资产 symbol）：桥按 `链上 pending − 手续费`
// 精确核对**实际发给用户**的资产金额，不等则拒绝（不签名、不广播）。
//
// 经济动机见 Adapter.WithdrawFeeFor：RGB20 提现本身就是一笔普通 BTC 交易，矿工费只能用 BTC 付，
// 而它出自桥的主池 —— 不校验就等于桥替用户垫矿工费。

const (
	testWithdrawFee     = int64(1000)
	testWithdrawPending = int64(500000)
)

// Test_ExpectedWithdrawPayout 手续费 → 应发金额的换算与边界。
func Test_ExpectedWithdrawPayout(t *testing.T) {
	adapter, err := NewAdapter(Config{
		SidecarAddr: "/tmp/nonexistent.sock",
		Contracts: []Contract{
			{Symbol: "RGB20_USDT"},
			{Symbol: "RGB20_NOFEE"},
		},
	}, newMemStore())
	require.NoError(t, err)
	// 手续费来自链上治理配置（由 neutrino 主包注入）；这里直接注入即可测换算。
	adapter.SetWithdrawFeeSource(func(symbol string) int64 {
		if symbol == "RGB20_USDT" {
			return testWithdrawFee
		}
		return 0
	})

	t.Run("fee configured: payout is pending minus fee", func(t *testing.T) {
		payout, configured, err := adapter.ExpectedWithdrawPayout("RGB20_USDT", testWithdrawPending)
		require.NoError(t, err)
		require.True(t, configured)
		require.Equal(t, testWithdrawPending-testWithdrawFee, payout)
	})

	t.Run("no fee configured: no check (legacy behaviour)", func(t *testing.T) {
		payout, configured, err := adapter.ExpectedWithdrawPayout("RGB20_NOFEE", testWithdrawPending)
		require.NoError(t, err)
		require.False(t, configured, "未配置手续费 ⇒ 不做金额比对，保持旧的只判上界行为")
		require.Zero(t, payout)
	})

	t.Run("symbol not registered: no check", func(t *testing.T) {
		_, configured, err := adapter.ExpectedWithdrawPayout("RGB20_UNKNOWN", testWithdrawPending)
		require.NoError(t, err)
		require.False(t, configured)
	})

	t.Run("pending does not cover the fee: reject", func(t *testing.T) {
		for _, pending := range []int64{testWithdrawFee, testWithdrawFee - 1, 0, -1} {
			_, configured, err := adapter.ExpectedWithdrawPayout("RGB20_USDT", pending)
			require.Error(t, err, "pending=%d", pending)
			require.False(t, configured)
			require.Contains(t, err.Error(), "does not cover the configured withdrawal fee")
		}
	})
}

// Test_WithdrawValidate_PayoutMustEqualPendingMinusFee 签名节点侧的精确比对：多给（桥垫手续费）
// 与少给（超收用户）都必须拒绝，只有精确等于才放行。
func Test_WithdrawValidate_PayoutMustEqualPendingMinusFee(t *testing.T) {
	const change = int64(200000)
	expected := testWithdrawPending - testWithdrawFee

	cases := []struct {
		name    string
		payout  int64
		wantErr bool
	}{
		{name: "exact: pending minus fee", payout: expected},
		{name: "too high: the invoice asks for the full pending (the bridge pays the fee)", payout: testWithdrawPending, wantErr: true},
		{name: "too low by one unit", payout: expected - 1, wantErr: true},
		{name: "too high by one unit", payout: expected + 1, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			psbtBytes := buildAnchoredWithdrawPSBT(t, coverageSealOutpoint)
			mock := NewMockSidecar()
			mock.ValidateResp = []*pb.ConsignmentValidation{
				anchoredConsignment(t, psbtBytes, change, []string{coverageSealOutpoint},
					anchoredSeal{vout: 1, amount: tc.payout}, anchoredSeal{vout: 2, amount: change}),
			}
			adapter, cleanup := newTestAdapter(t, mock, &fakeBridge{})
			defer cleanup()

			err := adapter.ValidateWithdrawPsbt(&ValidateWithdrawRequest{
				Psbt:            psbtBytes,
				Consignment:     []byte("consignment"),
				ExpectedAmount:  testWithdrawPending,
				ExpectedPayout:  expected,
				MinSyncedHeight: 100,
				FeeRate:         1,
			})
			if !tc.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			var mismatch *WithdrawPayoutMismatchError
			require.True(t, errors.As(err, &mismatch), "err=%v", err)
			// 错误信息必须说清"期望多少、实际多少、差在哪"。
			require.Equal(t, tc.payout, mismatch.Payout)
			require.Equal(t, expected, mismatch.Expected)
			require.Equal(t, testWithdrawPending, mismatch.Pending)
			require.Equal(t, testWithdrawFee, mismatch.Fee)
			msg := err.Error()
			require.Contains(t, msg, "withdraw payout mismatch")
			for _, want := range []string{
				"pays the user", "configured withdrawal fee", "requires exactly", "chain33 pending", "off by",
			} {
				require.Contains(t, msg, want)
			}
		})
	}
}

// Test_Withdraw_RejectsInvoiceWithoutTheFeeDeducted 端到端（协调者侧）：发票按完整提现额开
// （= 没扣手续费）时，提现必须在**构造之后、签名与广播之前**停下。
//
// 这条正是要防的形态：侧车不知道手续费，发票写多少它就发多少，所以"桥替用户垫矿工费"只能由
// 桥侧这条判据拦住。重复一遍副作用：链上那笔 burn 在用户提交提现时就已经把金额转进了锁定地址
// （Exec_Withdraw），被拒后它会一直躺在那里，直到「解锁」路径落地（独立任务，见
// WithdrawPayoutMismatchError 的注释与 WITHDRAW_LOCK_MODEL.md）。
func Test_Withdraw_RejectsInvoiceWithoutTheFeeDeducted(t *testing.T) {
	req := &WithdrawRequest{
		Chain33TxHash:    []byte("chain33-withdraw-hash"),
		Amount:           testWithdrawPending,
		FeeRate:          1,
		RecipientInvoice: "rgb:invoice-no-fee",
		AssetSymbol:      "RGB20_USDT",
		TxBlockHeight:    100,
	}

	mock := NewMockSidecar()
	bridge := &fakeBridge{}
	adapter, cleanup := newTestAdapter(t, mock, bridge)
	defer cleanup()
	// 配好手续费的部署（newTestAdapter 的 fixture 没配；手续费由链上治理配置注入）。
	adapter.SetWithdrawFeeSource(func(symbol string) int64 {
		if symbol == "RGB20_USDT" {
			return testWithdrawFee
		}
		return 0
	})

	psbtBytes := buildAnchoredWithdrawPSBT(t, stickySealA)
	arm := func(payout int64) {
		mock.BuildResp = &pb.BuildWithdrawalResponse{
			Psbt:        psbtBytes,
			Consignment: []byte("consignment-" + stickySealA),
		}
		mock.ValidateResp = []*pb.ConsignmentValidation{
			anchoredConsignment(t, psbtBytes, 200000, []string{stickySealA},
				anchoredSeal{vout: 1, amount: payout}, anchoredSeal{vout: 2, amount: 200000}),
		}
		mock.FinalizeResp = &pb.FinalizeWithdrawalResponse{Txid: "tx-a", ChangeSealOutpoint: stickySealChange}
	}

	// 负例：发票 = 完整提现额（手续费没扣）。
	arm(testWithdrawPending)
	_, err := adapter.Withdraw(context.Background(), req)
	require.Error(t, err)
	var mismatch *WithdrawPayoutMismatchError
	require.True(t, errors.As(err, &mismatch), "err=%v", err)
	require.Equal(t, testWithdrawPending, mismatch.Payout)
	require.Equal(t, testWithdrawPending-testWithdrawFee, mismatch.Expected)

	// 拒绝点必须早于签名与广播：没有签名请求、没有广播、也没有落下 sticky 绑定。
	require.Nil(t, bridge.signRequest(), "被拒的提现不得下发签名请求")
	require.Equal(t, 0, bridge.broadcastCallCount(), "被拒的提现不得广播")
	require.Empty(t, adapter.GetStickySeal(req.Chain33TxHash), "被拒的提现不得落下 sticky 绑定")
	require.NotEmpty(t, mock.buildRequests(), "构建本身可以发生过：判据在构建之后（侧车不解析发票金额）")

	// 正例：同一笔 pending，发票改成 pending − 手续费 ⇒ 放行，并恢复正常签名/广播。
	arm(testWithdrawPending - testWithdrawFee)
	res, err := adapter.Withdraw(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, "tx-a", res.Txid)
	require.NotNil(t, bridge.signRequest(), "金额对的提现必须正常进入签名")
	require.Equal(t, 1, bridge.broadcastCallCount())
	require.Equal(t, stickySealA, adapter.GetStickySeal(req.Chain33TxHash))
}
