package rgb20

import (
	"bytes"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/wire"
)

/*
 * 签名侧"已签集合"（A3 后半）。
 *
 * 背景：签名节点在签 threshold_sig（= chain33 铸币授权）之前做的去重，原先只有"本地 receive 已
 * minted 则拒"（ValidateDepositConsignment 里的那段）—— 而 validator 节点的本地 store 里通常
 * 根本没有 receive 记录（receive 由官方节点经 CreateReceive 创建），等于完全没有去重：协调者可以
 * 对同一笔 BTC 付款反复发起签名轮次，让每个签名节点反复产出签名产物、反复跑 GG18。
 *
 * 这里给签名节点自己记账：本节点签过的付款交易 txid 写进本地 KVStore 的一个顶层 bucket，之后同一
 * txid 再来直接拒绝，不进入签名轮次。语义要点：
 *
 *   - **签名成功后**才登记（不是签名前）：签名失败不烧 txid，合法重试不受影响；
 *   - 记录**付款交易所在 BTC 高度**作为 TTL 锚点：链上 canonical tip 高度 - 记录高度 >= TTL 即
 *     视为过期，过期条目被清掉、同一 txid 允许再签（给"保留期之外确实需要重签"留出口）；
 *   - TTL <= 0（默认 0）= 只增不删；改成正数后**重启即按新 TTL 清理一次**（见
 *     neutrinoClient.Start 里的启动清理），把只增不删期间攒下的旧条目一并删掉。
 *
 * 单位为什么是 BTC 高度（而不是墙上时钟）：高度由链决定，四个签名节点看到的是同一个、单调的计数，
 * 不受本机时钟漂移/回拨影响；代价是 TTL 判定要读一次链上 tip（GetBtcLastHeader 查询），因此
 * **只在 TTL > 0 且确有这么一条记录要判定时才查**，默认配置下本机制零查询、零行为变化。
 *
 * 这只是纵深防御：即便这里失守，链上 rgbx 仍按 txid 去重（formatDepositUsedTxIDKey），不会多铸。
 */

// signedDepositBucket 已签集合的顶层 bucket：key = 付款交易 txid，value = JSON(SignedDeposit)。
// bucket 不存在 = 空集 = 与引入本机制前完全一致的行为（不引入新存储引擎，复用同一 KVStore）。
var signedDepositBucket = []byte("rgb20-signed-deposit")

// SignedDeposit 本节点已为某笔充值付款交易签过 threshold_sig 的记录。
type SignedDeposit struct {
	Txid string `json:"txid"`
	// Height 付款交易所在的 BTC 高度（取自签名前已校验的 SPV 证明，TxProof.BlockHeight）。
	// TTL 的锚点：链上 canonical tip 高度 - Height >= TTL 视为过期。
	Height uint64 `json:"height"`
	// SignedAt 本节点签署时间（unix 秒）；只用于排查，不参与过期判定。
	SignedAt int64 `json:"signedAt"`
}

// SignedDepositSet 已签集合（内存缓存 + KVStore 持久化，与 SealIndex/ReceiveStore 同构）。
type SignedDepositSet struct {
	mu    sync.RWMutex
	store KVStore
	cache map[string]*SignedDeposit
	// ttl 保留期（BTC 块数）；<= 0 表示只增不删。
	ttl int64
	// now 取当前 BTC 高度（链上 canonical tip）。仅在 TTL > 0 且确有记录需要判定时调用。
	now func() (uint64, error)
}

func newSignedDepositSet(store KVStore, ttl int64, now func() (uint64, error)) *SignedDepositSet {
	s := &SignedDepositSet{
		store: store,
		cache: make(map[string]*SignedDeposit),
		ttl:   ttl,
		now:   now,
	}
	s.load()
	return s
}

func (s *SignedDepositSet) load() {
	_ = s.store.ForEach(signedDepositBucket, func(k, v []byte) error {
		entry := &SignedDeposit{}
		if err := unmarshalJSON(v, entry); err != nil {
			return nil
		}
		if entry.Txid == "" {
			entry.Txid = string(k)
		}
		s.cache[entry.Txid] = entry
		return nil
	})
}

