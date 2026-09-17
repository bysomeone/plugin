package rgb20

import (
	"errors"
	"math"
	"testing"

	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/stretchr/testify/require"
)

/*
 * 充值正常路径的配套修复的测试：
 *
 *  ① 本地深度门控：链上可见深度不够时**不签、不提交**（重试只等，不空跑签名轮次）；
 *  ② N 来自链上（桥接查询），不是中继自己镜像的配置；
 *  ③ 签名产物落盘：重试优先重发，签名轮次不必重跑；
 *  ④ 产物丢失（rgb20-deposit-sig 无记录）时可自愈：重新签名一轮并提交成功。
 *
 * 注意 N > 1 是这些用例的重点：CI 里 [exec.sub.rgbx].minBtcConfirmations=1 会让"提交那一刻
 * 链上可见深度是 0"这件事被掩盖（N=1 时深度 0 也够），本地单测必须显式覆盖 N > 1。
 */

const (
	testProofHeight = uint64(100) // fakeBridge 默认 SPV 证明的付款交易高度 H
	testHeaderConfs = uint64(6)   // cfg.HeaderRelayConfirmations（= blockConfirmations B）
	testRgbxConfs   = uint64(6)   // 链上 minBtcConfirmations N（> 1：CI 的 N=1 不算）
	// testRequiredBest = H + B + N - 1：本地 best 达到它才允许提交（此时链上可见深度 = N）。
	testRequiredBest = testProofHeight + testHeaderConfs + testRgbxConfs - 1
)

const testDepositTxid = "1111111111111111111111111111111111111111111111111111111111111111"

// newDepositTestAdapter 只构造适配器 + 注入桥接，不连侧车、不起后台协程（这些用例直接驱动
// submitDeposit，不需要侧车参与）。
func newDepositTestAdapter(t *testing.T, bridge Chain33Bridge, store KVStore) *Adapter {
	t.Helper()
	if store == nil {
		store = NewMemStore()
	}
	adapter, err := NewAdapter(Config{
		SidecarAddr: "test-no-sidecar", Contracts: []Contract{
			{Symbol: "RGB20_USDT", Precision: 6, MinDeposit: 100, MinWithdraw: 100},
		},
		HeaderRelayConfirmations: uint32(testHeaderConfs),
	}, store)
	require.NoError(t, err)
	adapter.SetBridge(bridge)
	return adapter
}

// seedSettledReceive 造一条"已结算、等待铸造"的充值记录（含 consignment 与 pending-mint seal）。
func seedSettledReceive(t *testing.T, a *Adapter, receiveID, txid string, amount int64) *ReceiveRecord {
	t.Helper()
	seal := FormatOutpoint(txid, 0)
	rec := &ReceiveRecord{
		ReceiveID:   receiveID,
		RequestID:   "req-" + receiveID,
		AssetSymbol: "RGB20_USDT",
		Chain33Addr: "1JnYYeefMhWsXvZyvjCKPZK7eYQdFpzDsk",
		Amount:      amount,
		Status:      ReceiveStatusSettled,
		Consignment: []byte("consignment-bytes"),
		Txid:        txid,
		Vout:        0,
		Seal:        seal,
	}
	require.NoError(t, a.receives.Put(rec))
	require.NoError(t, a.seals.Add(&Seal{
		Outpoint: seal, AssetID: "rgb:asset", AssetSymbol: rec.AssetSymbol,
		Amount: amount, Status: SealStatusPendingMint,
	}))
	return rec
}

