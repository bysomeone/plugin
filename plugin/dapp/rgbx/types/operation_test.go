package types

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本文件锚定 operationId 的**派生规格**（S2）：域分隔标签 + 定长前缀编码 + 输入集合。
//
// 为什么值得钉死：operationId 是链上台账的键，规则变了历史 id 全变（"同一笔操作换了身份"）。
// 下面的期望值是**冻结向量**——派生实现一旦被改动（哪怕只是换标签、改字段顺序、去掉长度前缀），
// 这里立刻变红，逼改动者意识到自己动了线上身份口径（要么改回来，要么显式升级到 v2 标签）。

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	require.NoError(t, err)
	return b
}

// Test_MintOperationID_frozenVectors 冻结向量：输入 → 期望 id 逐字节固定。
func Test_MintOperationID_frozenVectors(t *testing.T) {
	btcTxID := mustHex(t, "1111111111111111111111111111111111111111111111111111111111111111")

	tests := []struct {
		name    string
		symbol  string
		txID    []byte
		addr    string
		amount  int64
		wantHex string
	}{
		{
			name:    "XBTC 典型充值",
			symbol:  "XBTC",
			txID:    btcTxID,
			addr:    "1AmRYcURfDGxBhiaJAvEGdRkvkoM7ztn1u",
			amount:  1000,
			wantHex: "2887d23f26245bc50b325e4e45f2feea69d053e862f8d0aad5936773a9ab9377",
		},
		{
			name:    "XRGB20_USDT 充值",
			symbol:  "XRGB20_USDT",
			txID:    btcTxID,
			addr:    "1MCurL8dSuJUWQKabUCGYXuM9Ypm5ftN9s",
			amount:  500,
			wantHex: "da02b2ffa1ae27f26226880fdae9b04c2069addb16733c237c6a165925c8d534",
		},
	}
	for _, tc := range tests {
		got, err := MintOperationID(tc.symbol, tc.txID, tc.addr, tc.amount)
		require.NoErrorf(t, err, tc.name)
		require.Lenf(t, got, OperationIDLen, tc.name)
		require.Equalf(t, tc.wantHex, hex.EncodeToString(got),
			"%s: operationId 派生规格变了（冻结向量 testdata 口径）", tc.name)
		// 确定性：同输入必得同结果
		again, err := MintOperationID(tc.symbol, tc.txID, tc.addr, tc.amount)
		require.NoError(t, err)
		require.Truef(t, bytes.Equal(got, again), "%s: 同输入必须同结果", tc.name)
	}
}

// Test_MintOperationID_expectedIDs 与**冻结字面量**比对（派生规格的锁）。
// 这三个值是用当前实现算出来后**手写钉住**的；改派生规则必然让本用例变红。
func Test_MintOperationID_expectedIDs(t *testing.T) {
	txID := mustHex(t, "1111111111111111111111111111111111111111111111111111111111111111")

	mintID, err := MintOperationID("XBTC", txID, "1AmRYcURfDGxBhiaJAvEGdRkvkoM7ztn1u", 1000)
	require.NoError(t, err)
	require.Equal(t,
		"2887d23f26245bc50b325e4e45f2feea69d053e862f8d0aad5936773a9ab9377",
		hex.EncodeToString(mintID))

	burnID, err := BurnOperationID("XBTC", []byte("burn-tx-hash"))
	require.NoError(t, err)
	require.Equal(t,
		"ad36e57d8f48cd05368dee9a038767ea0e224c7f4efcf91062788f1e8fb8697b",
		hex.EncodeToString(burnID))
}

// Test_MintOperationID_distinguishesInputs 输入任一不同 ⇒ id 不同（含"金额进原像"这一条）。
func Test_MintOperationID_distinguishesInputs(t *testing.T) {
	txID := mustHex(t, "2222222222222222222222222222222222222222222222222222222222222222")
	addr := "1AmRYcURfDGxBhiaJAvEGdRkvkoM7ztn1u"

	base, err := MintOperationID("XBTC", txID, addr, 1000)
	require.NoError(t, err)

	others := [][]byte{}
	// 金额不同（"改金额"必须换 id：台账记录无法被重新解释成另一个金额）
	other, err := MintOperationID("XBTC", txID, addr, 1001)
	require.NoError(t, err)
	others = append(others, other)
	// txid 不同
	other, err = MintOperationID("XBTC", mustHex(t, "3333333333333333333333333333333333333333333333333333333333333333"), addr, 1000)
	require.NoError(t, err)
	others = append(others, other)
	// 充值地址不同（同一笔 BTC 交易给多个用户付款 ⇒ 多笔独立充值）
	other, err = MintOperationID("XBTC", txID, "1MCurL8dSuJUWQKabUCGYXuM9Ypm5ftN9s", 1000)
	require.NoError(t, err)
	others = append(others, other)
	// symbol 不同
	other, err = MintOperationID("XRGB20_USDT", txID, addr, 1000)
	require.NoError(t, err)
	others = append(others, other)
	// 大小写不同的同一 symbol ⇒ **同一个** id（symbol 大小写不敏感是既有口径），所以不放进 others
	same, err := MintOperationID("xbtc", txID, addr, 1000)
	require.NoError(t, err)
	require.True(t, bytes.Equal(base, same), "symbol 大小写不敏感：'xbtc' 与 'XBTC' 必须同 id")

	for i, o := range others {
		require.Falsef(t, bytes.Equal(base, o), "第 %d 个变体必须得到不同 id", i)
	}
}

