package executor

import (
	"bytes"
	"errors"
	"fmt"
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

/*
 * B8：链上最小确认数（canonical tip 高度 >= proof.BlockHeight + N - 1）。
 *
 * 本文件覆盖：
 *   - 边界：tip == proof.Height + N - 1 通过、少 1 拒、tip 低于 proof.Height 拒；
 *   - N 可配（[exec.sub.rgbx].minBtcConfirmations）生效，默认 6；
 *   - 头链重组回退（B3/B4 之后 canonical tip 可以下降）→ 原来看似够深的证明被拒；
 *   - fail-closed：查询不到 tip / 头链为空一律拒；
 *   - 顺序：永久性判定（承诺/金额/merkle）先于暂时性的深度判定，报错不被"确认不足"掩盖；
 *   - 充值 / 提现确认两条路径都生效。
 */

// 证明所在 BTC 区块高度（fixture 里所有 proof 都用它）。
const testB8ProofHeight = uint64(100)

// b8Fixture 一个"能通过 checkDeposit 既有全部校验"的 BTC(XBTC) 充值环境 + 可控 tip。
type b8Fixture struct {
	r           *rgbx
	state       db.DB
	api         *mocks.QueueProtocolAPI
	txID        chainhash.Hash
	raw         []byte
	branch      [][]byte
	depositAddr string
	amount      int64
	pkScript    []byte
	// tip 注册进 mock 的 canonical tip 指针：改 (*tip).Height 即可模拟 tip 前进 / 重组回退。
	tip *ltypes.BtcHeader
}

func newB8Fixture(t *testing.T) *b8Fixture {
	t.Helper()
	f := &b8Fixture{amount: 1000, pkScript: []byte{0x51}}
	dir, state, _ := util.CreateTestDB()
	t.Cleanup(func() { util.CloseTestDB(dir, state) })
	f.state = state

	f.api = &mocks.QueueProtocolAPI{}
	f.api.On("GetConfig").Return(types.NewChain33Config(types.GetDefaultCfgstring()))
	f.api.On("Query", ltypes.LightclientX, "GetBtcNetName", mock.Anything).Return(
		&types.ReplyString{Data: "testnet3"}, nil)
	f.r = newRgbx().(*rgbx)
	f.r.SetAPI(f.api)
	f.r.SetStateDB(state)

	prevHash := chainhash.DoubleHashH([]byte("b8-prevout"))
	btcTx := wire.NewMsgTx(wire.TxVersion)
	btcTx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: prevHash, Index: 0}, nil, nil))
	btcTx.AddTxOut(wire.NewTxOut(f.amount, f.pkScript))
	var buf bytes.Buffer
	require.NoError(t, btcTx.SerializeNoWitness(&buf))
	f.raw = buf.Bytes()
	f.txID = btcTx.TxHash()

	leaves := [][]byte{f.txID.CloneBytes()}
	_, f.branch = merkle.GetMerkleRootAndBranch(leaves, 0)
	rootHash, err := chainhash.NewHash(merkle.GetMerkleRoot(leaves))
	require.NoError(t, err)
	// SPV 头：单交易区块，header.merkleRoot == txid。
	f.api.On("Query", ltypes.LightclientX, "GetBtcHeader", mock.Anything).Return(&ltypes.BtcHeader{
		Hash: "b8-block", Height: testB8ProofHeight, MerkleRoot: rootHash.String(),
	}, nil)

	require.NoError(t, state.Set(formatCrossChainInfoKey("BTC"), types.Encode(&rtypes.CrossChainInfo{
		AssetSymbol: "BTC", PkScript: f.pkScript,
	})))
	// 充值承诺：fromUtxo 口径 —— 交易的第一笔输入就是该 utxo。
	f.depositAddr = rtypes.FormatUtxo(prevHash.String(), 0)
	return f
}

// expectTip 声明 canonical tip（GetBtcLastHeader 的返回值），并返回可变更的指针：
// 直接改 (*tip).Height 即可模拟 tip 前进 / 重组回退。每个 fixture 只调用一次。
func (f *b8Fixture) expectTip(height uint64) *ltypes.BtcHeader {
	f.tip = &ltypes.BtcHeader{Hash: "b8-tip", Height: height}
	// testify 的 Return 保存的是这里的指针，调用方修改其指向内容对后续调用可见。
	f.api.On("Query", ltypes.LightclientX, "GetBtcLastHeader", mock.Anything).Return(f.tip, nil)
	return f.tip
}

