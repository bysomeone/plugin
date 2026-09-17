package types

import (
	"bytes"
	"crypto/sha256"
	"errors"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
)

/*
 * P2WSH 每用户 BTC 充值地址 —— 派生规格 v1（冻结，字节级）
 *
 * 规格来源：C0 冻结草案 /Users/jiangpeng/.claude/plans/p2wsh-derivation-spec.md §1。
 * 本文件是该规格在 Go 侧的**唯一实现**：桥（lightclient/neutrino）与合约（rgbx/executor）
 * 都必须调用这里的函数，不允许各自再写一份 —— 三份实现（Go 桥 / Go 合约 / Rust 侧车）
 * 对同一组 (userID, tssPub) 必须得到逐字节相同的 witnessScript / program / 地址。
 * 冻结的测试向量见 testdata/p2wsh_deposit_vectors.json（由 p2wsh_deposit_test.go 校验）。
 *
 *	witnessScript := push(userID) || OP_DROP || push(tssPub) || OP_CHECKSIG
 *	                 ^^^^^^^^^^^   ^^^^^^^   ^^^^^^^^^^^^^   ^^^^^^^^^^^
 *	                 最小 push      0x75      最小 push        0xac
 *	program       := SHA256(witnessScript)      // 单次 sha256，32 字节（不是 hash160）
 *	pkScript      := OP_0 || push(program)      // witness v0，34 字节
 *	address       := bech32(BIP173, witver=0, hrp(网络) || program)
 *
 * 语义要点：
 *   - userID 是 chain33 充值地址串的**原始字节**（禁止 ToUpper/Trim/hex 解码/前缀剥离），
 *     它在脚本里只被 push 后立刻 OP_DROP，是**纯标签**：花费时 witness 栈只需
 *     [sig, witnessScript]，userID 不需要由花费方提供。它只影响 program，
 *     不影响花费条件（OP_CHECKSIG 只消费栈顶两项，DROP 后的残留不参与判定）。
 *   - 因此地址是 (chain33 地址, TSS 群公钥) 的确定性函数，**执行器侧无需 bech32、
 *     无需网络参数**即可重建（只比 program/pkScript，不比地址串）。
 *   - tssPub 一变（re-keygen），所有用户地址全废 —— 这是"上线后禁止 re-DKG、
 *     只许 refresh"硬约束的由来（见 TECHNICAL.md）。
 */

const (
	// P2WSHDepositSpecV1 派生规格的版本标签（**只存在于链下**：脚本里不写版本字节，
	// 见 C0 §6⑥-A；版本信息由 registry / 查询返回值携带）。
	P2WSHDepositSpecV1 = "p2wsh-deposit-v1"

	// MaxDepositUserIDLen userID（chain33 地址串原始字节）的最大长度。
	// 上限 75 = 最小 push 编码（OP_PUSHBYTES_n，n ∈ [1,75]）的分界：
	// 超过 75 字节就必须用 OP_PUSHDATA1，规格未定义、拒绝。
	MaxDepositUserIDLen = 75

	// depositTssPubKeyLen 压缩 secp256k1 公钥长度。只接受压缩格式：
	// 非压缩（65 字节）会让脚本与地址随提交格式漂移，规格明确禁止。
	depositTssPubKeyLen = 33
)

var (
	// ErrInvalidDepositUserID userID 为空或超过最小 push 的长度上限。
	ErrInvalidDepositUserID = errors.New("invalid deposit user id")
	// ErrInvalidTssPubKey tssPub 不是 33 字节压缩 secp256k1 公钥。
	ErrInvalidTssPubKey = errors.New("invalid tss pubkey")
)

// ParseDepositTssPubKey 校验并规范化 TSS 群公钥：**只接受 33 字节压缩格式**。
//
// 契约（C0 §1.1）：进入脚本的必须是 btcec.ParsePubKey 后 SerializeCompressed() 的结果。
// 这里不接受 65 字节非压缩输入（不做"帮你压缩"的兼容）—— 同一条公钥的两种编码会产生
// 两个不同地址，谁负责规范化必须唯一，否则桥发的地址与执行器重建的地址会静默错开，
// 用户的 BTC 会打进无人认领的脚本。非压缩在 CommitDKG 阶段就被拒（ErrInvalidDkgAddress）。
func ParseDepositTssPubKey(pub []byte) ([]byte, error) {
	if len(pub) != depositTssPubKeyLen {
		return nil, ErrInvalidTssPubKey
	}
	parsed, err := btcec.ParsePubKey(pub)
	if err != nil {
		return nil, ErrInvalidTssPubKey
	}
	compressed := parsed.SerializeCompressed()
	if !bytes.Equal(compressed, pub) {
		return nil, ErrInvalidTssPubKey
	}
	return compressed, nil
}

