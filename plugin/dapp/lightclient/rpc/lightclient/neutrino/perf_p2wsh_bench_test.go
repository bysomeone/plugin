package neutrino

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/33cn/chain33/common"
	"github.com/33cn/chain33/common/address"
	"github.com/decred/base58"

	pb "github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20/pb"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/gcs"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/walletdb"
	_ "github.com/btcsuite/btcwallet/walletdb/bdb"
	"github.com/lightninglabs/neutrino"
)

/*
 * P2WSH 多地址并发的**局部基准**（性能评估用，非产品代码）。
 *
 * 只测"watch 集变大以后哪些代价按什么曲线涨"，不跑 E2E harness。四组：
 *
 *   1. watch 集本身的增删查（depositScriptSet）—— 本仓自己的 map，应当 O(1)；
 *   2. 新增一个用户地址的真实代价（ensureUserDepositScript 全路径 + 批量 import）；
 *   3. neutrino（上游）每块/每命中块的代价 —— 逐字复刻两处上游循环，见各自的 NOTE；
 *   4. GCS 布隆过滤器 MatchAny 随 watch 集规模的真实成本（用真库真过滤器）。
 *
 * 3 里的两段是**复刻**（上游函数未导出，见路径注释），复刻的只有循环结构，
 * 被调用的 `txscript.PayToAddrScript` / `bytes.Equal` 都是真库真函数。
 */

// ---- 1. watch 集（本仓） ----

// BenchmarkDepositScriptSetLookup watch 集反解归属：map（现状）vs 线性扫（对照）。
func BenchmarkDepositScriptSetLookup(b *testing.B) {
	for _, n := range []int{100, 1000, 10000, 100000} {
		set := newDepositScriptSet()
		for i := 0; i < n; i++ {
			set.add(fmt.Sprintf("user-%d", i), randPkScript(i))
		}
		target := randPkScript(n / 2)
		entries := set.entries()

		b.Run(fmt.Sprintf("map/N=%d", n), func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, ok := set.lookupUser(target); !ok {
					b.Fatal("miss")
				}
			}
		})
		b.Run(fmt.Sprintf("linearScan/N=%d", n), func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				found := false
				for _, e := range entries {
					if bytes.Equal(e.pkScript, target) {
						found = true
						break
					}
				}
				if !found {
					b.Fatal("miss")
				}
			}
		})
	}
}

// ---- 2. 新增用户地址 ----

func benchNewWallet(tb testing.TB, pub *btcec.PublicKey, cfg config) (*btcWallet, walletdb.DB) {
	tb.Helper()
	params := chaincfg.RegressionNetParams
	dir := tb.TempDir()
	_, walletDB, err := openWalletDB(dir, "btcwallet.db")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = walletDB.Close() })
	pubPass := []byte("hello")
	if err := wallet.CreateWatchingOnly(walletDB, pubPass, &params, time.Now()); err != nil {
		tb.Fatal(err)
	}
	w, err := wallet.Open(walletDB, pubPass, nil, &params, 0)
	if err != nil {
		tb.Fatal(err)
	}
	_, neutrinoDB, err := openWalletDB(dir, "neutrino.db")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = neutrinoDB.Close() })
	b := &btcWallet{
		Wallet:      w,
		db:          walletDB,
		chainParams: params,
		notifyFn:    func([]btcutil.Address) error { return nil },
		client: &neutrinoClient{
			tss:         &tssService{tssPublicKey: pub},
			cfg:         cfg,
			deposits:    newDepositScriptSet(),
			neutrinoCfg: neutrino.Config{Database: neutrinoDB},
		},
	}
	return b, walletDB
}

