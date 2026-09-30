package neutrino

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/33cn/chain33/types"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
)

/*
 * 跨链资产精度上链（本地侧车 ↔ 链上记录一致）。
 *
 * 问题：链上 `crossChainInfo` 是前端 / CLI 换算金额的唯一依据（"这个跨链资产该填几位小数"），
 * 而它的精度在 CommitDKG 那一刻往往还不知道 —— RGB20 资产的精度只能来自合约，合约又可能还没发行
 * / 还没被侧车采纳（"连侧车"与"等 DKG"是启动路径上两条独立的 goroutine）。所以：
 *
 *   - CommitDKG 只为 BTC 类资产写死精度（协议常量 8），RGB20 类只能是"尚未回填"哨兵；
 *   - 侧车连上、合约精度读出来之后，由本文件把真实值经 crossChainAssetMeta 回填上链（一次性）；
 *   - 回填之后再逐个与侧车读数核对：**对不上就拒绝启动**（fail-closed），与 TSS share 自检同一取向。
 *
 * 为什么回填的不是配置里的某个值：配置声明一份精度只会带来"两边不一致时按谁算"的静默分歧
 * （旧版就是这样：配置默认 6、合约发行 8，全程无人比对）。现在的单一真相是**合约**，链上那份只是
 * 它的登记副本，所以这里只做"把合约说的写上去 + 校对"，没有可填的配置项。
 *
 * 为什么这套逻辑放在 neutrino 包而不是 rgb20 包：它要读**主链状态**（crossChainInfo）并提交交易，
 * 而 rgb20 适配器只面向侧车（链上耦合全部经 Chain33Bridge 单点）。放在这里，两侧职责不混。
 */

// ErrCrossChainPrecisionMismatch 链上记录的精度与本节点侧车读到的合约精度不一致（或类别不符）。
// 静态分歧，重试不会变 —— 调用方 fail-closed 拒绝启动。
var ErrCrossChainPrecisionMismatch = errors.New("cross-chain asset precision mismatch")

const (
	// crossChainPrecisionVerifyInterval 两轮核对之间的间隔。
	crossChainPrecisionVerifyInterval = 3 * time.Second
	// crossChainPrecisionStallErrorAfter 状态未达成持续这么久之后，日志升级为 ERROR。
	// 与 commitDKG 同一口径（见 tss_commit.go）：等出块、钱包刚重启还没解锁都是正常形态，
	// 一上来就报 ERROR 只会把噪音当故障，持续不达成才是需要运维介入的形态。
	crossChainPrecisionStallErrorAfter = 30 * time.Second
	// crossChainPrecisionStallRepeatLogs 同一个原因每核对这么多次重报一条（首次与原因变化必报）。
	crossChainPrecisionStallRepeatLogs = 20
)

// errCrossChainPrecisionMismatchDetail 带具体分歧项的 mismatch 错误：
// panic 出去的栈里只有它，运维要能照着看清楚"哪个符号、链上是什么、合约是什么"。
type errCrossChainPrecisionMismatchDetail struct {
	mismatches []string
}

func (e *errCrossChainPrecisionMismatchDetail) Error() string {
	return fmt.Sprintf("%v: %s; the on-chain precision is what the frontend and the CLI use to scale "+
		"amounts, so a bridge whose contract disagrees with it would convert money at the wrong scale; "+
		"fix the chain record (the backfill is write-once, so a wrong value needs a fresh chain) or the "+
		"contract, then restart", ErrCrossChainPrecisionMismatch, strings.Join(e.mismatches, " | "))
}

// Is 让 errors.Is(err, ErrCrossChainPrecisionMismatch) 成立（调用方按类型判定）。
func (e *errCrossChainPrecisionMismatchDetail) Is(target error) bool {
	return errors.Is(target, ErrCrossChainPrecisionMismatch)
}

