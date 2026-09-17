package commands

import (
	"encoding/hex"
	"strings"
	"testing"

	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/stretchr/testify/require"
)

// 回归背景（P2WSH 充值场景阻塞）：`rgbx getCrossChainInfo` 输出的 pubkey/pkScript 带 0x 前缀，
// 而 CLI 的 flag 帮助就是让人从那里取值（btcDepositTx --tssPubkey / commitDKG -p -k）。
// 旧实现直接 hex.DecodeString 会以 "invalid byte: U+0078 'x'" 失败，导致充值交易根本构造不出来。
const testTssPubkeyHex = "03ecafc5b02a5a6abf0d4dbeb7d3b48fc2c3f585357995e3d5e5bacc8a8acf037b"

func Test_decodeHexAuto_ToleratesOptionalPrefixAndSpaces(t *testing.T) {
	want, err := hex.DecodeString(testTssPubkeyHex)
	require.NoError(t, err)

	cases := []struct {
		name string
		in   string
	}{
		{"plain", testTssPubkeyHex},
		{"0x prefix", "0x" + testTssPubkeyHex},
		{"0X prefix", "0X" + testTssPubkeyHex},
		{"surrounding spaces", "  " + testTssPubkeyHex + "\n"},
		{"0x plus spaces", " 0x" + testTssPubkeyHex + " "},
		{"uppercase digits", "0x" + strings.ToUpper(testTssPubkeyHex)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeHexAuto(tc.in)
			require.NoError(t, err)
			require.Equal(t, want, got)
		})
	}
}

func Test_decodeHexAuto_EmptyIsEmpty(t *testing.T) {
	got, err := decodeHexAuto("   ")
	require.NoError(t, err)
	require.Empty(t, got)
}

func Test_decodeHexAuto_ErrorsAreClear(t *testing.T) {
	// 奇数长度：必须自带长度，便于定位（hex 原生的 "odd length hex string" 不带长度）。
	_, err := decodeHexAuto("0xabc")
	require.Error(t, err)
	require.Contains(t, err.Error(), "odd-length")
	require.Contains(t, err.Error(), "3")

	// 非法字符：指到具体字节（0x 已被摘掉，报错的是 'z' 而不是 'x'）。
	_, err = decodeHexAuto("0xzz")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid byte")

	// 裸 0x（摘掉前缀后为空）不是错误，与旧的空串语义一致。
	got, err := decodeHexAuto("0x")
	require.NoError(t, err)
	require.Empty(t, got)
}

// 用户输入路径：utxo / optional / list 三个入口都要能吃 0x 前缀。
func Test_UserHexInputPaths_AcceptPrefix(t *testing.T) {
	pkScript := "0014500fe7e50287fa1fb520c4b47ea998874d79d44f"

	utxo, err := parseDepositUTXO("0x" + strings.Repeat("11", 32) + ":0:100000:0x" + pkScript)
	require.NoError(t, err)
	raw, err := hex.DecodeString(pkScript)
	require.NoError(t, err)
	require.Equal(t, raw, utxo.pkScript)

	opt, err := decodeHexOptional(" 0x" + pkScript)
	require.NoError(t, err)
	require.Equal(t, raw, opt)

	list, err := decodeHexList("0x" + strings.Repeat("ab", 3) + ", " + strings.Repeat("cd", 3))
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, []byte{0xab, 0xab, 0xab}, list[0])
	require.Equal(t, []byte{0xcd, 0xcd, 0xcd}, list[1])
}

// 关键行为：带不带 0x 必须派生出**同一个** P2WSH 充值地址（否则"容错"就成了另一种错）。
func Test_btcDepositAddress_PrefixDoesNotChangeDerivation(t *testing.T) {
	user := "1KSBd17H7ZK8iT37aJztFB22XGwsPTdwE4"
	params := &chaincfg.RegressionNetParams

	plainKey, err := decodeHexAuto(testTssPubkeyHex)
	require.NoError(t, err)
	prefixedKey, err := decodeHexAuto("0x" + testTssPubkeyHex)
	require.NoError(t, err)
	require.Equal(t, plainKey, prefixedKey)

	plainAddr, err := rtypes.DeriveDepositAddress(user, plainKey, params)
	require.NoError(t, err)
	prefixedAddr, err := rtypes.DeriveDepositAddress(user, prefixedKey, params)
	require.NoError(t, err)
	// 同一个 tssPubkey + 同一个 userID ⇒ 同一个充值地址（0x 前缀不影响归属认定）。
	require.Equal(t, plainAddr, prefixedAddr)
	require.True(t, strings.HasPrefix(plainAddr, "bcrt1"))
}