func (f *b8Fixture) deposit(txData []byte) *rtypes.DepositAsset {
	return &rtypes.DepositAsset{
		Amount:         f.amount,
		DepositAddress: f.depositAddr,
		AssetSymbol:    "btc",
		TxProof: &rtypes.BtcTxProof{
			TxData:      txData,
			BlockHeight: testB8ProofHeight,
			BlockHash:   "b8-block",
			TxIndex:     0,
			MerkleProof: f.branch,
		},
	}
}

func Test_btcConfirmations(t *testing.T) {
	tc := []struct {
		tip, proof, want uint64
	}{
		{tip: 100, proof: 100, want: 1},
		{tip: 105, proof: 100, want: 6},
		{tip: 99, proof: 100, want: 0}, // 头链回退到该块之前：无确认
		{tip: 0, proof: 0, want: 1},
	}
	for _, c := range tc {
		require.Equalf(t, c.want, btcConfirmations(c.tip, c.proof), "tip=%d proof=%d", c.tip, c.proof)
	}
}

// Test_checkDeposit_btcConfirmationsBoundary 边界 + 错误信息内容（含 tip 高度 / 证明高度 / 要求的 N）。
func Test_checkDeposit_btcConfirmationsBoundary(t *testing.T) {
	require.Equal(t, int64(6), rgbxCfg.MinBtcConfirmations, "默认值应为 6")
	required := uint64(rgbxCfg.MinBtcConfirmations)

	f := newB8Fixture(t)
	tip := f.expectTip(testB8ProofHeight + required - 1)

	// 刚好满足：tip == proof.Height + N - 1（确认数恰为 N）
	require.NoError(t, f.r.checkDeposit("tx-exact", f.deposit(f.raw)))

	// 少 1 个确认 → 拒
	tip.Height = testB8ProofHeight + required - 2
	err := f.r.checkDeposit("tx-short", f.deposit(f.raw))
	require.ErrorIs(t, err, ErrInsufficientBtcConfirmations)
	require.Contains(t, err.Error(), fmt.Sprintf("tipHeight=%d", tip.Height))
	require.Contains(t, err.Error(), fmt.Sprintf("proofHeight=%d", testB8ProofHeight))
	require.Contains(t, err.Error(), fmt.Sprintf("required=%d", required))

	// tip 低于证明高度（该块所在分叉已被回退掉）→ 拒
	tip.Height = testB8ProofHeight - 1
	require.ErrorIs(t, f.r.checkDeposit("tx-below", f.deposit(f.raw)), ErrInsufficientBtcConfirmations)
}

// Test_checkDeposit_btcConfirmationsConfigurable N 可配：配大/配小各一例。
func Test_checkDeposit_btcConfirmationsConfigurable(t *testing.T) {
	orig := rgbxCfg.MinBtcConfirmations
	t.Cleanup(func() { rgbxCfg.MinBtcConfirmations = orig })

	tc := []struct {
		name    string
		n       int64
		tip     uint64
		wantErr bool
	}{
		{name: "n=1 证明高度本身即满足", n: 1, tip: testB8ProofHeight, wantErr: false},
		{name: "n=1 低于证明高度仍拒", n: 1, tip: testB8ProofHeight - 1, wantErr: true},
		{name: "n=2 恰好满足", n: 2, tip: testB8ProofHeight + 1, wantErr: false},
		{name: "n=2 少 1 拒", n: 2, tip: testB8ProofHeight, wantErr: true},
		{name: "n=100 恰好满足", n: 100, tip: testB8ProofHeight + 99, wantErr: false},
		{name: "n=100 少 1 拒", n: 100, tip: testB8ProofHeight + 98, wantErr: true},
	}
	for _, c := range tc {
		t.Run(c.name, func(t *testing.T) {
			f := newB8Fixture(t)
			f.expectTip(c.tip)
			rgbxCfg.MinBtcConfirmations = c.n
			err := f.r.checkDeposit("tx", f.deposit(f.raw))
			if c.wantErr {
				require.ErrorIs(t, err, ErrInsufficientBtcConfirmations)
				require.Contains(t, err.Error(), fmt.Sprintf("required=%d", c.n))
				return
			}
			require.NoError(t, err)
		})
	}
}