// ensureCrossChainAssetPrecisions 让链上每个已注册合约的精度等于侧车读到的合约精度，并核对到一致为止。
//
// 步骤（全部在侧车连上、且合约精度本地校验通过之后）：
//  1. 从侧车读每个合约声明的精度（rgb20.ContractPrecisions；合约没声明精度 / 侧车不认识 → panic）；
//  2. 对每个符号读链上 crossChainInfo：
//     - 记录还没上链（DKG 尚未提交）→ 等待（不是错误，会自愈）；
//     - 链上还是"尚未回填"哨兵 → 提交一次 crossChainAssetMeta 回填，然后继续轮询；
//     - 链上已等于合约精度 → 该项通过；
//     - 链上已是不等于合约精度的其它值（或类别不是 RGB20）→ **不可自愈**，panic 拒绝启动。
//
// 注意"回填"只提交一次（提交失败会重试提交，与 commitDKGToChain 同一形状）：判据始终是
// **链上状态**，而不是"提交有没有报错" —— 桥可能在链上同一条记录上收到 duplicate / 其它拒绝，
// 那都不代表达成。
func (n *neutrinoClient) ensureCrossChainAssetPrecisions() {
	precisions, err := n.rgb20.ContractPrecisions(n.ctx)
	if err != nil {
		// 到这里说明 Connect 之后的又一次读取失败：静态分歧（合约没声明精度 / 侧车不认识合约）
		// 或暂时不可用。两者都不能带着走 —— 精度算错是静默的 10^n 倍金额错误。
		log.Error("ensureCrossChainAssetPrecisions read contract precisions failed, refuse to start", "err", err)
		panic(err)
	}
	if len(precisions) == 0 {
		log.Info("ensureCrossChainAssetPrecisions no registered contract declares a precision " +
			"(no rgb20.contracts configured, or every contract still lacks its assetId), " +
			"nothing to record on chain")
		return
	}
	symbols := make([]string, 0, len(precisions))
	for symbol := range precisions {
		symbols = append(symbols, symbol)
	}
	// 固定顺序：日志与错误信息要稳定（map 遍历顺序随机会让同一故障每次报得不一样）。
	sort.Strings(symbols)
	log.Info("ensureCrossChainAssetPrecisions start", "symbols", symbols)

	n.ensureCrossChainAssetPrecisionsWith(n.ctx, symbols, precisions,
		n.queryCrossChainInfoBounded, n.submitMainChainTx)
}

