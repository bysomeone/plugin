package executor

import (
	"testing"

	"github.com/33cn/chain33/client/mocks"
	"github.com/33cn/chain33/types"
	"github.com/33cn/chain33/util"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/stretchr/testify/require"
)

func Test_rgbx_transferAsset(t *testing.T) {
	r := newRgbx()
	addr, _ := util.Genaddress()
	utxoAddr := "74503993e7c8d4280f6fbb99ae5aaa92231a1981a358e40f97e2b4f4dfbea13c:0"
	spendHash := chainhash.DoubleHashH([]byte("spend")).String()

	dir, state, _ := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	api := &mocks.QueueProtocolAPI{}
	r.SetAPI(api)
	cfg := types.NewChain33Config(types.GetDefaultCfgstring())
	api.On("GetConfig").Return(cfg)
	r.SetStateDB(state)

	var err error
	require.NoError(t, state.Set(formatPayloadKey([]byte("xc")), types.Encode(&rtypes.TransferAsset{
		Symbol: "xn",
	})))
	_, err = r.(*rgbx).transferAsset(
		&rtypes.ConfirmTx{
			TxHash:    []byte("xc"),
			UtxoProof: &rtypes.UtxoSpendingProof{OpRetOutputIdx: 0},
		},
		"txH", "cH", spendHash,
	)
	require.Equal(t, types.ErrNotSupport, err)

	_, err = r.(*rgbx).transferAsset(
		&rtypes.ConfirmTx{
			TxHash:    []byte("missing"),
			UtxoProof: &rtypes.UtxoSpendingProof{OpRetOutputIdx: 0},
		},
		"txH", "cH", spendHash,
	)
	require.Error(t, err)

	require.NoError(t, state.Set(formatPayloadKey([]byte("p0")), types.Encode(&rtypes.TransferAsset{
		Symbol: "nosuch", Amount: 1, FromUtxo: utxoAddr, To: addr,
	})))
	_, err = r.(*rgbx).transferAsset(
		&rtypes.ConfirmTx{TxHash: []byte("p0"), UtxoProof: &rtypes.UtxoSpendingProof{OpRetOutputIdx: 0}},
		"txH", "cH", spendHash,
	)
	require.Equal(t, ErrAssetNotExist, err)

	require.NoError(t, state.Set(formatAssetKey("col"), types.Encode(&rtypes.RgbxAsset{Symbol: "col", Type: 1})))
	require.NoError(t, state.Set(formatPayloadKey([]byte("p1")), types.Encode(&rtypes.TransferAsset{
		Symbol: "col", To: addr, Amount: 1,
	})))
	recp, err := r.(*rgbx).transferAsset(
		&rtypes.ConfirmTx{TxHash: []byte("p1"), UtxoProof: &rtypes.UtxoSpendingProof{OpRetOutputIdx: 0}},
		"txH", "cH", spendHash,
	)
	require.NoError(t, err)
	require.NotEmpty(t, recp.KV)

	require.NoError(t, state.Set(formatAssetKey("norm"), types.Encode(&rtypes.RgbxAsset{})))
	acc, err := r.(*rgbx).newAccount("norm")
	require.NoError(t, err)
	_, err = acc.Mint(utxoAddr, 5)
	require.NoError(t, err)
	require.NoError(t, state.Set(formatPayloadKey([]byte("p2")), types.Encode(&rtypes.TransferAsset{
		Symbol: "norm", Amount: 5, FromUtxo: utxoAddr, To: addr,
	})))
	recp, err = r.(*rgbx).transferAsset(
		&rtypes.ConfirmTx{TxHash: []byte("p2"), UtxoProof: &rtypes.UtxoSpendingProof{OpRetOutputIdx: 0}},
		"txH", "cH", spendHash,
	)
	require.NoError(t, err)
	require.NotEmpty(t, recp.KV)

	require.NoError(t, state.Set(formatPayloadKey([]byte("p3")), types.Encode(&rtypes.TransferAsset{
		Symbol: "norm", Amount: 3, FromUtxo: utxoAddr, To: addr, ChangeAddr: addr,
	})))
	_, err = acc.Mint(utxoAddr, 10)
	require.NoError(t, err)
	recp, err = r.(*rgbx).transferAsset(
		&rtypes.ConfirmTx{TxHash: []byte("p3"), UtxoProof: &rtypes.UtxoSpendingProof{OpRetOutputIdx: 0}},
		"txH", "cH", spendHash,
	)
	require.NoError(t, err)
	require.NotEmpty(t, recp.KV)

	require.NoError(t, state.Set(formatPayloadKey([]byte("p4")), types.Encode(&rtypes.TransferAsset{
		Symbol: "norm", Amount: 9999, FromUtxo: utxoAddr, To: addr,
	})))
	_, err = r.(*rgbx).transferAsset(
		&rtypes.ConfirmTx{TxHash: []byte("p4"), UtxoProof: &rtypes.UtxoSpendingProof{OpRetOutputIdx: 0}},
		"txH", "cH", spendHash,
	)
	require.Error(t, err)
}

