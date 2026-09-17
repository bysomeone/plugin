package executor

import (
	"fmt"
	"math"
	"sort"
	"time"

	dbm "github.com/33cn/chain33/common/db"
	ltypes "github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// btcCheckpointTable 已知 BTC 区块头锚点表：网络 → 高度 → 该高度区块的 hash
// （chainhash.Hash.String() 的小写 hex 口径，与 ltypes.BtcHeader.Hash 一致）。
//
// 两个用途都要求"该高度已深度确认、不可能再回滚"：
//  1. bootstrap 锚点：链上还没有任何 BTC 头时，首个头必须能沿 prevHash 回溯到创世或命中本表
//     （见 checkBootstrapAnchor）。没有它，mainnet 只能从创世开始同步几百万个头，不现实；
//  2. 同步校验：已同步的链走到表中高度时，头 hash 必须与表一致，否则整条链都不是真实链
//     （见 btcChainContext.VerifyCheckpoint）。
//
// **表的来源（方案 A）**：不再手工维护，而是由 init() 按网络从 btcd chaincfg 的内置 Checkpoints
// 直接生成（每个网络最后一条锚点见 TestBtcCheckpointTableGolden），运维要更新鲜的起点时优先**升级
// btcd**；确实需要额外手加时走 extraBtcCheckpoints（只能比 chaincfg 最高锚点更高）。
//
// 由此带来两条必须知道的运行约束：
//   - **锚点集合随 btcd 版本变化**：升级 btcd = 同时改变执行器与中继（neutrino 把头链硬锚在
//     btcd 的 checkpoint 上，见 blockmanager.findNextHeaderCheckpoint）的锚点集合；
//   - **所有节点必须同一个 build**：不同 build 的锚点不同，头链走到某个高度时 hash 必然对不上，
//     表现是"中继自己不报错、链上头链静默停滞"（执行器 VerifyCheckpoint 命中错锚点报
//     ErrBadCheckpoint→ErrBtcHeaderVerify）。regtest 链每次启动都会重建，任何网络都不该给它填锚点
//     （btcd 里 regtest 的 Net 就是 wire.TestNet，chaincfg 对它是 nil）。
var btcCheckpointTable = map[wire.BitcoinNet]map[uint64]string{}

// btcCheckpointNetNames 本执行器维护锚点的网络名，取值与 ltypes.GetBtcChainParams 一一对应。
//
// 这里用"网络名"而不是 wire.BitcoinNet 列表，是为了让锚点表与中继共用**同一个** net→params 映射
// （中继的 neutrino.Config.ChainParams 与执行器都来自 ltypes.GetBtcChainParams）：两边锚点一旦不同源，
// 就是文件头说的那类静默停滞。
var btcCheckpointNetNames = []string{"mainnet", "testnet3", "testnet4", "regtest", "simnet", "signet"}

// extraBtcCheckpoints 可选的**编译期**扩展层：在 chaincfg 内置锚点之上追加更高的锚点。
//
// 规则（见 buildBtcCheckpointTable，违反即 init() panic 拒绝启动）：
//  1. 必须**完整包含**本网络 chaincfg 的每一条锚点（同高度同 hash）——少一条或改一条都拒绝；
//  2. 只允许新增**严格高于** chaincfg 最高锚点的高度；
//  3. chaincfg 没有锚点的网络（regtest/testnet4/signet/simnet）不允许扩展。
//
// 用它做什么：主网需要"比 btcd 内置的最后一个锚点更新鲜"的起点时，**首选升级 btcd**（那样中继侧的
// 锚点集合会一起前进，两边天然同源）；只有在不能马上升级 btcd、又必须把 mainnet 起点往前挪时才手加
// 一条。
//
// **启用它 ⇒ 中继与执行器必然不同源，头链会 fail-closed 停住（设计如此，不是 bug）**：中继的
// bootstrap 起点只从 chaincfg 推导（neutrino 的 btcHeaderStartHeight），看不到本扩展层，于是本执行器
// 给出的最高锚点会比中继推出的起点高出一条（见 Query_GetBtcCheckpoint）；中继启动期的 bootstrap 起点
// 断言（neutrino 的 btcBootstrapAnchorGuard，比较的正是"本地推出的起点"与"链上这个锚点 + 1"）因此
// 判定两边不同源，**拒发任何 bootstrap 批**——链上头链停在空链状态，这是刻意的 fail-closed（宁可停住
// 也不让头链从一个中继没认可的起点长起来）。即：本扩展层只能与"中继侧同步获得同一个锚点"的改造配套
// 使用，单独加会让中继停止推进头链。运维遇到中继报"起点与链上锚点不配套"时，正确动作是**同步升级
// 两边的 btcd**（或去掉这里的额外锚点），而不是去找一个已经不存在的高度配置项。
//
// 当前为空：所有网络的锚点都来自 chaincfg，不需要扩展。
var extraBtcCheckpoints = map[wire.BitcoinNet]map[uint64]string{
	// 示例（当前不启用）：
	// wire.MainNet: {
	//	// 必须原样重述 chaincfg 的每一条锚点，再追加更高的一条。
	//	810000: "000000000000000000028028ca82b6aa81ce789e4eb9e0321b74c3cbaf405dd1",
	//	840000: "<getblockhash 840000 的结果>",
	// },
}

func init() {
	if err := reloadBtcCheckpointTable(extraBtcCheckpoints); err != nil {
		// fail-closed：锚点表是信任根，写错一条 = 头链在该高度永久过不去（静默停滞），
		// 写少一条 = 该网络失去锚点约束。这两张表都是编译期常量，写错属于代码缺陷，必须让它在
		// 启动/CI 阶段就炸出来，绝不允许带着一张错的表跑起来。
		panic(err)
	}
}

// reloadBtcCheckpointTable 按 btcCheckpointNetNames 重新生成 btcCheckpointTable。
// 生产上只在 init() 调用一次（表是只读的编译期数据）；单测用它验证扩展层的注入路径，
// 用完必须用 extraBtcCheckpoints 再调一次恢复原状。
func reloadBtcCheckpointTable(extra map[wire.BitcoinNet]map[uint64]string) error {
	for _, name := range btcCheckpointNetNames {
		params := ltypes.GetBtcChainParams(name)
		checkpoints, err := buildBtcCheckpointTable(params, extra[params.Net])
		if err != nil {
			return err
		}
		btcCheckpointTable[params.Net] = checkpoints
	}
	return nil
}

// buildBtcCheckpointTable 把 chaincfg 的内置锚点与扩展层合并成本网络的有效锚点表，并执行扩展层断言。
//
// 返回的表恒等于"chaincfg 的全部锚点 + extra 里更高的那些"；extra 与 chaincfg 冲突、少项、
// 或试图加更低的高度时返回错误（调用方 init() 直接 panic）。纯函数，便于单测覆盖各类违反。
func buildBtcCheckpointTable(params *chaincfg.Params, extra map[uint64]string) (map[uint64]string, error) {
	table := make(map[uint64]string, len(params.Checkpoints)+len(extra))
	// maxHeight chaincfg 内置的最高锚点高度；没有内置锚点时保持 0。
	var maxHeight uint64
	builtin := make(map[uint64]string, len(params.Checkpoints))
	for _, cp := range params.Checkpoints {
		if cp.Hash == nil {
			return nil, fmt.Errorf("btc checkpoint: chaincfg %s has a nil hash at height %d", params.Name, cp.Height)
		}
		height := uint64(cp.Height)
		table[height] = cp.Hash.String()
		builtin[height] = cp.Hash.String()
		if height > maxHeight {
			maxHeight = height
		}
	}
	if len(extra) == 0 {
		return table, nil
	}
	if len(builtin) == 0 {
		return nil, fmt.Errorf("btc checkpoint: extraBtcCheckpoints[%s] is set but chaincfg has no built-in "+
			"checkpoint for this net, so 'only above the highest chaincfg anchor' cannot be asserted; "+
			"nets without built-in anchors (regtest/testnet4/signet/simnet) must not be anchored at all",
			params.Name)
	}
	// 规则 1：完整包含 chaincfg（同高度同 hash）。改一条 = 手填错锚点，少一条 = 悄悄丢掉一个锚点。
	for height, hash := range builtin {
		got, ok := extra[height]
		if !ok {
			return nil, fmt.Errorf("btc checkpoint: extraBtcCheckpoints[%s] must restate every chaincfg anchor "+
				"unchanged, but height %d (%s) is missing", params.Name, height, hash)
		}
		if got != hash {
			return nil, fmt.Errorf("btc checkpoint: extraBtcCheckpoints[%s] changes the chaincfg anchor at height "+
				"%d: chaincfg=%s extra=%s", params.Name, height, hash, got)
		}
	}
	// 规则 2：只允许新增严格高于 chaincfg 最高锚点的高度。
	for height, hash := range extra {
		if _, ok := builtin[height]; ok {
			continue
		}
		if height <= maxHeight {
			return nil, fmt.Errorf("btc checkpoint: extraBtcCheckpoints[%s] adds height %d (%s), which is not "+
				"above the highest chaincfg anchor %d; the extension layer may only add fresher anchors",
				params.Name, height, hash, maxHeight)
		}
		table[height] = hash
	}
	return table, nil
}

type btcChainContext struct {
	params *chaincfg.Params
	// checkpoints 本网络已配置的锚点表，取自 btcCheckpointTable（无则为 nil）。
	checkpoints map[uint64]string
	// checkHeight 当前待校验区块的高度。btcd 的 ChainCtx 接口不带高度参数，而
	// FindPreviousCheckpoint 需要知道"校验到哪了"，故由调用方在每次校验前设置（setCheckHeight）。
	checkHeight int32
}

func newBtcChainContext(params *chaincfg.Params) *btcChainContext {
	return &btcChainContext{params: params, checkpoints: btcCheckpointTable[params.Net]}
}

// setCheckHeight 记录本次要校验的区块高度，供 FindPreviousCheckpoint 定位其之前的最近锚点。
func (c *btcChainContext) setCheckHeight(height int32) {
	c.checkHeight = height
}

func (c *btcChainContext) ChainParams() *chaincfg.Params {
	return c.params
}

func (c *btcChainContext) BlocksPerRetarget() int32 {
	return int32(c.params.TargetTimespan / c.params.TargetTimePerBlock)
}

func (c *btcChainContext) MinRetargetTimespan() int64 {
	target := int64(c.params.TargetTimespan / time.Second)
	return target / c.params.RetargetAdjustmentFactor
}

func (c *btcChainContext) MaxRetargetTimespan() int64 {
	target := int64(c.params.TargetTimespan / time.Second)
	return target * c.params.RetargetAdjustmentFactor
}

// VerifyCheckpoint 校验"该高度的区块 hash 是否与本网络已知锚点一致"。
// 表里没有记录该高度时返回 true——不知道就不假装知道，只对已维护的锚点做强约束。
func (c *btcChainContext) VerifyCheckpoint(height int32, hash *chainhash.Hash) bool {
	want, ok := c.checkpoints[uint64(height)]
	if !ok {
		return true
	}
	return hash != nil && want == hash.String()
}

// FindPreviousCheckpoint 返回当前校验高度之前最近的一个已知锚点。
// 本执行器只接受"接在当前 tip 上"的头（单个高度、不允许分叉），已提交的链不可能回退到锚点之前，
// 故该返回值只会用于 btcd 的"禁止在锚点之前分叉"判断，属防御性冗余。
func (c *btcChainContext) FindPreviousCheckpoint() (blockchain.HeaderCtx, error) {
	height, hash, ok := previousCheckpoint(c.checkpoints, c.checkHeight)
	if !ok {
		return nil, nil
	}
	return newBtcHeaderContext(&ltypes.BtcHeader{Height: height, Hash: hash}, nil, nil), nil
}

// previousCheckpoint 取 (height, hash) 之前（严格小于）最近的一个锚点。
func previousCheckpoint(checkpoints map[uint64]string, height int32) (uint64, string, bool) {
	if height <= 0 {
		return 0, "", false
	}
	return highestCheckpointBelow(checkpoints, uint64(height))
}

// highestCheckpoint 取表里最高的锚点（= 本网络最新鲜的信任根，bootstrap 起点就锚在它上面）。
func highestCheckpoint(checkpoints map[uint64]string) (uint64, string, bool) {
	return highestCheckpointBelow(checkpoints, math.MaxUint64)
}

// highestCheckpointBelow 取表中严格小于 limit 的最高锚点；没有则 ok=false。
func highestCheckpointBelow(checkpoints map[uint64]string, limit uint64) (uint64, string, bool) {
	var best uint64
	var bestHash string
	found := false
	for h, hash := range checkpoints {
		if h >= limit {
			continue
		}
		if !found || h > best {
			best, bestHash, found = h, hash, true
		}
	}
	return best, bestHash, found
}

type btcHeaderContext struct {
	header   *ltypes.BtcHeader
	parent   blockchain.HeaderCtx
	localDB  dbm.KV
	ancestor map[uint64]blockchain.HeaderCtx
}

func newBtcHeaderContext(header *ltypes.BtcHeader, parent blockchain.HeaderCtx, localDB dbm.KV) *btcHeaderContext {
	ctx := &btcHeaderContext{
		header:   header,
		parent:   parent,
		localDB:  localDB,
		ancestor: make(map[uint64]blockchain.HeaderCtx),
	}
	ctx.ancestor[header.GetHeight()] = ctx
	return ctx
}

func (h *btcHeaderContext) Height() int32 {
	return int32(h.header.GetHeight())
}

func (h *btcHeaderContext) Bits() uint32 {
	return uint32(h.header.GetBits())
}

func (h *btcHeaderContext) Timestamp() int64 {
	return h.header.GetTime()
}

func (h *btcHeaderContext) Parent() blockchain.HeaderCtx {
	return h.parent
}

func (h *btcHeaderContext) RelativeAncestorCtx(distance int32) blockchain.HeaderCtx {
	if distance <= 0 {
		return h
	}

	curr := blockchain.HeaderCtx(h)
	for i := int32(0); i < distance && curr != nil; i++ {
		curr = curr.Parent()
	}
	if curr != nil {
		if ancestor, ok := curr.(*btcHeaderContext); ok {
			h.ancestor[ancestor.header.GetHeight()] = ancestor
		}
		return curr
	}

	targetHeight := int64(h.header.GetHeight()) - int64(distance)
	if targetHeight < 0 {
		return nil
	}
	if ancestor, ok := h.ancestor[uint64(targetHeight)]; ok {
		return ancestor
	}
	if h.localDB == nil {
		return nil
	}
	targetHeader, err := getBtcHeader(h.localDB, uint64(targetHeight))
	if err != nil {
		return nil
	}
	ancestor := newBtcHeaderContext(targetHeader, nil, h.localDB)
	h.ancestor[targetHeader.GetHeight()] = ancestor
	return ancestor
}

// maxAnchorTraceDepth 沿 prevHash 回查 localDB 的最大步数（纯防御性上限，防止恶意构造的超长回查）。
const maxAnchorTraceDepth = 1 << 20

// checkBootstrapAnchor 校验 bootstrap（链上还没有任何 BTC 头）时的首个头是否锚定在该网络的真实链上。
//
// 为什么必须做：没有任何锚点时，"谁先提交谁就定义这条链"——攻击者把 Bits 设成网络最大目标
// （= powLimit，PoW sanity 只校验"自己的 hash ≤ 自己声明的 Bits"，因此必过），可以秒挖出一个头
// 作为链的起点，随后在同一个难度调整窗口内难度不变，一路自造出一条承载伪造充值交易的"比特币链"。
// 唯一的根治办法是要求首个头必须落在真实链上：
//
//	① 直接接创世：高度 1 且 prevHash == 该网络创世 hash（由 btcd chaincfg 给出，无需维护数据）；
//	② 命中已知锚点：prevHash == btcCheckpointTable 中 (height-1) 的区块 hash；
//	③ localDB 可回溯：沿 prevHash 逐级回查已存的头，最终到达创世或某个锚点。
//
// 三条都不满足即拒绝。mainnet/testnet3 的锚点来自 btcd chaincfg（见 btcCheckpointTable），
// 因此中继的提交起点必须正好是"本网络最高锚点 + 1"；这个起点**不是配置项**，而是中继从**同一份**
// chaincfg 自己推出来的（neutrino 的 btcHeaderStartHeight；中继启动期还会拿本执行器给出的锚点做一次
// 同样的断言，见 neutrino 的 btcBootstrapAnchorGuard）。两边推不出同一个值只可能是两边 btcd 的
// checkpoint 表不同源（含本执行器启用了 extraBtcCheckpoints，见那里写的 fail-closed 后果）。
func checkBootstrapAnchor(first *ltypes.BtcHeader, params *chaincfg.Params, ldb dbm.KV) error {
	height := first.GetHeight()
	genesisHash := params.GenesisHash.String()
	checkpoints := btcCheckpointTable[params.Net]

	// reject 拒绝时把"照做就能过"的信息一并给出（L1）：原来的 ErrBtcHeaderNoAnchor 只说被拒了，
	// 运维既看不到本网络有哪些锚点，也不知道中继本该从哪个高度起 —— 只能猜。
	reject := func() error {
		detail := anchorRejectDetail(first, params, checkpoints)
		elog.Error("checkBootstrapAnchor first btc header cannot be anchored to the real chain "+
			"(neither genesis, nor a known checkpoint, nor traceable in localdb)", "detail", detail)
		return fmt.Errorf("%w: %s", ErrBtcHeaderNoAnchor, detail)
	}

	// 高度 0 只可能是创世块本身。
	if height == 0 {
		if first.GetHash() != genesisHash {
			return reject()
		}
		return nil
	}
	// ① 直接接创世。
	if height == 1 && first.GetPreviousHash() == genesisHash {
		return nil
	}
	// ② 首个头的父块刚好是一个已知锚点。
	if hash, ok := checkpoints[height-1]; ok && hash == first.GetPreviousHash() {
		return nil
	}
	// ③ 沿 prevHash 回查 localDB。
	if traceHeaderToAnchor(height-1, first.GetPreviousHash(), params, ldb, checkpoints) {
		return nil
	}
	return reject()
}

// anchorRejectDetail 组装 bootstrap 锚点被拒时的可操作信息（错误信息与日志同源）：
// 本网络已知锚点高度列表、首个头的高度、以及中继本该推出的提交起点（= 最高锚点 + 1）。
//
// 起点那一项（expectedRelayStartHeight）是**诊断值、不是配置项**：中继自己从同一份 chaincfg 推出它
// （neutrino 的 btcHeaderStartHeight），没有任何可改的键。给出它是为了在"两边对不上"时能直接看出根因
// 是两边 btcd 的 checkpoint 表不同源（典型成因：两边 build 用了不同版本的 btcd，或本执行器用
// extraBtcCheckpoints 加了中继没有的锚点），对应动作是同步升级两边的 btcd / 去掉额外锚点。
func anchorRejectDetail(first *ltypes.BtcHeader, params *chaincfg.Params, checkpoints map[uint64]string) string {
	heights := make([]uint64, 0, len(checkpoints))
	for h := range checkpoints {
		heights = append(heights, h)
	}
	sort.Slice(heights, func(i, j int) bool { return heights[i] < heights[j] })

	detail := fmt.Sprintf("net=%s firstHeight=%d firstPrevHash=%s genesisHash=%s knownCheckpointHeights=%v",
		params.Name, first.GetHeight(), first.GetPreviousHash(), params.GenesisHash.String(), heights)
	if top, topHash, ok := highestCheckpoint(checkpoints); ok {
		detail += fmt.Sprintf(" highestCheckpoint=%d:%s expectedRelayStartHeight=%d", top, topHash, top+1)
	} else {
		detail += " highestCheckpoint=none (this net has no anchor: bootstrap only works from genesis, expectedRelayStartHeight=1)"
	}
	return detail + anchorMismatchHint
}

// anchorMismatchHint 拒绝信息末尾的成因说明（错误信息与日志同源）：提交起点不是可配置项，两边对不上
// 时没有键可改，唯一成因是两边 btcd 的 checkpoint 表不同源（见 extraBtcCheckpoints 与
// anchorRejectDetail 的说明）。
const anchorMismatchHint = " (start height is derived by the relay from the same btcd chaincfg, it is not a config item: " +
	"sync-upgrade btcd on both sides, or drop the extra compile-time anchors added via extraBtcCheckpoints)"

// traceHeaderToAnchor 判断"高度 height、hash 为 hash 的头"是否能沿 prevHash 回溯到锚点
// （创世或 btcCheckpointTable 中的记录）。依赖 localDB 里按高度存的历史头。
func traceHeaderToAnchor(height uint64, hash string, params *chaincfg.Params, ldb dbm.KV, checkpoints map[uint64]string) bool {
	if ldb == nil {
		return false
	}
	genesisHash := params.GenesisHash.String()
	for i := 0; i < maxAnchorTraceDepth; i++ {
		if height == 0 {
			return hash == genesisHash
		}
		if want, ok := checkpoints[height]; ok && want == hash {
			return true
		}
		header, err := getBtcHeader(ldb, height)
		if err != nil || header.GetHash() != hash {
			return false
		}
		height--
		hash = header.GetPreviousHash()
	}
	return false
}

func toWireHeader(head *ltypes.BtcHeader) (*wire.BlockHeader, error) {
	preHash, err := chainhash.NewHashFromStr(head.GetPreviousHash())
	if err != nil {
		return nil, err
	}
	merkleRoot, err := chainhash.NewHashFromStr(head.GetMerkleRoot())
	if err != nil {
		return nil, err
	}

	h := &wire.BlockHeader{}
	h.Version = int32(head.Version)
	h.PrevBlock = *preHash
	h.MerkleRoot = *merkleRoot
	h.Bits = uint32(head.Bits)
	h.Nonce = uint32(head.Nonce)
	h.Timestamp = time.Unix(head.Time, 0)

	return h, nil
}
