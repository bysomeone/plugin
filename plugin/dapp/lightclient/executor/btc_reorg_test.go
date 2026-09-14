package executor

import (
	"math/big"
	"testing"
	"time"

	"github.com/33cn/chain33/common/crypto"
	dbm "github.com/33cn/chain33/common/db"
	"github.com/33cn/chain33/types"
	"github.com/33cn/chain33/util"
	ltypes "github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/stretchr/testify/require"
)

/*
 * B3（reorg 回退）+ B4（按累积工作量选链）的用例。
 *
 * regtest 的 PoWNoRetargeting=true ⇒ 每个头的 Bits 都是 powLimitBits、单块工作量恒为 1，
 * 所以真挖矿场景里"更重"等价于"更长"，足以覆盖 E2E 流程；
 * "更短但更重"（由工作量而不是长度决定胜负）用 planBtcHeaders 的纯函数用例覆盖。
 */

var btcTestBits = chaincfg.RegressionNetParams.PowLimitBits

// btcReorgTester 模拟框架对一笔 BtcHeaders 交易的处理：CheckTx → Exec（写 statedb）→ ExecLocal（写 localdb）。
type btcReorgTester struct {
	t          *testing.T
	cli        *lightclient
	stateDB    dbm.DB
	localDB    dbm.KVDB
	commitPriv crypto.PrivKey
	clock      time.Time
}

func newBtcReorgTester(t *testing.T) *btcReorgTester {
	t.Helper()
	dir, stateDB, localDB := util.CreateTestDB()
	t.Cleanup(func() { util.CloseTestDB(dir, stateDB) })

	cli := newLightclient().(*lightclient)
	setupTestDriver(t, cli)
	cli.SetStateDB(stateDB)
	cli.SetLocalDB(localDB)

	commitAddr, commitPriv := util.Genaddress()
	lightCfg.CommitAddress = commitAddr
	lightCfg.BtcNetName = "regtest"

	return &btcReorgTester{
		t: t, cli: cli, stateDB: stateDB, localDB: localDB,
		commitPriv: commitPriv, clock: types.Now().Add(-time.Hour),
	}
}

// nextTimestamp 造链用的单调时钟：保证所有头的时间戳整体递增，避免 median-time-past 规则误伤。
func (e *btcReorgTester) nextTimestamp() time.Time {
	e.clock = e.clock.Add(time.Minute)
	return e.clock
}

// mineChain 从 prev 之后连续挖 [from, to] 高度的头。
func (e *btcReorgTester) mineChain(prev *ltypes.BtcHeader, from, to uint64) []*ltypes.BtcHeader {
	e.t.Helper()
	headers := make([]*ltypes.BtcHeader, 0, to-from+1)
	for height := from; height <= to; height++ {
		prev = mineBtcHeader(e.t, prev, height, btcTestBits, e.nextTimestamp())
		headers = append(headers, prev)
	}
	return headers
}

// mineBootstrapChain 以 regtest 创世为锚点，从高度 from 挖到 to。
func (e *btcReorgTester) mineBootstrapChain(from, to uint64) []*ltypes.BtcHeader {
	e.t.Helper()
	first := mineBtcHeaderFrom(e.t, chaincfg.RegressionNetParams.GenesisHash.String(), from, btcTestBits, e.nextTimestamp())
	return append([]*ltypes.BtcHeader{first}, e.mineChain(first, from+1, to)...)
}

// submit 走一遍 CheckTx → Exec → ExecLocal，并把回执 KV 落到 statedb、localdb KV 落到 localdb（模拟框架）。
func (e *btcReorgTester) submit(headers []*ltypes.BtcHeader) (*types.Receipt, *ltypes.BtcHeadersLog, error) {
	e.t.Helper()
	tx := buildCheckTx(e.t, &ltypes.BtcHeaders{Headers: headers}, e.commitPriv)
	if err := e.cli.CheckTx(tx, 0); err != nil {
		return nil, nil, err
	}
	receipt, err := e.cli.Exec_BtcHeaders(&ltypes.BtcHeaders{Headers: headers}, tx, 0)
	if err != nil {
		return nil, nil, err
	}
	applyKV(e.t, e.stateDB, receipt.GetKV())

	receiptData := &types.ReceiptData{Ty: receipt.GetTy(), Logs: receipt.GetLogs()}
	dbSet, err := e.cli.ExecLocal_BtcHeaders(&ltypes.BtcHeaders{Headers: headers}, tx, receiptData, 0)
	require.NoError(e.t, err)
	applyKV(e.t, e.localDB, dbSet.GetKV())

	log, err := findBtcHeadersLog(receiptData)
	require.NoError(e.t, err)
	require.NotNil(e.t, log)
	return receipt, log, nil
}

