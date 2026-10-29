package executor

import (
	"testing"

	"github.com/33cn/chain33/client/mocks"
	"github.com/33cn/chain33/types"
	"github.com/33cn/chain33/util"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/stretchr/testify/require"
)

func TestRgbx_Query_ListPendingTx(t *testing.T) {

	r := newRgbx()
	dir, db, local := util.CreateTestDB()
	defer util.CloseTestDB(dir, db)
	r.SetLocalDB(local)
	funcName := "ListPendingTx"
	_, err := r.Query(funcName, types.Encode(&rtypes.ReqListPendingTx{}))
	require.Equal(t, types.ErrInvalidParam, err)
	_, err = r.Query(funcName, types.Encode(&rtypes.ReqListPendingTx{Count: maxListCount + 1}))
	require.Equal(t, types.ErrInvalidParam, err)

	key1, key2, key3 := formatPendingTxKey(0, 1), formatPendingTxKey(0, 2), formatPendingTxKey(0, 3)
	require.Nil(t, db.Set(key1, []byte("invalidData")))
	require.Nil(t, db.Set(key2, types.Encode(&rtypes.PendingTx{TxIndex: 2})))
	require.Nil(t, db.Set(key3, types.Encode(&rtypes.PendingTx{TxIndex: 3})))
	msg, err := r.Query(funcName, types.Encode(&rtypes.ReqListPendingTx{Count: 10}))
	require.Nil(t, err)
	require.Equal(t, 2, len(msg.(*rtypes.PendingTxs).GetPendingList()))
	msg, err = r.Query(funcName, types.Encode(&rtypes.ReqListPendingTx{Count: 10, StartIndex: 3}))
	require.Nil(t, err)
	require.Equal(t, 0, len(msg.(*rtypes.PendingTxs).GetPendingList()))
}

func TestRgbx_Query_ListPendingTx_EndHeight(t *testing.T) {
	r := newRgbx()
	dir, db, local := util.CreateTestDB()
	defer util.CloseTestDB(dir, db)
	r.SetLocalDB(local)
	funcName := "ListPendingTx"

	require.NoError(t, db.Set(formatPendingTxKey(1, 1), types.Encode(&rtypes.PendingTx{
		TxBlockHeight: 1,
		TxIndex:       1,
	})))
	require.NoError(t, db.Set(formatPendingTxKey(2, 1), types.Encode(&rtypes.PendingTx{
		TxBlockHeight: 2,
		TxIndex:       1,
	})))
	require.NoError(t, db.Set(formatPendingTxKey(3, 1), types.Encode(&rtypes.PendingTx{
		TxBlockHeight: 3,
		TxIndex:       1,
	})))

	msg, err := r.Query(funcName, types.Encode(&rtypes.ReqListPendingTx{Count: 10, EndHeight: 2}))
	require.NoError(t, err)
	list := msg.(*rtypes.PendingTxs).GetPendingList()
	require.Len(t, list, 2)
	require.Equal(t, int64(1), list[0].GetTxBlockHeight())
	require.Equal(t, int64(2), list[1].GetTxBlockHeight())

	msg, err = r.Query(funcName, types.Encode(&rtypes.ReqListPendingTx{Count: 10, EndHeight: 1}))
	require.NoError(t, err)
	list = msg.(*rtypes.PendingTxs).GetPendingList()
	require.Len(t, list, 1)
	require.Equal(t, int64(1), list[0].GetTxBlockHeight())
}

func TestRgbx_Query_GetPendingTx(t *testing.T) {
	r := newRgbx()
	dir, db, local := util.CreateTestDB()
	defer util.CloseTestDB(dir, db)
	r.SetLocalDB(local)
	funcName := "GetPendingTx"
	require.Nil(t, db.Set(formatPendingTxKey(0, 1), types.Encode(&rtypes.PendingTx{TxIndex: 1})))
	_, err := r.Query(funcName, types.Encode(&rtypes.ReqGetPendingTx{}))
	require.Equal(t, types.ErrNotFound, err)
	msg, err := r.Query(funcName, types.Encode(&rtypes.ReqGetPendingTx{Index: 1}))
	require.Nil(t, err)
	require.Equal(t, int64(1), msg.(*rtypes.PendingTx).GetTxIndex())
}

func TestRgbx_Query_GetConfirmedHeight(t *testing.T) {
	r := newRgbx()
	dir, db, local := util.CreateTestDB()
	defer util.CloseTestDB(dir, db)
	r.SetLocalDB(local)
	funcName := "GetConfirmedHeight"
	_, err := r.Query(funcName, nil)
	require.Nil(t, err)
	require.Nil(t, db.Set([]byte(confirmedHeightKey), types.Encode(&types.Int64{Data: 1})))
	msg, err := r.Query(funcName, nil)
	require.Nil(t, err)
	require.Equal(t, int64(1), msg.(*types.Int64).GetData())
}

