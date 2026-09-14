package executor

import (
	"github.com/33cn/chain33/types"
	ltypes "github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
)

/*
 * 实现交易相关数据本地执行，数据不上链
 * 非关键数据，本地存储(localDB), 用于辅助查询，效率高
 */

// ExecLocal_BtcHeaders 维护 localDB 里"按高度 + 按 hash"两套头索引，使 GetBtcHeader / GetBtcHeaderByHash
// 始终反映 **canonical 链**。
//
// B3/B4 之后 canonical tip 可能回退/换分支，这时必须：
//  1. 把被替换掉的旧 canonical 高度（[forkHeight+1, 旧 tip]，含新 tip 之上那些旧分叉残留的高度）从
//     localDB 删掉 —— 否则 SPV 证明会继续按旧分叉的头校验（rgbx 的 validateBtcTxProof 拿
//     GetBtcHeader(proof.BlockHeight) 的 hash 与证明里的 blockHash 比对，删了旧分叉头，旧分叉上的
//     证明自然变成 ErrInvalidBtcProofBlock，这正是我们要的）；
//  2. 同时删掉这些旧头的 hash -> height 索引，避免 GetBtcHeaderByHash(旧hash) 命中一个高度相同、
//     实际属于新链的头（那种"查得到但不是它"比查不到更危险）；
//  3. 再按高度写入本批头（新 canonical 链）。
//
// 决策不在这里重算，直接从回执日志（Exec 写出）读取：localdb 的写入必须与共识决策逐节点一致，
// 而日志是区块数据的一部分，所有节点看到的完全相同。
func (l *lightclient) ExecLocal_BtcHeaders(headers *ltypes.BtcHeaders, tx *types.Transaction, receiptData *types.ReceiptData, index int) (*types.LocalDBSet, error) {
	dbSet := &types.LocalDBSet{}

	kv := l.btcHeadersLocalKV(headers.GetHeaders(), receiptData)
	if len(kv) == 0 {
		return dbSet, nil
	}
	//auto gen for localdb auto rollback
	return l.addAutoRollBack(tx, kv), nil
}

// btcHeadersLocalKV 计算本批头需要写入/删除的 localdb kv。
func (l *lightclient) btcHeadersLocalKV(headers []*ltypes.BtcHeader, receiptData *types.ReceiptData) []*types.KeyValue {
	log, err := findBtcHeadersLog(receiptData)
	if err != nil {
		// 回执日志解不开：宁可不动 localdb（fail-closed），也不能把分叉头写进 canonical 索引。
		elog.Error("btcHeadersLocalKV decode BtcHeadersLog err", "err", err)
		return nil
	}
	if log == nil {
		elog.Error("btcHeadersLocalKV no BtcHeadersLog in receipt, skip localdb update")
		return nil
	}

	var kv []*types.KeyValue

	// B3/B4 之前的旧日志（历史区块重放）：保持旧行为——按高度写入本批头，不做回退删除。
	if log.GetReorgRuleVersion() < btcReorgRuleVersion {
		kv = appendBtcHeadersKV(kv, headers)
		return kv
	}
	if !log.GetCanonicalSwitched() {
		// 本批是更轻的分叉：canonical 链没变，localdb 里的逐高度头必须原样保留。
		return nil
	}

	// 需要清掉的高度区间 [forkHeight+1, max(旧 tip, 新 tip)]：
	//   - 到旧 tip 为止：旧 canonical 分支的头（含新 tip 更低时残留在上面的那些），
	//   - 再加新 tip：保证收尾后 localdb 里不存在"高于 canonical tip 的逐高度头"。
	// 删除先于写入（同一批 KV 内顺序生效），所以新 canonical 头最后落地。
	dropTo := log.GetLastHeight()
	if log.GetCommitHeight() > dropTo {
		dropTo = log.GetCommitHeight()
	}
	for height := log.GetForkHeight() + 1; height <= dropTo; height++ {
		old, err := btcReadHeader(l.GetLocalDB(), height)
		if err == nil && old.GetHash() != "" {
			// nil value = 删除（见 chain33 system/dapp KVCreator / common/db LocalDB 的空值语义）
			kv = append(kv, &types.KeyValue{Key: btcHeaderHashHeightKey(old.GetHash())})
		}
		kv = append(kv, &types.KeyValue{Key: btcHeaderKey(height)})
	}
	return appendBtcHeadersKV(kv, headers)
}

func appendBtcHeadersKV(kv []*types.KeyValue, headers []*ltypes.BtcHeader) []*types.KeyValue {
	for _, h := range headers {
		kv = append(kv,
			&types.KeyValue{
				Key:   btcHeaderKey(h.GetHeight()),
				Value: types.Encode(h),
			}, &types.KeyValue{
				Key:   btcHeaderHashHeightKey(h.GetHash()),
				Value: types.Encode(&types.Int64{Data: int64(h.GetHeight())}),
			})
	}
	return kv
}

// findBtcHeadersLog 从回执日志里取 BtcHeadersLog；没有该日志时返回 (nil, nil)。
func findBtcHeadersLog(receiptData *types.ReceiptData) (*ltypes.BtcHeadersLog, error) {
	if receiptData == nil {
		return nil, nil
	}
	for _, receiptLog := range receiptData.GetLogs() {
		if receiptLog.GetTy() != ltypes.TyBtcHeadersLog {
			continue
		}
		log := &ltypes.BtcHeadersLog{}
		if err := types.Decode(receiptLog.GetLog(), log); err != nil {
			return nil, err
		}
		return log, nil
	}
	return nil, nil
}

// 当区块回滚时，框架支持自动回滚localdb kv，需要对exec-local返回的kv进行封装
func (l *lightclient) addAutoRollBack(tx *types.Transaction, kv []*types.KeyValue) *types.LocalDBSet {

	dbSet := &types.LocalDBSet{}
	dbSet.KV = l.AddRollbackKV(tx, tx.Execer, kv)
	return dbSet
}
