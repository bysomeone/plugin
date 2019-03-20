package executor

import (
	"fmt"

	"github.com/33cn/chain33/types"
	ptypes "github.com/33cn/plugin/plugin/dapp/calculator/types/calculator"
)

/*
 * 实现交易的链上执行接口
 * 关键数据上链（statedb）并生成交易回执（log）
 */

func (c *calculator) Exec_Add(payload *ptypes.Add, tx *types.Transaction, index int) (*types.Receipt, error) {
	var receipt *types.Receipt
	sum := payload.Addend + payload.Summand
	addLog := &ptypes.AddLog{Sum: sum}
	logs := []*types.ReceiptLog{{Ty: ptypes.TyAddLog, Log: types.Encode(addLog)}}
	key := fmt.Sprintf("%s-%s-formula", KeyPrefixStateDB, tx.Hash())
	val := fmt.Sprintf("%d+%d=%d", payload.Summand, payload.Addend, sum)
	receipt = &types.Receipt{
		Ty:   types.ExecOk,
		KV:   []*types.KeyValue{{Key: []byte(key), Value: []byte(val)}},
		Logs: logs,
	}
	return receipt, nil
}

func (c *calculator) Exec_Sub(payload *ptypes.Subtract, tx *types.Transaction, index int) (*types.Receipt, error) {
	//implement code
	return &types.Receipt{}, nil
}

func (c *calculator) Exec_Mul(payload *ptypes.Multiply, tx *types.Transaction, index int) (*types.Receipt, error) {
	//implement code
	return &types.Receipt{}, nil
}

func (c *calculator) Exec_Div(payload *ptypes.Divide, tx *types.Transaction, index int) (*types.Receipt, error) {
	//implement code
	return &types.Receipt{}, nil
}