func TestRgbx_Query_GetAsset(t *testing.T) {

	r := newRgbx()
	dir, db, local := util.CreateTestDB()
	defer util.CloseTestDB(dir, db)
	api := &mocks.QueueProtocolAPI{}
	r.SetAPI(api)
	cfg := types.NewChain33Config(types.GetDefaultCfgstring())
	api.On("GetConfig").Return(cfg)
	r.SetStateDB(local)
	funcName := "GetAsset"
	_, err := r.Query(funcName, nil)
	require.Equal(t, types.ErrNotFound, err)
	require.Nil(t, db.Set(formatAssetKey("test"), types.Encode(&rtypes.RgbxAsset{Symbol: "test"})))
	msg, err := r.Query(funcName, types.Encode(&types.ReqString{Data: "test"}))
	require.Nil(t, err)
	require.Equal(t, "test", msg.(*rtypes.RgbxAsset).Symbol)
}

func TestRgbx_Query_GetCrossChainInfo(t *testing.T) {
	r := newRgbx()
	dir, db, local := util.CreateTestDB()
	defer util.CloseTestDB(dir, db)
	api := &mocks.QueueProtocolAPI{}
	r.SetAPI(api)
	cfg := types.NewChain33Config(types.GetDefaultCfgstring())
	api.On("GetConfig").Return(cfg)
	r.SetStateDB(local)
	funcName := "GetCrossChainInfo"

	msg, err := r.Query(funcName, types.Encode(&types.ReqString{Data: "btc"}))
	require.NoError(t, err)
	require.Equal(t, "", msg.(*rtypes.CrossChainInfo).GetAssetSymbol())

	info := &rtypes.CrossChainInfo{AssetSymbol: "BTC", TssAddress: "tb1qxx"}
	require.NoError(t, db.Set(formatCrossChainInfoKey("btc"), types.Encode(info)))
	msg, err = r.Query(funcName, types.Encode(&types.ReqString{Data: "btc"}))
	require.NoError(t, err)
	require.Equal(t, "BTC", msg.(*rtypes.CrossChainInfo).GetAssetSymbol())
	require.Equal(t, "tb1qxx", msg.(*rtypes.CrossChainInfo).GetTssAddress())

	require.NoError(t, db.Set(formatCrossChainInfoKey("bad"), []byte("not-protobuf")))
	_, err = r.Query(funcName, types.Encode(&types.ReqString{Data: "bad"}))
	require.Error(t, err)
}

// The record is stored under the ORIGINAL symbol (BTC), while balances and transfers use the
// mapped spelling (XBTC). A query for either spelling must find the same record.
func TestRgbx_Query_GetCrossChainInfo_AcceptsMappedSpelling(t *testing.T) {
	r := newRgbx().(*rgbx)
	dir, state, _ := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	api := &mocks.QueueProtocolAPI{}
	api.On("GetConfig").Return(types.NewChain33Config(types.GetDefaultCfgstring()))
	r.SetAPI(api)
	r.SetStateDB(state)

	info := &rtypes.CrossChainInfo{AssetSymbol: "BTC", TssAddress: "tb1qoriginal", Precision: 8}
	require.NoError(t, state.Set(formatCrossChainInfoKey("btc"), types.Encode(info)))

	for _, spelling := range []string{"BTC", "btc", "XBTC", "xbtc"} {
		msg, err := r.Query_GetCrossChainInfo(&types.ReqString{Data: spelling})
		require.NoError(t, err, "symbol=%s", spelling)
		require.Equal(t, "tb1qoriginal", msg.(*rtypes.CrossChainInfo).GetTssAddress(),
			"symbol=%s must resolve to the original record", spelling)
	}
}

// If both spellings exist as separate records, an explicit query must get the exact one: the
// alias must never silently mask a distinct record.
func TestRgbx_Query_GetCrossChainInfo_ExactSpellingWins(t *testing.T) {
	r := newRgbx().(*rgbx)
	dir, state, _ := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	api := &mocks.QueueProtocolAPI{}
	api.On("GetConfig").Return(types.NewChain33Config(types.GetDefaultCfgstring()))
	r.SetAPI(api)
	r.SetStateDB(state)

	require.NoError(t, state.Set(formatCrossChainInfoKey("btc"),
		types.Encode(&rtypes.CrossChainInfo{AssetSymbol: "BTC", TssAddress: "tb1qoriginal"})))
	require.NoError(t, state.Set(formatCrossChainInfoKey("xbtc"),
		types.Encode(&rtypes.CrossChainInfo{AssetSymbol: "XBTC", TssAddress: "tb1qmapped"})))

	msg, err := r.Query_GetCrossChainInfo(&types.ReqString{Data: "XBTC"})
	require.NoError(t, err)
	require.Equal(t, "tb1qmapped", msg.(*rtypes.CrossChainInfo).GetTssAddress(),
		"an exact XBTC query must prefer the XBTC record")

	msg, err = r.Query_GetCrossChainInfo(&types.ReqString{Data: "BTC"})
	require.NoError(t, err)
	require.Equal(t, "tb1qoriginal", msg.(*rtypes.CrossChainInfo).GetTssAddress(),
		"an exact BTC query must prefer the BTC record")
}
