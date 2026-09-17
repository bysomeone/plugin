package executor

import (
	"bytes"
	"testing"

	"github.com/33cn/chain33/client/mocks"
	"github.com/33cn/chain33/common/db"
	"github.com/33cn/chain33/system/dapp"
	"github.com/33cn/chain33/types"
	"github.com/33cn/chain33/util"
	paratypes "github.com/33cn/plugin/plugin/dapp/paracross/types"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func testExec(t *testing.T, driver dapp.Driver, actionName string, action types.Message, expectErr error, index int) *types.Receipt {

	tx, err := driver.GetExecutorType().CreateTransaction(actionName, action)
	require.Nilf(t, err, "testcase %d", index)
	tx.Sign(types.SECP256K1, testPriv)
	recp, err := driver.Exec(tx, 0)
	require.Equalf(t, expectErr, err, "testcase %d", index)
	return recp
}

func TestRgbx_Exec_Mint(t *testing.T) {

	r := newRgbx()
	mint := &rtypes.MintAsset{}
	testExec(t, r, rtypes.NameMintAction, mint, nil, 0)
}

func TestRgbx_Exec_Transfer(t *testing.T) {

	r := newRgbx()
	addr2, _ := util.Genaddress()
	utxoAddr := "74503993e7c8d4280f6fbb99ae5aaa92231a1981a358e40f97e2b4f4dfbea13c:0"
	tcArr := []*testCase{
		{
			expectErr: nil,
			action:    &rtypes.TransferAsset{FromUtxo: utxoAddr},
		},
		{
			expectErr: ErrAssetNotExist,
			action:    &rtypes.TransferAsset{Symbol: "test"},
		},
		{
			expectErr: nil,
			action:    &rtypes.TransferAsset{Symbol: "collect"},
		},
		{
			expectErr: nil,
			action:    &rtypes.TransferAsset{Symbol: "normal", Amount: 1},
		},
		{
			expectErr: types.ErrNoBalance,
			action:    &rtypes.TransferAsset{Symbol: "normal", Amount: 1},
		},
		{
			expectErr: nil,
			action:    &rtypes.TransferAsset{Symbol: "xbtc", To: addr2, Amount: 1},
		},
	}

	dir, state, _ := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	api := &mocks.QueueProtocolAPI{}
	r.SetAPI(api)
	cfg := types.NewChain33Config(types.GetDefaultCfgstring())
	api.On("GetConfig").Return(cfg)
	r.SetStateDB(state)
	require.Nil(t, state.Set(formatAssetKey("normal"), types.Encode(&rtypes.RgbxAsset{})))
	require.Nil(t, state.Set(formatAssetKey("collect"), types.Encode(&rtypes.RgbxAsset{Type: 1})))

	acc, err := r.(*rgbx).newAccount("normal")
	require.Nil(t, err)
	_, err = acc.Mint(testCommitAddr, 1)
	require.Nil(t, err)
	crossAcc, err := r.(*rgbx).newAccount("xbtc")
	require.Nil(t, err)
	_, err = crossAcc.Mint(testCommitAddr, 1)
	require.Nil(t, err)

	for idx, tc := range tcArr {
		testExec(t, r, rtypes.NameTransferAction, tc.action, tc.expectErr, idx)
	}
}

func TestRgbx_Exec_Confirm(t *testing.T) {

	r := newRgbx()
	addr, _ := util.Genaddress()
	utxoAddr := "74503993e7c8d4280f6fbb99ae5aaa92231a1981a358e40f97e2b4f4dfbea13c:0"
	normal, normal1, collect, collect1 := "normal", "normal1", "collect", "collect1"

	mintScript, _ := txscript.NullDataScript([]byte(normal))
	mintScript1, _ := txscript.NullDataScript([]byte(collect))
	transferScript, _ := txscript.NullDataScript([]byte(normal1))
	transferScript1, _ := txscript.NullDataScript([]byte(collect1))

	// A2：归属 utxo id 取解析后交易的 txid，故 SpendingTx 必须是规范编码的真实交易字节。
	spendTx := &wire.MsgTx{Version: 2}
	spendTx.TxIn = append(spendTx.TxIn,
		wire.NewTxIn(&wire.OutPoint{Hash: chainhash.DoubleHashH([]byte("confirm-outs")), Index: 0}, nil, nil))
	spendTx.TxOut = append(spendTx.TxOut,
		wire.NewTxOut(0, []byte("opret")), wire.NewTxOut(1000, []byte("owner")))
	spendBuf := bytes.NewBuffer(make([]byte, 0, spendTx.SerializeSizeStripped()))
	require.Nil(t, spendTx.SerializeNoWitness(spendBuf))
	spendRaw := spendBuf.Bytes()
	ownerUtxo := rtypes.FormatUtxo(spendTx.TxHash().String(), 1)

	tcArr := []*testCase{
		{
			expectErr: nil,
			action:    &rtypes.ConfirmTx{Timeout: true},
		},
		{
			expectErr: nil,
			action:    &rtypes.ConfirmTx{UtxoProof: &rtypes.UtxoSpendingProof{SpendingTx: spendRaw}},
		},
		{
			expectErr: nil,
			action:    &rtypes.ConfirmTx{ActionType: rtypes.TyMintAction, TxHash: []byte(normal), UtxoProof: &rtypes.UtxoSpendingProof{SpendingTx: spendRaw, OpRetOutputPkScript: mintScript}},
		},
		{
			expectErr: nil,
			action:    &rtypes.ConfirmTx{ActionType: rtypes.TyMintAction, TxHash: []byte(collect), UtxoProof: &rtypes.UtxoSpendingProof{SpendingTx: spendRaw, OpRetOutputPkScript: mintScript1}},
		},
		{
			expectErr: nil,
			action:    &rtypes.ConfirmTx{TxHash: []byte(normal1), UtxoProof: &rtypes.UtxoSpendingProof{SpendingTx: spendRaw, OpRetOutputPkScript: transferScript}},
		},
		{
			expectErr: nil,
			action:    &rtypes.ConfirmTx{TxHash: []byte(collect1), UtxoProof: &rtypes.UtxoSpendingProof{SpendingTx: spendRaw, OpRetOutputPkScript: transferScript1}},
		},
	}

	dir, state, _ := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	api := &mocks.QueueProtocolAPI{}
	r.SetAPI(api)
	cfg := types.NewChain33Config(types.GetDefaultCfgstring())
	api.On("GetConfig").Return(cfg)
	r.SetStateDB(state)

	require.Nil(t, state.Set(formatPayloadKey([]byte(normal)), types.Encode(&rtypes.MintAsset{Symbol: normal, TotalAmount: 1})))
	require.Nil(t, state.Set(formatPayloadKey([]byte(collect)), types.Encode(&rtypes.MintAsset{Symbol: collect, Type: 1, TotalAmount: 1})))

	require.Nil(t, state.Set(formatAssetKey(normal1), types.Encode(&rtypes.RgbxAsset{})))
	require.Nil(t, state.Set(formatAssetKey(collect1), types.Encode(&rtypes.RgbxAsset{Type: 1, Symbol: collect1})))

	require.Nil(t, state.Set(formatPayloadKey([]byte(normal1)),
		types.Encode(&rtypes.TransferAsset{Symbol: normal1, Amount: 1, FromUtxo: utxoAddr, To: addr})))
	require.Nil(t, state.Set(formatPayloadKey([]byte(collect1)),
		types.Encode(&rtypes.TransferAsset{Symbol: collect1, To: addr})))

	accDB, err := r.(*rgbx).newAccount(normal1)
	require.Nil(t, err)
	_, err = accDB.Mint(utxoAddr, 2)
	require.Nil(t, err)

	for idx, tc := range tcArr {
		recp := testExec(t, r, rtypes.NameConfirmAction, tc.action, tc.expectErr, idx)
		if len(recp.GetKV()) > 0 {
			util.SaveKVList(state, recp.KV)
		}
	}
	// check mint
	asset := &rtypes.RgbxAsset{}
	require.Nil(t, readDB(state, formatAssetKey(normal), asset))
	require.Equal(t, formatSymbol(normal), asset.Symbol)
	require.Nil(t, readDB(state, formatAssetKey(collect), asset))
	require.Equal(t, formatSymbol(collect), asset.Symbol)
	require.Equal(t, rtypes.Collectible, rtypes.AssetType(asset.Type))
	// 归属 id = 解析后 spending tx 的规范 txid + opReturn 后一个输出索引
	require.Equal(t, ownerUtxo, asset.Owner)

	// check transfer
	require.Nil(t, readDB(state, formatAssetKey(collect1), asset))
	require.Equal(t, addr, asset.Owner)
	require.Equal(t, int64(0), accDB.LoadAccount(utxoAddr).Balance)
	require.Equal(t, int64(1), accDB.LoadAccount(addr).Balance)
	require.Equal(t, int64(1), accDB.LoadAccount(ownerUtxo).Balance)
}

// TestRgbx_Exec_Confirm_ownerIsCanonicalTxID A2 验收：
//   - 归属 utxo id 与解析后 SpendingTx 的规范身份（btc txid）一致，而不是原始字节的 DoubleHashH；
//   - 同一笔花费的"另一份编码"（尾部追加字节）不再改变归属：Exec 不产生任何状态变更（冻结），
//     CheckTx 侧则直接拒绝（见 Test_checkConfirm 的 ErrNonCanonicalSpendingTx 用例）。
func TestRgbx_Exec_Confirm_ownerIsCanonicalTxID(t *testing.T) {
	symbol := "owner1"
	mintScript, _ := txscript.NullDataScript([]byte(symbol))

	spendTx := &wire.MsgTx{Version: 2}
	spendTx.TxIn = append(spendTx.TxIn,
		wire.NewTxIn(&wire.OutPoint{Hash: chainhash.DoubleHashH([]byte("a2-prevout")), Index: 0}, nil, nil))
	spendTx.TxOut = append(spendTx.TxOut,
		wire.NewTxOut(0, []byte("opret")), wire.NewTxOut(1000, []byte("owner")))
	spendBuf := bytes.NewBuffer(make([]byte, 0, spendTx.SerializeSizeStripped()))
	require.Nil(t, spendTx.SerializeNoWitness(spendBuf))
	raw := spendBuf.Bytes()
	appended := append(append([]byte{}, raw...), 0x00, 0x01)

	ownerByTxID := rtypes.FormatUtxo(spendTx.TxHash().String(), 1)
	// 复现前提：规范编码下 txid 与原始字节哈希一致；而"尾部追加字节"的那份编码在旧口径下
	// 会得到另一个 owner id —— 同一笔花费被记到另一个 owner 名下（本次修复要消除的差异）。
	require.Equal(t, ownerByTxID, rtypes.FormatUtxo(chainhash.DoubleHashH(raw).String(), 1))
	require.NotEqual(t, ownerByTxID, rtypes.FormatUtxo(chainhash.DoubleHashH(appended).String(), 1))

	run := func(t *testing.T, spendingTx []byte) (*types.Receipt, db.DB, *rgbx) {
		t.Helper()
		r := newRgbx().(*rgbx)
		dir, state, _ := util.CreateTestDB()
		t.Cleanup(func() { util.CloseTestDB(dir, state) })
		api := &mocks.QueueProtocolAPI{}
		r.SetAPI(api)
		api.On("GetConfig").Return(types.NewChain33Config(types.GetDefaultCfgstring()))
		r.SetStateDB(state)
		require.Nil(t, state.Set(formatPayloadKey([]byte(symbol)),
			types.Encode(&rtypes.MintAsset{Symbol: symbol, TotalAmount: 1})))

		recp := testExec(t, r, rtypes.NameConfirmAction, &rtypes.ConfirmTx{
			ActionType: rtypes.TyMintAction,
			TxHash:     []byte(symbol),
			UtxoProof: &rtypes.UtxoSpendingProof{
				SpendingTx:          spendingTx,
				OpRetOutputPkScript: mintScript,
			},
		}, nil, 0)
		util.SaveKVList(state, recp.KV)
		return recp, state, r
	}

	t.Run("canonical", func(t *testing.T) {
		_, state, r := run(t, raw)
		asset := &rtypes.RgbxAsset{}
		require.Nil(t, readDB(state, formatAssetKey(symbol), asset))
		require.Equal(t, spendTx.TxHash().String(), asset.GenesisBtcTxHash)
		// Normal 资产的归属落在账户余额上（assetReceipt 只对 Collectible 写 Owner）
		acc, err := r.newAccount(symbol)
		require.Nil(t, err)
		require.Equal(t, int64(1), acc.LoadAccount(ownerByTxID).Balance)
	})

	t.Run("trailing-bytes-freezes", func(t *testing.T) {
		recp, state, r := run(t, appended)
		require.NotNil(t, recp)
		require.Empty(t, recp.KV, "非规范编码不得产生任何状态变更")
		asset := &rtypes.RgbxAsset{}
		require.Error(t, readDB(state, formatAssetKey(symbol), asset))
		acc, err := r.newAccount(symbol)
		require.Nil(t, err)
		require.Equal(t, int64(0), acc.LoadAccount(ownerByTxID).Balance)
	})
}

func TestRgbx_Exec_Deposit(t *testing.T) {
	r := newRgbx()
	deposit := &rtypes.DepositAsset{
		Amount:         100,
		DepositAddress: testCommitAddr,
		AssetSymbol:    "btc",
		TxProof:        &rtypes.BtcTxProof{},
	}
	dir, state, _ := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	api := &mocks.QueueProtocolAPI{}
	r.SetAPI(api)
	cfg := types.NewChain33Config(types.GetDefaultCfgstring())
	api.On("GetConfig").Return(cfg)
	r.SetStateDB(state)
	testExec(t, r, rtypes.NameDepositAssetAction, deposit, nil, 0)
}

func TestRgbx_Exec_Withdraw(t *testing.T) {
	r := newRgbx()
	withdraw := &rtypes.WithdrawAsset{
		Amount:          600,
		FeeRate:         10,
		DestinationAddr: "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kygt080",
		AssetSymbol:     "BTC",
	}
	dir, state, _ := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	api := &mocks.QueueProtocolAPI{}
	r.SetAPI(api)
	cfg := types.NewChain33Config(types.GetDefaultCfgstring())
	api.On("GetConfig").Return(cfg)
	r.SetStateDB(state)
	// withdraw 使用 newAccount 直接操作账户，symbol 为 "BTC"
	// 但为了测试通过，需要设置 cross chain info 和账户余额
	require.NoError(t, state.Set(formatCrossChainInfoKey("BTC"), types.Encode(&rtypes.CrossChainInfo{AssetSymbol: "BTC"})))
	acc, err := r.(*rgbx).newAccount("xBTC")
	require.Nil(t, err)
	_, err = acc.Mint(testCommitAddr, 1000)
	require.Nil(t, err)
	testExec(t, r, rtypes.NameWithdrawAssetAction, withdraw, nil, 0)
}

func TestRgbx_Exec_CommitDKG(t *testing.T) {
	r := newRgbx()
	dkgAddr, pkScript, pubkey := newTestnetWitnessAddrAndPub(t)
	commit := &rtypes.CommitDKG{AssetSymbol: "btc", DkgAddress: dkgAddr, PkScript: pkScript, Pubkey: pubkey}

	dir, state, _ := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	api := &mocks.QueueProtocolAPI{}
	r.SetAPI(api)
	cfg := types.NewChain33Config(types.GetDefaultCfgstring())
	api.On("GetConfig").Return(cfg)
	api.On("Query", paratypes.ParaX, "GetNodeGroupStatus", mock.Anything).Return(
		&paratypes.ParaNodeGroupStatus{TargetAddrs: testCommitAddr}, nil)
	r.SetStateDB(state)

	recp := testExec(t, r, rtypes.NameCommitDKGAction, commit, nil, 0)
	require.NotNil(t, recp)
	require.NotEmpty(t, recp.KV)
}
