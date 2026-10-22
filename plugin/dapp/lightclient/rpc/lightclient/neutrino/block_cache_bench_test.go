package neutrino

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

const (
	// benchDepositsPerBlock is a synthetic batch large enough to show the
	// per-block vs per-deposit cost split.
	benchDepositsPerBlock = 32
	// benchFetchLatency models the btcd RPC round trip measured in PERF_P2WSH.md
	// (§5.3: 3.4-6.7 ms on macOS + Docker Desktop). It is a stand-in, not a live RPC.
	benchFetchLatency = 5 * time.Millisecond
)

func benchProofBatch(tb testing.TB) ([]*wire.MsgTx, *btcjson.GetBlockVerboseResult) {
	tb.Helper()
	txs := make([]*wire.MsgTx, benchDepositsPerBlock)
	txIDs := make([]string, benchDepositsPerBlock)
	for i := 0; i < benchDepositsPerBlock; i++ {
		tx := wire.NewMsgTx(wire.TxVersion)
		tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{byte(i), 0x11}, Index: 0}, nil, nil))
		tx.AddTxOut(wire.NewTxOut(int64(1_000+i), []byte{txscript.OP_TRUE}))
		txs[i] = tx
		txIDs[i] = tx.TxHash().String()
	}
	return txs, &btcjson.GetBlockVerboseResult{Tx: txIDs, Time: 1_700_000_000, Height: 800_000}
}

// BenchmarkDepositProofPerBlock compares the pre-O7 path (no cache: every deposit in a
// block downloads the whole block) with the cached path (one download per block).
// Each iteration is one block carrying benchDepositsPerBlock deposits, so
//
//	ns/op   = cost of the whole block
//	ns/tx   = cost per deposit (1 / (deposits per second) for a single-threaded watcher)
//
// The fetch is a 5 ms stub (see benchFetchLatency); the proof assembly is real.
func BenchmarkDepositProofPerBlock(b *testing.B) {
	txs, verbose := benchProofBatch(b)

	for _, tc := range []struct {
		name      string
		cacheSize int
	}{
		{"before_no_cache", 0},
		{"after_lru64", 64},
	} {
		b.Run(tc.name, func(b *testing.B) {
			var fetches int64
			w := &btcWallet{
				blockCache: newBtcBlockCache(tc.cacheSize),
				blockVerboseFn: func(*chainhash.Hash) (*btcjson.GetBlockVerboseResult, error) {
					atomic.AddInt64(&fetches, 1)
					time.Sleep(benchFetchLatency)
					return verbose, nil
				},
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				blockHash := chainhash.Hash{byte(i), byte(i >> 8), byte(i >> 16), 0x5a}
				for j, tx := range txs {
					pending := &btcPendingTx{
						tx:          tx,
						txHash:      tx.TxHash(),
						blockHash:   blockHash,
						blockHeight: 800_000 + int32(j),
					}
					if _, err := w.buildTxExistenceProof(pending); err != nil {
						b.Fatal(err)
					}
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(fetches)/float64(b.N), "fetches/block")
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*benchDepositsPerBlock), "ns/tx")
		})
	}
}

// BenchmarkDepositProofCacheHit measures what a deposit costs once the block is cached:
// the per-tx parse + merkle branch, no RPC. This is the floor the block cache gives a
// serial watcher for all but the first deposit of each block.
func BenchmarkDepositProofCacheHit(b *testing.B) {
	txs, verbose := benchProofBatch(b)
	blockHash := chainhash.Hash{0x5b}
	w := &btcWallet{
		blockCache: newBtcBlockCache(64),
		blockVerboseFn: func(*chainhash.Hash) (*btcjson.GetBlockVerboseResult, error) {
			return verbose, nil
		},
	}
	if _, err := w.buildTxExistenceProof(&btcPendingTx{
		tx: txs[0], txHash: txs[0].TxHash(), blockHash: blockHash, blockHeight: 800_000,
	}); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tx := txs[i%len(txs)]
		pending := &btcPendingTx{
			tx: tx, txHash: tx.TxHash(), blockHash: blockHash, blockHeight: 800_000,
		}
		if _, err := w.buildTxExistenceProof(pending); err != nil {
			b.Fatal(err)
		}
	}
}