// appendCanonicalPush 追加一个**最小 push 编码**的数据（规格 §1.2）。
//
// 为什么不用 txscript.NewScriptBuilder().AddData()：AddData 会把"长度为 1 且值 ≤ 16"的数据
// 编码成 OP_1..OP_16（见 btcd txscript/scriptbuilder.go 的 addData），而 Rust 侧
// bitcoin::script::Builder::push_slice 是原样 push。两者对 1 字节 userID 会给出**不同的
// witnessScript**（进而不同地址），而规格 §1.2 要求的就是"≤75 字节一律 OP_PUSHBYTES_n"。
// 所以这里按规格直接写长度前缀：0x00 即 OP_0（空 push），0x01..0x4b 即 OP_PUSHBYTES_1..75。
// 超过 75 字节需要 OP_PUSHDATA1 的形态规格未定义，直接拒绝，不做 PUSHDATA 分支。
func appendCanonicalPush(script, data []byte) ([]byte, error) {
	if len(data) > MaxDepositUserIDLen {
		return nil, ErrInvalidDepositUserID
	}
	script = append(script, byte(len(data)))
	return append(script, data...), nil
}

// DeriveDepositWitnessScript 按冻结规格构造 witnessScript（逐字节）。
//
// userID 必须是 chain33 充值地址串的原始字节（可审计：链上脚本可读可对账），
// 长度 ∈ [1, MaxDepositUserIDLen]；tssPub 必须是 33 字节压缩公钥。
//
// 该脚本同时是 P2WSH 的 scriptCode：BIP143 计算 sighash 必须传 witnessScript 本身，
// **不是** output program（见 p2wsh_deposit_spend_test.go 的可复现证明）。
func DeriveDepositWitnessScript(userID string, tssPub []byte) ([]byte, error) {
	if len(userID) == 0 || len(userID) > MaxDepositUserIDLen {
		return nil, ErrInvalidDepositUserID
	}
	pub, err := ParseDepositTssPubKey(tssPub)
	if err != nil {
		return nil, err
	}
	script, err := appendCanonicalPush(nil, []byte(userID))
	if err != nil {
		return nil, err
	}
	script = append(script, txscript.OP_DROP)
	if script, err = appendCanonicalPush(script, pub); err != nil {
		return nil, err
	}
	return append(script, txscript.OP_CHECKSIG), nil
}

// DeriveDepositPkScript 派生用户充值地址的 pkScript（= witness v0 program）。
//
// pkScript := OP_0 || push(SHA256(witnessScript))，34 字节。
// 执行器只比这个值（不解析地址串、不需要网络参数），比"比地址串"少一层编解码歧义。
func DeriveDepositPkScript(userID string, tssPub []byte) ([]byte, error) {
	witnessScript, err := DeriveDepositWitnessScript(userID, tssPub)
	if err != nil {
		return nil, err
	}
	program := sha256.Sum256(witnessScript)
	return append([]byte{txscript.OP_0, byte(len(program))}, program[:]...), nil
}

// DeriveDepositAddress 派生用户在指定网络上的 bech32 充值地址。
//
// 只有**发放地址 / 展示**需要它（桥侧、测试向量）；执行器一律只比 pkScript。
// params 决定 hrp（bc / tb / bcrt / sb），与 lighttypes.GetBtcChainParams 同源。
func DeriveDepositAddress(userID string, tssPub []byte, params *chaincfg.Params) (string, error) {
	if params == nil {
		return "", ErrInvalidTssPubKey
	}
	witnessScript, err := DeriveDepositWitnessScript(userID, tssPub)
	if err != nil {
		return "", err
	}
	program := sha256.Sum256(witnessScript)
	addr, err := btcutil.NewAddressWitnessScriptHash(program[:], params)
	if err != nil {
		return "", err
	}
	return addr.String(), nil
}

// IsDepositPkScript 判断某个输出脚本是否就是 (userID, tssPub) 对应的充值 program。
func IsDepositPkScript(pkScript []byte, userID string, tssPub []byte) bool {
	expect, err := DeriveDepositPkScript(userID, tssPub)
	if err != nil {
		return false
	}
	return bytes.Equal(pkScript, expect)
}

// IsNativeP2WSHScript 判断脚本是否为 native P2WSH（witness v0，32 字节 program）。
//
// 用途一（E15-a）：合约侧**禁止 native P2WSH 作为提现目标** —— 桥的提现付款若落到
// native P2WSH，该笔付款可被收款人回头当作充值证明（见 validate_proof.go 的不变式注释）。
// 用途二（C2/C3）：桥侧自检"自有付款没有落到任何用户 P2WSH"。
// 嵌套 P2SH-P2WSH 的输出脚本是 P2SH，不会被误判。
func IsNativeP2WSHScript(pkScript []byte) bool {
	witVer, program, err := txscript.ExtractWitnessProgramInfo(pkScript)
	if err != nil {
		return false
	}
	return witVer == 0 && len(program) == 32
}