// Test_checkDeposit_btcConfirmationsSameTipOppositeVerdict 同一深度、不同 N 给出相反结论：
// 直接证明"深度门槛确实由配置决定"，而不是写死的常量。
func Test_checkDeposit_btcConfirmationsSameTipOppositeVerdict(t *testing.T) {
	orig := rgbxCfg.MinBtcConfirmations
	t.Cleanup(func() { rgbxCfg.MinBtcConfirmations = orig })

	f := newB8Fixture(t)
	// 证明高度 + 4：默认 N=6 时确认数只有 5 → 拒
	f.expectTip(testB8ProofHeight + 4)
	rgbxCfg.MinBtcConfirmations = 6
	require.ErrorIs(t, f.r.checkDeposit("tx", f.deposit(f.raw)), ErrInsufficientBtcConfirmations)
	// 同一个 tip，N 调到 5 → 确认数恰好 5 → 通过
	rgbxCfg.MinBtcConfirmations = 5
	require.NoError(t, f.r.checkDeposit("tx", f.deposit(f.raw)))
}

// Test_checkDeposit_btcConfirmationsReorgRollback 重组回退：canonical tip 下降后，
// 同一份"原来看似够深"的证明必须被拒 —— 这正是把确认数搬到链上想要的行为
// （铸币本身不随头链回退，但被回退掉的证明不再能通过校验）。
func Test_checkDeposit_btcConfirmationsReorgRollback(t *testing.T) {
	require.Greater(t, rgbxCfg.MinBtcConfirmations, int64(1), "本用例假设 N > 1")
	required := uint64(rgbxCfg.MinBtcConfirmations)

	f := newB8Fixture(t)
	tip := f.expectTip(testB8ProofHeight + required - 1)
	require.NoError(t, f.r.checkDeposit("tx-before-reorg", f.deposit(f.raw)))

	// B3/B4 之后 canonical tip 可以下降（更重的分叉替换了原链）：深度缩水 → 同一份证明被拒。
	tip.Height = testB8ProofHeight + required - 2
	require.ErrorIs(t, f.r.checkDeposit("tx-after-reorg", f.deposit(f.raw)), ErrInsufficientBtcConfirmations)
}

// Test_checkDeposit_btcConfirmationsFailClosed 拿不到 tip 时 fail-closed：查询失败 / 头链为空都拒。
func Test_checkDeposit_btcConfirmationsFailClosed(t *testing.T) {
	t.Run("query error", func(t *testing.T) {
		f := newB8Fixture(t)
		f.api.On("Query", ltypes.LightclientX, "GetBtcLastHeader", mock.Anything).Return(nil, errors.New("lightclient down"))
		err := f.r.checkDeposit("tx", f.deposit(f.raw))
		require.ErrorIs(t, err, ErrInsufficientBtcConfirmations)
		require.Contains(t, err.Error(), "tipHeight=unknown")
		require.Contains(t, err.Error(), fmt.Sprintf("proofHeight=%d", testB8ProofHeight))
	})

	t.Run("empty tip", func(t *testing.T) {
		f := newB8Fixture(t)
		// lightclient 头链从未写入时返回零值头（Hash == ""），不是错误。
		f.api.On("Query", ltypes.LightclientX, "GetBtcLastHeader", mock.Anything).Return(&ltypes.BtcHeader{}, nil)
		err := f.r.checkDeposit("tx", f.deposit(f.raw))
		require.ErrorIs(t, err, ErrInsufficientBtcConfirmations)
		require.Contains(t, err.Error(), "tipHeight=none")
	})

	t.Run("invalid tip type", func(t *testing.T) {
		f := newB8Fixture(t)
		f.api.On("Query", ltypes.LightclientX, "GetBtcLastHeader", mock.Anything).Return(&types.ReplyString{Data: "x"}, nil)
		require.ErrorIs(t, f.r.checkDeposit("tx", f.deposit(f.raw)), ErrInsufficientBtcConfirmations)
	})
}

// Test_checkDeposit_permanentErrorsNotMaskedByConfirmations 顺序断言：
// 深度不足（暂时性）不得把"这份证明永远无效"（永久性）的错误掩盖成"确认不足"。
func Test_checkDeposit_permanentErrorsNotMaskedByConfirmations(t *testing.T) {
	f := newB8Fixture(t)
	f.expectTip(testB8ProofHeight - 1) // 深度一定不足

	// 1) merkle 不匹配（证明本身无效）
	badMerkle := f.deposit(f.raw)
	badMerkle.TxProof.MerkleProof = [][]byte{bytes.Repeat([]byte{1}, 32)}
	require.Equal(t, ErrInvalidBtcProofMerkle, f.r.checkDeposit("tx", badMerkle))

	// 2) OP_RETURN / fromUtxo 承诺不匹配
	badCommit := f.deposit(f.raw)
	badCommit.DepositAddress = rtypes.FormatUtxo(chainhash.DoubleHashH([]byte("other")).String(), 0)
	require.Equal(t, ErrInvalidDepositCommitment, f.r.checkDeposit("tx", badCommit))

	// 3) 金额不匹配
	badAmount := f.deposit(f.raw)
	badAmount.Amount = f.amount - 1
	require.Equal(t, ErrInvalidDepositAmount, f.r.checkDeposit("tx", badAmount))

	// 4) 非规范编码（尾部追加字节）
	appended := append(append([]byte{}, f.raw...), 0x00)
	require.Equal(t, ErrInvalidBtcTxProof, f.r.checkDeposit("tx", f.deposit(appended)))
}

