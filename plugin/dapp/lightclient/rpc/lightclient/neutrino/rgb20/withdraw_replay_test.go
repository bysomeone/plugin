package rgb20

import (
	"context"
	"errors"
	"testing"

	pb "github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20/pb"
	"github.com/stretchr/testify/require"
)

// 本文件覆盖 E9-A：**首次广播失败后重试必须重放同一笔提现**（同一组 seal ⇒ 侧车发回同一份
// 未签 PSBT ⇒ 同一 txid），而不是按当前账本重选 seal 构建第二笔付款。
//
// 协调者侧的三件事在这里钉住：
//   1. 首次构建不下发 input_seals（没有 sticky 记录 ⇒ 侧车走常规选 seal 路径）；
//   2. 广播失败后 sticky 记录已落盘，重试把它作为 input_seals 下发（触发侧车重放）；
//   3. 重试仍比对「本份 PSBT 的 seal 输入集」与记录（persistStickySeal 绝不覆盖）。
// 外加与 BTC 侧同构的状态门：state 非空且 sticky 为空 ⇒ 停下（不可恢复），绝不重新选 seal。

// withdrawReq 构造一笔 RGB20 提现请求（测试共用）。
func withdrawReq(hash string) *WithdrawRequest {
	return &WithdrawRequest{
		Chain33TxHash:    []byte(hash),
		Amount:           500000,
		FeeRate:          1,
		RecipientInvoice: "rgb:invoice",
		AssetSymbol:      "RGB20_USDT",
		TxBlockHeight:    100,
	}
}

// armWithdrawBuild 让假侧车按给定 seal 返回一份可校验的提现构建。
func armWithdrawBuild(t *testing.T, mock *MockSidecar, seal string, txid string) {
	t.Helper()
	psbtBytes := buildAnchoredWithdrawPSBT(t, seal)
	mock.BuildResp = &pb.BuildWithdrawalResponse{
		Psbt:        psbtBytes,
		Consignment: []byte("consignment-" + seal),
	}
	mock.ValidateResp = []*pb.ConsignmentValidation{
		anchoredConsignment(t, psbtBytes, 200000, []string{seal}, anchoredSeal{vout: 1, amount: 500000}),
	}
	mock.FinalizeResp = &pb.FinalizeWithdrawalResponse{Txid: txid, ChangeSealOutpoint: stickySealChange}
}

// Test_Withdraw_FailedBroadcastIsRetriedAsReplay 事故路径（E9 的触发形态）：第一次构建 + 签名 +
// finalize 都成功，**广播失败**（连接被重置）。此时 sticky 记录已经落盘、链上账本已推进。
// 重试必须：
//   - 把记录里的 seal 作为 input_seals 下发给侧车（侧车据此重放同一份 PSBT ⇒ 同一 txid）；
//   - 拿到同一个 txid 后正常广播（配合 neutrino.BroadcastTx 的幂等判定「节点已知即成功」）；
//   - 记录不被改写（同一组 seal ⇒ 幂等重放，不是失败）。
func Test_Withdraw_FailedBroadcastIsRetriedAsReplay(t *testing.T) {
	req := withdrawReq("chain33-withdraw-hash")

	mock := NewMockSidecar()
	bridge := &fakeBridge{broadcastErr: errors.New("connection reset by peer")}
	adapter, cleanup := newTestAdapter(t, mock, bridge)
	defer cleanup()

	armWithdrawBuild(t, mock, stickySealA, "tx-a")

	// 第一次：构建/签名/finalize 都过，广播失败 ⇒ 返回错误（可重试），记录已落盘。
	_, err := adapter.Withdraw(context.Background(), req)
	require.Error(t, err)
	require.Contains(t, err.Error(), "broadcast")
	require.Equal(t, stickySealA, adapter.GetStickySeal(req.Chain33TxHash), "首次构建后必须已绑定 seal 集合")
	_, unrecoverable := IsUnrecoverableWithdraw(err)
	require.False(t, unrecoverable, "广播失败是可重试的，不能判为不可恢复")

	firstReqs := mock.buildRequests()
	require.Len(t, firstReqs, 1)
	require.Empty(t, firstReqs[0].InputSeals, "首次构建没有 sticky 记录 ⇒ 不下发 input_seals")

	// 第二次：网络恢复。重试必须下发 input_seals，让侧车重放同一笔交易。
	bridge.setBroadcastErr(nil)
	mock.ValidateResp = []*pb.ConsignmentValidation{
		anchoredConsignment(t, mock.BuildResp.Psbt, 200000, []string{stickySealA},
			anchoredSeal{vout: 1, amount: 500000}),
	}
	res, err := adapter.Withdraw(context.Background(), req)
	require.NoError(t, err, "重试必须成功（广播失败已恢复）")

	builds := mock.buildRequests()
	require.Len(t, builds, 2)
	require.Equal(t, []string{stickySealA}, builds[1].InputSeals,
		"重试必须把 sticky seal 作为 input_seals 下发（侧车据此重放同一份 PSBT）")
	require.Equal(t, "tx-a", res.Txid, "重放必须得到同一个 txid（幂等重放，不是第二笔付款）")
	require.Equal(t, stickySealA, adapter.GetStickySeal(req.Chain33TxHash), "记录不因重试而改变")
	require.Equal(t, 2, bridge.broadcastCallCount(), "两次尝试各广播一次")
}

// Test_Withdraw_StateWithoutStickySealIsStopped 与 BTC 侧同构的状态门：本地状态非空（本笔已经
// 走到过广播）而 sticky 记录为空（记录丢失/存储被清）⇒ 绝不能再按当前账本选 seal 构建
// ——那正是 E9 的双付形态。判为不可恢复（重试改变不了"记录不在"这件事）。
func Test_Withdraw_StateWithoutStickySealIsStopped(t *testing.T) {
	req := withdrawReq("chain33-withdraw-hash")

	mock := NewMockSidecar()
	bridge := &fakeBridge{withdrawState: []byte("broadcasted")}
	adapter, cleanup := newTestAdapter(t, mock, bridge)
	defer cleanup()

	armWithdrawBuild(t, mock, stickySealA, "tx-a")

	_, err := adapter.Withdraw(context.Background(), req)
	require.Error(t, err)
	class, unrecoverable := IsUnrecoverableWithdraw(err)
	require.True(t, unrecoverable, "err=%v", err)
	require.Equal(t, unrecoverableClassStickySealMismatch, class)
	require.Empty(t, mock.buildRequests(), "状态门必须在构建之前拦住（不能重新选 seal）")
	require.Equal(t, 0, bridge.broadcastCallCount())
}

// Test_DecodeStickySeals encodeStickySeals 的逆：空记录 → nil（不下发），正常记录 → outpoint 列表。
func Test_DecodeStickySeals(t *testing.T) {
	require.Nil(t, decodeStickySeals(""))
	require.Equal(t, []string{stickySealA}, decodeStickySeals(stickySealA))
	// encode → decode 往返：编码是「排序 + 逗号分隔」，解码必须逐项还原（含多项）。
	encoded := encodeStickySeals([]string{stickySealB, stickySealA, stickySealA})
	require.Equal(t, []string{stickySealA, stickySealB}, decodeStickySeals(encoded))
}
