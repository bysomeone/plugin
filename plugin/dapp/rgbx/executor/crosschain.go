package executor

import (
	"encoding/hex"
	"errors"
	"strings"

	"github.com/33cn/chain33/types"
	paratypes "github.com/33cn/plugin/plugin/dapp/paracross/types"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
)

const defaultGuardianParachainTitle = "user.p.rgbxguardians."

func (r *rgbx) Exec_CommitDKG(commit *rtypes.CommitDKG, tx *types.Transaction, index int) (*types.Receipt, error) {

	symbol := formatSymbol(commit.GetAssetSymbol())
	receipt := &types.Receipt{Ty: types.ExecOk}
	txHash := hex.EncodeToString(tx.Hash())
	commitAddr := tx.From()
	addrs := &types.ReqAddrs{}
	err := readDB(r.GetStateDB(), formatDkgConfirmationsKey(symbol, commit.DkgAddress), addrs)
	if err != nil && !errors.Is(err, types.ErrNotFound) {
		elog.Error("Exec_CommitDKG", "txHash", txHash, "symbol", symbol, "dkgAddr", commit.DkgAddress, "readDB err", err)
		return nil, ErrGetDkgConfirmations
	}
	for _, addr := range addrs.Addrs {
		if commitAddr == addr {
			return receipt, nil
		}
	}
	addrs.Addrs = append(addrs.Addrs, commitAddr)
	guardianAddrs, err := r.getGuardianNodeAddress(rgbxCfg.GuardianParachainTitle)
	if err != nil {
		elog.Error("Exec_CommitDKG", "txHash", txHash, "symbol", symbol, "getGuardianNodeAddress err", err)
		return nil, ErrGetGuardianNodeAddress
	}

	encodeAddrs := types.Encode(addrs)
	receipt.KV = append(receipt.KV, &types.KeyValue{
		Key:   formatDkgConfirmationsKey(symbol, commit.GetDkgAddress()),
		Value: encodeAddrs,
	})

	receipt.Logs = append(receipt.Logs, &types.ReceiptLog{
		Ty:  rtypes.TyCommitDKGLog,
		Log: encodeAddrs,
	})

	if len(strings.Split(guardianAddrs, ",")) == len(addrs.Addrs) {

		info := &rtypes.CrossChainInfo{
			AssetSymbol:   symbol,
			WrappedSymbol: formatCrossChainSymbol(symbol),
			TssAddress:    commit.DkgAddress,
			PkScript:      commit.PkScript,
			// RGB20 分支（BL-1）：TSS 组公钥，供 checkDeposit/Exec_Deposit 验 thresholdSig。
			// 同一 (symbol,address) 的所有 CommitDKG 必须提交相同 pubkey。
			Pubkey: commit.GetPubkey(),
		}
		receipt.KV = append(receipt.KV, &types.KeyValue{
			Key:   formatCrossChainInfoKey(symbol),
			Value: types.Encode(info),
		})
	}

	return receipt, nil

}

