package rollup

import (
	"errors"

	"github.com/33cn/chain33/types"
	rtypes "github.com/33cn/plugin/plugin/dapp/rollup/types"
)

func (r *RollUp) getValidatorPubKeys() *rtypes.ValidatorPubs {

	req := &rtypes.ChainTitle{Value: r.chainCfg.GetTitle()}

	reply, err := r.mainChainGrpc.QueryChain(r.ctx, &types.ChainExecutor{
		Driver:   rtypes.RollupX,
		FuncName: "GetValidatorPubs",
		Param:    types.Encode(req),
	})

	if err != nil || !reply.GetIsOk() {
		rlog.Error("getValidatorPubKeys", "msg", string(reply.GetMsg()), "query err", err)
		return nil
	}

	res := &rtypes.ValidatorPubs{}

	err = types.Decode(reply.GetMsg(), res)
	if err != nil {
		rlog.Error("getValidatorPubKeys", "decode err", err)
		return nil
	}

	return res
}

func (r *RollUp) getRollupStatus() *rtypes.RollupStatus {

	req := &rtypes.ChainTitle{Value: r.chainCfg.GetTitle()}

	reply, err := r.mainChainGrpc.QueryChain(r.ctx, &types.ChainExecutor{
		Driver:   rtypes.RollupX,
		FuncName: "GetRollupStatus",
		Param:    types.Encode(req),
	})

	if err != nil || !reply.GetIsOk() {
		rlog.Error("getRollupStatus", "msg", string(reply.GetMsg()), "query err", err)
		return nil
	}

	res := &rtypes.RollupStatus{}

	err = types.Decode(reply.GetMsg(), res)
	if err != nil {
		rlog.Error("getRollupStatus", "decode err", err)
		return nil
	}
	return res
}

func (r *RollUp) createTx(exec, action string, payload []byte) (*types.Transaction, error) {

	req := &types.CreateTxIn{
		Execer:     []byte(exec),
		Payload:    payload,
		ActionName: action,
	}
	reply, err := r.mainChainGrpc.CreateTransaction(r.ctx, req)
	tx := &types.Transaction{}
	err = types.Decode(reply.GetData(), tx)
	return tx, err
}

func (r *RollUp) getProperFeeRate() int64 {

	reply, err := r.mainChainGrpc.GetProperFee(r.ctx, nil)
	if err != nil {
		rlog.Error("getProperFeeRate", "err", err)
	} else {
		r.lastFeeRate = reply.GetProperFee()
	}

	return r.lastFeeRate
}

func (r *RollUp) sendTx(tx *types.Transaction) error {

	reply, err := r.mainChainGrpc.SendTransaction(r.ctx, tx)
	if err == nil && !reply.GetIsOk() {
		err = errors.New(string(reply.GetMsg()))
	}
	return err
}
