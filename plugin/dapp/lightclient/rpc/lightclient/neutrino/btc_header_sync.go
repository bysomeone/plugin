package neutrino

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/33cn/chain33/types"
	ltypes "github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
)

/*
 * 中继侧的头链对账（B3 的必要配套）。
 *
 * 改造前 submitBitcoinHeaders 只用一个内存变量 nextSubmitHeight = 上次提交的最高高度 + 1 单调推进，
 * 从不回读 chain33 的 canonical tip。BTC 一旦重组（或本地头库被重建/换网），中继会继续提交"父块在
 * 链上从没出现过"的批：B3 之前报 ErrBtcHeaderDisorder，B3/B5 之后报 ErrBtcHeaderUnknownAncestor /
 * ErrBtcHeaderNoAnchor —— 错误码清楚了，但中继自己永远不回退，于是永久卡在"重试同一个批"上
 * （真机症状：chain33 头链 tip 停在旧链高度 771，btcd 已经换了新链）。SPV 证明继续按旧分叉的头校验，
 * 桥也跟着停摆。
 *
 * 这里的做法：**每个 tick 先对账，再提交**。
 *  1. 读链上 canonical tip（lightclient 的 GetBtcLastHeader 查询），与本地（btcd/neutrino）的逐高度
 *     头逐个高度比对 hash，找到"最高的 hash 一致的高度" h，下一批从 h+1 开始重发；
 *  2. 链上该高度没有头（低于链上起点，或曾属于已被换掉的旧分支）→ 继续往下找；
 *  3. 链上 tip 低于本地视图（正常）→ 从链上 tip 往下比对；链上 tip 高于本地视图 → 本地还没追上，
 *     本轮不提交（见 errBtcReconcileLocalBehind）；
 *  4. 回退深度上限 24（与执行器 maxBtcReorgDepth 对齐）；在允许窗口内找不到任何一致高度 → 不可自愈，
 *     显式报错并停在那个状态（运维需要重新 bootstrap），不再无脑重试。
 *
 * 本地逐高度视图直接用 btcd/neutrino 自己的头库（BlockHeaders.FetchHeaderByHeight，落盘在
 * <dbPath>/lightclient/neutrino.db），**不额外引入存储**：neutrino 检测到重组时会回滚它自己的按高度
 * 索引（headerfs.BlockHeaderStore.RollbackLastBlock），因此它始终是"本地 canonical 链"的视图。
 *
 * 与既有约束的关系：
 *   - B7（单笔 ≤ maxBtcHeadersPerTx=64 头、批内高度必须逐个 +1）：本批区间由 btcHeaderBatchRange
 *     连续生成，批大小固定 64 —— 正好等于执行器上限，天然满足但**不得再调大**（见 btcHeaderBatchSize）；
 *   - B5（bootstrap 锚点）：链上还没有任何头时，起点只能用配置的 btcHeaderStartHeight（它必须是
 *     本网络锚点高度 + 1，见 CONFIG.md），不能由本地视图推出来；
 *   - B3/B4（回退深度 24 + 按累积工作量选链）：本文件只负责"从一致点重发"，选链与深度判定仍在执行器。
 */

// maxBtcHeaderReorgDepth 允许的链上回退深度（块），必须与执行器的 maxBtcReorgDepth 一致
// （plugin/dapp/lightclient/executor/btc_chain.go）。比链上 tip 旧超过这个深度的分叉点会被链上以
// ErrBtcReorgTooDeep 拒收，中继据此判定为"不可自愈，需要重新 bootstrap"。
const maxBtcHeaderReorgDepth = 24

// maxBtcHeadersPerTx 执行器 B7 的单笔交易头数上限，与
// plugin/dapp/lightclient/executor/checktx.go 的同名常量必须一致（跨包不可见，这里复刻一份；
// 执行器侧有"64 收、65 拒"的用例钉住那个值，这里是它的中继侧镜像）。越界的批会被整批拒收
// （ErrBtcHeadersTooMany），而中继会把"确定性拒收"当成不可自愈并停止重发，所以两边必须同步改。
const maxBtcHeadersPerTx = 64

