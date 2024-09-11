package btc

import (
	"context"

	"github.com/33cn/chain33/client"
	"github.com/33cn/chain33/types"
	"github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient"
)

func init() {
	lightclient.Register("btc", newClient)
}

type btcLight struct {
	ctx context.Context
	cli BtcClient
	cfg config
	api client.QueueProtocolAPI
}

func newClient() lightclient.Lighter {
	return &btcLight{}
}

func (b *btcLight) Init(ctx context.Context, api client.QueueProtocolAPI, commitAddr string, jsonCfg []byte) error {

	b.ctx = ctx
	b.api = api
	types.MustDecode(jsonCfg, &b.cfg)

	return nil
}

func (b *btcLight) Start() {

}
