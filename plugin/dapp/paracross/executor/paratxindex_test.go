// Copyright Fuzamei Corp. 2018 All Rights Reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package executor

import (
	"bytes"
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/33cn/chain33/client/mocks"
	"github.com/33cn/chain33/common/address"
	"github.com/33cn/chain33/types"
	"github.com/33cn/plugin/plugin/dapp/paracross/testnode"
	pt "github.com/33cn/plugin/plugin/dapp/paracross/types"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const (
	indexTestTitle   = "user.p.test."
	indexTestHeight  = int64(100)
	indexTestExec    = "user.p.test.paracross"
	indexTestParaTx  = "user.p.test.coins"
	indexTestAddress = "1KSBd17H7ZK8iT37aJztFB22XGwsPTdwE4"
)

// newIndexTestTx 构造一笔交易, ty为跨链action类型
func newIndexTestTx(t *testing.T, cfg *types.Chain33Config, execer string, ty int32) *types.Transaction {
	return newIndexTestTxNonce(t, cfg, execer, ty, 0)
}

// newIndexTestTxNonce 同上, nonce用于区分同一区块内的重复交易
func newIndexTestTxNonce(t *testing.T, cfg *types.Chain33Config, execer string, ty int32, nonce int64) *types.Transaction {
	payload := &pt.ParacrossAction{Ty: ty}
	switch ty {
	case int32(pt.ParacrossActionAssetTransfer):
		payload.Value = &pt.ParacrossAction_AssetTransfer{AssetTransfer: &types.AssetsTransfer{
			Cointoken: "test", Amount: 10, To: indexTestAddress, Note: []byte("transfer")}}
	case int32(pt.ParacrossActionAssetWithdraw):
		payload.Value = &pt.ParacrossAction_AssetWithdraw{AssetWithdraw: &types.AssetsWithdraw{
			Cointoken: "test", Amount: 11, To: indexTestAddress}}
	case int32(pt.ParacrossActionCrossAssetTransfer):
		payload.Value = &pt.ParacrossAction_CrossAssetTransfer{CrossAssetTransfer: &pt.CrossAssetTransfer{
			AssetExec: "coins", AssetSymbol: "bty", Amount: 12, ToAddr: indexTestAddress}}
	default:
		payload.Value = &pt.ParacrossAction_Commit{Commit: &pt.ParacrossCommitAction{
			Status: &pt.ParacrossNodeStatus{Title: indexTestTitle, Height: 1}}}
	}
	tx := &types.Transaction{Execer: []byte(execer), Payload: types.Encode(payload), Nonce: nonce}
	tx.To = address.ExecAddress(execer)
	require.NotEmpty(t, tx.Hash())
	return tx
}

// newIndexTestDetail 构造区块, 收据全部ExecOk
func newIndexTestDetail(txs ...*types.Transaction) *types.BlockDetail {
	receipts := make([]*types.ReceiptData, len(txs))
	for i := range receipts {
		receipts[i] = &types.ReceiptData{Ty: types.ExecOk}
	}
	return &types.BlockDetail{
		Block:    &types.Block{Height: indexTestHeight, Txs: txs},
		Receipts: receipts,
	}
}

// newIndexTestStatus 构造commit状态, bitMapBits为平行链提交的跨链结果位图
func newIndexTestStatus(cfg *types.Chain33Config, detail *types.BlockDetail, bitMapBits ...uint) *pt.ParacrossNodeStatus {
	bitMap := big.NewInt(0)
	for _, bit := range bitMapBits {
		bitMap.SetBit(bitMap, int(bit), 1)
	}
	return &pt.ParacrossNodeStatus{
		Title:           indexTestTitle,
		Height:          1,
		MainBlockHeight: detail.Block.Height,
		MainBlockHash:   detail.Block.Hash(cfg),
		TxResult:        []byte(hex.EncodeToString(bitMap.Bytes())),
		// 长度不等于ParaCrossStatusBitMapVerLen, 否则会走"老版本平行链"的提前返回
		CrossTxResult: []byte("01"),
	}
}

// newIndexTestAPI 构造mock, 同时提供区块体和索引
func newIndexTestAPI(t *testing.T, cfg *types.Chain33Config, detail *types.BlockDetail,
	index *types.HeightParaIndex, indexErr error) *mocks.QueueProtocolAPI {
	api := &mocks.QueueProtocolAPI{}
	api.On("GetConfig").Return(cfg)
	api.On("GetBlockByHashes", mock.Anything).Return(
		&types.BlockDetails{Items: []*types.BlockDetail{detail}}, nil)
	if indexErr != nil {
		api.On("GetParaTxIndex", mock.Anything).Return(nil, indexErr)
	} else {
		api.On("GetParaTxIndex", mock.Anything).Return(index, nil)
	}
	return api
}

// requireSameCrossTxs 断言索引路径和区块体路径得到相同的跨链交易序列
func requireSameCrossTxs(t *testing.T, indexTxs []*paraCrossTx, blockTxs []*types.Transaction) {
	require.Equal(t, len(blockTxs), len(indexTxs), "cross tx count not equal")
	for i := range blockTxs {
		item := indexTxs[i]
		if item.noAction {
			//索引跳过的交易必须确实不需要执行
			require.False(t, isParaCrossAssetAction(item.ty),
				"tx %d skipped but ty %d needs execution", i, item.ty)
			continue
		}
		require.True(t, bytes.Equal(types.Encode(blockTxs[i]), types.Encode(item.tx)),
			"tx %d not byte equal", i)
	}
}

// TestParaTxIndexEquivalence 索引路径和区块体路径必须得到完全相同的(crossTxs, crossRst)
func TestParaTxIndexEquivalence(t *testing.T) {
	cfg := chain33TestMainCfg

	cases := []struct {
		name string
		txs  []*types.Transaction
		bits []uint
		// 位图是否应该置位, 没有跨链交易时位图必然为空
		wantBit bool
	}{
		{
			name: "with asset transfer",
			txs: []*types.Transaction{
				newIndexTestTxNonce(t, cfg, indexTestExec, 0, 1),
				newIndexTestTxNonce(t, cfg, indexTestExec, int32(pt.ParacrossActionAssetTransfer), 2),
				newIndexTestTxNonce(t, cfg, indexTestParaTx, 0, 3),
				newIndexTestTxNonce(t, cfg, indexTestExec, int32(pt.ParacrossActionCrossAssetTransfer), 4),
				newIndexTestTxNonce(t, cfg, indexTestExec, int32(pt.ParacrossActionAssetWithdraw), 5),
			},
			// 位图覆盖索引元素: 0=commit, 1=para tx, 2,3,4=资产转移
			bits:    []uint{0, 2, 3, 4},
			wantBit: true,
		},
		{
			name: "without asset transfer",
			txs: []*types.Transaction{
				newIndexTestTxNonce(t, cfg, indexTestExec, 0, 1),
				newIndexTestTxNonce(t, cfg, indexTestParaTx, 0, 2),
				newIndexTestTxNonce(t, cfg, indexTestExec, 0, 3),
			},
			bits:    []uint{0, 1, 2},
			wantBit: true,
		},
		{
			name: "without paracross tx",
			txs: []*types.Transaction{
				newIndexTestTxNonce(t, cfg, indexTestParaTx, 0, 1),
				newIndexTestTxNonce(t, cfg, indexTestParaTx, 0, 2),
			},
			bits: []uint{0, 1},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			detail := newIndexTestDetail(tc.txs...)
			status := newIndexTestStatus(cfg, detail, tc.bits...)

			// 区块体路径
			blockTxs, blockRst, err := getCrossTxsByRst(apiWithConfigOnly(cfg, detail), status)
			require.NoError(t, err)

			// 索引路径
			indexes, err := buildParaTxIndex(cfg, detail)
			require.NoError(t, err)
			index := findParaTxIndex(t, indexes, indexTestTitle)
			require.NotNil(t, index, "index should be built")
			require.Equal(t, status.MainBlockHash, index.GetHash())
			require.Equal(t, uint32(len(FilterTxsForPara(cfg, detail.FilterParaTxsByTitle(cfg, indexTestTitle)))),
				index.GetBaseHashCount())

			indexTxs, indexRst, done, err := getCrossTxsFromIndex(
				newIndexTestAPI(t, cfg, detail, index, nil), status)
			require.NoError(t, err)
			require.True(t, done)

			require.Equal(t, len(blockTxs), len(indexTxs))
			requireSameCrossTxs(t, indexTxs, blockTxs)
			// 位图逐字节相同
			require.True(t, bytes.Equal(blockRst, indexRst),
				"bitmap not equal, block %x index %x", blockRst, indexRst)
			// 位图确实有置位, 否则上面的比较没有意义
			require.Equal(t, tc.wantBit, new(big.Int).SetBytes(indexRst).Sign() > 0,
				"index bitmap %x", indexRst)
		})
	}
}

