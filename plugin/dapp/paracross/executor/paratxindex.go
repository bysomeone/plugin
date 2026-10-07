// Copyright Fuzamei Corp. 2018 All Rights Reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package executor

import (
	"bytes"
	"math/big"

	"github.com/33cn/chain33/client"
	"github.com/33cn/chain33/common"
	"github.com/33cn/chain33/types"
	pt "github.com/33cn/plugin/plugin/dapp/paracross/types"
)

func init() {
	types.RegParaTxIndexBuilder(buildParaTxIndex)
}

// paraCrossTx 一笔平行链跨链交易
// noAction为true时tx为nil, 表示区块索引已经确认该交易的跨链执行动作是空操作,
// 执行方不需要交易内容就可以跳过
type paraCrossTx struct {
	ty       uint32
	tx       *types.Transaction
	noAction bool
}

// isParaCrossAssetAction 是否为跨链资产转移类action
// 只有这几类会被execCrossTxNew/rollbackCrossTxNew真正处理, 和FilterParaCrossAssetTxHashes保持一致
func isParaCrossAssetAction(ty uint32) bool {
	return ty == uint32(pt.ParacrossActionAssetTransfer) ||
		ty == uint32(pt.ParacrossActionAssetWithdraw) ||
		ty >= uint32(pt.ParacrossActionCrossAssetTransfer)
}

// newParaCrossTxs 把完整的平行链跨链交易转换成统一的结构
// 交易的执行动作未知, 因此不能标记为noAction, 保持原有的执行路径
func newParaCrossTxs(txs []*types.Transaction) []*paraCrossTx {
	if len(txs) == 0 {
		return nil
	}
	crossTxs := make([]*paraCrossTx, 0, len(txs))
	for _, tx := range txs {
		crossTxs = append(crossTxs, &paraCrossTx{tx: tx})
	}
	return crossTxs
}

// buildParaTxIndex 构建区块的平行链跨链交易索引, 注册到types.RegParaTxIndexBuilder
// 该函数在区块保存时调用, 只能使用区块自身的交易和收据, 不能再读取别的区块
// 返回错误时不写索引, 跨链执行会回退到读取区块体
func buildParaTxIndex(cfg *types.Chain33Config, detail *types.BlockDetail) ([]*types.HeightParaIndex, error) {
	if cfg.IsPara() || detail == nil || detail.Block == nil {
		return nil, nil
	}
	//收据缺失时无法复现FilterTxsForPara, 放弃建索引
	if len(detail.Receipts) != len(detail.Block.Txs) {
		return nil, types.ErrInvalidParam
	}
	var indexes []*types.HeightParaIndex
	for _, title := range paraTxTitles(detail.Block.Txs) {
		index, ok := buildTitleParaTxIndex(cfg, detail, title)
		if ok {
			indexes = append(indexes, index)
		}
	}
	return indexes, nil
}

// paraTxTitles 收集区块中出现过的平行链title, 和saveParaTxTable使用的title一致
func paraTxTitles(txs []*types.Transaction) []string {
	var titles []string
	seen := make(map[string]bool)
	for _, tx := range txs {
		title, ok := types.GetParaExecTitleName(string(tx.Execer))
		if !ok || seen[title] {
			continue
		}
		seen[title] = true
		titles = append(titles, title)
	}
	return titles
}

// buildTitleParaTxIndex 构建单个title的索引
// 返回false时该title不写索引, 跨链执行会回退到读取区块体
func buildTitleParaTxIndex(cfg *types.Chain33Config, detail *types.BlockDetail, title string) (*types.HeightParaIndex, bool) {
	//过滤必须和getCrossTxsByRst完全一致, 否则索引和区块体两条路径的结果会不同
	paraTxs := FilterTxsForPara(cfg, detail.FilterParaTxsByTitle(cfg, title))
	baseIndexes := make(map[string]uint32, len(paraTxs))
	for i, tx := range paraTxs {
		hash := string(tx.Hash())
		if _, exist := baseIndexes[hash]; exist {
			//区块内出现重复交易时, 位图到交易的下标映射不再唯一, 放弃该区块的索引
			clog.Error("buildTitleParaTxIndex duplicate para tx", "height", detail.Block.Height, "title", title,
				"txhash", common.ToHex(tx.Hash()))
			return nil, false
		}
		baseIndexes[hash] = uint32(i)
	}

	index := &types.HeightParaIndex{
		Height:        detail.Block.Height,
		Title:         title,
		Hash:          detail.Block.Hash(cfg),
		BaseHashCount: uint32(len(paraTxs)),
	}
	for _, tx := range FilterParaCrossTxs(paraTxs) {
		baseIndex, exist := baseIndexes[string(tx.Hash())]
		if !exist {
			return nil, false
		}
		item := &types.ParaTxIndexItem{BaseIndex: baseIndex}
		ty, err := paraCrossActionType(tx)
		if err != nil {
			//类型未知时保留完整交易, 执行方会复现同样的解码失败
			clog.Error("buildTitleParaTxIndex decode action", "height", detail.Block.Height, "title", title,
				"txhash", common.ToHex(tx.Hash()), "err", err.Error())
			item.Tx = types.Encode(tx)
		} else {
			item.Ty = int32(ty)
			if isParaCrossAssetAction(ty) {
				item.Tx = types.Encode(tx)
			}
		}
		index.Items = append(index.Items, item)
	}
	return index, true
}