// TestSubmitDeposit_BlocksUntilChainVisibleDepthReached ①：本地 best 还差一个高度时**不提交也不签名**；
// 达到 H + B + N - 1 后提交**一次**即成。
//
// 场景来源：链上要求 canonical tip >= H + N - 1，而中继提交头链只到 best - B；中继通知充值的阈值
// 却是 best >= H + B - 1 —— 两者相减 = 提交那一刻链上可见深度是 0，首笔提交注定被拒（B8）。
func TestSubmitDeposit_BlocksUntilChainVisibleDepthReached(t *testing.T) {
	bridge := &fakeBridge{}
	adapter := newDepositTestAdapter(t, bridge, nil)
	rec := seedSettledReceive(t, adapter, "recv-depth", testDepositTxid, 1000)

	// 差一个高度：不签、不提交，返回"等深度"。
	bridge.setDepth(testRequiredBest-1, testRgbxConfs)
	err := adapter.submitDeposit(rec)
	require.Error(t, err)
	require.True(t, errors.Is(err, errDepositDepthPending), "应返回等深度错误, got %v", err)
	require.Equal(t, 0, bridge.signCallCount(), "深度不够时不得驱动签名轮次")
	require.Equal(t, 0, bridge.submitCallCount(), "深度不够时不得提交")
	require.Equal(t, ReceiveStatusSettled, mustReceiveStatus(t, adapter, rec.ReceiveID))

	// 再等一轮仍是同样结果（重试廉价、且不碰签名轮次）
	bridge.setDepth(testRequiredBest-1, testRgbxConfs)
	require.Error(t, adapter.submitDeposit(rec))
	require.Equal(t, 0, bridge.signCallCount())
	require.Equal(t, 0, bridge.submitCallCount())

	// best 长够：一次提交即成，签名轮次只驱动一次。
	bridge.setDepth(testRequiredBest, testRgbxConfs)
	require.NoError(t, adapter.submitDeposit(rec))
	require.Equal(t, 1, bridge.signCallCount(), "够深时应恰好驱动一次签名轮次")
	require.Equal(t, 1, bridge.submitCallCount(), "够深时应恰好提交一次")
	require.Equal(t, ReceiveStatusMinted, mustReceiveStatus(t, adapter, rec.ReceiveID))
	require.Equal(t, 0, adapter.depositSigs.Len(), "铸造成功后应清掉落盘产物")
}

// TestSubmitDeposit_GateUsesOnChainMinConfs ②：门控里的 N 取自链上（桥接查询），不是中继本地常量。
// 同一个 best，N=3 够、N=6 不够 —— 若 N 写死（或镜像错），这个用例会红。
func TestSubmitDeposit_GateUsesOnChainMinConfs(t *testing.T) {
	best := testProofHeight + testHeaderConfs + 3 - 1 // 刚好够 N=3（此时链上可见深度 3）

	bridge := &fakeBridge{}
	adapter := newDepositTestAdapter(t, bridge, nil)
	rec := seedSettledReceive(t, adapter, "recv-n6", testDepositTxid, 1000)

	// 链上 N=6：同一个 best 不够深 → 不提交。
	bridge.setDepth(best, 6)
	err := adapter.submitDeposit(rec)
	require.True(t, errors.Is(err, errDepositDepthPending), "N=6 时应判定深度不足, got %v", err)
	require.Equal(t, 0, bridge.submitCallCount())

	// 链上 N=3：同样的 best 够深 → 提交。
	bridge.setDepth(best, 3)
	require.NoError(t, adapter.submitDeposit(rec))
	require.Equal(t, 1, bridge.submitCallCount())
}

// TestSubmitDeposit_FailsClosedWhenGateInputsUnavailable ①的 fail-closed 面：算不出深度就不提交。
// （best 取不到 / 链上 N 查不到 —— 后者在旧版主链执行器上就是"没有这个查询"。）
func TestSubmitDeposit_FailsClosedWhenGateInputsUnavailable(t *testing.T) {
	t.Run("best height unavailable", func(t *testing.T) {
		bridge := &fakeBridge{}
		adapter := newDepositTestAdapter(t, bridge, nil)
		rec := seedSettledReceive(t, adapter, "recv-nobest", testDepositTxid, 1000)

		bridge.setDepthErrs(errors.New("neutrino best block not ready"), nil)
		err := adapter.submitDeposit(rec)
		require.Error(t, err)
		require.True(t, errors.Is(err, errDepositGateUnavailable), "应归类为门控算不出来, got %v", err)
		require.True(t, isDepositRetryNote(err), "门控算不出来应被调用方按已知状态处理（不刷屏）")
		require.Equal(t, 0, bridge.signCallCount())
		require.Equal(t, 0, bridge.submitCallCount())
	})

	t.Run("on-chain min confirmations unavailable", func(t *testing.T) {
		bridge := &fakeBridge{}
		adapter := newDepositTestAdapter(t, bridge, nil)
		rec := seedSettledReceive(t, adapter, "recv-noconf", testDepositTxid, 1000)

		// 旧版主链执行器没有这个查询时，中继拿到的就是这条错误。
		bridge.setDepthErrs(nil, errors.New("query GetRgbxMinBtcConfirmations: ErrActionNotSupport"))
		err := adapter.submitDeposit(rec)
		require.Error(t, err)
		require.True(t, errors.Is(err, errDepositGateUnavailable), "应归类为门控算不出来, got %v", err)
		require.True(t, isDepositRetryNote(err))
		require.Equal(t, 0, bridge.signCallCount())
		require.Equal(t, 0, bridge.submitCallCount())
	})
}

