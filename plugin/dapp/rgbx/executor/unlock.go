package executor

import (
	"encoding/hex"
	"errors"

	"github.com/33cn/chain33/types"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
)

// Exec_Unlock refunds the amount still locked for an unsettled withdrawal burn back to that
// withdrawal's original owner.
//
// It is the on-chain side of the "abandon a withdrawal" escape hatch. Once released, the burn
// can never be settled by Confirm (checkWithdrawConfirm rejects any terminal state), so the
// same locked amount cannot be both refunded and paid out. The burned supply counters are
// deliberately left untouched: this burn was never settled, so minted - burned stays unchanged.
//
// Every check from checkUnlock is repeated here (defense in depth: Exec may be reached without
// CheckTx). The transfer amount and recipient come from the immutable ledger record, not from
// the payload, which is only required to match it (and is covered by the signature).
func (r *rgbx) Exec_Unlock(unlock *rtypes.UnlockAsset, tx *types.Transaction, _ int) (*types.Receipt, error) {
	txHash := hex.EncodeToString(tx.Hash())
	burnTxHash := unlock.GetBurnTxHash()
	if len(burnTxHash) == 0 {
		elog.Error("Exec_Unlock empty burn tx hash", "txHash", txHash)
		return nil, ErrInvalidUnlockRequest
	}
	withdraw := &rtypes.WithdrawAsset{}
	if err := readDB(r.GetStateDB(), formatPayloadKey(burnTxHash), withdraw); err != nil {
		elog.Error("Exec_Unlock read withdraw payload", "txHash", txHash,
			"burnTxHash", hex.EncodeToString(burnTxHash), "err", err)
		return nil, ErrWithdrawNotExist
	}
	// Both spellings resolve to the same registered asset; compare canonically so a payload
	// written with either spelling accepts an unlock signed with the other. The signature
	// still binds the raw payload (verifyUnlockSig below).
	withdrawSymbol, err := r.canonicalCrossChainSymbol(withdraw.GetAssetSymbol())
	if err != nil {
		elog.Error("Exec_Unlock canonical withdraw symbol", "txHash", txHash,
			"symbol", withdraw.GetAssetSymbol(), "err", err)
		return nil, ErrGetCrossChainInfo
	}
	unlockSymbol, err := r.canonicalCrossChainSymbol(unlock.GetAssetSymbol())
	if err != nil {
		elog.Error("Exec_Unlock canonical unlock symbol", "txHash", txHash,
			"symbol", unlock.GetAssetSymbol(), "err", err)
		return nil, ErrGetCrossChainInfo
	}
	if unlockSymbol != withdrawSymbol {
		elog.Error("Exec_Unlock symbol mismatch", "txHash", txHash,
			"burnTxHash", hex.EncodeToString(burnTxHash),
			"unlockSymbol", unlock.GetAssetSymbol(), "withdrawSymbol", withdraw.GetAssetSymbol())
		return nil, ErrOperationMismatch
	}
	info, err := r.getCrossChainInfo(withdrawSymbol)
	if err != nil {
		elog.Error("Exec_Unlock getCrossChainInfo", "txHash", txHash,
			"symbol", withdrawSymbol, "err", err)
		return nil, ErrGetCrossChainInfo
	}
	symbol := ensureCrossChainSymbol(withdrawSymbol)
	opID, err := rtypes.BurnOperationID(symbol, burnTxHash)
	if err != nil {
		elog.Error("Exec_Unlock derive burn operation id", "txHash", txHash,
			"symbol", symbol, "burnTxHash", hex.EncodeToString(burnTxHash), "err", err)
		return nil, ErrInvalidOperationID
	}
	rec := &rtypes.OperationRecord{}
	if err = readDB(r.GetStateDB(), formatOperationRecordKey(opID), rec); err != nil {
		elog.Error("Exec_Unlock read operation record", "txHash", txHash, "symbol", symbol,
			"burnTxHash", hex.EncodeToString(burnTxHash), "opID", hex.EncodeToString(opID), "err", err)
		return nil, ErrOperationNotExist
	}
	if rec.GetKind() != rtypes.OperationKindBurn || rec.GetSymbol() != formatSymbol(symbol) {
		elog.Error("Exec_Unlock operation kind mismatch", "txHash", txHash, "symbol", symbol,
			"burnTxHash", hex.EncodeToString(burnTxHash), "opID", hex.EncodeToString(opID),
			"kind", rec.GetKind(), "recSymbol", rec.GetSymbol())
		return nil, ErrOperationMismatch
	}
	if rec.GetOwner() != unlock.GetOwner() {
		elog.Error("Exec_Unlock owner mismatch", "txHash", txHash, "symbol", symbol,
			"burnTxHash", hex.EncodeToString(burnTxHash), "opID", hex.EncodeToString(opID),
			"recordOwner", rec.GetOwner(), "payloadOwner", unlock.GetOwner())
		return nil, ErrOperationOwnerMismatch
	}
	if rec.GetAmount() != unlock.GetAmount() {
		elog.Error("Exec_Unlock amount mismatch", "txHash", txHash, "symbol", symbol,
			"burnTxHash", hex.EncodeToString(burnTxHash), "opID", hex.EncodeToString(opID),
			"recordedAmount", rec.GetAmount(), "payloadAmount", unlock.GetAmount())
		return nil, ErrOperationAmountMismatch
	}
	_, err = r.GetStateDB().Get(formatWithdrawFinalKey(burnTxHash))
	if err == nil {
		elog.Error("Exec_Unlock withdraw already final", "txHash", txHash, "symbol", symbol,
			"burnTxHash", hex.EncodeToString(burnTxHash))
		return nil, ErrWithdrawAlreadyFinal
	}
	if !errors.Is(err, types.ErrNotFound) {
		elog.Error("Exec_Unlock read final key", "txHash", txHash, "symbol", symbol,
			"burnTxHash", hex.EncodeToString(burnTxHash), "err", err)
		return nil, err
	}
	// Defense in depth: re-verify the group signature (same as Exec_Deposit).
	if err = verifyUnlockSig(info.GetPubkey(), unlock); err != nil {
		elog.Error("Exec_Unlock verifyUnlockSig", "txHash", txHash, "symbol", symbol,
			"burnTxHash", hex.EncodeToString(burnTxHash), "err", err)
		return nil, err
	}
	accDB, err := r.newAccount(symbol)
	if err != nil {
		elog.Error("Exec_Unlock newAccount", "txHash", txHash, "symbol", symbol, "err", err)
		return nil, err
	}
	lockAddr := r.crossChainLockAddress(accDB)
	// Use the ledger's owner/amount: the payload has been checked to match them, and the
	// record is immutable since lock time.
	transferReceipt, err := accDB.Transfer(lockAddr, rec.GetOwner(), rec.GetAmount())
	if err != nil {
		elog.Error("Exec_Unlock transfer", "txHash", txHash, "symbol", symbol, "lockAddr", lockAddr,
			"owner", rec.GetOwner(), "amount", rec.GetAmount(), "err", err)
		return nil, err
	}
	receipt := &types.Receipt{Ty: types.ExecOk}
	receipt.KV = append(receipt.KV, transferReceipt.KV...)
	receipt.Logs = append(receipt.Logs, transferReceipt.Logs...)
	receipt.KV = append(receipt.KV, &types.KeyValue{
		Key:   formatWithdrawFinalKey(burnTxHash),
		Value: withdrawFinalReleased,
	})
	receipt.Logs = append(receipt.Logs, &types.ReceiptLog{
		Ty: rtypes.TyUnlockAssetLog,
		Log: types.Encode(&rtypes.UnlockAsset{
			BurnTxHash:  burnTxHash,
			AssetSymbol: withdrawSymbol,
			Amount:      rec.GetAmount(),
			Owner:       rec.GetOwner(),
		}),
	})
	elog.Info("Exec_Unlock released", "txHash", txHash, "burnTxHash", hex.EncodeToString(burnTxHash),
		"symbol", symbol, "owner", rec.GetOwner(), "amount", rec.GetAmount())
	return receipt, nil
}