// ensureCrossChainAssetPrecisionsWith 是 ensureCrossChainAssetPrecisions 的实现，依赖显式传入（便于单测）。
func (n *neutrinoClient) ensureCrossChainAssetPrecisionsWith(ctx context.Context,
	symbols []string, precisions map[string]uint32,
	query func(symbol string) (*rtypes.CrossChainInfo, error),
	submit func(exec, action string, payload types.Message) (string, error)) {

	// submitted 按符号记录"本节点已经成功投递过一次回填"，避免每 3s 灌一份同样的载荷进 mempool。
	submitted := make(map[string]bool, len(symbols))
	start := time.Now()
	var reported string
	var lastReportAt int

	for attempt := 1; ; attempt++ {
		select {
		case <-ctx.Done():
			log.Warn("ensureCrossChainAssetPrecisions client shutting down, stop verifying")
			return
		default:
		}

		var waiting []string
		var toBackfill []string
		var mismatches []string
		for _, symbol := range symbols {
			want := precisions[symbol]
			info, err := query(symbol)
			switch {
			case errors.Is(err, errCrossChainInfoNotOnChain):
				waiting = append(waiting, fmt.Sprintf("%s (the cross chain record is not on chain yet, "+
					"the guardians have not committed their DKG for it)", symbol))
			case err != nil:
				// 查询不可用：没有判据，不算失败也不算成功。
				waiting = append(waiting, fmt.Sprintf("%s (cannot read the on-chain record: %v)", symbol, err))
			case crossChainInfoAbsentOnChain(info):
				// 判据必须落在**记录内容**上，不能只看有没有报错：执行器对不存在的 symbol 返回
				// 空记录 + nil error（既有契约），一律按"链上还没有"处理 —— 否则空记录会被当成
				// "类别不是 RGB20"而把正常启动早期误判成不可自愈的分歧。
				waiting = append(waiting, fmt.Sprintf("%s (the cross chain record is not on chain yet, "+
					"the guardians have not committed their DKG for it)", symbol))
			case !rtypes.IsRgb20CrossChainAsset(info):
				mismatches = append(mismatches, fmt.Sprintf("symbol=%s: the chain registers it as asset "+
					"kind %s while this node's config registers it as an RGB20 contract (assetKind=%d)",
					symbol, rtypes.CrossChainAssetKind(info.GetAssetKind()).String(), info.GetAssetKind()))
			case info.GetPrecision() == want:
				// 已一致。
			case info.GetPrecision() == rtypes.PrecisionUnknown:
				toBackfill = append(toBackfill, symbol)
			default:
				mismatches = append(mismatches, fmt.Sprintf("symbol=%s: the chain records precision=%d but "+
					"the contract declares precision=%d (amounts would be off by 10^%d)",
					symbol, info.GetPrecision(), want, int64(info.GetPrecision())-int64(want)))
			}
		}
		if len(mismatches) > 0 {
			sort.Strings(mismatches)
			panic(&errCrossChainPrecisionMismatchDetail{mismatches: mismatches})
		}
		for _, symbol := range toBackfill {
			if submitted[symbol] {
				waiting = append(waiting, fmt.Sprintf("%s (the precision backfill was submitted but the "+
					"on-chain record still says \"not backfilled\")", symbol))
				continue
			}
			hash, submitErr := submit(rtypes.RgbxX, rtypes.NameCrossChainMetaAction,
				&rtypes.CrossChainAssetMeta{AssetSymbol: symbol, Precision: precisions[symbol]})
			if submitErr != nil {
				waiting = append(waiting, fmt.Sprintf("%s (submit the precision backfill failed: %v)",
					symbol, submitErr))
				continue
			}
			submitted[symbol] = true
			log.Info("cross chain asset precision backfill submitted to main chain",
				"symbol", symbol, "precision", precisions[symbol], "txHash", hash)
			// 提交成功 ≠ 已生效：**判据始终是链上状态**。下一轮会重新查链上记录，只有读回
			// 真实精度才算这一项通过（提交被链上拒、或还没出块，都要在这里继续等）。
			waiting = append(waiting, fmt.Sprintf("%s (the precision backfill was submitted, waiting for "+
				"the chain to record precision=%d)", symbol, precisions[symbol]))
		}
		if len(waiting) == 0 {
			log.Info("ensureCrossChainAssetPrecisions on-chain precisions match the contracts",
				"symbols", symbols, "elapsed", time.Since(start).String())
			return
		}
		sort.Strings(waiting)
		reportCrossChainPrecisionStall(&reported, &lastReportAt, attempt, start, strings.Join(waiting, "; "))
		time.Sleep(crossChainPrecisionVerifyInterval)
	}
}

// reportCrossChainPrecisionStall 报告"精度尚未与合约对齐"。
// 同一原因不重复刷屏（首次与原因变化必报，之后每 crossChainPrecisionStallRepeatLogs 次一条）；
// 持续超过 crossChainPrecisionStallErrorAfter 升级为 ERROR。
func reportCrossChainPrecisionStall(reported *string, lastReportAt *int, attempt int, start time.Time, reason string) {
	if *reported == reason && attempt-*lastReportAt < crossChainPrecisionStallRepeatLogs {
		log.Debug("cross chain asset precision not aligned yet", "attempt", attempt, "reason", reason)
		return
	}
	*reported, *lastReportAt = reason, attempt
	args := []interface{}{"attempt", attempt, "reason", reason, "elapsed", time.Since(start).String()}
	if time.Since(start) >= crossChainPrecisionStallErrorAfter {
		log.Error("cross chain asset precision not aligned with the contract yet: the chain's "+
			"crossChainInfo.precision is what the frontend and the CLI scale amounts with, so it must "+
			"carry the contract's value", args...)
		return
	}
	log.Info("cross chain asset precision not aligned yet", args...)
}
