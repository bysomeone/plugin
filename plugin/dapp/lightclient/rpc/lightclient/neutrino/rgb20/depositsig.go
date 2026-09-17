package rgb20

import (
	"fmt"
	"sync"
	"time"

	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
)

/*
 * 已签充值的落盘产物（签名落盘、重试优先重发）。
 *
 * 背景：submitDeposit 原本每次重试都走"组 DepositAsset → TSS 签 → 提交"整条链路。这带来两个问题：
 *
 *  1. 链上深度不够时（B8：链上要求 canonical tip >= H + N - 1，而中继上链的头链只到 best - B），
 *     首笔提交注定被拒，而每次 30s 重试都会**空跑一整轮 GG18 签名**；
 *  2. 签名产物只存在于内存里，进程重启即丢，哪怕是"签名成功、提交瞬间失败"这种最该直接续上的情况。
 *
 * 因此把签名轮次的产出落盘：**先落盘、再提交**；重试时若已有落盘产物就只重发，省掉一轮 GG18。
 *
 * 注意这只是**性能缓存**，不是重试的唯一依据：产物丢了（落盘失败后重启、数据目录被清、从旧快照
 * 恢复）就再签一轮 —— 充值重签是逐字节幂等的（签的是 C = sha256(Encode(DepositAsset{thresholdSig:nil}))，
 * 只有金额/地址/符号 + SPV 证明，没有 nonce、时间戳或 UTXO 选择），链上还按 txid 去重兜底。
 *
 * 粒度选"完整 DepositAsset"而不是只存 thresholdSig 字节：链上验签的消息是
 * C = sha256(types.Encode(DepositAsset{thresholdSig:nil}))（与 tss.computeRgb20DepositMsg、
 * rgbx 执行器 computeDepositSignMessage 同口径）—— 金额、目标地址、资产符号、SPV 证明都在这个编码里。
 * 重发时若重新构造 DepositAsset（例如重新取一次 SPV，merkle 分支/高度与签名时不同），签名就对不上了；
 * 存整份对象则"签的是什么就重发什么"，不存在这个风险。
 */

// depositSigBucket 已签充值产物的顶层 bucket：key = 付款交易 txid，value = JSON(SignedDepositArtifact)。
var depositSigBucket = []byte("rgb20-deposit-sig")

// SignedDepositArtifact 一次充值签名轮次的落盘产物（可重发的完整提交对象）。
type SignedDepositArtifact struct {
	Txid      string `json:"txid"`      // 付款交易 txid（bucket key，冗余存一份便于排查）
	ReceiveID string `json:"receiveId"` // 桥侧 receive id
	Height    uint64 `json:"height"`    // 付款交易所在 BTC 高度（深度门控与观测用）
	SessionID string `json:"sessionId"` // 产生签名的 TSS session id（排查用）
	SignedAt  int64  `json:"signedAt"`  // 签名时间（unix 秒，排查用）
	// Deposit 签名后的完整充值对象：thresholdSig 已填，TxProof 与签名时的完全一致。
	Deposit *rtypes.DepositAsset `json:"deposit"`

	// durable 是否已落盘（纯运行时状态，不序列化）。false = 只在内存里（落盘失败），
	// 调用方必须在提交前先把落盘重试成功（见 submitDeposit）。
	durable bool
}

// DepositSignatureStore 已签充值产物存储（内存缓存 + KVStore 持久化，与 SealIndex/ReceiveStore 同构）。
type DepositSignatureStore struct {
	mu    sync.RWMutex
	store KVStore
	// cache txid -> artifact
	cache map[string]*SignedDepositArtifact
}

func newDepositSignatureStore(store KVStore) *DepositSignatureStore {
	s := &DepositSignatureStore{store: store, cache: make(map[string]*SignedDepositArtifact)}
	s.load()
	return s
}

func (s *DepositSignatureStore) load() {
	_ = s.store.ForEach(depositSigBucket, func(k, v []byte) error {
		art := &SignedDepositArtifact{}
		if err := unmarshalJSON(v, art); err != nil || art.Deposit == nil {
			// 解不出来的条目直接忽略：它不能用来重发（宁可按"没有产物"处理，由上层决定怎么走）。
			log.Error("deposit sig load: bad artifact", "txid", string(k), "err", err)
			return nil
		}
		if art.Txid == "" {
			art.Txid = string(k)
		}
		art.durable = true
		s.cache[art.Txid] = art
		return nil
	})
}

// Get 取该付款交易的已签产物；没有则返回 (nil, nil)。
func (s *DepositSignatureStore) Get(txid string) (*SignedDepositArtifact, error) {
	if txid == "" {
		return nil, fmt.Errorf("empty txid")
	}
	s.mu.RLock()
	art := s.cache[txid]
	s.mu.RUnlock()
	return art, nil
}

// Put 写入产物：先放内存缓存，再尽力落盘。
//
// 落盘失败时**缓存里仍然有这份产物**（返回错误给调用方）：签名已经产出，不能因为一次写盘失败就
// 丢掉它；调用方下次调用 Put 即可重试落盘。契约：调用方在产物 durable 之前**不得提交**
// （见 submitDeposit 的"先落盘、再提交"）—— 进程若在落盘成功前重启，产物随之丢失，下一轮重签一份
// 完全相同的对象即可（重签幂等，只是白花一轮 GG18）。
func (s *DepositSignatureStore) Put(art *SignedDepositArtifact) error {
	if art == nil || art.Txid == "" || art.Deposit == nil {
		return fmt.Errorf("invalid signed deposit artifact")
	}
	if art.SignedAt == 0 {
		art.SignedAt = time.Now().Unix()
	}
	s.mu.Lock()
	s.cache[art.Txid] = art
	s.mu.Unlock()
	return s.persist(art)
}

// persist 把产物写进 KVStore（已落盘则直接返回）。
func (s *DepositSignatureStore) persist(art *SignedDepositArtifact) error {
	if art.durable {
		return nil
	}
	if err := s.store.Put(depositSigBucket, []byte(art.Txid), mustJSON(art)); err != nil {
		return err
	}
	art.durable = true
	return nil
}

// Delete 删除产物（铸造成功后清理，避免只增不删）。
func (s *DepositSignatureStore) Delete(txid string) error {
	if txid == "" {
		return nil
	}
	s.mu.Lock()
	delete(s.cache, txid)
	s.mu.Unlock()
	return s.store.Delete(depositSigBucket, []byte(txid))
}

// Len 当前条目数（测试/观测用）。
func (s *DepositSignatureStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.cache)
}

// --- 充值重试路径的日志限流 ---

// depositNoteOnce 同一个 key 只允许报一次（30s 轮询会把同一个状态反复带回来，避免刷屏）。
// key 形如 "depth:<txid>" / "missing-sig:<txid>"：同一个 txid 的不同问题各自报一次。
type depositNoteOnce struct {
	mu   sync.Mutex
	seen map[string]bool
}

func newDepositNoteOnce() *depositNoteOnce {
	return &depositNoteOnce{seen: make(map[string]bool)}
}

func (o *depositNoteOnce) allow(key string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.seen[key] {
		return false
	}
	o.seen[key] = true
	return true
}
