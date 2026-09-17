package types

import (
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/stretchr/testify/require"
)

/*
 * P2WSH 充值地址派生规格 v1 的**冻结测试向量**（E16：把 C 波的前提做成可复现工件）。
 *
 * 向量文件 testdata/p2wsh_deposit_vectors.json 是三方（Go 桥 / Go 合约 / Rust 侧车）
 * 共用的同一组数据：谁改了派生实现，这里的重算比对就会变红；Rust 侧应当读同一个文件
 * 跑一遍同样的断言。向量值本身已用**独立实现**复核过（Python hashlib 的 sha256 +
 * 手写 BIP173 bech32 编码，见下 TestP2WSHDepositVectors_primaryLiterals 的注释）。
 *
 * 覆盖：
 *   - (userID, tssPub) → witnessScript → program → pkScript → 各网络地址 全链路重算；
 *   - 主向量另有 Go 字面量硬编码（防止"改坏实现后顺手重生成 JSON"）；
 *   - 地址形态复核：能解回同一个 program、是 witness v0、hrp 与网络一致；
 *   - 边界/拒绝：userID 空或超长、tssPub 非压缩/非 33 字节。
 */

//go:embed testdata/p2wsh_deposit_vectors.json
var p2wshDepositVectorsJSON []byte

type p2wshVector struct {
	Name          string            `json:"name"`
	Note          string            `json:"note"`
	UserID        string            `json:"userID"`
	TssPubKey     string            `json:"tssPubKey"`
	TssPrivKey    string            `json:"tssPrivKey"`
	WitnessScript string            `json:"witnessScript"`
	Program       string            `json:"programSHA256"`
	PkScript      string            `json:"pkScript"`
	Addresses     map[string]string `json:"addresses"`
	WitnessLen    int               `json:"witnessScriptLen"`
}

type p2wshVectorDoc struct {
	Spec    string        `json:"spec"`
	Vectors []p2wshVector `json:"vectors"`
}

// depositVectorParams 各网络的 BTC 参数（与 lighttypes.GetBtcChainParams 同源）。
func depositVectorParams() map[string]*chaincfg.Params {
	return map[string]*chaincfg.Params{
		"mainnet":  &chaincfg.MainNetParams,
		"testnet3": &chaincfg.TestNet3Params,
		"regtest":  &chaincfg.RegressionNetParams,
		"signet":   &chaincfg.SigNetParams,
		"simnet":   &chaincfg.SimNetParams,
	}
}

func loadDepositVectors(t *testing.T) p2wshVectorDoc {
	t.Helper()
	var doc p2wshVectorDoc
	require.NoError(t, json.Unmarshal(p2wshDepositVectorsJSON, &doc))
	require.Equal(t, P2WSHDepositSpecV1, doc.Spec)
	return doc
}

// TestP2WSHDepositVectors 逐向量重算派生结果，与冻结值逐字节比对。
func TestP2WSHDepositVectors(t *testing.T) {
	doc := loadDepositVectors(t)
	require.Equal(t, []string{
		"v1-chain33-address",
		"v2-second-chain33-address",
		"v3-userID-max-len-75",
		"v4-userID-len-1-value-5",
	}, vectorNames(doc), "向量集变化必须是有意为之（三方共用，删/加向量要同步 Rust 侧）")

	params := depositVectorParams()
	for _, v := range doc.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			pub, err := hex.DecodeString(v.TssPubKey)
			require.NoError(t, err)

			witnessScript, err := DeriveDepositWitnessScript(v.UserID, pub)
			require.NoError(t, err)
			require.Equal(t, v.WitnessScript, hex.EncodeToString(witnessScript), "witnessScript 必须逐字节一致")
			require.Equal(t, v.WitnessLen, len(witnessScript))
			// 规格 §1.2 的结构断言：push(userID) || OP_DROP || push(tssPub) || OP_CHECKSIG
			require.Equal(t, byte(len(v.UserID)), witnessScript[0], "userID 必须是最小 push 前缀")
			require.Equal(t, byte(0x75), witnessScript[len(v.UserID)+1], "OP_DROP")
			require.Equal(t, byte(len(pub)), witnessScript[len(v.UserID)+2], "tssPub 必须是最小 push 前缀")
			require.Equal(t, byte(0xac), witnessScript[len(witnessScript)-1], "OP_CHECKSIG")

			pkScript, err := DeriveDepositPkScript(v.UserID, pub)
			require.NoError(t, err)
			require.Equal(t, v.PkScript, hex.EncodeToString(pkScript))
			require.Equal(t, "0020"+v.Program, v.PkScript, "pkScript = OP_0 || push32(sha256(witnessScript))")
			require.Len(t, pkScript, 34)

			for net, expectAddr := range v.Addresses {
				p, ok := params[net]
				require.True(t, ok, "未知网络 %s", net)
				addr, err := DeriveDepositAddress(v.UserID, pub, p)
				require.NoError(t, err)
				require.Equal(t, expectAddr, addr, "网络 %s 的地址必须一致", net)
				require.True(t, strings.HasPrefix(addr, p.Bech32HRPSegwit+"1"), "hrp 必须与网络一致")

				// 地址形态复核：解回来必须是同一个 program、witness v0。
				decoded, err := btcutil.DecodeAddress(addr, p)
				require.NoError(t, err)
				back, err := txscript.PayToAddrScript(decoded)
				require.NoError(t, err)
				require.Equal(t, pkScript, back, "地址必须解回同一个 P2WSH program")
				require.True(t, IsNativeP2WSHScript(back))
			}

			require.True(t, IsDepositPkScript(pkScript, v.UserID, pub))
			require.False(t, IsDepositPkScript(pkScript, v.UserID+"x", pub), "userID 变一字节即不匹配")
		})
	}
}