// applyKV 应用 KV 集合。nil value 在 chain33 里就是"删除"：statedb 走 DB.Delete，
// localdb 是 dapp 返回 nil、框架侧落到空值（LocalDB.Get 的 isdeleted 视空值为 ErrNotFound）。
func applyKV(t *testing.T, kvdb dbm.KV, kvs []*types.KeyValue) {
	t.Helper()
	for _, kv := range kvs {
		if kv.GetValue() == nil {
			if db, ok := kvdb.(dbm.DB); ok {
				require.NoError(t, db.Delete(kv.GetKey()))
			} else {
				require.NoError(t, kvdb.Set(kv.GetKey(), nil))
			}
			continue
		}
		require.NoError(t, kvdb.Set(kv.GetKey(), kv.GetValue()))
	}
}

func (e *btcReorgTester) tip() *ltypes.BtcHeader {
	e.t.Helper()
	tip, err := getBtcLastHeader(e.stateDB)
	require.NoError(e.t, err)
	return tip
}

func (e *btcReorgTester) chainState() *ltypes.BtcChainState {
	e.t.Helper()
	state, err := getBtcChainState(e.stateDB)
	require.NoError(e.t, err)
	return state
}

// localHeader 按高度读 localdb 的 canonical 头；不存在返回 nil。
func (e *btcReorgTester) localHeader(height uint64) *ltypes.BtcHeader {
	e.t.Helper()
	header, err := getBtcHeader(e.localDB, height)
	if err != nil || header.GetHash() == "" {
		return nil
	}
	return header
}

// localHeaderByHash 走 hash -> height 索引读 localdb；索引被删则返回 nil。
func (e *btcReorgTester) localHeaderByHash(hash string) *ltypes.BtcHeader {
	e.t.Helper()
	height, err := getBtcHeight(e.localDB, hash)
	if err != nil || height.GetData() == 0 {
		return nil
	}
	return e.localHeader(uint64(height.GetData()))
}

// chainHeights 返回窗口里的高度序列，便于断言。
func chainHeights(state *ltypes.BtcChainState) []uint64 {
	heights := make([]uint64, 0, len(state.GetNodes()))
	for _, node := range state.GetNodes() {
		heights = append(heights, node.GetHeight())
	}
	return heights
}

func chainNodeHashes(state *ltypes.BtcChainState) map[uint64]string {
	hashes := make(map[uint64]string, len(state.GetNodes()))
	for _, node := range state.GetNodes() {
		hashes[node.GetHeight()] = node.GetHash()
	}
	return hashes
}

