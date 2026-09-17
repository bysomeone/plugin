package neutrino

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

/*
 * C4：**扫集**在签名层的真实闭环（与 C3 的提现闭环同构，但输入形态是它的难点）。
 *
 * 一笔扫集要花**多个不同 witnessScript 的输入**（每个用户一个脚本），所以它同时考验三件事：
 *
 *	1. 每个输入必须带**自己的** witnessScript（PSBT_IN_WITNESS_SCRIPT 逐输入）；
 *	2. BIP143 的 scriptCode 逐输入取自己的 witnessScript（统一填主池脚本必废）；
 *	3. 自建 finalize 对每个输入产出 `[sig||sighashType, witnessScript]`。
 *
 * 这里跑完整闭环：真实 P2WSH UTXO → 构造多脚本输入的扫集 PSBT → 走真实签名路径
 * signPsbtWithSigners → finalize → Extract → **每个输入各自**用 txscript.NewEngine 验签。
 * 输出只有一个（回主池），与侧车 build_sweep 的形态一致（`engine.rs` 的
 * `sweep_spends_several_user_scripts_into_the_main_pool`）—— 侧车那边构造、这里签名验签，
 * 两边合起来才是"扫集能上链"的完整证据。
 */

// sweepFixtureInput 一个用户充值脚本作为扫集输入的素材。
func sweepFixtureInput(t *testing.T, f *p2wshSignFixture, userID, seed string, idx uint32, value int64) psbtInputSpec {
	t.Helper()
	ws, pk := f.deriveExtraScript(t, userID)
	return psbtInputSpec{
		outpoint: wire.OutPoint{
			Hash:  chainhash.DoubleHashH([]byte(seed)),
			Index: idx,
		},
		pkScript:      pk,
		value:         value,
		witnessScript: ws,
	}
}

// TestSweepPsbt_SeveralWitnessScriptsSignAndVerify 扫集主闭环：两个不同用户的 P2WSH 输入，
// 一个回主池的输出，签名 → finalize → 逐输入验签。
func TestSweepPsbt_SeveralWitnessScriptsSignAndVerify(t *testing.T) {
	f := newP2WSHSignFixture(t)
	// 主池输入那一份不用；这里两个输入都是**用户充值脚本**（扫集的形态）。
	inA := psbtInputSpec{
		outpoint:      wire.OutPoint{Hash: f.fundingTx.TxHash(), Index: 0},
		pkScript:      f.pkScript,
		value:         f.value,
		witnessScript: f.witnessScript,
	}
	inB := sweepFixtureInput(t, f, "1KSBd17H7ZK8iT37aJztFB22XGwsPTdwE4", "sweep-input-b", 1, 42_000)
	require.False(t, bytes.Equal(inA.witnessScript, inB.witnessScript), "两个不同用户必须各有各的 witnessScript")
	require.False(t, bytes.Equal(inA.pkScript, inB.pkScript))

	total := inA.value + inB.value
	p, _ := psbtFor(t, []psbtInputSpec{inA, inB},
		[]*wire.TxOut{wire.NewTxOut(total-1500, f.mainPoolScript)})

	signed, err := f.newSigningService(t).signPsbtWithSigners(p, []string{"self"}, f.signFn())
	require.NoError(t, err, "多脚本输入的扫集必须能走完整签名 + finalize")

	sp, err := psbt.NewFromRawBytes(bytes.NewReader(signed), false)
	require.NoError(t, err)
	require.Len(t, sp.Inputs, 2)
	require.NotNil(t, sp.Inputs[0].FinalScriptWitness)
	require.NotNil(t, sp.Inputs[1].FinalScriptWitness)

	tx, err := psbt.Extract(sp)
	require.NoError(t, err)

	// 每个输入的 witness 栈必须是它**自己的** `[sig||sighashType, witnessScript]`：
	// 这是"一笔交易里多个脚本各自成签名轮次"的直接证据（把 A 的脚本塞进 B 的输入必废）。
	for i, want := range []psbtInputSpec{inA, inB} {
		require.Len(t, tx.TxIn[i].Witness, 2, "input %d: witness 栈 = [sig, witnessScript]", i)
		require.Equal(t, want.witnessScript, tx.TxIn[i].Witness[1],
			"input %d 的栈顶必须是它自己的 witnessScript", i)
		require.Equal(t, byte(txscript.SigHashAll), tx.TxIn[i].Witness[0][len(tx.TxIn[i].Witness[0])-1])
	}
	require.NoError(t, verifyInputWithEngine(t, tx, 0, inA.pkScript, inA.value),
		"扫集输入 0（用户 A 的充值脚本）必须验签通过")
	require.NoError(t, verifyInputWithEngine(t, tx, 1, inB.pkScript, inB.value),
		"扫集输入 1（用户 B 的充值脚本）必须验签通过")
	t.Logf("PASS: 扫集 %d 个输入各自验签通过 txid=%s inputs=%s,%s",
		len(tx.TxIn), tx.TxHash(),
		hex.EncodeToString(inA.pkScript), hex.EncodeToString(inB.pkScript))
}

