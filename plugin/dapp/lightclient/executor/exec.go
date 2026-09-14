package executor

import (
	"github.com/33cn/chain33/types"
	ltypes "github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
)

/*
 * 实现交易的链上执行接口
 * 关键数据上链（statedb）并生成交易回执（log）
 */

func (l *lightclient) Exec_BtcHeaders(headers *ltypes.BtcHeaders, tx *types.Transaction, index int) (*types.Receipt, error) {
	receipt := &types.Receipt{Ty: types.ExecOk}

	// 决策与 CheckTx 同一套逻辑（statedb 状态 + 本批头），保证"交易能过检查"与"执行结果"一致。
	state, err := loadBtcChainState(l.GetStateDB(), l.GetLocalDB())
	if err != nil {
		elog.Error("Exec_BtcHeaders", "loadBtcChainState err", err)
		return nil, ErrBtcGetLastHeader
	}
	plan, err := planBtcHeaders(state, headers.GetHeaders())
	if err != nil {
		elog.Error("Exec_BtcHeaders", "planBtcHeaders err", err)
		return nil, err
	}

	prevHeader, err := getBtcLastHeader(l.GetStateDB())
	if err != nil {
		elog.Error("Exec_BtcHeaders", "getBtcLastHeader err", err)
		return nil, ErrBtcGetLastHeader
	}

	commitHeader := plan.tip
	elog.Debug("Exec_BtcHeaders", "lastHeight", prevHeader.GetHeight(), "lastHash", prevHeader.GetHash(),
		"commitHeight", commitHeader.GetHeight(), "commitHash", commitHeader.GetHash(),
		"forkHeight", plan.forkHeight, "switched", plan.switched, "tipWork", plan.tipWork)

	log := &ltypes.BtcHeadersLog{
		LastHeight:    prevHeader.GetHeight(),
		LastHash:      prevHeader.GetHash(),
		CommitHash:    commitHeader.GetHash(),
		CommitHeight:  commitHeader.GetHeight(),
		Confirmations: commitHeader.GetConfirmations(),
		// B3/B4：本批的挂载点与选链结论，ExecLocal 靠它决定 localdb 的逐高度头怎么改。
		ForkHeight:        plan.forkHeight,
		CanonicalSwitched: plan.switched,
		TipWork:           encodeBtcWork(plan.tipWork),
		ReorgRuleVersion:  btcReorgRuleVersion,
	}

	// 未切换（本批是一条合法的、但累积工作量不占优的分叉）：不改 canonical 链，
	// 也不写任何状态——只留日志，运维据此能看到"收到了更轻的分叉"。
	if plan.switched {
		receipt.KV = append(receipt.KV,
			&types.KeyValue{
				Key:   btcLastHeaderKey(),
				Value: types.Encode(commitHeader),
			},
			&types.KeyValue{
				Key:   btcChainStateKey(),
				Value: types.Encode(plan.state),
			})
	}

	receipt.Logs = append(receipt.Logs, &types.ReceiptLog{
		Ty:  ltypes.TyBtcHeadersLog,
		Log: types.Encode(log),
	})

	return receipt, nil
}
