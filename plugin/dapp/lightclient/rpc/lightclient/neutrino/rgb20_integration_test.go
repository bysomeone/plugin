package neutrino

import (
	"bytes"
	"context"
	"encoding/asn1"
	"encoding/json"
	"math/big"
	"testing"

	"github.com/33cn/chain33/types"
	ltypes "github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
	"github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20"
	rgb20pb "github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20/pb"
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

// derEncodeSignature 手工 DER 编码 (r, s)。
//
// 不用 ecdsa.Signature.Serialize()：btcec/v2 的 Signature 是 decred secp256k1 的类型别名，
// 其 Serialize() 会把 S 强制归一化为 low-S（保证签名不可锻造），因此 Serialize() 的产物永远
// 到不了 tss.go 里的 normalizeLowS。要构造"真正 high-S 的签名输入"，只能自己编码 DER。
func derEncodeSignature(t *testing.T, r, s btcec.ModNScalar) []byte {
	t.Helper()
	rBytes, sBytes := r.Bytes(), s.Bytes()
	der, err := asn1.Marshal(struct {
		R, S *big.Int
	}{
		R: new(big.Int).SetBytes(rBytes[:]),
		S: new(big.Int).SetBytes(sBytes[:]),
	})
	require.NoError(t, err)
	return der
}

// Test_signPsbtWithSigners_broadcastTxLowSSig signPsbt：sighash 取自 PSBT witness utxo，
// high-S 签名被归一化为 low-S，且返回值是已 finalize、可直接广播的交易。
//
// 断言对象是"真正会被广播的产物"而非 PSBT 的中间态：signPsbtWithSigners 末尾会 MaybeFinalizeAll，
// partial_sigs 被搬入 final witness，所以只能从 Extract 出的交易里取签名来验（这也正是侧车
// extract_tx 后广播的那笔交易，即 49a743974 修的 "Witness program hash mismatch" 回归面）。
//
// 为什么不拆成"partial_sigs 中间态"+"finalize 产物"两个测试：finalize 前的中间态无法从该函数的
// 返回值拿到（它只返回 finalize 后的序列化字节），要构造中间态只能把签名逻辑复制一遍，那样测的是
// 副本而不是被测函数。故此处合并为对 finalize 产物的一次断言。
//
// 反经验证注记：若让"signer 返回的原始字节"绕过整条归一化链直达 witness，本测试即变红（测到了东西）；
// 但单独摘掉 tss.go 里的 normalizeLowS 调用，本测试仍绿——因为紧随其后的 sig.Serialize() 自身就会
// 把 S 归一化为 low-S。即该调用在当前依赖版本下是冗余的，low-S 广播不变量实际由 Serialize() 保证。
func Test_signPsbtWithSigners_broadcastTxLowSSig(t *testing.T) {
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
		r := sig.R()
		s := sig.S()
		// 强制 high-S：若为低-S 则取负。
		if !s.IsOverHalfOrder() {
			s = *new(btcec.ModNScalar).NegateVal(&s)
		}
		require.True(t, s.IsOverHalfOrder(), "测试输入必须是 high-S")
		// 关键：不能走 sig.Serialize()——btcec/decred 的 Serialize 会把 S 强制归一化回 low-S，
		// 那样这个"high-S 输入"到不了被测的 normalizeLowS，测试就测不出归一化是否还在。
		// 这里手工 DER 编码，保证送进 signPsbtWithSigners 的确实是 high-S 签名。
		der := derEncodeSignature(t, r, s)
		back, err := ecdsa.ParseDERSignature(der)
		require.NoError(t, err)
		backS := back.S()
		require.True(t, backS.IsOverHalfOrder(), "DER 编码后必须仍是 high-S，否则本测试测不到归一化")
		return &signResult{sig: der}
	})
	require.NoError(t, err)
	require.NotEmpty(t, signed)

	// 返回的 PSBT 应已 finalize（partial_sigs 搬入 final witness），并能 Extract 成可广播交易。
	out, err := psbt.NewFromRawBytes(bytes.NewReader(signed), false)
	require.NoError(t, err)
	require.Empty(t, out.Inputs[0].PartialSigs, "finalize 后 partial_sigs 应已搬入 final witness")
	broadcastTx, err := psbt.Extract(out)
	require.NoError(t, err)

	// P2WPKH witness = [sig||sighashType, pubkey]。
	witness := broadcastTx.TxIn[0].Witness
	require.Len(t, witness, 2, "P2WPKH witness 应为 [sig, pubkey]")
	sigBytes := witness[0]
	require.Equal(t, byte(txscript.SigHashAll), sigBytes[len(sigBytes)-1])
	parsed, err := ecdsa.ParseDERSignature(sigBytes[:len(sigBytes)-1])
	require.NoError(t, err)
	s := parsed.S()
	require.False(t, s.IsOverHalfOrder(), "signature must be low-S")
	require.Equal(t, pub.SerializeCompressed(), witness[1])

	// 该签名必须真的对这笔广播交易有效（签的是本输入的 witness sighash，而非别的消息）。
	sigHashes := txscript.NewTxSigHashes(broadcastTx, txscript.NewMultiPrevOutFetcher(
		map[wire.OutPoint]*wire.TxOut{prevOut: {Value: 100000, PkScript: pkScript}}))
	sigHash, err := txscript.CalcWitnessSigHash(pkScript, sigHashes, txscript.SigHashAll, broadcastTx, 0, 100000)
	require.NoError(t, err)
	require.True(t, parsed.Verify(sigHash, pub))
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

