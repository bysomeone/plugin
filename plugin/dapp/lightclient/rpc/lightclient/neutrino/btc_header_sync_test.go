package neutrino

import (
	"errors"
	"fmt"
	"testing"
	"time"

	ltypes "github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
	"github.com/stretchr/testify/require"
)

// ---- 测试替身：链上 canonical 视图 / 本地视图 ----

type fakeChainHeaderView struct {
	tipHeight uint64
	hashes    map[uint64]string
	tipErr    error
	hashErr   error
	probes    int
}

func (f *fakeChainHeaderView) chainTipHeader() (*ltypes.BtcHeader, error) {
	if f.tipErr != nil {
		return nil, f.tipErr
	}
	if f.tipHeight == 0 {
		return &ltypes.BtcHeader{}, nil
	}
	return &ltypes.BtcHeader{Height: f.tipHeight, Hash: f.hashes[f.tipHeight]}, nil
}

func (f *fakeChainHeaderView) chainHeaderHashAt(height uint64) (string, bool, error) {
	f.probes++
	if f.hashErr != nil {
		return "", false, f.hashErr
	}
	hash, ok := f.hashes[height]
	if !ok || hash == "" {
		return "", false, nil
	}
	return hash, true, nil
}

type fakeLocalHeaderView struct {
	hashes map[uint64]string
	err    error
	reads  int
}

func (f *fakeLocalHeaderView) localHeaderHashAt(height uint64) (string, error) {
	f.reads++
	if f.err != nil {
		return "", f.err
	}
	hash, ok := f.hashes[height]
	if !ok || hash == "" {
		return "", fmt.Errorf("local btc header at height %d not found", height)
	}
	return hash, nil
}

// testBtcHash 造一个可读的假 hash（64 位十六进制，符合 ltypes.BtcHeader.Hash 的口径）。
func testBtcHash(branch byte, height uint64) string {
	return fmt.Sprintf("%02x%062d", branch, height)
}

// testBtcChain 造 [from, to] 的逐高度 hash（同一分支号）。
func testBtcChain(branch byte, from, to uint64) map[uint64]string {
	hashes := make(map[uint64]string, to-from+1)
	for h := from; h <= to; h++ {
		hashes[h] = testBtcHash(branch, h)
	}
	return hashes
}

func testBtcMerge(dst, src map[uint64]string) map[uint64]string {
	for h, hash := range src {
		dst[h] = hash
	}
	return dst
}

func Test_btcHeaderReconciler_normalForwardProgress(t *testing.T) {
	chain := &fakeChainHeaderView{tipHeight: 10, hashes: testBtcChain(0xaa, 1, 10)}
	local := &fakeLocalHeaderView{hashes: testBtcChain(0xaa, 1, 15)}
	r := &btcHeaderReconciler{chain: chain, local: local, startHeight: 840001, maxDepth: maxBtcHeaderReorgDepth}

	plan, err := r.plan(15)
	require.NoError(t, err)
	require.Equal(t, uint64(11), plan.nextSubmitHeight)
	require.Equal(t, uint64(10), plan.matchedHeight)
	require.Equal(t, uint64(10), plan.chainTipHeight)
	require.False(t, plan.rolledBack)
	require.Equal(t, 1, plan.probes, "正常推进只需比对链上 tip 一个高度")
}

func Test_btcHeaderReconciler_rollsBackToCommonAncestorAfterReorg(t *testing.T) {
	// 链上（重组后）：高度 1..8 是 A 分支，9..12 已经切到 B 分支。
	chainHashes := testBtcMerge(testBtcChain(0xaa, 1, 8), testBtcChain(0xbb, 9, 12))
	chain := &fakeChainHeaderView{tipHeight: 12, hashes: chainHashes}
	// 本地（btcd）视图：1..8 与链上一致，9..15 是原来提交过的 A 分支（已作废）。
	localHashes := testBtcMerge(testBtcChain(0xaa, 1, 8), testBtcChain(0xcc, 9, 15))
	local := &fakeLocalHeaderView{hashes: localHashes}
	r := &btcHeaderReconciler{chain: chain, local: local, startHeight: 840001, maxDepth: maxBtcHeaderReorgDepth}

	plan, err := r.plan(15)
	require.NoError(t, err)
	// 必须回退到一致高度 8，从 9 重发；而不是继续从 16 提交"父块从没上过链"的批。
	require.Equal(t, uint64(9), plan.nextSubmitHeight)
	require.Equal(t, uint64(8), plan.matchedHeight)
	require.True(t, plan.rolledBack)
	require.Equal(t, 5, plan.probes)

	// 重发的批次自 9 起连续（B7：批内高度逐个 +1）。
	from, to, ok := btcHeaderBatchRange(plan.nextSubmitHeight, 15, btcHeaderBatchSize)
	require.True(t, ok)
	require.Equal(t, uint64(9), from)
	require.Equal(t, uint64(15), to)
}

