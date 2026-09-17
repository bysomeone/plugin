package executor

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/33cn/chain33/client/mocks"
	"github.com/33cn/chain33/common/crypto"
	"github.com/33cn/chain33/system/dapp"
	"github.com/33cn/chain33/types"
	"github.com/33cn/chain33/util"
	"github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
	ltypes "github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
	paratypes "github.com/33cn/plugin/plugin/dapp/paracross/types"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var testCommitAddr string
var testPriv crypto.PrivKey
var testCfg *types.Chain33Config

func init() {
	testCommitAddr, testPriv = util.Genaddress()
	rgbxCfg.CommitAddress = testCommitAddr
	testCfg = types.NewChain33Config(types.GetDefaultCfgstring())
	Init(driverName, testCfg, nil)
}

type testCase struct {
	action    types.Message
	expectErr error
}

func Test_CheckTx(t *testing.T) {

	r := newRgbx()

	action := &rtypes.RgbxAction{}
	tx := &types.Transaction{Payload: []byte("testdata")}
	require.Equal(t, types.ErrActionNotSupport, r.CheckTx(tx, 0))

	tx.Payload = types.Encode(action)
	require.Equal(t, types.ErrActionNotSupport, r.CheckTx(tx, 0))
}

func testCheck(t *testing.T, driver dapp.Driver, tx *types.Transaction, action types.Message, expectErr error, idx int) {

	tx.Payload = types.Encode(action)
	require.Equalf(t, expectErr, driver.CheckTx(tx, 0), "testcase: %d", idx)
}

func Test_checkMint(t *testing.T) {

	r := newRgbx()
	action := &rtypes.RgbxAction{}
	action.Ty = rtypes.TyMintAction

	tx := &types.Transaction{}
	mintAction := &rtypes.RgbxAction_Mint{}
	action.Value = mintAction

	tcArr := []*testCase{
		{
			expectErr: ErrInvalidSymbolLength,
			action:    &rtypes.MintAsset{Symbol: ""},
		},
		{
			expectErr: ErrInvalidSymbolLength,
			action:    &rtypes.MintAsset{Symbol: "aaaabbbbccccdddde"},
		},
		{
			// A6：非白名单字符（ToUpper 非单射，"xſ" 与 "xs" 归一化相同）必须被拒，
			// 否则可与已有 symbol 撞同一个 asset key（抢注/别名化）。
			expectErr: ErrInvalidAssetSymbol,
			action:    &rtypes.MintAsset{Symbol: "xſ", TotalAmount: 1},
		},
		{
			expectErr: ErrInvalidAssetSymbol,
			action:    &rtypes.MintAsset{Symbol: "us-dt", TotalAmount: 1},
		},
		{
			expectErr: ErrInvalidAssetAmount,
			action:    &rtypes.MintAsset{Symbol: "test"},
		},
		{
			expectErr: ErrInvalidAssetAmount,
			action:    &rtypes.MintAsset{Symbol: "test", TotalAmount: rtypes.MaxAssetAmount + 1},
		},
		{
			expectErr: ErrInvalidAssetAmount,
			action:    &rtypes.MintAsset{Symbol: "test", Type: 1, TotalAmount: 2},
		},
		{
			expectErr: ErrInvalidMetaHashLength,
			action:    &rtypes.MintAsset{Symbol: "test", TotalAmount: 1, MetaHash: []byte(strings.Repeat("abcd", 9))},
		},
		{
			expectErr: ErrDuplicateAssetSymbol,
			action:    &rtypes.MintAsset{Symbol: "test", TotalAmount: 1, MetaHash: []byte("hash")},
		},
		{
			expectErr: ErrNilGenesisOut,
			action:    &rtypes.MintAsset{Symbol: "test1", TotalAmount: 1, MetaHash: []byte("hash"), GenesisOut: &rtypes.OutPoint{Hash: "hash"}},
		},
		{
			expectErr: nil,
			action: &rtypes.MintAsset{Symbol: "test1", TotalAmount: 1, MetaHash: []byte("hash"), GenesisOut: &rtypes.OutPoint{
				Hash:     "hash",
				PkScript: []byte("pubkey"),
			}},
		},
	}

	dir, state, _ := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	api := &mocks.QueueProtocolAPI{}
	r.SetAPI(api)
	api.On("GetConfig").Return(testCfg)
	r.SetStateDB(state)
	err := state.Set(formatAssetKey("test"), []byte("test"))
	require.Nil(t, err)

	for idx, tc := range tcArr {
		mintAction.Mint = tc.action.(*rtypes.MintAsset)
		testCheck(t, r, tx, action, tc.expectErr, idx)
	}
}

