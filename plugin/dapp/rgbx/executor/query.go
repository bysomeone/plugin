package executor

import (
	"encoding/hex"
	"errors"

	"github.com/33cn/chain33/types"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
)

const maxListCount = 1000

func (r *rgbx) Query_ListPendingTx(req *rtypes.ReqListPendingTx) (types.Message, error) {

	if req.GetCount() <= 0 || req.GetCount() > maxListCount {
		return nil, types.ErrInvalidParam
	}
	startKey := formatPendingTxKey(req.GetStartHeight(), req.GetStartIndex())
	values, err := r.GetLocalDB().List([]byte(pendingTxKeyPrefix), startKey, req.GetCount(), 1)
	if err != nil && !errors.Is(err, types.ErrNotFound) {
		elog.Error("Query_GetPendingTxs", "list err", err, "req", req.String())
		return nil, err
	}

	pendingTxs := &rtypes.PendingTxs{}
	for _, v := range values {
		tx := &rtypes.PendingTx{}
		err := types.Decode(v, tx)
		if err != nil {
			elog.Error("Query_GetPendingTxs", "decode err", err)
			continue
		}
		// 0 means no end height limit
		if req.GetEndHeight() > 0 && tx.GetTxBlockHeight() > req.GetEndHeight() {
			break
		}

		pendingTxs.PendingList = append(pendingTxs.PendingList, tx)
	}

	return pendingTxs, nil
}

func (r *rgbx) Query_GetConfirmedHeight(_ *types.ReqNil) (types.Message, error) {

	v, err := r.GetLocalDB().Get([]byte(confirmedHeightKey))

	reply := &types.Int64{}
	if errors.Is(err, types.ErrNotFound) {
		return reply, nil
	}
	if err != nil {
		elog.Error("Query_GetConfirmedHeight", "get db err", err)
		return nil, err
	}

	err = types.Decode(v, reply)
	if err != nil {
		elog.Error("Query_GetConfirmedHeight", "decode err", err)
		return nil, err
	}

	return reply, nil

}

func (r *rgbx) Query_GetAsset(req *types.ReqString) (types.Message, error) {

	symbol := req.GetData()
	v, err := r.GetStateDB().Get(formatAssetKey(symbol))
	reply := &rtypes.RgbxAsset{}
	if err != nil {
		elog.Error("Query_GetAsset", "symbol", symbol, "get db err", err)
		return nil, err
	}

	err = types.Decode(v, reply)
	if err != nil {
		elog.Error("Query_GetAsset", "symbol", symbol, "decode err", err)
		return nil, err
	}

	return reply, nil
}

func (r *rgbx) Query_GetPendingTx(req *rtypes.ReqGetPendingTx) (types.Message, error) {

	v, err := r.GetLocalDB().Get(formatPendingTxKey(req.GetHeight(), req.GetIndex()))

	reply := &rtypes.PendingTx{}
	if err != nil {
		elog.Error("Query_GetPendingTx", "height", req.GetHeight(),
			"index", req.GetIndex(), "get db err", err)
		return nil, err
	}

	err = types.Decode(v, reply)
	if err != nil {
		elog.Error("Query_GetPendingTx", "height", req.GetHeight(),
			"index", req.GetIndex(), "decode err", err)
		return nil, err
	}

	return reply, nil
}

func (r *rgbx) Query_GetCrossChainInfo(req *types.ReqString) (types.Message, error) {

	symbol := req.GetData()
	v, err := r.GetStateDB().Get(formatCrossChainInfoKey(symbol))
	reply := &rtypes.CrossChainInfo{}
	if errors.Is(err, types.ErrNotFound) {
		return reply, nil
	}
	if err != nil {
		elog.Error("Query_GetCrossChainInfo", "symbol", symbol, "get db err", err)
		return nil, err
	}

	err = types.Decode(v, reply)
	if err != nil {
		elog.Error("Query_GetCrossChainInfo", "symbol", symbol, "decode err", err)
		return nil, err
	}

	return reply, nil
}

// Query_GetOperation 按全局 operationId 查询一条台账记录（S2 的可审计入口）。
//
// 读的是**共识状态**（statedb，query 带 stateHash ⇒ 读到的是链上状态，各节点一致），
// 因此这条查询的结果本身就是"链上怎么说"，不需要信任任何桥侧/侧车的自述。
//
// settled 是**查询时补算**的（不写回记录，记录本身不可变）：
//   - 铸造记录：已铸造，恒 true；
//   - 销毁记录：是否已结算放款 = 链上是否存在该 burn 的已消费键（formatWithdrawUsedKey，S3）。
//
// 找不到记录时返回 ErrNotFound（而不是空记录）：审计里"没有这条记录"和"记录内容为空"
// 是两件不同的事，前者必须响亮。
func (r *rgbx) Query_GetOperation(req *rtypes.ReqGetOperation) (types.Message, error) {

	opID := req.GetOperationId()
	if len(opID) != rtypes.OperationIDLen {
		elog.Error("Query_GetOperation invalid operation id length", "opID", hex.EncodeToString(opID),
			"len", len(opID), "expectLen", rtypes.OperationIDLen)
		return nil, types.ErrInvalidParam
	}
	rec := &rtypes.OperationRecord{}
	if err := readDB(r.GetStateDB(), formatOperationRecordKey(opID), rec); err != nil {
		elog.Error("Query_GetOperation get record", "opID", hex.EncodeToString(opID), "err", err)
		return nil, err
	}
	rec.Settled = r.isOperationSettled(rec)
	return rec, nil
}