// TestSubmitDeposit_RetryOnlyResubmits ③：签名产物落盘后，重试只重发、**签名轮次不再被驱动**；
// 且重发的对象与签名时那份完全一致（txid/金额/SPV 证明/thresholdSig 都不变，签名才对得上）。
func TestSubmitDeposit_RetryOnlyResubmits(t *testing.T) {
	bridge := &fakeBridge{}
	adapter := newDepositTestAdapter(t, bridge, nil)
	rec := seedSettledReceive(t, adapter, "recv-retry", testDepositTxid, 1000)
	bridge.setDepth(testRequiredBest+10, testRgbxConfs)

	// 第一次：签名成功、落盘成功，但提交失败（例如主链暂时不可用）。
	bridge.setSubmitErr(errors.New("chain33 busy"))
	err := adapter.submitDeposit(rec)
	require.Error(t, err)
	require.Equal(t, 1, bridge.signCallCount())
	require.Equal(t, 1, bridge.submitCallCount())
	require.Equal(t, 1, adapter.depositSigs.Len(), "签名产物应已落盘")
	require.Equal(t, ReceiveStatusSettled, mustReceiveStatus(t, adapter, rec.ReceiveID))

	// 第二次（重试）：只重发，不再签名。
	bridge.setSubmitErr(nil)
	require.NoError(t, adapter.submitDeposit(rec))
	require.Equal(t, 1, bridge.signCallCount(), "重试不得再驱动签名轮次")
	require.Equal(t, 2, bridge.submitCallCount())
	require.Equal(t, ReceiveStatusMinted, mustReceiveStatus(t, adapter, rec.ReceiveID))

	attempts := bridge.submitAttemptList()
	require.Len(t, attempts, 2)
	require.Equal(t, attempts[0].GetThresholdSig(), attempts[1].GetThresholdSig(), "重发必须用同一份签名")
	require.Equal(t, attempts[0].GetTxProof().GetTxData(), attempts[1].GetTxProof().GetTxData())
	require.Equal(t, attempts[0].GetAmount(), attempts[1].GetAmount())
	require.Equal(t, attempts[0].GetDepositAddress(), attempts[1].GetDepositAddress())
}

// TestSubmitDeposit_ArtifactSurvivesRestart ③：产物是落盘的，重启（新适配器、同一个 store）后
// 依然只重发、不重签 —— 省掉一轮 GG18（产物丢了也只是重签，见下一条用例）。
func TestSubmitDeposit_ArtifactSurvivesRestart(t *testing.T) {
	store := NewMemStore()

	bridge1 := &fakeBridge{}
	adapter1 := newDepositTestAdapter(t, bridge1, store)
	rec := seedSettledReceive(t, adapter1, "recv-restart", testDepositTxid, 1000)
	bridge1.setDepth(testRequiredBest+10, testRgbxConfs)
	bridge1.setSubmitErr(errors.New("chain33 busy"))
	require.Error(t, adapter1.submitDeposit(rec))
	require.Equal(t, 1, adapter1.depositSigs.Len())

	// 重启：新适配器（新内存缓存）读同一个 store。
	bridge2 := &fakeBridge{}
	bridge2.setDepth(testRequiredBest+10, testRgbxConfs)
	adapter2 := newDepositTestAdapter(t, bridge2, store)
	require.Equal(t, 1, adapter2.depositSigs.Len(), "产物应从 store 里恢复")
	require.NoError(t, adapter2.submitDeposit(rec))
	require.Equal(t, 0, bridge2.signCallCount(), "重启后的重试不得重跑签名轮次")
	require.Equal(t, 1, bridge2.submitCallCount())
}