func Test_checkTransfer(t *testing.T) {
	r := newRgbx()
	action := &rtypes.RgbxAction{}
	action.Ty = rtypes.TyTransferAction
	addr, priv := util.Genaddress()
	tx := &types.Transaction{}
	tx.Sign(types.SECP256K1, priv)
	value := &rtypes.RgbxAction_Transfer{}
	action.Value = value
	utxoAddr := "74503993e7c8d4280f6fbb99ae5aaa92231a1981a358e40f97e2b4f4dfbea13c:0"
	tcArr := []*testCase{
		{
			expectErr: ErrInvalidFromUtxo,
			action:    &rtypes.TransferAsset{FromUtxo: "f4dfbea13c:0", To: addr, Amount: 1, Symbol: "normal"},
		},
		{
			expectErr: types.ErrInvalidAddress,
			action:    &rtypes.TransferAsset{To: utxoAddr, ChangeAddr: "invalidaddr", Amount: 1, Symbol: "normal"},
		},
		{
			expectErr: ErrInvalidFromUtxo,
			action:    &rtypes.TransferAsset{FromUtxo: utxoAddr, To: addr, Amount: 1, Symbol: "normal"},
		},
		{
			expectErr: ErrAssetNotExist,
			action:    &rtypes.TransferAsset{To: addr, Amount: 1},
		},
		{
			expectErr: ErrInvalidAssetAmount,
			action:    &rtypes.TransferAsset{To: addr, Symbol: "normal"},
		},
		{
			expectErr: types.ErrInsufficientBalance,
			action:    &rtypes.TransferAsset{To: addr, Symbol: "normal", Amount: 1},
		},
		{
			expectErr: ErrInvalidAssetSender,
			action:    &rtypes.TransferAsset{To: addr, Symbol: "collect", Amount: 1},
		},
		{
			expectErr: nil,
			action:    &rtypes.TransferAsset{To: addr, Symbol: "xbtc", Amount: 1},
		},
		{
			expectErr: nil,
			action:    &rtypes.TransferAsset{FromUtxo: utxoAddr, To: addr, Symbol: "xbtc", Amount: 1},
		},
		{
			expectErr: types.ErrInsufficientBalance,
			action:    &rtypes.TransferAsset{To: addr, Symbol: "xbtc", Amount: 2},
		},
		{
			expectErr: nil,
			action:    &rtypes.TransferAsset{FromUtxo: utxoAddr, To: tx.From(), Symbol: "collect", Amount: 1, FromUtxoPkScript: []byte("pubkey")},
		},
	}

	dir, state, _ := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	api := &mocks.QueueProtocolAPI{}
	r.SetAPI(api)
	api.On("GetConfig").Return(testCfg)
	r.SetStateDB(state)
	err := state.Set(formatAssetKey("normal"), types.Encode(&rtypes.RgbxAsset{}))
	require.Nil(t, err)
	err = state.Set(formatAssetKey("collect"), types.Encode(&rtypes.RgbxAsset{
		Type:  1,
		Owner: utxoAddr,
	}))
	require.Nil(t, err)
	err = state.Set(formatCrossChainInfoKey("btc"), types.Encode(&rtypes.CrossChainInfo{AssetSymbol: "BTC"}))
	require.Nil(t, err)
	// checkCrossChainTransfer 现在使用 newAccount，symbol 为 "xbtc"
	crossAcc, err := r.(*rgbx).newAccount("xbtc")
	require.Nil(t, err)
	_, err = crossAcc.Mint(tx.From(), 1)
	require.Nil(t, err)

	for idx, tc := range tcArr {
		value.Transfer = tc.action.(*rtypes.TransferAsset)
		testCheck(t, r, tx, action, tc.expectErr, idx)
	}
}

