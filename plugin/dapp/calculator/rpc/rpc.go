package rpc

/*
 * 实现json rpc和grpc service接口
 * json rpc用Jrpc结构作为接收实例
 * grpc使用channelClient结构作为接收实例
 */

import (
	"context"
	"encoding/hex"

	"github.com/33cn/chain33/types"
	ptypes "github.com/33cn/plugin/plugin/dapp/calculator/types/calculator"
)

func (j *Jrpc) CreateRawAddTx(in *ptypes.Add, result *interface{}) error {

	data, err := types.CallCreateTx(ptypes.CalculatorX, ptypes.NameAddAction, in)
	if err != nil {
		return err
	}
	*result = hex.EncodeToString(data)
	return nil
}

func (j *Jrpc) QueryCalcCount(in *ptypes.ReqQueryCalcCount, result *interface{}) error {

	reply, err := j.cli.QueryCalcCount(context.Background(), in)
	if err != nil {
		return err
	}
	*result = *reply
	return nil
}

func (c *channelClient) QueryCalcCount(ctx context.Context, in *ptypes.ReqQueryCalcCount) (*ptypes.ReplyQueryCalcCount, error) {

	msg, err := c.Query(ptypes.CalculatorX, "CalcCount", in)
	if err != nil {
		return nil, err
	}
	if reply, ok := msg.(*ptypes.ReplyQueryCalcCount); ok {
		return reply, nil
	}
	return nil, types.ErrTypeAsset
}