// btcHeaderBatchSize 单批提交的最大头数 = 执行器 B7 的单笔上限，**正好用满**。
//
// 为什么是 64（而不是原先的 16）：mainnet 的内置锚点只到 810000（见执行器 btcCheckpointTable），
// 距当前 tip 有约 10 万个头，而提交速度 ≈ 批大小 / (btcBlockInterval/3)：16 头/批 ≈ 288 头/小时 →
// 追平要 ~15 天，64 头/批 ≈ 1152 头/小时 → ~4 天，直接决定上线排期。加大批不放松任何一条校验
// （执行器对每个头都做 PoW/难度/时间/上下文校验），只放大单笔交易的体积：64 头 ≈ 16KB，
// 远小于 chain33 的 100KB 交易上限（types.MaxTxSize）。
const btcHeaderBatchSize = maxBtcHeadersPerTx

// btcHeaderBatchRefreshInterval 在途批次的"续期"间隔：本批已经被链上受理、链上却还没把它纳入
// canonical 时，最多每隔这么久重发一次。两个作用：
//  1. mempool 里的交易可能被挤掉，重发能把它续上（重复提交由链上按 ErrDupTx 拒绝，中继按"已受理"处理）；
//  2. 避免每个 tick 都发一笔"注定重复"的交易。
const btcHeaderBatchRefreshInterval = 20 * time.Minute

// maxBtcHeaderFutileSubmits 同一批连续被当成"新交易"受理（说明上一笔已不在 mempool）、而链上 canonical
// 链始终没有推进的次数上限。超过后报一次错（不停心跳，也不再刷屏）：本地这条链比链上现有的链更轻，
// 或链上根本没有出块。
const maxBtcHeaderFutileSubmits = 3

var (
	// errBtcReconcileLocalBehind 本地（btcd）头视图落后链上 canonical 链超过可回退深度：btcd 还在这条
	// 链上往前同步（重启/重建头库），等它追上即可。此时任何提交要么挂在链上窗口之外（ErrBtcReorgTooDeep），
	// 要么只能是一条不占优的分叉，都没有意义。
	errBtcReconcileLocalBehind = errors.New("btc header reconcile: local header view is behind the on-chain chain")
	// errBtcReconcileNoCommonHeader 在允许的回退深度内找不到任何与链上 canonical 链一致的高度：
	// 链上可能发生过超深重组、换过网络/起点，或 btcHeaderStartHeight 与链上起点（B5 锚点）不配套。
	// 不可自愈，必须显式报错并停在这里（运维重新 bootstrap）。
	errBtcReconcileNoCommonHeader = errors.New("btc header reconcile: no common canonical header within the allowed depth")
	// errBtcAnchorQueryUnsupported 链上的 lightclient 执行器没有 GetBtcCheckpoint 查询（旧节点）。
	// L2 的 bootstrap 起点断言对此降级：WARN 一次后照常提交，不做硬依赖。
	errBtcAnchorQueryUnsupported = errors.New("btc header reconcile: the on-chain lightclient executor does not support the btc checkpoint query")
)

// btcChainHeaderView 链上（chain33 lightclient 执行器）的 canonical 头链只读视图。
type btcChainHeaderView interface {
	// chainTipHeader 返回链上 canonical 头链的 tip；链上还没有任何 BTC 头时返回高度 0 的空头。
	chainTipHeader() (*ltypes.BtcHeader, error)
	// chainHeaderHashAt 返回链上该高度的 canonical 头 hash；found=false 表示链上该高度没有头。
	chainHeaderHashAt(height uint64) (hash string, found bool, err error)
	// chainAnchor 返回链上执行器（lightclient）本网络最高的有效锚点；链上不支持该查询（旧节点）时
	// 返回 errBtcAnchorQueryUnsupported，本网络没有锚点（regtest 等）时返回高度 0 的空头。
	chainAnchor() (*ltypes.BtcHeader, error)
}

// btcLocalHeaderView 本地（btcd/neutrino）canonical 头链的只读视图。
type btcLocalHeaderView interface {
	// localHeaderHashAt 返回本地该高度的 canonical 头 hash。
	localHeaderHashAt(height uint64) (string, error)
}

// btcHeaderReconciler 把中继的本地头视图与链上 canonical 头链对齐。
type btcHeaderReconciler struct {
	chain btcChainHeaderView
	local btcLocalHeaderView
	// startHeight 链上还没有任何头时的提交起点（cfg.BtcHeaderStartHeight）。
	startHeight uint64
	// maxDepth 允许的最大回退深度，必须与执行器 maxBtcReorgDepth 一致。
	maxDepth uint64
	// anchor bootstrap 起点与链上锚点的配套断言（L2，只在 bootstrap 分支生效）。
	anchor btcBootstrapAnchorGuard
}

