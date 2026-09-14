package rgb20

import (
	"bytes"
	"errors"
	"sync"
	"testing"

	pb "github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20/pb"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

// testDepositTxData 构造一笔最小但**规范编码**的 BTC 付款交易（无 witness、reader 恰好消费完）。
// nonce 用来区分不同的付款交易（不同 txid）。签名侧去重按它的 txid 判定。
func testDepositTxData(t *testing.T, nonce uint32) []byte {
	t.Helper()
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{byte(nonce)}, Index: nonce}, nil, nil))
	tx.AddTxOut(wire.NewTxOut(int64(1000+int(nonce)), []byte{0x51}))
	var buf bytes.Buffer
	require.NoError(t, tx.SerializeNoWitness(&buf))
	return buf.Bytes()
}

// testDepositPayload 构造一条 rgb20-deposit 签名消息（付款交易 + SPV 高度）。
func testDepositPayload(t *testing.T, nonce uint32, height uint64) *DepositSignPayload {
	t.Helper()
	return &DepositSignPayload{
		Deposit: &rtypes.DepositAsset{
			Amount:         1000,
			DepositAddress: "addr",
			AssetSymbol:    "RGB20_USDT",
			TxProof:        &rtypes.BtcTxProof{TxData: testDepositTxData(t, nonce), BlockHeight: height},
		},
		Consignment:    []byte("consignment"),
		ReceiveID:      "recv-1",
		Chain33Addr:    "addr",
		BtcBlockHeight: height,
	}
}

// newSignedTestAdapter 构造签名侧去重测试用的适配器（不连侧车：去重路径不碰侧车）。
// ttl 为已签集合保留期（BTC 块数）；store 可复用（模拟重启）。
func newSignedTestAdapter(t *testing.T, ttl int64, store KVStore, bridge Chain33Bridge) *Adapter {
	t.Helper()
	adapter, err := NewAdapter(Config{
		SidecarAddr:      "unused-dedup-test",
		Precision:        6,
		SignedDepositTTL: ttl,
		Contracts:        []Contract{{Symbol: "RGB20_USDT", Precision: 6}},
	}, store)
	require.NoError(t, err)
	adapter.SetBridge(bridge)
	return adapter
}

// countingStore 统计写次数（幂等用例：重复标记不得重复写库）。
type countingStore struct {
	KVStore
	mu   sync.Mutex
	puts int
}

func (c *countingStore) Put(bucket, key, value []byte) error {
	c.mu.Lock()
	c.puts++
	c.mu.Unlock()
	return c.KVStore.Put(bucket, key, value)
}

func (c *countingStore) putCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.puts
}

// Test_SignedDepositSet_MarkOnlyAfterSign 未签过放行；**签名后**才进集合；已签过即拒。
func Test_SignedDepositSet_MarkOnlyAfterSign(t *testing.T) {
	bridge := &fakeBridge{}
	store := NewMemStore()
	adapter := newSignedTestAdapter(t, 0, store, bridge)
	payload := testDepositPayload(t, 1, 100)
	txid, err := depositTxid(payload)
	require.NoError(t, err)

	// 1. 未签过 → 放行，且集合还是空的（"签名前不登记"：签名失败不烧 txid）
	require.NoError(t, adapter.CheckDepositSigned(payload))
	require.False(t, adapter.signed.Has(txid))
	require.Zero(t, adapter.signed.Len())

	// 2. 签名成功后登记 → 进集合
	require.NoError(t, adapter.MarkDepositSigned(payload))
	require.True(t, adapter.signed.Has(txid))
	require.Equal(t, uint64(100), mustEntry(t, adapter.signed, txid).Height)

	// 3. 已签过 → 拒绝签名（明确错误）
	err = adapter.CheckDepositSigned(payload)
	require.Error(t, err)
	require.Contains(t, err.Error(), "already signed")
}

// Test_SignedDepositSet_ZeroOrMinusOneNeverPrunes TTL=0/-1 = 只增不删：跑再久也不删、也不查链上高度。
func Test_SignedDepositSet_ZeroOrMinusOneNeverPrunes(t *testing.T) {
	for _, ttl := range []int64{0, -1} {
		bridge := &fakeBridge{btcTip: 10_000_000}
		adapter := newSignedTestAdapter(t, ttl, NewMemStore(), bridge)
		payload := testDepositPayload(t, 2, 100)
		require.NoError(t, adapter.MarkDepositSigned(payload))

		// 链上高度已经远远超过记录高度，但 TTL<=0 时永不过期
		require.NoError(t, adapter.PruneSignedDeposits())
		require.Equal(t, 1, adapter.signed.Len())
		require.Error(t, adapter.CheckDepositSigned(payload), "ttl=%d 时不得过期", ttl)

		// 只增不删路径不该有任何链上查询（默认配置下本机制零查询）
		require.Zero(t, bridge.tipCallCount(), "ttl=%d 时不应查询链上高度", ttl)
	}
}

