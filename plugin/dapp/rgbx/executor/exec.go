package executor

import (
	"encoding/hex"
	"github.com/33cn/chain33/types"
	rgbxtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
)

/*
 * 实现交易的链上执行接口
 * 关键数据上链（statedb）并生成交易回执（log）
 */

func (r *rgbx) Exec_Mint(mint *rgbxtypes.MintAsset, tx *types.Transaction, index int) (*types.Receipt, error) {
	receipt := &types.Receipt{Ty: types.ExecOk}

	receipt.KV = append(receipt.KV, &types.KeyValue{
		Key:   formatPayloadKey(tx.Hash()),
		Value: types.Encode(mint),
	})

	receipt.Logs = append(receipt.Logs, &types.ReceiptLog{
		Ty: rgbxtypes.TyPendingTxLog,
		Log: types.Encode(&rgbxtypes.PendingTx{
			ActionType: rgbxtypes.TyMintAction,
			Timestamp:  r.GetBlockTime(),
			TxHash:     tx.Hash(),
			Utxo:       mint.GetFirstPrevOut(),
		}),
	})

	return receipt, nil
}

func (r *rgbx) Exec_Transfer(transfer *rgbxtypes.TransferAsset, tx *types.Transaction, index int) (*types.Receipt, error) {

	txHash := hex.EncodeToString(tx.Hash())
	elog.Debug("Exec_Transfer", "txHash", txHash,
		"from", transfer.GetFrom().Address(), "to", transfer.GetTo().Address())

	// from是btc utxo, 记录并等待confirm交易
	if transfer.GetFrom().GetUtxo() != nil {
		receipt := &types.Receipt{
			Ty: types.ExecOk,
			KV: []*types.KeyValue{{Key: formatPayloadKey(tx.Hash()), Value: types.Encode(transfer)}},
		}
		return receipt, nil
	}
	accDB, err := r.newAccount(transfer.GetSymbol())
	if err != nil {
		elog.Error("Exec_Transfer newAccount", "txHash", txHash,
			"from", transfer.From.Address(), "to", transfer.To.Address(), "err", err)
		return nil, err
	}
	receipt, err := accDB.Transfer(transfer.GetFrom().Address(), transfer.GetTo().Address(), transfer.GetAmount())
	if err != nil {
		elog.Error("Exec_Transfer transfer", "txHash", txHash,
			"from", transfer.From.Address(), "to", transfer.To.Address(), "err", err)
		return nil, err
	}
	return receipt, nil
}

func (r *rgbx) Exec_Confirm(payload *rgbxtypes.ConfirmTx, tx *types.Transaction, index int) (*types.Receipt, error) {
	receipt := &types.Receipt{Ty: types.ExecOk}
	//implement code
	return receipt, nil
}