func Test_checkConfirm(t *testing.T) {
	r := newRgbx()
	action := &rtypes.RgbxAction{}
	action.Ty = rtypes.TyConfirmAction
	tx := &types.Transaction{}
	tx.Sign(types.SECP256K1, testPriv)
	value := &rtypes.RgbxAction_Confirm{}
	action.Value = value
	utxoAddr := "74503993e7c8d4280f6fbb99ae5aaa92231a1981a358e40f97e2b4f4dfbea13c:0"

	btcTx := wire.MsgTx{}
	out, err := wire.NewOutPointFromString(utxoAddr)
	require.Nil(t, err)
	btcTx.TxIn = append(btcTx.TxIn,
		&wire.TxIn{PreviousOutPoint: *out},
		&wire.TxIn{PreviousOutPoint: wire.OutPoint{
			Hash:  out.Hash,
			Index: 1,
		}})
	btcTx.TxOut = append(btcTx.TxOut, wire.NewTxOut(0, []byte("testScript")))
	buf := bytes.NewBuffer(make([]byte, 0, btcTx.SerializeSizeStripped()))
	err = btcTx.SerializeNoWitness(buf)
	require.Nil(t, err)

	tcArr := []*testCase{
		{
			expectErr: ErrPendingTxNotExist,
			action:    &rtypes.ConfirmTx{TxIndex: 1},
		},
		{
			expectErr: ErrTxAlreadyConfirmed,
			action:    &rtypes.ConfirmTx{TxBlockHeight: 1},
		},
		{
			expectErr: ErrConfirmedHashNotEqual,
			action:    &rtypes.ConfirmTx{TxBlockHeight: 2, TxIndex: 0, TxHash: []byte("hash")},
		},
		{
			expectErr: nil,
			action:    &rtypes.ConfirmTx{Timeout: true},
		},
		{
			expectErr: ErrWithdrawConfirmTimeoutNotAllowed,
			action:    &rtypes.ConfirmTx{Timeout: true, ActionType: rtypes.TyWithdrawAsset},
		},
		{
			expectErr: ErrDecodeBtcTx,
			action:    &rtypes.ConfirmTx{UtxoProof: &rtypes.UtxoSpendingProof{SpendingTx: []byte("invalidBtcTxData")}},
		},
		{
			// A2：尾部追加字节的同一笔花费（解析结果与 txid 不变）必须被拒，
			// 否则归属 utxo id 会随编码变化，同一笔花费被记到另一个 owner id。
			expectErr: ErrNonCanonicalSpendingTx,
			action: &rtypes.ConfirmTx{UtxoProof: &rtypes.UtxoSpendingProof{
				SpendingTx:          append(append([]byte{}, buf.Bytes()...), 0x00),
				OpRetOutputPkScript: []byte("testScript"),
			}},
		},
		{
			expectErr: ErrInvalidSpendingTxIn,
			action:    &rtypes.ConfirmTx{UtxoProof: &rtypes.UtxoSpendingProof{SpendingTx: buf.Bytes(), SpendingInputIdx: 2}},
		},
		{
			expectErr: ErrSpendingInputNotEqual,
			action:    &rtypes.ConfirmTx{UtxoProof: &rtypes.UtxoSpendingProof{SpendingTx: buf.Bytes(), SpendingInputIdx: 1}},
		},
		{
			expectErr: nil,
			action:    &rtypes.ConfirmTx{UtxoProof: &rtypes.UtxoSpendingProof{SpendingTx: buf.Bytes(), OpRetOutputIdx: -1}},
		},
		{
			expectErr: ErrOpRetOutputPkScriptNotEqual,
			action:    &rtypes.ConfirmTx{UtxoProof: &rtypes.UtxoSpendingProof{SpendingTx: buf.Bytes()}},
		},
		{
			expectErr: nil,
			action:    &rtypes.ConfirmTx{UtxoProof: &rtypes.UtxoSpendingProof{SpendingTx: buf.Bytes(), OpRetOutputPkScript: []byte("testScript")}},
		},
	}

	dir, state, local := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	api := &mocks.QueueProtocolAPI{}
	r.SetAPI(api)
	api.On("GetConfig").Return(testCfg)
	r.SetStateDB(state)
	r.SetLocalDB(local)
	require.Nil(t, state.Set(formatPayloadKey(nil), types.Encode(&rtypes.PendingTx{})))
	require.Nil(t, state.Set(formatPayloadKey([]byte("hash")), types.Encode(&rtypes.MintAsset{Symbol: "x", TotalAmount: 1})))
	require.Nil(t, local.Set(formatPendingTxKey(0, 0), types.Encode(&rtypes.PendingTx{Utxo: &rtypes.OutPoint{Hash: out.Hash.String()}})))
	require.Nil(t, local.Set(formatPendingTxKey(2, 0), types.Encode(&rtypes.PendingTx{
		Utxo:   &rtypes.OutPoint{Hash: out.Hash.String()},
		TxHash: []byte("other"),
	})))
	require.Nil(t, local.Set(formatPendingTxKey(1, 0), types.Encode(&rtypes.PendingTx{Confirmed: true})))

	for idx, tc := range tcArr {
		value.Confirm = tc.action.(*rtypes.ConfirmTx)
		testCheck(t, r, tx, action, tc.expectErr, idx)
	}
}

