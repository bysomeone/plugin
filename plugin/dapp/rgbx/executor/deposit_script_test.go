package executor

import (
	"encoding/hex"
	"testing"

	"github.com/33cn/chain33/client/mocks"
	"github.com/33cn/chain33/common/crypto"
	"github.com/33cn/chain33/common/db"
	"github.com/33cn/chain33/types"
	"github.com/33cn/chain33/util"
	ltypes "github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

/*
 * Covers on-chain deposit-address registration:
 *   - Exec derives the frozen P2WSH pkScript/address, stores the item and logs it;
 *   - a second registration of the same user is idempotent (no KV, same result);
 *   - the query only reads the registry (unregistered => ErrNotFound, never derives);
 *   - a missing/invalid BTC group pubkey is rejected (fail-closed);
 *   - the height listing filters and pages like the abandon listing.
 */

type depositScriptFixture struct {
	r      *rgbx
	state  db.DB
	api    *mocks.QueueProtocolAPI
	tssPub []byte
}

func newDepositScriptFixture(t *testing.T) *depositScriptFixture {
	t.Helper()
	r, state, api := newBridgeConfigTestExec(t, bridgeConfigTestChainCfg())
	f := &depositScriptFixture{
		r:      r,
		state:  state,
		api:    api,
		tssPub: newP2WSHDepositFixture(t, frozenVectorA).tssPub,
	}
	api.On("Query", ltypes.LightclientX, "GetBtcNetName", mock.Anything).
		Return(&types.ReplyString{Data: "testnet3"}, nil)
	return f
}

func (f *depositScriptFixture) registerCrossChainInfo(t *testing.T) {
	t.Helper()
	setCrossChainInfoForTest(t, f.state, rtypes.BTCSymbol, rtypes.CrossChainAssetBtc,
		rtypes.BTCChainPrecision, f.tssPub)
}

func signWith(priv crypto.PrivKey, payload string) *types.Transaction {
	tx := &types.Transaction{Payload: []byte(payload)}
	tx.Sign(types.SECP256K1, priv)
	return tx
}

func httpDepositScriptAction() *rtypes.RgbxAction {
	return &rtypes.RgbxAction{
		Ty:    rtypes.TyRegisterDepositScript,
		Value: &rtypes.RgbxAction_RegisterDepositScript{RegisterDepositScript: &rtypes.RegisterDepositScript{}},
	}
}

func TestDepositScript_ExecStoresAndLogs(t *testing.T) {
	f := newDepositScriptFixture(t)
	f.registerCrossChainInfo(t)
	height := int64(9)
	f.r.SetEnv(height, 0, 0)

	tx := signedTestTx("register-deposit")
	userID := tx.From()
	require.NotEmpty(t, userID)

	receipt, err := f.r.Exec_RegisterDepositScript(&rtypes.RegisterDepositScript{}, tx, 3)
	require.NoError(t, err)
	require.Len(t, receipt.GetKV(), 3)
	require.Len(t, receipt.GetLogs(), 1)
	require.Equal(t, int32(rtypes.TyDepositScriptLog), receipt.GetLogs()[0].GetTy())

	item := &rtypes.DepositScriptItem{}
	require.NoError(t, types.Decode(receipt.GetLogs()[0].GetLog(), item))

	expectAddress, err := rtypes.DeriveDepositAddress(userID, f.tssPub, &chaincfg.TestNet3Params)
	require.NoError(t, err)
	expectPkScript, err := rtypes.DeriveDepositPkScript(userID, f.tssPub)
	require.NoError(t, err)

	require.Equal(t, userID, item.GetUserID())
	require.Equal(t, expectAddress, item.GetAddress())
	require.Equal(t, hex.EncodeToString(expectPkScript), item.GetPkScript())
	require.Equal(t, rtypes.P2WSHDepositSpecV1, item.GetSpec())
	require.Equal(t, height, item.GetHeight())
	require.Equal(t, int32(3), item.GetIndex())

	// The registry key is the documented mavl-rgbx-deposit-script-<userID>.
	require.Equal(t, string(formatDepositScriptKey(userID)), string(receipt.GetKV()[0].GetKey()))
	applyStateKV(t, f.state, receipt.GetKV())

	stored, err := f.r.Query_GetDepositAddress(&types.ReqString{Data: userID})
	require.NoError(t, err)
	require.Equal(t, item.GetAddress(), stored.GetAddress())
	require.Equal(t, item.GetPkScript(), stored.GetPkScript())
	require.Equal(t, item.GetSpec(), stored.GetSpec())
}

func TestDepositScript_Idempotent(t *testing.T) {
	f := newDepositScriptFixture(t)
	f.registerCrossChainInfo(t)

	f.r.SetEnv(5, 0, 0)
	first, err := f.r.Exec_RegisterDepositScript(&rtypes.RegisterDepositScript{}, signedTestTx("register-1"), 1)
	require.NoError(t, err)
	applyStateKV(t, f.state, first.GetKV())
	firstItem := &rtypes.DepositScriptItem{}
	require.NoError(t, types.Decode(first.GetLogs()[0].GetLog(), firstItem))

	// Second registration at another height returns the same item and writes nothing.
	f.r.SetEnv(8, 0, 0)
	second, err := f.r.Exec_RegisterDepositScript(&rtypes.RegisterDepositScript{}, signedTestTx("register-2"), 0)
	require.NoError(t, err)
	require.Empty(t, second.GetKV())
	require.Len(t, second.GetLogs(), 1)
	secondItem := &rtypes.DepositScriptItem{}
	require.NoError(t, types.Decode(second.GetLogs()[0].GetLog(), secondItem))
	require.Equal(t, firstItem.GetAddress(), secondItem.GetAddress())
	require.Equal(t, firstItem.GetPkScript(), secondItem.GetPkScript())
	require.Equal(t, firstItem.GetHeight(), secondItem.GetHeight())
	require.Equal(t, firstItem.GetIndex(), secondItem.GetIndex())

	// The height index/counter was not touched by the idempotent call.
	count, err := f.r.depositScriptHeightCount(8)
	require.NoError(t, err)
	require.Zero(t, count)
}

func TestDepositScript_QueryUnregistered(t *testing.T) {
	f := newDepositScriptFixture(t)
	f.registerCrossChainInfo(t)
	addr, _ := util.Genaddress()

	_, err := f.r.Query_GetDepositAddress(&types.ReqString{Data: addr})
	require.ErrorIs(t, err, types.ErrNotFound)

	// Empty user id is an argument error, not "not bound".
	_, err = f.r.Query_GetDepositAddress(&types.ReqString{})
	require.ErrorIs(t, err, types.ErrInvalidParam)
}

func TestDepositScript_RejectsMissingOrInvalidBtcInfo(t *testing.T) {
	t.Run("missing cross chain info", func(t *testing.T) {
		f := newDepositScriptFixture(t)
		tx := signedTestTx("register-missing-info")
		tx.Payload = types.Encode(httpDepositScriptAction())

		err := f.r.CheckTx(tx, 0)
		require.ErrorIs(t, err, ErrGetCrossChainInfo)

		_, err = f.r.Exec_RegisterDepositScript(&rtypes.RegisterDepositScript{}, tx, 0)
		require.ErrorIs(t, err, ErrGetCrossChainInfo)
	})

	t.Run("invalid pubkey", func(t *testing.T) {
		f := newDepositScriptFixture(t)
		setCrossChainInfoForTest(t, f.state, rtypes.BTCSymbol, rtypes.CrossChainAssetBtc,
			rtypes.BTCChainPrecision, []byte{0x02})
		tx := signedTestTx("register-invalid-pub")
		tx.Payload = types.Encode(httpDepositScriptAction())

		err := f.r.CheckTx(tx, 0)
		require.ErrorIs(t, err, ErrInvalidCrossChainInfo)

		_, err = f.r.Exec_RegisterDepositScript(&rtypes.RegisterDepositScript{}, tx, 0)
		require.ErrorIs(t, err, ErrInvalidCrossChainInfo)
	})
}

func TestDepositScript_QueryFiltersByHeightAndPages(t *testing.T) {
	f := newDepositScriptFixture(t)
	f.registerCrossChainInfo(t)

	_, privA := util.Genaddress()
	_, privB := util.Genaddress()
	heightA, heightB := int64(5), int64(7)

	f.r.SetEnv(heightA, 0, 0)
	recA, err := f.r.Exec_RegisterDepositScript(&rtypes.RegisterDepositScript{},
		signWith(privA, "register-a"), 2)
	require.NoError(t, err)
	applyStateKV(t, f.state, recA.GetKV())
	userA := signWith(privA, "register-a").From()

	f.r.SetEnv(heightB, 0, 0)
	recB, err := f.r.Exec_RegisterDepositScript(&rtypes.RegisterDepositScript{},
		signWith(privB, "register-b"), 0)
	require.NoError(t, err)
	applyStateKV(t, f.state, recB.GetKV())
	userB := signWith(privB, "register-b").From()

	// Single height.
	list, err := f.r.Query_ListDepositRegistrations(&rtypes.ReqListPendingTx{
		StartHeight: heightB, EndHeight: heightB, Count: 10,
	})
	require.NoError(t, err)
	require.Len(t, list.GetItems(), 1)
	require.Equal(t, userB, list.GetItems()[0].GetUserID())
	require.Equal(t, heightB, list.GetItems()[0].GetHeight())

	// Full range, ascending by height.
	list, err = f.r.Query_ListDepositRegistrations(&rtypes.ReqListPendingTx{
		StartHeight: 0, EndHeight: heightB, Count: 10,
	})
	require.NoError(t, err)
	require.Len(t, list.GetItems(), 2)
	require.Equal(t, userA, list.GetItems()[0].GetUserID())
	require.Equal(t, userB, list.GetItems()[1].GetUserID())

	// StartIndex skips a registration at StartHeight only (A has tx index 2).
	list, err = f.r.Query_ListDepositRegistrations(&rtypes.ReqListPendingTx{
		StartHeight: heightA, EndHeight: heightB, Count: 10, StartIndex: 3,
	})
	require.NoError(t, err)
	require.Len(t, list.GetItems(), 1)
	require.Equal(t, userB, list.GetItems()[0].GetUserID())

	// Count is a hard cap.
	list, err = f.r.Query_ListDepositRegistrations(&rtypes.ReqListPendingTx{
		StartHeight: 0, EndHeight: heightB, Count: 1,
	})
	require.NoError(t, err)
	require.Len(t, list.GetItems(), 1)
	require.Equal(t, userA, list.GetItems()[0].GetUserID())

	// EndHeight = 0 means "up to the current height".
	f.r.SetEnv(heightB, 0, 0)
	list, err = f.r.Query_ListDepositRegistrations(&rtypes.ReqListPendingTx{
		StartHeight: 0, Count: 10,
	})
	require.NoError(t, err)
	require.Len(t, list.GetItems(), 2)

	// Invalid arguments.
	_, err = f.r.Query_ListDepositRegistrations(&rtypes.ReqListPendingTx{StartHeight: 0, Count: 0})
	require.ErrorIs(t, err, types.ErrInvalidParam)
	_, err = f.r.Query_ListDepositRegistrations(&rtypes.ReqListPendingTx{StartHeight: 5, EndHeight: 1, Count: 1})
	require.ErrorIs(t, err, types.ErrInvalidParam)
	_, err = f.r.Query_ListDepositRegistrations(&rtypes.ReqListPendingTx{StartHeight: -1, Count: 1})
	require.ErrorIs(t, err, types.ErrInvalidParam)
}

func TestDepositScript_ExecDispatch(t *testing.T) {
	f := newDepositScriptFixture(t)
	f.registerCrossChainInfo(t)
	f.r.SetEnv(3, 0, 0)

	tx, err := f.r.GetExecutorType().CreateTransaction(rtypes.NameRegisterDepositScriptAction,
		&rtypes.RegisterDepositScript{})
	require.NoError(t, err)
	tx.Sign(types.SECP256K1, testPriv)

	receipt, err := f.r.Exec(tx, 0)
	require.NoError(t, err)
	require.Len(t, receipt.GetLogs(), 1)
	require.Equal(t, int32(rtypes.TyDepositScriptLog), receipt.GetLogs()[0].GetTy())

	// CheckTx dispatch accepts the same action when the BTC info is usable.
	require.NoError(t, f.r.CheckTx(tx, 0))
}

func TestDepositScript_QueryUsesRegisteredName(t *testing.T) {
	f := newDepositScriptFixture(t)
	f.registerCrossChainInfo(t)
	f.r.SetEnv(1, 0, 0)

	tx := signedTestTx("register-query")
	rec, err := f.r.Exec_RegisterDepositScript(&rtypes.RegisterDepositScript{}, tx, 0)
	require.NoError(t, err)
	applyStateKV(t, f.state, rec.GetKV())

	msg, err := f.r.Query("GetDepositAddress", types.Encode(&types.ReqString{Data: tx.From()}))
	require.NoError(t, err)
	item, ok := msg.(*rtypes.DepositScriptItem)
	require.True(t, ok)
	require.Equal(t, tx.From(), item.GetUserID())

	msg, err = f.r.Query("ListDepositRegistrations", types.Encode(&rtypes.ReqListPendingTx{
		StartHeight: 0, EndHeight: 1, Count: 10,
	}))
	require.NoError(t, err)
	list, ok := msg.(*rtypes.DepositScriptList)
	require.True(t, ok)
	require.Len(t, list.GetItems(), 1)
}