// btcHeaderSyncPlan 一轮对账的结论。
type btcHeaderSyncPlan struct {
	// nextSubmitHeight 下一批应从该高度开始提交。
	nextSubmitHeight uint64
	// chainTipHeight 本轮读到的链上 canonical tip 高度（0 = 链上还没有任何头）。
	chainTipHeight uint64
	// localTipHeight 本轮本地视图的 tip 高度。
	localTipHeight uint64
	// matchedHeight 找到的最高一致高度。
	matchedHeight uint64
	// rolledBack 一致高度低于链上 tip：本批是从一致点回退重发（重组修复），而不是正常往前扩展。
	rolledBack bool
	// probes 本轮实际比对的高度个数（可观测性：正常推进恒为 1）。
	probes int
}

// key 识别"同一个计划"：起点与链上 tip 相同即视为同一批（链上 tip 动了说明有进展）。
// 用于"不可自愈的拒收后不再重复提交同一批"的判断。
func (p *btcHeaderSyncPlan) key() string {
	return fmt.Sprintf("next=%d,chainTip=%d", p.nextSubmitHeight, p.chainTipHeight)
}

// plan 对账一次，得出下一批的起点。
//
// 错误语义：errBtcReconcileLocalBehind 表示"等本地同步，本轮不提交"；errBtcReconcileNoCommonHeader
// 表示"不可自愈，报错并停在这里"；其余错误是查询/读取失败（下一轮重试即可，不改变任何状态）。
func (r *btcHeaderReconciler) plan(localTipHeight uint64) (*btcHeaderSyncPlan, error) {
	tip, err := r.chain.chainTipHeader()
	if err != nil {
		return nil, err
	}
	plan := &btcHeaderSyncPlan{localTipHeight: localTipHeight}
	if tip.GetHeight() == 0 {
		// 链上还没有任何 BTC 头：这就是"同步起点"，其合法性由 B5 的锚点校验决定
		// （首个头的父块必须能锚定到创世或本网络 checkpoint），因此只能用配置值，
		// 不能由本地视图推出来。
		plan.nextSubmitHeight = r.startHeight
		return plan, nil
	}

	chainTip := tip.GetHeight()
	plan.chainTipHeight = chainTip

	// 比对窗口：从 min(链上 tip, 本地 tip) 往下，最多 maxDepth 个高度。
	start := chainTip
	if localTipHeight < start {
		start = localTipHeight
	}
	floor := uint64(0)
	if chainTip > r.maxDepth {
		floor = chainTip - r.maxDepth
	}
	if start < floor {
		return nil, fmt.Errorf("%w: chainTip=%d localTip=%d", errBtcReconcileLocalBehind, chainTip, localTipHeight)
	}

	for h := start; ; h-- {
		plan.probes++
		chainHash, found, err := r.chain.chainHeaderHashAt(h)
		if err != nil {
			return nil, fmt.Errorf("read on-chain btc header at height %d: %w", h, err)
		}
		if found {
			localHash, err := r.local.localHeaderHashAt(h)
			if err != nil {
				return nil, fmt.Errorf("read local btc header at height %d: %w", h, err)
			}
			if chainHash == localHash {
				plan.matchedHeight = h
				plan.nextSubmitHeight = h + 1
				plan.rolledBack = h < chainTip
				return plan, nil
			}
		}
		// 链上该高度没有头，或 hash 与本地不一致（同高度不同 hash / 本地也缺这个高度）：继续往下找。
		if h == floor || h == 0 {
			break
		}
	}
	return nil, fmt.Errorf("%w: chainTip=%d localTip=%d probes=%d",
		errBtcReconcileNoCommonHeader, chainTip, localTipHeight, plan.probes)
}

