package executor

import (
	"bytes"
	"encoding/hex"
	"errors"

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
)

// The storage layout, per-height counter and paging rules are shared with the other
// height-indexed request lists (see height_index.go); these wrappers keep the abandon keys and
// helpers readable at the call sites.

// formatAbandonRequestKey indexes a proposal by (height, per-height sequence).
func formatAbandonRequestKey(height, sequence int64) []byte {
	return formatHeightIndexKey(abandonRequestKeyPrefix, height, sequence)
}

// formatAbandonHeightCountKey stores how many proposals exist at a height.
func formatAbandonHeightCountKey(height int64) []byte {
	return formatHeightCountKey(abandonHeightCountKeyPrefix, height)
}

// abandonHeightCount reads the number of proposals at a height (missing = 0).
func (r *rgbx) abandonHeightCount(height int64) (int64, error) {
	return r.heightIndexCount(abandonHeightCountKeyPrefix, height)
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
		KV: r.heightIndexKVs(abandonRequestKeyPrefix, abandonHeightCountKeyPrefix, height, sequence, item),
		Logs: []*types.ReceiptLog{
			{Ty: rtypes.TyAbandonWithdrawLog, Log: types.Encode(item)},
		},
	}, nil
}

// Query_ListAbandonRequests lists proposals in a height range. StartIndex skips proposals
// whose block transaction index is lower at StartHeight. EndHeight = 0 means "up to the
// current height". Callers page by advancing StartHeight.
func (r *rgbx) Query_ListAbandonRequests(req *rtypes.ReqListPendingTx) (*rtypes.AbandonWithdrawList, error) {
	items, err := listHeightIndexed(r, req, abandonRequestKeyPrefix, abandonHeightCountKeyPrefix,
		func() *rtypes.AbandonWithdrawItem { return &rtypes.AbandonWithdrawItem{} })
	if err != nil {
		return nil, err
	}
	return &rtypes.AbandonWithdrawList{Items: items}, nil
}