// Test_OperationID_domainSeparated 充值 op 与提现 op **不会碰撞**：即使输入字节完全相同
// （symbol 相同、哈希相同），域分隔标签也保证两个 id 不同 —— 台账里两类操作因此不会互相顶替。
func Test_OperationID_domainSeparated(t *testing.T) {
	raw := mustHex(t, "4444444444444444444444444444444444444444444444444444444444444444")

	mintID, err := MintOperationID("XBTC", raw, "1AmRYcURfDGxBhiaJAvEGdRkvkoM7ztn1u", 1000)
	require.NoError(t, err)
	burnID, err := BurnOperationID("XBTC", raw)
	require.NoError(t, err)
	require.False(t, bytes.Equal(mintID, burnID), "铸造 op 与销毁 op 必须域分隔")
	require.Len(t, mintID, OperationIDLen)
	require.Len(t, burnID, OperationIDLen)
}

// Test_MintOperationID_lengthPrefixAvoidsAmbiguity 长度前缀消除拼接歧义：
// ("ab","c") 与 ("a","bc") 这类"边界移动"必须得到不同 id（无前缀编码会撞）。
func Test_MintOperationID_lengthPrefixAvoidsAmbiguity(t *testing.T) {
	txID := mustHex(t, "5555555555555555555555555555555555555555555555555555555555555555")

	// userID="AB" 且 symbol="X"（归一化后）与 userID="B" 且 symbol="XA" 在无长度前缀时原像相同
	a, err := MintOperationID("X", txID, "AB", 7)
	require.NoError(t, err)
	b, err := MintOperationID("XA", txID, "B", 7)
	require.NoError(t, err)
	require.False(t, bytes.Equal(a, b), "边界不同的输入必须得到不同 id")
}

// Test_OperationID_rejectsInvalidInput 输入不合法（非 32 字节 txid / 空 symbol / 空地址 / 非正金额）
// 一律报错，不产生 id：id 是台账的键，绝不能由"半份输入"派生出一个身份。
func Test_OperationID_rejectsInvalidInput(t *testing.T) {
	valid := mustHex(t, "6666666666666666666666666666666666666666666666666666666666666666")

	cases := []struct {
		name   string
		fn     func() ([]byte, error)
		expect error
	}{
		{"txid 长度不对", func() ([]byte, error) { return MintOperationID("XBTC", valid[:31], "addr", 1) }, ErrInvalidOperationIDInput},
		{"txid 为 nil", func() ([]byte, error) { return MintOperationID("XBTC", nil, "addr", 1) }, ErrInvalidOperationIDInput},
		{"symbol 为空", func() ([]byte, error) { return MintOperationID("", valid, "addr", 1) }, ErrInvalidOperationIDInput},
		{"地址为空", func() ([]byte, error) { return MintOperationID("XBTC", valid, "", 1) }, ErrInvalidOperationIDInput},
		{"金额为 0", func() ([]byte, error) { return MintOperationID("XBTC", valid, "addr", 0) }, ErrInvalidOperationIDInput},
		{"金额为负", func() ([]byte, error) { return MintOperationID("XBTC", valid, "addr", -1) }, ErrInvalidOperationIDInput},
		{"burn 哈希为空", func() ([]byte, error) { return BurnOperationID("XBTC", nil) }, ErrInvalidOperationIDInput},
		{"burn symbol 为空", func() ([]byte, error) { return BurnOperationID("", []byte("h")) }, ErrInvalidOperationIDInput},
	}
	for _, tc := range cases {
		_, err := tc.fn()
		require.Errorf(t, err, tc.name)
		require.ErrorIsf(t, err, tc.expect, tc.name)
	}
}
