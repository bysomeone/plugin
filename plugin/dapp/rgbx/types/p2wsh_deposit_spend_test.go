package types

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

/*
 * P2WSH 花费路径的可复现证明（E16 的核心：把"BIP143 的 scriptCode 必须是 witnessScript"
 * 这条 C 波硬前提从文档记述变成能跑的工件）。
 *
 * 结论（本文件逐条断言）：
 *  1. 用 (userID, tssPub) 派生出的 P2WSH program 收到的钱，**可以被 TSS 私钥花掉**——
 *     witness 栈只有 [sig, witnessScript]，userID 不由花费方提供（它只是 OP_DROP 标签）；
 *  2. sighash 必须用 **witnessScript** 作为 scriptCode 计算：这样签出的签名能通过脚本验证；
 *  3. 用 **output program**（= pkScript，即当前 tss.go 对 P2WPKH 的写法照搬到 P2WSH 的后果）
 *     作为 scriptCode 算出的 sighash，签名**必然验证失败** —— 两者不是同一个字节串。
 *
 * 复现前提：本文件不依赖任何外部节点，纯 txscript 引擎 + BIP143 签名。
 */

// p2wshSpendFixture 构造一笔"外部用户充值到该用户 P2WSH 地址"的交易，再构造花费它的一笔交易。
type p2wshSpendFixture struct {
	userID        string
	tssPub        []byte
	tssPriv       *btcec.PrivateKey
	witnessScript []byte
	pkScript      []byte
	depositTx     *wire.MsgTx
	prevOutIdx    uint32
	amount        int64
}

func newP2WSHSpendFixture(t *testing.T) *p2wshSpendFixture {
	t.Helper()
	doc := loadDepositVectors(t)
	f := &p2wshSpendFixture{}
	for _, v := range doc.Vectors {
		if v.Name == "v1-chain33-address" {
			f.userID = v.UserID
			var err error
			f.tssPub, err = hex.DecodeString(v.TssPubKey)
			require.NoError(t, err)
			privBytes, err := hex.DecodeString(v.TssPrivKey)
			require.NoError(t, err)
			f.tssPriv, _ = btcec.PrivKeyFromBytes(privBytes)
			require.Equal(t, f.tssPub, f.tssPriv.PubKey().SerializeCompressed())
		}
	}
	require.NotEmpty(t, f.userID, "主向量必须存在")

	var err error
	f.witnessScript, err = DeriveDepositWitnessScript(f.userID, f.tssPub)
	require.NoError(t, err)
	f.pkScript, err = DeriveDepositPkScript(f.userID, f.tssPub)
	require.NoError(t, err)

	// 充值交易：外部付款方付 100000 sat 到该用户的 P2WSH program（链上能看到的只有这个 34 字节脚本）。
	f.amount = 100000
	f.prevOutIdx = 0
	f.depositTx = wire.NewMsgTx(wire.TxVersion)
	f.depositTx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.DoubleHashH([]byte("ext-funding")), Index: 1}, nil, nil))
	f.depositTx.AddTxOut(wire.NewTxOut(f.amount, f.pkScript))
	return f
}

// spendTx 构造花费"充值交易第 idx 个输出"的交易（扫集/提现的形态：一个输入、一个回主池的输出）。
func (f *p2wshSpendFixture) spendTx(outIdx uint32) (*wire.MsgTx, *txscript.CannedPrevOutputFetcher, *txscript.TxSigHashes) {
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: f.depositTx.TxHash(), Index: outIdx}, nil, nil))
	tx.AddTxOut(wire.NewTxOut(f.amount-1000, []byte{0x00, 0x14, 0x11, 0x22})) // 找零回主池形态（内容无所谓）
	fetcher := txscript.NewCannedPrevOutputFetcher(f.pkScript, f.amount)
	return tx, fetcher, txscript.NewTxSigHashes(tx, fetcher)
}

// signInput 按给定 scriptCode 算 BIP143 sighash 并签出 witness。
func (f *p2wshSpendFixture) signInput(t *testing.T, tx *wire.MsgTx, sigHashes *txscript.TxSigHashes, inIdx int, scriptCode []byte) {
	t.Helper()
	sigHash, err := txscript.CalcWitnessSigHash(scriptCode, sigHashes, txscript.SigHashAll, tx, inIdx, f.amount)
	require.NoError(t, err)
	sig := ecdsa.Sign(f.tssPriv, sigHash)
	sigBytes := append(sig.Serialize(), byte(txscript.SigHashAll))
	// 关键：witness 栈只有 [sig, witnessScript]，没有 CHECKMULTISIG 的 dummy、没有 userID
	tx.TxIn[inIdx].Witness = wire.TxWitness{sigBytes, f.witnessScript}
}

func (f *p2wshSpendFixture) executeEngine(t *testing.T, tx *wire.MsgTx, fetcher *txscript.CannedPrevOutputFetcher, sigHashes *txscript.TxSigHashes) error {
	t.Helper()
	vm, err := txscript.NewEngine(f.pkScript, tx, 0, txscript.StandardVerifyFlags, nil, sigHashes, f.amount, fetcher)
	require.NoError(t, err)
	return vm.Execute()
}

