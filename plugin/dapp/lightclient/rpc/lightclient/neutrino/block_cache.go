package neutrino

import (
	"container/list"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// defaultBtcBlockCacheSize bounds the block metadata cache. A bridge typically seeds
// deposits from a handful of recent blocks, so 64 blocks keeps the working set while
// capping memory (one entry is just the block's txid strings + time + height).
const defaultBtcBlockCacheSize = 64

// btcBlockVerboseFetcher is the subset of rpcclient.Client.GetBlockVerbose that the
// proof path needs. Injectable so tests can count block fetches without a btcd node.
type btcBlockVerboseFetcher func(*chainhash.Hash) (*btcjson.GetBlockVerboseResult, error)

// btcBlockMeta is everything buildTxExistenceProof needs from a block. It is immutable
// once stored; the txid slice is shared with the cache and must not be mutated.
type btcBlockMeta struct {
	txIDs  []string
	time   int64
	height int64
}

// btcBlockCache is a bounded LRU cache keyed by block hash. A block's contents are
// immutable for a given hash, so a reorg cannot make an entry stale: the replacement
// block has a different hash. That is why there is no invalidation path.
type btcBlockCache struct {
	mu    sync.Mutex
	max   int
	lru   *list.List
	items map[chainhash.Hash]*list.Element
	loads map[chainhash.Hash]*btcBlockLoad
}

type btcBlockCacheEntry struct {
	hash chainhash.Hash
	meta *btcBlockMeta
}

// btcBlockLoad deduplicates concurrent loads of the same block hash (single-flight).
type btcBlockLoad struct {
	done chan struct{}
	meta *btcBlockMeta
	err  error
}

func newBtcBlockCache(max int) *btcBlockCache {
	return &btcBlockCache{
		max:   max,
		lru:   list.New(),
		items: make(map[chainhash.Hash]*list.Element),
		loads: make(map[chainhash.Hash]*btcBlockLoad),
	}
}

// getOrLoad returns the cached metadata for hash, otherwise calls load exactly once even
// when several callers race on a cold entry. Load errors are returned to every waiter and
// are never cached, so a transient failure cannot poison later proofs.
func (c *btcBlockCache) getOrLoad(hash chainhash.Hash, load func(chainhash.Hash) (*btcBlockMeta, error)) (*btcBlockMeta, error) {
	if c == nil || c.max <= 0 {
		return load(hash)
	}

	c.mu.Lock()
	if el, ok := c.items[hash]; ok {
		c.lru.MoveToFront(el)
		meta := el.Value.(*btcBlockCacheEntry).meta
		c.mu.Unlock()
		return meta, nil
	}
	if pending, ok := c.loads[hash]; ok {
		c.mu.Unlock()
		<-pending.done
		return pending.meta, pending.err
	}
	pending := &btcBlockLoad{done: make(chan struct{})}
	c.loads[hash] = pending
	c.mu.Unlock()

	meta, err := load(hash)
	// An empty result is never a valid block and must not be cached: callers would then
	// fail to find any tx in it and return an error instead of falling back.
	if err == nil && (meta == nil || len(meta.txIDs) == 0) {
		meta, err = nil, fmt.Errorf("empty block metadata")
	}

	c.mu.Lock()
	delete(c.loads, hash)
	if err == nil {
		c.putLocked(hash, meta)
	}
	pending.meta, pending.err = meta, err
	c.mu.Unlock()
	close(pending.done)

	return meta, err
}

func (c *btcBlockCache) putLocked(hash chainhash.Hash, meta *btcBlockMeta) {
	if el, ok := c.items[hash]; ok {
		el.Value.(*btcBlockCacheEntry).meta = meta
		c.lru.MoveToFront(el)
		return
	}
	el := c.lru.PushFront(&btcBlockCacheEntry{hash: hash, meta: meta})
	c.items[hash] = el
	for c.lru.Len() > c.max {
		back := c.lru.Back()
		if back == nil {
			return
		}
		c.lru.Remove(back)
		delete(c.items, back.Value.(*btcBlockCacheEntry).hash)
	}
}

// blockVerboseEndpoint returns the getblockverbose implementation to use, if any.
// The injected fetcher takes precedence so tests never need a live btcd.
func (b *btcWallet) blockVerboseEndpoint() btcBlockVerboseFetcher {
	if b.blockVerboseFn != nil {
		return b.blockVerboseFn
	}
	if b.rpcClient != nil {
		return b.rpcClient.GetBlockVerbose
	}
	return nil
}