// Test_isValidSymbolCharset A6：formatSymbol 的 ToUpper 只在 ASCII 字母/数字/下划线上单射，
// 白名单之外的字符会与 ASCII 字母归一化到同一结果（别名化），必须拒绝。
func Test_isValidSymbolCharset(t *testing.T) {
	for _, ok := range []string{"BTC", "btc", "RGB20_USDT", "normal1", "x9", "A_b_9"} {
		require.Truef(t, isValidSymbolCharset(ok), "合法 symbol: %q", ok)
	}
	for _, bad := range []string{"", "xſ", "uſdt", "İ", "us-dt", "us dt", "us.dt", "usdt√", "全角Ａ"} {
		require.Falsef(t, isValidSymbolCharset(bad), "非法 symbol: %q", bad)
	}

	// 复现前提：ToUpper 对白名单外字符非单射（"xſ" 与 "xs" 得到同一个 key），
	// 这正是为什么必须在入口处按字符集拒绝，而不是只依赖 formatSymbol 归一化。
	require.Equal(t, formatSymbol("xs"), formatSymbol("xſ"))
	require.Equal(t, formatAssetKey("XS"), formatAssetKey("xſ"))

	// 白名单内归一化保持单射（大小写仍是同一个资产，这是既有语义）
	require.Equal(t, formatSymbol("btc"), formatSymbol("BTC"))
	require.NotEqual(t, formatSymbol("usdt"), formatSymbol("usdc"))
}

// Test_checkMint_rejectsAliasedSymbol A6 验收：别名化 symbol 无法抢注已有 symbol 的 asset key。
// 已有 "USDT" 资产时，mint "uſdt"（ToUpper 后同为 "USDT"）必须在入口被拒，
// 而不是被当作 "USDT" 的重复资产（ErrDuplicateAssetSymbol）——否则 key 的所有权口径就乱了。
func Test_checkMint_rejectsAliasedSymbol(t *testing.T) {
	r := newRgbx()
	dir, state, _ := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	api := &mocks.QueueProtocolAPI{}
	r.SetAPI(api)
	api.On("GetConfig").Return(testCfg)
	r.SetStateDB(state)

	// 未注册 "USDT" 时也不能用别名占位（否则合法 USDT 之后会被判为重复）
	err := r.(*rgbx).checkMint("tx", &rtypes.MintAsset{Symbol: "uſdt", TotalAmount: 1})
	require.Equal(t, ErrInvalidAssetSymbol, err)

	// 合法 symbol 正常通过
	err = r.(*rgbx).checkMint("tx", &rtypes.MintAsset{
		Symbol: "USDT", TotalAmount: 1, GenesisOut: &rtypes.OutPoint{Hash: "hash", PkScript: []byte("pk")},
	})
	require.NoError(t, err)
}

func newTestnetWitnessAddr(t *testing.T) (addr string, pkScript []byte) {
	t.Helper()
	addr, pkScript, _ = newTestnetWitnessAddrAndPub(t)
	return addr, pkScript
}

// newTestnetWitnessAddrAndPub 同 newTestnetWitnessAddr，另外返回与 pkScript 绑定的压缩公钥
// （CommitDKG 现在对所有 symbol 都要求带 pubkey，且校验 hash160(pubkey)==pkScript[2:]）。
func newTestnetWitnessAddrAndPub(t *testing.T) (addr string, pkScript, pub []byte) {
	t.Helper()
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pub = priv.PubKey().SerializeCompressed()
	waddr, err := btcutil.NewAddressWitnessPubKeyHash(btcutil.Hash160(pub), &chaincfg.TestNet3Params)
	require.NoError(t, err)
	pk, err := txscript.PayToAddrScript(waddr)
	require.NoError(t, err)
	return waddr.String(), pk, pub
}

// newTestnetP2WSHAddr 由给定 witnessScript 造一个 native P2WSH 目标地址（任意脚本，
// 用于证明"普通 P2WSH 目标不受影响"）。
func newTestnetP2WSHAddr(t *testing.T, witnessScript []byte) (string, []byte) {
	t.Helper()
	sum := sha256.Sum256(witnessScript)
	addr, err := btcutil.NewAddressWitnessScriptHash(sum[:], &chaincfg.TestNet3Params)
	require.NoError(t, err)
	pk, err := txscript.PayToAddrScript(addr)
	require.NoError(t, err)
	return addr.String(), pk
}