// Test_SignedDepositSet_TTLExpiresAndAllowsResign TTL 正数：过期的清掉可再签，未过期的保留。
func Test_SignedDepositSet_TTLExpiresAndAllowsResign(t *testing.T) {
	bridge := &fakeBridge{btcTip: 104}
	adapter := newSignedTestAdapter(t, 5, NewMemStore(), bridge)
	oldTx := testDepositPayload(t, 3, 100)   // 100 + 5 = 105
	freshTx := testDepositPayload(t, 4, 103) // 103 + 5 = 108
	require.NoError(t, adapter.MarkDepositSigned(oldTx))
	require.NoError(t, adapter.MarkDepositSigned(freshTx))

	// tip=104：两条都还在保留期内 → 都拒
	require.Error(t, adapter.CheckDepositSigned(oldTx))
	require.Error(t, adapter.CheckDepositSigned(freshTx))
	require.Equal(t, 2, adapter.signed.Len())

	// tip=105 批量清理：过期的删、未过期的留
	bridge.setTip(105)
	require.NoError(t, adapter.PruneSignedDeposits())
	require.Equal(t, 1, adapter.signed.Len(), "只清过期的那条")
	require.False(t, adapter.signed.Has(mustTxid(t, oldTx)))
	require.True(t, adapter.signed.Has(mustTxid(t, freshTx)))
	require.Error(t, adapter.CheckDepositSigned(freshTx), "未过期的仍拒")

	// 过期的那条可以再签（校验路径上判定为过期 → 清掉 → 放行）
	bridge.setTip(106)
	require.NoError(t, adapter.CheckDepositSigned(oldTx))
	require.Error(t, adapter.CheckDepositSigned(freshTx))

	// tip=108：fresh 也到保留期末尾，放行后集合清空
	bridge.setTip(108)
	require.NoError(t, adapter.CheckDepositSigned(freshTx))
	require.Zero(t, adapter.signed.Len())
}

// Test_SignedDepositSet_ConfigChangePrunesOnRestart 用户要求的行为：
// 先用 TTL=0（只增不删）攒下旧记录 → 改成 TTL 正数并重启 → 旧记录被**立刻**清掉。
func Test_SignedDepositSet_ConfigChangePrunesOnRestart(t *testing.T) {
	bridge := &fakeBridge{btcTip: 100_000}
	store := NewMemStore()
	// 1. TTL=0 期间签了 3 笔（只增不删）
	old := newSignedTestAdapter(t, 0, store, bridge)
	for _, nonce := range []uint32{11, 12, 13} {
		require.NoError(t, old.MarkDepositSigned(testDepositPayload(t, nonce, 200+uint64(nonce))))
	}
	require.Equal(t, 3, old.signed.Len())

	// 2. 改配置 signedDepositTTL=10 后重启：新适配器挂在同一个 store 上，启动清理（client.Start 里
	//    调用的 PruneSignedDeposits）应立即把过期的旧记录删掉。
	restarted := newSignedTestAdapter(t, 10, store, bridge)
	require.Equal(t, 3, restarted.signed.Len(), "清理前旧记录仍在（重启不清空存储）")
	require.NoError(t, restarted.PruneSignedDeposits())
	require.Zero(t, restarted.signed.Len(), "启动清理应立刻删掉过期旧记录")

	// 存储里也真的没了（同一 store 上新起一个集合，看不到旧记录）
	reload := newSignedDepositSet(store, 10, bridge.BtcTipHeight)
	require.Zero(t, reload.Len())

	// 清理后同一 txid 可以再签（记录已不在集合里 → 放行且不再需要查链上高度）
	require.NoError(t, restarted.CheckDepositSigned(testDepositPayload(t, 11, 1000)))

	// 新配置的 TTL 对新记录生效：付款高度落在保留期内的记录仍然被拒
	bridge.setTip(1005)
	fresh := testDepositPayload(t, 14, 1000) // 1005 - 1000 = 5 < TTL(10)
	require.NoError(t, restarted.CheckDepositSigned(fresh))
	require.NoError(t, restarted.MarkDepositSigned(fresh))
	require.Error(t, restarted.CheckDepositSigned(fresh))
	require.Equal(t, 1, restarted.signed.Len(), "保留期内的新记录必须留下")
}

// Test_SignedDepositSet_MarkIdempotent 重复标记不重复写库，且不改写首次记录的高度。
func Test_SignedDepositSet_MarkIdempotent(t *testing.T) {
	bridge := &fakeBridge{}
	store := &countingStore{KVStore: NewMemStore()}
	adapter := newSignedTestAdapter(t, 0, store, bridge)
	payload := testDepositPayload(t, 21, 100)
	txid := mustTxid(t, payload)

	require.NoError(t, adapter.MarkDepositSigned(payload))
	require.Equal(t, 1, store.putCount())
	require.NoError(t, adapter.MarkDepositSigned(payload))
	require.Equal(t, 1, store.putCount(), "重复标记不应重复写库")

	// 高度仍是最初记录的（TTL 锚点不因重复标记漂移）
	require.Equal(t, uint64(100), mustEntry(t, adapter.signed, txid).Height)
	require.NoError(t, adapter.MarkDepositSigned(testDepositPayload(t, 21, 999)))
	require.Equal(t, uint64(100), mustEntry(t, adapter.signed, txid).Height)
}

