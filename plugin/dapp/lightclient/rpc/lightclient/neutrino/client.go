// Copyright Fuzamei Corp. 2018 All Rights Reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package neutrino integrate btc light client neutrino
package neutrino

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/33cn/chain33/client"
	"github.com/33cn/chain33/common/address"
	"github.com/33cn/chain33/common/crypto"
	"github.com/33cn/chain33/queue"
	"github.com/33cn/chain33/rpc/grpcclient"
	"github.com/33cn/chain33/system/crypto/secp256k1"
	"github.com/33cn/chain33/types"
	"github.com/lightninglabs/neutrino/headerfs"

	"github.com/33cn/chain33/common/log/log15"
	"github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient"
	"github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	_ "github.com/btcsuite/btcwallet/walletdb/bdb"
	"github.com/lightninglabs/neutrino"
)

var log = log15.New("module", "lightclient.neutrino")

var _ lightclient.Lighter = &neutrinoClient{}

func init() {

	lightclient.Register("neutrino", newClient)
}

func newClient() lightclient.Lighter {
	return &neutrinoClient{}
}

type neutrinoClient struct {
	ctx               context.Context
	qclient           queue.Client
	chain33Api        client.QueueProtocolAPI
	mainChainGrpc     types.Chain33Client
	cfg               config
	commitAddressType int32
	commitAddr        string
	commitKey         crypto.PrivKey
	commitKeyMu       sync.RWMutex
	initCommitKeyOnce sync.Once
	neutrinoCfg       neutrino.Config
	tss               *tssService
	neutrinoCS        *neutrino.ChainService
	bw                *btcWallet
	rgbx              *rgbx
	rgb20             *rgb20.Adapter
	bestBlock         *headerfs.BlockStamp
	lock              sync.RWMutex
	chain33FeeRate    int64
	withdrawReqChan   chan *rtypes.PendingTx
}

// Init init client context
func (n *neutrinoClient) Init(ctx context.Context, q queue.Queue, cfg *lightclient.Config) error {

	n.ctx = ctx
	n.qclient = q.Client()
	n.chain33Api, _ = client.New(n.qclient, nil)
	n.chain33FeeRate = 100000
	n.commitAddr = cfg.CommitAddr
	commitAddressType, err := address.GetAddressType(n.commitAddr)
	if err != nil {
		panic("invalid address type for authAccount config, " + n.commitAddr)
	}
	n.commitAddressType = commitAddressType
	if cfg.CommitKey != "" {
		_, n.commitKey, err = getPrivKey(secp256k1.Name, cfg.CommitKey)
		if err != nil {
			return err
		}
	}
	subCfg, _ := json.Marshal(cfg.Neutrino)
	types.MustDecode(subCfg, &n.cfg)
	chainCfg := q.GetConfig()
	n.mainChainGrpc, err = grpcclient.NewMainChainClient(chainCfg, "")
	if err != nil {
		panic("init main chain grpc client err:" + err.Error())
	}
	n.tss = newTssService(n)
	err = n.initNeutrinoConfig(chainCfg)
	if err != nil {
		log.Error("Init", "initNeutrinoConfig error", err)
		return err
	}
	// RGB20 适配器：所有节点都需要（签名节点只读校验 ValidateConsignment/交叉核对）。
	// 不能放在 IsOfficialNode 分支里，否则 para2-4（validator）不创建 adapter，
	// handleRgb20DepositSign/handleRgb20WithdrawSign 报 "rgb20 adapter not configured"，
	// 不参与 deposit/withdraw 的 GG18 组签名（组签名超时）。
	if err := n.initRgb20Adapter(); err != nil {
		log.Error("Init", "initRgb20Adapter error", err)
		return err
	}
	if !n.cfg.IsOfficialNode {
		return nil
	}
	n.withdrawReqChan = make(chan *rtypes.PendingTx, 256)
	cs, err := neutrino.NewChainService(n.neutrinoCfg)
	if err != nil {
		log.Error("Init", "NewChainService error", err)
		_ = n.neutrinoCfg.Database.Close()
		return err
	}
	n.neutrinoCS = cs
	bw, err := newBtcWallet(n)
	if err != nil {
		log.Error("Init", "newBtcWallet error", err)
		return err
	}
	n.bw = bw
	n.rgbx = newRGBX()
	return nil

}

// initRgb20Adapter 按配置构造 RGB20 侧车适配器（所有节点都需要：签名节点用只读校验）。
func (n *neutrinoClient) initRgb20Adapter() error {
	if n.cfg.Rgb20.SidecarAddr == "" {
		// 未配置侧车，跳过（BTC-only 部署）。
		log.Debug("initRgb20Adapter rgb20 not configured")
		return nil
	}
	rgbCfg := rgb20.Config{
		SidecarAddr:       n.cfg.Rgb20.SidecarAddr,
		ConsignmentListen: n.cfg.Rgb20.ConsignmentListen,
		Precision:         n.cfg.Rgb20.Precision,
		ChangeAddress:     n.cfg.Rgb20.ChangeAddress,
		MinConfirmations:  n.cfg.BlockConfirmations,
	}
	for _, c := range n.cfg.Rgb20.Contracts {
		rgbCfg.Contracts = append(rgbCfg.Contracts, rgb20.Contract{
			Symbol:        c.Symbol,
			SidecarSymbol: c.SidecarSymbol,
			AssetID:       c.AssetID,
			Precision:     c.Precision,
			MinDeposit:    c.MinDeposit,
			MinWithdraw:   c.MinWithdraw,
		})
	}
	adapter, err := rgb20.NewAdapter(rgbCfg, rgb20.NewWalletStore(n.neutrinoCfg.Database))
	if err != nil {
		return err
	}
	adapter.SetBridge(n) // n 实现 rgb20.Chain33Bridge
	n.rgb20 = adapter
	log.Info("initRgb20Adapter", "sidecarAddr", n.cfg.Rgb20.SidecarAddr,
		"contracts", len(rgbCfg.Contracts))
	return nil
}