func Test_checkWithdraw(t *testing.T) {
	r := newRgbx()
	action := &rtypes.RgbxAction{}
	action.Ty = rtypes.TyWithdrawAsset
	userAddr, userPriv := util.Genaddress()
	validDest, _ := newTestnetWitnessAddr(t)
	tx := &types.Transaction{}
	tx.Sign(types.SECP256K1, userPriv)
	value := &rtypes.RgbxAction_Withdraw{}
	action.Value = value

	tcArr := []*testCase{
		{expectErr: ErrInvalidWithdrawAmount, action: &rtypes.WithdrawAsset{
			AssetSymbol: "btc", Amount: minBtcWithdrawAmount - 1, DestinationAddr: validDest, FeeRate: 1,
		}},
		{expectErr: ErrInvalidWithdrawFeeRate, action: &rtypes.WithdrawAsset{
			AssetSymbol: "btc", Amount: minBtcWithdrawAmount, DestinationAddr: validDest, FeeRate: 0,
		}},
		{expectErr: ErrInvalidWithdrawFeeRate, action: &rtypes.WithdrawAsset{
			AssetSymbol: "btc", Amount: minBtcWithdrawAmount, DestinationAddr: validDest, FeeRate: maxBtcFeeRate + 1,
		}},
		{expectErr: ErrInvalidWithdrawDestination, action: &rtypes.WithdrawAsset{
			AssetSymbol: "btc", Amount: minBtcWithdrawAmount, DestinationAddr: "not-a-btc-address", FeeRate: 1,
		}},
		{expectErr: types.ErrInsufficientBalance, action: &rtypes.WithdrawAsset{
			AssetSymbol: "btc", Amount: 10001, DestinationAddr: validDest, FeeRate: 1,
		}},
		{expectErr: nil, action: &rtypes.WithdrawAsset{
			AssetSymbol: "btc", Amount: minBtcWithdrawAmount, DestinationAddr: validDest, FeeRate: 10,
		}},
	}

	dir, state, _ := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	api := &mocks.QueueProtocolAPI{}
	r.SetAPI(api)
	api.On("GetConfig").Return(testCfg)
	api.On("Query", ltypes.LightclientX, "GetBtcNetName", mock.Anything).Return(&types.ReplyString{Data: "testnet3"}, nil)
	r.SetStateDB(state)
	// E15-a 之后 checkWithdraw 需要 tssPub（判"目标是不是发起人自己的充值脚本"），
	// 因此 CrossChainInfo 必须带 pubkey（无 pubkey ⇒ ErrInvalidCrossChainInfo，见专用用例）。
	infoWithPub := func() *rtypes.CrossChainInfo {
		return &rtypes.CrossChainInfo{AssetSymbol: "BTC", Pubkey: newP2WSHDepositFixture(t, frozenVectorA).tssPub}
	}
	require.Nil(t, state.Set(formatCrossChainInfoKey("btc"), types.Encode(infoWithPub())))
	// checkWithdraw 现在使用 newAccount(withdraw.GetAssetSymbol())，即 newAccount("btc")
	// formatSymbol("btc") -> "BTC"，所以账户 symbol 是 "BTC"
	acc, err := r.(*rgbx).newAccount("xbtc")
	require.Nil(t, err)
	_, err = acc.Mint(userAddr, 10000)
	require.Nil(t, err)

	for idx, tc := range tcArr {
		if idx == 1 {
			require.Nil(t, state.Delete(formatCrossChainInfoKey("btc")))
		}
		if idx == 2 {
			require.Nil(t, state.Set(formatCrossChainInfoKey("btc"), types.Encode(infoWithPub())))
		}
		value.Withdraw = tc.action.(*rtypes.WithdrawAsset)
		testCheck(t, r, tx, action, tc.expectErr, idx)
	}
}