// Test_SignedDepositSet_TxidStrictParse txid 必须从 TxProof.TxData 严格解析：
// 尾部带多余字节的"另一份编码"不得进集合，也不得绕过集合。
func Test_SignedDepositSet_TxidStrictParse(t *testing.T) {
	bridge := &fakeBridge{}
	adapter := newSignedTestAdapter(t, 0, NewMemStore(), bridge)

	// 空 TxProof / 空 TxData
	require.Error(t, adapter.CheckDepositSigned(&DepositSignPayload{Deposit: &rtypes.DepositAsset{}}))
	require.Error(t, adapter.MarkDepositSigned(&DepositSignPayload{Deposit: &rtypes.DepositAsset{}}))

	// 尾随字节：同一笔交易的另一份编码
	payload := testDepositPayload(t, 31, 100)
	bad := &DepositSignPayload{
		Deposit: &rtypes.DepositAsset{
			Amount:         1000,
			DepositAddress: "addr",
			AssetSymbol:    "RGB20_USDT",
			TxProof: &rtypes.BtcTxProof{
				TxData:      append(append([]byte{}, payload.Deposit.TxProof.TxData...), 0xff),
				BlockHeight: 100,
			},
		},
		Consignment: []byte("consignment"),
		ReceiveID:   "recv-1",
		Chain33Addr: "addr",
	}
	require.ErrorContains(t, adapter.CheckDepositSigned(bad), "non-canonical")
	require.ErrorContains(t, adapter.MarkDepositSigned(bad), "non-canonical")
	require.Zero(t, adapter.signed.Len())

	// 规范编码：能解析出稳定 txid
	require.NoError(t, adapter.CheckDepositSigned(payload))
}

// Test_SignedDepositSet_TipQueryFailureFailsClosed 取不到链上高度时拒绝签名（fail-closed），
// 且只在确有记录需要判定时才查询。
func Test_SignedDepositSet_TipQueryFailureFailsClosed(t *testing.T) {
	bridge := &fakeBridge{btcTip: 100}
	adapter := newSignedTestAdapter(t, 5, NewMemStore(), bridge)
	payload := testDepositPayload(t, 41, 100)
	require.NoError(t, adapter.MarkDepositSigned(payload))
	callsAfterMark := bridge.tipCallCount()

	bridge.setTipErr(errBoomForTest)
	require.Error(t, adapter.CheckDepositSigned(payload), "判不了就必须拒（不得放行重复签名）")
	require.Error(t, adapter.PruneSignedDeposits())
	require.Greater(t, bridge.tipCallCount(), callsAfterMark)

	// 没有记录的 txid 不查询链上高度（默认路径零开销）
	other := testDepositPayload(t, 42, 100)
	callsBefore := bridge.tipCallCount()
	bridge.setTipErr(nil)
	require.NoError(t, adapter.CheckDepositSigned(other))
	require.Equal(t, callsBefore, bridge.tipCallCount())
}

// Test_ValidateDepositConsignment_SignedSetDedup 校验流程内的去重：通过校验并签名后，
// 同一条消息再来会被 ValidateDepositConsignment 拒掉（不再进签名轮次）。
func Test_ValidateDepositConsignment_SignedSetDedup(t *testing.T) {
	mock := NewMockSidecar()
	mock.ValidateResp = []*pb.ConsignmentValidation{validValidation(200), validValidation(200)}
	adapter, cleanup := newTestAdapter(t, mock, &fakeBridge{})
	defer cleanup()

	rec := &ReceiveRecord{
		ReceiveID:   "recv-1",
		AssetSymbol: "RGB20_USDT",
		Chain33Addr: "addr",
		Amount:      1000,
		Status:      ReceiveStatusSettled,
		Seal:        "aaaa:0",
	}
	require.NoError(t, adapter.receives.Put(rec))
	payload := testDepositPayload(t, 51, 100)
	payload.ReceiveID = "recv-1"

	// 首次：签名前校验通过 → 签名成功 → 登记
	require.NoError(t, adapter.ValidateDepositConsignment(payload))
	require.NoError(t, adapter.MarkDepositSigned(payload))

	// 再来一次（同一笔付款交易的重复 signing 轮次）：校验阶段就被拒
	err := adapter.ValidateDepositConsignment(payload)
	require.ErrorContains(t, err, "already signed")
}

var errBoomForTest = errors.New("btc tip query failed")

// mustTxid 取 payload 的付款交易 txid（解析失败直接失败测试）。
func mustTxid(t *testing.T, payload *DepositSignPayload) string {
	t.Helper()
	txid, err := depositTxid(payload)
	require.NoError(t, err)
	return txid
}

func mustEntry(t *testing.T, s *SignedDepositSet, txid string) *SignedDeposit {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.cache[txid]
	require.True(t, ok, "txid %s 不在已签集合里", txid)
	return entry
}