func Test_rgbx_transferAsset_defaultChangeUtxo(t *testing.T) {
	r := newRgbx()
	addr, _ := util.Genaddress()
	utxoAddr := "74503993e7c8d4280f6fbb99ae5aaa92231a1981a358e40f97e2b4f4dfbea13c:0"
	spendHash := chainhash.DoubleHashH([]byte("x")).String()
	changeUtxo := rtypes.FormatUtxo(spendHash, 1)

	dir, state, _ := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	api := &mocks.QueueProtocolAPI{}
	r.SetAPI(api)
	api.On("GetConfig").Return(types.NewChain33Config(types.GetDefaultCfgstring()))
	r.SetStateDB(state)

	require.NoError(t, state.Set(formatAssetKey("norm2"), types.Encode(&rtypes.RgbxAsset{})))
	acc, err := r.(*rgbx).newAccount("norm2")
	require.NoError(t, err)
	_, err = acc.Mint(utxoAddr, 10)
	require.NoError(t, err)
	require.NoError(t, state.Set(formatPayloadKey([]byte("pch")), types.Encode(&rtypes.TransferAsset{
		Symbol: "norm2", Amount: 3, FromUtxo: utxoAddr, To: addr,
	})))
	recp, err := r.(*rgbx).transferAsset(
		&rtypes.ConfirmTx{
			TxHash:    []byte("pch"),
			UtxoProof: &rtypes.UtxoSpendingProof{OpRetOutputIdx: 0},
		},
		"txH", "cH", spendHash,
	)
	require.NoError(t, err)
	require.NotEmpty(t, recp.KV)
	require.Equal(t, int64(7), acc.LoadAccount(changeUtxo).Balance)
}

func Test_rgbx_confirmWithdrawSettlement(t *testing.T) {
	r := newRgbx()
	wHash := []byte("withdraw1")

	dir, state, _ := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	api := &mocks.QueueProtocolAPI{}
	r.SetAPI(api)
	api.On("GetConfig").Return(types.NewChain33Config(types.GetDefaultCfgstring()))
	r.SetStateDB(state)

	_, err := r.(*rgbx).confirmWithdrawSettlement(
		&rtypes.ConfirmTx{TxHash: wHash},
		"txH", "cH",
	)
	require.Error(t, err)

	// confirmWithdrawSettlement 使用 newAccount，symbol 直接为 "BTC"
	withdraw := &rtypes.WithdrawAsset{AssetSymbol: "BTC", Amount: 100}
	require.NoError(t, state.Set(formatPayloadKey(wHash), types.Encode(withdraw)))
	acc, err := r.(*rgbx).newAccount("xBTC")
	require.NoError(t, err)
	lockAddr := r.(*rgbx).crossChainLockAddress(acc)
	_, err = acc.Mint(lockAddr, 100)
	require.NoError(t, err)

	recp, err := r.(*rgbx).confirmWithdrawSettlement(
		&rtypes.ConfirmTx{TxHash: wHash},
		"txH", "cH",
	)
	require.NoError(t, err)
	require.NotNil(t, recp)

	wHash2 := []byte("withdraw2")
	require.NoError(t, state.Set(formatPayloadKey(wHash2), types.Encode(&rtypes.WithdrawAsset{AssetSymbol: "BTC", Amount: 500})))
	_, err = r.(*rgbx).confirmWithdrawSettlement(
		&rtypes.ConfirmTx{TxHash: wHash2},
		"txH", "cH",
	)
	require.Error(t, err)
}