// TestBtcPlanHeadersWorkSelectsHeaviest B4 的纯函数用例：选链只看累积工作量，不看长度。
func TestBtcPlanHeadersWorkSelectsHeaviest(t *testing.T) {
	regtestBits := int64(chaincfg.RegressionNetParams.PowLimitBits)
	oneBlockWork := btcWorkOfBits(regtestBits)
	// 一个"极重"的块（mainnet 的 powLimit 对应约 2^32 的工作量），用来在纯计算里构造"更短但更重"。
	heavyWork := btcWorkOfBits(int64(chaincfg.MainNetParams.PowLimitBits))
	require.Equal(t, 1, heavyWork.Cmp(big.NewInt(1e9)), "用例假设：重块的 work 远大于 1e9")

	// 窗口 100(work 1000) -> 101(work 1e9)，tip = 101。
	state := func() *ltypes.BtcChainState {
		return &ltypes.BtcChainState{Nodes: []*ltypes.BtcChainNode{
			{Height: 100, Hash: "hash-100", Work: big.NewInt(1000).Bytes()},
			{Height: 101, Hash: "hash-101", Work: big.NewInt(1e9).Bytes()},
		}}
	}
	header := func(height uint64, prevHash string, bits int64) *ltypes.BtcHeader {
		return &ltypes.BtcHeader{
			Height: height, Hash: "hash-" + prevHash + "-" + big.NewInt(int64(height)).String(),
			PreviousHash: prevHash, Bits: bits,
		}
	}

	t.Run("bootstrap defines the chain", func(t *testing.T) {
		plan, err := planBtcHeaders(&ltypes.BtcChainState{}, []*ltypes.BtcHeader{
			header(5, "anchor", regtestBits),
			header(6, "hash-anchor-5", regtestBits),
		})
		require.NoError(t, err)
		require.True(t, plan.switched)
		require.EqualValues(t, 4, plan.forkHeight)
		require.Equal(t, []uint64{5, 6}, chainHeights(plan.state))
	})

	t.Run("extension switches", func(t *testing.T) {
		plan, err := planBtcHeaders(state(), []*ltypes.BtcHeader{header(102, "hash-101", regtestBits)})
		require.NoError(t, err)
		require.True(t, plan.switched)
		require.EqualValues(t, 101, plan.forkHeight)
		require.Equal(t, []uint64{100, 101, 102}, chainHeights(plan.state))
	})

	t.Run("shorter but heavier branch wins", func(t *testing.T) {
		// 从高度 100 分叉，只有一个头，但它的工作量远大于 canonical 在 101 上的工作量。
		plan, err := planBtcHeaders(state(), []*ltypes.BtcHeader{header(101, "hash-100", int64(chaincfg.MainNetParams.PowLimitBits))})
		require.NoError(t, err)
		require.True(t, plan.switched, "更短但更重的分支必须胜出")
		require.EqualValues(t, 100, plan.forkHeight)
		require.Equal(t, []uint64{100, 101}, chainHeights(plan.state))
		require.Equal(t, "hash-hash-100-101", chainNodeHashes(plan.state)[101], "高度 101 必须换成新分支的头")
	})

	t.Run("longer but lighter branch loses", func(t *testing.T) {
		// 从高度 100 分叉，2 个普通块：更长，但累积工作量（1000 + 2）远小于 tip 的 1e9。
		first := header(101, "hash-100", regtestBits)
		plan, err := planBtcHeaders(state(), []*ltypes.BtcHeader{first, header(102, first.GetHash(), regtestBits)})
		require.NoError(t, err)
		require.False(t, plan.switched, "更长但更轻的分支不得切换")
		require.Equal(t, state().GetNodes(), plan.state.GetNodes())
	})

	t.Run("equal work loses", func(t *testing.T) {
		// canonical tip 的高度 101 恰好比节点 100 重一个普通块：分叉一个普通块 → 工作量相等。
		tipWork := new(big.Int).Add(big.NewInt(1000), oneBlockWork)
		tie := &ltypes.BtcChainState{Nodes: []*ltypes.BtcChainNode{
			{Height: 100, Hash: "hash-100", Work: big.NewInt(1000).Bytes()},
			{Height: 101, Hash: "hash-101", Work: tipWork.Bytes()},
		}}
		plan, err := planBtcHeaders(tie, []*ltypes.BtcHeader{header(101, "hash-100", regtestBits)})
		require.NoError(t, err)
		require.False(t, plan.switched, "工作量相等时必须保持原 canonical 链（严格更大才切换）")
	})

	t.Run("window is bounded", func(t *testing.T) {
		headers := make([]*ltypes.BtcHeader, 0, btcWorkWindowSize+10)
		prev := "hash-101"
		for i := 0; i < btcWorkWindowSize+10; i++ {
			h := header(102+uint64(i), prev, regtestBits)
			headers = append(headers, h)
			prev = h.GetHash()
		}
		plan, err := planBtcHeaders(state(), headers)
		require.NoError(t, err)
		require.Len(t, plan.state.GetNodes(), btcWorkWindowSize, "窗口不能无限增长")
		require.EqualValues(t, 101+btcWorkWindowSize+10, plan.state.GetNodes()[btcWorkWindowSize-1].GetHeight())
	})
}