// IsSigned 判断 txid 是否仍在保留期内（= 本节点不应再为它签名）。
// 返回的 error 表示**无法判定**（当前高度取不到）：调用方必须按"已签"处理（fail-closed），
// 而不是放行 —— 本机制是对可疑协调者的纵深防御，判不了就不能放行。TTL <= 0 时不查高度。
func (s *SignedDepositSet) IsSigned(txid string) (bool, error) {
	if txid == "" {
		return false, nil
	}
	s.mu.RLock()
	entry, ok := s.cache[txid]
	s.mu.RUnlock()
	if !ok {
		return false, nil
	}
	if s.ttl <= 0 {
		return true, nil
	}
	tip, err := s.currentHeight()
	if err != nil {
		return false, fmt.Errorf("signed set: current btc height unavailable: %w", err)
	}
	if !s.isExpired(entry, tip) {
		return true, nil
	}
	// 已过期：清掉（顺带清掉同一批过期的其它记录）并放行。
	s.pruneWithTip(tip)
	return false, nil
}

// Mark 登记 txid 已签（幂等：已存在则不重复写库）。
// height 为付款交易所在 BTC 高度（TTL 锚点）。
func (s *SignedDepositSet) Mark(txid string, height uint64) error {
	if txid == "" {
		return fmt.Errorf("empty txid")
	}
	s.mu.RLock()
	_, exists := s.cache[txid]
	s.mu.RUnlock()
	if exists {
		return nil
	}
	entry := &SignedDeposit{Txid: txid, Height: height, SignedAt: time.Now().Unix()}
	if err := s.store.Put(signedDepositBucket, []byte(txid), mustJSON(entry)); err != nil {
		return err
	}
	s.mu.Lock()
	s.cache[txid] = entry
	s.mu.Unlock()
	return nil
}

// Has 判断 txid 是否在集合里，**不看 TTL**（测试/观测用）。
func (s *SignedDepositSet) Has(txid string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.cache[txid]
	return ok
}

// Len 当前记录条数（测试/观测用）。
func (s *SignedDepositSet) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.cache)
}

// Prune 清理所有已过期的记录，返回删除条数。
// TTL <= 0（只增不删）时是 no-op，**且不做任何链上查询**。
func (s *SignedDepositSet) Prune() (int, error) {
	if s.ttl <= 0 {
		return 0, nil
	}
	tip, err := s.currentHeight()
	if err != nil {
		return 0, fmt.Errorf("signed set prune: current btc height unavailable: %w", err)
	}
	return s.pruneWithTip(tip), nil
}

func (s *SignedDepositSet) currentHeight() (uint64, error) {
	if s.now == nil {
		return 0, fmt.Errorf("btc height clock not set")
	}
	return s.now()
}

// isExpired 判断记录在 tip 高度下是否已过期。TTL <= 0 时永不过期。
func (s *SignedDepositSet) isExpired(entry *SignedDeposit, tip uint64) bool {
	if s.ttl <= 0 || entry == nil {
		return false
	}
	// 链上 tip 落后于记录高度（重组/换网/头链刚 bootstrap）时不判过期：判不了就保持"已签"。
	if tip < entry.Height {
		return false
	}
	return tip-entry.Height >= uint64(s.ttl)
}

// pruneWithTip 按给定 tip 高度批量删除过期记录，返回删除条数（调用方已确保 TTL > 0）。
// 先快照待删 key、再逐个删：不在持锁遍历时改缓存。
func (s *SignedDepositSet) pruneWithTip(tip uint64) int {
	s.mu.RLock()
	stale := make([]string, 0, len(s.cache))
	for txid, entry := range s.cache {
		if s.isExpired(entry, tip) {
			stale = append(stale, txid)
		}
	}
	s.mu.RUnlock()
	removed := 0
	for _, txid := range stale {
		if err := s.store.Delete(signedDepositBucket, []byte(txid)); err != nil {
			log.Error("signed set prune delete", "txid", txid, "err", err)
			continue
		}
		s.mu.Lock()
		delete(s.cache, txid)
		s.mu.Unlock()
		removed++
	}
	return removed
}