// btcBootstrapAnchorGuard 中继的 bootstrap 起点断言（L2）：把"btcHeaderStartHeight 与本网络锚点不配套"
// 从运行期的 ErrBtcHeaderNoAnchor（执行器 checkBootstrapAnchor 拒收）提前成中继启动期的显式 ERROR。
//
// 背景：bootstrap（链上还没有任何 BTC 头）时首个头必须锚定到真实链，执行器只接受
// "首个头的父块 == 本网络锚点"。锚点来自 btcd chaincfg（见执行器 btcCheckpointTable），中继无法靠
// chain33 之外的任何信息知道它，于是配置写错时表现为：每个 batch 都被确定性拒收（ErrBtcHeaderNoAnchor），
// 中继把该计划标记为 halted 后静默停在那里，链上头链停在空链状态 —— 除了翻执行器日志没有别的线索。
// 这里在提交前用执行器的 Query_GetBtcCheckpoint 拿到同一个锚点，一次说清楚该填多少。
//
// 只在 bootstrap 分支断言：BtcHeaderStartHeight 只在"链上 tip==0"时被使用（见 btcHeaderSyncPlan.plan），
// 头链一旦长起来，起点由对账得出，配置值就失效了（执行器侧同样只在链上 tip==0 时用它）。
// 因此这里不做常驻校验，避免在正常运行的链上误报。
//
// 不硬依赖：查询不被支持（旧执行器）或失败（主链未就绪）时 WARN 一次后照常提交；
// 本网络没有锚点（regtest 等）时也照常提交（那种网络只能从创世起，由执行器兜底）。
type btcBootstrapAnchorGuard struct {
	// anchorHeight 链上执行器给出的本网络最高锚点高度；anchorRead=true 表示已经问过（锚点是编译期
	// 常量，一个进程生命周期内只问一次；查询不被支持时也置位，旧执行器不会在运行中变新）。
	anchorHeight uint64
	anchorRead   bool
	// report 同一个问题状态只报一次，避免每个 tick 刷屏。
	report btcHeaderReportLimiter
}

// check 判断本轮能否提交 bootstrap 批次；false 表示起点与链上锚点不配套，必须不发交易（fail-closed）。
func (g *btcBootstrapAnchorGuard) check(chain btcChainHeaderView, startHeight uint64) bool {
	if !g.anchorRead {
		anchor, err := chain.chainAnchor()
		switch {
		case errors.Is(err, errBtcAnchorQueryUnsupported):
			// 旧执行器：拿不到锚点就不做这个断言（升级主链后自动生效）。
			g.anchorRead = true
			log.Warn("submitBitcoinHeaders the on-chain lightclient executor does not support the btc "+
				"checkpoint query, skip the bootstrap start-height assertion", "err", err,
				"btcHeaderStartHeight", startHeight)
			return true
		case err != nil:
			// 查询失败（主链未就绪/网络抖动）：本轮不判定，下一轮再问（只报一次，不刷屏）。
			if g.report.allow("query-error") {
				log.Warn("submitBitcoinHeaders read the on-chain btc checkpoint failed, retry on the next tick",
					"err", err)
			}
			return true
		}
		g.anchorRead = true
		g.anchorHeight = anchor.GetHeight()
	}
	if g.anchorHeight == 0 {
		// 本网络没有锚点（regtest/testnet4/signet/simnet）：只能从创世起，合法性由执行器的锚点校验兜底。
		return true
	}
	if startHeight == g.anchorHeight+1 {
		g.report.reset()
		return true
	}
	// 不配套：链上会拒收每一个 bootstrap 批（且是"重试同一批永远不会成功"的确定性拒收），
	// 因此在源头断开：限流 ERROR + 不发交易，等配置改对（重启）或链上锚点变化。
	if g.report.allow(fmt.Sprintf("mismatch:%d:%d", g.anchorHeight, startHeight)) {
		log.Error("submitBitcoinHeaders btcHeaderStartHeight does not match the on-chain btc checkpoint, "+
			"refusing to submit btc headers (fail-closed): the first bootstrap header's parent must be a "+
			"known anchor, otherwise the main chain rejects it with ErrBtcHeaderNoAnchor",
			"btcHeaderStartHeight", startHeight, "anchorHeight", g.anchorHeight,
			"expectedBtcHeaderStartHeight", g.anchorHeight+1)
	}
	return false
}

// btcHeaderBatchRange 计算本批覆盖的高度区间 [from, to]；没有可提交的高度时 ok=false。
//
// 三条由构造保证的不变量（执行器侧对应 B7 的校验）：
//   - 区间自 nextSubmitHeight 起、连续且逐个 +1（同一笔交易内的头高度必须严格 +1）；
//   - 区间不超过 "已确认" 高度 confirmedHeight（链上还没有的头不能提交）；
//   - 区间长度不超过执行器上限 maxBtcHeadersPerTx —— 传入更大的 batchSize 会被截断到上限，
//     这样即使有人把 btcHeaderBatchSize 调大，也不会发出一个注定被 ErrBtcHeadersTooMany 整批拒收的批。
func btcHeaderBatchRange(nextSubmitHeight, confirmedHeight uint64, batchSize int) (from, to uint64, ok bool) {
	if batchSize > maxBtcHeadersPerTx {
		batchSize = maxBtcHeadersPerTx
	}
	if batchSize <= 0 || nextSubmitHeight == 0 || nextSubmitHeight > confirmedHeight {
		return 0, 0, false
	}
	to = nextSubmitHeight + uint64(batchSize) - 1
	if to < nextSubmitHeight || to > confirmedHeight { // 溢出保护
		to = confirmedHeight
	}
	return nextSubmitHeight, to, true
}