// TestSubmitDeposit_DuplicateProofTreatedAsMinted ③的幂等面：链上按 btc-txid 去重拒绝（重复证明）时，
// 说明这笔付款交易的铸造链上已经认过 —— 按已铸造处理，否则会每 30s 重发一次、永远停在 settled。
func TestSubmitDeposit_DuplicateProofTreatedAsMinted(t *testing.T) {
	bridge := &fakeBridge{}
	adapter := newDepositTestAdapter(t, bridge, nil)
	rec := seedSettledReceive(t, adapter, "recv-dup", testDepositTxid, 1000)
	bridge.setDepth(testRequiredBest+10, testRgbxConfs)
	// rgbx 执行器 checkDeposit 的 ErrDuplicateDepositProof 文案。
	bridge.setSubmitErr(errors.New("send tx failed: duplicate deposit proof"))

	require.NoError(t, adapter.submitDeposit(rec))
	require.Equal(t, ReceiveStatusMinted, mustReceiveStatus(t, adapter, rec.ReceiveID))
	require.Equal(t, 0, adapter.depositSigs.Len(), "按已铸造处理后应清掉产物")
	require.False(t, adapter.seals.IsPendingMint(rec.Seal))
}

// TestSubmitDeposit_ResignsWhenArtifactLost ④：落盘产物丢失（`rgb20-deposit-sig` 里没有这条 txid）
// 时，重试会**重新签名一轮**并成功提交 —— 这条记录能自愈。
//
// 场景：签名成功、产物落盘，提交失败；随后进程重启且数据目录被清 / 从旧快照恢复 —— 本地只剩一条
// settled 的 receive，产物没了。重签之所以安全：签的是 C = sha256(Encode(DepositAsset{thresholdSig:nil}))，
// 内容只有金额/目标地址/资产符号 + SPV 证明，没有 nonce、时间戳或 UTXO 选择 —— 同一笔充值每次重签
// 得到的 C 逐字节相同，链上还按 txid 去重（formatDepositUsedTxIDKey）兜底。
func TestSubmitDeposit_ResignsWhenArtifactLost(t *testing.T) {
	store := NewMemStore()

	bridge1 := &fakeBridge{}
	adapter1 := newDepositTestAdapter(t, bridge1, store)
	rec := seedSettledReceive(t, adapter1, "recv-lost-artifact", testDepositTxid, 1000)
	bridge1.setDepth(testRequiredBest+10, testRgbxConfs)
	bridge1.setSubmitErr(errors.New("chain33 busy"))
	require.Error(t, adapter1.submitDeposit(rec))
	require.Equal(t, 1, bridge1.signCallCount())
	require.Equal(t, 1, adapter1.depositSigs.Len(), "第一轮应已把签名产物落盘")

	// 产物丢失：抹掉 `rgb20-deposit-sig` 里的记录（等价于重启后数据目录被清 / 从旧快照恢复）。
	require.NoError(t, store.Delete(depositSigBucket, []byte(testDepositTxid)))

	// 重启：新适配器（新内存缓存）读同一个 store —— 产物不在，只剩待铸造的 receive。
	bridge2 := &fakeBridge{}
	bridge2.setDepth(testRequiredBest+10, testRgbxConfs)
	adapter2 := newDepositTestAdapter(t, bridge2, store)
	require.Equal(t, 0, adapter2.depositSigs.Len(), "产物应确实不在本地")

	// 自愈：没有产物就重新签一轮，提交成功、正常 minted（不得停在异常态）。
	require.NoError(t, adapter2.submitDeposit(rec))
	require.Equal(t, 1, bridge2.signCallCount(), "产物丢失时必须重新驱动签名轮次")
	require.Equal(t, 1, bridge2.submitCallCount())
	require.Equal(t, ReceiveStatusMinted, mustReceiveStatus(t, adapter2, rec.ReceiveID))
	require.Equal(t, 0, adapter2.depositSigs.Len(), "铸造成功后应清掉落盘产物")
	require.False(t, adapter2.seals.IsPendingMint(rec.Seal))
}