// Start starting routine
func (n *neutrinoClient) Start() {

	n.initCommitKey()
	n.tss.start()
	go n.subMsg()
	go n.cleanUp()
	// RGB20 侧车连接：所有节点都需要（签名节点只读校验 ValidateConsignment/交叉核对）。
	// 侧车可能晚于本节点启动（需先拿到 GG18 公钥再起侧车），这里后台重试。
	if n.rgb20 != nil {
		go func() {
			n.waitUntilDone("rgb20 connect", func() bool {
				if err := n.rgb20.Connect(n.ctx); err != nil {
					log.Debug("Start rgb20 connect retry", "err", err)
					return false
				}
				return true
			}, time.Second*3)
			log.Info("Start rgb20 sidecar connected")
		}()
	}
	if !n.cfg.IsOfficialNode {
		return
	}
	if err := n.neutrinoCS.Start(); err != nil {
		log.Error("Start", "neutrinoCS start error", err)
		_ = n.neutrinoCfg.Database.Close()
		panic(err)
	}

	go n.handleBlockSync()
	go n.submitBitcoinHeaders()
	// 依赖tss地址的任务需要等待tss完成
	n.waitUntilDone("waitDKGCompleted", func() bool {
		return n.tss.isDKGCompleted()
	}, time.Second*3)
	if err := n.bw.start(); err != nil {
		log.Error("Start", "btcwallet start error", err)
		n.bw.stop()
		panic(err)
	}
	go n.depositWatcher()
	go n.withdrawalProcessor()
	n.rgbx.start(n)
	// RGB20 官方节点：充值轮询 + HTTP（consignment 上传/充值请求）。Start 非阻塞，后台等待侧车连接。
	if n.rgb20 != nil {
		if err := n.rgb20.Start(n.ctx); err != nil {
			log.Error("Start", "rgb20 start error", err)
		}
	}
}

// handle subscription messages
func (n *neutrinoClient) subMsg() {

	n.qclient.Sub(moduleName)
	for {

		select {
		case <-n.ctx.Done():
			return
		case msg := <-n.qclient.Recv():

			if msg == nil {
				log.Error("SubMsg", "err", "receive nil msg")
				return
			}
			data, ok := msg.Data.(*types.TopicData)
			if msg.Ty == types.EventReceiveSubData && ok && data.Topic == tssSignNotifyTopic {
				n.tss.subChan <- data
			} else {
				log.Error("SubMsg receive invalid msg", "ty", msg.Ty, "ok", ok)
			}
		}
	}
}

func (n *neutrinoClient) cleanUp() {

	<-n.ctx.Done()
	if n.rgb20 != nil {
		n.rgb20.Stop()
	}
	if n.cfg.IsOfficialNode {
		if err := n.neutrinoCS.Stop(); err != nil {
			log.Error("cleanUp Unable to stop neutrino server", "err", err)
		}
		n.bw.stop()
	}
	if err := n.neutrinoCfg.Database.Close(); err != nil {
		log.Error("cleanUp Unable to close neutrino db", "err", err)
	}
}

func (n *neutrinoClient) handleBlockSync() {

	n.syncBestBlock()
	interval := time.Duration(n.cfg.BtcBlockInterval)/3 + 1
	ticker := time.NewTicker(time.Second * interval)
	checkTipTicker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	defer checkTipTicker.Stop()
	for {

		select {

		case <-n.ctx.Done():
			return
		case <-ticker.C:

			n.syncBestBlock()
		case <-checkTipTicker.C:
			_, blkTip, err1 := n.neutrinoCS.BlockHeaders.ChainTip()
			_, filTip, err2 := n.neutrinoCS.RegFilterHeaders.ChainTip()
			if err1 != nil || err2 != nil {
				log.Warn("read header tip failed", "blkErr", err1, "filErr", err2)
				return
			}
			log.Info("checkTip", "blockTip", blkTip, "filterTip", filTip, "isCurrent", n.neutrinoCS.IsCurrent())
			if blkTip > filTip+1 {
				// 已经有 block header 但 cfheader 跟不上，且不是仅差 1 的同步窗口
				log.Error("cfheaders lagging block headers; check btcd --peerblockfilters",
					"blockTip", blkTip, "filterTip", filTip)
			}
		}
	}
}

func (n *neutrinoClient) syncBestBlock() {
	blk, err := n.neutrinoCS.BestBlock()
	if err != nil {
		log.Error("syncBestBlock", "err", err)
		return
	}
	// log.Debug("syncBestBlock", "height", blk.Height, "hash", blk.Hash.String())
	n.setBestBlock(blk)
}

func (n *neutrinoClient) getBestBlock() *headerfs.BlockStamp {
	n.lock.RLock()
	defer n.lock.RUnlock()
	return n.bestBlock
}

func (n *neutrinoClient) setBestBlock(blk *headerfs.BlockStamp) {
	n.lock.Lock()
	defer n.lock.Unlock()
	if blk != nil {
		n.bestBlock = blk
	}
}

func (n *neutrinoClient) getBestBlockHeight() int32 {
	n.lock.RLock()
	defer n.lock.RUnlock()
	if n.bestBlock != nil {
		return n.bestBlock.Height
	}
	return 0
}