// Test_rgbx_confirmWithdrawSettlement_usedKey S3：提现结算成功登记 consumed 集合，
// 同一 burn 二次结算被 checkWithdrawConfirm 拒绝，不同 burn 互不影响（与充值侧 deposited- 对称）。
func Test_rgbx_confirmWithdrawSettlement_usedKey(t *testing.T) {
	r := newRgbx()
	burnA, burnB := []byte("burnA"), []byte("burnB")

	dir, state, _ := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	api := &mocks.QueueProtocolAPI{}
	r.SetAPI(api)
	api.On("GetConfig").Return(types.NewChain33Config(types.GetDefaultCfgstring()))
	r.SetStateDB(state)

	// 两笔待结算提现共用锁仓地址：A=100, B=150
	require.NoError(t, state.Set(formatPayloadKey(burnA), types.Encode(&rtypes.WithdrawAsset{AssetSymbol: "BTC", Amount: 100})))
	require.NoError(t, state.Set(formatPayloadKey(burnB), types.Encode(&rtypes.WithdrawAsset{AssetSymbol: "BTC", Amount: 150})))
	acc, err := r.(*rgbx).newAccount("xBTC")
	require.NoError(t, err)
	lockAddr := r.(*rgbx).crossChainLockAddress(acc)
	_, err = acc.Mint(lockAddr, 250)
	require.NoError(t, err)

	// 单次结算：成功，且只销毁该 burn 的锁定额度，consumed(burnA) 被登记
	recpA, err := r.(*rgbx).confirmWithdrawSettlement(&rtypes.ConfirmTx{TxHash: burnA}, "txA", "cA")
	require.NoError(t, err)
	require.NotNil(t, recpA)
	applyStateKV(t, state, recpA.KV)
	require.Equal(t, int64(150), acc.LoadAccount(lockAddr).GetBalance())

	used, err := state.Get(formatWithdrawUsedKey(burnA))
	require.NoError(t, err)
	require.Equal(t, []byte("used"), used)

	// 守卫：同一 burn 被拒（明确错误码）；不同 burn 放行（继续走 SPV 校验）
	err = r.(*rgbx).checkWithdrawConfirm("txA2", "cA2", &rtypes.ConfirmTx{TxHash: burnA}, &rtypes.PendingTx{AssetSymbol: "BTC"})
	require.Equal(t, ErrWithdrawAlreadyConfirmed, err)
	err = r.(*rgbx).checkWithdrawConfirm("txB", "cB", &rtypes.ConfirmTx{TxHash: burnB}, &rtypes.PendingTx{AssetSymbol: "BTC"})
	require.Equal(t, ErrInvalidBtcTxProof, err)

	// 另一笔 burn 仍可正常结算
	recpB, err := r.(*rgbx).confirmWithdrawSettlement(&rtypes.ConfirmTx{TxHash: burnB}, "txB", "cB")
	require.NoError(t, err)
	applyStateKV(t, state, recpB.KV)
	require.Equal(t, int64(0), acc.LoadAccount(lockAddr).GetBalance())

	// 升级兼容：consumed 集合是"只增不减的拒绝条件"，未登记的历史 burn 一律不受影响
	_, err = state.Get(formatWithdrawUsedKey(burnB))
	require.NoError(t, err)
	unused, err := state.Get(formatWithdrawUsedKey([]byte("never-settled")))
	require.ErrorIs(t, err, types.ErrNotFound)
	require.Nil(t, unused)
}