// TestBtcPlanHeadersReorgDepthLimit B3 的深度上限：恰好 maxBtcReorgDepth 允许，再深一块就拒。
func TestBtcPlanHeadersReorgDepthLimit(t *testing.T) {
	tipHeight := uint64(1000)
	state := &ltypes.BtcChainState{}
	for height := tipHeight - maxBtcReorgDepth; height <= tipHeight; height++ {
		state.Nodes = append(state.Nodes, &ltypes.BtcChainNode{
			Height: height, Hash: "hash-" + big.NewInt(int64(height)).String(),
			Work: big.NewInt(int64(height)).Bytes(),
		})
	}
	forkAt := func(height uint64) *ltypes.BtcHeader {
		return &ltypes.BtcHeader{
			Height:       height + 1,
			Hash:         "fork-" + big.NewInt(int64(height)).String(),
			PreviousHash: "hash-" + big.NewInt(int64(height)).String(),
			Bits:         int64(chaincfg.RegressionNetParams.PowLimitBits),
		}
	}

	// 最深的合法分叉点：tip - maxBtcReorgDepth（工作量更轻 ⇒ 只验不切）
	plan, err := planBtcHeaders(state, []*ltypes.BtcHeader{forkAt(tipHeight - maxBtcReorgDepth)})
	require.NoError(t, err)
	require.False(t, plan.switched)

	// 再深一块：fail-closed
	_, err = planBtcHeaders(state, []*ltypes.BtcHeader{forkAt(tipHeight - maxBtcReorgDepth - 1)})
	require.Equal(t, ErrBtcReorgTooDeep, err)

	// 父块在 tip 之上（跳高，链不连续）
	gap := forkAt(tipHeight)
	gap.Height = tipHeight + 3
	_, err = planBtcHeaders(state, []*ltypes.BtcHeader{gap})
	require.Equal(t, ErrBtcHeaderDisorder, err)

	// 高度在窗口内但 hash 对不上：分叉点不是 canonical 链上的区块
	unknown := forkAt(tipHeight - 1)
	unknown.PreviousHash = "not-on-our-chain"
	_, err = planBtcHeaders(state, []*ltypes.BtcHeader{unknown})
	require.Equal(t, ErrBtcHeaderUnknownAncestor, err)
}

