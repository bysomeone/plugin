package executor

import (
	"fmt"

	"github.com/33cn/chain33/types"
	ptypes "github.com/33cn/plugin/plugin/dapp/calculator/types/calculator"
)

/*
 * 实现交易相关数据本地执行，数据不上链
 * 非关键数据，本地存储(localDB), 用于辅助查询，效率高
 */

func (c *calculator) ExecLocal_Add(payload *ptypes.Add, tx *types.Transaction, receiptData *types.ReceiptData, index int) (*types.LocalDBSet, error) {
	var dbSet *types.LocalDBSet
	var countInfo ptypes.ReplyQueryCalcCount
	localKey := []byte(fmt.Sprintf("%s-CalcCount-Add", KeyPrefixLocalDB))
	oldVal, err := c.GetLocalDB().Get(localKey)
	if err != nil && err != types.ErrNotFound {
		return nil, err
	}
	err = types.Decode(oldVal, &countInfo)
	if err != nil {
		elog.Error("execLocalAdd", "DecodeErr", err)
		return nil, types.ErrDecode
	}
	countInfo.Count++
	dbSet = &types.LocalDBSet{KV: []*types.KeyValue{{Key: localKey, Value: types.Encode(&countInfo)}}}
	return dbSet, nil
}

func (c *calculator) ExecLocal_Sub(payload *ptypes.Subtract, tx *types.Transaction, receiptData *types.ReceiptData, index int) (*types.LocalDBSet, error) {
	//implement code
	return &types.LocalDBSet{}, nil
}

func (c *calculator) ExecLocal_Mul(payload *ptypes.Multiply, tx *types.Transaction, receiptData *types.ReceiptData, index int) (*types.LocalDBSet, error) {
	//implement code
	return &types.LocalDBSet{}, nil
}

func (c *calculator) ExecLocal_Div(payload *ptypes.Divide, tx *types.Transaction, receiptData *types.ReceiptData, index int) (*types.LocalDBSet, error) {
	//implement code
	return &types.LocalDBSet{}, nil
}