func (r *rgbx) Exec_Deposit(deposit *rtypes.DepositAsset, tx *types.Transaction, index int) (*types.Receipt, error) {

	receipt := &types.Receipt{Ty: types.ExecOk}
	txHash := tx.Hash()
	symbol := ensureCrossChainSymbol(deposit.GetAssetSymbol())
	// RGB20 分支：防御性复验 thresholdSig（CheckTx 已验，这里防止绕过 CheckTx 直接 Exec）。
	if rtypes.IsRgb20Symbol(deposit.GetAssetSymbol()) {
		info, err := r.getCrossChainInfo(deposit.GetAssetSymbol())
		if err != nil {
			elog.Error("Exec_Deposit getCrossChainInfo", "txHash", hex.EncodeToString(txHash), "symbol", symbol, "err", err)
			return nil, ErrGetCrossChainInfo
		}
		if err := verifyThresholdSig(info.GetPubkey(), deposit); err != nil {
			elog.Error("Exec_Deposit verifyThresholdSig", "txHash", hex.EncodeToString(txHash), "symbol", symbol, "err", err)
			return nil, err
		}
	}
	accDB, err := r.newAccount(symbol)
	if err != nil {
		elog.Error("Exec_Deposit newCrossChainAccount", "txHash", hex.EncodeToString(txHash), "symbol", symbol,
			"err", err)
		return nil, err
	}
	depositReceipt, err := accDB.Mint(deposit.GetDepositAddress(), deposit.GetAmount())
	if err != nil {
		elog.Error("Exec_Deposit Mint", "txHash", hex.EncodeToString(txHash), "symbol", symbol,
			"depositAddr", deposit.GetDepositAddress(), "amount", deposit.GetAmount(), "err", err)
		return nil, err
	}
	receipt.KV = append(receipt.KV, depositReceipt.KV...)
	receipt.Logs = append(receipt.Logs, depositReceipt.Logs...)

	// 防双铸标记：以解析后 btc 交易的 txid（规范化身份）登记该充值已消费。
	// CheckTx（同高度先于 Exec 执行）已强制严格解析，此处解析失败不可达；
	// 若真发生则记日志跳过写入，不让链因一个本不该出现的证明而停机。
	if txID, err := parseBtcTxIDStrict(hex.EncodeToString(txHash), deposit.GetTxProof().GetTxData()); err != nil {
		elog.Error("Exec_Deposit parse strict btc tx id", "txHash", hex.EncodeToString(txHash),
			"symbol", symbol, "err", err)
	} else {
		receipt.KV = append(receipt.KV, &types.KeyValue{
			Key:   formatDepositUsedTxIDKey(txID),
			Value: []byte("used"),
		})
	}

	return receipt, nil

}

func (r *rgbx) Exec_Withdraw(withdraw *rtypes.WithdrawAsset, tx *types.Transaction, index int) (*types.Receipt, error) {
	receipt := &types.Receipt{Ty: types.ExecOk}
	txHash := tx.Hash()
	symbol := ensureCrossChainSymbol(withdraw.GetAssetSymbol())
	accDB, err := r.newAccount(symbol)
	if err != nil {
		return nil, err
	}
	lockAddr := r.crossChainLockAddress(accDB)
	lockReceipt, err := accDB.Transfer(tx.From(), lockAddr, withdraw.GetAmount())
	if err != nil {
		elog.Error("Exec_Withdraw lock transfer", "txHash", hex.EncodeToString(txHash), "from", tx.From(),
			"symbol", symbol, "amount", withdraw.GetAmount(), "err", err)
		return nil, err
	}
	receipt.KV = append(receipt.KV, lockReceipt.KV...)
	receipt.Logs = append(receipt.Logs, lockReceipt.Logs...)

	receipt.KV = append(receipt.KV, &types.KeyValue{
		Key:   formatPayloadKey(txHash),
		Value: types.Encode(withdraw),
	})
	receipt.Logs = append(receipt.Logs, &types.ReceiptLog{
		Ty: rtypes.TyPendingTxLog,
		Log: types.Encode(&rtypes.PendingTx{
			ActionType:    rtypes.TyWithdrawAsset,
			Timestamp:     r.GetBlockTime(),
			TxBlockHeight: r.GetHeight(),
			TxIndex:       int64(index),
			TxHash:        txHash,
			FromAddress:   tx.From(),
			AssetSymbol:   formatSymbol(withdraw.GetAssetSymbol()),
			TargetAddress: withdraw.GetDestinationAddr(),
			Amount:        withdraw.GetAmount(),
			FeeRate:       withdraw.GetFeeRate(),
		}),
	})
	return receipt, nil
}

func (r *rgbx) getGuardianNodeAddress(title string) (string, error) {

	params := &paratypes.ReqParacrossNodeInfo{Title: title}
	resp, err := r.GetAPI().Query(paratypes.ParaX, "GetNodeGroupStatus", params)
	if err != nil {
		elog.Error("getGuardianNodeAddress", "title", title, "err", err)
		return "", err
	}

	status := resp.(*paratypes.ParaNodeGroupStatus)
	return status.TargetAddrs, nil
}