// Query_GetOperationSupply 按 symbol 查询铸造/销毁累计（S2）。
// minted − burned 即"桥仍然欠出去的额度"，可与侧车/RGB 侧的账本做交叉核对。
func (r *rgbx) Query_GetOperationSupply(req *types.ReqString) (types.Message, error) {

	if req.GetData() == "" {
		return nil, types.ErrInvalidParam
	}
	// 存入台账的 symbol 是**跨链账户符号**（Exec_Deposit/Exec_Withdraw 用的是 ensureCrossChainSymbol），
	// 查询侧必须做同一归一化："btc" 与 "XBTC" 都要落到同一个 key。少了这一步就会读到空计数
	// （曾经真的这么错过一次：写入 opssupply-XBTC，查询读 opssupply-BTC ⇒ 永远 0）。
	symbol := ensureCrossChainSymbol(req.GetData())
	supply := &rtypes.OperationSupply{}
	if err := readDB(r.GetStateDB(), formatOperationSupplyKey(symbol), supply); err != nil {
		// 从未有过任何该 symbol 的操作：返回零值而不是报错（与 GetCrossChainInfo 的处理一致）
		if errors.Is(err, types.ErrNotFound) {
			return &rtypes.OperationSupply{Symbol: formatSymbol(symbol), Network: operationNetwork(symbol)}, nil
		}
		elog.Error("Query_GetOperationSupply get supply", "symbol", symbol, "err", err)
		return nil, err
	}
	return supply, nil
}

// Query_ListOperations 枚举某 symbol 某类操作的台账记录（S2）。
//
// 执行期/查询期的 stateDB 只有 Get/Set（没有 List，见 db.KV 接口），所以枚举靠**显式下标**：
// 供应量记录里存了下一可用下标，每个下标一条索引键（→ operationId）。因此本查询是
// "按下标逐个 Get"，条数受 count 限制（maxListCount），返回 nextIndex 供续查。
func (r *rgbx) Query_ListOperations(req *rtypes.ReqListOperations) (types.Message, error) {

	if req.GetCount() <= 0 || req.GetCount() > maxListCount || req.GetStart() < 0 {
		return nil, types.ErrInvalidParam
	}
	if req.GetKind() != rtypes.OperationKindMint && req.GetKind() != rtypes.OperationKindBurn {
		return nil, types.ErrInvalidParam
	}
	if req.GetAssetSymbol() == "" {
		return nil, types.ErrInvalidParam
	}
	// 同 GetOperationSupply：台账里的 symbol 是跨链账户符号，查询侧做同一归一化。
	symbol := ensureCrossChainSymbol(req.GetAssetSymbol())
	reply := &rtypes.OperationRecords{}
	for i := int32(0); i < req.GetCount(); i++ {
		index := req.GetStart() + int64(i)
		opID, err := r.GetStateDB().Get(formatOperationIndexKey(symbol, req.GetKind(), index))
		if err != nil {
			// 下标取尽（或索引缺失）：正常结束，不是错误
			if errors.Is(err, types.ErrNotFound) {
				return reply, nil
			}
			elog.Error("Query_ListOperations get index", "symbol", symbol, "kind", req.GetKind(),
				"index", index, "err", err)
			return nil, err
		}
		rec := &rtypes.OperationRecord{}
		if err = readDB(r.GetStateDB(), formatOperationRecordKey(opID), rec); err != nil {
			elog.Error("Query_ListOperations get record", "symbol", symbol, "kind", req.GetKind(),
				"index", index, "opID", hex.EncodeToString(opID), "err", err)
			return nil, err
		}
		rec.Settled = r.isOperationSettled(rec)
		reply.Records = append(reply.Records, rec)
	}
	// 取满一页 ⇒ 可能还有下一页
	reply.NextIndex = req.GetStart() + int64(req.GetCount())
	return reply, nil
}

// isOperationSettled 补算记录的"是否已结算"（不写回记录）。
// 判定完全基于共识状态：销毁记录的结算标记就是 S3 的已消费键（formatWithdrawUsedKey），
// 铸造记录在铸造成功那一刻即已成账。
func (r *rgbx) isOperationSettled(rec *rtypes.OperationRecord) bool {
	if rec.GetKind() != rtypes.OperationKindBurn {
		return true
	}
	_, err := r.GetStateDB().Get(formatWithdrawUsedKey(rec.GetChain33TxHash()))
	return err == nil
}

func (r *rgbx) Query_ListPendingTxByFrom(req *types.ReqString) (types.Message, error) {
	fromAddr := req.GetData()
	if fromAddr == "" {
		return nil, types.ErrInvalidParam
	}
	list := &rtypes.TxBlockIndexList{}
	err := readDB(r.GetLocalDB(), formatPendingTxFromKey(fromAddr), list)
	if errors.Is(err, types.ErrNotFound) {
		return &rtypes.PendingTxs{}, nil
	}
	if err != nil {
		elog.Error("Query_ListPendingTxByFrom", "from", fromAddr, "read list err", err)
		return nil, err
	}
	reply := &rtypes.PendingTxs{}
	for _, item := range list.GetBlockIndexList() {
		tx := &rtypes.PendingTx{}
		err = readDB(r.GetLocalDB(), formatPendingTxKey(item.GetBlockHeight(), item.GetTxIndex()), tx)
		if err != nil {
			continue
		}
		if tx.GetConfirmed() {
			continue
		}
		reply.PendingList = append(reply.PendingList, tx)
	}
	return reply, nil
}
