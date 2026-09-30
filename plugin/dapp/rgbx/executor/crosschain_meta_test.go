package executor

import (
	"testing"

	"github.com/33cn/chain33/types"
	"github.com/33cn/chain33/util"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/stretchr/testify/require"
)

// Test_CrossChainMeta_backfill 精度回填（crossChainAssetMeta）的链上语义：
// **一次性**、只接受"尚未回填"或"幂等地回填同一个值"、只有 guardian 能提交。
//
// 这条动作的存在理由：RGB20 资产的精度只能来自合约，而合约在 CommitDKG 那一刻还没被侧车核对
// 出来，且 CommitDKG 一个 symbol 只提交一次（不能二次补写）。所以精度必须有一条单独的回填路径，
// 且它的判据必须全落在共识状态上（提交者白名单 + 记录既有值 + 精度上界）。
func Test_CrossChainMeta_backfill(t *testing.T) {
	r := newRgbx().(*rgbx)
	tx := &types.Transaction{}
	tx.Sign(types.SECP256K1, testPriv) // from == testCommitAddr（guardian）
	require.Equal(t, testCommitAddr, tx.From())

	dir, state, _ := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	r.SetAPI(mockGuardianAPI(t, testCommitAddr))
	r.SetStateDB(state)

	pub := newP2WSHDepositFixture(t, frozenVectorA).tssPub
	// 记录已由 CommitDKG 建好：类别 RGB20、精度是"尚未回填"哨兵。
	setCrossChainInfoForTest(t, state, rtypes.RGB20USDTSymbol, rtypes.CrossChainAssetRgb20,
		rtypes.PrecisionUnknown, pub)

	readPrecision := func() uint32 {
		t.Helper()
		info := &rtypes.CrossChainInfo{}
		require.NoError(t, readDB(r.GetStateDB(), formatCrossChainInfoKey(rtypes.RGB20USDTSymbol), info))
		return info.GetPrecision()
	}
	require.Equal(t, rtypes.PrecisionUnknown, readPrecision())

	// 1) 首次回填：0..MaxPrecision 都是合法值（0 是"不可分割"这个真实精度，不是"未知"）。
	meta := &rtypes.CrossChainAssetMeta{AssetSymbol: rtypes.RGB20USDTSymbol, Precision: 6}
	require.NoError(t, r.checkCrossChainMeta("tx", tx.From(), meta))
	receipt, err := r.Exec_CrossChainMeta(meta, tx, 0)
	require.NoError(t, err)
	applyStateKV(t, state, receipt.KV)
	require.Equal(t, uint32(6), readPrecision())
	// 回执日志带载荷：按区块能查"精度被写成了什么、哪个符号"（KV 只留最终状态）。
	require.Len(t, receipt.Logs, 1)
	require.Equal(t, int32(rtypes.TyCrossChainMetaLog), receipt.Logs[0].Ty)
	logged := &rtypes.CrossChainAssetMeta{}
	require.NoError(t, types.Decode(receipt.Logs[0].Log, logged))
	require.Equal(t, meta.GetPrecision(), logged.GetPrecision())
	require.Equal(t, meta.GetAssetSymbol(), logged.GetAssetSymbol())

	// 2) 幂等重发同一个值：接受（多个 guardian 各自核对后都会提交一次，不能因此互相打架）。
	require.NoError(t, r.checkCrossChainMeta("tx2", tx.From(), meta))
	receipt, err = r.Exec_CrossChainMeta(meta, tx, 1)
	require.NoError(t, err)
	applyStateKV(t, state, receipt.KV)
	require.Equal(t, uint32(6), readPrecision())

	// 3) 回填成**别的**值：拒绝。写入即不可改写 —— 否则"链上精度"就成了"最后一次提交说了算"，
	//    前端/CLI 拿它换算金额时没有任何稳定性可言（真正的分歧由桥侧核对时 fail-closed 暴露）。
	other := &rtypes.CrossChainAssetMeta{AssetSymbol: rtypes.RGB20USDTSymbol, Precision: 8}
	require.Equal(t, ErrCrossChainPrecisionAlreadySet, r.checkCrossChainMeta("tx3", tx.From(), other))
	_, err = r.Exec_CrossChainMeta(other, tx, 2)
	require.Equal(t, ErrCrossChainPrecisionAlreadySet, err)
	require.Equal(t, uint32(6), readPrecision(), "被拒的回填不得改动链上值")
}