// TestBtcReorgSwitchCanonicalChain B3/B4 主用例：更重的分叉切换 canonical 链，逐高度头随之更新。
func TestBtcReorgSwitchCanonicalChain(t *testing.T) {
	e := newBtcReorgTester(t)

	// 1. bootstrap：canonical 链 1..5
	chainA := e.mineBootstrapChain(1, 5)
	_, log, err := e.submit(chainA)
	require.NoError(t, err)
	require.True(t, log.GetCanonicalSwitched())
	require.EqualValues(t, 0, log.GetForkHeight(), "bootstrap 的挂载点是创世高度 0")
	require.EqualValues(t, 5, e.tip().GetHeight())
	require.Equal(t, []uint64{1, 2, 3, 4, 5}, chainHeights(e.chainState()))
	for _, h := range chainA {
		require.Equal(t, h.GetHash(), e.localHeader(h.GetHeight()).GetHash())
	}

	// 2. 从高度 3 分叉出更长的 B 链（4..7）：累积工作量更大 ⇒ 切换 canonical tip
	forkPoint := chainA[2] // 高度 3
	chainB := e.mineChain(forkPoint, 4, 7)
	_, log, err = e.submit(chainB)
	require.NoError(t, err)
	require.True(t, log.GetCanonicalSwitched())
	require.EqualValues(t, 3, log.GetForkHeight())
	require.EqualValues(t, 7, e.tip().GetHeight())
	require.Equal(t, chainB[len(chainB)-1].GetHash(), e.tip().GetHash())

	// 索引窗口：1..3 是两链共有的前缀，4..7 换成 B 链
	require.Equal(t, []uint64{1, 2, 3, 4, 5, 6, 7}, chainHeights(e.chainState()))
	stateHashes := chainNodeHashes(e.chainState())
	require.Equal(t, chainA[0].GetHash(), stateHashes[1])
	require.Equal(t, forkPoint.GetHash(), stateHashes[3])
	for _, h := range chainB {
		require.Equal(t, h.GetHash(), stateHashes[h.GetHeight()])
	}

	// 3. localdb 的逐高度头必须反映新 canonical 链（否则 SPV 证明会按旧分叉校验）
	for height := uint64(4); height <= 7; height++ {
		local := e.localHeader(height)
		require.NotNil(t, local, "高度 %d 必须有 canonical 头", height)
		require.Equal(t, stateHashes[height], local.GetHash())
	}
	for _, h := range chainA[3:] { // 旧分叉的 A4、A5
		require.Nil(t, e.localHeaderByHash(h.GetHash()), "旧分叉的 hash 索引必须删除")
	}
	require.Equal(t, chainB[0].GetHash(), e.localHeaderByHash(chainB[0].GetHash()).GetHash())

	// 4. 查询路径（rgbx 校验证明走的就是 GetBtcHeader）看到的是新链
	msg, err := e.cli.Query_GetBtcHeader(&ltypes.ReqGetBtcHeader{Height: 5})
	require.NoError(t, err)
	require.Equal(t, chainB[1].GetHash(), msg.(*ltypes.BtcHeader).GetHash())
	// 旧分叉上的证明：证明里的 blockHash = A5，而高度 5 的头已经是 B5 ⇒ 下游比对失败（ErrInvalidBtcProofBlock）
	require.NotEqual(t, chainA[4].GetHash(), msg.(*ltypes.BtcHeader).GetHash())
	_, err = e.cli.Query_GetBtcHeaderByHash(&types.ReqString{Data: chainA[4].GetHash()})
	require.Equal(t, types.ErrNotFound, err)

	// 5. 回退到 A 分支：从高度 3 续挖到 14（11 块 > B 的 4 块）⇒ 重新切回 A
	chainA2 := e.mineChain(forkPoint, 4, 14)
	_, log, err = e.submit(chainA2)
	require.NoError(t, err)
	require.True(t, log.GetCanonicalSwitched())
	require.EqualValues(t, 3, log.GetForkHeight())
	require.EqualValues(t, 14, e.tip().GetHeight())
	require.Equal(t, chainA2[len(chainA2)-1].GetHash(), e.tip().GetHash())
	for height := uint64(4); height <= 14; height++ {
		require.Equal(t, chainA2[height-4].GetHash(), e.localHeader(height).GetHash())
	}
	for _, h := range chainB {
		require.Nil(t, e.localHeaderByHash(h.GetHash()), "回退后 B 链的 hash 索引必须清掉")
	}
	require.Nil(t, e.localHeader(15), "新 tip 之上不能残留旧链的头")
}

// TestBtcReorgLighterForkAcceptedButNotSwitched 更轻的分叉：交易成功，但 canonical 链与 localdb 都不动。
func TestBtcReorgLighterForkAcceptedButNotSwitched(t *testing.T) {
	e := newBtcReorgTester(t)

	chainA := e.mineBootstrapChain(1, 6)
	_, _, err := e.submit(chainA)
	require.NoError(t, err)
	require.EqualValues(t, 6, e.tip().GetHeight())

	// 从高度 2 分叉出 2 块（总工作量少于 canonical 的 6 块）
	lighter := e.mineChain(chainA[1], 3, 4)
	_, log, err := e.submit(lighter)
	require.NoError(t, err, "合法但更轻的分叉应当被接受（只是不切 canonical）")
	require.False(t, log.GetCanonicalSwitched())
	require.EqualValues(t, 2, log.GetForkHeight())
	require.EqualValues(t, 6, e.tip().GetHeight())
	require.Equal(t, chainA[5].GetHash(), e.tip().GetHash())
	require.Equal(t, []uint64{1, 2, 3, 4, 5, 6}, chainHeights(e.chainState()))

	// localdb 完全没动：高度 3..6 仍是 A 链
	for height := uint64(3); height <= 6; height++ {
		require.Equal(t, chainA[height-1].GetHash(), e.localHeader(height).GetHash())
	}
	for _, h := range lighter {
		require.Nil(t, e.localHeaderByHash(h.GetHash()), "更轻的分叉不得写入 canonical 索引")
	}
}

