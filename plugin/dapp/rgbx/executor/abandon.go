package executor

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/33cn/chain33/system/dapp"
	"github.com/33cn/chain33/types"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
)

/*
 * Abandon-withdraw proposals.
 *
 * The chain only records the proposal: a business-side operator asks the TSS group to refund
 * an unsettled withdrawal (unlockAsset). Whether a proposal is honoured is decided off-chain
 * by each parachain node's local whitelist, so no authority check belongs here. What the
 * chain does enforce is that the proposal describes a real, still-open withdrawal:
 *
 *   - the burn must have a Withdraw payload and an immutable burn ledger record;
 *   - the owner must match the ledger's owner byte for byte (typo protection);
 *   - the burn must not have reached a terminal state (settled or released).
 *
 * Multiple proposals for the same burn are allowed on purpose: the first valid one wins
 * off-chain, and a stuck proposal must not block a later, corrected one.
 */

const (
	abandonRequestKeyPrefix     = KeyPrefixStateDB + "abandon-req-"
	abandonHeightCountKeyPrefix = KeyPrefixStateDB + "abandon-count-"
	// maxAbandonHeightScan bounds how many heights one listing call walks: stateDB has no
	// List, so callers page by advancing StartHeight.
	maxAbandonHeightScan = int64(maxListCount)
)

// formatAbandonRequestKey indexes a proposal by (height, per-height sequence). The sequence
// keeps keys contiguous inside a block, which is what the query loop walks.
func formatAbandonRequestKey(height, sequence int64) []byte {
	return []byte(abandonRequestKeyPrefix + dapp.HeightIndexStr(height, sequence))
}

// formatAbandonHeightCountKey stores how many proposals exist at a height. StateDB has no
// List, so this counter is how the height-range query enumerates them.
func formatAbandonHeightCountKey(height int64) []byte {
	return []byte(fmt.Sprintf("%s%020d", abandonHeightCountKeyPrefix, height))
}

