package rgb20

import (
	"fmt"
	"time"
)

// Receive 状态机。
const (
	ReceiveStatusCreated  = "created" // 侧车已建 receive，等待用户付款
	ReceiveStatusSettled  = "settled" // consignment 已结算，等待确认铸造
	ReceiveStatusMinted   = "minted"  // 已铸造到 Chain33
	ReceiveStatusFailed   = "failed"  // 结算/铸造失败
	ReceiveStatusTimedOut = "timeout" // 超时未付款
)

var (
	receiveBucket = []byte("rgb20-receive")
	// txidBucket 保存 "已知 RGB txid" 集合，key=txid，value=receiveId。
	// btcwallet.analyzeTransaction 分类时先查该集合以跳过 BTC 充值路径（BL-5）。
	txidBucket = []byte("rgb20-known-txid")
)

// ReceiveRecord 一次充值请求（receive_id ↔ Chain33 请求）的映射与归因记录。
type ReceiveRecord struct {
	ReceiveID   string `json:"receiveId"`
	RequestID   string `json:"requestId"`   // Chain33 侧请求 ID（HTTP 请求 ID）
	AssetSymbol string `json:"assetSymbol"` // RGB20 资产符号，如 RGB20_USDT
	Chain33Addr string `json:"chain33Addr"` // 用户 Chain33 地址（铸造目标）
	Amount      int64  `json:"amount"`      // 请求金额（资产最小单位）
	Status      string `json:"status"`
	Invoice     string `json:"invoice"`
	// Consignment 用户上传/代理交付的 consignment（settle 后由 http 或 proxy 写入）。
	Consignment []byte `json:"consignment"`
	Txid        string `json:"txid"` // 付款 BTC 交易（结算后）
	Vout        uint32 `json:"vout"` // 打开的收款 seal vout
	Seal        string `json:"seal"` // 打开的收款 seal outpoint "txid:vout"
	CreatedAt   int64  `json:"createdAt"`
	SettledAt   int64  `json:"settledAt"`
}

// SetConsignment 写入 consignment 并触发侧车结算。
func (r *ReceiveStore) SetConsignment(receiveID string, consignment []byte) error {
	rec, err := r.Get(receiveID)
	if err != nil {
		return err
	}
	rec.Consignment = consignment
	return r.Put(rec)
}

// ReceiveStore 管理 receive_id ↔ Chain33 请求映射与归因。
type ReceiveStore struct {
	store KVStore
}

func newReceiveStore(store KVStore) *ReceiveStore {
	return &ReceiveStore{store: store}
}

// Put 保存/更新一条充值记录。
func (r *ReceiveStore) Put(rec *ReceiveRecord) error {
	if rec == nil || rec.ReceiveID == "" {
		return fmt.Errorf("invalid receive record")
	}
	if rec.CreatedAt == 0 {
		rec.CreatedAt = time.Now().Unix()
	}
	return r.store.Put(receiveBucket, []byte(rec.ReceiveID), mustJSON(rec))
}

// Get 按 receive_id 读取充值记录。
func (r *ReceiveStore) Get(receiveID string) (*ReceiveRecord, error) {
	val, err := r.store.Get(receiveBucket, []byte(receiveID))
	if err != nil {
		return nil, err
	}
	rec := &ReceiveRecord{}
	if err := unmarshalJSON(val, rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// UpdateStatus 更新充值记录状态。
func (r *ReceiveStore) UpdateStatus(receiveID, status string) error {
	rec, err := r.Get(receiveID)
	if err != nil {
		return err
	}
	rec.Status = status
	return r.Put(rec)
}

// Settle 结算：写入付款交易与打开的 seal，标记 settled。
func (r *ReceiveStore) Settle(receiveID, txid string, vout uint32, seal string) error {
	rec, err := r.Get(receiveID)
	if err != nil {
		return err
	}
	rec.Txid = txid
	rec.Vout = vout
	rec.Seal = seal
	rec.Status = ReceiveStatusSettled
	rec.SettledAt = time.Now().Unix()
	if err := r.Put(rec); err != nil {
		return err
	}
	return r.putKnownTxid(txid, receiveID)
}

// putKnownTxid 记录已知 RGB txid（侧车状态排除的数据来源）。
func (r *ReceiveStore) putKnownTxid(txid, receiveID string) error {
	return r.store.Put(txidBucket, []byte(txid), []byte(receiveID))
}

// IsKnownRgbTxid 判断 txid 是否为已知 RGB 交易。
func (r *ReceiveStore) IsKnownRgbTxid(txid string) bool {
	if txid == "" {
		return false
	}
	_, err := r.store.Get(txidBucket, []byte(txid))
	return err == nil
}

// ListByStatus 按状态列出充值记录（用于恢复/超时清理）。
func (r *ReceiveStore) ListByStatus(status string) ([]*ReceiveRecord, error) {
	var out []*ReceiveRecord
	err := r.store.ForEach(receiveBucket, func(_, v []byte) error {
		rec := &ReceiveRecord{}
		if err := unmarshalJSON(v, rec); err != nil {
			return nil // 跳过损坏记录
		}
		if status == "" || rec.Status == status {
			out = append(out, rec)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AttrByTxid 按付款 txid 归因充值记录（结算后由转账状态触发）。
func (r *ReceiveStore) AttrByTxid(txid string) (*ReceiveRecord, error) {
	if txid == "" {
		return nil, fmt.Errorf("empty txid")
	}
	val, err := r.store.Get(txidBucket, []byte(txid))
	if err != nil {
		return nil, err
	}
	return r.Get(string(val))
}