// --- 适配器侧（签名节点签名前后调用） ---

// CheckDepositSigned 签名侧去重（A3 后半）：payload 的付款交易已在本节点签过则返回错误（拒绝签名）。
// 已在保留期外的旧记录会被清掉并放行。无法判定（当前高度取不到）时同样返回错误（fail-closed）。
func (a *Adapter) CheckDepositSigned(payload *DepositSignPayload) error {
	txid, err := depositTxid(payload)
	if err != nil {
		return err
	}
	if a.signed == nil {
		return nil
	}
	signed, err := a.signed.IsSigned(txid)
	if err != nil {
		return fmt.Errorf("signed-set check tx %s: %w", txid, err)
	}
	if signed {
		return fmt.Errorf("deposit tx %s already signed by this node", txid)
	}
	return nil
}

// MarkDepositSigned 登记 payload 的付款交易为"已签"（幂等）。
// **只应在签名成功之后调用**：签名失败不登记，避免把 txid 烧掉、误伤合法重试。
// 登记成功后顺带清理过期记录（TTL > 0 时；清理失败只记日志，不影响本次签名结果）。
func (a *Adapter) MarkDepositSigned(payload *DepositSignPayload) error {
	txid, err := depositTxid(payload)
	if err != nil {
		return err
	}
	if a.signed == nil {
		return nil
	}
	if err := a.signed.Mark(txid, depositBlockHeight(payload)); err != nil {
		return err
	}
	if n, err := a.signed.Prune(); err != nil {
		log.Error("MarkDepositSigned prune expired", "err", err)
	} else if n > 0 {
		log.Info("MarkDepositSigned pruned expired signed deposits", "count", n)
	}
	return nil
}

// PruneSignedDeposits 按当前 TTL 清理已过期的已签记录（启动/改配置重启后立即执行一次）。
// TTL <= 0（只增不删）时是 no-op。返回错误表示本次清理没做成（取不到链上高度），
// 调用方可重试；已签记录的判定本身不受影响。
func (a *Adapter) PruneSignedDeposits() error {
	if a.signed == nil {
		return nil
	}
	n, err := a.signed.Prune()
	if err != nil {
		return err
	}
	if n > 0 {
		log.Info("PruneSignedDeposits removed expired signed deposits", "count", n, "ttl", a.signed.ttl)
	}
	return nil
}

// depositTxid 从签名节点收到的 rgb20-deposit 消息里严格解析付款交易 txid。
// 口径与签名前的 SPV 校验（neutrino.VerifyDepositSpv）一致：按 btcwire 规范编码反序列化，
// 且必须完整消费 reader —— 尾部带多余字节的"同一笔交易的另一份编码"既不能进集合、
// 也不能拿它绕过集合（A3 前半已在 SPV 侧拒绝，这里独立再挡一次）。
func depositTxid(payload *DepositSignPayload) (string, error) {
	if payload == nil || payload.Deposit == nil {
		return "", fmt.Errorf("invalid rgb20-deposit payload")
	}
	txData := payload.Deposit.GetTxProof().GetTxData()
	if len(txData) == 0 {
		return "", fmt.Errorf("empty deposit tx data")
	}
	reader := bytes.NewReader(txData)
	var tx wire.MsgTx
	if err := tx.DeserializeNoWitness(reader); err != nil {
		return "", fmt.Errorf("decode deposit tx: %w", err)
	}
	if reader.Len() != 0 {
		return "", fmt.Errorf("non-canonical deposit tx data: %d trailing bytes", reader.Len())
	}
	return tx.TxHash().String(), nil
}

// depositBlockHeight 付款交易所在 BTC 高度（TTL 锚点）。取自签名前已校验的 SPV 证明。
func depositBlockHeight(payload *DepositSignPayload) uint64 {
	if payload == nil || payload.Deposit == nil {
		return 0
	}
	return payload.Deposit.GetTxProof().GetBlockHeight()
}
