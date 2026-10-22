package executor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/33cn/chain33/types"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
)

/*
 * Bump-withdraw-fee requests.
 *
 * A withdrawal can get stuck when the fee it was signed with no longer clears the mempool.
 * The sender may ask the bridge to re-sign the replacement transaction at a higher rate, but
 * the chain only records that request and emits an event: settlement (Confirm) and abandonment
 * (Unlock/Abandon) are unchanged, and re-signing happens off chain.
 *
 * What the chain does enforce:
 *
 *   - only the withdrawal's original sender may bump it (tx.From() == the burn ledger owner);
 *   - the burn must still be open: the payload exists and no terminal state (settled by
 *     Confirm, released by Unlock) has been recorded;
 *   - the requested rate is above the original withdrawal rate and within maxBtcFeeRate.
 *
 * The chain deliberately does NOT anchor the check to the previous bump request. A request the
 * bridge rejects off chain (for example a rate that would eat into the withdrawal amount) must
 * not occupy the slot and block a later, viable request. Monotonicity against the rate that is
 * actually applied is a bridge-side decision.
 */

const (
	bumpWithdrawFeeRequestKeyPrefix     = KeyPrefixStateDB + "bump-req-"
	bumpWithdrawFeeHeightCountKeyPrefix = KeyPrefixStateDB + "bump-count-"
	// bumpWithdrawFeeLatestKeyPrefix indexes the latest accepted bump per burn. The same item
	// shape is stored here and in the per-height index; this key gives readers the current
	// request for a burn without walking its whole history. It does not gate later requests.
	bumpWithdrawFeeLatestKeyPrefix = KeyPrefixStateDB + "bump-latest-"
)

// formatBumpWithdrawFeeLatestKey indexes the latest bump of a withdrawal by its burn hash.
// Fixed length: hash first, then concatenate (same shape as formatWithdrawFinalKey).
func formatBumpWithdrawFeeLatestKey(withdrawalTxHash []byte) []byte {
	hash := sha256.Sum256(withdrawalTxHash)
	return append([]byte(bumpWithdrawFeeLatestKeyPrefix), hash[:]...)
}

// checkBumpWithdrawFee is the CheckTx entry point. Exec repeats the same validation (Exec may
// be reached without CheckTx) through validateBumpWithdrawFee.
func (r *rgbx) checkBumpWithdrawFee(fromAddr, txHash string, bump *rtypes.BumpWithdrawFee) error {
	_, err := r.validateBumpWithdrawFee(fromAddr, txHash, bump)
	return err
}

// validateBumpWithdrawFee checks a bump request and returns the original withdrawal rate, which
// is the floor the requested rate must beat. The returned rate is also what Exec records as
// OldFeeRate.
func (r *rgbx) validateBumpWithdrawFee(fromAddr, txHash string,
	bump *rtypes.BumpWithdrawFee) (int64, error) {

	if bump == nil || len(bump.GetWithdrawalTxHash()) == 0 || fromAddr == "" {
		elog.Error("checkBumpWithdrawFee invalid request", "txHash", txHash,
			"withdrawalTxHashLen", len(bump.GetWithdrawalTxHash()), "from", fromAddr)
		return 0, ErrInvalidBumpWithdrawFeeRequest
	}
	withdrawalTxHash := bump.GetWithdrawalTxHash()
	withdraw := &rtypes.WithdrawAsset{}
	if err := readDB(r.GetStateDB(), formatPayloadKey(withdrawalTxHash), withdraw); err != nil {
		elog.Error("checkBumpWithdrawFee read withdraw payload", "txHash", txHash,
			"withdrawalTxHash", hex.EncodeToString(withdrawalTxHash), "err", err)
		return 0, ErrWithdrawNotExist
	}

	// Terminal state guard: settled (paid out) or released (refunded) leaves nothing to bump.
	// A read error other than ErrNotFound is fail-closed.
	if _, err := r.GetStateDB().Get(formatWithdrawFinalKey(withdrawalTxHash)); err == nil {
		elog.Error("checkBumpWithdrawFee withdraw already final", "txHash", txHash,
			"withdrawalTxHash", hex.EncodeToString(withdrawalTxHash))
		return 0, ErrWithdrawAlreadyFinal
	} else if !errors.Is(err, types.ErrNotFound) {
		elog.Error("checkBumpWithdrawFee read final key", "txHash", txHash,
			"withdrawalTxHash", hex.EncodeToString(withdrawalTxHash), "err", err)
		return 0, err
	}

	// The burn ledger is the consensus-state record of who locked the withdrawal, written by
	// Exec_Withdraw. Authorization is checked against it, not against the payload or tx.From()
	// alone.
	symbol := ensureCrossChainSymbol(withdraw.GetAssetSymbol())
	opID, err := rtypes.BurnOperationID(symbol, withdrawalTxHash)
	if err != nil {
		elog.Error("checkBumpWithdrawFee derive burn operation id", "txHash", txHash,
			"symbol", symbol, "withdrawalTxHash", hex.EncodeToString(withdrawalTxHash), "err", err)
		return 0, ErrInvalidOperationID
	}
	record := &rtypes.OperationRecord{}
	if err = readDB(r.GetStateDB(), formatOperationRecordKey(opID), record); err != nil {
		elog.Error("checkBumpWithdrawFee read operation record", "txHash", txHash, "symbol", symbol,
			"withdrawalTxHash", hex.EncodeToString(withdrawalTxHash),
			"opID", hex.EncodeToString(opID), "err", err)
		return 0, ErrOperationNotExist
	}
	if record.GetKind() != rtypes.OperationKindBurn || record.GetSymbol() != formatSymbol(symbol) ||
		!bytes.Equal(record.GetChain33TxHash(), withdrawalTxHash) {
		elog.Error("checkBumpWithdrawFee operation record mismatch", "txHash", txHash, "symbol", symbol,
			"withdrawalTxHash", hex.EncodeToString(withdrawalTxHash), "kind", record.GetKind(),
			"recordSymbol", record.GetSymbol(), "recordBurnHash", hex.EncodeToString(record.GetChain33TxHash()))
		return 0, ErrOperationMismatch
	}
	if record.GetOwner() != fromAddr {
		elog.Error("checkBumpWithdrawFee owner mismatch", "txHash", txHash, "symbol", symbol,
			"withdrawalTxHash", hex.EncodeToString(withdrawalTxHash),
			"recordOwner", record.GetOwner(), "from", fromAddr)
		return 0, ErrBumpWithdrawFeeNotOwner
	}

	feeRate := bump.GetFeeRate()
	if feeRate > maxBtcFeeRate {
		elog.Error("checkBumpWithdrawFee fee rate too high", "txHash", txHash,
			"withdrawalTxHash", hex.EncodeToString(withdrawalTxHash),
			"feeRate", feeRate, "maxBtcFeeRate", maxBtcFeeRate)
		return 0, ErrInvalidWithdrawFeeRate
	}
	if feeRate <= withdraw.GetFeeRate() {
		elog.Error("checkBumpWithdrawFee fee rate not higher", "txHash", txHash,
			"withdrawalTxHash", hex.EncodeToString(withdrawalTxHash),
			"feeRate", feeRate, "withdrawFeeRate", withdraw.GetFeeRate())
		return 0, ErrBumpWithdrawFeeRateNotHigher
	}
	return withdraw.GetFeeRate(), nil
}