// Test_VerifyDepositSpv_rejectsNonCanonicalTxData A3（第一半）验收：签名节点在签 C 之前必须拒绝
// 非规范编码的 TxData（尾部追加字节）——"同一笔支付交易的另一份编码"（txid/merkle 全不变）不得
// 被签名节点放行，否则签名节点会为被别名化的 deposit 出签名。
func Test_VerifyDepositSpv_rejectsNonCanonicalTxData(t *testing.T) {
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.DoubleHashH([]byte("a3-prevout")), Index: 0}, nil, nil))
	tx.AddTxOut(wire.NewTxOut(1000, []byte{0x51}))
	buf := bytes.NewBuffer(make([]byte, 0, tx.SerializeSizeStripped()))
	require.NoError(t, tx.SerializeNoWitness(buf))
	raw := buf.Bytes()

	// mainChainGrpc 留空：规范性校验必须先于任何 BTC 头查询。若该校验被去掉，下面的用例会走到
	// getLightBtcHeader 而 panic（而非返回错误），即测试以失败暴露回归。
	n := &neutrinoClient{}

	require.EqualError(t, n.VerifyDepositSpv(&rtypes.BtcTxProof{}), "empty spv proof")
	require.ErrorContains(t, n.VerifyDepositSpv(&rtypes.BtcTxProof{TxData: []byte{0xff, 0xff}}), "decode spv tx")

	for _, extra := range []int{1, 4, 32} {
		appended := append(append([]byte{}, raw...), bytes.Repeat([]byte{0x00}, extra)...)
		// 复现前提：尾部追加字节后 txid 不变（SPV 的 txid 口径完全一致），只有原始字节变了。
		var parsed wire.MsgTx
		require.NoError(t, parsed.DeserializeNoWitness(bytes.NewReader(appended)))
		require.Equal(t, tx.TxHash(), parsed.TxHash())

		err := n.VerifyDepositSpv(&rtypes.BtcTxProof{TxData: appended})
		require.ErrorContainsf(t, err, "non-canonical", "尾部 %d 字节的 TxData 必须被签名节点拒绝", extra)
	}
}

// ---- E11：RGB20 提现签名通知必须携带提现上下文（签名节点才可能执行交叉核对）----