// TestSubmitDeposit_ResubmitWithArtifactKeepsOneSignatureRound ⑤：产物存在时走重发，**签名轮次
// 一次都不再驱动** —— 这是落盘产物存在的全部意义（省一轮 GG18），也是"提交失败不丢产物"的价值。
func TestSubmitDeposit_ResubmitWithArtifactKeepsOneSignatureRound(t *testing.T) {
	bridge := &fakeBridge{}
	adapter := newDepositTestAdapter(t, bridge, nil)
	rec := seedSettledReceive(t, adapter, "recv-resubmit", testDepositTxid, 1000)
	bridge.setDepth(testRequiredBest+10, testRgbxConfs)

	bridge.setSubmitErr(errors.New("chain33 busy"))
	require.Error(t, adapter.submitDeposit(rec))
	require.Equal(t, 1, bridge.signCallCount())
	require.Equal(t, 1, adapter.depositSigs.Len())

	// 重试：走重发路径，不再签名。
	bridge.setSubmitErr(nil)
	require.NoError(t, adapter.submitDeposit(rec))
	require.Equal(t, 1, bridge.signCallCount(), "有产物时重试不得再驱动签名轮次")
	require.Equal(t, 2, bridge.submitCallCount())
	require.Equal(t, ReceiveStatusMinted, mustReceiveStatus(t, adapter, rec.ReceiveID))
}

// TestRequiredSubmitHeight 门控公式：本地 best 阈值 = H + B + N - 1。
// 对照链上判据 canonical tip >= H + N - 1 与链上可见 tip = best - B，两者等价。
func TestRequiredSubmitHeight(t *testing.T) {
	tests := []struct {
		name        string
		proofHeight uint64
		headerConfs uint64
		minConfs    uint64
		want        uint64
		wantOK      bool
	}{
		{name: "typical", proofHeight: 100, headerConfs: 6, minConfs: 6, want: 111, wantOK: true},
		{name: "no requirements", proofHeight: 100, headerConfs: 0, minConfs: 0, want: 100, wantOK: true},
		{name: "min confs only", proofHeight: 1, headerConfs: 0, minConfs: 1, want: 1, wantOK: true},
		{name: "header confs only", proofHeight: 1, headerConfs: 6, minConfs: 0, want: 6, wantOK: true},
		{name: "overflow", proofHeight: math.MaxUint64, headerConfs: 1, minConfs: 1, want: 0, wantOK: false},
		{name: "overflow on extra", proofHeight: 1, headerConfs: math.MaxUint64, minConfs: 2, want: 0, wantOK: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := requiredSubmitHeight(tc.proofHeight, tc.headerConfs, tc.minConfs)
			require.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				require.Equal(t, tc.want, got)
			}
		})
	}

	// 语义核对：best = required 时，链上可见 tip（= best - B）的确认深度恰好是 N。
	best, ok := requiredSubmitHeight(testProofHeight, testHeaderConfs, testRgbxConfs)
	require.True(t, ok)
	chainVisibleTip := best - testHeaderConfs
	require.Equal(t, testRgbxConfs, chainVisibleTip-testProofHeight+1)
}

// TestDepositSignatureStore_RoundTrip ③的存储层：落盘后再读（新实例）内容一致、可删除。
func TestDepositSignatureStore_RoundTrip(t *testing.T) {
	store := NewMemStore()
	s1 := newDepositSignatureStore(store)
	dep := &rtypes.DepositAsset{
		Amount: 1000, DepositAddress: "addr", AssetSymbol: "RGB20_USDT",
		ThresholdSig: []byte("sig"), TxProof: &rtypes.BtcTxProof{TxData: []byte("tx"), BlockHeight: 100},
	}
	require.NoError(t, s1.Put(&SignedDepositArtifact{Txid: testDepositTxid, ReceiveID: "r", Height: 100, Deposit: dep}))
	require.Equal(t, 1, s1.Len())

	s2 := newDepositSignatureStore(store)
	art, err := s2.Get(testDepositTxid)
	require.NoError(t, err)
	require.NotNil(t, art)
	require.True(t, art.durable, "从 store 读出的产物必须是已落盘状态")
	require.Equal(t, []byte("sig"), art.Deposit.GetThresholdSig())
	require.Equal(t, uint64(100), art.Deposit.GetTxProof().GetBlockHeight())

	require.NoError(t, s2.Delete(testDepositTxid))
	require.Equal(t, 0, s2.Len())
	got, err := newDepositSignatureStore(store).Get(testDepositTxid)
	require.NoError(t, err)
	require.Nil(t, got)
}

func mustReceiveStatus(t *testing.T, a *Adapter, receiveID string) string {
	t.Helper()
	rec, err := a.receives.Get(receiveID)
	require.NoError(t, err)
	return rec.Status
}
