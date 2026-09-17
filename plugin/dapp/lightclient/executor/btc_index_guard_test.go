package executor

import (
	"testing"

	"github.com/33cn/chain33/types"
	ltypes "github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
	"github.com/stretchr/testify/require"
)

/*
 * E6(b) 的用例：Query_GetBtcHeader / Query_GetBtcHeaderByHash 读 localdb 时必须用共识状态
 * （statedb 的 btc-chainstate 窗口 + btc-lastheader）交叉校验。
 *
 * 覆盖四类语义（外加逃生阀）：
 *   - 窗口内一致 → 放行；
 *   - 窗口内不一致 → 拒（fail-closed）；
 *   - 窗口外 / 窗口为空（老链）→ 维持原行为，不因"校验不了"而拒；
 *   - 窗口内有高度但没有 localdb 数据 → 仍是原来的"取不到"语义，不是"不一致"；
 *   - tip 与 btc-lastheader 不一致（丢库/落后）→ 拒；allowBtcIndexMismatch=true 时只报错不拒。
 */

// setAllowBtcIndexMismatch 临时设置逃生阀（lightCfg 是包级变量，用完必须还原）。
func setAllowBtcIndexMismatch(t *testing.T, allow bool) {
	t.Helper()
	saved := lightCfg.AllowBtcIndexMismatch
	lightCfg.AllowBtcIndexMismatch = allow
	t.Cleanup(func() { lightCfg.AllowBtcIndexMismatch = saved })
}

// overwriteLocalHeader 把 localdb 里某高度的头换成另一份（模拟"本地库内容不是 canonical 链"）。
func overwriteLocalHeader(t *testing.T, e *btcReorgTester, height uint64, hash string) {
	t.Helper()
	old := e.localHeader(height)
	require.NotNil(t, old, "用例前提：该高度先有 canonical 头")
	// 旧的 hash → height 索引留着不动也是可以的：按高度读不经过索引，恰好用来验证"按高度读被拒"。
	require.NoError(t, e.localDB.Set(btcHeaderKey(height), types.Encode(&ltypes.BtcHeader{
		Height: height, Hash: hash,
	})))
}

// TestBtcQueryHeaderCrossCheckInWindow 窗口内一致放行、不一致拒绝。
func TestBtcQueryHeaderCrossCheckInWindow(t *testing.T) {
	e := newBtcReorgTester(t)
	setAllowBtcIndexMismatch(t, false)

	chain := e.mineBootstrapChain(1, 5)
	_, _, err := e.submit(chain)
	require.NoError(t, err)
	require.Equal(t, []uint64{1, 2, 3, 4, 5}, chainHeights(e.chainState()), "用例前提：窗口 = 1..5")

	t.Run("consistent header is served", func(t *testing.T) {
		msg, err := e.cli.Query_GetBtcHeader(&ltypes.ReqGetBtcHeader{Height: 3})
		require.NoError(t, err)
		require.Equal(t, chain[2].GetHash(), msg.(*ltypes.BtcHeader).GetHash())
	})

	t.Run("by hash is served", func(t *testing.T) {
		msg, err := e.cli.Query_GetBtcHeaderByHash(&types.ReqString{Data: chain[2].GetHash()})
		require.NoError(t, err)
		require.Equal(t, chain[2].GetHash(), msg.(*ltypes.BtcHeader).GetHash())
	})

	t.Run("mismatch in window is rejected", func(t *testing.T) {
		overwriteLocalHeader(t, e, 3, "ff"+chain[2].GetHash()[2:])
		_, err := e.cli.Query_GetBtcHeader(&ltypes.ReqGetBtcHeader{Height: 3})
		require.ErrorIs(t, err, ErrBtcHeaderNotCanonical,
			"localdb 的头与 canonical 窗口不一致时必须 fail-closed")
		// 同一条 localdb 数据下，按 hash 查也必须拒（hash → height 索引被指出同一个高度）
		_, err = e.cli.Query_GetBtcHeaderByHash(&types.ReqString{Data: chain[2].GetHash()})
		require.ErrorIs(t, err, ErrBtcHeaderNotCanonical)
	})

	t.Run("escape valve only reports", func(t *testing.T) {
		setAllowBtcIndexMismatch(t, true)
		msg, err := e.cli.Query_GetBtcHeader(&ltypes.ReqGetBtcHeader{Height: 3})
		require.NoError(t, err, "逃生阀打开后不得拒绝")
		require.Equal(t, "ff"+chain[2].GetHash()[2:], msg.(*ltypes.BtcHeader).GetHash(),
			"逃生阀只放行，不改写 localdb 的内容")
	})
}