// Test_checkWithdraw_depositScriptGuard E15-a：**只拒"提现目标 = 发起人自己的充值脚本"**，
// 不拒一般的 P2WSH / P2TR 目标（交易所、多签钱包、闪电通道大量使用这两类地址）。
func Test_checkWithdraw_depositScriptGuard(t *testing.T) {
	r := newRgbx().(*rgbx)
	tx := &types.Transaction{}
	tx.Sign(types.SECP256K1, testPriv) // from == testCommitAddr
	fromAddr := tx.From()
	require.Equal(t, testCommitAddr, fromAddr)

	dir, state, _ := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	api := &mocks.QueueProtocolAPI{}
	r.SetAPI(api)
	api.On("GetConfig").Return(testCfg)
	api.On("Query", ltypes.LightclientX, "GetBtcNetName", mock.Anything).Return(&types.ReplyString{Data: "testnet3"}, nil)
	r.SetStateDB(state)

	params := &chaincfg.TestNet3Params
	user := newP2WSHDepositFixture(t, frozenVectorA)
	require.NoError(t, state.Set(formatCrossChainInfoKey("btc"), types.Encode(user.crossChainInfo([]byte{0x51}))))

	// 与 checkWithdraw 的 ensureCrossChainSymbol("btc") 口径一致：账户 symbol 是 "XBTC"
	acc, err := r.newAccount("xbtc")
	require.NoError(t, err)
	_, err = acc.Mint(fromAddr, 100000)
	require.NoError(t, err)

	// 1) 目标 = 发起人自己的充值地址（P2WSH(发起人, tssPub)）→ 拒
	ownDepositAddr, err := rtypes.DeriveDepositAddress(fromAddr, user.tssPub, params)
	require.NoError(t, err)
	ownScript, err := r.decodeBtcAddressScript(ownDepositAddr)
	require.NoError(t, err)
	require.True(t, rtypes.IsDepositPkScript(ownScript, fromAddr, user.tssPub), "前提：这个目标就是自己的充值脚本")
	require.Equal(t, ErrWithdrawToDepositScript, r.checkWithdraw(fromAddr, "tx", &rtypes.WithdrawAsset{
		AssetSymbol: "btc", Amount: 10000, FeeRate: 10, DestinationAddr: ownDepositAddr,
	}))

	// 2) 目标 = **普通** P2WSH（2-of-2 多签）→ 放行
	priv1, _ := btcec.NewPrivateKey()
	priv2, _ := btcec.NewPrivateKey()
	multisig, err := txscript.NewScriptBuilder().
		AddOp(txscript.OP_2).
		AddData(priv1.PubKey().SerializeCompressed()).
		AddData(priv2.PubKey().SerializeCompressed()).
		AddOp(txscript.OP_2).AddOp(txscript.OP_CHECKMULTISIG).Script()
	require.NoError(t, err)
	multisigAddr, _ := newTestnetP2WSHAddr(t, multisig)
	require.NoError(t, r.checkWithdraw(fromAddr, "tx", &rtypes.WithdrawAsset{
		AssetSymbol: "btc", Amount: 10000, FeeRate: 10, DestinationAddr: multisigAddr,
	}))

	// 3) 目标 = P2WPKH → 放行
	wpkhAddr, _ := newTestnetWitnessAddr(t)
	require.NoError(t, r.checkWithdraw(fromAddr, "tx", &rtypes.WithdrawAsset{
		AssetSymbol: "btc", Amount: 10000, FeeRate: 10, DestinationAddr: wpkhAddr,
	}))

	// 4) 目标 = P2TR（witness v1）→ 放行
	taprootAddr, err := btcutil.NewAddressTaproot(bytes.Repeat([]byte{0x02}, 32), params)
	require.NoError(t, err)
	require.NoError(t, r.checkWithdraw(fromAddr, "tx", &rtypes.WithdrawAsset{
		AssetSymbol: "btc", Amount: 10000, FeeRate: 10, DestinationAddr: taprootAddr.String(),
	}))

	// 5) 已知覆盖面边界（如实钉住，不假装覆盖）：目标是**另一个** chain33 地址的充值地址时，
	//    链上无法枚举已发放地址 ⇒ 放行；这一段只能由桥侧 registry 兜底（C2/C4）。
	otherUser := newP2WSHDepositFixture(t, frozenVectorB)
	require.NotEqual(t, fromAddr, otherUser.depositAddr)
	otherDepositAddr, err := rtypes.DeriveDepositAddress(otherUser.depositAddr, user.tssPub, params)
	require.NoError(t, err)
	require.True(t, rtypes.IsDepositPkScript(mustDecodeScript(t, r, otherDepositAddr), otherUser.depositAddr, user.tssPub))
	require.NoError(t, r.checkWithdraw(fromAddr, "tx", &rtypes.WithdrawAsset{
		AssetSymbol: "btc", Amount: 10000, FeeRate: 10, DestinationAddr: otherDepositAddr,
	}), "链上挡不住这个变体（边界，需桥侧 registry 兜底）")

	// 6) CrossChainInfo 没有 pubkey ⇒ fail-closed（无法判定"是不是自己的充值脚本"就不放行）
	require.NoError(t, state.Set(formatCrossChainInfoKey("btc"), types.Encode(&rtypes.CrossChainInfo{AssetSymbol: "BTC"})))
	require.Equal(t, ErrInvalidCrossChainInfo, r.checkWithdraw(fromAddr, "tx", &rtypes.WithdrawAsset{
		AssetSymbol: "btc", Amount: 10000, FeeRate: 10, DestinationAddr: wpkhAddr,
	}))
}

func mustDecodeScript(t *testing.T, r *rgbx, addr string) []byte {
	t.Helper()
	script, err := r.decodeBtcAddressScript(addr)
	require.NoError(t, err)
	return script
}

