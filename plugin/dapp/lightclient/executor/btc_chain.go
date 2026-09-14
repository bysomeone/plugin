package executor

import (
	"math/big"

	dbm "github.com/33cn/chain33/common/db"
	"github.com/33cn/chain33/types"
	ltypes "github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
	"github.com/btcsuite/btcd/blockchain"
)

/*
 * B3 + B4：BTC 头链的 reorg 回退（深度上限）与按累积工作量选链。
 *
 * 改造前的语义是"只接受接在当前 tip 上的头"（prevHeader.Height+1 == h.Height 且 hash 对得上），
 * 任何分叉一律 ErrBtcHeaderDisorder，且没有任何回退机制 —— BTC 一旦重组（真机上见过 tip 停在
 * 旧链高度 771、btcd 已换新链），头链就永久卡死，SPV 证明继续按旧分叉的头校验。
 *
 * 现在的语义（与 Bitcoin 的 header chain 一致，但把可回退深度限制在有界范围内）：
 *  1. 本批首个头必须挂在一个**已知的 canonical 节点**上：statedb 里保存了一个"最近节点窗口"
 *     （含 tip，共 btcWorkWindowSize 个），窗口里任一节点都可作为分叉点；
 *  2. 每个节点记录 (height, hash, chainwork)，chainwork 由 Bits 用 btcd 的 CalcWork 累积而来；
 *  3. 本批 tip 的累积工作量 **严格大于** 当前 canonical tip 时才切换 canonical 链，
 *     否则本批只是一条合法的更轻分叉：交易仍然成功，但不动 canonical（改了 canonical 才是更糟的设计，
 *     见下）；
 *  4. 分叉点比 tip 旧超过 maxBtcReorgDepth → fail-closed 拒绝（ErrBtcReorgTooDeep）。
 *
 * 为什么"更轻的分叉只验不切"而不是直接拒绝：拒绝会让中继在收到自己视图里合法、只是不占优的分支时
 * 无法推进（例如中继回退到分叉点后重发 canonical 链自身），而接受 + 不切 canonical 与 Bitcoin 节点
 * 的行为一致。代价是不保存侧链索引（保存侧链需要连侧链的逐高度头一起存，才能做上下文/难度校验，
 * 状态与复杂度都不划算）：因此侧链必须在分叉点重新提交（中继按 canonical tip 与自身视图比对后回退），
 * 这条约束由检查侧（ErrBtcHeaderUnknownAncestor）明确暴露出来。
 *
 * chainwork 的口径：work 的绝对值没有外部含义，只有**同一条状态序列内**可比较 —— 原点取"本链第一次
 * 写入状态时窗口里最老的那个节点"。所有分支的 work 都从窗口内某个节点（或本批自身）起算，因此相互可比。
 */

const (
	// maxBtcReorgDepth 允许的头链回退深度（块）。
	// 取值依据：BTC 主网史上最深的一次重组是 2013-03-11 的 24 块（BIP50 那次），其后远小于此；
	// 深度只放大"必须更重才能切换"这一条约束的状态与计算量，不放大攻击面（想切换仍要更多工作量），
	// 所以留足余量比卡死划算：超过这个深度的重组只能靠运维重新 bootstrap（错误码会明确报出）。
	maxBtcReorgDepth = 24
	// btcWorkWindowSize canonical 链在 statedb 里保留的节点数（含 tip）。窗口大小 = 可回退深度 + 1。
	btcWorkWindowSize = maxBtcReorgDepth + 1
	// btcReorgRuleVersion 写出 BtcHeadersLog 的规则版本，见 ltypes.BtcHeadersLog.reorgRuleVersion。
	btcReorgRuleVersion = 1
)

// btcWorkOfBits 该难度（nBits 紧凑表示）对应的累积工作量，复用 btcd 的实现。
func btcWorkOfBits(bits int64) *big.Int {
	return blockchain.CalcWork(uint32(bits))
}

// encodeBtcWork 累积工作量编码：big.Int 的大端字节（无前导零，确定性编码）；0 编码为空 bytes。
func encodeBtcWork(work *big.Int) []byte {
	if work == nil || work.Sign() <= 0 {
		return nil
	}
	return work.Bytes()
}

func decodeBtcWork(data []byte) *big.Int {
	return new(big.Int).SetBytes(data)
}

// btcReadHeader 从 localDB 按高度读头；localDB 未设置时返回 ErrNotFound（测试与非节点环境）。
func btcReadHeader(ldb dbm.KVDB, height uint64) (*ltypes.BtcHeader, error) {
	if ldb == nil {
		return nil, types.ErrNotFound
	}
	return getBtcHeader(ldb, height)
}

