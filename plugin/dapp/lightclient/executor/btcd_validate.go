package executor

import (
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
// 填法：从任意可信全节点执行 `getblockhash <height>`，把 (height, hash) 填进来即可。
// 高度建议取**已深度确认**的近期高度（如某个难度调整周期的边界），并随版本发布滚动更新。
// 留空表示该网络只接受"从创世锚定"的 bootstrap（各网络创世 hash 由 btcd chaincfg 给出，
// 无需在此维护）。regtest 链每次启动都会重建，**不要**给它填 checkpoint。
var btcCheckpointTable = map[wire.BitcoinNet]map[uint64]string{
	wire.MainNet: {
		// TODO(上线前填写)：例如 840000: "<getblockhash 840000 的结果>"。
		// 不填则 mainnet 只能从创世（高度 1）开始提交头，实际不可用。
	},
	wire.TestNet3: {
		// TODO(上线前填写)：同上，填一个 testnet3 的近期高度。
	},
	// 以下网络暂不需要锚点表（testnet4/signet/simnet 未用于生产充值）。
	wire.TestNet4: {},
	wire.SigNet:   {},
	wire.SimNet:   {},
	// regtest 链每次重建，填了反而是错锚点（btcd 里 regtest 的 Net 就是 wire.TestNet）。
	wire.TestNet: {},
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
	var best uint64
	var bestHash string
	found := false
	for h, hash := range checkpoints {
		if h >= uint64(height) {
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
// 三条都不满足即拒绝。注意 mainnet 必须先在 btcCheckpointTable 里填一个近期高度，否则只能
// 从创世开始（几百万个头，不可用）。
func checkBootstrapAnchor(first *ltypes.BtcHeader, params *chaincfg.Params, ldb dbm.KV) error {
	height := first.GetHeight()
	genesisHash := params.GenesisHash.String()

	// 高度 0 只可能是创世块本身。
	if height == 0 {
		if first.GetHash() != genesisHash {
			return ErrBtcHeaderNoAnchor
		}
		return nil
	}
	// ① 直接接创世。
	if height == 1 && first.GetPreviousHash() == genesisHash {
		return nil
	}
	// ② 首个头的父块刚好是一个已知锚点。
	checkpoints := btcCheckpointTable[params.Net]
	if hash, ok := checkpoints[height-1]; ok && hash == first.GetPreviousHash() {
		return nil
	}
	// ③ 沿 prevHash 回查 localDB。
	if traceHeaderToAnchor(height-1, first.GetPreviousHash(), params, ldb, checkpoints) {
		return nil
	}
	return ErrBtcHeaderNoAnchor
}

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
