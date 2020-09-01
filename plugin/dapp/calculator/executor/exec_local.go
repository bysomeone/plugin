package executor

import (
	"fmt"

	"github.com/33cn/chain33/types"
	calculatortypes "github.com/33cn/plugin/plugin/dapp/calculator/types"
)

/*
 * 实现交易相关数据本地执行，数据不上链
 * 非关键数据，本地存储(localDB), 用于辅助查询，效率高
 */

func (c *calculator) ExecLocal_Add(payload *calculatortypes.Add, tx *types.Transaction, receiptData *types.ReceiptData, index int) (*types.LocalDBSet, error) {
	var dbSet *types.LocalDBSet
	var countInfo calculatortypes.ReplyQueryCalcCount
	localKey := []byte(fmt.Sprintf("%s-CalcCount-Add", KeyPrefixLocalDB))
	oldVal, err := c.GetLocalDB().Get(localKey)
	//此处需要注意，目前db接口，获取key未找到记录，返回空时候也带一个notFound错误，需要特殊处理，而不是直接返回错误
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
	return c.addAutoRollBack(tx, dbSet.KV), nil
}

func (c *calculator) ExecLocal_Sub(payload *calculatortypes.Subtract, tx *types.Transaction, receiptData *types.ReceiptData, index int) (*types.LocalDBSet, error) {
	dbSet := &types.LocalDBSet{}
	//implement code
	return c.addAutoRollBack(tx, dbSet.KV), nil
}

func (c *calculator) ExecLocal_Mul(payload *calculatortypes.Multiply, tx *types.Transaction, receiptData *types.ReceiptData, index int) (*types.LocalDBSet, error) {
	dbSet := &types.LocalDBSet{}
	//implement code
	return c.addAutoRollBack(tx, dbSet.KV), nil
}

func (c *calculator) ExecLocal_Div(payload *calculatortypes.Divide, tx *types.Transaction, receiptData *types.ReceiptData, index int) (*types.LocalDBSet, error) {
	dbSet := &types.LocalDBSet{}
	//implement code
	return c.addAutoRollBack(tx, dbSet.KV), nil
}

// 当区块回滚时，框架自动回滚localdb kv，需要将local返回的kv进行封装
func (c *calculator) addAutoRollBack(tx *types.Transaction, kv []*types.KeyValue) *types.LocalDBSet {

	dbSet := &types.LocalDBSet{}
	dbSet.KV = c.AddRollbackKV(tx, tx.Execer, kv)
	return dbSet
}
