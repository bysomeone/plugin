package executor

import (
	"github.com/33cn/chain33/types"
	lightclienttypes "github.com/33cn/plugin/plugin/dapp/lightclient/types"
)

/*
 * 实现交易相关数据本地执行，数据不上链
 * 非关键数据，本地存储(localDB), 用于辅助查询，效率高
 */

func (l *lightclient) ExecLocal_CommitHeaders(payload *lightclienttypes.CommitHeaders, tx *types.Transaction, receiptData *types.ReceiptData, index int) (*types.LocalDBSet, error) {
	dbSet := &types.LocalDBSet{}
	//implement code, add customize kv to dbSet...

	//auto gen for localdb auto rollback
	return l.addAutoRollBack(tx, dbSet.KV), nil
}

// 当区块回滚时，框架支持自动回滚localdb kv，需要对exec-local返回的kv进行封装
func (l *lightclient) addAutoRollBack(tx *types.Transaction, kv []*types.KeyValue) *types.LocalDBSet {

	dbSet := &types.LocalDBSet{}
	dbSet.KV = l.AddRollbackKV(tx, tx.Execer, kv)
	return dbSet
}