func Test_btcHeaderReconciler_sameHeightDifferentHashWalksDown(t *testing.T) {
	chain := &fakeChainHeaderView{
		tipHeight: 10,
		hashes:    testBtcMerge(testBtcChain(0xaa, 1, 10), map[uint64]string{}),
	}
	local := &fakeLocalHeaderView{
		hashes: testBtcMerge(testBtcChain(0xaa, 1, 7), testBtcChain(0xcc, 8, 10)),
	}
	r := &btcHeaderReconciler{chain: chain, local: local, startHeight: 1, maxDepth: maxBtcHeaderReorgDepth}

	plan, err := r.plan(10)
	require.NoError(t, err)
	require.Equal(t, uint64(8), plan.nextSubmitHeight)
	require.Equal(t, uint64(7), plan.matchedHeight)
	require.True(t, plan.rolledBack)
	require.Equal(t, 4, plan.probes)
}

func Test_btcHeaderReconciler_tooDeepReturnsExplicitError(t *testing.T) {
	// 链上 tip=100，本地从 71 起就与链上不一致：允许窗口是 [76, 100]，窗口内找不到一致高度。
	chain := &fakeChainHeaderView{
		tipHeight: 100,
		hashes:    testBtcMerge(testBtcChain(0xaa, 1, 70), testBtcChain(0xbb, 71, 100)),
	}
	local := &fakeLocalHeaderView{
		hashes: testBtcMerge(testBtcChain(0xaa, 1, 70), testBtcChain(0xcc, 71, 100)),
	}
	r := &btcHeaderReconciler{chain: chain, local: local, startHeight: 1, maxDepth: maxBtcHeaderReorgDepth}

	plan, err := r.plan(100)
	require.Error(t, err)
	require.True(t, errors.Is(err, errBtcReconcileNoCommonHeader), "必须是不可自愈的错位: %v", err)
	require.False(t, errors.Is(err, errBtcReconcileLocalBehind))
	require.Nil(t, plan)
	// 比对次数有界（回退深度 + 1），不会无限往下找。
	require.Equal(t, int(maxBtcHeaderReorgDepth)+1, chain.probes)
}

func Test_btcHeaderReconciler_localBehindWaitsForSync(t *testing.T) {
	chain := &fakeChainHeaderView{
		tipHeight: 100,
		hashes:    testBtcMerge(testBtcChain(0xaa, 1, 76), testBtcChain(0xaa, 77, 100)),
	}
	local := &fakeLocalHeaderView{hashes: testBtcChain(0xaa, 1, 50)}
	r := &btcHeaderReconciler{chain: chain, local: local, startHeight: 1, maxDepth: maxBtcHeaderReorgDepth}

	plan, err := r.plan(50)
	require.Error(t, err)
	require.True(t, errors.Is(err, errBtcReconcileLocalBehind), "本地落后应等同步而不是报不可自愈: %v", err)
	require.Nil(t, plan)
	require.Zero(t, chain.probes, "本地落后时不必逐高度比对")
}

func Test_btcHeaderReconciler_bootstrapUsesConfiguredStartHeight(t *testing.T) {
	chain := &fakeChainHeaderView{tipHeight: 0}
	local := &fakeLocalHeaderView{hashes: testBtcChain(0xaa, 1, 100)}
	r := &btcHeaderReconciler{chain: chain, local: local, startHeight: 840001, maxDepth: maxBtcHeaderReorgDepth}

	plan, err := r.plan(100)
	require.NoError(t, err)
	require.Equal(t, uint64(840001), plan.nextSubmitHeight, "链上还没有头时用配置的同步起点")
	require.Zero(t, plan.chainTipHeight)
	require.Zero(t, chain.probes)
	require.Zero(t, local.reads)
}

