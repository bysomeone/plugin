package executor

import (
	"strings"
	"testing"

	ltypes "github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

/*
 * 锚点方案 A（btcCheckpointTable 取自 btcd chaincfg）+ 扩展层 + bootstrap 拒绝时的可操作信息（L1）
 * + Query_GetBtcCheckpoint（L2 的执行器侧）的测试。
 */

// cpHash 造一个 chainhash。
func cpHash(t *testing.T, s string) *chainhash.Hash {
	t.Helper()
	h, err := chainhash.NewHashFromStr(s)
	require.NoError(t, err)
	return h
}

// restateChaincfg 按扩展层的规则 1 把 chaincfg 的锚点完整重述一遍（= 写 extraBtcCheckpoints 的写法：
// 全集声明，再追加比最高锚点更高的条目）。
func restateChaincfg(p *chaincfg.Params) map[uint64]string {
	out := make(map[uint64]string, len(p.Checkpoints))
	for _, cp := range p.Checkpoints {
		out[uint64(cp.Height)] = cp.Hash.String()
	}
	return out
}

// TestBtcCheckpointTableGolden 黄金锚点表：钉住各网络"最后一条锚点的高度 + hash"（以及条数）。
//
// 这是**升级 btcd 的红灯**：锚点集合被依赖地改变了（新增/移除/换 hash），这个用例必须变红——
// 它意味着执行器与中继（neutrino 把自己的头链硬锚在 btcd 的 checkpoint 上）的锚点集合一起变了，
// 需要显式确认"所有节点同 build、btcHeaderStartHeight 是否要跟着调"（见 CONFIG.md 2.1.1）。
// 变更这张表时请同步更新本用例里的常量，并说明理由。
func TestBtcCheckpointTableGolden(t *testing.T) {
	tests := []struct {
		netName  string
		count    int
		last     uint64
		lastHash string
	}{
		{
			netName:  "mainnet",
			count:    35,
			last:     810000,
			lastHash: "000000000000000000028028ca82b6aa81ce789e4eb9e0321b74c3cbaf405dd1",
		},
		{
			netName:  "testnet3",
			count:    21,
			last:     2344474,
			lastHash: "0000000000000004877fa2d36316398528de4f347df2f8a96f76613a298ce060",
		},
		// 以下网络 chaincfg 没有内置锚点（regtest 链每次启动都会重建，任何网络都不该给它填锚点）。
		{netName: "testnet4", count: 0},
		{netName: "regtest", count: 0},
		{netName: "simnet", count: 0},
		{netName: "signet", count: 0},
	}

	for _, tc := range tests {
		t.Run(tc.netName, func(t *testing.T) {
			params := ltypes.GetBtcChainParams(tc.netName)
			got := btcCheckpointTable[params.Net]
			require.Len(t, got, tc.count, "锚点条数变化 = btcd 锚点集合被依赖地改了（%s）", tc.netName)
			if tc.count == 0 {
				return
			}
			require.Len(t, params.Checkpoints, tc.count, "表必须与 chaincfg 完全一致")
			height, hash, ok := highestCheckpoint(got)
			require.True(t, ok)
			require.EqualValues(t, tc.last, height)
			require.Equal(t, tc.lastHash, hash)
			// 最高锚点 = chaincfg 的最后一条（方案 A：表就是 chaincfg 转出来的）。
			last := params.Checkpoints[len(params.Checkpoints)-1]
			require.EqualValues(t, last.Height, height)
			require.Equal(t, last.Hash.String(), hash)
		})
	}
}

// TestBtcCheckpointTableMatchesChaincfg 表里的每一项都必须与 chaincfg 同高度同 hash
// （方案 A 的"表 = chaincfg 转换"这一不变量，逐项校验，而不只看最后一条）。
func TestBtcCheckpointTableMatchesChaincfg(t *testing.T) {
	for _, name := range btcCheckpointNetNames {
		params := ltypes.GetBtcChainParams(name)
		table := btcCheckpointTable[params.Net]
		require.Len(t, table, len(params.Checkpoints), "网络 %s", name)
		for _, cp := range params.Checkpoints {
			require.Equal(t, cp.Hash.String(), table[uint64(cp.Height)], "网络 %s 高度 %d", name, cp.Height)
		}
	}
}

// TestBtcCheckpointNetNamesCoverConfiguredNets 守住"锚点表与中继同源"的前提：
// btcCheckpointNetNames 里的每个网络名都必须能被 ltypes.GetBtcChainParams 解析成不同的
// wire.BitcoinNet，且都已在 btcCheckpointTable 里（新增网络名忘了加进列表 = 该网络没有锚点约束）。
func TestBtcCheckpointNetNamesCoverConfiguredNets(t *testing.T) {
	seen := make(map[wire.BitcoinNet]string, len(btcCheckpointNetNames))
	for _, name := range btcCheckpointNetNames {
		net := ltypes.GetBtcChainParams(name).Net
		if prev, dup := seen[net]; dup {
			t.Fatalf("网络名 %s 与 %s 解析到同一个 wire.BitcoinNet %v", name, prev, net)
		}
		seen[net] = name
		_, ok := btcCheckpointTable[net]
		require.True(t, ok, "网络 %s（%v）没有在 btcCheckpointTable 里", name, net)
	}
}

// TestBuildBtcCheckpointTable 扩展层的三条规则（含各类违反），以及"合法扩展被合入"的正例。
func TestBuildBtcCheckpointTable(t *testing.T) {
	const (
		cp100 = "1111111111111111111111111111111111111111111111111111111111111111"
		cp200 = "2222222222222222222222222222222222222222222222222222222222222222"
		cp300 = "3333333333333333333333333333333333333333333333333333333333333333"
	)
	params := &chaincfg.Params{
		Name: "fakenet",
		Checkpoints: []chaincfg.Checkpoint{
			{Height: 100, Hash: cpHash(t, cp100)},
			{Height: 200, Hash: cpHash(t, cp200)},
		},
	}
	bare := &chaincfg.Params{Name: "baretnet"}

	t.Run("no extension keeps chaincfg as is", func(t *testing.T) {
		table, err := buildBtcCheckpointTable(params, nil)
		require.NoError(t, err)
		require.Equal(t, map[uint64]string{100: cp100, 200: cp200}, table)
	})

	t.Run("valid extension above the highest anchor is merged", func(t *testing.T) {
		extra := map[uint64]string{100: cp100, 200: cp200, 300: cp300}
		table, err := buildBtcCheckpointTable(params, extra)
		require.NoError(t, err)
		require.Equal(t, map[uint64]string{100: cp100, 200: cp200, 300: cp300}, table)
	})

	t.Run("missing a chaincfg entry is rejected", func(t *testing.T) {
		_, err := buildBtcCheckpointTable(params, map[uint64]string{200: cp200, 300: cp300})
		require.ErrorContains(t, err, "must restate every chaincfg anchor")
	})

	t.Run("changing a chaincfg entry is rejected", func(t *testing.T) {
		_, err := buildBtcCheckpointTable(params, map[uint64]string{100: cp300, 200: cp200, 300: cp300})
		require.ErrorContains(t, err, "changes the chaincfg anchor at height 100")
	})

	t.Run("adding a height below the highest anchor is rejected", func(t *testing.T) {
		_, err := buildBtcCheckpointTable(params, map[uint64]string{100: cp100, 200: cp200, 150: cp300})
		require.ErrorContains(t, err, "not above the highest chaincfg anchor 200")
	})

	t.Run("extending a net without chaincfg anchors is rejected", func(t *testing.T) {
		_, err := buildBtcCheckpointTable(bare, map[uint64]string{300: cp300})
		require.ErrorContains(t, err, "has no built-in checkpoint")
	})

	t.Run("rejecting a chaincfg checkpoint with a nil hash", func(t *testing.T) {
		_, err := buildBtcCheckpointTable(&chaincfg.Params{
			Name:        "nilhashnet",
			Checkpoints: []chaincfg.Checkpoint{{Height: 100}},
		}, nil)
		require.ErrorContains(t, err, "nil hash")
	})
}

// TestBtcCheckpointTableExtensionIsApplied 走一遍 init() 的填表路径：扩展层被合入主网络的表，
// 其他网络不受影响，且 Query_GetBtcCheckpoint 返回的是**扩展后**的最高锚点（L2 的执行器侧）。
func TestBtcCheckpointTableExtensionIsApplied(t *testing.T) {
	// 还原全局表：扩展层是编译期常量（当前为空），测完必须把表恢复成生产的样子，否则会污染其他用例。
	defer func() { require.NoError(t, reloadBtcCheckpointTable(extraBtcCheckpoints)) }()

	extraHash := strings.Repeat("ab", 32)
	extraHeight := uint64(chaincfg.MainNetParams.Checkpoints[len(chaincfg.MainNetParams.Checkpoints)-1].Height) + 1000
	extra := restateChaincfg(&chaincfg.MainNetParams)
	extra[extraHeight] = extraHash

	require.NoError(t, reloadBtcCheckpointTable(map[wire.BitcoinNet]map[uint64]string{wire.MainNet: extra}))

	mainTable := btcCheckpointTable[wire.MainNet]
	require.Len(t, mainTable, len(chaincfg.MainNetParams.Checkpoints)+1)
	height, hash, ok := highestCheckpoint(mainTable)
	require.True(t, ok)
	require.Equal(t, extraHeight, height)
	require.Equal(t, extraHash, hash)
	// 其余网络照旧（扩展层只影响声明的网络）。
	require.Len(t, btcCheckpointTable[wire.TestNet3], len(chaincfg.TestNet3Params.Checkpoints))

	cli := newLightclient().(*lightclient)
	oldNet := lightCfg.BtcNetName
	defer func() { lightCfg.BtcNetName = oldNet }()

	lightCfg.BtcNetName = "mainnet"
	msg, err := cli.Query_GetBtcCheckpoint(nil)
	require.NoError(t, err)
	anchor, ok := msg.(*ltypes.BtcHeader)
	require.True(t, ok)
	require.Equal(t, extraHeight, anchor.GetHeight())
	require.Equal(t, extraHash, anchor.GetHash())
}

// TestQueryGetBtcCheckpoint 查询返回本网络最高锚点；没有锚点的网络返回高度 0 的空头
// （中继据此跳过 L2 断言，不做硬依赖）。
func TestQueryGetBtcCheckpoint(t *testing.T) {
	cli := newLightclient().(*lightclient)
	oldNet := lightCfg.BtcNetName
	defer func() { lightCfg.BtcNetName = oldNet }()

	lightCfg.BtcNetName = "mainnet"
	msg, err := cli.Query_GetBtcCheckpoint(nil)
	require.NoError(t, err)
	anchor, ok := msg.(*ltypes.BtcHeader)
	require.True(t, ok)
	mainLast := chaincfg.MainNetParams.Checkpoints[len(chaincfg.MainNetParams.Checkpoints)-1]
	require.EqualValues(t, mainLast.Height, anchor.GetHeight())
	require.Equal(t, mainLast.Hash.String(), anchor.GetHash())

	lightCfg.BtcNetName = "regtest"
	msg, err = cli.Query_GetBtcCheckpoint(nil)
	require.NoError(t, err)
	anchor, ok = msg.(*ltypes.BtcHeader)
	require.True(t, ok)
	require.Zero(t, anchor.GetHeight())
	require.Empty(t, anchor.GetHash())
}
