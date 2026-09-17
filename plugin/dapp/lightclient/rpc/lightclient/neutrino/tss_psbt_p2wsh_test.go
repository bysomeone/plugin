package neutrino

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"testing"

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

/*
 * C3（签名层）的真实验签闭环。
 *
 * 这里跑的不是"签名字节长什么样"，而是**完整的可复现花费**：
 *
 *	C1 冻结向量 → 真实的 P2WSH UTXO（钱确实付给了那个 program）
 *	  → 构造花费它的 PSBT（每输入带 witness_script）
 *	  → 走 **真实的签名路径** signPsbtWithSigners（TSS 用 signFn 注入，与单测同一注入点）
 *	  → 它自己 finalize（P2WSH 不能交给 psbt.MaybeFinalizeAll，见 tss.go 的注释）
 *	  → psbt.Extract 出广播形态的交易
 *	  → txscript.NewEngine 对**该输入**验签
 *
 * 验签通过才说明 scriptCode 用对了 —— 这是 C3 的全部意义所在：
 * 用 output program（34 字节 `00 20 || program`）当 scriptCode 算出的 sighash 与用
 * witnessScript 算出的**不是同一个**，拿前者签出来的签名在链上必被 OP_CHECKSIG 拒。
 *
 * 向量与 C1/C2 **同源**（同一份 testdata/p2wsh_deposit_vectors.json，见 deposit_p2wsh_test.go）：
 * 桥、合约、侧车三方对同一组 (userID, tssPub) 必须得到逐字节相同的脚本与地址。
 */

// p2wshSignFixture 一组 P2WSH 花费夹具：一个真实付到该 program 的 UTXO + 能花它的群私钥。
type p2wshSignFixture struct {
	userID        string
	groupPub      []byte
	groupPriv     *btcec.PrivateKey
	witnessScript []byte
	pkScript      []byte
	// fundingTx 模拟"外部用户付 BTC 到该用户的 P2WSH 充值地址"的交易（链上只看得见 34 字节 program）。
	fundingTx *wire.MsgTx
	value     int64
	// mainPoolScript 主池（桥自己的 P2WPKH）脚本 —— 找零/费输入的去处。
	mainPoolScript []byte
}

func newP2WSHSignFixture(t *testing.T) *p2wshSignFixture {
	t.Helper()
	vectors := loadDepositVectors(t)
	v := vectors[0] // v1-chain33-address：C1 的主向量
	privBytes, err := hex.DecodeString(v.TssPrivKey)
	require.NoError(t, err, "冻结向量必须带可用于验签的测试私钥")
	priv, pub := btcec.PrivKeyFromBytes(privBytes)
	require.Equal(t, vectorPub(t, v), pub.SerializeCompressed(), "向量里的私钥必须对应向量里的公钥")

	ws, err := rtypes.DeriveDepositWitnessScript(v.UserID, pub.SerializeCompressed())
	require.NoError(t, err)
	require.Equal(t, v.WitnessScript, hex.EncodeToString(ws), "witnessScript 必须与冻结向量逐字节一致")
	pkScript, err := rtypes.DeriveDepositPkScript(v.UserID, pub.SerializeCompressed())
	require.NoError(t, err)
	require.Equal(t, v.PkScript, hex.EncodeToString(pkScript), "pkScript 必须与冻结向量逐字节一致")

	f := &p2wshSignFixture{
		userID:        v.UserID,
		groupPub:      pub.SerializeCompressed(),
		groupPriv:     priv,
		witnessScript: ws,
		pkScript:      pkScript,
		value:         100_000,
	}
	// 主池脚本 = 同一把群钥的 P2WPKH（与生产一致的"主池"形态）。
	mainAddr, err := btcutil.NewAddressWitnessPubKeyHash(btcutil.Hash160(f.groupPub), &chaincfg.RegressionNetParams)
	require.NoError(t, err)
	f.mainPoolScript, err = txscript.PayToAddrScript(mainAddr)
	require.NoError(t, err)

	// 充值交易：外部付款方付 value 到这个 program。
	f.fundingTx = wire.NewMsgTx(wire.TxVersion)
	f.fundingTx.AddTxIn(wire.NewTxIn(
		&wire.OutPoint{Hash: chainhash.DoubleHashH([]byte("ext-funding-" + v.UserID)), Index: 0}, nil, nil))
	f.fundingTx.AddTxOut(wire.NewTxOut(f.value, f.pkScript))
	return f
}