func Test_btcHeaderReconciler_skipsHeightsMissingOnChain(t *testing.T) {
	// 链上 tip 记到 12，但 localdb 里 11、12 两个高度没有头（起点之下 / 被换链删除）：
	// 对账要继续往下找，而不是把"查不到"当成不一致就报错。
	chain := &fakeChainHeaderView{tipHeight: 12, hashes: testBtcChain(0xaa, 1, 10)}
	local := &fakeLocalHeaderView{hashes: testBtcChain(0xaa, 1, 15)}
	r := &btcHeaderReconciler{chain: chain, local: local, startHeight: 1, maxDepth: maxBtcHeaderReorgDepth}

	plan, err := r.plan(15)
	require.NoError(t, err)
	require.Equal(t, uint64(11), plan.nextSubmitHeight)
	require.Equal(t, uint64(10), plan.matchedHeight)
	require.Equal(t, 3, plan.probes)
}

func Test_btcHeaderReconciler_queryErrorIsNotUnrecoverable(t *testing.T) {
	queryErr := errors.New("rpc error: code = Unavailable desc = connection refused")
	chain := &fakeChainHeaderView{tipHeight: 10, hashes: testBtcChain(0xaa, 1, 10), hashErr: queryErr}
	local := &fakeLocalHeaderView{hashes: testBtcChain(0xaa, 1, 15)}
	r := &btcHeaderReconciler{chain: chain, local: local, startHeight: 1, maxDepth: maxBtcHeaderReorgDepth}

	plan, err := r.plan(15)
	require.Error(t, err)
	require.True(t, errors.Is(err, queryErr))
	// 查询故障只是本轮的临时失败，下一轮重试；不能误判成"深度超限/配置错位"那样停在那里。
	require.False(t, errors.Is(err, errBtcReconcileNoCommonHeader))
	require.False(t, errors.Is(err, errBtcReconcileLocalBehind))
	require.Nil(t, plan)
}

func Test_btcHeaderBatchRange(t *testing.T) {
	tests := []struct {
		name      string
		next      uint64
		confirmed uint64
		size      int
		wantFrom  uint64
		wantTo    uint64
		wantOk    bool
	}{
		{name: "full batch", next: 10, confirmed: 100, size: 16, wantFrom: 10, wantTo: 25, wantOk: true},
		{name: "truncated by confirmations", next: 95, confirmed: 100, size: 16, wantFrom: 95, wantTo: 100, wantOk: true},
		{name: "single header", next: 100, confirmed: 100, size: 16, wantFrom: 100, wantTo: 100, wantOk: true},
		{name: "nothing confirmed yet", next: 101, confirmed: 100, size: 16, wantOk: false},
		{name: "no start height", next: 0, confirmed: 100, size: 16, wantOk: false},
		{name: "zero batch size", next: 10, confirmed: 100, size: 0, wantOk: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			from, to, ok := btcHeaderBatchRange(tt.next, tt.confirmed, tt.size)
			require.Equal(t, tt.wantOk, ok)
			if !ok {
				return
			}
			require.Equal(t, tt.wantFrom, from)
			require.Equal(t, tt.wantTo, to)
			// B7：批内高度必须逐个 +1，且不超过执行器上限。
			require.LessOrEqual(t, int(to-from+1), btcHeaderBatchSize)
		})
	}
}

func Test_classifyBtcHeaderSubmitErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want btcHeaderSubmitResult
	}{
		{name: "nil", err: nil, want: btcHeaderSubmitAccepted},
		{name: "duplicate tx", err: errors.New("ErrDupTx"), want: btcHeaderSubmitDuplicateAccepted},
		{name: "duplicate text", err: errors.New("duplicate tx in mempool"), want: btcHeaderSubmitDuplicateAccepted},
		{
			name: "reorg too deep",
			err:  errors.New("rpc error: code = Unknown desc = ErrBtcReorgTooDeep"),
			want: btcHeaderSubmitRejected,
		},
		{name: "unknown ancestor", err: errors.New("ErrBtcHeaderUnknownAncestor"), want: btcHeaderSubmitRejected},
		{name: "no anchor (起点配置错位)", err: errors.New("ErrBtcHeaderNoAnchor"), want: btcHeaderSubmitRejected},
		{name: "duplicate height", err: errors.New("ErrBtcHeaderDuplicateHeight"), want: btcHeaderSubmitRejected},
		{name: "illegal commit address", err: errors.New("ErrIllegalCommitAddress"), want: btcHeaderSubmitRejected},
		{name: "network error", err: errors.New("connection refused"), want: btcHeaderSubmitTransient},
		{name: "timeout", err: errors.New("context deadline exceeded"), want: btcHeaderSubmitTransient},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, classifyBtcHeaderSubmitErr(tt.err))
		})
	}
}