// TestP2WSHSpend_scriptCodeMustBeWitnessScript E16 主断言：
// 用 witnessScript 算 sighash → 通过；用 output program 算 → 失败。
func TestP2WSHSpend_scriptCodeMustBeWitnessScript(t *testing.T) {
	f := newP2WSHSpendFixture(t)

	// 1) scriptCode = witnessScript（正确）
	tx, fetcher, sigHashes := f.spendTx(f.prevOutIdx)
	f.signInput(t, tx, sigHashes, 0, f.witnessScript)
	require.NoError(t, f.executeEngine(t, tx, fetcher, sigHashes),
		"用 witnessScript 作 scriptCode 的签名必须通过 P2WSH 脚本验证")

	// 2) scriptCode = output program / pkScript（错误：当前 tss.go 对 P2WPKH 的写法直接照搬到 P2WSH）
	badTx, badFetcher, badSigHashes := f.spendTx(f.prevOutIdx)
	f.signInput(t, badTx, badSigHashes, 0, f.pkScript)
	err := f.executeEngine(t, badTx, badFetcher, badSigHashes)
	require.Error(t, err, "用 output program 作 scriptCode 的签名必须验证失败")
	require.Contains(t, err.Error(), "signature", "失败原因应当是签名不匹配（而非脚本结构错误）：%v", err)

	// 3) 两种 scriptCode 确实会算出不同 sighash（不是"碰巧还通过"）
	goodHash, err := txscript.CalcWitnessSigHash(f.witnessScript, sigHashes, txscript.SigHashAll, tx, 0, f.amount)
	require.NoError(t, err)
	badHash, err := txscript.CalcWitnessSigHash(f.pkScript, sigHashes, txscript.SigHashAll, tx, 0, f.amount)
	require.NoError(t, err)
	require.False(t, bytes.Equal(goodHash, badHash), "witnessScript 与 output program 必须是不同的 scriptCode")

	// 4) 两种"看着像但不对"的 scriptCode 同样失败：
	//    (a) 自己再加一遍 varint 长度前缀（BIP143 的 scriptCode 由 btcd 在 CalcWitnessSigHash 内写 varint，
	//        调用方再包一层就变成"push 整个脚本"，语义完全不同）；
	//    (b) 少掉尾部 OP_CHECKSIG 的脚本。
	varintPrefixed := append([]byte{byte(len(f.witnessScript))}, f.witnessScript...)
	require.NotEqual(t, f.witnessScript, varintPrefixed)
	wrongCodes := map[string][]byte{
		"手动再加 varint 前缀":  varintPrefixed,
		"缺尾部 OP_CHECKSIG": f.witnessScript[:len(f.witnessScript)-1],
	}
	for name, wrong := range wrongCodes {
		badTx2, badFetcher2, badSigHashes2 := f.spendTx(f.prevOutIdx)
		f.signInput(t, badTx2, badSigHashes2, 0, wrong)
		require.Error(t, f.executeEngine(t, badTx2, badFetcher2, badSigHashes2), "scriptCode %s 必须失败", name)
	}

	// 5) 正面锚点：真正被采用的那份 scriptCode 就是 witnessScript 原文（调用方不再自己加工）。
	require.Equal(t, byte(0x22), f.witnessScript[0], "scriptCode 首字节是 userID 的 push 前缀，不是 varint")
}

// TestP2WSHSpend_userIDIsALabelOnly userID 只是标签：
//   - 花费时**不需要**提供 userID（witness 栈里没有它）；
//   - 但换个 userID 派生出的 program 就收不到这笔钱（脚本不同 → 地址不同）。
func TestP2WSHSpend_userIDIsALabelOnly(t *testing.T) {
	f := newP2WSHSpendFixture(t)

	// 栈上只有 [sig, witnessScript]：多塞一个 userID 会被 CleanStack 拒（脚本执行完栈上不止一项）
	tx, fetcher, sigHashes := f.spendTx(f.prevOutIdx)
	f.signInput(t, tx, sigHashes, 0, f.witnessScript)
	tx.TxIn[0].Witness = append(tx.TxIn[0].Witness, []byte(f.userID))
	require.Error(t, f.executeEngine(t, tx, fetcher, sigHashes),
		"userID 不应由花费方提供；多塞一项会破坏 CleanStack")

	// 同一把 TSS 钥、不同 userID → 不同 program（OP_DROP 的标签确实进了哈希）
	otherScript, err := DeriveDepositPkScript(f.userID+"x", f.tssPub)
	require.NoError(t, err)
	require.False(t, bytes.Equal(otherScript, f.pkScript))

	// 用"另一个用户"的 program 构造的输出，花不到：签名者的钥虽然相同，但脚本哈希不匹配。
	otherTx := wire.NewMsgTx(wire.TxVersion)
	otherTx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: f.depositTx.TxHash(), Index: f.prevOutIdx}, nil, nil))
	otherTx.AddTxOut(wire.NewTxOut(f.amount-1000, []byte{0x00, 0x14, 0x11, 0x22}))
	otherFetcher := txscript.NewCannedPrevOutputFetcher(otherScript, f.amount)
	otherSigs := txscript.NewTxSigHashes(otherTx, otherFetcher)
	// 用 witnessScript（而不是 otherScript）作 scriptCode 签名 → 与 otherScript 对应的 program 不符
	f.signInput(t, otherTx, otherSigs, 0, f.witnessScript)
	vm, err := txscript.NewEngine(otherScript, otherTx, 0, txscript.StandardVerifyFlags, nil, otherSigs, f.amount, otherFetcher)
	require.NoError(t, err)
	require.Error(t, vm.Execute(), "witnessScript 的哈希必须等于 output program，否则拒绝")
}