// Test_checkDeposit_btcConfirmationsOnCheckTxPath 走 CheckTx 全链路（充值的共识强制点）。
func Test_checkDeposit_btcConfirmationsOnCheckTxPath(t *testing.T) {
	f := newB8Fixture(t)
	tip := f.expectTip(testB8ProofHeight + uint64(rgbxCfg.MinBtcConfirmations) - 1)

	action := &rtypes.RgbxAction{Ty: rtypes.TyDepositAsset, Value: &rtypes.RgbxAction_Deposit{}}
	tx := &types.Transaction{}
	deposit := action.Value.(*rtypes.RgbxAction_Deposit)

	deposit.Deposit = f.deposit(f.raw)
	tx.Payload = types.Encode(action)
	require.NoError(t, f.r.CheckTx(tx, 0))

	tip.Height = testB8ProofHeight
	deposit.Deposit = f.deposit(f.raw)
	tx.Payload = types.Encode(action)
	require.ErrorIs(t, f.r.CheckTx(tx, 0), ErrInsufficientBtcConfirmations)
}

// Test_checkWithdrawConfirm_btcConfirmations 提现确认路径（经 validateBtcTxProof 的那条）同样生效。
func Test_checkWithdrawConfirm_btcConfirmations(t *testing.T) {
	require.Greater(t, rgbxCfg.MinBtcConfirmations, int64(1))
	required := uint64(rgbxCfg.MinBtcConfirmations)

	f := newB8Fixture(t)
	tip := f.expectTip(testB8ProofHeight + required - 1)

	// BTC(XBTC) 提现确认：pending + 一份指向同样高度的、含提现承诺的证明。
	// 复用 b8Fixture 的 btc 交易（txid 与其 merkle 分支一致），承诺数据换成提现前缀。
	burnTxHash := []byte{1, 2, 3}
	confirm := &rtypes.ConfirmTx{TxHash: burnTxHash, BtcTxProof: f.deposit(f.raw).TxProof}
	pending := &rtypes.PendingTx{AssetSymbol: "btc", Amount: f.amount, TargetAddress: f.depositAddr}

	// 该证明的 OP_RETURN 承诺不是提现承诺 → 永久性错误优先于深度（tip 此时深度是够的）
	require.Equal(t, ErrInvalidBtcProofCommitment, f.r.checkWithdrawConfirm("tx", "confirm", confirm, pending))

	// 深度不足时，同一份证明的报错仍是承诺错误（顺序断言），而不是"确认不足"。
	tip.Height = testB8ProofHeight - 1
	require.Equal(t, ErrInvalidBtcProofCommitment, f.r.checkWithdrawConfirm("tx", "confirm", confirm, pending))

	// RGB20 分支（跳过承诺/金额校验）：深度不足必须被拒 —— 这是 B8 对提现路径的净效果。
	rgb20 := &rtypes.ConfirmTx{TxHash: burnTxHash, BtcTxProof: f.deposit(f.raw).TxProof}
	rgb20Pending := &rtypes.PendingTx{AssetSymbol: rtypes.RGB20USDTSymbol, Amount: 1}
	require.ErrorIs(t, f.r.checkWithdrawConfirm("tx", "confirm", rgb20, rgb20Pending), ErrInsufficientBtcConfirmations)

	// 深度刚好满足 → 通过（RGB20 分支不再有其它链上校验）
	tip.Height = testB8ProofHeight + required - 1
	require.NoError(t, f.r.checkWithdrawConfirm("tx", "confirm", rgb20, rgb20Pending))

	// 再少 1 个确认 → 拒
	tip.Height = testB8ProofHeight + required - 2
	require.ErrorIs(t, f.r.checkWithdrawConfirm("tx", "confirm", rgb20, rgb20Pending), ErrInsufficientBtcConfirmations)
}
