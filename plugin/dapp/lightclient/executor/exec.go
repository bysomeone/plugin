package executor

import (
	"github.com/33cn/chain33/types"
	lightclienttypes "github.com/33cn/plugin/plugin/dapp/lightclient/types"
)

/*
 * 实现交易的链上执行接口
 * 关键数据上链（statedb）并生成交易回执（log）
 */

func (l *lightclient) Exec_CommitHeaders(payload *lightclienttypes.CommitHeaders, tx *types.Transaction, index int) (*types.Receipt, error) {
	receipt := &types.Receipt{Ty: types.ExecOk}
	//implement code
	return receipt, nil
}