// Test_handleRgb20WithdrawSign_RequiresWithdrawContext 锁住 E11：签名节点收到**不带上下文**
// 的提现签名通知时必须直接拒签，而不是"跳过校验、只参与签名"。
//
// 之前 tssService 里有一条 len(notify.Payload)==0 ⇒ 跳过全部提现核对直接 GG18 签名的旁路
// （本为 E2E 的 sign-psbt 端点所加），它同时也是生产路径 ⇒ ValidateWithdrawPsbt 整段不执行
// （金额覆盖核对 S1、BL-4 交叉核对、费用区间、dust cap、同步高度、closed-seal 非 pending-mint
// 全部失效）。这个旁路已改为独立的、需显式开关的测试签名类型（transactionTypeTestSign）。
//
// 断言方式：tssService 的 client 为 nil —— 一旦真的往下走到签名就会 panic，因此"返回错误而
// 不是 panic"本身即证明没有进入签名路径。
func Test_handleRgb20WithdrawSign_RequiresWithdrawContext(t *testing.T) {
	ts := &tssService{}
	err := ts.handleRgb20WithdrawSign(&ltypes.TssSignNotify{Psbt: []byte{0x01}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "without payload")

	payload, err := json.Marshal(&rgb20.WithdrawSignPayload{Chain33TxHash: []byte{0xaa}})
	require.NoError(t, err)
	err = ts.handleRgb20WithdrawSign(&ltypes.TssSignNotify{Psbt: []byte{0x01}, Payload: payload})
	require.Error(t, err)
	require.Contains(t, err.Error(), "without consignment")
}

// Test_handleTestSignNotify_RequiresOptIn 无上下文的测试签名必须由本地配置显式打开
// （默认关闭）：打开它就等于"用 TSS 组私钥签任意 PSBT"，生产环境不得可达。
func Test_handleTestSignNotify_RequiresOptIn(t *testing.T) {
	ts := &tssService{client: &neutrinoClient{}}
	require.False(t, ts.testSignEnabled())
	err := ts.handleTestSignNotify(&ltypes.TssSignNotify{Psbt: []byte{0x01}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "testSignPsbt")

	ts.client.cfg.Rgb20.TestSignPsbt = true
	require.True(t, ts.testSignEnabled())
}

// Test_checkWithdrawClaims 签名节点对协调者声称值的核对（信任边界）：金额/费率/高度/收款
// invoice 任一项与链上 pending（真值）不一致即拒签。
func Test_checkWithdrawClaims(t *testing.T) {
	pending := &rtypes.PendingTx{
		Amount:        500000,
		FeeRate:       2,
		TxBlockHeight: 120,
		TargetAddress: "rgb:invoice",
	}
	claim := &rgb20.WithdrawSignPayload{
		Chain33TxHash:    []byte{0x01},
		Amount:           500000,
		FeeRate:          2,
		TxBlockHeight:    120,
		RecipientInvoice: "rgb:invoice",
	}
	require.NoError(t, checkWithdrawClaims(claim, pending))

	cases := []struct {
		name    string
		mutate  func(*rgb20.WithdrawSignPayload)
		wantErr string
	}{
		{"amount", func(p *rgb20.WithdrawSignPayload) { p.Amount = 499999 }, "amount mismatch"},
		{"fee rate", func(p *rgb20.WithdrawSignPayload) { p.FeeRate = 3 }, "fee rate mismatch"},
		{"block height", func(p *rgb20.WithdrawSignPayload) { p.TxBlockHeight = 121 }, "block height mismatch"},
		{"recipient invoice", func(p *rgb20.WithdrawSignPayload) { p.RecipientInvoice = "rgb:other" }, "recipient invoice mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := *claim
			tc.mutate(&c)
			err := checkWithdrawClaims(&c, pending)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// Test_SignRejectionRelay 拒签回执的回传与采纳：
//   - 协调者只采纳**本轮签名节点**发来的回执（回执走 P2P 广播，任何人都能发，否则伪造一份
//     回执就能让一笔合法提现被永久判为不可恢复）；
//   - 回执还原成不可恢复错误（协调者据此停止重试，不再每秒刷屏）；
//   - 回执在 isSigner 门槛之前处理：它是广播事实，Signers 为空，过不了签名者检查。
func Test_SignRejectionRelay(t *testing.T) {
	hash := []byte("chain33-withdraw-hash")
	newService := func() *tssService {
		ts := newTssService(&neutrinoClient{})
		ts.dkgCompleted.Store(true) // 通知处理的前置条件（真实运行时由 init 置位）
		ts.setSignRoundSigners(hash, []string{"peer-1", "peer-2"})
		return ts
	}

	t.Run("accepted from a signer of this round and classified", func(t *testing.T) {
		ts := newService()
		require.NoError(t, ts.takeSignRejection(hash), "尚无回执时不得拒绝")
		notice, err := json.Marshal(&rgb20.WithdrawSignReject{
			Chain33TxHash: hash,
			Class:         "sticky-seal-mismatch",
			Reason:        "sticky seal changed: recorded=a spending=b",
			Rejector:      "peer-2",
		})
		require.NoError(t, err)
		ts.handleSignNotify(&types.TopicData{
			Topic: tssSignNotifyTopic,
			From:  "peer-2",
			Data:  types.Encode(&ltypes.TssSignNotify{TxType: transactionTypeRgb20WithdrawReject, Payload: notice}),
		})
		err = ts.takeSignRejection(hash)
		require.Error(t, err)
		class, unrecoverable := rgb20.IsUnrecoverableWithdraw(err)
		require.True(t, unrecoverable, "err=%v", err)
		require.Equal(t, "sticky-seal-mismatch", class)
		require.Contains(t, err.Error(), "peer-2")
	})

	t.Run("ignored from a node that is not a signer of this round", func(t *testing.T) {
		ts := newService()
		notice, err := json.Marshal(&rgb20.WithdrawSignReject{
			Chain33TxHash: hash,
			Class:         "sticky-seal-mismatch",
			Reason:        "forged",
			Rejector:      "peer-9",
		})
		require.NoError(t, err)
		ts.handleSignNotify(&types.TopicData{
			Topic: tssSignNotifyTopic,
			Data:  types.Encode(&ltypes.TssSignNotify{TxType: transactionTypeRgb20WithdrawReject, Payload: notice}),
		})
		require.NoError(t, ts.takeSignRejection(hash), "非本轮签名节点的回执必须被忽略")
	})

	t.Run("non-signer withdraw notify is dropped", func(t *testing.T) {
		ts := newService()
		ts.handleSignNotify(&types.TopicData{
			Topic: tssSignNotifyTopic,
			Data: types.Encode(&ltypes.TssSignNotify{
				TxType:  transactionTypeRgb20Withdraw,
				Signers: []string{"peer-1"},
				Psbt:    []byte{0x01},
			}),
		})
		require.Empty(t, ts.signRejections)
	})
}

// ---- E9-B / E11 处理器级用例：真实适配器 + 假侧车，验证判定的**组合**（而非单个函数）----

// stubRgb20Bridge 只提供 TSS 脚本（交叉核对用），其余 Chain33Bridge 方法本用例不会调用。
// 嵌入 nil 接口：一旦调用到未实现的方法会立刻 panic（而不是静默返回零值）。
type stubRgb20Bridge struct{ rgb20.Chain33Bridge }

func (s *stubRgb20Bridge) TSSPkScript() []byte { return testRgb20TssScript }

func (s *stubRgb20Bridge) TSSAddress() string { return "bcrt1qxy2kgdygjrsqtzq2n0yrf2493p83kkfjhx0wlh" }

// testRgb20TssScript 与 rgb20 包单测同形的 P2WPKH 脚本（OP_0 <20B>）。
var testRgb20TssScript = []byte{0x00, 0x14, 0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09,
	0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13}

const testRgb20SealOutpoint = "1111111111111111111111111111111111111111111111111111111111111111:0"

// buildTestRgb20WithdrawPSBT 构造真机布局的提现 PSBT：vout0 = OP_RETURN（RGB 承诺）、
// vout1 = 收款 dust（非 TSS，离开桥控制）、vout2 = 找零回 TSS。
func buildTestRgb20WithdrawPSBT(t *testing.T, sealOutpoint string) []byte {
	t.Helper()
	op, err := wire.NewOutPointFromString(sealOutpoint)
	require.NoError(t, err)
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(wire.NewTxIn(op, nil, nil))
	tx.AddTxOut(wire.NewTxOut(0, []byte{txscript.OP_RETURN, 0x01}))
	tx.AddTxOut(wire.NewTxOut(546, []byte{0x51}))
	tx.AddTxOut(wire.NewTxOut(4000, testRgb20TssScript))
	p, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)
	p.Inputs[0].WitnessUtxo = &wire.TxOut{Value: 5000, PkScript: testRgb20TssScript}
	var buf bytes.Buffer
	require.NoError(t, p.Serialize(&buf))
	return buf.Bytes()
}

// rgb20Consignment 构造锚定在该 PSBT 交易上的侧车校验结果：关闭 sealOutpoint、打开
// vout1（收款，离开桥控制）。
func rgb20Consignment(t *testing.T, psbtBytes []byte, sealOutpoint string, amount int64) *rgb20pb.ConsignmentValidation {
	t.Helper()
	p, err := psbt.NewFromRawBytes(bytes.NewReader(psbtBytes), false)
	require.NoError(t, err)
	anchor := p.UnsignedTx.TxHash().String()
	return &rgb20pb.ConsignmentValidation{
		Valid:        true,
		Amount:       amount,
		SyncedHeight: 200,
		ClosedSeals:  []string{sealOutpoint},
		OpenedSeals: []*rgb20pb.OpenedSeal{
			{Outpoint: anchor + ":1", Amount: amount},
		},
	}
}

// newTestSignerService 构造一个真实的 rgb20 适配器（连假侧车）+ 签名节点服务。
func newTestSignerService(t *testing.T) (*tssService, *rgb20.MockSidecar, *rgb20.Adapter) {
	t.Helper()
	mock := rgb20.NewMockSidecar()
	sock, cleanup := rgb20.StartTestSidecar(t, mock)
	t.Cleanup(cleanup)
	adapter, err := rgb20.NewAdapter(rgb20.Config{
		SidecarAddr: sock,
		Precision:   6,
		Contracts: []rgb20.Contract{
			{Symbol: "RGB20_USDT", Precision: 6, MinDeposit: 100, MinWithdraw: 100},
		},
		ChangeAddress: "bcrt1qxxxx",
	}, rgb20.NewMemStore())
	require.NoError(t, err)
	adapter.SetBridge(&stubRgb20Bridge{})
	require.NoError(t, adapter.Connect(context.Background()))
	require.NoError(t, adapter.Start(context.Background()))
	t.Cleanup(adapter.Stop)

	ts := &tssService{client: &neutrinoClient{rgb20: adapter}}
	return ts, mock, adapter
}

// Test_validateRgb20WithdrawSign 处理器级组合：声称值核对 → 交叉核对 → sticky 核对都在
// **签名之前**，任一步失败即拒签；全部通过时返回本笔花掉的 seal 集合（供签名成功后落盘）。
func Test_validateRgb20WithdrawSign(t *testing.T) {
	chain33Hash := []byte("chain33-withdraw-hash")
	pending := &rtypes.PendingTx{
		Amount:        500000,
		FeeRate:       1,
		TxBlockHeight: 100,
		TargetAddress: "rgb:invoice",
	}
	claim := &rgb20.WithdrawSignPayload{
		Chain33TxHash:    chain33Hash,
		Amount:           500000,
		FeeRate:          1,
		TxBlockHeight:    100,
		RecipientInvoice: "rgb:invoice",
	}
	psbtBytes := buildTestRgb20WithdrawPSBT(t, testRgb20SealOutpoint)
	consignment := []byte("consignment")

	t.Run("claims tampered by the coordinator are refused before any rgb check", func(t *testing.T) {
		ts, mock, _ := newTestSignerService(t)
		mock.ValidateResp = []*rgb20pb.ConsignmentValidation{
			rgb20Consignment(t, psbtBytes, testRgb20SealOutpoint, 500000),
		}
		bad := *claim
		bad.Amount = 600000
		_, err := ts.validateRgb20WithdrawSign(&bad, psbtBytes, consignment, pending)
		require.Error(t, err)
		require.Contains(t, err.Error(), "amount mismatch")
		// 声称值就没过，侧车那一步不该被触及（假侧车每响应一次就消费一条 ValidateResp）。
		require.Len(t, mock.ValidateResp, 1, "声称值核对失败时不得去问侧车")
	})

	t.Run("valid request returns the spent seal set", func(t *testing.T) {
		ts, mock, adapter := newTestSignerService(t)
		mock.ValidateResp = []*rgb20pb.ConsignmentValidation{
			rgb20Consignment(t, psbtBytes, testRgb20SealOutpoint, 500000),
		}
		seals, err := ts.validateRgb20WithdrawSign(claim, psbtBytes, consignment, pending)
		require.NoError(t, err)
		require.Equal(t, []string{testRgb20SealOutpoint}, seals)
		// 判定阶段**不得**落盘 sticky 记录：记录必须等签名成功之后才写，否则一次签名失败
		// （GG18 超时、广播失败前的任何一步）就会把该笔提现的合法重试永久锁死。
		require.Empty(t, adapter.GetStickySeal(chain33Hash), "校验阶段不得写 sticky 记录")
	})

	t.Run("sticky seal switched by a retry is refused (E9)", func(t *testing.T) {
		const otherSeal = "2222222222222222222222222222222222222222222222222222222222222222:1"
		ts, mock, adapter := newTestSignerService(t)
		// 本笔提现此前已按 seal A 成功签名并落盘（签名成功之后才写，见 handleRgb20WithdrawSign）。
		require.NoError(t, adapter.SetStickySeal(chain33Hash, []string{testRgb20SealOutpoint}))

		// 重试时侧车换了一组 seal：consignment 关闭的是 seal B（不是既有记录里的 seal A）。
		mock.ValidateResp = []*rgb20pb.ConsignmentValidation{
			rgb20Consignment(t, psbtBytes, otherSeal, 500000),
		}
		_, err := ts.validateRgb20WithdrawSign(claim, psbtBytes, consignment, pending)
		require.Error(t, err)
		class, unrecoverable := rgb20.IsUnrecoverableWithdraw(err)
		require.True(t, unrecoverable, "err=%v", err)
		require.Equal(t, "sticky-seal-mismatch", class)
	})

	t.Run("consignment that closes nothing in this tx is refused (fail-closed)", func(t *testing.T) {
		ts, mock, _ := newTestSignerService(t)
		v := rgb20Consignment(t, psbtBytes, testRgb20SealOutpoint, 500000)
		v.ClosedSeals = nil // 本笔 burn 没有绑定任何 seal
		mock.ValidateResp = []*rgb20pb.ConsignmentValidation{v}
		_, err := ts.validateRgb20WithdrawSign(claim, psbtBytes, consignment, pending)
		require.Error(t, err)
		_, unrecoverable := rgb20.IsUnrecoverableWithdraw(err)
		require.True(t, unrecoverable, "err=%v", err)
	})
}