func Test_checkDeposit(t *testing.T) {
	r := newRgbx()
	action := &rtypes.RgbxAction{}
	action.Ty = rtypes.TyDepositAsset
	tx := &types.Transaction{}
	value := &rtypes.RgbxAction_Deposit{}
	action.Value = value

	depAddr, _ := util.Genaddress()
	var minimalBtcTx wire.MsgTx
	minimalBtcTx.Version = 2
	buf := bytes.NewBuffer(make([]byte, 0, minimalBtcTx.SerializeSizeStripped()))
	require.NoError(t, minimalBtcTx.SerializeNoWitness(buf))

	// 重复充值用例的 TxData 必须是一份可严格解析的合法交易（解析失败会先被 ErrInvalidBtcTxProof 拒掉），
	// 且 txid 需与下面 ErrGetBtcHeader 用例用的 minimalBtcTx 不同。
	dupTx := &wire.MsgTx{Version: 2}
	dupTx.TxOut = append(dupTx.TxOut, wire.NewTxOut(1, []byte{0x51}))
	dupBuf := bytes.NewBuffer(nil)
	require.NoError(t, dupTx.SerializeNoWitness(dupBuf))
	dupProofData := dupBuf.Bytes()
	dupTxID, err := parseBtcTxIDStrict("dup-tx", dupProofData)
	require.NoError(t, err)
	minimalTxID := minimalBtcTx.TxHash()
	require.NotEqual(t, minimalTxID.CloneBytes(), dupTxID)

	tcArr := []*testCase{
		{expectErr: ErrInvalidDepositAmount, action: &rtypes.DepositAsset{
			AssetSymbol: "btc", Amount: 0, DepositAddress: depAddr, TxProof: &rtypes.BtcTxProof{TxData: []byte{1}},
		}},
		{expectErr: ErrInvalidDepositAddress, action: &rtypes.DepositAsset{
			AssetSymbol: "btc", Amount: 1, DepositAddress: "", TxProof: &rtypes.BtcTxProof{TxData: []byte{1}},
		}},
		{expectErr: ErrInvalidBtcTxProof, action: &rtypes.DepositAsset{
			AssetSymbol: "btc", Amount: 1, DepositAddress: depAddr, TxProof: nil,
		}},
		{expectErr: ErrInvalidBtcTxProof, action: &rtypes.DepositAsset{
			AssetSymbol: "btc", Amount: 1, DepositAddress: depAddr, TxProof: &rtypes.BtcTxProof{TxData: []byte{0xff}},
		}},
		{expectErr: ErrDuplicateDepositProof, action: &rtypes.DepositAsset{
			AssetSymbol: "btc", Amount: 1, DepositAddress: depAddr, TxProof: &rtypes.BtcTxProof{TxData: dupProofData},
		}},
	}

	dir, state, _ := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	api := &mocks.QueueProtocolAPI{}
	r.SetAPI(api)
	api.On("GetConfig").Return(testCfg)
	r.SetStateDB(state)
	require.Nil(t, state.Set(formatDepositUsedTxIDKey(dupTxID), []byte("1")))

	for idx, tc := range tcArr {
		value.Deposit = tc.action.(*rtypes.DepositAsset)
		testCheck(t, r, tx, action, tc.expectErr, idx)
	}

	// decode ok, header query fails
	api.On("Query", ltypes.LightclientX, "GetBtcHeader", mock.Anything).Return(nil, errors.New("no header"))
	value.Deposit = &rtypes.DepositAsset{
		AssetSymbol:    "btc",
		Amount:         1,
		DepositAddress: depAddr,
		TxProof: &rtypes.BtcTxProof{
			TxData:      buf.Bytes(),
			BlockHeight: 1,
			BlockHash:   "00",
			TxIndex:     0,
			MerkleProof: nil,
		},
	}
	testCheck(t, r, tx, action, ErrGetBtcHeader, len(tcArr))
}