// TestSweepPsbt_MismatchedWitnessScriptMustFail 反向验证：把两个输入的 witnessScript 对调
// （= "每个输入带自己的 scriptCode"这条判据被改坏），签名路径必须拒 —— 而不是照着别人的脚本
// 出签名，签出一笔链上必废的交易。
func TestSweepPsbt_MismatchedWitnessScriptMustFail(t *testing.T) {
	f := newP2WSHSignFixture(t)
	inA := psbtInputSpec{
		outpoint:      wire.OutPoint{Hash: f.fundingTx.TxHash(), Index: 0},
		pkScript:      f.pkScript,
		value:         f.value,
		witnessScript: f.witnessScript,
	}
	inB := sweepFixtureInput(t, f, "1KSBd17H7ZK8iT37aJztFB22XGwsPTdwE4", "sweep-input-b", 1, 42_000)

	// 对调 witnessScript：A 的输入拿 B 的脚本。sha256(witnessScript) != prevout program，
	// resolvePsbtInputScriptCode 的绑定校验必须拦住。
	swappedA, swappedB := inA, inB
	swappedA.witnessScript, swappedB.witnessScript = inB.witnessScript, inA.witnessScript
	total := inA.value + inB.value
	p, _ := psbtFor(t, []psbtInputSpec{swappedA, swappedB},
		[]*wire.TxOut{wire.NewTxOut(total-1500, f.mainPoolScript)})

	_, err := f.newSigningService(t).signPsbtWithSigners(p, []string{"self"}, f.signFn())
	require.Error(t, err, "witnessScript 与 prevout program 不绑定时必须拒签")
	require.Contains(t, err.Error(), "does not hash to the prevout program")
	t.Logf("FAIL(对调 witnessScript 被拒): %v", err)

	// 引擎级证据：直接按对调后的脚本签，链上必被 OP_CHECKSIG 拒（"签名必废"的直接证明）。
	good := signedFor(t, f, p)
	tx, err := psbt.Extract(good)
	require.NoError(t, err)
	err = verifyInputWithEngine(t, tx, 0, inA.pkScript, inA.value)
	require.Error(t, err, "用别人的 witnessScript 签出来的输入不可能通过验签")
	t.Logf("FAIL(对调后的输入验签失败): %v", err)
}

// signedFor 是上面那条引擎级证据的辅助：手工按 PSBT 里的 witnessScript 出 BIP143 签名并填进
// witness（绕过签名路径的 fail-closed 拦截），得到一个"签名路径若放行就会产出"的广播形态交易。
func signedFor(t *testing.T, f *p2wshSignFixture, p *psbt.Packet) *psbt.Packet {
	t.Helper()
	// 深拷贝一份，不动调用方手里的 PSBT（witness 是逐输入写的，共用底层会互相污染）。
	var buf0 bytes.Buffer
	require.NoError(t, p.Serialize(&buf0))
	out, err := psbt.NewFromRawBytes(bytes.NewReader(buf0.Bytes()), false)
	require.NoError(t, err)
	fetcher := txscript.NewMultiPrevOutFetcher(nil)
	for i := range out.Inputs {
		fetcher.AddPrevOut(out.UnsignedTx.TxIn[i].PreviousOutPoint, out.Inputs[i].WitnessUtxo)
	}
	sigHashes := txscript.NewTxSigHashes(out.UnsignedTx, fetcher)
	for i := range out.Inputs {
		ws := out.Inputs[i].WitnessScript
		hash, err := txscript.CalcWitnessSigHash(ws, sigHashes, txscript.SigHashAll,
			out.UnsignedTx, i, out.Inputs[i].WitnessUtxo.Value)
		require.NoError(t, err)
		res := f.signFn()(hash, "")
		require.NoError(t, res.err)
		withType := append(append([]byte{}, res.sig...), byte(txscript.SigHashAll))
		var buf bytes.Buffer
		require.NoError(t, psbt.WriteTxWitness(&buf, wire.TxWitness{withType, ws}))
		out.Inputs[i].FinalScriptWitness = buf.Bytes()
	}
	return out
}