// btcHeaderSubmitResult 一次提交的结果分类（见 classifyBtcHeaderSubmitErr）。
type btcHeaderSubmitResult int

const (
	// btcHeaderSubmitAccepted 链上受理了这笔交易（进了 mempool）。
	btcHeaderSubmitAccepted btcHeaderSubmitResult = iota
	// btcHeaderSubmitDuplicateAccepted 同一笔交易已经在 mempool 里（ErrDupTx）：等价于"本批已在途"。
	btcHeaderSubmitDuplicateAccepted
	// btcHeaderSubmitTransient 可自愈的失败（网络/主链未就绪/费用等），下一轮对账再试。
	btcHeaderSubmitTransient
	// btcHeaderSubmitRejected 链上按确定性规则拒收：同样的批重发多少次都是一样的结果，必须显式报错
	// 并停止重复提交，而不是淹没在重试里。
	btcHeaderSubmitRejected
)

// btcHeaderRejectedErrs 执行器（plugin/dapp/lightclient/executor/errors.go）里"重试同一批永远不会成功"
// 的错误码。命中即判定不可自愈。
var btcHeaderRejectedErrs = []string{
	"ErrIllegalCommitAddress",     // commitAddress 未配置，或提交者不是它
	"ErrBtcReorgTooDeep",          // 分叉点比链上 tip 旧超过 maxBtcReorgDepth
	"ErrBtcHeaderUnknownAncestor", // 父块不在 canonical 链的已知窗口里
	"ErrBtcHeaderDisorder",        // 高度跳高 / 与挂载点不连续
	"ErrBtcHeaderDuplicateHeight", // 批内高度重复或跳高
	"ErrBtcHeadersTooMany",        // 单笔头数超过上限
	"ErrBtcHeaderNoAnchor",        // bootstrap 首个头无法锚定（btcHeaderStartHeight 与 checkpoint 不配套）
	"ErrBtcHeaderContextMissing",  // 挂载点在 canonical 链上但拿不到完整头
	"ErrInvalidBtcBlockHash",
	"ErrBtcTargetBits",
	"ErrBtcHeaderVerify",
}

// classifyBtcHeaderSubmitErr 给一次提交的结果分类。
func classifyBtcHeaderSubmitErr(err error) btcHeaderSubmitResult {
	if err == nil {
		return btcHeaderSubmitAccepted
	}
	msg := err.Error()
	// ErrDupTx：同一笔交易已经在 mempool 里（chain33 system/mempool/check.go），
	// 等价于"本批已经在途"，按已受理处理（改造前的 submitMainChainTxUntilSuccess 也是这个口径）。
	if strings.Contains(msg, "ErrDupTx") || strings.Contains(msg, "duplicate") {
		return btcHeaderSubmitDuplicateAccepted
	}
	for _, code := range btcHeaderRejectedErrs {
		if strings.Contains(msg, code) {
			return btcHeaderSubmitRejected
		}
	}
	return btcHeaderSubmitTransient
}

// btcHeaderSyncState 提交循环的内存状态，不落盘：重启后重新对账即可得出同样的结论。
type btcHeaderSyncState struct {
	// pendingBottom/pendingTop 最近一次被链上受理的批次覆盖的高度区间（pendingTop=0 表示没有在途批次）。
	pendingBottom uint64
	pendingTop    uint64
	// pendingAt 该批次最近一次提交（含续期）的时刻。
	pendingAt time.Time
	// futileSubmits 同一批连续被当成"新交易"受理、而链上 canonical 链毫无推进的次数。
	futileSubmits int
	// haltedKey 已被判定为不可自愈的计划（chain33 明确拒收），在其变化之前不再重复提交。
	haltedKey string
	// report 对账侧的问题（同一状态只报一次，避免刷屏）：对账成功即说明问题消失，可以重置。
	report btcHeaderReportLimiter
	// submitReport 提交侧的问题（同一个错只报一次）：提交被受理才说明问题消失，可以重置。
	submitReport btcHeaderReportLimiter
}

