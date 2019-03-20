package executor

import (
	"fmt"

	"github.com/33cn/chain33/types"
	ptypes "github.com/33cn/plugin/plugin/dapp/calculator/types/calculator"
)

/*
 * 实现区块回退时本地执行的数据清除
 */

func (c *calculator) ExecDelLocal_Add(payload *ptypes.Add, tx *types.Transaction, receiptData *types.ReceiptData, index int) (*types.LocalDBSet, error) {
	var dbSet *types.LocalDBSet
	var countInfo ptypes.ReplyQueryCalcCount
	localKey := []byte(fmt.Sprintf("%s-CalcCount-Add", KeyPrefixLocalDB))
	oldVal, err := c.GetLocalDB().Get(localKey)
	if err != nil && err != types.ErrNotFound {
		return nil, err
	}
	err = types.Decode(oldVal, &countInfo)
	if err != nil {
		elog.Error("execDelLocalAdd", "DecodeErr", err)
		return nil, types.ErrDecode
	}
	countInfo.Count--
	if countInfo.Count < 0 {
		countInfo.Count = 0
	}
	dbSet = &types.LocalDBSet{KV: []*types.KeyValue{{Key: localKey, Value: types.Encode(&countInfo)}}}
	return dbSet, nil
}

func (c *calculator) ExecDelLocal_Sub(payload *ptypes.Subtract, tx *types.Transaction, receiptData *types.ReceiptData, index int) (*types.LocalDBSet, error) {
	//implement code
	return &types.LocalDBSet{}, nil
}

func (c *calculator) ExecDelLocal_Mul(payload *ptypes.Multiply, tx *types.Transaction, receiptData *types.ReceiptData, index int) (*types.LocalDBSet, error) {
	//implement code
	return &types.LocalDBSet{}, nil
}

func (c *calculator) ExecDelLocal_Div(payload *ptypes.Divide, tx *types.Transaction, receiptData *types.ReceiptData, index int) (*types.LocalDBSet, error) {
	//implement code
	return &types.LocalDBSet{}, nil
}
