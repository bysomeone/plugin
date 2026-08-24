package neutrino

import (
	"bytes"
	"testing"

	"github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

const testKnownRgbTxid = "0000000000000000000000000000000000000000000000000000000000000001"

func btcutilNewWitnessAddr(pub *btcec.PublicKey, params *chaincfg.Params) (btcutil.Address, error) {
	pubHash := btcutil.Hash160(pub.SerializeCompressed())
	return btcutil.NewAddressWitnessPubKeyHash(pubHash, params)
}

func pubKeyHashScript(pub *btcec.PublicKey) []byte {
	pubHash := btcutil.Hash160(pub.SerializeCompressed())
	return append([]byte{txscript.OP_0, 0x14}, pubHash...)
}

// newTestRgb20Adapter 构造一个带内存存储、已登记已知 RGB txid 的适配器（不连接真实侧车）。
func newTestRgb20Adapter(t *testing.T) *rgb20.Adapter {
	t.Helper()
	adapter, err := rgb20.NewAdapter(rgb20.Config{SidecarAddr: "/tmp/nonexistent.sock"}, rgb20.NewMemStore())
	require.NoError(t, err)
	return adapter
}

func testBtcWalletWithRgb20(adapter *rgb20.Adapter) *btcWallet {
	return &btcWallet{
		client: &neutrinoClient{rgb20: adapter},
	}
}

// Test_analyzeTransaction_skipsKnownRgbTx BL-5：已知 RGB 充值交易（即使带 rgbx:deposit OP_RETURN 双标记）
// 也跳过 BTC 充值路径，避免双入账。
func Test_analyzeTransaction_skipsKnownRgbTx(t *testing.T) {
	adapter := newTestRgb20Adapter(t)
	b := testBtcWalletWithRgb20(adapter)

	// 先登记一条已结算的 RGB 充值记录（receive settle 会记录已知 txid）。
	rec := &rgb20.ReceiveRecord{
		ReceiveID:   "recv-1",
		AssetSymbol: rtypes.RGB20USDTSymbol,
		Chain33Addr: "1JnYYeefMhWsXvZyvjCKPZK7eYQdFpzDsk",
		Amount:      1000,
		Status:      rgb20.ReceiveStatusSettled,
	}
	require.NoError(t, adapter.ReceiveStore().Put(rec))
	require.NoError(t, adapter.ReceiveStore().Settle("recv-1", testKnownRgbTxid, 0, testKnownRgbTxid+":0"))
	require.True(t, adapter.IsKnownRgbTxid(testKnownRgbTxid))

	// 构造一个带 TSS 输出 + rgbx:deposit OP_RETURN 的"充值"交易；其 txid 为已知 RGB txid。
	knownHash, err := chainhash.NewHashFromStr(testKnownRgbTxid)
	require.NoError(t, err)
	tx := wire.NewMsgTx(wire.TxVersion)
	opRet, err := txscript.NullDataScript([]byte("rgbx:deposit:1JnYYeefMhWsXvZyvjCKPZK7eYQdFpzDsk"))
	require.NoError(t, err)
	tx.AddTxOut(wire.NewTxOut(0, opRet))
	tx.AddTxOut(wire.NewTxOut(100000, []byte{txscript.OP_0, 0x14, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}))

	pending := b.analyzeTransaction(knownHash, tx)
	require.Nil(t, pending, "known rgb tx must be skipped from BTC deposit path")
}

// Test_analyzeTransaction_normalDepositUnchanged 非 RGB 的普通充值仍走 BTC 路径。
func Test_analyzeTransaction_normalDepositUnchanged(t *testing.T) {
	adapter := newTestRgb20Adapter(t)
	b := testBtcWalletWithRgb20(adapter)

	// 需要 tssPkScript / tssPubKey 才能判定普通充值。
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pub := priv.PubKey()
	waddr, err := btcutilNewWitnessAddr(pub, &chaincfg.TestNet3Params)
	require.NoError(t, err)
	pkScript, err := txscript.PayToAddrScript(waddr)
	require.NoError(t, err)
	b.tssPkScript = pkScript
	b.tssPubKey = pub

	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxOut(wire.NewTxOut(100000, pkScript))

	hash := tx.TxHash()
	pending := b.analyzeTransaction(&hash, tx)
	require.NotNil(t, pending)
	require.Equal(t, transactionTypeDeposit, pending.txType)
}

// Test_signPsbtWithSigners_writesLowSPartialSig signPsbt：sighash 取自 PSBT witness utxo，
// 写入 partial_sigs，且 high-S 签名被归一化为 low-S。
func Test_signPsbtWithSigners_writesLowSPartialSig(t *testing.T) {
	// 构造含 1 个 P2WPKH 输入的未签 PSBT。
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pub := priv.PubKey()
	pkScript := pubKeyHashScript(pub)

	prevTx := wire.NewMsgTx(wire.TxVersion)
	prevTx.AddTxOut(wire.NewTxOut(100000, pkScript))
	prevOut := wire.OutPoint{Hash: prevTx.TxHash(), Index: 0}

	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(wire.NewTxIn(&prevOut, nil, nil))
	tx.AddTxOut(wire.NewTxOut(99000, pkScript))

	p, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)
	p.Inputs[0].WitnessUtxo = &wire.TxOut{Value: 100000, PkScript: pkScript}
	p.Inputs[0].SighashType = txscript.SigHashAll

	ts := &tssService{tssPublicKey: pub}
	signed, err := ts.signPsbtWithSigners(p, []string{"peer"}, func(sigHash []byte, _ string) *signResult {
		sig := ecdsa.Sign(priv, sigHash)
		s := sig.S()
		// 强制 high-S：若为低-S 则取负。
		if !s.IsOverHalfOrder() {
			r := sig.R()
			sNeg := new(btcec.ModNScalar).NegateVal(&s)
			sig = ecdsa.NewSignature(&r, sNeg)
		}
		s = sig.S()
		require.True(t, s.IsOverHalfOrder())
		return &signResult{sig: sig.Serialize()}
	})
	require.NoError(t, err)
	require.NotEmpty(t, signed)

	// 反序列化检查 partial_sigs 已写入且 low-S。
	out, err := psbt.NewFromRawBytes(bytes.NewReader(signed), false)
	require.NoError(t, err)
	require.Len(t, out.Inputs[0].PartialSigs, 1)
	sigBytes := out.Inputs[0].PartialSigs[0].Signature
	require.Equal(t, byte(txscript.SigHashAll), sigBytes[len(sigBytes)-1])
	parsed, err := ecdsa.ParseDERSignature(sigBytes[:len(sigBytes)-1])
	require.NoError(t, err)
	ps := parsed.S()
	require.False(t, ps.IsOverHalfOrder(), "signature must be low-S")
	require.Equal(t, pub.SerializeCompressed(), out.Inputs[0].PartialSigs[0].PubKey)
}