// allowSubmit 本轮是否应该提交计划中的批次。
// 在途批次覆盖区间内的（重）提交只在超过 btcHeaderBatchRefreshInterval 后放行一次（心跳）；
// 计划越过在途批次（链上往前走了），或回退到在途批次之下（链上否定了它所在的分支）时都要提交。
func (st *btcHeaderSyncState) allowSubmit(next uint64, now time.Time) bool {
	if st.pendingTop == 0 || next > st.pendingTop || next < st.pendingBottom {
		return true
	}
	return now.Sub(st.pendingAt) >= btcHeaderBatchRefreshInterval
}

// btcHeaderSubmitReason 本轮"是否提交"的结论。
type btcHeaderSubmitReason int

const (
	// btcHeaderSubmitReady 可以提交 [from, to]。
	btcHeaderSubmitReady btcHeaderSubmitReason = iota
	// btcHeaderSubmitPlanHalted 该计划刚被链上确定性拒收，等它变化再试。
	btcHeaderSubmitPlanHalted
	// btcHeaderSubmitNothingConfirmed 没有"已确认"的高度可提交（或在途批次已覆盖）。
	btcHeaderSubmitNothingConfirmed
	// btcHeaderSubmitAlreadyInFlight 该区间已在途（或刚落链），本轮不重发。
	btcHeaderSubmitAlreadyInFlight
)

// beginSubmit 提交前的决策。与 finishSubmit 配对使用，提交循环与测试共用同一套状态转移。
func (st *btcHeaderSyncState) beginSubmit(plan *btcHeaderSyncPlan, confirmedHeight uint64, now time.Time) (from, to uint64, reason btcHeaderSubmitReason) {
	if st.halted(plan) {
		return 0, 0, btcHeaderSubmitPlanHalted
	}
	from, to, ok := btcHeaderBatchRange(plan.nextSubmitHeight, confirmedHeight, btcHeaderBatchSize)
	if !ok {
		return 0, 0, btcHeaderSubmitNothingConfirmed
	}
	if !st.allowSubmit(plan.nextSubmitHeight, now) {
		return 0, 0, btcHeaderSubmitAlreadyInFlight
	}
	return from, to, btcHeaderSubmitReady
}

// finishSubmit 提交后的状态转移。[from, top] 是实际提交（或尝试提交）的高度区间。
func (st *btcHeaderSyncState) finishSubmit(plan *btcHeaderSyncPlan, from, top uint64, now time.Time, result btcHeaderSubmitResult) {
	switch result {
	case btcHeaderSubmitAccepted, btcHeaderSubmitDuplicateAccepted:
		st.onSubmitted(from, top, now, result == btcHeaderSubmitDuplicateAccepted)
	case btcHeaderSubmitRejected:
		st.halt(plan)
	}
}

// onSubmitted 记录一次"链上已受理"的提交。
func (st *btcHeaderSyncState) onSubmitted(bottom, top uint64, now time.Time, duplicate bool) {
	switch {
	case duplicate:
		// 本批已经在 mempool 里：只是续期，不算新提交。
		if st.pendingTop == 0 {
			st.pendingBottom, st.pendingTop = bottom, top
		}
		st.pendingAt = now
	case st.pendingTop != 0 && st.pendingBottom == bottom && st.pendingTop == top:
		// 同一批被当成新交易重新受理：上一笔已经不在 mempool 里了，而链上又没有推进。
		st.futileSubmits++
		st.pendingAt = now
	default:
		st.futileSubmits = 0
		st.pendingBottom, st.pendingTop, st.pendingAt = bottom, top, now
	}
}

// halted 该计划是否已被判定为不可自愈（链上明确拒收）。
func (st *btcHeaderSyncState) halted(plan *btcHeaderSyncPlan) bool {
	return st.haltedKey != "" && st.haltedKey == plan.key()
}

// halt 记下被确定性拒收的计划：在它变化之前不再重复提交。计划一变（链上 tip 动了，或对账给出不同的
// 起点）就自动恢复，不需要重启。
func (st *btcHeaderSyncState) halt(plan *btcHeaderSyncPlan) { st.haltedKey = plan.key() }