func Test_btcHeaderSyncState_inFlightBatchIsNotResubmittedEveryTick(t *testing.T) {
	st := &btcHeaderSyncState{}
	now := time.Now()

	require.True(t, st.allowSubmit(100, now), "没有在途批次时必须提交")

	st.onSubmitted(100, 115, now, false)
	require.False(t, st.allowSubmit(100, now.Add(time.Minute)), "在途批次覆盖范围内的起点不重发")
	require.False(t, st.allowSubmit(110, now.Add(time.Minute)))
	require.True(t, st.allowSubmit(116, now.Add(time.Minute)), "越过在途批次（链上往前走了）要提交")
	require.True(t, st.allowSubmit(95, now.Add(time.Minute)), "回退到在途批次之下（链上否定了它）要提交")
	require.True(t, st.allowSubmit(100, now.Add(btcHeaderBatchRefreshInterval)),
		"超过续期间隔后放行一次心跳（mempool 里的交易可能被挤掉）")
}

func Test_btcHeaderSyncState_countsOnlyFreshResubmitsAsFutile(t *testing.T) {
	st := &btcHeaderSyncState{}
	now := time.Now()

	st.onSubmitted(100, 115, now, false)
	st.onSubmitted(100, 115, now.Add(time.Minute), true) // 重复提交（还在 mempool）：续期，不算空转
	require.Zero(t, st.futileSubmits)
	require.Equal(t, now.Add(time.Minute), st.pendingAt)

	st.onSubmitted(100, 115, now.Add(2*time.Minute), false) // 又变成新交易：上一笔已不在 mempool
	st.onSubmitted(100, 115, now.Add(3*time.Minute), false)
	require.Equal(t, 2, st.futileSubmits)

	// 换了一批（链上推进了）：空转计数清零。
	st.onSubmitted(116, 131, now.Add(4*time.Minute), false)
	require.Zero(t, st.futileSubmits)
	require.Equal(t, uint64(116), st.pendingBottom)
	require.Equal(t, uint64(131), st.pendingTop)
}

// Test_btcHeaderSubmitCycle_reorgResendsFromForkPointOnly 按 tick 驱动"对账 + 提交决策 + 状态转移"
// 这一整套生产代码，断言重组后的行为：
//   - 回退到一致高度，从那里重发（而不是继续提交父块从没上过链的批）；
//   - 同一批在途时后续 tick 不重发；
//   - 链上推进（或计划变化）后恢复正常推进。
func Test_btcHeaderSubmitCycle_reorgResendsFromForkPointOnly(t *testing.T) {
	// 链上（重组后）：1..8 是 A 分支（也是我们提交过的），9..12 已切到 B 分支。
	chain := &fakeChainHeaderView{
		tipHeight: 12,
		hashes:    testBtcMerge(testBtcChain(0xaa, 1, 8), testBtcChain(0xbb, 9, 12)),
	}
	// 本地视图：1..8 与链上一致，9..15 是本地（btcd）自己的链，正是要重发的部分。
	local := &fakeLocalHeaderView{
		hashes: testBtcMerge(testBtcChain(0xaa, 1, 8), testBtcChain(0xcc, 9, 15)),
	}
	r := &btcHeaderReconciler{chain: chain, local: local, startHeight: 1, maxDepth: maxBtcHeaderReorgDepth}
	st := &btcHeaderSyncState{}
	now := time.Now()

	// tick 1：对账 → 回退到 8 → 从 9 重发 [9, 15]。
	plan, err := r.plan(15)
	require.NoError(t, err)
	require.True(t, plan.rolledBack)
	from, to, reason := st.beginSubmit(plan, 15, now)
	require.Equal(t, btcHeaderSubmitReady, reason)
	require.Equal(t, uint64(9), from, "重发必须自一致高度 +1 起，不能继续提交未知父块的批")
	require.Equal(t, uint64(15), to)
	st.finishSubmit(plan, from, to, now, btcHeaderSubmitAccepted)

	// tick 2（链上还没把这一批纳入 canonical）：不重发同一个批。
	chain.tipHeight = 12
	plan2, err := r.plan(15)
	require.NoError(t, err)
	_, _, reason = st.beginSubmit(plan2, 15, now.Add(time.Minute))
	require.Equal(t, btcHeaderSubmitAlreadyInFlight, reason)

	// tick 3：链上把这一批纳入 canonical（tip 变成 15，且是我们这条链）→ 恢复正常推进到 16。
	chain.tipHeight = 15
	chain.hashes = testBtcChain(0xcc, 1, 15)
	local.hashes = testBtcChain(0xcc, 1, 16)
	plan3, err := r.plan(16)
	require.NoError(t, err)
	require.False(t, plan3.rolledBack)
	require.Equal(t, uint64(16), plan3.nextSubmitHeight)
	from3, to3, reason := st.beginSubmit(plan3, 16, now.Add(2*time.Minute))
	require.Equal(t, btcHeaderSubmitReady, reason)
	require.Equal(t, uint64(16), from3)
	require.Equal(t, uint64(16), to3)
}