// Exec_BumpWithdrawFee records a request: the latest-per-burn record, one entry in the
// per-height index (for the bridge's range polling) and a receipt log.
func (r *rgbx) Exec_BumpWithdrawFee(bump *rtypes.BumpWithdrawFee, tx *types.Transaction,
	index int) (*types.Receipt, error) {

	txHash := hex.EncodeToString(tx.Hash())
	withdrawFeeRate, err := r.validateBumpWithdrawFee(tx.From(), txHash, bump)
	if err != nil {
		return nil, err
	}
	height := r.GetHeight()
	sequence, err := r.heightIndexCount(bumpWithdrawFeeHeightCountKeyPrefix, height)
	if err != nil {
		return nil, err
	}
	item := &rtypes.BumpWithdrawFeeItem{
		TxHash:           tx.Hash(),
		WithdrawalTxHash: bump.GetWithdrawalTxHash(),
		Owner:            tx.From(),
		FeeRate:          bump.GetFeeRate(),
		OldFeeRate:       withdrawFeeRate,
		Height:           height,
		Index:            int32(index),
	}
	kvs := []*types.KeyValue{
		{Key: formatBumpWithdrawFeeLatestKey(bump.GetWithdrawalTxHash()), Value: types.Encode(item)},
	}
	kvs = append(kvs, r.heightIndexKVs(bumpWithdrawFeeRequestKeyPrefix,
		bumpWithdrawFeeHeightCountKeyPrefix, height, sequence, item)...)
	elog.Info("Exec_BumpWithdrawFee recorded", "txHash", txHash,
		"withdrawalTxHash", hex.EncodeToString(bump.GetWithdrawalTxHash()), "owner", tx.From(),
		"oldFeeRate", withdrawFeeRate, "feeRate", bump.GetFeeRate(), "height", height, "index", index)
	return &types.Receipt{
		Ty: types.ExecOk,
		KV: kvs,
		Logs: []*types.ReceiptLog{
			{Ty: rtypes.TyBumpWithdrawFeeLog, Log: types.Encode(item)},
		},
	}, nil
}

// Query_ListBumpWithdrawFeeRequests lists accepted requests in a height range, so the bridge can
// poll by advancing StartHeight. Paging rules match Query_ListAbandonRequests.
func (r *rgbx) Query_ListBumpWithdrawFeeRequests(req *rtypes.ReqListPendingTx) (*rtypes.BumpWithdrawFeeList, error) {
	items, err := listHeightIndexed(r, req, bumpWithdrawFeeRequestKeyPrefix,
		bumpWithdrawFeeHeightCountKeyPrefix,
		func() *rtypes.BumpWithdrawFeeItem { return &rtypes.BumpWithdrawFeeItem{} })
	if err != nil {
		return nil, err
	}
	return &rtypes.BumpWithdrawFeeList{Items: items}, nil
}
