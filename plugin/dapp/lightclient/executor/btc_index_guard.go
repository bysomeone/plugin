package executor

import (
	"fmt"
	"sync"

	dbm "github.com/33cn/chain33/common/db"
	ltypes "github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
)

/*
 * E6(b)：把 localdb 当成"共识状态的确定性投影"来读。
 *
 * 问题：Query_GetBtcHeader / Query_GetBtcHeaderByHash 从 **localdb**（LODB-lightclient-btc-header-<height>，
 * 见 kv.go）按高度取头，而读方是 rgbx 的共识判定（validate_proof.go 的 SPV 校验，每个节点出块时都会重跑）。
 * localdb 是节点私有数据（由非共识路径 ExecLocal_BtcHeaders 写），一旦它与共识状态不一致（丢库、落后、
 * 残留旧分叉头、被外部改成别的链），"区块是否合法"就取决于各节点本地库 —— 这正是 B8 在
 * getBtcCanonicalTip 的注释里（validate_proof.go:263-273）明确要避免的老毛病。
 *
 * 本文件不改 stateDB 结构，而是给这两条读路径加上**用共识状态做的交叉校验**：
 *
 *  1. 逐高度校验（checkBtcHeaderCanonical）：请求高度落在 statedb 的 canonical 窗口
 *     （btc-chainstate，最近 btcWorkWindowSize 个节点，见 btc_chain.go）里时，localdb 取到的头
 *     必须与窗口节点的 hash 一致，否则 fail-closed 报错。
 *     头 hash 已承诺 merkleRoot（Hash 由 btcd 的 BlockHeader.BlockHash() 得出 = 对含 merkleRoot 的
 *     80 字节头做双 SHA256，见 neutrino/bitcoin.go 的 buildBtcHeaderBatch），所以校验 hash 就等于
 *     校验 merkleRoot，不需要改 proto。
 *  2. tip 校验（checkBtcLocalIndexTip）：localdb 在"共识 tip 高度"（statedb 的 btc-lastheader）上的头
 *     必须与共识 tip 一致，否则 fail-closed —— 这是"localdb 丢库/落后"最早能被发现的信号。
 *
 * 判定边界（重要，别扩大）：
 *   - 只有"窗口里有这条 + hash 不一致"才拒；"窗口里没有这条"（高度比窗口老、窗口为空、老链在 B3/B4
 *     之前只写了 btc-lastheader）**维持原行为**——那部分是结构上无法用 stateDB 校验的（深于
 *     maxBtcReorgDepth 的重组不可能，真出现不一致会被 types.ErrCheckStateHash 变成本节点停机）。
 *   - 拒绝的后果是 **fail-closed**：本节点拒收依赖该高度的区块/证明 → 与本节点状态哈希不符 → 掉队停机，
 *     不是全网静默分叉（与 E6 勘察的结论一致）。
 *   - 逃生阀 allowBtcIndexMismatch（[exec.sub.lightclient]，默认 false）：只在运维确认要做冷修
 *     （手工/重放重建本地索引）时临时打开；打开后不一致只报错不拒绝。
 */

var (
	// 同一类不一致在进程内只报一次（避免每个块 / 每次查询刷屏）；判定本身不缓存，每次都按当次
	// DB 内容重算，因此修好（或瞬时错位恢复）后立刻恢复正常行为。
	btcWindowMismatchLogOnce sync.Once
	btcTipMismatchLogOnce    sync.Once
)

// btcCanonicalNodeAt 在 canonical 窗口里按高度取节点。
// 高度不在窗口里（比窗口更老、高于 tip、窗口尚未写入）时返回 nil —— 调用方据此维持原行为。
func btcCanonicalNodeAt(state *ltypes.BtcChainState, height uint64) *ltypes.BtcChainNode {
	for _, node := range state.GetNodes() {
		if node.GetHeight() == height {
			return node
		}
	}
	return nil
}