// Test_btcHeaderSubmitCycle_stopsAfterUnrecoverableRejection 链上确定性拒收后不再重复提交同一批
// （不刷屏），计划一变就恢复。
func Test_btcHeaderSubmitCycle_stopsAfterUnrecoverableRejection(t *testing.T) {
	st := &btcHeaderSyncState{}
	now := time.Now()
	plan := &btcHeaderSyncPlan{nextSubmitHeight: 100, chainTipHeight: 99}

	from, to, reason := st.beginSubmit(plan, 200, now)
	require.Equal(t, btcHeaderSubmitReady, reason)
	st.finishSubmit(plan, from, to, now, btcHeaderSubmitRejected)

	// 同一个计划：后续 tick 都不再提交。
	for i := 1; i <= 3; i++ {
		next := now.Add(time.Duration(i) * time.Hour)
		_, _, reason = st.beginSubmit(plan, 200, next)
		require.Equal(t, btcHeaderSubmitPlanHalted, reason, "tick %d 不应重复提交被拒收的同一批", i)
	}

	// 链上 tip 变了（有进展）→ 计划变了 → 允许再试。
	moved := &btcHeaderSyncPlan{nextSubmitHeight: 100, chainTipHeight: 105}
	_, _, reason = st.beginSubmit(moved, 200, now.Add(4*time.Hour))
	require.Equal(t, btcHeaderSubmitReady, reason)
}

func Test_btcHeaderSyncState_haltOnlyUntilPlanChanges(t *testing.T) {
	st := &btcHeaderSyncState{}
	plan := &btcHeaderSyncPlan{nextSubmitHeight: 100, chainTipHeight: 99}
	require.False(t, st.halted(plan))

	st.halt(plan)
	require.True(t, st.halted(plan))
	require.True(t, st.halted(&btcHeaderSyncPlan{nextSubmitHeight: 100, chainTipHeight: 99}),
		"同一个计划（起点与链上 tip 都没变）不再重复提交")

	require.False(t, st.halted(&btcHeaderSyncPlan{nextSubmitHeight: 100, chainTipHeight: 105}),
		"链上 tip 变了 → 计划变了 → 允许再试")
	require.False(t, st.halted(&btcHeaderSyncPlan{nextSubmitHeight: 90, chainTipHeight: 99}),
		"对账给出不同起点 → 允许再试")
}

func Test_btcHeaderReportLimiter_reportsOncePerState(t *testing.T) {
	l := &btcHeaderReportLimiter{}
	require.True(t, l.allow("unreconcilable:chainTip=100"))
	require.False(t, l.allow("unreconcilable:chainTip=100"), "同一状态不刷屏")
	require.True(t, l.allow("unreconcilable:chainTip=200"), "状态变了再报一次")

	l.reset()
	require.True(t, l.allow("unreconcilable:chainTip=200"), "对账恢复正常后，下次出错再报")
}