// TestBtcReorgRejectedByCheckTx 深度超限 / 父块未知：CheckTx 阶段就拒（fail-closed）。
func TestBtcReorgRejectedByCheckTx(t *testing.T) {
	e := newBtcReorgTester(t)

	chainA := e.mineBootstrapChain(1, 6)
	_, _, err := e.submit(chainA)
	require.NoError(t, err)

	t.Run("unknown ancestor", func(t *testing.T) {
		// 父块高度落在窗口内、hash 却不是 canonical 链上的区块：真机上"中继自己的 tip 与链上不一致"
		// 就是这个形态（父子高度对得上、hash 对不上），显式报 ErrBtcHeaderUnknownAncestor。
		bad := &ltypes.BtcHeader{Height: 7, Hash: "fake-7", PreviousHash: "not-our-tip"}
		_, _, err := e.submit([]*ltypes.BtcHeader{bad})
		require.Equal(t, ErrBtcHeaderUnknownAncestor, err)
	})

	t.Run("gap above tip", func(t *testing.T) {
		gap := &ltypes.BtcHeader{Height: 9, Hash: "fake-9", PreviousHash: chainA[5].GetHash()}
		_, _, err := e.submit([]*ltypes.BtcHeader{gap})
		require.Equal(t, ErrBtcHeaderDisorder, err)
	})

	t.Run("depth boundary: allowed step is not rejected as too deep", func(t *testing.T) {
		// 把链推到 tip = 5 + maxBtcReorgDepth，窗口最低点 = 6。
		extra := e.mineChain(chainA[5], 7, 5+maxBtcReorgDepth)
		_, _, err := e.submit(extra)
		require.NoError(t, err)
		tip := e.tip()
		require.EqualValues(t, 5+maxBtcReorgDepth, tip.GetHeight())

		lowest := e.chainState().GetNodes()[0]
		require.EqualValues(t, tip.GetHeight()-maxBtcReorgDepth, lowest.GetHeight(),
			"窗口最低点应为 tip - maxBtcReorgDepth")

		// 从窗口最低点分叉：深度正好等于上限，必须放行（这里更轻 ⇒ 只验不切）
		boundary := e.mineChain(chainA[4], 6, 6)
		_, log, err := e.submit(boundary)
		require.NoError(t, err)
		require.False(t, log.GetCanonicalSwitched())

		// 再深一块（挂到窗口最低点之下的高度 4）：拒
		tooDeep := &ltypes.BtcHeader{Height: 5, Hash: "fake-5", PreviousHash: chainA[3].GetHash()}
		_, _, err = e.submit([]*ltypes.BtcHeader{tooDeep})
		require.Equal(t, ErrBtcReorgTooDeep, err)
	})
}

// TestBtcChainStateLegacyRebuild 历史数据兼容：statedb 里只有 btc-lastheader（B3/B4 之前的布局），
// 窗口要用 localdb 的逐高度头回溯重建，升级后第一个批头不能被"父块不在窗口里"卡死。
func TestBtcChainStateLegacyRebuild(t *testing.T) {
	e := newBtcReorgTester(t)

	chainA := e.mineBootstrapChain(1, 5)
	// 只写 tip + localdb 的逐高度头（老版本的写入方式），不写 btc-chainstate
	require.NoError(t, e.stateDB.Set(btcLastHeaderKey(), types.Encode(chainA[4])))
	applyKV(t, e.localDB, appendBtcHeadersKV(nil, chainA))

	next := mineBtcHeader(t, chainA[4], 6, btcTestBits, e.nextTimestamp())
	_, log, err := e.submit([]*ltypes.BtcHeader{next})
	require.NoError(t, err, "老数据必须能继续扩展，否则升级即卡死")
	require.True(t, log.GetCanonicalSwitched())
	require.EqualValues(t, 5, log.GetForkHeight())

	// 重建出来的窗口覆盖 localdb 能回溯到的高度（1..6）
	require.Equal(t, []uint64{1, 2, 3, 4, 5, 6}, chainHeights(e.chainState()))
	require.Equal(t, next.GetHash(), e.tip().GetHash())
}