// abandonHeightCount reads the number of proposals at a height (missing = 0).
func (r *rgbx) abandonHeightCount(height int64) (int64, error) {
	if r.GetStateDB() == nil {
		return 0, nil
	}
	value, err := r.GetStateDB().Get(formatAbandonHeightCountKey(height))
	if errors.Is(err, types.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	count := &types.Int64{}
	if err = types.Decode(value, count); err != nil {
		elog.Error("abandonHeightCount decode", "height", height, "err", err)
		return 0, err
	}
	return count.GetData(), nil
}

// checkAbandon validates an abandon proposal against consensus state.
func (r *rgbx) checkAbandon(txHash string, abandon *rtypes.AbandonWithdraw) error {
	if abandon == nil || len(abandon.GetBurnTxHash()) == 0 || abandon.GetOwner() == "" {
		elog.Error("checkAbandon invalid request", "txHash", txHash,
			"burnTxHashLen", len(abandon.GetBurnTxHash()), "owner", abandon.GetOwner())
		return ErrInvalidAbandonRequest
	}
	burnTxHash := abandon.GetBurnTxHash()
	withdraw := &rtypes.WithdrawAsset{}
	if err := readDB(r.GetStateDB(), formatPayloadKey(burnTxHash), withdraw); err != nil {
		elog.Error("checkAbandon read withdraw payload", "txHash", txHash,
			"burnTxHash", hex.EncodeToString(burnTxHash), "err", err)
		return ErrWithdrawNotExist
	}
	symbol := ensureCrossChainSymbol(withdraw.GetAssetSymbol())
	opID, err := rtypes.BurnOperationID(symbol, burnTxHash)
	if err != nil {
		elog.Error("checkAbandon derive burn operation id", "txHash", txHash,
			"symbol", symbol, "burnTxHash", hex.EncodeToString(burnTxHash), "err", err)
		return ErrInvalidOperationID
	}
	record := &rtypes.OperationRecord{}
	if err = readDB(r.GetStateDB(), formatOperationRecordKey(opID), record); err != nil {
		elog.Error("checkAbandon read operation record", "txHash", txHash, "symbol", symbol,
			"burnTxHash", hex.EncodeToString(burnTxHash), "opID", hex.EncodeToString(opID), "err", err)
		return ErrOperationNotExist
	}
	if record.GetKind() != rtypes.OperationKindBurn || record.GetSymbol() != formatSymbol(symbol) ||
		!bytes.Equal(record.GetChain33TxHash(), burnTxHash) {
		elog.Error("checkAbandon operation record mismatch", "txHash", txHash, "symbol", symbol,
			"burnTxHash", hex.EncodeToString(burnTxHash), "kind", record.GetKind(),
			"recordSymbol", record.GetSymbol(), "recordBurnHash", hex.EncodeToString(record.GetChain33TxHash()))
		return ErrOperationMismatch
	}
	if record.GetOwner() != abandon.GetOwner() {
		elog.Error("checkAbandon owner mismatch", "txHash", txHash, "symbol", symbol,
			"burnTxHash", hex.EncodeToString(burnTxHash),
			"recordOwner", record.GetOwner(), "payloadOwner", abandon.GetOwner())
		return ErrOperationOwnerMismatch
	}
	// Terminal state guard: a settled or released burn has nothing left to abandon.
	// A read error other than ErrNotFound is fail-closed.
	if _, err = r.GetStateDB().Get(formatWithdrawFinalKey(burnTxHash)); err == nil {
		elog.Error("checkAbandon withdraw already final", "txHash", txHash, "symbol", symbol,
			"burnTxHash", hex.EncodeToString(burnTxHash))
		return ErrWithdrawAlreadyFinal
	} else if !errors.Is(err, types.ErrNotFound) {
		elog.Error("checkAbandon read final key", "txHash", txHash, "symbol", symbol,
			"burnTxHash", hex.EncodeToString(burnTxHash), "err", err)
		return err
	}
	return nil
}

// Exec_Abandon records a proposal. It repeats every CheckTx check (Exec may be reached
// without CheckTx) and appends the proposal to the per-height index plus a receipt log.
func (r *rgbx) Exec_Abandon(abandon *rtypes.AbandonWithdraw, tx *types.Transaction, index int) (*types.Receipt, error) {
	txHash := hex.EncodeToString(tx.Hash())
	if err := r.checkAbandon(txHash, abandon); err != nil {
		return nil, err
	}
	height := r.GetHeight()
	sequence, err := r.abandonHeightCount(height)
	if err != nil {
		return nil, err
	}
	item := &rtypes.AbandonWithdrawItem{
		TxHash:     tx.Hash(),
		BurnTxHash: abandon.GetBurnTxHash(),
		Owner:      abandon.GetOwner(),
		Reason:     abandon.GetReason(),
		Proposer:   tx.From(),
		Height:     height,
		Index:      int32(index),
	}
	elog.Info("Exec_Abandon recorded", "txHash", txHash,
		"burnTxHash", hex.EncodeToString(abandon.GetBurnTxHash()), "owner", abandon.GetOwner(),
		"proposer", tx.From(), "height", height, "index", index)
	return &types.Receipt{
		Ty: types.ExecOk,
		KV: []*types.KeyValue{
			{Key: formatAbandonRequestKey(height, sequence), Value: types.Encode(item)},
			{Key: formatAbandonHeightCountKey(height), Value: types.Encode(&types.Int64{Data: sequence + 1})},
		},
		Logs: []*types.ReceiptLog{
			{Ty: rtypes.TyAbandonWithdrawLog, Log: types.Encode(item)},
		},
	}, nil
}

// Query_ListAbandonRequests lists proposals in a height range. StartIndex skips proposals
// whose block transaction index is lower at StartHeight. EndHeight = 0 means "up to the
// current height". StateDB has no List, so at most maxAbandonHeightScan heights are walked
// per call; callers page by advancing StartHeight.
func (r *rgbx) Query_ListAbandonRequests(req *rtypes.ReqListPendingTx) (*rtypes.AbandonWithdrawList, error) {
	if req.GetCount() <= 0 || req.GetCount() > maxListCount {
		return nil, types.ErrInvalidParam
	}
	start := req.GetStartHeight()
	if start < 0 {
		return nil, types.ErrInvalidParam
	}
	end := req.GetEndHeight()
	if end > 0 && end < start {
		return nil, types.ErrInvalidParam
	}
	if end <= 0 {
		end = r.GetHeight()
		if end < start {
			end = start + maxAbandonHeightScan - 1
		}
	}
	if end-start+1 > maxAbandonHeightScan {
		end = start + maxAbandonHeightScan - 1
	}
	reply := &rtypes.AbandonWithdrawList{}
	for height := start; height <= end; height++ {
		count, err := r.abandonHeightCount(height)
		if err != nil {
			return nil, err
		}
		for sequence := int64(0); sequence < count; sequence++ {
			item := &rtypes.AbandonWithdrawItem{}
			if err = readDB(r.GetStateDB(), formatAbandonRequestKey(height, sequence), item); err != nil {
				elog.Error("Query_ListAbandonRequests read item", "height", height,
					"sequence", sequence, "err", err)
				return nil, err
			}
			if height == start && int64(item.GetIndex()) < req.GetStartIndex() {
				continue
			}
			reply.Items = append(reply.Items, item)
			if int32(len(reply.Items)) >= req.GetCount() {
				return reply, nil
			}
		}
	}
	return reply, nil
}