// BenchmarkImportDepositWitnessScripts 批量导入 N 个脚本的代价（waddrmgr 写库 + 加密 + 订阅）。
// 关键问题：**批量 import 是不是每脚本成本相同**（有没有 O(N²) 的隐藏代价）。
func BenchmarkImportDepositWitnessScripts(b *testing.B) {
	priv, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x11}, 32))
	pub := priv.PubKey()
	for _, n := range []int{1, 10, 100, 1000} {
		b.Run(fmt.Sprintf("batch=%d", n), func(b *testing.B) {
			w, _ := benchNewWallet(b, pub, config{})
			scripts := make([][]byte, 0, n)
			for i := 0; i < n; i++ {
				ws, err := rtypes.DeriveDepositWitnessScript(chain33Addr(i), pub.SerializeCompressed())
				if err != nil {
					b.Fatal(err)
				}
				scripts = append(scripts, ws)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				// 每轮用全新的脚本集合（waddrmgr 幂等，重复导入走 duplicate 分支测不到真写入）
				fresh := make([][]byte, 0, n)
				for j := 0; j < n; j++ {
					ws, err := rtypes.DeriveDepositWitnessScript(chain33Addr(i*100000+j), pub.SerializeCompressed())
					if err != nil {
						b.Fatal(err)
					}
					fresh = append(fresh, ws)
				}
				b.StartTimer()
				if _, err := w.importDepositWitnessScripts(fresh); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkEnsureUserDepositScript 发放一个新地址的全路径（派生 + import + 落盘 + 下发）。
// 这是"用户点一次要地址"的实际代价 —— 桥的地址发放 QPS 上限就是它的倒数。
func BenchmarkEnsureUserDepositScript(b *testing.B) {
	priv, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x11}, 32))
	pub := priv.PubKey()
	w, _ := benchNewWallet(b, pub, config{})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := w.ensureUserDepositScript(chain33Addr(i))
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDeriveDepositPkScript 派生本身（纯函数，链下调用一次）—— 用来证明"派生不是瓶颈"。
func BenchmarkDeriveDepositPkScript(b *testing.B) {
	priv, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x11}, 32))
	pub := priv.PubKey().SerializeCompressed()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := rtypes.DeriveDepositPkScript("12CiL9Q4gmir5LwZ8bEXeNdVGvFCxMzHdR", pub); err != nil {
			b.Fatal(err)
		}
	}
}

// ---- 3. neutrino 上游循环的复刻 ----

// BenchmarkNeutrinoPaysWatchedAddr 复刻 `lightninglabs/neutrino@v0.16.0/rescan.go:1303`
// 的 `rescanOptions.paysWatchedAddr` 的内层循环：**对每个输出 × 每个 watch 地址**
// 调一次 `txscript.PayToAddrScript` 再 `bytes.Equal`，且命中后**不提前退出**
// （上游只在 `anyMatchingOutputs` 置位后继续跑完整个内层循环）。
//
// 这段只在"块过滤器命中"的块上跑（extractBlockMatches）。txOuts=2000 ≈ 一个满块的输出数。
func BenchmarkNeutrinoPaysWatchedAddr(b *testing.B) {
	const txOuts = 2000
	outs := make([][]byte, txOuts)
	for i := range outs {
		outs[i] = randPkScript(i)
	}
	for _, m := range []int{100, 1000, 10000, 100000} {
		addrs := make([]btcutil.Address, 0, m)
		for i := 0; i < m; i++ {
			addr, err := btcutil.NewAddressWitnessScriptHash(randPkScript(1_000_000 + i)[2:], &chaincfg.RegressionNetParams)
			if err != nil {
				b.Fatal(err)
			}
			addrs = append(addrs, addr)
		}
		b.Run(fmt.Sprintf("watchAddrs=%d", m), func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				anyMatching := false
				for _, pkScript := range outs {
					for _, addr := range addrs {
						addrScript, err := txscript.PayToAddrScript(addr)
						if err != nil {
							b.Fatal(err)
						}
						if !bytes.Equal(pkScript, addrScript) {
							continue
						}
						anyMatching = true
					}
				}
				_ = anyMatching
			}
		})
	}
}

