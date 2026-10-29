package executor

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"

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

// Query_GetCrossChainInfo returns the record of a cross-chain asset.
//
// Both spellings of the symbol are accepted (BTC and XBTC, RGB20_USDT and XRGB20_USDT): the
// record is stored under the ORIGINAL symbol, while balances and transfers use the mapped one.
// The exact spelling is tried first, so an explicit query for one spelling never silently
// returns a different record when both exist. A missing record returns an empty structure
// (the existing contract) rather than an error.
func (r *rgbx) Query_GetCrossChainInfo(req *types.ReqString) (types.Message, error) {

	symbol := req.GetData()
	reply := &rtypes.CrossChainInfo{}
	_, value, err := readSymbolRecord(r.GetStateDB(), formatCrossChainInfoKey, symbol)
	if errors.Is(err, types.ErrNotFound) {
		return reply, nil
	}
	if err != nil {
		elog.Error("Query_GetCrossChainInfo", "symbol", symbol, "get db err", err)
		return nil, err
	}

	err = types.Decode(value, reply)
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
//   - 销毁记录：是否已结算放款 = 该 burn 的终态键是否为 settled（formatWithdrawFinalKey）；
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
// 判定完全基于共识状态：销毁记录的结算标记就是 S3 的终态键（formatWithdrawFinalKey）。
// 只有值 == settled 才算已结算：released 表示这笔 burn 已被 Unlock 退回（从未结算），
// 铸造记录在铸造成功那一刻即已成账。
func (r *rgbx) isOperationSettled(rec *rtypes.OperationRecord) bool {
	if rec.GetKind() != rtypes.OperationKindBurn {
		return true
	}
	value, err := r.GetStateDB().Get(formatWithdrawFinalKey(rec.GetChain33TxHash()))
	return err == nil && bytes.Equal(value, withdrawFinalSettled)
}

// Query_GetWithdrawFinalState reports the terminal state of a withdrawal burn:
// "" while it is still locked, "settled" after Confirm paid it out, or "released" after
// Unlock refunded it. The payload is the burn's chain33 transaction hash in hex.
//
// This is a read-only view of the consensus-state key that decides both terminal paths, so it
// answers "did the refund happen?" without trusting any bridge-side status. It is also the only
// reader that can tell released apart from "not settled yet": OperationRecord.Settled is false
// in both cases by design (released means the burn was refunded, never paid out).
func (r *rgbx) Query_GetWithdrawFinalState(req *types.ReqString) (types.Message, error) {
	burnTxHash, err := decodeBurnTxHash(req.GetData())
	if err != nil {
		return nil, err
	}
	value, err := r.GetStateDB().Get(formatWithdrawFinalKey(burnTxHash))
	if errors.Is(err, types.ErrNotFound) {
		return &types.ReplyString{}, nil
	}
	if err != nil {
		elog.Error("Query_GetWithdrawFinalState get db", "burnTxHash", req.GetData(), "err", err)
		return nil, err
	}
	return &types.ReplyString{Data: string(value)}, nil
}

// Query_GetWithdrawConsignmentHash returns the hex sha256 of the RGB20 consignment the bridge
// delivered for a withdrawal burn. The payload is the burn's chain33 transaction hash in hex.
//
// Not every settled withdrawal has one: the bridge submits Confirm even when it has no local
// consignment record (settlement must never wait on delivery), so ErrNotFound here means "no
// hash was registered", not "the withdrawal does not exist".
func (r *rgbx) Query_GetWithdrawConsignmentHash(req *types.ReqString) (types.Message, error) {
	burnTxHash, err := decodeBurnTxHash(req.GetData())
	if err != nil {
		return nil, err
	}
	value, err := r.GetStateDB().Get(formatWithdrawConsignmentKey(burnTxHash))
	if errors.Is(err, types.ErrNotFound) {
		return nil, types.ErrNotFound
	}
	if err != nil {
		elog.Error("Query_GetWithdrawConsignmentHash get db", "burnTxHash", req.GetData(), "err", err)
		return nil, err
	}
	if len(value) == 0 {
		return nil, types.ErrNotFound
	}
	return &types.ReplyString{Data: hex.EncodeToString(value)}, nil
}

// Withdrawal status values returned by deriveWithdrawStatus.
const (
	withdrawStatusPending  = "pending"
	withdrawStatusSettled  = "settled"
	withdrawStatusReleased = "released"
	withdrawStatusUnknown  = "unknown"
)

// deriveWithdrawStatus resolves the single status string shown to callers.
//
// The consensus terminal state is authoritative. When there is none, a local pending record
// means the burn exists and is still waiting; without either signal the outcome is unknown
// (e.g. the node has no local pending index for a burn that was already processed).
func deriveWithdrawStatus(hasPending bool, finalState string) string {
	switch finalState {
	case withdrawStatusSettled:
		return withdrawStatusSettled
	case withdrawStatusReleased:
		return withdrawStatusReleased
	}
	if hasPending {
		return withdrawStatusPending
	}
	return withdrawStatusUnknown
}

// loadLocalPendingTxByHash finds the node's local pending record for a chain33 tx hash.
// A missing hash index or a record already tombstoned by Confirm both return (nil, nil):
// local pending data is supplementary, never the source of a final verdict.
func (r *rgbx) loadLocalPendingTxByHash(txHash []byte) (*rtypes.PendingTx, error) {
	if len(txHash) == 0 || r.GetLocalDB() == nil {
		return nil, nil
	}
	index := &rtypes.TxBlockIndex{}
	err := readDB(r.GetLocalDB(), formatPendingTxHashKey(txHash), index)
	if errors.Is(err, types.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	pending := &rtypes.PendingTx{}
	err = readDB(r.GetLocalDB(), formatPendingTxKey(index.GetBlockHeight(), index.GetTxIndex()), pending)
	if errors.Is(err, types.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if pending.GetConfirmed() {
		return nil, nil
	}
	return pending, nil
}

// Query_GetWithdrawStatus answers "how did this withdrawal end?" in one call.
//
// The verdict comes from consensus state: the terminal key written by Confirm (settled) or
// Unlock (released). The local pending record and the registered consignment hash are
// supplementary fields; neither can turn a burn into settled or released.
func (r *rgbx) Query_GetWithdrawStatus(req *types.ReqString) (types.Message, error) {
	burnTxHash, err := decodeBurnTxHash(req.GetData())
	if err != nil {
		return nil, err
	}
	reply := &rtypes.WithdrawStatus{}

	pending, err := r.loadLocalPendingTxByHash(burnTxHash)
	if err != nil {
		elog.Error("Query_GetWithdrawStatus load local pending", "burnTxHash", req.GetData(), "err", err)
		return nil, err
	}
	reply.Pending = pending

	value, err := r.GetStateDB().Get(formatWithdrawFinalKey(burnTxHash))
	if err == nil {
		reply.FinalState = string(value)
	} else if !errors.Is(err, types.ErrNotFound) {
		elog.Error("Query_GetWithdrawStatus get final state", "burnTxHash", req.GetData(), "err", err)
		return nil, err
	}

	consignment, err := r.GetStateDB().Get(formatWithdrawConsignmentKey(burnTxHash))
	if err == nil {
		if len(consignment) > 0 {
			reply.ConsignmentHash = hex.EncodeToString(consignment)
		}
	} else if !errors.Is(err, types.ErrNotFound) {
		elog.Error("Query_GetWithdrawStatus get consignment", "burnTxHash", req.GetData(), "err", err)
		return nil, err
	}

	reply.Status = deriveWithdrawStatus(pending != nil, reply.GetFinalState())
	return reply, nil
}

// Query_GetCrossChainLockAddress returns the shared address that holds the assets of unsettled// withdrawals for a cross-chain asset. The payload is the chain33 asset symbol (e.g. RGB20_USDT).
//
// Read-only: it derives the same deterministic address the executor transfers into on Withdraw
// and out of on Unlock. Operators (and the E2E that checks the refund) use it to observe the
// locked balance without knowing the address derivation.
func (r *rgbx) Query_GetCrossChainLockAddress(req *types.ReqString) (types.Message, error) {
	if req.GetData() == "" {
		return nil, types.ErrInvalidParam
	}
	symbol := ensureCrossChainSymbol(req.GetData())
	accDB, err := r.newAccount(symbol)
	if err != nil {
		elog.Error("Query_GetCrossChainLockAddress newAccount", "symbol", symbol, "err", err)
		return nil, err
	}
	return &types.ReplyString{Data: r.crossChainLockAddress(accDB)}, nil
}

// decodeBurnTxHash parses the hex form used by CLI/RPC callers. A "0x" prefix is tolerated
// because the CLI prints transaction hashes that way; an empty or non-hex value is an
// argument error, not "no such withdrawal".
func decodeBurnTxHash(s string) ([]byte, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "0x")
	if s == "" {
		return nil, types.ErrInvalidParam
	}
	hash, err := hex.DecodeString(s)
	if err != nil || len(hash) == 0 {
		return nil, types.ErrInvalidParam
	}
	return hash, nil
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