// getBtcChainState 读取 statedb 里的头链状态；尚未写入（升级前的历史数据 / 全新链）时返回空状态。
func getBtcChainState(sdb dbm.KV) (*ltypes.BtcChainState, error) {
	state := &ltypes.BtcChainState{}
	err := readDB(sdb, btcChainStateKey(), state)
	if err == types.ErrNotFound {
		return &ltypes.BtcChainState{}, nil
	}
	if err != nil {
		return nil, err
	}
	return state, nil
}

// loadBtcChainState 读取头链状态，并在 statedb 里还没有状态时用 btc-lastheader + localDB 重建窗口。
//
// 兼容性：B3/B4 之前上链的历史数据只有 btc-lastheader 一个 tip，没有窗口。若不重建，升级后第一个
// 批头就会因为"父块不在窗口里"被拒，等于把老链直接卡死。重建从 tip 沿 prevHash 回溯 localDB 里
// 按高度存的头（最多收满窗口），失败也不报错：窗口退化成只有 tip 一个节点，扩展（最常见路径）照常
// 可用，只有回退会被拒（fail-closed）。
func loadBtcChainState(sdb dbm.KV, ldb dbm.KVDB) (*ltypes.BtcChainState, error) {
	state, err := getBtcChainState(sdb)
	if err != nil {
		return nil, err
	}
	if len(state.GetNodes()) > 0 {
		return state, nil
	}
	tip, err := getBtcLastHeader(sdb)
	if err != nil {
		return nil, err
	}
	if tip.GetHash() == "" {
		return state, nil
	}
	return rebuildBtcChainState(tip, ldb), nil
}

// rebuildBtcChainState 用 tip（statedb 的 btc-lastheader）和 localDB 的逐高度头重建窗口。
func rebuildBtcChainState(tip *ltypes.BtcHeader, ldb dbm.KVDB) *ltypes.BtcChainState {
	headers := []*ltypes.BtcHeader{tip}
	cur := tip
	for len(headers) < btcWorkWindowSize && cur.GetHeight() > 0 {
		prev, err := btcReadHeader(ldb, cur.GetHeight()-1)
		if err != nil || prev.GetHash() == "" || prev.GetHash() != cur.GetPreviousHash() {
			break
		}
		headers = append(headers, prev)
		cur = prev
	}
	// headers 是从 tip 往回的顺序，反转成升序后从头累积工作量（最老节点的 work 即该链的 work 原点）。
	nodes := make([]*ltypes.BtcChainNode, 0, len(headers))
	work := new(big.Int)
	for i := len(headers) - 1; i >= 0; i-- {
		work = new(big.Int).Add(work, btcWorkOfBits(headers[i].GetBits()))
		nodes = append(nodes, &ltypes.BtcChainNode{
			Height: headers[i].GetHeight(),
			Hash:   headers[i].GetHash(),
			Work:   encodeBtcWork(work),
		})
	}
	return &ltypes.BtcChainState{Nodes: nodes}
}

// findBtcAttachNode 在窗口里查找本批首个头的父块。
//
// 错误码语义：
//   - ErrBtcHeaderDisorder：窗口为空（调用方应走 bootstrap 分支）或父块高度在 tip 之上（跳高，链不连续）；
//   - ErrBtcReorgTooDeep：父块比 tip 旧超过 maxBtcReorgDepth；
//   - ErrBtcHeaderUnknownAncestor：父块高度在窗口范围内但没有匹配的节点（分叉点不是我们的 canonical 链）。
func findBtcAttachNode(state *ltypes.BtcChainState, first *ltypes.BtcHeader) (*ltypes.BtcChainNode, error) {
	nodes := state.GetNodes()
	if len(nodes) == 0 || first.GetHeight() == 0 {
		return nil, ErrBtcHeaderDisorder
	}
	tipNode := nodes[len(nodes)-1]
	parentHeight := first.GetHeight() - 1
	if parentHeight > tipNode.GetHeight() {
		return nil, ErrBtcHeaderDisorder
	}
	if parentHeight+maxBtcReorgDepth < tipNode.GetHeight() {
		return nil, ErrBtcReorgTooDeep
	}
	for _, node := range nodes {
		if node.GetHeight() != parentHeight {
			continue
		}
		if node.GetHash() == first.GetPreviousHash() {
			return node, nil
		}
		return nil, ErrBtcHeaderUnknownAncestor
	}
	return nil, ErrBtcHeaderUnknownAncestor
}