// TestP2WSHDepositVectors_primaryLiterals 主向量的**字面量**硬编码。
//
// 这些值不是从实现里生成的，而是用独立实现逐项复核过：
//   - witnessScript 的字节按规格手工展开（0x22 + "1AmRYcURfDGxBhiaJAvEGdRkvkoM7ztn1u" 的
//     ASCII + 0x75 + 0x21 + 生成元压缩公钥 + 0xac）；
//   - programSHA256 由 python3 hashlib.sha256(witnessScript) 独立算出；
//   - 地址由一份从零手写的 BIP173 bech32（polymod/convertbits）独立编码算出，与 btcd 的输出一致。
//
// 有这组字面量，任何"改了实现再重生成 JSON"的改动都会在这里被拦住。
func TestP2WSHDepositVectors_primaryLiterals(t *testing.T) {
	const (
		userID        = "1AmRYcURfDGxBhiaJAvEGdRkvkoM7ztn1u"
		tssPubHex     = "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798" // privkey = 1
		witnessScript = "2231416d525963555266444778426869614a4176454764526b766b6f4d377a746e317575" +
			"210279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798ac"
		program  = "78d9392c30b67443d288c548adde74e0eff45454048cf3e7cc2f82f43da70465"
		pkScript = "0020" + program
		// 各网络地址（bech32 v0；signet 与 testnet3 同为 tb 前缀）
		addrMainnet  = "bc1q0rvnjtpske6y855gc4y2mhn5urhlg4z5qjx08e7v97p0g0d8q3jsepkfqp"
		addrTestnet3 = "tb1q0rvnjtpske6y855gc4y2mhn5urhlg4z5qjx08e7v97p0g0d8q3jswfqx6w"
		addrRegtest  = "bcrt1q0rvnjtpske6y855gc4y2mhn5urhlg4z5qjx08e7v97p0g0d8q3jsrs2q05"
		addrSimnet   = "sb1q0rvnjtpske6y855gc4y2mhn5urhlg4z5qjx08e7v97p0g0d8q3jsfc2prt"
	)
	pub, err := hex.DecodeString(tssPubHex)
	require.NoError(t, err)

	ws, err := DeriveDepositWitnessScript(userID, pub)
	require.NoError(t, err)
	require.Equal(t, witnessScript, hex.EncodeToString(ws))
	require.Len(t, ws, 71, "34 字节地址 → 35 + 1 + 34 + 1 = 71 字节")

	pks, err := DeriveDepositPkScript(userID, pub)
	require.NoError(t, err)
	require.Equal(t, pkScript, hex.EncodeToString(pks))

	cases := []struct {
		net  string
		want string
	}{
		{"mainnet", addrMainnet},
		{"testnet3", addrTestnet3},
		{"regtest", addrRegtest},
		{"simnet", addrSimnet},
	}
	params := depositVectorParams()
	for _, c := range cases {
		addr, err := DeriveDepositAddress(userID, pub, params[c.net])
		require.NoError(t, err)
		require.Equal(t, c.want, addr, c.net)
	}
}

