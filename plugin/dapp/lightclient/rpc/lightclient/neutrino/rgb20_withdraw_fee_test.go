package neutrino

import (
	"bytes"
	"strconv"
	"testing"

	"github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20"
	rgb20pb "github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20/pb"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/stretchr/testify/require"
)

// 本文件覆盖**签名节点侧**的提现手续费核对（`rgb20.contracts[].withdrawFee`）：
// 签名节点用**本节点自己**的配置算出应发额（链上 pending − 手续费），并核对本笔转移实际发给用户的
// 资产是否精确等于它 —— 不采信协调者下发的任何声称值（协调者恶意/有 bug 时，签名节点是最后一道）。
//
// 为什么这一侧必须单独测：端到端（CI 的 scenario_rgb20_withdraw_fee_mismatch）里**走到不了这里** ——
// 协调者自己先跑同一套核对并把提现拒掉，签名通知根本不会发出。所以"签名节点会不会放行一笔没扣
// 手续费的提现"只能在这里钉住。
func Test_validateRgb20WithdrawSign_FeeCheck(t *testing.T) {
	const (
		fee      = int64(1000)
		amount   = int64(500000)
		expected = amount - fee
	)
	pending := &rtypes.PendingTx{
		Amount:        amount,
		FeeRate:       1,
		TxBlockHeight: 100,
		TargetAddress: "rgb:invoice",
		// 手续费按**链上资产符号**查（PendingTx.AssetSymbol 由 Exec_Withdraw 写入，注册表也按它索引）。
		AssetSymbol: "RGB20_USDT",
	}
	claim := &rgb20.WithdrawSignPayload{
		Chain33TxHash:    []byte("chain33-withdraw-hash"),
		Amount:           amount,
		FeeRate:          1,
		TxBlockHeight:    100,
		RecipientInvoice: "rgb:invoice",
	}
	psbtBytes := buildTestRgb20WithdrawPSBT(t, testRgb20SealOutpoint)
	consignment := []byte("consignment")

	// newSigner 按本节点的配置（fee=0 表示这个节点没配手续费）构造签名节点服务。
	newSigner := func(t *testing.T, fee int64) (*tssService, *rgb20.MockSidecar) {
		t.Helper()
		ts, mock, adapter := newTestSignerService(t)
		if fee > 0 {
			adapter.Registry().Register(&rgb20.Contract{
				Symbol: "RGB20_USDT", MinDeposit: 100, MinWithdraw: 100, WithdrawFee: fee,
			})
		}
		return ts, mock
	}

	t.Run("refuses a payout that does not deduct the fee", func(t *testing.T) {
		ts, mock := newSigner(t, fee)
		// 发票按完整提现额开：实际发给用户 amount，而配置要求恰好 expected。
		mock.ValidateResp = []*rgb20pb.ConsignmentValidation{
			rgb20ConsignmentWithChange(t, psbtBytes, testRgb20SealOutpoint, amount, 0),
		}
		_, err := ts.validateRgb20WithdrawSign(claim, psbtBytes, consignment, pending)
		require.Error(t, err)
		require.Contains(t, err.Error(), "withdraw payout mismatch")
		require.Contains(t, err.Error(), "requires exactly "+itoa(expected))
	})

	t.Run("accepts exactly pending minus the fee", func(t *testing.T) {
		ts, mock := newSigner(t, fee)
		mock.ValidateResp = []*rgb20pb.ConsignmentValidation{
			rgb20ConsignmentWithChange(t, psbtBytes, testRgb20SealOutpoint, expected, fee),
		}
		seals, err := ts.validateRgb20WithdrawSign(claim, psbtBytes, consignment, pending)
		require.NoError(t, err)
		require.Equal(t, []string{testRgb20SealOutpoint}, seals)
	})

	t.Run("node without a configured fee: the amount check is skipped", func(t *testing.T) {
		ts, mock := newSigner(t, 0)
		// 没配手续费 ⇒ 退回"只判上界"的旧行为：发给用户 ≤ pending 即放行（这里正是等于）。
		mock.ValidateResp = []*rgb20pb.ConsignmentValidation{
			rgb20ConsignmentWithChange(t, psbtBytes, testRgb20SealOutpoint, amount, 0),
		}
		_, err := ts.validateRgb20WithdrawSign(claim, psbtBytes, consignment, pending)
		require.NoError(t, err)
	})

	t.Run("pending does not even cover the fee: refused", func(t *testing.T) {
		ts, _ := newSigner(t, fee)
		// 一笔"提现额连手续费都不够"的 pending（proto 消息带锁，不能按值拷贝，另起一份）。
		small := &rtypes.PendingTx{
			Amount:        fee,
			FeeRate:       1,
			TxBlockHeight: 100,
			TargetAddress: "rgb:invoice",
			AssetSymbol:   "RGB20_USDT",
		}
		smallClaim := &rgb20.WithdrawSignPayload{
			Chain33TxHash:    []byte("chain33-withdraw-hash"),
			Amount:           fee,
			FeeRate:          1,
			TxBlockHeight:    100,
			RecipientInvoice: "rgb:invoice",
		}
		_, err := ts.validateRgb20WithdrawSign(smallClaim, psbtBytes, consignment, small)
		require.Error(t, err)
		require.Contains(t, err.Error(), "does not cover the configured withdrawal fee")
	})
}

// rgb20ConsignmentWithChange 在 rgb20Consignment 之上补一个"找零回桥"的 opened seal
// （vout2 = TSS 脚本），使可支配额 = 应发额 + 找零。真实提现就是这个形状（RGB 状态守恒：
// 被花 seal 的面额 = 发给用户的 + 找零回桥的），少了它可支配额会小于 pending 而被另一条判据
// （"withdraw amount exceeds sealed balance"）先拒掉，测的就不是手续费这条了。
func rgb20ConsignmentWithChange(t *testing.T, psbtBytes []byte, sealOutpoint string,
	payout, change int64) *rgb20pb.ConsignmentValidation {
	t.Helper()
	v := rgb20Consignment(t, psbtBytes, sealOutpoint, payout)
	if change <= 0 {
		return v
	}
	p, err := psbt.NewFromRawBytes(bytes.NewReader(psbtBytes), false)
	require.NoError(t, err)
	v.OpenedSeals = append(v.OpenedSeals, &rgb20pb.OpenedSeal{
		Outpoint: p.UnsignedTx.TxHash().String() + ":2",
		Amount:   change,
	})
	return v
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
