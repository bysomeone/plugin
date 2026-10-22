package neutrino

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20"
	"github.com/btcsuite/btcd/btcjson"
)

/*
 * 扫集触发（C4，Go 侧）：把散在用户 P2WSH 充值地址上的 BTC 归集回主池。
 *
 * 为什么需要它：P2WSH 充值地址上线后，用户的充值**不再进主池**（钱停在各自的 P2WSH 上），
 * 而提现只能花主池的 BTC —— 没有扫集，"钱进得去、出不来"。
 *
 * 时序（规格 §6① 选项 A）：扫集是**纯 UTXO 整理**，不改变任何 RGB 账（入账在铸币那一刻就已经
 * 完成），所以它不卡在铸币的前后，只是一条闲时 ticker：攒够 minUtxos 笔再合并成一次
 * （UTXO 太少时手续费不划算），攒不够就静默跳过。
 *
 * 触发方是**发放充值地址的那个节点**：充值 UTXO 的归属（watch 集）只有它（以及和它共用侧车的
 * 节点）看得见。注意这里不自己枚举 UTXO —— "值不值得扫"由侧车按同一份 watch 集回答
 * （`BuildSweep` 的 input_count == 0 表示"没有值得扫的"），Go 侧据此只是决定要不要发起。
 *
 * 失败一律不致命：扫集是可重试的 UTXO 整理，失败只影响流动性（提现暂时没有主池 BTC 可花），
 * 不影响任何账。所以循环里只记日志，下一轮再来。
 */

const (
	// defaultSweepIntervalSeconds 触发检查间隔默认值。
	defaultSweepIntervalSeconds = 300
	// defaultSweepMinUtxos 触发阈值默认值。
	defaultSweepMinUtxos = 5
	// defaultSweepFeeRate 默认费率（sat/vB），也是动态估算的下限。
	defaultSweepFeeRate = 2
	// defaultSweepMaxFeeRate 费率硬上限默认值（sat/vB）：估算值超过它时本轮不扫。
	// 扫集只是 UTXO 整理，宁可让充值 UTXO 多停一会儿，也不为一笔整理交易付离谱的矿工费。
	defaultSweepMaxFeeRate = 50
	// sweepFeeEstimateConfTarget 动态费率估算的目标确认数（estimatesmartfee）。
	sweepFeeEstimateConfTarget = 6
	// defaultSweepMinConfirmations 只归集达到该确认数的充值 UTXO（扫集要付真实矿工费，
	// 花未确认的充值 UTXO 会在充值交易被重组时连带把扫集也作废）。
	defaultSweepMinConfirmations = 6
)

// sweepFeeRateEstimateFn 查询 btcd 的动态费率估算（FeeRate 单位 BTC/kvB）。
//
// 默认走 btcwallet 的 btcd RPC 客户端；btcRPC 未配置时返回错误（调用方回退到配置费率）。
// 抽成变量是为了让测试覆盖"估算成功 / 无结果 / RPC 失败"三种分支，不改变生产路径。
var sweepFeeRateEstimateFn = func(n *neutrinoClient) (*btcjson.EstimateSmartFeeResult, error) {
	if n == nil || n.bw == nil || n.bw.rpcClient == nil {
		return nil, errors.New("btc rpc client not configured")
	}
	return n.bw.rpcClient.EstimateSmartFee(sweepFeeEstimateConfTarget, nil)
}

// sweepSettings 扫集的有效配置（把"未配置"折成默认值，只算一次）。
type sweepSettings struct {
	interval         time.Duration
	minUtxos         uint32
	feeRate          uint32
	maxFeeRate       uint32
	minConfirmations uint32
}

func (n *neutrinoClient) sweepSettings() sweepSettings {
	c := n.cfg.UserDepositSweep
	s := sweepSettings{
		interval:         time.Second * defaultSweepIntervalSeconds,
		minUtxos:         defaultSweepMinUtxos,
		feeRate:          defaultSweepFeeRate,
		maxFeeRate:       defaultSweepMaxFeeRate,
		minConfirmations: defaultSweepMinConfirmations,
	}
	if c.IntervalSeconds > 0 {
		s.interval = time.Second * time.Duration(c.IntervalSeconds)
	}
	if c.MinUtxos > 0 {
		s.minUtxos = uint32(c.MinUtxos)
	}
	if c.FeeRate > 0 {
		s.feeRate = uint32(c.FeeRate)
	}
	if c.MaxFeeRate > 0 {
		s.maxFeeRate = uint32(c.MaxFeeRate)
	}
	if c.MinConfirmations > 0 {
		s.minConfirmations = uint32(c.MinConfirmations)
	}
	return s
}

