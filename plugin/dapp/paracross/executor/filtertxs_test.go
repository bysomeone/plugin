// Copyright Fuzamei Corp. 2018 All Rights Reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package executor

import (
	"encoding/hex"
	"strings"
	"testing"

	chainlog "github.com/33cn/chain33/common/log/log15"
	"github.com/33cn/chain33/types"
	pt "github.com/33cn/plugin/plugin/dapp/paracross/types"
	"github.com/stretchr/testify/require"
)

// mainCfgWithDiscardTxs 构造主链(非平行链)配置, withKey=false 时不写 discardTxs,
// 与 chain33.toml 的现状一致, withKey=true 时写显式列表
func mainCfgWithDiscardTxs(withKey bool, discard ...string) *types.Chain33Config {
	cfgStr := strings.Replace(types.GetDefaultCfgstring(), `Title="local"`, `Title="test"`, 1)
	if withKey {
		quoted := make([]string, 0, len(discard))
		for _, d := range discard {
			quoted = append(quoted, `"`+d+`"`)
		}
		cfgStr += "\n[exec.sub.paracross]\ndiscardTxs=[" + strings.Join(quoted, ",") + "]\n"
	}
	return types.NewChain33Config(cfgStr)
}

// captureLogs 替换 log15 root handler, 收集 fn 执行期间的全部日志
func captureLogs(fn func()) []*chainlog.Record {
	var records []*chainlog.Record
	prev := chainlog.Root().GetHandler()
	chainlog.Root().SetHandler(chainlog.FuncHandler(int(chainlog.LvlDebug), func(r *chainlog.Record) error {
		records = append(records, r)
		return nil
	}))
	defer chainlog.Root().SetHandler(prev)
	fn()
	return records
}

// discardTestDetail 三笔交易: paracross commit, 跨链资产转移, 普通平行链交易
func discardTestDetail(t *testing.T, cfg *types.Chain33Config) *types.BlockDetail {
	t.Helper()
	return newIndexTestDetail(
		newIndexTestTxNonce(t, cfg, indexTestExec, 0, 1),
		newIndexTestTxNonce(t, cfg, indexTestExec, int32(pt.ParacrossActionAssetTransfer), 2),
		newIndexTestTxNonce(t, cfg, indexTestParaTx, 0, 3),
	)
}

func filterTxsForTest(t *testing.T, cfg *types.Chain33Config, detail *types.BlockDetail) []*types.Transaction {
	t.Helper()
	return FilterTxsForPara(cfg, detail.FilterParaTxsByTitle(cfg, indexTestTitle))
}

// TestFilterTxsForParaMissingDiscardTxsNoErrorLog 缺省不配置 discardTxs 时不能打 error 日志,
// 否则主链节点每执行一次都会往日志里写一条 "chain config ... not found"
func TestFilterTxsForParaMissingDiscardTxsNoErrorLog(t *testing.T) {
	cfg := mainCfgWithDiscardTxs(false)
	detail := discardTestDetail(t, cfg)

	var txs []*types.Transaction
	records := captureLogs(func() {
		txs = filterTxsForTest(t, cfg, detail)
	})
	require.NotEmpty(t, txs, "filter must still keep the para txs")
	for _, r := range records {
		require.False(t, r.Lvl == chainlog.LvlError && strings.Contains(r.Msg, "discardTxs"),
			"absent optional config must not be logged as error: %s", r.Msg)
	}
}

// TestFilterTxsForParaMissingDiscardTxsEqualsEmptyList 缺省与显式空列表必须完全等价
func TestFilterTxsForParaMissingDiscardTxsEqualsEmptyList(t *testing.T) {
	absentCfg := mainCfgWithDiscardTxs(false)
	emptyCfg := mainCfgWithDiscardTxs(true)
	detail := discardTestDetail(t, absentCfg)

	absent := filterTxsForTest(t, absentCfg, detail)
	empty := filterTxsForTest(t, emptyCfg, detail)
	require.NotEmpty(t, absent)
	require.Equal(t, len(empty), len(absent))
	for i := range absent {
		require.Equal(t, hex.EncodeToString(empty[i].Hash()), hex.EncodeToString(absent[i].Hash()))
	}
}

// TestFilterTxsForParaDiscardsConfiguredTx 配了非空列表时, 命中的交易必须被丢弃
func TestFilterTxsForParaDiscardsConfiguredTx(t *testing.T) {
	baseCfg := mainCfgWithDiscardTxs(false)
	detail := discardTestDetail(t, baseCfg)
	base := filterTxsForTest(t, baseCfg, detail)
	require.Equal(t, 3, len(base))

	discardHash := hex.EncodeToString(detail.Block.Txs[1].Hash())
	for _, item := range []string{discardHash, "0x" + discardHash} {
		cfg := mainCfgWithDiscardTxs(true, item)
		filtered := filterTxsForTest(t, cfg, detail)
		require.Equal(t, len(base)-1, len(filtered), "discardTxs=%s", item)
		for _, tx := range filtered {
			require.NotEqual(t, discardHash, hex.EncodeToString(tx.Hash()))
		}
	}
}
