package executor

import (
	"github.com/33cn/chain33/types"
	rgbxtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
)

/*
 * 实现交易的链上执行接口
 * 关键数据上链（statedb）并生成交易回执（log）
 */

func (r *rgbx) Exec_Mint(payload *rgbxtypes.MintAsset, tx *types.Transaction, index int) (*types.Receipt, error) {
	receipt := &types.Receipt{Ty: types.ExecOk}
	//implement code
	return receipt, nil
}

func (r *rgbx) Exec_Transfer(payload *rgbxtypes.TransferAsset, tx *types.Transaction, index int) (*types.Receipt, error) {
	receipt := &types.Receipt{Ty: types.ExecOk}
	//implement code
	return receipt, nil
}