// sweepFeeRate 决定本轮扫集实际使用的费率（sat/vB）。ok=false 表示本轮跳过。
//
// 每次扫集前向 btcd 估算费率（estimatesmartfee，6 个确认目标）：
//   - RPC 失败 / 无 feerate（regtest 常态）/ 结果为 0 → 回退到配置费率；
//   - 估算值 > maxFeeRate → **本轮跳过**（不降级到上限继续扫，下一轮再看）；
//   - 估算值 < 配置费率 → 用配置费率（下限，避免费率过低长期不上链）。
//
// 费率估算失败绝不让扫集整体失败：它只影响本轮用哪个费率，扫集本身照常发起。
func (n *neutrinoClient) sweepFeeRate(s sweepSettings) (uint32, bool) {
	rate := s.feeRate
	res, err := sweepFeeRateEstimateFn(n)
	switch {
	case err != nil:
		log.Info("sweepFeeRate fee estimation failed, fall back to the configured feeRate",
			"err", err, "feeRate", s.feeRate)
	case res == nil || res.FeeRate == nil || *res.FeeRate <= 0:
		log.Debug("sweepFeeRate no fee estimate available, fall back to the configured feeRate",
			"feeRate", s.feeRate)
	default:
		// BTC/kvB → sat/vB 是 ×1e5；向上取整，避免估算值被截断后略低于实际需求。
		estimated := math.Ceil(*res.FeeRate * 1e5)
		if estimated > float64(s.maxFeeRate) {
			log.Info("sweepFeeRate estimated fee rate exceeds the configured cap, skip this round",
				"estimatedFeeRate", estimated, "maxFeeRate", s.maxFeeRate)
			return 0, false
		}
		if uint32(estimated) > rate {
			rate = uint32(estimated)
		}
	}
	// 配置自身矛盾（feeRate > maxFeeRate）时，回退 / 下限取到的费率同样会突破硬上限：
	// 按硬上限语义跳过，并把矛盾写进日志，而不是静默按超上限的费率扫集。
	if rate > s.maxFeeRate {
		log.Info("sweepFeeRate configured feeRate exceeds maxFeeRate, skip this round",
			"feeRate", rate, "maxFeeRate", s.maxFeeRate)
		return 0, false
	}
	return rate, true
}

// sweepWorker 闲时扫集循环（只在官方节点 + userDepositSweep.enable 打开时启动）。
func (n *neutrinoClient) sweepWorker() {
	s := n.sweepSettings()
	log.Info("sweepWorker start", "interval", s.interval, "minUtxos", s.minUtxos,
		"feeRate", s.feeRate, "maxFeeRate", s.maxFeeRate, "minConfirmations", s.minConfirmations)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-ticker.C:
			n.sweepOnce(s)
		}
	}
}

// sweepOnce 发起一次扫集；"没有值得扫的"、"侧车未就绪"与"估算费率超上限"都只是静默返回
// （超上限会打一条 INFO，说明估算值与上限）。
func (n *neutrinoClient) sweepOnce(s sweepSettings) {
	if n.rgb20 == nil || !n.rgb20.IsConnected() {
		log.Debug("sweepOnce sidecar not connected, skip")
		return
	}
	feeRate, ok := n.sweepFeeRate(s)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(n.ctx, 2*time.Minute)
	defer cancel()
	res, err := n.rgb20.Sweep(ctx, &rgb20.SweepRequest{
		FeeRate:          feeRate,
		MinUtxos:         s.minUtxos,
		MinConfirmations: s.minConfirmations,
	})
	if err != nil {
		// 失败不致命：扫集是纯 UTXO 整理，下一轮再来。
		log.Error("sweepOnce sweep failed, retry next round", "err", err)
		return
	}
	if res == nil {
		log.Debug("sweepOnce nothing to sweep", "minUtxos", s.minUtxos, "minConfirmations", s.minConfirmations)
		return
	}
	log.Info("sweepOnce swept user deposit utxos into the main pool",
		"btcTxid", res.Txid, "inputs", res.InputCount,
		"inputValue", res.InputValue, "fee", res.Fee)
}
