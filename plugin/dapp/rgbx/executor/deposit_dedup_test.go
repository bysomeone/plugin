package executor

import (
	"bytes"
	"testing"

	"github.com/33cn/chain33/client/mocks"
	"github.com/33cn/chain33/common/db"
	"github.com/33cn/chain33/common/merkle"
	"github.com/33cn/chain33/types"
	"github.com/33cn/chain33/util"
	ltypes "github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// 本文件覆盖 E1（充值防双铸可被"同一笔交易的另一份编码"绕过）的修复：
//   - 唯一标识由 TxData 原始字节哈希改为解析后 btc 交易的 txid；
//   - TxData 必须被完整解析（尾部追加字节等非规范编码直接拒绝）。
// 关键复现前提：给一笔已序列化的 btc 交易追加尾部字节后，txid（以及解析出的输出、OP_RETURN
// 承诺、金额）完全不变，只有原始字节变了。

// e1DepositFixture BTC(XBTC) 充值路径的最小可用环境。
type e1DepositFixture struct {
	r           *rgbx
	state       db.DB
	cfg         *types.Chain33Config
	txID        chainhash.Hash
	raw         []byte
	branch      [][]byte
	depositAddr string
	amount      int64
	pkScript    []byte
}

func newE1DepositFixture(t *testing.T) *e1DepositFixture {
	t.Helper()
	f := &e1DepositFixture{amount: 1000, pkScript: []byte{0x51}}
	dir, state, _ := util.CreateTestDB()
	t.Cleanup(func() { util.CloseTestDB(dir, state) })
	f.state = state
	f.cfg = types.NewChain33Config(types.GetDefaultCfgstring())

	api := &mocks.QueueProtocolAPI{}
	api.On("GetConfig").Return(f.cfg)
	f.r = newRgbx().(*rgbx)
	f.r.SetAPI(api)
	f.r.SetStateDB(state)

	prevHash := chainhash.DoubleHashH([]byte("e1-prevout"))
	var btcTx wire.MsgTx
	btcTx.Version = 2
	btcTx.TxIn = append(btcTx.TxIn, wire.NewTxIn(&wire.OutPoint{Hash: prevHash, Index: 0}, nil, nil))
	btcTx.TxOut = append(btcTx.TxOut, wire.NewTxOut(f.amount, f.pkScript))
	buf := new(bytes.Buffer)
	require.NoError(t, btcTx.SerializeNoWitness(buf))
	f.raw = buf.Bytes()
	f.txID = btcTx.TxHash()
	leaves := [][]byte{f.txID.CloneBytes()}
	_, f.branch = merkle.GetMerkleRootAndBranch(leaves, 0)
	rootHash, err := chainhash.NewHash(merkle.GetMerkleRoot(leaves))
	require.NoError(t, err)
	api.On("Query", ltypes.LightclientX, "GetBtcHeader", mock.Anything).Return(&ltypes.BtcHeader{
		Hash: "e1-block", Height: 100, MerkleRoot: rootHash.String(),
	}, nil)
	// B8：链上最小确认数校验读 canonical tip（statedb）。这里给足深度（>= 100 + 6 - 1），
	// 本文件只关心 txid 口径，深度是否满足由 btc_confirm_test.go 覆盖。
	api.On("Query", ltypes.LightclientX, "GetBtcLastHeader", mock.Anything).Return(&ltypes.BtcHeader{
		Hash: "e1-tip", Height: 100 + uint64(defaultMinBtcConfirmations) - 1,
	}, nil)

	require.NoError(t, state.Set(formatCrossChainInfoKey("BTC"), types.Encode(&rtypes.CrossChainInfo{
		AssetSymbol: "BTC", PkScript: f.pkScript,
	})))
	f.depositAddr = rtypes.FormatUtxo(prevHash.String(), 0)
	return f
}

// deposit 用给定 TxData 构造充值请求（字节可不同，但 txid 相同）。
func (f *e1DepositFixture) deposit(txData []byte) *rtypes.DepositAsset {
	return &rtypes.DepositAsset{
		Amount:         f.amount,
		DepositAddress: f.depositAddr,
		AssetSymbol:    "btc",
		TxProof: &rtypes.BtcTxProof{
			TxData:      txData,
			BlockHeight: 100,
			BlockHash:   "e1-block",
			TxIndex:     0,
			MerkleProof: f.branch,
		},
	}
}

// appended 返回"同一笔交易的尾部追加 n 字节"变体（txid 不变）。
func (f *e1DepositFixture) appended(t *testing.T, n int) []byte {
	t.Helper()
	out := append(append([]byte{}, f.raw...), bytes.Repeat([]byte{0x00}, n)...)
	var parsed wire.MsgTx
	require.NoError(t, parsed.DeserializeNoWitness(bytes.NewReader(out)))
	require.Equal(t, f.txID, parsed.TxHash(), "追加字节后 txid 必须不变（复现前提）")
	return out
}

// Test_checkDeposit_rejectsAppendedTxDataReplay E1 验收断言：
// 同一笔真实充值（同 txid）换一份"尾部追加字节"的编码重复提交，必须被拒 —— 修复前 err == nil。
func Test_checkDeposit_rejectsAppendedTxDataReplay(t *testing.T) {
	f := newE1DepositFixture(t)
	require.NoError(t, f.r.checkDeposit("tx-first", f.deposit(f.raw)))
	// 模拟 Exec_Deposit 写入已消费标记（txid 口径，见 Exec_Deposit）
	require.NoError(t, f.state.Set(formatDepositUsedTxIDKey(f.txID.CloneBytes()), []byte("used")))

	// 原始字节重复提交：改动前后都应被拒
	require.Equal(t, ErrDuplicateDepositProof, f.r.checkDeposit("tx-replay-same", f.deposit(f.raw)))

	// 尾部追加 1/4/32 字节：txid 不变，但字节变了 —— 修复前会绕过重复检查（可无限铸币）
	for _, n := range []int{1, 4, 32} {
		appended := f.appended(t, n)
		err := f.r.checkDeposit("tx-replay-appended", f.deposit(appended))
		require.Equalf(t, ErrInvalidBtcTxProof, err, "追加 %d 字节的充值证明必须被拒", n)
	}
}

// Test_checkDeposit_duplicateByCanonicalTxID txid 口径去重：状态里有该 txid 的已消费记录时，
// 同一笔交易必须被拒。
func Test_checkDeposit_duplicateByCanonicalTxID(t *testing.T) {
	f := newE1DepositFixture(t)
	require.NoError(t, f.state.Set(formatDepositUsedTxIDKey(f.txID.CloneBytes()), []byte("used")))

	require.Equal(t, ErrDuplicateDepositProof, f.r.checkDeposit("tx-replay", f.deposit(f.raw)))
}

// Test_checkDeposit_normalAndDistinctTxData 正常充值不受影响：全新充值通过；另一笔（不同 txid）
// 充值互不影响；同一笔交易换了别的合法编码也仍按同一身份处理。
func Test_checkDeposit_normalAndDistinctTxData(t *testing.T) {
	f := newE1DepositFixture(t)
	require.NoError(t, f.r.checkDeposit("tx-fresh", f.deposit(f.raw)))
	require.NoError(t, f.state.Set(formatDepositUsedTxIDKey(f.txID.CloneBytes()), []byte("used")))

	// 另一笔不同 txid 的充值：不受影响（把它的 txid 作为已消费记录写入，也不影响上面的 txid）
	otherTx := &wire.MsgTx{Version: 2}
	otherTx.TxIn = append(otherTx.TxIn, wire.NewTxIn(&wire.OutPoint{Hash: chainhash.DoubleHashH([]byte("other")), Index: 0}, nil, nil))
	otherTx.TxOut = append(otherTx.TxOut, wire.NewTxOut(f.amount, f.pkScript))
	otherRaw := new(bytes.Buffer)
	require.NoError(t, otherTx.SerializeNoWitness(otherRaw))
	require.NotEqual(t, f.txID, otherTx.TxHash())
	// merkle 证明不匹配 → 走不到后面；此处只断言"未被判为重复"
	err := f.r.checkDeposit("tx-other", f.deposit(otherRaw.Bytes()))
	require.Equal(t, ErrInvalidBtcProofMerkle, err)
}

// Test_parseBtcTxIDStrict 严格解析（A）：完整消费才算合法。
func Test_parseBtcTxIDStrict(t *testing.T) {
	f := newE1DepositFixture(t)

	_, err := parseBtcTxIDStrict("tx1", nil)
	require.Equal(t, ErrInvalidBtcTxProof, err)

	_, err = parseBtcTxIDStrict("tx1", []byte{0xff, 0xff})
	require.Equal(t, ErrInvalidBtcTxProof, err)

	for _, n := range []int{1, 4, 32} {
		_, err = parseBtcTxIDStrict("tx1", f.appended(t, n))
		require.Equalf(t, ErrInvalidBtcTxProof, err, "尾部 %d 字节必须被拒", n)
	}

	txID, err := parseBtcTxIDStrict("tx1", f.raw)
	require.NoError(t, err)
	require.Equal(t, f.txID.CloneBytes(), txID)
}

// Test_Exec_Deposit_writesDepositTxIDKey Exec 侧登记已消费：写入 txid 口径 key。
func Test_Exec_Deposit_writesDepositTxIDKey(t *testing.T) {
	f := newE1DepositFixture(t)
	recp := testExec(t, f.r, rtypes.NameDepositAssetAction, f.deposit(f.raw), nil, 0)
	require.NotNil(t, recp)

	kv := make(map[string][]byte)
	for _, item := range recp.GetKV() {
		kv[string(item.GetKey())] = item.GetValue()
	}
	require.Contains(t, kv, string(formatDepositUsedTxIDKey(f.txID.CloneBytes())))
}