// Test_CrossChainMeta_rejections 回填的各类拒绝：提交者、精度上界/哨兵、类别、记录是否存在。
func Test_CrossChainMeta_rejections(t *testing.T) {
	r := newRgbx().(*rgbx)
	dir, state, _ := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	r.SetAPI(mockGuardianAPI(t, testCommitAddr))
	r.SetStateDB(state)

	pub := newP2WSHDepositFixture(t, frozenVectorA).tssPub
	setCrossChainInfoForTest(t, state, rtypes.RGB20USDTSymbol, rtypes.CrossChainAssetRgb20,
		rtypes.PrecisionUnknown, pub)
	// 一条 BTC 类记录：精度由 CommitDKG 写死，不需要也不允许回填。
	setCrossChainInfoForTest(t, state, "btc", rtypes.CrossChainAssetBtc, rtypes.BTCChainPrecision, pub)

	guardianTx := &types.Transaction{}
	guardianTx.Sign(types.SECP256K1, testPriv)

	// 精度非法（哨兵本身 / 超出链上口径）一律拒：拿哨兵去算 10^precision 会溢出或静默算错钱。
	for _, p := range []uint32{rtypes.PrecisionUnknown, rtypes.MaxPrecision + 1} {
		err := r.checkCrossChainMeta("tx", testCommitAddr, &rtypes.CrossChainAssetMeta{
			AssetSymbol: rtypes.RGB20USDTSymbol, Precision: p,
		})
		require.Equal(t, ErrInvalidAssetPrecision, err, "precision=%d", p)
	}

	// BTC 类资产的精度不是回填出来的（建记录时就是协议常量），对它的回填一律拒。
	require.Equal(t, ErrInvalidAssetKind, r.checkCrossChainMeta("tx", testCommitAddr,
		&rtypes.CrossChainAssetMeta{AssetSymbol: "btc", Precision: 6}))

	// 记录不存在：回填没有对象（顺序上它应该先被 CommitDKG 建出来）。
	require.Equal(t, ErrGetCrossChainInfo, r.checkCrossChainMeta("tx", testCommitAddr,
		&rtypes.CrossChainAssetMeta{AssetSymbol: "NOSUCHASSET", Precision: 6}))

	// 非 guardian 提交：精度在链上无法自证，只能由受信集合登记，否则任何人都能把某个资产的
	// 精度改成别的值，让前端/CLI 静默算错金额。
	otherAddr, _ := util.Genaddress()
	require.Equal(t, ErrInvalidGuardianCommitter, r.checkCrossChainMeta("tx", otherAddr,
		&rtypes.CrossChainAssetMeta{AssetSymbol: rtypes.RGB20USDTSymbol, Precision: 6}))

	// 符号同 checkCommitDKG 一样走字符集校验（别名化 symbol 会写到别的资产的 key 上）。
	require.Equal(t, ErrInvalidAssetSymbol, r.checkCrossChainMeta("tx", testCommitAddr,
		&rtypes.CrossChainAssetMeta{AssetSymbol: "rgb20_usdtſ", Precision: 6}))
}

// Test_Exec_CommitDKG_registersKindAndPrecision CommitDKG 建记录时必须把类别与精度一起写进去 ——
// 它们是前端 / CLI / 执行器判定分支与换算金额的唯一依据。
func Test_Exec_CommitDKG_registersKindAndPrecision(t *testing.T) {
	r := newRgbx().(*rgbx)
	dir, state, _ := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	r.SetAPI(mockGuardianAPI(t, testCommitAddr))
	r.SetStateDB(state)

	addr, pkScript, pub := newTestnetWitnessAddrAndPub(t)

	newCommit := func(symbol string, kind rtypes.CrossChainAssetKind, precision uint32) {
		commit := &rtypes.CommitDKG{
			AssetSymbol: symbol, DkgAddress: addr, PkScript: pkScript, Pubkey: pub,
			AssetKind: uint32(kind), Precision: precision,
		}
		tx, err := r.GetExecutorType().CreateTransaction(rtypes.NameCommitDKGAction, commit)
		require.NoError(t, err)
		tx.Sign(types.SECP256K1, testPriv)
		receipt, err := r.Exec(tx, 0)
		require.NoError(t, err)
		applyStateKV(t, state, receipt.KV)
	}

	newCommit("BTC", rtypes.CrossChainAssetBtc, rtypes.BTCChainPrecision)
	info := &rtypes.CrossChainInfo{}
	require.NoError(t, readDB(r.GetStateDB(), formatCrossChainInfoKey("BTC"), info))
	require.Equal(t, uint32(rtypes.CrossChainAssetBtc), info.GetAssetKind())
	require.Equal(t, rtypes.BTCChainPrecision, info.GetPrecision())
	require.False(t, rtypes.IsRgb20CrossChainAsset(info))

	newCommit(rtypes.RGB20USDTSymbol, rtypes.CrossChainAssetRgb20, rtypes.PrecisionUnknown)
	require.NoError(t, readDB(r.GetStateDB(), formatCrossChainInfoKey(rtypes.RGB20USDTSymbol), info))
	require.True(t, rtypes.IsRgb20CrossChainAsset(info), "类别按载荷登记，不再按符号字面量推断")
	require.Equal(t, rtypes.PrecisionUnknown, info.GetPrecision(), "回填前必须是哨兵，而不是猜一个值")
}