// TestBtcQueryHeaderCrossCheckOutOfWindow 窗口外（比窗口更老）维持原行为：不因为"校验不了"而拒。
func TestBtcQueryHeaderCrossCheckOutOfWindow(t *testing.T) {
	e := newBtcReorgTester(t)
	setAllowBtcIndexMismatch(t, false)

	// 一批 30 个头：窗口 = 最近 25 个节点（高度 6..30），高度 3 落在窗口之外。
	chain := e.mineBootstrapChain(1, 30)
	_, _, err := e.submit(chain)
	require.NoError(t, err)
	nodes := e.chainState().GetNodes()
	require.Len(t, nodes, btcWorkWindowSize)
	require.EqualValues(t, 6, nodes[0].GetHeight(), "用例前提：窗口最低点 = 6，高度 3 在窗口外")

	// 窗口外的 localdb 数据即便与"我们的记忆"不符也无从校验（结构上无法用 stateDB 判定），
	// 必须原样返回 —— 否则老链 / 深于窗口的历史数据会被一刀切拒掉。
	overwriteLocalHeader(t, e, 3, "outside-window-hash")
	msg, err := e.cli.Query_GetBtcHeader(&ltypes.ReqGetBtcHeader{Height: 3})
	require.NoError(t, err)
	require.Equal(t, "outside-window-hash", msg.(*ltypes.BtcHeader).GetHash())

	// 窗口内的高度仍然要校验（同一份 fixture 里换个高度）
	overwriteLocalHeader(t, e, 7, "in-window-hash")
	_, err = e.cli.Query_GetBtcHeader(&ltypes.ReqGetBtcHeader{Height: 7})
	require.ErrorIs(t, err, ErrBtcHeaderNotCanonical)
}

// TestBtcQueryHeaderCrossCheckMissingLocalHeader 窗口内"没有这条"与"这条不一致"必须区分：
// 前者是原来的取不到语义（ErrNotFound），不能报成不一致。
func TestBtcQueryHeaderCrossCheckMissingLocalHeader(t *testing.T) {
	e := newBtcReorgTester(t)
	setAllowBtcIndexMismatch(t, false)

	chain := e.mineBootstrapChain(1, 5)
	_, _, err := e.submit(chain)
	require.NoError(t, err)

	// 删掉高度 4 的逐高度头（模拟 localdb 只缺这一条）
	applyKV(t, e.localDB, []*types.KeyValue{{Key: btcHeaderKey(4)}})
	_, err = e.cli.Query_GetBtcHeader(&ltypes.ReqGetBtcHeader{Height: 4})
	require.Equal(t, types.ErrNotFound, err, "缺失应保持原有的取不到语义")
	require.NotErrorIs(t, err, ErrBtcHeaderNotCanonical)
}

// TestBtcQueryHeaderCrossCheckLegacyWindow 窗口数据缺失（B3/B4 之前的老链只有 btc-lastheader）：
// 校验不了就不能拒，必须维持原行为。
func TestBtcQueryHeaderCrossCheckLegacyWindow(t *testing.T) {
	e := newBtcReorgTester(t)
	setAllowBtcIndexMismatch(t, false)

	chain := e.mineBootstrapChain(1, 5)
	// 老版本的写入方式：只写 btc-lastheader + localdb 逐高度头，没有 btc-chainstate
	require.NoError(t, e.stateDB.Set(btcLastHeaderKey(), types.Encode(chain[4])))
	applyKV(t, e.localDB, appendBtcHeadersKV(nil, chain))
	state, err := getBtcChainState(e.stateDB)
	require.NoError(t, err)
	require.Empty(t, state.GetNodes(), "用例前提：没有窗口")

	msg, err := e.cli.Query_GetBtcHeader(&ltypes.ReqGetBtcHeader{Height: 2})
	require.NoError(t, err, "老链没有窗口数据时不得因此拒查询")
	require.Equal(t, chain[1].GetHash(), msg.(*ltypes.BtcHeader).GetHash())
}

// TestBtcQueryHeaderCrossCheckEmptyChain 链上还没有任何头（statedb 空）：localdb 里有什么都不影响判定。
func TestBtcQueryHeaderCrossCheckEmptyChain(t *testing.T) {
	e := newBtcReorgTester(t)
	setAllowBtcIndexMismatch(t, false)

	header := e.mineBootstrapChain(1, 1)[0]
	applyKV(t, e.localDB, appendBtcHeadersKV(nil, []*ltypes.BtcHeader{header}))

	msg, err := e.cli.Query_GetBtcHeader(&ltypes.ReqGetBtcHeader{Height: 1})
	require.NoError(t, err)
	require.Equal(t, header.GetHash(), msg.(*ltypes.BtcHeader).GetHash())
}

