package neutrino

import (
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/stretchr/testify/require"
)

// 本文件覆盖 RGB20 提现的**广播幂等**判定（E9-A 的配套项）：事故形态下重试会广播同一笔交易，
// 节点以「已在 mempool / 已存在」拒绝，但钱已经在路上——必须算成功，否则重试永远失败、链上那笔
// burn 一直挂着。判定依据是「节点是否已经知道这个 txid」，不是错误文本。

const (
	knownTxid   = "0000000000000000000000000000000000000000000000000000000000000001"
	unknownTxid = "0000000000000000000000000000000000000000000000000000000000000002"
)

// fakeTxLookup 假的全节点查询：只有 knownTxid 查得到。
type fakeTxLookup struct {
	calls int
}

func (f *fakeTxLookup) GetRawTransactionVerbose(hash *chainhash.Hash) (*btcjson.TxRawResult, error) {
	f.calls++
	if hash.String() == knownTxid {
		return &btcjson.TxRawResult{Txid: hash.String()}, nil
	}
	return nil, errors.New("-5: No such mempool or blockchain transaction")
}

// Test_txKnownToNode 节点可见性查询：已知 ⇒ true；未知/无法查询 ⇒ false（fail-safe：
// 查询不可用时退回「按广播结果判定」，绝不把失败当成功）。
func Test_txKnownToNode(t *testing.T) {
	lookup := &fakeTxLookup{}
	require.True(t, txKnownToNode(lookup, knownTxid))
	require.False(t, txKnownToNode(lookup, unknownTxid))
	require.False(t, txKnownToNode(nil, knownTxid), "没有全节点 RPC 时不得凭空判定为已知")
	require.False(t, txKnownToNode(lookup, ""), "空 txid 不得判定为已知")
	require.False(t, txKnownToNode(lookup, "not-a-txid"))
}

// Test_broadcastOutcomeIsSuccess 广播结果判定：
//   - 没报错 ⇒ 成功；
//   - 报错但节点已知这笔交易（回包丢失 / 重复广播）⇒ 成功；
//   - 报错且节点不知道 ⇒ 失败（真实的广播失败必须照旧返回错误，不能掩盖）。
func Test_broadcastOutcomeIsSuccess(t *testing.T) {
	lookup := &fakeTxLookup{}
	require.True(t, broadcastOutcomeIsSuccess(lookup, knownTxid, nil))
	require.True(t, broadcastOutcomeIsSuccess(lookup, knownTxid, errors.New("already in mempool")))
	require.False(t, broadcastOutcomeIsSuccess(lookup, unknownTxid, errors.New("connection reset by peer")))
	require.False(t, broadcastOutcomeIsSuccess(nil, unknownTxid, errors.New("connection reset by peer")))
}