// deriveExtraScript 用同一个群公钥、另一个 userID 再派生一组充值脚本
// （真实形态：一把 TSS 群钥服务 N 个用户，各自一个 witnessScript）。
func (f *p2wshSignFixture) deriveExtraScript(t *testing.T, userID string) (witnessScript, pkScript []byte) {
	t.Helper()
	ws, err := rtypes.DeriveDepositWitnessScript(userID, f.groupPub)
	require.NoError(t, err)
	ps, err := rtypes.DeriveDepositPkScript(userID, f.groupPub)
	require.NoError(t, err)
	require.False(t, bytes.Equal(ps, f.pkScript), "不同 userID 必须派生出不同的 program")
	return ws, ps
}

// signFn 是本夹具的"TSS 组签名"替身：对传入的 sighash 用群私钥出 DER 签名（不带 sighash 字节，
// 与真实 signMsg 的返回口径一致）。注入点与生产同构（psbtSignFunc）。
func (f *p2wshSignFixture) signFn() psbtSignFunc {
	return func(sigHash []byte, _ string) *signResult {
		if len(sigHash) != 32 {
			// 不是 32 字节就不是 BIP143 sighash，直接失败而不是让 ecdsa.Sign 去猜。
			return &signResult{err: fmt.Errorf("unexpected sighash length %d", len(sigHash))}
		}
		return &signResult{sig: ecdsa.Sign(f.groupPriv, sigHash).Serialize()}
	}
}

// newSigningService 构造一个只带群公钥的 tssService：signPsbtWithSigners 只用到它来填
// partial_sigs 的 pubkey，不需要 DB / P2P / DKG。
func (f *p2wshSignFixture) newSigningService(t *testing.T) *tssService {
	t.Helper()
	pub, err := btcec.ParsePubKey(f.groupPub)
	require.NoError(t, err)
	return &tssService{tssPublicKey: pub}
}

// psbtFor 构造一个真机形态的 PSBT：每个输入给出 (outpoint, prevout 脚本, 面额)，
// 每个输入可带自己的 witnessScript（BIP143 逐输入 scriptCode 的载体）。
func psbtFor(t *testing.T, ins []psbtInputSpec, outs []*wire.TxOut) (*psbt.Packet, []byte) {
	t.Helper()
	tx := wire.NewMsgTx(wire.TxVersion)
	for _, in := range ins {
		tx.AddTxIn(wire.NewTxIn(&in.outpoint, nil, nil))
	}
	for _, out := range outs {
		tx.AddTxOut(out)
	}
	p, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)
	for i, in := range ins {
		p.Inputs[i].WitnessUtxo = &wire.TxOut{Value: in.value, PkScript: in.pkScript}
		if len(in.witnessScript) > 0 {
			p.Inputs[i].WitnessScript = in.witnessScript
		}
	}
	// 序列化再解析一遍：证明 witness_script 真的走上了 PSBT 的线格式
	// （PSBT_IN_WITNESS_SCRIPT），而不是只活在内存结构里。
	var buf bytes.Buffer
	require.NoError(t, p.Serialize(&buf))
	roundTripped, err := psbt.NewFromRawBytes(bytes.NewReader(buf.Bytes()), false)
	require.NoError(t, err)
	return roundTripped, buf.Bytes()
}

type psbtInputSpec struct {
	outpoint      wire.OutPoint
	pkScript      []byte
	value         int64
	witnessScript []byte
}

// verifyInputWithEngine 用 txscript 引擎对**该输入**做真实脚本验证（P2WSH 的花费判据）。
func verifyInputWithEngine(t *testing.T, tx *wire.MsgTx, idx int, pkScript []byte, value int64) error {
	t.Helper()
	fetcher := txscript.NewCannedPrevOutputFetcher(pkScript, value)
	sigHashes := txscript.NewTxSigHashes(tx, fetcher)
	vm, err := txscript.NewEngine(pkScript, tx, idx, txscript.StandardVerifyFlags, nil, sigHashes, value, fetcher)
	require.NoError(t, err, "脚本结构本身必须可执行（验签失败与脚本非法是两回事）")
	return vm.Execute()
}