func Test_checkCommitDKG(t *testing.T) {
	r := newRgbx()
	action := &rtypes.RgbxAction{}
	action.Ty = rtypes.TyCommitDKGAction
	tx := &types.Transaction{}
	tx.Sign(types.SECP256K1, testPriv)
	value := &rtypes.RgbxAction_CommitDKG{}
	action.Value = value

	dkgAddr, validPk, validPub := newTestnetWitnessAddrAndPub(t)

	// P2WSH 起，**所有** symbol 的 CommitDKG 都必须带 TSS 群公钥（派生充值脚本要用），
	// 且 hash160(pubkey) 必须等于 DKG 地址的 pkScript[2:]。
	tcArr := []*testCase{
		{expectErr: ErrInvalidDkgAddress, action: &rtypes.CommitDKG{
			AssetSymbol: "btc", DkgAddress: dkgAddr, PkScript: []byte{0x01}, Pubkey: validPub,
		}},
		{expectErr: ErrInvalidDkgAddress, action: &rtypes.CommitDKG{
			AssetSymbol: "btc", DkgAddress: dkgAddr, PkScript: validPk, // 缺 pubkey → 拒
		}},
		{expectErr: ErrInvalidDkgAddress, action: &rtypes.CommitDKG{
			AssetSymbol: "btc", DkgAddress: dkgAddr, PkScript: validPk, Pubkey: newP2WSHDepositFixture(t, frozenVectorB).tssPub,
		}},
		{expectErr: ErrInvalidDkgAddress, action: &rtypes.CommitDKG{
			AssetSymbol: "btc", DkgAddress: dkgAddr, PkScript: validPk,
			Pubkey: append(append([]byte{}, validPub[:32]...), validPub[32]^0x01), // 改了最后一字节 → 与地址不符
		}},
		{expectErr: ErrGetGuardianNodeAddress, action: &rtypes.CommitDKG{
			AssetSymbol: "btc", DkgAddress: dkgAddr, PkScript: validPk, Pubkey: validPub,
		}},
		{expectErr: ErrInvalidGuardianCommitter, action: &rtypes.CommitDKG{
			AssetSymbol: "btc", DkgAddress: dkgAddr, PkScript: validPk, Pubkey: validPub,
		}},
		{expectErr: ErrDuplicateDKGCommit, action: &rtypes.CommitDKG{
			AssetSymbol: "btc", DkgAddress: dkgAddr, PkScript: validPk, Pubkey: validPub,
		}},
		{expectErr: nil, action: &rtypes.CommitDKG{
			AssetSymbol: "btc", DkgAddress: dkgAddr, PkScript: validPk, Pubkey: validPub,
		}},
	}

	dir, state, _ := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	api := &mocks.QueueProtocolAPI{}
	r.SetAPI(api)
	api.On("GetConfig").Return(testCfg)
	api.On("Query", ltypes.LightclientX, "GetBtcNetName", mock.Anything).Return(&types.ReplyString{Data: "testnet3"}, nil)
	r.SetStateDB(state)

	for idx, tc := range tcArr {
		switch idx {
		case 4:
			api.ExpectedCalls = nil
			api.On("GetConfig").Return(testCfg)
			api.On("Query", ltypes.LightclientX, "GetBtcNetName", mock.Anything).Return(&types.ReplyString{Data: "testnet3"}, nil)
			api.On("Query", paratypes.ParaX, "GetNodeGroupStatus", mock.Anything).Return(nil, errors.New("query fail"))
		case 5:
			api.ExpectedCalls = nil
			api.On("GetConfig").Return(testCfg)
			api.On("Query", ltypes.LightclientX, "GetBtcNetName", mock.Anything).Return(&types.ReplyString{Data: "testnet3"}, nil)
			api.On("Query", paratypes.ParaX, "GetNodeGroupStatus", mock.Anything).Return(
				&paratypes.ParaNodeGroupStatus{TargetAddrs: "other1,other2"}, nil)
		case 6:
			api.ExpectedCalls = nil
			api.On("GetConfig").Return(testCfg)
			api.On("Query", ltypes.LightclientX, "GetBtcNetName", mock.Anything).Return(&types.ReplyString{Data: "testnet3"}, nil)
			api.On("Query", paratypes.ParaX, "GetNodeGroupStatus", mock.Anything).Return(
				&paratypes.ParaNodeGroupStatus{TargetAddrs: testCommitAddr}, nil)
			require.Nil(t, state.Set(formatCrossChainInfoKey("btc"), types.Encode(&rtypes.CrossChainInfo{AssetSymbol: "BTC"})))
		case 7:
			api.ExpectedCalls = nil
			api.On("GetConfig").Return(testCfg)
			api.On("Query", ltypes.LightclientX, "GetBtcNetName", mock.Anything).Return(&types.ReplyString{Data: "testnet3"}, nil)
			api.On("Query", paratypes.ParaX, "GetNodeGroupStatus", mock.Anything).Return(
				&paratypes.ParaNodeGroupStatus{TargetAddrs: testCommitAddr}, nil)
			require.Nil(t, state.Delete(formatCrossChainInfoKey("btc")))
		}
		value.CommitDKG = tc.action.(*rtypes.CommitDKG)
		testCheck(t, r, tx, action, tc.expectErr, idx)
	}
}

func Test_decodeBtcAddressScript(t *testing.T) {

	params := lighttypes.GetBtcChainParams("regtest")

	priv, err := btcec.NewPrivateKey()
	require.Nil(t, err)
	pub := priv.PubKey().SerializeCompressed()
	waddr, err := btcutil.NewAddressWitnessPubKeyHash(btcutil.Hash160(pub), params)
	require.Nil(t, err)
	fmt.Println(waddr.String())
	_, err = btcutil.DecodeAddress(waddr.String(), params)
	require.Nil(t, err)
}