// btcHeaderReportLimiter 同一个问题状态只报一次：key 变了（状态变了）才再报，避免每个 tick 刷屏。
type btcHeaderReportLimiter struct{ key string }

func (l *btcHeaderReportLimiter) allow(key string) bool {
	if key == l.key {
		return false
	}
	l.key = key
	return true
}

func (l *btcHeaderReportLimiter) reset() { l.key = "" }

// chain33BtcHeaderView 链上视图的真实实现：走 lightclient 执行器的查询。
// 为了可测试，接口只暴露"按高度取 hash"，实现见下。链上逐高度头存在执行器 localdb
// （key 前缀 LODB-lightclient-，见 executor/kv.go），由 ExecLocal_BtcHeaders 按 canonical 链维护。
type chain33BtcHeaderView struct{ n *neutrinoClient }

// chainTipHeader 读链上 canonical 头链的 tip（statedb 的 btc-lastheader）。
func (v *chain33BtcHeaderView) chainTipHeader() (*ltypes.BtcHeader, error) {
	header := &ltypes.BtcHeader{}
	if err := v.query("GetBtcLastHeader", nil, header); err != nil {
		return nil, err
	}
	return header, nil
}

// chainHeaderHashAt 读链上 canonical 链在该高度的头 hash。
func (v *chain33BtcHeaderView) chainHeaderHashAt(height uint64) (string, bool, error) {
	header := &ltypes.BtcHeader{}
	err := v.query("GetBtcHeader", types.Encode(&ltypes.ReqGetBtcHeader{Height: height}), header)
	if err != nil {
		// 执行器 localdb 没有该高度的头时查询以 ErrNotFound 失败（见 chain33 executor.LocalDB.Get），
		// 这是"链上该高度没有 canonical 头"的正常信号，不是查询故障。
		if strings.Contains(err.Error(), types.ErrNotFound.Error()) {
			return "", false, nil
		}
		return "", false, err
	}
	if header.GetHash() == "" {
		return "", false, nil
	}
	return header.GetHash(), true, nil
}

// btcChainAnchorQuery 执行器侧返回"本网络最高有效锚点"的查询名（executor/query.go 的
// Query_GetBtcCheckpoint）。
const btcChainAnchorQuery = "GetBtcCheckpoint"

// chainAnchor 读链上执行器维护的本网络最高锚点（bootstrap 信任根）。
func (v *chain33BtcHeaderView) chainAnchor() (*ltypes.BtcHeader, error) {
	header := &ltypes.BtcHeader{}
	if err := v.query(btcChainAnchorQuery, nil, header); err != nil {
		if isBtcAnchorQueryUnsupported(err) {
			return nil, fmt.Errorf("%w: %v", errBtcAnchorQueryUnsupported, err)
		}
		return nil, err
	}
	return header, nil
}

// isBtcAnchorQueryUnsupported 判断错误是不是"链上执行器没有这个查询方法"（旧节点）。
// chain33 的 DriverBase.Query 对未知的 Query_ 函数返回 ErrActionNotSupport（system/dapp/query.go），
// 据此把"不支持"与"查询失败"（主链未就绪/网络抖动，下一轮重试）区分开。
func isBtcAnchorQueryUnsupported(err error) bool {
	return err != nil && strings.Contains(err.Error(), types.ErrActionNotSupport.Error())
}

func (v *chain33BtcHeaderView) query(funcName string, param []byte, out types.Message) error {
	reply, err := v.n.mainChainGrpc.QueryChain(v.n.ctx, &types.ChainExecutor{
		Driver:   ltypes.LightclientX,
		FuncName: funcName,
		Param:    param,
	})
	if err != nil {
		return fmt.Errorf("query %s: %w", funcName, err)
	}
	if err = types.Decode(reply.GetMsg(), out); err != nil {
		return fmt.Errorf("decode %s: %w", funcName, err)
	}
	return nil
}

// neutrinoBtcHeaderView 本地视图的真实实现：btcd/neutrino 自己的按高度头库。
type neutrinoBtcHeaderView struct{ n *neutrinoClient }

func (v *neutrinoBtcHeaderView) localHeaderHashAt(height uint64) (string, error) {
	if v.n.neutrinoCS == nil {
		return "", errors.New("neutrino chain service not started")
	}
	header, err := v.n.neutrinoCS.BlockHeaders.FetchHeaderByHeight(uint32(height))
	if err != nil {
		return "", err
	}
	return header.BlockHash().String(), nil
}
