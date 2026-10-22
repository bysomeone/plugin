package neutrino

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- fixtures ----

func proofTestTx(t *testing.T, seed int) *wire.MsgTx {
	t.Helper()
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{byte(seed), 0xaa}, Index: uint32(seed)}, nil, nil))
	tx.AddTxOut(wire.NewTxOut(int64(1000+seed), []byte{txscript.OP_TRUE}))
	return tx
}

func proofTestPending(t *testing.T, tx *wire.MsgTx, blockHash chainhash.Hash, height int32) *btcPendingTx {
	t.Helper()
	return &btcPendingTx{
		tx:          tx,
		txHash:      tx.TxHash(),
		blockHash:   blockHash,
		blockHeight: height,
	}
}

func verboseResult(txs []*wire.MsgTx, blockTime int64, height int64) *btcjson.GetBlockVerboseResult {
	txIDs := make([]string, len(txs))
	for i, tx := range txs {
		txIDs[i] = tx.TxHash().String()
	}
	return &btcjson.GetBlockVerboseResult{Tx: txIDs, Time: blockTime, Height: height}
}

// getBlockOnlyChain embeds chain.Interface so the fake only has to implement GetBlock;
// buildTxExistenceProof's fallback path calls nothing else on it.
type getBlockOnlyChain struct {
	chain.Interface
	block *wire.MsgBlock
}

func (c *getBlockOnlyChain) GetBlock(*chainhash.Hash) (*wire.MsgBlock, error) {
	if c.block == nil {
		return nil, fmt.Errorf("no block")
	}
	return c.block, nil
}

func blockWithTxs(t *testing.T, txs []*wire.MsgTx, timestamp time.Time) *wire.MsgBlock {
	t.Helper()
	header := &wire.BlockHeader{Timestamp: timestamp}
	block := wire.NewMsgBlock(header)
	for _, tx := range txs {
		require.NoError(t, block.AddTransaction(tx))
	}
	return block
}

// ---- block cache ----

// TestBlockCacheFetchesBlockOnceForAllDeposits is the O7 core regression: two deposits
// solved by the same block must issue exactly one getblockverbose call, and both proofs
// must point at the right tx index / block time / height.
func TestBlockCacheFetchesBlockOnceForAllDeposits(t *testing.T) {
	txs := []*wire.MsgTx{proofTestTx(t, 1), proofTestTx(t, 2)}
	blockHash := chainhash.Hash{0x42}
	var fetches int32
	w := &btcWallet{
		blockCache: newBtcBlockCache(defaultBtcBlockCacheSize),
		blockVerboseFn: func(*chainhash.Hash) (*btcjson.GetBlockVerboseResult, error) {
			atomic.AddInt32(&fetches, 1)
			return verboseResult(txs, 1_700_000_000, 800_000), nil
		},
	}

	for i, tx := range txs {
		spv, err := w.buildTxExistenceProof(proofTestPending(t, tx, blockHash, 800_000))
		require.NoError(t, err)
		require.NotNil(t, spv)
		assert.Equal(t, uint32(i), spv.TxIndex, "tx index must match block order")
		assert.Equal(t, int64(1_700_000_000), spv.Time)
		assert.Equal(t, uint64(800_000), spv.Height)
		assert.NotEmpty(t, spv.BranchProof)
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(&fetches), "one block fetch for the whole block")
}

// TestBlockCacheSingleFlight covers the concurrent case: workers picking up several
// deposits from the same block must still trigger only one fetch on a cold cache.
func TestBlockCacheSingleFlight(t *testing.T) {
	const deposits = 8
	txs := make([]*wire.MsgTx, deposits)
	for i := range txs {
		txs[i] = proofTestTx(t, 10+i)
	}
	blockHash := chainhash.Hash{0x43}
	var fetches int32
	w := &btcWallet{
		blockCache: newBtcBlockCache(defaultBtcBlockCacheSize),
		blockVerboseFn: func(*chainhash.Hash) (*btcjson.GetBlockVerboseResult, error) {
			atomic.AddInt32(&fetches, 1)
			time.Sleep(20 * time.Millisecond) // widen the race window
			return verboseResult(txs, 1_700_000_001, 800_001), nil
		},
	}

	var wg sync.WaitGroup
	errs := make(chan error, deposits)
	for i := 0; i < deposits; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			spv, err := w.buildTxExistenceProof(proofTestPending(t, txs[i], blockHash, 800_001))
			if err != nil {
				errs <- err
				return
			}
			if spv.TxIndex != uint32(i) {
				errs <- fmt.Errorf("tx %d: got index %d", i, spv.TxIndex)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(&fetches), "concurrent cold misses must share one fetch")
}

