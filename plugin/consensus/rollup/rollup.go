package rollup

import (
	"context"
	"runtime"
	"time"

	"github.com/33cn/chain33/common/log"
	"github.com/33cn/chain33/rpc/grpcclient"
	"github.com/33cn/chain33/system/consensus"
	"github.com/33cn/chain33/types"
)

const (
	minCommitTxCount          = 128
	eachValidatorCommitRounds = 10
)

var (
	rlog = log.New("module", "rollup")
)

func init() {
	consensus.RegCommitter("rollup", &RollUp{})
}

// RollUp roll up
type RollUp struct {
	nextBuildHeight int64
	nextBuildRound  int64
	initFragIndex   int32
	cfg             Config

	initDone chan struct{}

	ctx                  context.Context
	cancel               context.CancelFunc
	base                 *consensus.BaseClient
	chainCfg             *types.Chain33Config
	subChan              chan *types.TopicData
	minBuildRoundInCache int64
	mainChainGrpc        types.Chain33Client
	val                  *validator
	cache                *commitCache
	cross                *crossTxHandler
	lastFeeRate          int64
}

// Init init
func (r *RollUp) Init(base *consensus.BaseClient, chainCfg *types.Chain33Config, subCfg []byte) {

	if !chainCfg.IsPara() {
		return
	}

	types.MustDecode(subCfg, &r.cfg)

	r.chainCfg = chainCfg
	r.ctx, r.cancel = context.WithCancel(base.Context)
	r.initDone = make(chan struct{})
	r.subChan = make(chan *types.TopicData, 32)
	r.lastFeeRate = 100000
	r.cross = &crossTxHandler{}
	r.base = base

	var err error
	r.mainChainGrpc, err = grpcclient.NewMainChainClient(chainCfg, chainCfg.GetModuleConfig().RPC.MainChainGrpcAddr)
	if err != nil {
		panic("init main chain grpc client err:" + err.Error())
	}

	go r.initJob()
	go r.startRollupRoutine()
}

func (r *RollUp) initJob() {

	valPubs := r.getValidatorPubKeys()
	status := r.getRollupStatus()
	for len(valPubs.GetBlsPubs()) == 0 || status == nil {
		rlog.Warn("Init rollup wait 5 seconds...", "status", status, "valPubs", valPubs)
		time.Sleep(5 * time.Second)
		valPubs = r.getValidatorPubKeys()
		status = r.getRollupStatus()
	}
	r.val = &validator{}
	r.val.init(r.cfg, valPubs, status)
	r.nextBuildRound = status.CommitRound + 1
	r.initFragIndex = status.BlockFragIndex
	r.nextBuildHeight = status.CommitBlockHeight + 1
	if status.BlockFragIndex > 0 {
		r.nextBuildHeight = status.CommitBlockHeight
	}
	r.cache = newCommitCache(status.CommitRound)
	r.cross.init(r, status)
	r.trySubTopic(psValidatorSignTopic)
	r.initDone <- struct{}{}

}

func (r *RollUp) startRollupRoutine() {

	<-r.initDone

	if r.val.enable {
		go r.handleExit()
		go r.handleBuildBatch()
		go r.handleCommit()
		go r.syncRollupState()

		n := runtime.NumCPU()

		for i := 0; i < n; i++ {

			go r.handleSubMsg()
		}
	}
}

func (r *RollUp) handleExit() {

	for {

		select {
		case <-r.ctx.Done():
			return
		case <-r.val.exit:
			r.cancel()
			rlog.Info("rollup exit")
			return
		}
	}
}

// 同步链上已提交的最新 blockHeight 和 commitRound, 维护batch缓存
func (r *RollUp) syncRollupState() {

	ticker := time.NewTicker(time.Duration(r.cfg.CommitInterval) * time.Second)

	for {

		select {
		case <-ticker.C:
			valPubs := r.getValidatorPubKeys()
			status := r.getRollupStatus()

			if len(valPubs.GetBlsPubs()) > 0 {
				r.val.updateValidators(valPubs)
			}

			if status != nil {
				r.val.updateRollupStatus(status)
				r.cache.cleanHistory(status.CommitRound)
			}

		case <-r.ctx.Done():
			ticker.Stop()
			return
		}

	}

}
