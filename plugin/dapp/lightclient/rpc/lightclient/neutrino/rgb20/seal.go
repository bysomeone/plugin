package rgb20

import (
	"fmt"
	"sync"
)

// Seal 状态机：pending-mint（待铸造）→ minted（已铸造）→ consumed（已消费）。
// 规则（HR-5）：pending-mint 的 seal 不得被提现选择，也不得进 BTC listUnspent 费池。
const (
	SealStatusPendingMint = "pending-mint"
	SealStatusMinted      = "minted"
	SealStatusConsumed    = "consumed"
)

// Seal 记录一个 RGB seal（UTXO 级）。
type Seal struct {
	Outpoint       string `json:"outpoint"`       // "txid:vout"
	AssetID        string `json:"assetId"`        // rgb:...
	AssetSymbol    string `json:"assetSymbol"`    // RGB20_USDT
	Amount         int64  `json:"amount"`         // 资产数量（最小单位）
	BtcValue       int64  `json:"btcValue"`       // 该 UTXO 的 BTC 值（sat）
	Status         string `json:"status"`         // pending-mint | minted | consumed
	MaturityHeight uint32 `json:"maturityHeight"` // 确认高度
}

var sealBucket = []byte("rgb20-seal")

// SealIndex 维护 seal 索引状态机。内存缓存 + KVStore 持久化。
type SealIndex struct {
	mu    sync.RWMutex
	store KVStore
	// cache outpoint -> Seal
	cache map[string]*Seal
}

func newSealIndex(store KVStore) *SealIndex {
	idx := &SealIndex{store: store, cache: make(map[string]*Seal)}
	idx.load()
	return idx
}

func (s *SealIndex) load() {
	_ = s.store.ForEach(sealBucket, func(k, v []byte) error {
		seal := &Seal{}
		if err := unmarshalJSON(v, seal); err != nil {
			return nil
		}
		s.cache[string(k)] = seal
		return nil
	})
}

// Add 新增 seal（默认 pending-mint）。
func (s *SealIndex) Add(seal *Seal) error {
	if seal == nil || seal.Outpoint == "" {
		return fmt.Errorf("invalid seal")
	}
	if seal.Status == "" {
		seal.Status = SealStatusPendingMint
	}
	s.mu.Lock()
	s.cache[seal.Outpoint] = seal
	s.mu.Unlock()
	return s.store.Put(sealBucket, []byte(seal.Outpoint), mustJSON(seal))
}

// Get 读取 seal。
func (s *SealIndex) Get(outpoint string) (*Seal, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seal, ok := s.cache[outpoint]
	if !ok {
		return nil, false
	}
	cp := *seal
	return &cp, true
}

// MarkMinted 将 pending-mint seal 置为 minted。
func (s *SealIndex) MarkMinted(outpoint string) error {
	var data []byte
	s.mu.Lock()
	seal, ok := s.cache[outpoint]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("seal %s not found", outpoint)
	}
	if seal.Status == SealStatusPendingMint {
		seal.Status = SealStatusMinted
	}
	data = mustJSON(seal)
	s.mu.Unlock()
	return s.store.Put(sealBucket, []byte(outpoint), data)
}

// MarkConsumed 标记 seal 已消费（提现被花费）。
func (s *SealIndex) MarkConsumed(outpoint string) error {
	var data []byte
	s.mu.Lock()
	seal, ok := s.cache[outpoint]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("seal %s not found", outpoint)
	}
	seal.Status = SealStatusConsumed
	data = mustJSON(seal)
	s.mu.Unlock()
	return s.store.Put(sealBucket, []byte(outpoint), data)
}

// IsSealOutpoint 判断 outpoint 是否为已登记的 RGB seal。
// btcwallet.listUnspent 用它排除 seal（含 pending-mint）出 BTC 费池。
func (s *SealIndex) IsSealOutpoint(outpoint string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.cache[outpoint]
	return ok
}

// IsPendingMint 判断 seal 是否处于 pending-mint。
func (s *SealIndex) IsPendingMint(outpoint string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seal, ok := s.cache[outpoint]
	return ok && seal.Status == SealStatusPendingMint
}

// ListMinted 列出可被提现选择的 minted seals。
func (s *SealIndex) ListMinted(assetSymbol string) []*Seal {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Seal, 0, len(s.cache))
	for _, seal := range s.cache {
		if seal.Status == SealStatusMinted && (assetSymbol == "" || seal.AssetSymbol == assetSymbol) {
			cp := *seal
			out = append(out, &cp)
		}
	}
	return out
}

// ListByStatus 按状态列出 seals。
func (s *SealIndex) ListByStatus(status, assetSymbol string) []*Seal {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Seal, 0, len(s.cache))
	for _, seal := range s.cache {
		if (status == "" || seal.Status == status) && (assetSymbol == "" || seal.AssetSymbol == assetSymbol) {
			cp := *seal
			out = append(out, &cp)
		}
	}
	return out
}

// All 返回全部 seals（拷贝）。
func (s *SealIndex) All() []*Seal {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Seal, 0, len(s.cache))
	for _, seal := range s.cache {
		cp := *seal
		out = append(out, &cp)
	}
	return out
}