// ---------------------------------------------------------------------------
// 1) 主闭环：单个 P2WSH 输入，走真实签名路径 → finalize → Extract → 引擎验签通过
// ---------------------------------------------------------------------------

func TestSignPsbtWithSigners_p2wshSpendVerifies(t *testing.T) {
	f := newP2WSHSignFixture(t)
	p, _ := psbtFor(t, []psbtInputSpec{{
		outpoint:      wire.OutPoint{Hash: f.fundingTx.TxHash(), Index: 0},
		pkScript:      f.pkScript,
		value:         f.value,
		witnessScript: f.witnessScript,
	}}, []*wire.TxOut{wire.NewTxOut(f.value-1000, f.mainPoolScript)})

	signed, err := f.newSigningService(t).signPsbtWithSigners(p, []string{"self"}, f.signFn())
	require.NoError(t, err, "带 witness_script 的 P2WSH 输入必须能走完整签名 + finalize")

	sp, err := psbt.NewFromRawBytes(bytes.NewReader(signed), false)
	require.NoError(t, err)
	require.NotNil(t, sp.Inputs[0].FinalScriptWitness, "签名路径必须自己 finalize P2WSH（MaybeFinalizeAll 不支持非多签 P2WSH）")
	tx, err := psbt.Extract(sp)
	require.NoError(t, err)

	// witness 栈必须恰好是 [sig||sighashType, witnessScript]：没有 CHECKMULTISIG 的 dummy nil，
	// 也没有 userID（它只是 OP_DROP 标签，不由花费方提供）。
	require.Len(t, tx.TxIn[0].Witness, 2, "witness 栈 = [sig, witnessScript]")
	require.Equal(t, f.witnessScript, tx.TxIn[0].Witness[1], "栈顶必须是 witnessScript 原文")
	require.Equal(t, byte(txscript.SigHashAll), tx.TxIn[0].Witness[0][len(tx.TxIn[0].Witness[0])-1])

	require.NoError(t, verifyInputWithEngine(t, tx, 0, f.pkScript, f.value),
		"用 witnessScript 作 scriptCode 签出的 P2WSH 花费必须通过 txscript 引擎验证")
	t.Logf("PASS: 真实 P2WSH 花费验签通过 pkScript=%s txid=%s", hex.EncodeToString(f.pkScript), tx.TxHash())
}

// ---------------------------------------------------------------------------
// 2) 反向验证：scriptCode 用 output program / 摘掉 witnessScript ⇒ 必失败
// ---------------------------------------------------------------------------