// apiWithConfigOnly 只提供区块体的mock, 用于构造区块体路径的期望结果
func apiWithConfigOnly(cfg *types.Chain33Config, detail *types.BlockDetail) *mocks.QueueProtocolAPI {
	api := &mocks.QueueProtocolAPI{}
	api.On("GetConfig").Return(cfg)
	api.On("GetBlockByHashes", mock.Anything).Return(
		&types.BlockDetails{Items: []*types.BlockDetail{detail}}, nil)
	return api
}

func findParaTxIndex(t *testing.T, indexes []*types.HeightParaIndex, title string) *types.HeightParaIndex {
	for _, index := range indexes {
		if index.GetTitle() == title {
			return index
		}
	}
	return nil
}

// TestParaTxIndexFallback 索引不可用时必须回退到区块体路径, 且不能返回错误
func TestParaTxIndexFallback(t *testing.T) {
	cfg := chain33TestMainCfg
	txs := []*types.Transaction{
		newIndexTestTxNonce(t, cfg, indexTestExec, 0, 1),
		newIndexTestTxNonce(t, cfg, indexTestExec, int32(pt.ParacrossActionAssetTransfer), 2),
		newIndexTestTxNonce(t, cfg, indexTestParaTx, 0, 3),
	}
	detail := newIndexTestDetail(txs...)
	status := newIndexTestStatus(cfg, detail, 0, 2)

	indexes, err := buildParaTxIndex(cfg, detail)
	require.NoError(t, err)
	index := findParaTxIndex(t, indexes, indexTestTitle)
	require.NotNil(t, index)

	// 损坏的索引: 资产转移类交易没有保留完整字节
	brokenMissPayload := &types.HeightParaIndex{
		Height: index.GetHeight(), Title: index.GetTitle(), Hash: index.GetHash(),
		BaseHashCount: index.GetBaseHashCount(),
		Items: []*types.ParaTxIndexItem{
			{BaseIndex: 0, Ty: 0},
			{BaseIndex: 1, Ty: int32(pt.ParacrossActionAssetTransfer)},
		},
	}
	// 损坏的索引: baseIndex越界
	brokenIndex := &types.HeightParaIndex{
		Height: index.GetHeight(), Title: index.GetTitle(), Hash: index.GetHash(),
		BaseHashCount: 1,
		Items:         []*types.ParaTxIndexItem{{BaseIndex: 5, Ty: 0}},
	}

	cases := []struct {
		name  string
		index *types.HeightParaIndex
		err   error
	}{
		{name: "query failed", err: types.ErrNotFound},
		{name: "empty index", index: nil},
		{name: "block hash mismatch", index: &types.HeightParaIndex{
			Height: index.GetHeight(), Title: index.GetTitle(),
			Hash: []byte("other block hash"), BaseHashCount: index.GetBaseHashCount(),
			Items: index.GetItems()}},
		{name: "title mismatch", index: &types.HeightParaIndex{
			Height: index.GetHeight(), Title: "user.p.other.", Hash: index.GetHash(),
			BaseHashCount: index.GetBaseHashCount(), Items: index.GetItems()}},
		{name: "asset tx missing payload", index: brokenMissPayload},
		{name: "base index out of range", index: brokenIndex},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newIndexTestAPI(t, cfg, detail, tc.index, tc.err)
			crossTxs, crossRst, done, err := getCrossTxsFromIndex(api, status)
			// 索引不可用不能向上抛错, 必须交给调用方回退
			require.NoError(t, err)
			require.False(t, done)
			require.Nil(t, crossTxs)
			require.Nil(t, crossRst)

			// 回退路径必须仍然得出和区块体路径一致的结果
			paraCrossTxs, expectRst, err := getCrossTxs(api, status)
			require.NoError(t, err)
			require.NotNil(t, paraCrossTxs)
			require.False(t, bytes.Equal(crossRst, expectRst))

			fallbackTxs, fallbackRst, err := getCrossTxsByRst(api, status)
			require.NoError(t, err)
			require.Equal(t, len(paraCrossTxs), len(fallbackTxs))
			require.True(t, bytes.Equal(expectRst, fallbackRst))
		})
	}
}

