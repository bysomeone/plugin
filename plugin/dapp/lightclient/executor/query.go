package executor

import (
	"github.com/33cn/chain33/types"
	ltypes "github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
)

func (l *lightclient) Query_GetBtcLastHeader(req *types.ReqNil) (types.Message, error) {

	header, err := getBtcLastHeader(l.GetStateDB())
	return header, err
}

func (l *lightclient) Query_GetBtcHeader(req *ltypes.ReqGetBtcHeader) (types.Message, error) {

	header, err := getBtcHeader(l.GetLocalDB(), req.GetHeight())
	return header, err
}

func (l *lightclient) Query_GetBtcHeaderByHash(req *types.ReqString) (types.Message, error) {

	height, err := getBtcHeight(l.GetLocalDB(), req.GetData())
	if err != nil {
		elog.Error("Query_GetBtcHeaderByHash", "hash", req.GetData(), "err", err)
		return nil, err
	}
	header, err := getBtcHeader(l.GetLocalDB(), uint64(height.GetData()))
	return header, err
}

func (l *lightclient) Query_GetBtcNetName(req *types.ReqNil) (types.Message, error) {
	return &types.ReplyString{Data: lightCfg.BtcNetName}, nil
}

// Query_GetBtcCheckpoint 返回本网络最高的有效锚点（= bootstrap 的信任根），复用 ltypes.BtcHeader，
// 不新增 proto。
//
// 用途（L2）：中继在链上头链还是空（tip==0）时用它断言 cfg.BtcHeaderStartHeight == 锚点高度 + 1。
// 起点与锚点不配套的话，bootstrap 的每个批都会被执行器以 ErrBtcHeaderNoAnchor 拒收（见
// checkBootstrapAnchor），而中继自己不会退（同一个批重发多少次都一样）—— 中继启动期就把它变成一条
// 显式的 ERROR，而不是让人从运行期的报错里反推配置。
//
// 本网络没有锚点（regtest/testnet4/signet/simnet）时返回高度 0 的空头：调用方据此跳过断言，
// 不做硬依赖。
func (l *lightclient) Query_GetBtcCheckpoint(req *types.ReqNil) (types.Message, error) {
	params := ltypes.GetBtcChainParams(lightCfg.BtcNetName)
	height, hash, ok := highestCheckpoint(btcCheckpointTable[params.Net])
	if !ok {
		return &ltypes.BtcHeader{}, nil
	}
	return &ltypes.BtcHeader{Height: height, Hash: hash}, nil
}
