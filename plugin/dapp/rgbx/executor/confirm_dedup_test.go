package executor

import (
	"bytes"
	"testing"

	"github.com/33cn/chain33/client/mocks"
	"github.com/33cn/chain33/common/db"
	"github.com/33cn/chain33/types"
	"github.com/33cn/chain33/util"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// 本文件覆盖 E14：Exec_Confirm 的 UtxoProof 结算分支（mint / transfer）在 stateDB 里没有
// "已消费"键，去重只依赖非共识路径 ExecLocal_Confirm 写的 localdb pendingTx.Confirmed。
//
// 关键复现前提（本文件的 fixture 就按它搭）：pendingTx.Confirmed 由 **非共识** 的
// ExecLocal_Confirm 写入节点私有 LocalDB —— 它可以被 exec.disableExecLocal 整块跳过，
// 且 LocalDB 与 stateDB 是两次独立提交（存在崩溃窗口），因此它完全可能"陈旧"
//（该为 true 却为 false）。此时旧守卫不会拦下重复确认 ⇒ mint 被二次结算（二次铸币），
// 该块与其余节点的状态哈希不符 ⇒ 分叉/停链。
//
// 修复后的权威判据是 stateDB 的 formatConfirmUsedKey(confirm.GetTxHash())：
//   - Exec_Confirm 结算成功时随回执 KV 写入（随 stateDB 一起回滚，不误伤重放）；
//   - checkConfirm 先查该键，命中即拒（ErrMintAlreadyConfirmed / ErrTransferAlreadyConfirmed）；
//   - localdb 那条 pendingTx.Confirmed 保留作纵深防御，但不再是去重依据。

// e14ConfirmFixture 一笔 mint / transfer 确认的最小可用环境：
// statedb payload + statedb 去重键读写的 stateDB + localdb pendingTx + 一份规范编码的 BTC 花费交易。
type e14ConfirmFixture struct {
	r     *rgbx
	state db.DB
	local db.KVDB

	confirm *rtypes.ConfirmTx
	txHash  []byte // 被确认交易的 chain33 哈希（payload 键与 stateDB 去重键的身份）

	pendingUtxo string // 被花费的 rgbx utxo（= pendingTx.Utxo.ToString()）
	ownerUtxo   string // mint/transfer 的归属 utxo（解析后花费交易的 txid + opRetOutIdx + 1）
}

func newE14ConfirmFixture(t *testing.T, symbol string, actionType int32) *e14ConfirmFixture {
	t.Helper()
	f := &e14ConfirmFixture{txHash: []byte(symbol + "-tx")}

	dir, state, local := util.CreateTestDB()
	t.Cleanup(func() { util.CloseTestDB(dir, state) })
	f.state, f.local = state, local

	api := &mocks.QueueProtocolAPI{}
	api.On("GetConfig").Return(types.NewChain33Config(types.GetDefaultCfgstring()))
	f.r = newRgbx().(*rgbx)
	f.r.SetAPI(api)
	f.r.SetStateDB(state)
	f.r.SetLocalDB(local)

	// BTC 花费交易：input[0] 花掉 pending utxo，output[0] 是承诺该 chain33 交易的 OP_RETURN，
	// output[1] 是所有者输出（归属 utxo / 默认找零地址取 opRetOutIdx + 1）。
	commitment, err := txscript.NullDataScript(f.txHash)
	require.NoError(t, err)
	prevHash := chainhash.DoubleHashH([]byte("e14-prevout-" + symbol))
	spendTx := &wire.MsgTx{Version: 2}
	spendTx.TxIn = append(spendTx.TxIn,
		wire.NewTxIn(&wire.OutPoint{Hash: prevHash, Index: 0}, nil, nil))
	spendTx.TxOut = append(spendTx.TxOut,
		wire.NewTxOut(0, commitment), wire.NewTxOut(1000, []byte("owner-out")))
	buf := bytes.NewBuffer(make([]byte, 0, spendTx.SerializeSizeStripped()))
	require.NoError(t, spendTx.SerializeNoWitness(buf))

	f.pendingUtxo = rtypes.FormatUtxo(prevHash.String(), 0)
	f.ownerUtxo = rtypes.FormatUtxo(spendTx.TxHash().String(), 1)
	f.confirm = &rtypes.ConfirmTx{
		ActionType:    actionType,
		TxBlockHeight: 0,
		TxIndex:       0,
		TxHash:        f.txHash,
		UtxoProof: &rtypes.UtxoSpendingProof{
			SpendingTx:          buf.Bytes(),
			SpendingInputIdx:    0,
			OpRetOutputIdx:      0,
			OpRetOutputPkScript: commitment,
		},
	}

	// localdb 里的 pendingTx：**未确认**（Confirmed = false），即"陈旧 localdb"的复现前提。
	require.NoError(t, local.Set(formatPendingTxKey(0, 0), types.Encode(&rtypes.PendingTx{
		ActionType:    actionType,
		TxBlockHeight: 0,
		TxIndex:       0,
		TxHash:        f.txHash,
		Utxo:          &rtypes.OutPoint{Hash: prevHash.String(), Index: 0},
	})))
	return f
}

// setPayload 写入被确认交易的 payload（checkConfirm 与 Exec_Confirm 都要读它）。
func (f *e14ConfirmFixture) setPayload(t *testing.T, payload types.Message) {
	t.Helper()
	require.NoError(t, f.state.Set(formatPayloadKey(f.txHash), types.Encode(payload)))
}

// fundAsset 给 transfer 分支预备余额：asset key + 被花费 utxo 上的账户余额。
func (f *e14ConfirmFixture) fundAsset(t *testing.T, symbol string, amount int64) {
	t.Helper()
	require.NoError(t, f.state.Set(formatAssetKey(symbol), types.Encode(&rtypes.RgbxAsset{})))
	acc, err := f.r.newAccount(symbol)
	require.NoError(t, err)
	_, err = acc.Mint(f.pendingUtxo, amount)
	require.NoError(t, err)
}

// assertPendingNotConfirmed 复现前提校验：localdb 的确认标记仍是陈旧的 false，
// 因此下面拦截重复确认的只可能是 stateDB 的键，而不是旧守卫。
func (f *e14ConfirmFixture) assertPendingNotConfirmed(t *testing.T) {
	t.Helper()
	pending := &rtypes.PendingTx{}
	require.NoError(t, readDB(f.local, formatPendingTxKey(0, 0), pending))
	require.False(t, pending.Confirmed, "本用例的前提是 localdb 陈旧：pending.Confirmed 必须仍为 false")
}

// Test_rgbx_confirmDedup_mint E14 ① + ②：
//   - 首次 mint 确认成功结算，且"已消费"键随回执 KV 写进 stateDB；
//   - 同一笔确认再来一次（localdb 陈旧、旧守卫放行）⇒ 被 stateDB 键拒绝（ErrMintAlreadyConfirmed）。
func Test_rgbx_confirmDedup_mint(t *testing.T) {
	const symbol = "e14mint"
	f := newE14ConfirmFixture(t, symbol, rtypes.TyMintAction)
	f.setPayload(t, &rtypes.MintAsset{Symbol: symbol, TotalAmount: 7})

	// 首次确认：stateDB 里还没有"已消费"键，checkConfirm 放行
	require.NoError(t, f.r.checkConfirm(testCommitAddr, "txH1", f.confirm))

	// 结算成功 ⇒ 键随回执 KV 写入
	recp := testExec(t, f.r, rtypes.NameConfirmAction, f.confirm, nil, 0)
	require.NotNil(t, recp)
	applyStateKV(t, f.state, recp.KV)

	used, err := f.state.Get(formatConfirmUsedKey(f.txHash))
	require.NoError(t, err)
	require.Equal(t, []byte("used"), used)

	// 铸币生效（Normal 资产落在归属 utxo 的账户余额上）
	acc, err := f.r.newAccount(symbol)
	require.NoError(t, err)
	require.Equal(t, int64(7), acc.LoadAccount(f.ownerUtxo).Balance)

	// 重复确认同一笔：localdb 陈旧（旧守卫不拦），必须由 stateDB 键拦下
	f.assertPendingNotConfirmed(t)
	err = f.r.checkConfirm(testCommitAddr, "txH2", f.confirm)
	require.Equal(t, ErrMintAlreadyConfirmed, err)
	// 必须是 stateDB 键的新错误码，而不是 localdb 那条 —— 否则本用例没证明新守卫生效
	require.NotEqual(t, ErrTxAlreadyConfirmed, err)
	// 桥侧重试语义依赖该子串（neutrino commitPendingTx 视其为幂等成功）
	require.Contains(t, err.Error(), "already confirmed")
}

// Test_rgbx_confirmDedup_transfer E14（transfer 分支，与 mint 对称）：
// transfer 同样只有 localdb 守卫；结算成功写键，重复结算被 ErrTransferAlreadyConfirmed 拒。
func Test_rgbx_confirmDedup_transfer(t *testing.T) {
	const symbol = "e14xfer"
	addr, _ := util.Genaddress()
	f := newE14ConfirmFixture(t, symbol, rtypes.TyTransferAction)
	f.setPayload(t, &rtypes.TransferAsset{
		Symbol: symbol, Amount: 3, FromUtxo: f.pendingUtxo, To: addr,
	})
	f.fundAsset(t, symbol, 10)

	require.NoError(t, f.r.checkConfirm(testCommitAddr, "txH1", f.confirm))

	recp := testExec(t, f.r, rtypes.NameConfirmAction, f.confirm, nil, 0)
	require.NotNil(t, recp)
	applyStateKV(t, f.state, recp.KV)

	used, err := f.state.Get(formatConfirmUsedKey(f.txHash))
	require.NoError(t, err)
	require.Equal(t, []byte("used"), used)

	acc, err := f.r.newAccount(symbol)
	require.NoError(t, err)
	require.Equal(t, int64(0), acc.LoadAccount(f.pendingUtxo).Balance)
	require.Equal(t, int64(3), acc.LoadAccount(addr).Balance)
	require.Equal(t, int64(7), acc.LoadAccount(f.ownerUtxo).Balance)

	f.assertPendingNotConfirmed(t)
	err = f.r.checkConfirm(testCommitAddr, "txH2", f.confirm)
	require.Equal(t, ErrTransferAlreadyConfirmed, err)
}

// Test_rgbx_confirmDedup_freezeDoesNotConsume E14 边界：写入点必须只在**真正结算成功**的回执上。
// OP_RETURN 承诺不成立时 Exec_Confirm 只做标记/冻结、不改变任何资产归属（空回执），
// 此时不能写键 —— 否则用户补一份正确证明后的合法重试会被永久挡掉。
func Test_rgbx_confirmDedup_freezeDoesNotConsume(t *testing.T) {
	const symbol = "e14freeze"
	f := newE14ConfirmFixture(t, symbol, rtypes.TyMintAction)
	f.setPayload(t, &rtypes.MintAsset{Symbol: symbol, TotalAmount: 7})

	// 冻结路径：OP_RETURN 承诺不指向本确认交易 ⇒ 空回执、无状态变更、不写键
	frozen := proto.Clone(f.confirm).(*rtypes.ConfirmTx)
	frozen.UtxoProof.OpRetOutputPkScript = []byte("wrong-commitment")
	frozenRecp := testExec(t, f.r, rtypes.NameConfirmAction, frozen, nil, 0)
	require.Empty(t, frozenRecp.KV, "冻结路径不得产生任何状态变更（含去重键）")
	_, err := f.state.Get(formatConfirmUsedKey(f.txHash))
	require.ErrorIs(t, err, types.ErrNotFound)

	// 补一份正确证明重试：仍然合法，正常结算并写键
	require.NoError(t, f.r.checkConfirm(testCommitAddr, "txH1", f.confirm))
	recp := testExec(t, f.r, rtypes.NameConfirmAction, f.confirm, nil, 0)
	require.NotNil(t, recp)
	applyStateKV(t, f.state, recp.KV)

	used, err := f.state.Get(formatConfirmUsedKey(f.txHash))
	require.NoError(t, err)
	require.Equal(t, []byte("used"), used)
	f.assertPendingNotConfirmed(t)
	require.Equal(t, ErrMintAlreadyConfirmed, f.r.checkConfirm(testCommitAddr, "txH2", f.confirm))
}

// Test_rgbx_confirmUsedKey_multiActionPerSpend E14 键选型论证：
// **同一笔 BTC 花费合法地为两笔不同 chain33 交易（mint + transfer）提供证明**时，
// 两笔确认都必须被放行 —— 键取被确认交易（confirm.GetTxHash()）而不是 BTC 花费的 txid，
// 故不会把这类合法批量结算误拒（checkConfirm 是按 (TxBlockHeight, TxIndex) + 输入下标逐笔校验的，
// Exec_Confirm 的 mint / transfer 分支也共用同一份 UtxoProof）。
// 若键改成花费交易身份（btc txid），本用例第 3 步会被第一笔的键误拒。
func Test_rgbx_confirmUsedKey_multiActionPerSpend(t *testing.T) {
	const symbol = "e14multi"
	addr, _ := util.Genaddress()

	dir, state, local := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	api := &mocks.QueueProtocolAPI{}
	api.On("GetConfig").Return(types.NewChain33Config(types.GetDefaultCfgstring()))
	r := newRgbx().(*rgbx)
	r.SetAPI(api)
	r.SetStateDB(state)
	r.SetLocalDB(local)

	// 一笔 BTC 交易：两个输入各花一个 rgbx utxo，两个 OP_RETURN 各承诺一笔 chain33 交易
	mintHash, transferHash := []byte("multi-mint"), []byte("multi-transfer")
	mintCommit, err := txscript.NullDataScript(mintHash)
	require.NoError(t, err)
	transferCommit, err := txscript.NullDataScript(transferHash)
	require.NoError(t, err)
	prevA := chainhash.DoubleHashH([]byte("multi-prevA"))
	prevB := chainhash.DoubleHashH([]byte("multi-prevB"))
	spendTx := &wire.MsgTx{Version: 2}
	spendTx.TxIn = append(spendTx.TxIn,
		wire.NewTxIn(&wire.OutPoint{Hash: prevA, Index: 0}, nil, nil),
		wire.NewTxIn(&wire.OutPoint{Hash: prevB, Index: 0}, nil, nil))
	spendTx.TxOut = append(spendTx.TxOut,
		wire.NewTxOut(0, mintCommit), wire.NewTxOut(1000, []byte("owner-a")),
		wire.NewTxOut(0, transferCommit), wire.NewTxOut(1000, []byte("owner-b")))
	buf := bytes.NewBuffer(make([]byte, 0, spendTx.SerializeSizeStripped()))
	require.NoError(t, spendTx.SerializeNoWitness(buf))

	mintConfirm := func() *rtypes.ConfirmTx {
		return &rtypes.ConfirmTx{
			ActionType: rtypes.TyMintAction, TxHash: mintHash, TxBlockHeight: 0, TxIndex: 0,
			UtxoProof: &rtypes.UtxoSpendingProof{
				SpendingTx: buf.Bytes(), SpendingInputIdx: 0, OpRetOutputIdx: 0, OpRetOutputPkScript: mintCommit,
			},
		}
	}
	transferConfirm := func() *rtypes.ConfirmTx {
		return &rtypes.ConfirmTx{
			ActionType: rtypes.TyTransferAction, TxHash: transferHash, TxBlockHeight: 1, TxIndex: 0,
			UtxoProof: &rtypes.UtxoSpendingProof{
				SpendingTx: buf.Bytes(), SpendingInputIdx: 1, OpRetOutputIdx: 2, OpRetOutputPkScript: transferCommit,
			},
		}
	}

	require.NoError(t, state.Set(formatPayloadKey(mintHash), types.Encode(&rtypes.MintAsset{Symbol: symbol, TotalAmount: 5})))
	require.NoError(t, local.Set(formatPendingTxKey(0, 0), types.Encode(&rtypes.PendingTx{
		ActionType: rtypes.TyMintAction, TxHash: mintHash,
		Utxo: &rtypes.OutPoint{Hash: prevA.String(), Index: 0},
	})))
	// transfer 的账户 id 与被花费的 btc utxo 对齐（prevB:0）
	require.NoError(t, state.Set(formatPayloadKey(transferHash),
		types.Encode(&rtypes.TransferAsset{Symbol: symbol, Amount: 1, FromUtxo: rtypes.FormatUtxo(prevB.String(), 0), To: addr})))
	require.NoError(t, state.Set(formatAssetKey(symbol), types.Encode(&rtypes.RgbxAsset{})))
	acc, err := r.newAccount(symbol)
	require.NoError(t, err)
	_, err = acc.Mint(rtypes.FormatUtxo(prevB.String(), 0), 5)
	require.NoError(t, err)
	require.NoError(t, local.Set(formatPendingTxKey(1, 0), types.Encode(&rtypes.PendingTx{
		ActionType: rtypes.TyTransferAction, TxHash: transferHash,
		Utxo: &rtypes.OutPoint{Hash: prevB.String(), Index: 0},
	})))

	// 第一笔：mint（输入 0 / OP_RETURN 输出 0）—— 放行并结算，键登记在 mintHash 上
	require.NoError(t, r.checkConfirm(testCommitAddr, "txH1", mintConfirm()))
	applyStateKV(t, state, testExec(t, r, rtypes.NameConfirmAction, mintConfirm(), nil, 0).KV)
	used, err := state.Get(formatConfirmUsedKey(mintHash))
	require.NoError(t, err)
	require.Equal(t, []byte("used"), used)

	// 第二笔：**同一个 BTC 花费**、另一笔 chain33 交易（输入 1 / OP_RETURN 输出 2）—— 必须仍被放行
	// （这一条就是键选型的验收点：若键改成花费交易身份，这里会被第一笔的键误拒）
	require.NoError(t, r.checkConfirm(testCommitAddr, "txH2", transferConfirm()))

	// mint 的重复确认被拒（键登记在 mintHash 上，与 transfer 无关）
	require.Equal(t, ErrMintAlreadyConfirmed, r.checkConfirm(testCommitAddr, "txH3", mintConfirm()))

	// transfer 结算并登记自己的键后，它的重复确认同样被拒，且两笔的键互不影响
	applyStateKV(t, state, testExec(t, r, rtypes.NameConfirmAction, transferConfirm(), nil, 0).KV)
	usedTransfer, err := state.Get(formatConfirmUsedKey(transferHash))
	require.NoError(t, err)
	require.Equal(t, []byte("used"), usedTransfer)
	require.Equal(t, ErrTransferAlreadyConfirmed, r.checkConfirm(testCommitAddr, "txH4", transferConfirm()))
}