// paraCrossActionType 获取平行链跨链交易的action类型
func paraCrossActionType(tx *types.Transaction) (uint32, error) {
	var payload pt.ParacrossAction
	if err := types.Decode(tx.Payload, &payload); err != nil {
		return 0, err
	}
	return uint32(payload.Ty), nil
}

// getCrossTxsFromIndex 用区块索引重建跨链交易和位图
// done为true时结果可以直接使用, 为false时调用方需要回退到读取区块体
// 索引缺失、不可用以及区块里没有跨链交易都通过done和空结果表达, 不返回错误
func getCrossTxsFromIndex(api client.QueueProtocolAPI, status *pt.ParacrossNodeStatus) ([]*paraCrossTx, []byte, bool, error) {
	//只有ForkLoopCheckCommitTxDone之后的路径才读区块体, 之前的老路径不走索引
	if !pt.IsParaForkHeight(api.GetConfig(), status.MainBlockHeight, pt.ForkLoopCheckCommitTxDone) {
		return nil, nil, false, nil
	}

	rst, needCrossTx, err := getCrossTxRst(api, status)
	if err != nil {
		return nil, nil, false, err
	}
	if !needCrossTx {
		return nil, nil, true, nil
	}

	index, err := api.GetParaTxIndex(&types.ReqParaTxIndex{Height: status.MainBlockHeight, Title: status.Title})
	if err != nil || index == nil {
		// TEMPORARY PROBE, drop before the pull request: Info because the nodes do not emit debug.
		clog.Info("paracross index miss, reading the block body", "mainHeight", status.MainBlockHeight,
			"title", status.Title, "err", err)
		return nil, nil, false, nil
	}
	//索引必须来自commit指向的那个区块
	if !bytes.Equal(index.GetHash(), status.MainBlockHash) || index.GetTitle() != status.Title {
		clog.Error("getCrossTxsFromIndex index not match commit", "mainHeight", status.MainBlockHeight,
			"indexHash", common.ToHex(index.GetHash()), "mainHash", common.ToHex(status.MainBlockHash),
			"indexTitle", index.GetTitle(), "title", status.Title)
		return nil, nil, false, nil
	}

	bitmap := big.NewInt(0).SetBytes(rst)
	crossRst := big.NewInt(0)
	var crossTxs []*paraCrossTx
	for i, item := range index.GetItems() {
		if item.GetBaseIndex() >= index.GetBaseHashCount() {
			clog.Error("getCrossTxsFromIndex base index out of range", "index", i,
				"baseIndex", item.GetBaseIndex(), "baseHashCount", index.GetBaseHashCount())
			return nil, nil, false, nil
		}
		crossTx := &paraCrossTx{ty: uint32(item.GetTy())}
		if len(item.GetTx()) > 0 {
			var tx types.Transaction
			if err := types.Decode(item.GetTx(), &tx); err != nil {
				clog.Error("getCrossTxsFromIndex decode tx", "index", i, "err", err.Error())
				return nil, nil, false, nil
			}
			crossTx.tx = &tx
		} else if isParaCrossAssetAction(crossTx.ty) {
			//资产转移类交易必须有完整字节, 否则索引已损坏
			clog.Error("getCrossTxsFromIndex asset tx missing payload", "index", i, "ty", crossTx.ty)
			return nil, nil, false, nil
		} else {
			crossTx.noAction = true
		}
		if bitmap.Bit(int(item.GetBaseIndex())) == 1 {
			crossRst.SetBit(crossRst, i, 1)
		}
		crossTxs = append(crossTxs, crossTx)
	}
	// TEMPORARY PROBE, drop before the pull request.
	clog.Info("paracross index used", "mainHeight", status.MainBlockHeight, "title", status.Title,
		"txs", len(crossTxs))
	return crossTxs, crossRst.Bytes(), true, nil
}