// Test_isRgb20Asset_route 判断 RGB20 pending 是否路由到 rgb20 适配器。
func Test_isRgb20Asset_route(t *testing.T) {
	adapter := newTestRgb20Adapter(t)
	adapter.Registry().Register(&rgb20.Contract{
		Symbol:        rtypes.RGB20USDTSymbol,
		SidecarSymbol: "USDT",
	})
	n := &neutrinoClient{rgb20: adapter}
	require.True(t, n.isRgb20Asset(rtypes.RGB20USDTSymbol))
	require.False(t, n.isRgb20Asset("BTC"))
	// 未配置 rgb20 时一律 false
	n2 := &neutrinoClient{}
	require.False(t, n2.isRgb20Asset(rtypes.RGB20USDTSymbol))
}

// Test_processWithdrawConfirm_rgb20EmptyHash RGB20 提现确认缺 chain33 映射时直接丢弃，
// 避免 getPendingTxBlockIndex("") 对空 hash 反复查询死循环（HR-2）。
func Test_processWithdrawConfirm_rgb20EmptyHash(t *testing.T) {
	adapter := newTestRgb20Adapter(t)
	// 登记已知 RGB 提现 txid。
	rec := &rgb20.ReceiveRecord{ReceiveID: "recv-w", AssetSymbol: rtypes.RGB20USDTSymbol}
	require.NoError(t, adapter.ReceiveStore().Put(rec))
	require.NoError(t, adapter.ReceiveStore().Settle("recv-w", testKnownRgbTxid, 0, testKnownRgbTxid+":0"))

	bw := &btcWallet{removePendingChan: make(chan chainhash.Hash, 4)}
	n := &neutrinoClient{rgb20: adapter, rgbx: newRGBX(), bw: bw}

	knownHash, err := chainhash.NewHashFromStr(testKnownRgbTxid)
	require.NoError(t, err)
	confirm := &confirmWithdraw{
		btcPending: &btcPendingTx{
			txHash:                *knownHash,
			txType:                transactionTypeWithdraw,
			chain33WithdrawTxHash: nil, // 缺映射
		},
	}
	// 应返回 true（丢弃，不进入 getPendingTxBlockIndex 死循环）。
	require.True(t, n.processWithdrawConfirm(confirm))
}

// Test_normalizeLowS high-S 签名归一化为 low-S。
func Test_normalizeLowS(t *testing.T) {
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	msg := []byte("hello rgb")
	sig := ecdsa.Sign(priv, msg)
	s := sig.S()
	// 若本身就是 low-S，强制转 high-S 再归一化。
	if !s.IsOverHalfOrder() {
		r := sig.R()
		sNeg := new(btcec.ModNScalar).NegateVal(&s)
		sig = ecdsa.NewSignature(&r, sNeg)
	}
	highS := sig.S()
	require.True(t, highS.IsOverHalfOrder())
	norm := normalizeLowS(sig)
	ns := norm.S()
	require.False(t, ns.IsOverHalfOrder())
	require.True(t, norm.Verify(msg, priv.PubKey()))
}