// TestBtcChainStateRebuildStopsAtGap localdb 断链时重建必须停下；退化成"只有 tip"时扩展仍可用、回退被拒。
func TestBtcChainStateRebuildStopsAtGap(t *testing.T) {
	e := newBtcReorgTester(t)

	h1 := mineBtcHeaderFrom(t, chaincfg.RegressionNetParams.GenesisHash.String(), 1, btcTestBits, e.nextTimestamp())
	h2 := mineBtcHeader(t, h1, 2, btcTestBits, e.nextTimestamp())
	h3 := mineBtcHeader(t, h2, 3, btcTestBits, e.nextTimestamp())

	// localdb 只留 h3（缺 h2/h1）
	require.NoError(t, e.localDB.Set(btcHeaderKey(h3.GetHeight()), types.Encode(h3)))
	require.NoError(t, e.stateDB.Set(btcLastHeaderKey(), types.Encode(h3)))

	state, err := loadBtcChainState(e.stateDB, e.localDB)
	require.NoError(t, err)
	require.Equal(t, []uint64{3}, chainHeights(state))

	// 只有 tip 一个节点：扩展可用
	next := mineBtcHeader(t, h3, 4, btcTestBits, e.nextTimestamp())
	plan, err := planBtcHeaders(state, []*ltypes.BtcHeader{next})
	require.NoError(t, err)
	require.True(t, plan.switched)

	// 回退到高度 3 的另一个头（父块是高度 2）：挂载点不在窗口里 ⇒ 拒，不会按错误的祖先校验
	altH3 := mineBtcHeader(t, h2, 3, btcTestBits, e.nextTimestamp())
	_, err = planBtcHeaders(state, []*ltypes.BtcHeader{altH3})
	require.Equal(t, ErrBtcHeaderUnknownAncestor, err)
}

// TestBtcExecLocalLegacyLog 历史区块重放：老回执日志里没有 B3/B4 字段，
// ExecLocal 必须沿用旧行为（按高度写入本批头），不能按"未切换"处理而漏写。
func TestBtcExecLocalLegacyLog(t *testing.T) {
	e := newBtcReorgTester(t)

	h1 := mineBtcHeaderFrom(t, chaincfg.RegressionNetParams.GenesisHash.String(), 1, btcTestBits, e.nextTimestamp())
	h2 := mineBtcHeader(t, h1, 2, btcTestBits, e.nextTimestamp())
	headers := []*ltypes.BtcHeader{h1, h2}
	tx := buildCheckTx(t, &ltypes.BtcHeaders{Headers: headers}, e.commitPriv)

	legacyLog := &ltypes.BtcHeadersLog{CommitHeight: 2, CommitHash: h2.GetHash()}
	receiptData := &types.ReceiptData{Logs: []*types.ReceiptLog{
		{Ty: ltypes.TyBtcHeadersLog, Log: types.Encode(legacyLog)},
	}}
	dbSet, err := e.cli.ExecLocal_BtcHeaders(&ltypes.BtcHeaders{Headers: headers}, tx, receiptData, 0)
	require.NoError(t, err)
	require.True(t, hasKV(dbSet.GetKV(), btcHeaderKey(h1.GetHeight())))
	require.True(t, hasKV(dbSet.GetKV(), btcHeaderKey(h2.GetHeight())))

	// 新日志 + 未切换：一条 localdb kv 都不该有（连删除也不该有）
	newLog := &ltypes.BtcHeadersLog{
		LastHeight: 2, ForkHeight: 1, CanonicalSwitched: false,
		TipWork: big.NewInt(1).Bytes(), ReorgRuleVersion: btcReorgRuleVersion,
	}
	receiptData = &types.ReceiptData{Logs: []*types.ReceiptLog{
		{Ty: ltypes.TyBtcHeadersLog, Log: types.Encode(newLog)},
	}}
	dbSet, err = e.cli.ExecLocal_BtcHeaders(&ltypes.BtcHeaders{Headers: headers}, tx, receiptData, 0)
	require.NoError(t, err)
	require.Empty(t, dbSet.GetKV())
}