func TestSignPsbtWithSigners_p2wshWrongScriptCodeMustFail(t *testing.T) {
	f := newP2WSHSignFixture(t)
	outs := []*wire.TxOut{wire.NewTxOut(f.value-1000, f.mainPoolScript)}
	op := wire.OutPoint{Hash: f.fundingTx.TxHash(), Index: 0}
	svc := f.newSigningService(t)

	// (a) witnessScript 位置填 **output program**（正是"当前 tss.go 对 P2WPKH 的写法照搬到 P2WSH"
	//     的后果）：签名路径必须拒绝，而不是照着 program 签出一个链上必废的签名。
	pA, _ := psbtFor(t, []psbtInputSpec{{outpoint: op, pkScript: f.pkScript, value: f.value,
		witnessScript: f.pkScript}}, outs)
	_, errA := svc.signPsbtWithSigners(pA, []string{"self"}, f.signFn())
	require.Error(t, errA)
	t.Logf("FAIL(a) witnessScript=output program 被拒: %v", errA)

	// (b) 摘掉 WitnessScript：P2WSH 输入没有 scriptCode 可用，必须拒绝。
	pB, _ := psbtFor(t, []psbtInputSpec{{outpoint: op, pkScript: f.pkScript, value: f.value}}, outs)
	_, errB := svc.signPsbtWithSigners(pB, []string{"self"}, f.signFn())
	require.Error(t, errB)
	t.Logf("FAIL(b) 摘掉 WitnessScript 被拒: %v", errB)

	// (c) 引擎级证据：把签名路径的**旧行为**（scriptCode = prevOut.PkScript）手工复现一遍，
	//     finalize 出广播形态的交易，交给 txscript 引擎 —— 必须验签失败。
	//     这一条才是"钱花不出去"的直接证据（a/b 是签名侧的 fail-closed 拦截）。
	pC, _ := psbtFor(t, []psbtInputSpec{{outpoint: op, pkScript: f.pkScript, value: f.value,
		witnessScript: f.witnessScript}}, outs)
	unsigned := pC.UnsignedTx
	fetcher := txscript.NewCannedPrevOutputFetcher(f.pkScript, f.value)
	sigHashes := txscript.NewTxSigHashes(unsigned, fetcher)
	badHash, err := txscript.CalcWitnessSigHash(f.pkScript, sigHashes, txscript.SigHashAll, unsigned, 0, f.value)
	require.NoError(t, err)
	goodHash, err := txscript.CalcWitnessSigHash(f.witnessScript, sigHashes, txscript.SigHashAll, unsigned, 0, f.value)
	require.NoError(t, err)
	require.False(t, bytes.Equal(goodHash, badHash), "两种 scriptCode 必须算出不同 sighash")

	badTx := unsigned.Copy()
	sig := ecdsa.Sign(f.groupPriv, badHash)
	badTx.TxIn[0].Witness = wire.TxWitness{
		append(sig.Serialize(), byte(txscript.SigHashAll)), f.witnessScript,
	}
	errC := verifyInputWithEngine(t, badTx, 0, f.pkScript, f.value)
	require.Error(t, errC, "用 output program 作 scriptCode 的签名必须验证失败")
	t.Logf("FAIL(c) scriptCode=output program 的签名被引擎拒绝: %v", errC)

	// (d) 绑定校验：witnessScript 与 prevout program 对不上（改一个字节）必须拒绝 ——
	//     否则一份构造过的 PSBT 就能让 TSS 对任意脚本出签名（签名预言机）。
	tampered := append([]byte(nil), f.witnessScript...)
	tampered[len(tampered)-1] ^= 0x01
	pD, _ := psbtFor(t, []psbtInputSpec{{outpoint: op, pkScript: f.pkScript, value: f.value,
		witnessScript: tampered}}, outs)
	_, errD := svc.signPsbtWithSigners(pD, []string{"self"}, f.signFn())
	require.Error(t, errD)
	t.Logf("FAIL(d) witnessScript 与 prevout program 不绑定被拒: %v", errD)
}

// ---------------------------------------------------------------------------
// 3) 多脚本多 sighash：一笔 tx 的两个输入属于**不同** witnessScript
// ---------------------------------------------------------------------------

func TestSignPsbtWithSigners_multiScriptMultiSighash(t *testing.T) {
	f := newP2WSHSignFixture(t)
	// 真实形态：同一把 TSS 群钥，两个用户 ⇒ 两个不同的 witnessScript。
	wsB, pkB := f.deriveExtraScript(t, f.userID+"-second-user")

	// 第二笔"充值"：把钱付到用户 B 的 program。
	fundingB := wire.NewMsgTx(wire.TxVersion)
	fundingB.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.DoubleHashH([]byte("ext-funding-b")), Index: 1}, nil, nil))
	fundingB.AddTxOut(wire.NewTxOut(f.value, pkB))

	ins := []psbtInputSpec{
		{outpoint: wire.OutPoint{Hash: f.fundingTx.TxHash(), Index: 0}, pkScript: f.pkScript,
			value: f.value, witnessScript: f.witnessScript},
		{outpoint: wire.OutPoint{Hash: fundingB.TxHash(), Index: 0}, pkScript: pkB,
			value: f.value, witnessScript: wsB},
	}
	// 两个输入各自带回主池的找零（扫集/提现的真实形态）。
	p, _ := psbtFor(t, ins, []*wire.TxOut{
		wire.NewTxOut(f.value-500, f.mainPoolScript),
		wire.NewTxOut(f.value-500, f.mainPoolScript),
	})

	// 逐输入 sighash 必须互不相同：不同的 scriptCode ⇒ 不同的被签消息。
	// 这正是"签名轮次要能逐输入承载各自 sighash 与脚本"的判据，sessions 也按输入下标区分。
	unsigned := p.UnsignedTx
	fetcher := txscript.NewMultiPrevOutFetcher(map[wire.OutPoint]*wire.TxOut{})
	for _, in := range ins {
		fetcher.AddPrevOut(in.outpoint, &wire.TxOut{Value: in.value, PkScript: in.pkScript})
	}
	sigHashes := txscript.NewTxSigHashes(unsigned, fetcher)
	h0, err := txscript.CalcWitnessSigHash(f.witnessScript, sigHashes, txscript.SigHashAll, unsigned, 0, f.value)
	require.NoError(t, err)
	h1, err := txscript.CalcWitnessSigHash(wsB, sigHashes, txscript.SigHashAll, unsigned, 1, f.value)
	require.NoError(t, err)
	require.False(t, bytes.Equal(h0, h1), "两个不同 witnessScript 的输入必须算出不同 sighash")

	signed, err := f.newSigningService(t).signPsbtWithSigners(p, []string{"self"}, f.signFn())
	require.NoError(t, err)
	sp, err := psbt.NewFromRawBytes(bytes.NewReader(signed), false)
	require.NoError(t, err)
	tx, err := psbt.Extract(sp)
	require.NoError(t, err)

	// 每个输入用自己的 witnessScript 作为栈顶，各自验签都过。
	require.Equal(t, f.witnessScript, tx.TxIn[0].Witness[1])
	require.Equal(t, wsB, tx.TxIn[1].Witness[1])
	require.NoError(t, verifyInputWithEngine(t, tx, 0, f.pkScript, f.value), "用户 A 的输入必须验签通过")
	require.NoError(t, verifyInputWithEngine(t, tx, 1, pkB, f.value), "用户 B 的输入必须验签通过")
	t.Logf("PASS: 两个不同 witnessScript 的输入各自验签通过 txid=%s", tx.TxHash())
}