// resolveBtcAttachHeader 取挂载点的完整头（构建难度/时间校验上下文用）。
// 优先用 statedb 里的 tip（扩展路径最常见，且不依赖 localDB 是否完整），否则按高度读 localDB。
// 两者都拿不到时 fail-closed：没有父块头就无法校验难度与时间。
func resolveBtcAttachHeader(node *ltypes.BtcChainNode, tip *ltypes.BtcHeader, ldb dbm.KVDB) (*ltypes.BtcHeader, error) {
	if tip != nil && tip.GetHash() != "" &&
		tip.GetHash() == node.GetHash() && tip.GetHeight() == node.GetHeight() {
		return tip, nil
	}
	header, err := btcReadHeader(ldb, node.GetHeight())
	if err != nil || header.GetHash() != node.GetHash() {
		elog.Error("resolveBtcAttachHeader missing attach header", "height", node.GetHeight(),
			"hash", node.GetHash(), "err", err)
		return nil, ErrBtcHeaderContextMissing
	}
	return header, nil
}

// btcHeadersPlan 一批头的处理决策：Exec 据此写 statedb，ExecLocal 据此写 localdb。
type btcHeadersPlan struct {
	headers []*ltypes.BtcHeader
	tip     *ltypes.BtcHeader
	tipWork *big.Int
	// switched 本批 tip 的累积工作量是否严格大于 canonical tip（bootstrap 时恒为 true）。
	switched bool
	// state 切换后的新窗口；未切换时等于原状态（不产生任何状态变更）。
	state *ltypes.BtcChainState
	// forkHeight 挂载点高度（本批首个头的父块高度）。
	forkHeight uint64
}

// planBtcHeaders 计算本批头的挂载点、累积工作量与 canonical tip 是否切换。
// 只依赖 statedb 状态（不读 localDB、不做 PoW/难度校验——那些由 checkBtcHeaders 负责），
// 因此 CheckTx 与 Exec 可以复用同一套确定性逻辑。
func planBtcHeaders(state *ltypes.BtcChainState, headers []*ltypes.BtcHeader) (*btcHeadersPlan, error) {
	if len(headers) == 0 {
		return nil, types.ErrInvalidParam
	}
	nodes := state.GetNodes()
	plan := &btcHeadersPlan{headers: headers, tip: headers[len(headers)-1]}

	attachWork := new(big.Int)
	if len(nodes) == 0 {
		// bootstrap：链上还没有任何索引，本批的锚点（创世/checkpoint/可回溯历史）不在窗口里，
		// 工作量从本批首个头起算。
		if headers[0].GetHeight() > 0 {
			plan.forkHeight = headers[0].GetHeight() - 1
		}
	} else {
		attach, err := findBtcAttachNode(state, headers[0])
		if err != nil {
			return nil, err
		}
		attachWork = decodeBtcWork(attach.GetWork())
		plan.forkHeight = attach.GetHeight()
	}

	works := make([]*big.Int, len(headers))
	tipWork := new(big.Int).Set(attachWork)
	for i, h := range headers {
		tipWork = new(big.Int).Add(tipWork, btcWorkOfBits(h.GetBits()))
		works[i] = tipWork
	}
	plan.tipWork = tipWork

	// bootstrap（窗口为空）恒等于切换：这条链就是由此批头定义的。
	plan.switched = len(nodes) == 0 || tipWork.Cmp(decodeBtcWork(nodes[len(nodes)-1].GetWork())) > 0
	if !plan.switched {
		plan.state = state
		return plan, nil
	}

	// 切换：新窗口 = 旧窗口里 <= forkHeight 的节点（分叉点及其之前，两条链共有）+ 本批节点，
	// 再截取最近 btcWorkWindowSize 个（新 tip 更低时，被替换掉的旧分支节点会自然滑出窗口）。
	newNodes := make([]*ltypes.BtcChainNode, 0, len(nodes)+len(headers))
	for _, node := range nodes {
		if node.GetHeight() <= plan.forkHeight {
			newNodes = append(newNodes, node)
		}
	}
	for i, h := range headers {
		newNodes = append(newNodes, &ltypes.BtcChainNode{
			Height: h.GetHeight(),
			Hash:   h.GetHash(),
			Work:   encodeBtcWork(works[i]),
		})
	}
	if len(newNodes) > btcWorkWindowSize {
		newNodes = newNodes[len(newNodes)-btcWorkWindowSize:]
	}
	plan.state = &ltypes.BtcChainState{Nodes: newNodes}
	return plan, nil
}