// TestBuildParaTxIndexSkipsUnsupportedBlocks 收据数量不匹配时不能建索引
func TestBuildParaTxIndexSkipsUnsupportedBlocks(t *testing.T) {
	cfg := chain33TestMainCfg
	detail := newIndexTestDetail(
		newIndexTestTx(t, cfg, indexTestExec, 0),
		newIndexTestTx(t, cfg, indexTestParaTx, 0),
	)
	// 收据缺失
	detail.Receipts = nil
	_, err := buildParaTxIndex(cfg, detail)
	require.Equal(t, types.ErrInvalidParam, err)

	// 区块内出现重复交易时放弃索引, 保证位图到交易的下标映射唯一
	dup := newIndexTestTxNonce(t, cfg, indexTestExec, 0, 1)
	dupDetail := newIndexTestDetail(dup.Clone(), dup.Clone())
	indexes, err := buildParaTxIndex(cfg, dupDetail)
	require.NoError(t, err)
	require.Nil(t, indexes)

	// 平行链节点不写主链索引
	paraCfg := types.NewChain33Config(testnode.DefaultConfig)
	require.True(t, paraCfg.IsPara())
	detail.Receipts = []*types.ReceiptData{{Ty: types.ExecOk}, {Ty: types.ExecOk}}
	paraIndexes, err := buildParaTxIndex(paraCfg, detail)
	require.NoError(t, err)
	require.Nil(t, paraIndexes)
}

// TestParaCrossActionTypeMatchesAssetFilter 索引保留交易字节的类型集合必须覆盖执行侧真正处理的类型
func TestParaCrossActionTypeMatchesAssetFilter(t *testing.T) {
	require.True(t, isParaCrossAssetAction(uint32(pt.ParacrossActionAssetTransfer)))
	require.True(t, isParaCrossAssetAction(uint32(pt.ParacrossActionAssetWithdraw)))
	require.True(t, isParaCrossAssetAction(uint32(pt.ParacrossActionCrossAssetTransfer)))
	require.True(t, isParaCrossAssetAction(uint32(pt.ParacrossActionCrossAssetTransfer)+1))
	require.False(t, isParaCrossAssetAction(uint32(pt.ParacrossActionNodeConfig)))
	require.False(t, isParaCrossAssetAction(uint32(pt.ParacrossActionNodeGroupApply)))
	require.False(t, isParaCrossAssetAction(uint32(pt.ParacrossActionSelfStageConfig)))
	require.False(t, isParaCrossAssetAction(0))
}