// checkBtcHeaderCanonical 逐高度交叉校验：localdb 取到的头必须与 statedb canonical 窗口里同高度的节点一致。
//
// 只判"窗口里有这条"的情形；窗口里没有（含窗口为空）就返回 nil，交回原有语义。
func checkBtcHeaderCanonical(state *ltypes.BtcChainState, header *ltypes.BtcHeader, allowMismatch bool) error {
	node := btcCanonicalNodeAt(state, header.GetHeight())
	if node == nil {
		elog.Debug("checkBtcHeaderCanonical height not in canonical window, keep unchanged",
			"height", header.GetHeight(), "hash", header.GetHash(), "windowNodes", len(state.GetNodes()))
		return nil
	}
	if node.GetHash() == header.GetHash() {
		return nil
	}

	btcWindowMismatchLogOnce.Do(func() {
		elog.Error("lightclient local btc header index disagrees with the on-chain canonical chain; "+
			"block validity must not depend on this node's private localdb",
			"height", header.GetHeight(), "onChainHash", node.GetHash(), "localHash", header.GetHash(),
			"allowMismatch", allowMismatch,
			"hint", "rebuild the LODB-lightclient- btc header index (see CONFIG.md 2.1 for the recovery "+
				"procedure); setting [exec.sub.lightclient] allowBtcIndexMismatch=true is only for a "+
				"deliberate cold repair")
	})
	if allowMismatch {
		return nil
	}
	return fmt.Errorf("%w: height=%d onChainHash=%s localHash=%s",
		ErrBtcHeaderNotCanonical, header.GetHeight(), node.GetHash(), header.GetHash())
}

// checkBtcLocalIndexTip 一致性自检：localdb 在共识 tip 高度上的头必须与 statedb 的 btc-lastheader 一致。
//
// 语义：
//   - statedb 还没有头（btc-lastheader 为空）：不做判定 —— 链上还没有头链，localdb 里有什么都不影响判定；
//   - statedb 有 tip：localdb 在该高度上必须有头且 hash 相同，否则判为不一致（fail-closed）。
//     正常写入路径下两者恒等（同一个批头分别写 statedb 的 tip 与 localdb 的逐高度索引），所以
//     "取不到 / 不一致"就说明 localdb 丢了、落后了，或者停在别的链上。
//
// 与链上 tip 的一致性判不出来时的取向一律 fail-closed（读不到共识状态也拒）。
func checkBtcLocalIndexTip(sdb dbm.KV, ldb dbm.KVDB, allowMismatch bool) error {
	if sdb == nil {
		// 非节点环境（单测直接构造执行器、Upgrade 阶段 statedb 未绑定）：没有共识状态可比。
		return nil
	}
	tip, err := getBtcLastHeader(sdb)
	if err != nil {
		elog.Error("checkBtcLocalIndexTip read on-chain btc tip", "err", err)
		return fmt.Errorf("%w: read on-chain btc tip err=%v", ErrBtcLocalIndexMismatch, err)
	}
	if tip.GetHash() == "" {
		return nil
	}

	localHash, localErr := btcLocalTipHash(ldb, tip.GetHeight())
	if localErr == nil && localHash == tip.GetHash() {
		return nil
	}

	btcTipMismatchLogOnce.Do(func() {
		elog.Error("lightclient local btc header index has no matching header at the on-chain btc tip; "+
			"a node whose localdb disagrees with consensus would validate SPV proofs against it",
			"tipHeight", tip.GetHeight(), "tipHash", tip.GetHash(),
			"localHash", localHash, "localErr", localErr, "allowMismatch", allowMismatch,
			"hint", "rebuild the LODB-lightclient- btc header index (see CONFIG.md 2.1 for the recovery "+
				"procedure); setting [exec.sub.lightclient] allowBtcIndexMismatch=true is only for a "+
				"deliberate cold repair")
	})
	if allowMismatch {
		return nil
	}
	return fmt.Errorf("%w: tipHeight=%d tipHash=%s localHash=%s localErr=%v",
		ErrBtcLocalIndexMismatch, tip.GetHeight(), tip.GetHash(), localHash, localErr)
}

// btcLocalTipHash 读 localdb 在给定高度上的头 hash；取不到时返回错误（含 ldb 为 nil，即节点关闭了 localdb）。
func btcLocalTipHash(ldb dbm.KVDB, height uint64) (string, error) {
	if ldb == nil {
		return "", ErrBtcLocalIndexUnavailable
	}
	header, err := getBtcHeader(ldb, height)
	if err != nil {
		return "", err
	}
	return header.GetHash(), nil
}
