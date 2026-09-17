// Copyright Fuzamei Corp. 2018 All Rights Reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package neutrino integrate btc light client neutrino
package neutrino

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/33cn/chain33/client"
	"github.com/33cn/chain33/common/address"
	"github.com/33cn/chain33/common/crypto"
	"github.com/33cn/chain33/queue"
	"github.com/33cn/chain33/rpc/grpcclient"
	"github.com/33cn/chain33/system/crypto/secp256k1"
	"github.com/33cn/chain33/system/crypto/tss/cggmp"
	"github.com/33cn/chain33/types"
	"github.com/lightninglabs/neutrino/headerfs"

	"github.com/33cn/chain33/common/log/log15"
	"github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient"
	"github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcwallet/walletdb"
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

	// deposits 用户 P2WSH 充值脚本的 watch 集（program ↔ userID，见 deposit_address.go）。
	// **所有节点都要有**：签名节点凭它认定"这个输入脚本是用户充值脚本"（IsUserDepositScript），
	// 没有它，任何签名节点都会拒签花这笔 UTXO 的交易（C4 的跨节点分发要解决的就是这件事）。
	deposits *depositScriptSet
	// depositImporter 把充值脚本导入**本节点钱包**并订阅（只有真的在 watch 的官方节点才装）。
	// nil = 本节点不 watch（不跑交易监听），只维护登记本身。
	depositImporter func(witnessScripts [][]byte) error
	// depositsLoaded loadDepositScripts 是否已经跑完一轮（官方节点在钱包起来时跑，见
	// waitAndImportTSSAddress）。重复载入本身幂等，但会多触发一次 NotifyReceived ⇒ 一次 rescan，
	// 所以轮询那边只在没人载入过时才补一次。
	depositsLoaded atomic.Bool
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
	// 不参与 deposit/withdraw 的 CGGMP 组签名（组签名超时）。
	if err := n.initRgb20Adapter(); err != nil {
		log.Error("Init", "initRgb20Adapter error", err)
		return err
	}
	// watch 集必须在**所有**节点上存在（含不跑钱包的验证节点）：它是签名节点判断"这个输入脚本
	// 是不是用户充值脚本"的依据，见 IsUserDepositScript。
	n.deposits = newDepositScriptSet()
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
		// 无上下文的测试签名（E2E 的 sign-psbt 端点）：按配置透传，默认关闭。
		TestSignPsbt: n.cfg.Rgb20.TestSignPsbt,
		// 头链保留深度 B：头链只提交到 best - B，充值提交前的本地深度门控据此把链上判据换算成本地判据。
		// 与头链提交（bitcoin.go 的 btcConfirmedHeight）取同一个配置项，不允许各自取值。
		HeaderRelayConfirmations: n.cfg.BlockConfirmations,
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
	// TSS share ↔ 链上组公钥一致性自检（fail-closed，见 checkTssShareAgainstChain）。
	// 放在 tss/rgbx 任何后台流程之前：不一致的节点一旦跑起来，就会以"能参与签名但产出无人认"
	// 的静默失灵形态存在，比直接拒绝启动危险得多。
	if err := n.checkTssShareAgainstChain(); err != nil {
		if !n.cfg.Tss.AllowShareMismatch {
			log.Error("Start tss share consistency check failed, refuse to start", "err", err)
			panic(err)
		}
		log.Error("Start tss share consistency check failed, tolerated by tss.allowShareMismatch=true (escape hatch)",
			"err", err)
	}
	n.tss.start()
	go n.subMsg()
	go n.cleanUp()
	// RGB20 侧车连接：所有节点都需要（签名节点只读校验 ValidateConsignment/交叉核对）。
	// 侧车可能晚于本节点启动（需先拿到 TSS 群公钥再起侧车），这里后台重试。
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
	// 跨节点 watch 集补齐（C4）：**每个节点**都下发自己那份登记并接收侧车持有的并集 ——
	// 签名节点必须各自持有"哪些脚本是用户充值脚本"的登记，才能为花费它的交易出签名
	// （IsUserDepositScript）。见 deposit_address.go 的 syncDepositScriptsWithSidecar。
	if n.deposits != nil {
		go n.depositScriptSyncWorker()
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
	// 充值地址发放 HTTP（每用户 P2WSH 脚本按需 import，见 deposit_http.go / deposit_address.go）。
	// 未配置时不开启，但要把后果说清楚：桥不会 watch 任何新用户的充值脚本，打到每用户地址上的
	// BTC 除非已在 watch 集里（重启后从 neutrino.db 载入）否则**看不见**。
	// 扫集（C4）：把用户 P2WSH 上的充值 UTXO 闲时归集回主池 —— 没有它，P2WSH 充值地址上线后
	// 提现会因为"主池没有 BTC 可花"而卡死（钱进得去、出不来）。只在发放充值地址的那个节点上开。
	if n.cfg.UserDepositSweep.Enable {
		go n.sweepWorker()
	} else {
		log.Info("user deposit sweep is disabled (neutrino.userDepositSweep.enable=false): " +
			"BTC deposited to per-user P2WSH addresses stays there until it is swept, so withdrawals " +
			"have no main-pool BTC to spend unless something else refills the pool")
	}
	if n.cfg.DepositAddressListen != "" {
		go n.serveDepositAddressHTTP(n.cfg.DepositAddressListen)
	} else {
		log.Warn("deposit address issuance is disabled (neutrino.depositAddressListen is empty): " +
			"per-user P2WSH deposit scripts can no longer be imported on demand, deposits to addresses " +
			"that are not already watched will be invisible to the bridge")
	}
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

// ----------------------------------------------------------------------------
// TSS share ↔ 链上组公钥一致性自检（启动路径，见 Start）
//
// 危险态：某节点的 share 与链上 CrossChainInfo.pubkey 不一致（share 丢失后自动 re-DKG、换过钥、
// 或从别的环境恢复过数据）。该节点仍能"参与签名"，但产出的签名链上/其他节点不认 —— 表现是
// 静默失灵而不是报错：链上 CrossChainInfo 每个 symbol 只有一份、且写死不可改，re-DKG 换出的新
// 组公钥永远提交不上去（CommitDKG 被 `duplicate` 静默吞掉，见 submitMainChainTxUntilSuccess）。
//
// 自检在中继启动路径上做一次，不一致即 ERROR + 拒绝启动（fail-closed；逃生阀 tss.allowShareMismatch
// 默认关闭）。三种情形**不判定**（不把正常的当异常）：
//  1. 链上还没有该 symbol 的 CrossChainInfo（DKG 尚未 commit，或主链查询不可用 / 不支持）→ 不判定；
//  2. 本地还没有 DKG 结果（首次启动、DKG 还没跑）→ 不判定；
//  3. 本地 DKG 结果读出来但解不开（记录损坏）→ 不判定（WARN；这条路径的既有处置是重新 DKG，
//     不在这里改变它的行为）。
const (
	// shareCheckQueryTimeout 单次链上查询的超时。主链 grpc 的 QueryChain 在并发/时序下可能
	// 永久 hang（见 rpc.go getCrossChainInfo 的说明），而自检位于启动路径，不允许卡死启动。
	shareCheckQueryTimeout = 8 * time.Second
	// shareCheckQueryAttempts 查询失败时的总尝试次数。
	shareCheckQueryAttempts = 2
	// shareCheckQueryInterval 相邻两次尝试的间隔。
	shareCheckQueryInterval = time.Second
)

// errTssShareMismatch 本地 share 与链上 CrossChainInfo 不一致（危险态，默认拒绝启动）。
// 错误信息自带处置办法：panic 出去的栈里只有它，运维要能照着做。
type errTssShareMismatch struct {
	mismatches []string
}

func (e *errTssShareMismatch) Error() string {
	return fmt.Sprintf("local tss share does not match the on-chain cross chain info: [%s]; "+
		"the on-chain group key is per-symbol and immutable (a re-DKG cannot replace it, its CommitDKG "+
		"is rejected as duplicate), so this node would keep producing signatures that the chain and the "+
		"other nodes reject; fix by restoring the matching neutrino.db, or rebuild the whole group with a "+
		"fresh chain. Escape hatch (only to get a node up temporarily, do NOT leave it on): "+
		"tss.allowShareMismatch=true in [rpc.sub.light.neutrino.tss]",
		strings.Join(e.mismatches, " | "))
}

// checkTssShareAgainstChain 自检本地 share（DKG 组公钥）与每个已知 symbol 的链上 CrossChainInfo。
// 返回 nil 表示一致或无法判定；返回 *errTssShareMismatch 表示两边都有且不一致。
func (n *neutrinoClient) checkTssShareAgainstChain() error {
	localPub, err := n.loadTssGroupPubKeyFromDB()
	if err != nil {
		log.Warn("checkTssShareAgainstChain local dkg result unreadable, skip check", "err", err)
		return nil
	}
	if localPub == nil {
		// 情形②：本地还没有 DKG 结果（首次启动 / DKG 还没跑）。
		log.Info("checkTssShareAgainstChain no local dkg result, skip check")
		return nil
	}
	symbols := n.shareCheckSymbols()
	mismatches := checkShareAgainstChainInfos(localPub, symbols, n.queryCrossChainInfoBounded)
	if len(mismatches) > 0 {
		return &errTssShareMismatch{mismatches: mismatches}
	}
	log.Info("checkTssShareAgainstChain passed", "symbols", symbols,
		"localGroupPubkey", fmt.Sprintf("%x", localPub.SerializeCompressed()))
	return nil
}

// shareCheckSymbols 自检覆盖的 symbol 集合：BTC（链上 CrossChainInfo 不带 pubkey，按 hash160 比对）
// + 配置里的每个 RGB20 合约，去重并保持顺序。
func (n *neutrinoClient) shareCheckSymbols() []string {
	symbols := make([]string, 0, 1+len(n.cfg.Rgb20.Contracts))
	seen := make(map[string]bool, 1+len(n.cfg.Rgb20.Contracts))
	add := func(symbol string) {
		if symbol == "" || seen[symbol] {
			return
		}
		seen[symbol] = true
		symbols = append(symbols, symbol)
	}
	add(rtypes.BTCSymbol)
	for _, c := range n.cfg.Rgb20.Contracts {
		add(c.Symbol)
	}
	return symbols
}

// loadTssGroupPubKeyFromDB 从中继自己的 neutrino.db 读 CGGMP DKG 结果（bucket rgbx-tss /
// key cggmp-dkg-result，与 tss.go 的 loadDKGFromDB 同一份数据）并解出组公钥。
//
// 读不到（bucket/key 不存在 = 首次启动、DKG 还没跑、数据目录被清）返回 (nil, nil) —— 情形②；
// 读得到但解不开（记录损坏）返回 error —— 情形③，由调用方按"不判定"处理。
//
// 不复用 tssService.loadDKGFromDB：那个方法顺带写 t.tssPublicKey/t.tssAddress，从启动路径调用会与
// tss 自己的 goroutine 并发写同一批字段。这里只读 DB，不碰 tss 状态。
func (n *neutrinoClient) loadTssGroupPubKeyFromDB() (*btcec.PublicKey, error) {
	if n.neutrinoCfg.Database == nil {
		return nil, nil
	}
	var dkgData []byte
	err := walletdb.View(n.neutrinoCfg.Database, func(tx walletdb.ReadTx) error {
		bucket := tx.ReadBucket([]byte(tssBucketName))
		if bucket == nil {
			return walletdb.ErrBucketNotFound
		}
		dkgData = bucket.Get([]byte(dkgResultKey))
		if dkgData == nil {
			return types.ErrNotFound
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, types.ErrNotFound) || errors.Is(err, walletdb.ErrBucketNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var dkgResult cggmp.DKGResult
	if err := json.Unmarshal(dkgData, &dkgResult); err != nil {
		return nil, fmt.Errorf("decode local cggmp dkg result: %w", err)
	}
	pub, err := groupPubKeyFromDKG(&dkgResult)
	if err != nil {
		return nil, fmt.Errorf("parse local dkg group pubkey: %w", err)
	}
	return pub, nil
}

// errCrossChainInfoNotOnChain 链上还没有该 symbol 的 CrossChainInfo（DKG 尚未 commit）。
// 与"查询不可用"分开只为了日志级别与重试策略：前者是启动早期的正常形态（Info、不重试），
// 后者才是故障（WARN、可重试）。
var errCrossChainInfoNotOnChain = errors.New("cross chain info not found on chain")

// crossChainInfoAbsentOnChain 判断链上是否**没有**该 symbol 的 CrossChainInfo。
//
// 判据必须落在**记录内容**上，不能只看查询有没有报错：执行器的 Query_GetCrossChainInfo
// （plugin/dapp/rgbx/executor/query.go）在 symbol 不存在时返回**空结构体 + nil error**
// （既有契约，被 executor/query_test.go 钉住），于是"查得到"与"链上有"是两件事。
//
// 为什么这条判据曾经缺失、以及为什么它必须存在（踩过一次，别再踩）：
//   - 缺了它，空记录会以 err == nil 流进 commitDKGToChain 的"链上是另一把钥"分支（那是
//     **不可自愈**的形态：同一 symbol 只能提交一次，换钥会被 ErrDuplicateDKGCommit 挡住），
//     于是该节点永远不会去提交，链上永远没有记录 —— 表现是 DKG 阶段**死锁**，不是慢。
//   - 以前没暴露是因为旧环境是**原地续跑**的：链上早已有旧代码提交的 CrossChainInfo，
//     commitDKGToChain 走"确认分支"就过了，压根没进过提交分支。只有全新 reset
//     （链上没有任何 CrossChainInfo）才必须真的走提交分支，那时才会撞上这个误判。
func crossChainInfoAbsentOnChain(info *rtypes.CrossChainInfo) bool {
	return info.GetTssAddress() == "" && len(info.GetPubkey()) == 0
}

// queryCrossChainInfoBounded 查询链上 CrossChainInfo，带超时与有限重试。
//
// 不复用 rpc.go 的 getCrossChainInfo：那个函数把 n.ctx（进程级、不超时）直接传给 QueryChain，
// 而自检在启动路径上，不能被主链 grpc 的 hang 拖住启动（该 hang 在本仓库有据可查）。
// 重试只针对传输层错误（不可达 / 超时）：执行器明确回"没有"是业务结果，重试不会变好。
//
// 空记录在这里就翻译成 errCrossChainInfoNotOnChain（见 crossChainInfoAbsentOnChain），
// 让**两个调用方**（commitDKGToChain 的提交/核对、自检的比对）都拿到"链上还没有"这个正确判据。
func (n *neutrinoClient) queryCrossChainInfoBounded(symbol string) (*rtypes.CrossChainInfo, error) {
	var lastErr error
	for i := 0; i < shareCheckQueryAttempts; i++ {
		if i > 0 {
			time.Sleep(shareCheckQueryInterval)
		}
		ctx, cancel := context.WithTimeout(n.ctx, shareCheckQueryTimeout)
		reply, err := n.mainChainGrpc.QueryChain(ctx, &types.ChainExecutor{
			Driver:   rtypes.RgbxX,
			FuncName: "GetCrossChainInfo",
			Param:    types.Encode(&types.ReqString{Data: symbol}),
		})
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		if !reply.GetIsOk() {
			return nil, fmt.Errorf("%w: symbol=%s msg=%s", errCrossChainInfoNotOnChain, symbol, string(reply.GetMsg()))
		}
		info := &rtypes.CrossChainInfo{}
		if err := types.Decode(reply.GetMsg(), info); err != nil {
			lastErr = err
			continue
		}
		if crossChainInfoAbsentOnChain(info) {
			// 查得到，但链上没有这条记录（执行器对不存在的 symbol 就是回空记录 + nil error）。
			return nil, fmt.Errorf("%w: symbol=%s msg=%s", errCrossChainInfoNotOnChain, symbol, string(reply.GetMsg()))
		}
		return info, nil
	}
	return nil, lastErr
}

// checkShareAgainstChainInfos 纯比对逻辑（与链的耦合只有 query 一个入口，便于单测）。
//
// localPub 是本地 DKG 组公钥，query 按 symbol 查链上 CrossChainInfo（error 表示查询不可用）。
// 返回所有不一致的 symbol 描述，空 = 全部一致或全部不判定。
func checkShareAgainstChainInfos(localPub *btcec.PublicKey, symbols []string,
	query func(symbol string) (*rtypes.CrossChainInfo, error)) []string {

	localPubBytes := localPub.SerializeCompressed()
	localPubHash := btcutil.Hash160(localPubBytes)
	var mismatches []string
	for _, symbol := range symbols {
		info, err := query(symbol)
		if err != nil {
			if errors.Is(err, errCrossChainInfoNotOnChain) {
				// 情形①：链上还没有该 symbol 的 CrossChainInfo（DKG 尚未 commit /
				// guardian 未凑齐）。启动早期的正常形态。
				log.Info("checkShareAgainstChainInfos cross chain info not committed yet, skip symbol",
					"symbol", symbol)
			} else {
				// 查询不可用（主链 hang / 插件未启用 / 非 rgbx 链）：没有判据，不判定。
				log.Warn("checkShareAgainstChainInfos query cross chain info failed, skip symbol",
					"symbol", symbol, "err", err)
			}
			continue
		}
		// 防御：查询成功但 CrossChainInfo 里连 TssAddress 都没有（不成形的记录）→ 不判定。
		if info == nil || info.GetTssAddress() == "" {
			log.Info("checkShareAgainstChainInfos empty cross chain info, skip symbol", "symbol", symbol)
			continue
		}
		switch {
		case len(info.GetPubkey()) > 0:
			// 链上带 TSS 组公钥，逐字节比对。**这是全部 symbol 的正常路径**：C1 起 checkCommitDKG
			// 对所有 symbol（含 BTC/XBTC）强制 33 字节压缩公钥（P2WSH 充值地址 = f(userID, tssPub)，
			// 靠的就是这把钥），桥侧对应的提交见 tss_commit.go 的 buildCommitDKGPayload。
			if !bytes.Equal(info.GetPubkey(), localPubBytes) {
				mismatches = append(mismatches, fmt.Sprintf(
					"symbol=%s localGroupPubkey=%x chainPubkey=%x chainTssAddress=%s",
					symbol, localPubBytes, info.GetPubkey(), info.GetTssAddress()))
			}
		case isP2WPKHScript(info.GetPkScript()):
			// 链上记录没有 pubkey 时的退化路径（C1 前提交的 BTC CrossChainInfo 属这种形态；
			// 硬切后新提交的一律带 pubkey，因此这条只剩防御意义）：比对
			// hash160(组公钥) == pkScript[2:]，与链上 checkCommitDKG 的判定口径一致、与网络无关。
			if !bytes.Equal(localPubHash, info.GetPkScript()[2:]) {
				mismatches = append(mismatches, fmt.Sprintf(
					"symbol=%s localGroupPubkeyHash160=%x chainPkScript=%x chainTssAddress=%s",
					symbol, localPubHash, info.GetPkScript(), info.GetTssAddress()))
			}
		default:
			// 链上既没有 pubkey 也没有可比对的 P2WPKH 脚本：没有判据，不判定。
			log.Warn("checkShareAgainstChainInfos no comparable on-chain field, skip symbol",
				"symbol", symbol, "chainTssAddress", info.GetTssAddress())
		}
	}
	return mismatches
}

// isP2WPKHScript pkScript 是否是 P2WPKH（OP_0 <20 字节 hash>）。
func isP2WPKHScript(pkScript []byte) bool {
	return len(pkScript) == 22 && pkScript[0] == txscript.OP_0 && pkScript[1] == 0x14
}