// BenchmarkNeutrinoPaysWatchedAddrMap 同一件事用"program → userID"哈希表做（侧车/桥侧
// 已经有的结构，本仓 depositScriptSet 就是这个形状）：O(输出数) 而不是 O(输出数 × watch 集)。
func BenchmarkNeutrinoPaysWatchedAddrMap(b *testing.B) {
	const txOuts = 2000
	outs := make([][]byte, txOuts)
	for i := range outs {
		outs[i] = randPkScript(i)
	}
	for _, m := range []int{100, 1000, 10000, 100000} {
		table := make(map[string]struct{}, m)
		for i := 0; i < m; i++ {
			table[string(randPkScript(1_000_000+i))] = struct{}{}
		}
		b.Run(fmt.Sprintf("watchSet=%d", m), func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for _, pkScript := range outs {
					if _, ok := table[string(pkScript)]; ok {
						b.Fatal("unexpected hit")
					}
				}
			}
		})
	}
}

// BenchmarkNeutrinoSpendsWatchedInput 复刻 `neutrino@v0.16.0/rescan.go:1273` 的
// `spendsWatchedInput`：O(输入数 × watchInputs)。watchInputs 每命中一个输出就追加一条
// （`paysWatchedAddr` 里 `ro.watchInputs = append(...)`），**进程内单调增长**，
// 所以这条曲线随运行时间变差，不只是随用户数。
func BenchmarkNeutrinoSpendsWatchedInput(b *testing.B) {
	const txIns = 10
	ins := make([]wire.OutPoint, txIns)
	for i := range ins {
		ins[i] = wire.OutPoint{Hash: chainhash.Hash(sha256.Sum256([]byte{byte(i)})), Index: uint32(i)}
	}
	for _, m := range []int{100, 1000, 10000, 100000} {
		watched := make([]wire.OutPoint, m)
		for i := range watched {
			watched[i] = wire.OutPoint{Hash: chainhash.Hash(sha256.Sum256([]byte(fmt.Sprintf("w%d", i)))), Index: uint32(i)}
		}
		b.Run(fmt.Sprintf("watchInputs=%d", m), func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				found := false
				for _, in := range ins {
					for _, w := range watched {
						if in == w {
							found = true
							break
						}
					}
				}
				_ = found
			}
		})
	}
}

// ---- 4. GCS 过滤器匹配（真库） ----