// TestBtcQueryHeaderCrossCheckTipMismatch tip 自检：localdb 在共识 tip 高度上没有一致的头就拒。
func TestBtcQueryHeaderCrossCheckTipMismatch(t *testing.T) {
	e := newBtcReorgTester(t)
	setAllowBtcIndexMismatch(t, false)

	chain := e.mineBootstrapChain(1, 5)
	_, _, err := e.submit(chain)
	require.NoError(t, err)

	// localdb 丢库的形态之一：共识 tip（高度 5）那条没了，更低的高度还在
	applyKV(t, e.localDB, []*types.KeyValue{{Key: btcHeaderKey(5)}})
	_, err = e.cli.Query_GetBtcHeader(&ltypes.ReqGetBtcHeader{Height: 3})
	require.ErrorIs(t, err, ErrBtcLocalIndexMismatch, "共识 tip 高度在 localdb 里缺失必须 fail-closed")
	_, err = e.cli.Query_GetBtcHeaderByHash(&types.ReqString{Data: chain[2].GetHash()})
	require.ErrorIs(t, err, ErrBtcLocalIndexMismatch)

	// 逃生阀：只报错不拒（此时问的是一个完好的高度，应当正常返回）
	setAllowBtcIndexMismatch(t, true)
	msg, err := e.cli.Query_GetBtcHeader(&ltypes.ReqGetBtcHeader{Height: 3})
	require.NoError(t, err)
	require.Equal(t, chain[2].GetHash(), msg.(*ltypes.BtcHeader).GetHash())

	// 换成"tip 高度有头但 hash 不同"（停在别的链上）同样要拒
	setAllowBtcIndexMismatch(t, false)
	require.NoError(t, e.localDB.Set(btcHeaderKey(5), types.Encode(
		&ltypes.BtcHeader{Height: 5, Hash: "other-chain-5"})))
	_, err = e.cli.Query_GetBtcHeader(&ltypes.ReqGetBtcHeader{Height: 3})
	require.ErrorIs(t, err, ErrBtcLocalIndexMismatch)
}

// TestBtcQueryHeaderCrossCheckNoLocalDB 节点没有 localdb（exec.disableExecLocal）：
// 返回明确错误（fail-closed），不是 panic。
func TestBtcQueryHeaderCrossCheckNoLocalDB(t *testing.T) {
	e := newBtcReorgTester(t)
	setAllowBtcIndexMismatch(t, false)

	chain := e.mineBootstrapChain(1, 5)
	require.NoError(t, e.stateDB.Set(btcLastHeaderKey(), types.Encode(chain[4])))
	e.cli.SetLocalDB(nil)

	_, err := e.cli.Query_GetBtcHeader(&ltypes.ReqGetBtcHeader{Height: 3})
	require.ErrorIs(t, err, ErrBtcLocalIndexMismatch)
}

// TestBtcQueryHeaderCrossCheckCorruptState 读不到共识状态方向同样 fail-closed。
func TestBtcQueryHeaderCrossCheckCorruptState(t *testing.T) {
	e := newBtcReorgTester(t)
	setAllowBtcIndexMismatch(t, false)

	chain := e.mineBootstrapChain(1, 5)
	applyKV(t, e.localDB, appendBtcHeadersKV(nil, chain))
	require.NoError(t, e.stateDB.Set(btcLastHeaderKey(), []byte("bad-data")))

	_, err := e.cli.Query_GetBtcHeader(&ltypes.ReqGetBtcHeader{Height: 3})
	require.ErrorIs(t, err, ErrBtcLocalIndexMismatch)
}

// TestBtcHeaderCanonicalPure 纯函数用例：判定只看"窗口里有没有这个高度"。
func TestBtcHeaderCanonicalPure(t *testing.T) {
	state := &ltypes.BtcChainState{Nodes: []*ltypes.BtcChainNode{
		{Height: 10, Hash: "h10"},
		{Height: 11, Hash: "h11"},
	}}

	// 窗口里有且一致 → 放行
	require.NoError(t, checkBtcHeaderCanonical(state, &ltypes.BtcHeader{Height: 11, Hash: "h11"}, false))
	// 窗口里有但不一致 → 拒
	err := checkBtcHeaderCanonical(state, &ltypes.BtcHeader{Height: 11, Hash: "other"}, false)
	require.ErrorIs(t, err, ErrBtcHeaderNotCanonical)
	// 逃生阀 → 放行
	require.NoError(t, checkBtcHeaderCanonical(state, &ltypes.BtcHeader{Height: 11, Hash: "other"}, true))
	// 窗口里没有这个高度（更老 / 更高）→ 放行
	require.NoError(t, checkBtcHeaderCanonical(state, &ltypes.BtcHeader{Height: 9, Hash: "other"}, false))
	require.NoError(t, checkBtcHeaderCanonical(state, &ltypes.BtcHeader{Height: 12, Hash: "other"}, false))
	// 窗口为空（老链 / 测试 fixture）→ 放行
	require.NoError(t, checkBtcHeaderCanonical(&ltypes.BtcChainState{}, &ltypes.BtcHeader{Height: 11, Hash: "other"}, false))
}