// TestBlockCacheBoundedLRU verifies the cache keeps at most max blocks and re-fetches an
// evicted block instead of growing without bound.
func TestBlockCacheBoundedLRU(t *testing.T) {
	cache := newBtcBlockCache(2)
	var mu sync.Mutex
	loads := map[chainhash.Hash]int{}
	load := func(hash chainhash.Hash) (*btcBlockMeta, error) {
		mu.Lock()
		loads[hash]++
		mu.Unlock()
		return &btcBlockMeta{txIDs: []string{hash.String()}, time: 1, height: 2}, nil
	}

	hashes := []chainhash.Hash{{1}, {2}, {3}}
	for _, hash := range hashes {
		_, err := cache.getOrLoad(hash, load)
		require.NoError(t, err)
	}
	// {1} fell out when {3} was inserted.
	_, err := cache.getOrLoad(hashes[0], load)
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 2, loads[hashes[0]], "evicted entry must be loaded again")
	assert.Equal(t, 1, loads[hashes[1]])
	assert.Equal(t, 1, loads[hashes[2]])
}

// TestBlockCacheFailureFallsBackToNeutrino proves a failing getblockverbose never makes
// the proof fail while the neutrino fallback can serve it, and that the failure is not
// cached (the next call hits the RPC again).
func TestBlockCacheFailureFallsBackToNeutrino(t *testing.T) {
	tx := proofTestTx(t, 20)
	blockHash := chainhash.Hash{0x44}
	fallback := blockWithTxs(t, []*wire.MsgTx{tx}, time.Unix(1_700_000_002, 0))
	var fetches int32
	fail := int32(1)
	w := &btcWallet{
		blockCache:  newBtcBlockCache(defaultBtcBlockCacheSize),
		chainClient: &getBlockOnlyChain{block: fallback},
		blockVerboseFn: func(*chainhash.Hash) (*btcjson.GetBlockVerboseResult, error) {
			atomic.AddInt32(&fetches, 1)
			if atomic.LoadInt32(&fail) == 1 {
				return nil, fmt.Errorf("btcd unavailable")
			}
			return verboseResult([]*wire.MsgTx{tx}, 1_700_000_003, 800_003), nil
		},
	}

	spv, err := w.buildTxExistenceProof(proofTestPending(t, tx, blockHash, 800_003))
	require.NoError(t, err, "cache/RPC failure must not fail the proof")
	require.NotNil(t, spv)
	assert.Equal(t, uint64(800_003), spv.Height, "fallback uses the pending block height")
	assert.Equal(t, uint32(0), spv.TxIndex)

	// The error must not be cached: the next call retries getblockverbose.
	atomic.StoreInt32(&fail, 0)
	spv, err = w.buildTxExistenceProof(proofTestPending(t, tx, blockHash, 800_003))
	require.NoError(t, err)
	assert.Equal(t, int64(1_700_000_003), spv.Time, "successful retry uses the RPC metadata")
	assert.Equal(t, int32(2), atomic.LoadInt32(&fetches))
}

// TestBlockCacheEmptyResultIsNeverUsed covers the "never return an empty proof" requirement:
// an empty block payload is treated as a load failure and falls back.
func TestBlockCacheEmptyResultIsNeverUsed(t *testing.T) {
	tx := proofTestTx(t, 21)
	blockHash := chainhash.Hash{0x45}
	fallback := blockWithTxs(t, []*wire.MsgTx{tx}, time.Unix(1_700_000_004, 0))
	var fetches int32
	w := &btcWallet{
		blockCache:  newBtcBlockCache(defaultBtcBlockCacheSize),
		chainClient: &getBlockOnlyChain{block: fallback},
		blockVerboseFn: func(*chainhash.Hash) (*btcjson.GetBlockVerboseResult, error) {
			atomic.AddInt32(&fetches, 1)
			return &btcjson.GetBlockVerboseResult{}, nil
		},
	}

	spv, err := w.buildTxExistenceProof(proofTestPending(t, tx, blockHash, 800_004))
	require.NoError(t, err)
	require.NotNil(t, spv)
	assert.Equal(t, uint32(0), spv.TxIndex)
	assert.Equal(t, int32(1), atomic.LoadInt32(&fetches))
}
