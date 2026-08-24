package rgb20

import (
	"sync"

	"github.com/btcsuite/btcwallet/walletdb"
)

// KVStore 是按 bucket 分组的键值存储抽象。
// 生产环境由 walletdb(bbolt) 提供（与 neutrino 主包共用 btcwallet 的 bdb），
// 单测使用内存实现（memStore）。
type KVStore interface {
	Get(bucket, key []byte) ([]byte, error)
	Put(bucket, key, value []byte) error
	Delete(bucket, key []byte) error
	ForEach(bucket []byte, fn func(k, v []byte) error) error
}

// NewWalletStore 以 walletdb(bbolt) 构造 KVStore（neutrino 主包注入使用）。
func NewWalletStore(db walletdb.DB) KVStore {
	return newWalletStore(db)
}

// walletStore 基于 btcwallet 的 walletdb(bbolt) 实现 KVStore。
type walletStore struct {
	db walletdb.DB
}

func newWalletStore(db walletdb.DB) *walletStore {
	return &walletStore{db: db}
}

func (w *walletStore) Get(bucket, key []byte) ([]byte, error) {
	var val []byte
	err := w.db.View(func(tx walletdb.ReadTx) error {
		b := tx.ReadBucket(bucket)
		if b == nil {
			return walletdb.ErrBucketNotFound
		}
		val = b.Get(key)
		return nil
	}, func() {})
	if err != nil {
		return nil, err
	}
	if val == nil {
		return nil, walletdb.ErrBucketNotFound
	}
	// walletdb 返回的值仅在事务内有效，需拷贝
	out := make([]byte, len(val))
	copy(out, val)
	return out, nil
}

func (w *walletStore) Put(bucket, key, value []byte) error {
	return w.db.Update(func(tx walletdb.ReadWriteTx) error {
		b, err := tx.CreateTopLevelBucket(bucket)
		if err != nil {
			return err
		}
		return b.Put(key, value)
	}, func() {})
}

func (w *walletStore) Delete(bucket, key []byte) error {
	return w.db.Update(func(tx walletdb.ReadWriteTx) error {
		b := tx.ReadWriteBucket(bucket)
		if b == nil {
			return nil
		}
		return b.Delete(key)
	}, func() {})
}

func (w *walletStore) ForEach(bucket []byte, fn func(k, v []byte) error) error {
	return w.db.View(func(tx walletdb.ReadTx) error {
		b := tx.ReadBucket(bucket)
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			if v == nil {
				// 嵌套 bucket，跳过
				return nil
			}
			kc := make([]byte, len(k))
			copy(kc, k)
			vc := make([]byte, len(v))
			copy(vc, v)
			return fn(kc, vc)
		})
	}, func() {})
}

// NewMemStore 构造内存 KVStore（单测 / 无盘部署使用）。
func NewMemStore() KVStore {
	return newMemStore()
}

// memStore 内存 KVStore，供单元测试使用。
type memStore struct {
	mu   sync.RWMutex
	data map[string]map[string][]byte
}

func newMemStore() *memStore {
	return &memStore{data: make(map[string]map[string][]byte)}
}

func (m *memStore) Get(bucket, key []byte) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.data[string(bucket)]
	if !ok {
		return nil, walletdb.ErrBucketNotFound
	}
	val, ok := b[string(key)]
	if !ok {
		return nil, walletdb.ErrBucketNotFound
	}
	out := make([]byte, len(val))
	copy(out, val)
	return out, nil
}

func (m *memStore) Put(bucket, key, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.data[string(bucket)]
	if !ok {
		b = make(map[string][]byte)
		m.data[string(bucket)] = b
	}
	v := make([]byte, len(value))
	copy(v, value)
	b[string(key)] = v
	return nil
}

func (m *memStore) Delete(bucket, key []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.data[string(bucket)]; ok {
		delete(b, string(key))
	}
	return nil
}

func (m *memStore) ForEach(bucket []byte, fn func(k, v []byte) error) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.data[string(bucket)]
	if !ok {
		return nil
	}
	for k, v := range b {
		kc := []byte(k)
		vc := make([]byte, len(v))
		copy(vc, v)
		if err := fn(kc, vc); err != nil {
			return err
		}
	}
	return nil
}