// BenchmarkGCSFilterMatchAny 真 `gcs.Filter.MatchAny`：每块都要跑一次（neutrino 对每个
// 新块取 filter 再 MatchAny）。N = 块过滤器条目数（≈ 该块的输出/输入数），M = watch 集。
func BenchmarkGCSFilterMatchAny(b *testing.B) {
	keyHash := sha256.Sum256([]byte("bench-block"))
	key := [gcs.KeySize]byte{}
	copy(key[:], keyHash[:gcs.KeySize])
	for _, filterN := range []uint64{1000, 3000, 10000} {
		data := make([][]byte, filterN)
		for i := range data {
			h := sha256.Sum256([]byte(fmt.Sprintf("item-%d", i)))
			data[i] = h[:]
		}
		f, err := gcs.BuildGCSFilter(19, 784931, key, data)
		if err != nil {
			b.Fatal(err)
		}
		for _, m := range []int{100, 1000, 10000, 100000} {
			queries := make([][]byte, m)
			for i := range queries {
				h := sha256.Sum256([]byte(fmt.Sprintf("q-%d", i)))
				queries[i] = h[:]
			}
			b.Run(fmt.Sprintf("filterN=%d/watch=%d", filterN, m), func(b *testing.B) {
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := f.MatchAny(key, queries); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// ---- helpers ----

// chain33Addr 造一个**合法**的 chain33 base58 地址（与 btc address driver 的编码逐字节同构：
// version || ripemd160(pub) || sha256d 前 4 字节），因为 mergeRemoteDepositScripts 会走
// address.CheckAddress 校验，随便造的串会被跳过、测不到真实成本。
func chain33Addr(seed int) string {
	raw := common.Rimp160([]byte(fmt.Sprintf("perf-seed-%d", seed)))
	ad := make([]byte, 25)
	ad[0] = address.NormalVer
	copy(ad[1:21], raw)
	copy(ad[21:25], common.Sha2Sum(ad[0:21])[:4])
	return base58.Encode(ad)
}

// randPkScript 造一个形态正确的 P2WSH pkScript（OP_0 <32B program>）。
func randPkScript(seed int) []byte {
	h := sha256.Sum256([]byte(fmt.Sprintf("script-%d", seed)))
	out := make([]byte, 0, 34)
	out = append(out, txscript.OP_0, 32)
	return append(out, h[:]...)
}

var _ = hex.EncodeToString
var _ = rand.Reader

// BenchmarkPayToAddrScript 上游 paysWatchedAddr 内层循环的单价（每个 (输出, watch 地址) 一次）。
func BenchmarkPayToAddrScript(b *testing.B) {
	addr, err := btcutil.NewAddressWitnessScriptHash(randPkScript(7)[2:], &chaincfg.RegressionNetParams)
	if err != nil {
		b.Fatal(err)
	}
	target := randPkScript(7)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s, err := txscript.PayToAddrScript(addr)
		if err != nil {
			b.Fatal(err)
		}
		_ = bytes.Equal(target, s)
	}
}

// BenchmarkSyncDepositScriptsRound 每个节点每 30 秒跑一轮的 watch 集下发/合并（
// depositScriptSyncWorker → syncDepositScriptsWithSidecar → mergeRemoteDepositScripts）。
// 这里量的是**合并侧的派生与去重成本**（不含网络）：每条远程条目都要按本节点的 tssPub
// 重新派生一遍 program + witnessScript 再比对。
func BenchmarkSyncDepositScriptsRound(b *testing.B) {
	priv, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x11}, 32))
	pub := priv.PubKey()
	for _, n := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("entries=%d", n), func(b *testing.B) {
			w, _ := benchNewWallet(b, pub, config{})
			w.client.neutrinoCfg.Database = nil // 不落盘，只量内存合并成本
			entries := make([]*pb.DepositScriptEntry, 0, n)
			for i := 0; i < n; i++ {
				userID := chain33Addr(i)
				pk, err := rtypes.DeriveDepositPkScript(userID, pub.SerializeCompressed())
				if err != nil {
					b.Fatal(err)
				}
				entries = append(entries, &pb.DepositScriptEntry{UserId: userID, PkScript: pk})
			}
			// 冷启动：watch 集为空，每条都要派生 + 落库（首次下发）。
			b.Run("cold", func(b *testing.B) {
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					b.StopTimer()
					w.client.deposits = newDepositScriptSet()
					b.StartTimer()
					w.client.mergeRemoteDepositScripts(entries)
					if w.client.deposits.size() != n {
						b.Fatalf("merged %d/%d", w.client.deposits.size(), n)
					}
				}
			})
			// 稳态：watch 集已含全部条目（每 30 秒一轮的实际形态）—— 只有去重判断。
			w.client.deposits = newDepositScriptSet()
			for i := 0; i < n; i++ {
				w.client.deposits.add(chain33Addr(i), entries[i].PkScript)
			}
			b.Run("steady", func(b *testing.B) {
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					w.client.mergeRemoteDepositScripts(entries)
				}
			})
			// 下发侧：entries() 快照 + 组装请求（每条要 hex 解码一次 program）。
			b.Run("push", func(b *testing.B) {
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					local := w.client.deposits.entries()
					req := &pb.RegisterDepositScriptsRequest{Scripts: make([]*pb.DepositScriptEntry, 0, len(local))}
					for _, e := range local {
						req.Scripts = append(req.Scripts, &pb.DepositScriptEntry{UserId: e.userID, PkScript: e.pkScript})
					}
					if len(req.Scripts) != n {
						b.Fatalf("pushed %d/%d", len(req.Scripts), n)
					}
				}
			})
		})
	}
}