// TestAppendCanonicalPush_noSmallIntOpcode 单字节数据必须编码成 0x01 <val>，
// **不得**走 OP_1..OP_16 特例：Rust bitcoin::script::Builder::push_slice 是原样 push，
// 若 Go 侧用 txscript.ScriptBuilder.AddData，1 字节 userID 会得到不同脚本（进而不同地址）。
func TestAppendCanonicalPush_noSmallIntOpcode(t *testing.T) {
	for v := byte(1); v <= 16; v++ {
		got, err := appendCanonicalPush(nil, []byte{v})
		require.NoError(t, err)
		require.Equal(t, []byte{0x01, v}, got, "0x%02x 必须编码为 0x01 0x%02x", v, v)
	}
	empty, err := appendCanonicalPush(nil, nil)
	require.NoError(t, err)
	require.Equal(t, []byte{0x00}, empty)
	// 边界：75 字节用 OP_PUSHBYTES_75；76 字节规格未定义 → 拒绝
	max, err := appendCanonicalPush(nil, make([]byte, MaxDepositUserIDLen))
	require.NoError(t, err)
	require.Equal(t, byte(75), max[0])
	_, err = appendCanonicalPush(nil, make([]byte, MaxDepositUserIDLen+1))
	require.Equal(t, ErrInvalidDepositUserID, err)

	// v4 向量就是这个特例的固化（0x05 → 0x0105，而不是 0x55 = OP_5）
	doc := loadDepositVectors(t)
	for _, vt := range doc.Vectors {
		if vt.Name != "v4-userID-len-1-value-5" {
			continue
		}
		require.True(t, strings.HasPrefix(vt.WitnessScript, "010575"), "v4 必须固化 0x01 0x05 前缀")
		require.False(t, strings.HasPrefix(vt.WitnessScript, "5575"), "不得是 OP_5")
	}
}

// TestDeriveDeposit_rejectsInvalidInputs 输入边界的 fail-closed 断言。
func TestDeriveDeposit_rejectsInvalidInputs(t *testing.T) {
	pub, err := hex.DecodeString("0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798")
	require.NoError(t, err)

	_, err = DeriveDepositWitnessScript("", pub)
	require.Equal(t, ErrInvalidDepositUserID, err)
	_, err = DeriveDepositWitnessScript(strings.Repeat("a", MaxDepositUserIDLen+1), pub)
	require.Equal(t, ErrInvalidDepositUserID, err)
	_, err = DeriveDepositPkScript("", pub)
	require.Equal(t, ErrInvalidDepositUserID, err)
	_, err = DeriveDepositAddress("addr", pub, nil)
	require.ErrorIs(t, err, ErrInvalidTssPubKey)

	// tssPub：非 33 字节 / 非法前缀一律拒绝
	_, err = DeriveDepositWitnessScript("addr", pub[:32])
	require.Equal(t, ErrInvalidTssPubKey, err)
	_, err = DeriveDepositWitnessScript("addr", append([]byte{0x04}, make([]byte, 32)...))
	require.Equal(t, ErrInvalidTssPubKey, err)

	// 关键反例：非压缩编码的**同一把钥**必须被拒（否则脚本/地址会随提交格式漂移）
	priv, _ := btcec.PrivKeyFromBytes([]byte{0x01})
	uncompressed := priv.PubKey().SerializeUncompressed()
	require.Len(t, uncompressed, 65)
	require.Equal(t, pub, priv.PubKey().SerializeCompressed())
	_, err = DeriveDepositWitnessScript("addr", uncompressed)
	require.Equal(t, ErrInvalidTssPubKey, err)

	_, err = ParseDepositTssPubKey(nil)
	require.Equal(t, ErrInvalidTssPubKey, err)
	_, err = ParseDepositTssPubKey(append([]byte{0x03}, make([]byte, 32)...))
	require.Equal(t, ErrInvalidTssPubKey, err)
	got, err := ParseDepositTssPubKey(pub)
	require.NoError(t, err)
	require.Equal(t, pub, got)
}

// TestIsNativeP2WSHScript 只认 witness v0 + 32 字节 program；P2WPKH / P2SH / P2TR 都不算。
func TestIsNativeP2WSHScript(t *testing.T) {
	pub, err := hex.DecodeString("0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798")
	require.NoError(t, err)
	p2wsh, err := DeriveDepositPkScript("addr", pub)
	require.NoError(t, err)
	require.True(t, IsNativeP2WSHScript(p2wsh))

	// P2WPKH（主池脚本形态）：v0 + 20 字节 program → 不是 native P2WSH
	require.False(t, IsNativeP2WSHScript(append([]byte{0x00, 0x14}, make([]byte, 20)...)))
	// P2TR：v1 + 32 字节 → 不是
	require.False(t, IsNativeP2WSHScript(append([]byte{0x51, 0x20}, make([]byte, 32)...)))
	// P2SH（嵌套 P2SH-P2WSH 的输出脚本就是这个形态）→ 不是
	require.False(t, IsNativeP2WSHScript(append([]byte{0xa9, 0x14}, make([]byte, 21)...)))
	// 非法脚本
	require.False(t, IsNativeP2WSHScript(nil))
	require.False(t, IsNativeP2WSHScript([]byte{0x6a}))
}

func vectorNames(doc p2wshVectorDoc) []string {
	names := make([]string, 0, len(doc.Vectors))
	for _, v := range doc.Vectors {
		names = append(names, v.Name)
	}
	return names
}