// ---------------------------------------------------------------------------
// 4) 混合：一笔 tx 里同时有 P2WSH（用户充值）与 P2WPKH（主池）输入
//    这条锁住"自建 finalize 与上游 MaybeFinalizeAll 共存"
// ---------------------------------------------------------------------------

func TestSignPsbtWithSigners_mixedP2WSHAndP2WPKH(t *testing.T) {
	f := newP2WSHSignFixture(t)

	p2wpkhAddr, err := btcutil.NewAddressWitnessPubKeyHash(btcutil.Hash160(f.groupPub), &chaincfg.RegressionNetParams)
	require.NoError(t, err)
	p2wpkhScript, err := txscript.PayToAddrScript(p2wpkhAddr)
	require.NoError(t, err)
	fundingPool := wire.NewMsgTx(wire.TxVersion)
	fundingPool.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.DoubleHashH([]byte("pool-funding")), Index: 0}, nil, nil))
	fundingPool.AddTxOut(wire.NewTxOut(f.value, p2wpkhScript))

	ins := []psbtInputSpec{
		{outpoint: wire.OutPoint{Hash: f.fundingTx.TxHash(), Index: 0}, pkScript: f.pkScript,
			value: f.value, witnessScript: f.witnessScript},
		{outpoint: wire.OutPoint{Hash: fundingPool.TxHash(), Index: 0}, pkScript: p2wpkhScript,
			value: f.value}, // 主池输入：不带 witnessScript，走上游 finalizer
	}
	p, _ := psbtFor(t, ins, []*wire.TxOut{
		wire.NewTxOut(f.value-500, f.mainPoolScript),
		wire.NewTxOut(f.value-500, f.mainPoolScript),
	})

	signed, err := f.newSigningService(t).signPsbtWithSigners(p, []string{"self"}, f.signFn())
	require.NoError(t, err)
	sp, err := psbt.NewFromRawBytes(bytes.NewReader(signed), false)
	require.NoError(t, err)
	tx, err := psbt.Extract(sp)
	require.NoError(t, err)

	require.Len(t, tx.TxIn[0].Witness, 2)
	require.Equal(t, f.witnessScript, tx.TxIn[0].Witness[1], "P2WSH 输入：栈顶是 witnessScript")
	require.Len(t, tx.TxIn[1].Witness, 2)
	require.Equal(t, f.groupPub, tx.TxIn[1].Witness[1], "P2WPKH 输入：上游 finalizer 走 [sig, pubkey]")
	require.NoError(t, verifyInputWithEngine(t, tx, 0, f.pkScript, f.value))
	require.NoError(t, verifyInputWithEngine(t, tx, 1, p2wpkhScript, f.value))
	t.Logf("PASS: 同一笔 tx 里 P2WSH 与 P2WPKH 输入各自 finalize 且都验签通过 txid=%s", tx.TxHash())
}
