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
	// 防双铸标记 + 台账记录都要用到"解析后 btc 交易的规范 txid（E1 口径）"，因此在铸造前先解析：
	//   - 台账的 operationId 以该 txid 为原像之一（types.MintOperationID）；
	//   - 台账要在铸造**之前**完成"写一次"校验（同 id 已存在则拒绝，见 newOperationRecordKV），
	//     否则就变成"先铸后判"，判失败时资产已经进账。
	// CheckTx（checkDepositDuplicate → parseBtcTxIDStrict）已强制严格解析，这里失败不可达；
	// 真发生时按 fail-closed 处理：整笔充值失败（不铸、不登记），而不是静默地"铸了但不记账"
	// —— 后者会让供应量少算，把合法的后续提现挡在退出闸门外。
	txID, err := parseBtcTxIDStrict(hex.EncodeToString(txHash), deposit.GetTxProof().GetTxData())
	if err != nil {
		elog.Error("Exec_Deposit parse strict btc tx id", "txHash", hex.EncodeToString(txHash),
			"symbol", symbol, "err", err)
		return nil, err
	}
	owner := deposit.GetDepositAddress()
	opID, err := rtypes.MintOperationID(symbol, txID, owner, deposit.GetAmount())
	if err != nil {
		elog.Error("Exec_Deposit derive mint operation id", "txHash", hex.EncodeToString(txHash),
			"symbol", symbol, "depositAddr", owner, "amount", deposit.GetAmount(), "err", err)
		return nil, ErrInvalidOperationID
	}
	operationKVs, err := r.recordMintOperation(opID, txID, symbol, owner, deposit.GetAmount(), txHash)
	if err != nil {
		elog.Error("Exec_Deposit record mint operation", "txHash", hex.EncodeToString(txHash),
			"symbol", symbol, "opID", hex.EncodeToString(opID), "err", err)
		return nil, err
	}

	accDB, err := r.newAccount(symbol)
	if err != nil {
		elog.Error("Exec_Deposit newCrossChainAccount", "txHash", hex.EncodeToString(txHash), "symbol", symbol,
			"err", err)
		return nil, err
	}
	depositReceipt, err := accDB.Mint(owner, deposit.GetAmount())
	if err != nil {
		elog.Error("Exec_Deposit Mint", "txHash", hex.EncodeToString(txHash), "symbol", symbol,
			"depositAddr", owner, "amount", deposit.GetAmount(), "err", err)
		return nil, err
	}
	receipt.KV = append(receipt.KV, depositReceipt.KV...)
	receipt.Logs = append(receipt.Logs, depositReceipt.Logs...)

	// 铸造成功才登记：防双铸标记（E1 的 txid 口径）+ 全局操作台账（S2：记录 + 下标 + 供应量）。
	// 三者都是回执 KV，随 stateDB 一起提交/回滚。
	receipt.KV = append(receipt.KV, &types.KeyValue{
		Key:   formatDepositUsedTxIDKey(txID),
		Value: []byte("used"),
	})
	receipt.KV = append(receipt.KV, operationKVs...)

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
	// S2：锁定前先按 (symbol, 本笔 burn 的 chain33 哈希) 派生 operationId 并完成台账"写一次"校验。
	// 从锁定成功这一刻起，本笔提现的金额就固定写进了共识状态（不可变记录），结算时的放款判据
	// （checkWithdrawOperationLedger）以它为准，而不是以结算时可变的输入为准。
	opID, err := rtypes.BurnOperationID(symbol, txHash)
	if err != nil {
		elog.Error("Exec_Withdraw derive burn operation id", "txHash", hex.EncodeToString(txHash),
			"symbol", symbol, "err", err)
		return nil, ErrInvalidOperationID
	}
	operationKVs, err := r.recordBurnOperation(opID, symbol, tx.From(), withdraw.GetAmount(), txHash)
	if err != nil {
		elog.Error("Exec_Withdraw record burn operation", "txHash", hex.EncodeToString(txHash),
			"symbol", symbol, "opID", hex.EncodeToString(opID), "err", err)
		return nil, err
	}
	lockReceipt, err := accDB.Transfer(tx.From(), lockAddr, withdraw.GetAmount())
	if err != nil {
		elog.Error("Exec_Withdraw lock transfer", "txHash", hex.EncodeToString(txHash), "from", tx.From(),
			"symbol", symbol, "amount", withdraw.GetAmount(), "err", err)
		return nil, err
	}
	receipt.KV = append(receipt.KV, lockReceipt.KV...)
	receipt.Logs = append(receipt.Logs, lockReceipt.Logs...)
	receipt.KV = append(receipt.KV, operationKVs...)

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
