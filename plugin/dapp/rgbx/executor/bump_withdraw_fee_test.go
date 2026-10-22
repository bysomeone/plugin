package executor

import (
	"testing"

	"github.com/33cn/chain33/common/db"
	"github.com/33cn/chain33/types"
	"github.com/33cn/chain33/util"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/stretchr/testify/require"
)

/*
 * Covers fee-bump requests for open withdrawals: only the original sender may submit one, the
 * burn must still be open, and the requested rate must be above the original withdrawal rate
 * and within maxBtcFeeRate. The chain intentionally does not anchor the rate to a previous bump
 * request, so a later lower-but-still-higher-than-original request remains valid.
 */

type bumpWithdrawFeeFixture struct {
	r        *rgbx
	state    db.DB
	burnHash []byte
	owner    string
	withdraw *rtypes.WithdrawAsset
}

func newBumpWithdrawFeeFixture(t *testing.T) *bumpWithdrawFeeFixture {
	t.Helper()
	r, state, _ := newBridgeConfigTestExec(t, bridgeConfigTestChainCfg())
	f := &bumpWithdrawFeeFixture{
		r:        r,
		state:    state,
		burnHash: []byte("bump-burn-tx"),
		// testCommitAddr is the address of testPriv, so signedTestTx / CreateTransaction
		// produce tx.From() == owner.
		owner:    testCommitAddr,
		withdraw: &rtypes.WithdrawAsset{AssetSymbol: "usdt", Amount: 100, FeeRate: 10},
	}
	require.NoError(t, state.Set(formatPayloadKey(f.burnHash), types.Encode(f.withdraw)))
	symbol := ensureCrossChainSymbol(f.withdraw.GetAssetSymbol())
	opID, err := rtypes.BurnOperationID(symbol, f.burnHash)
	require.NoError(t, err)
	kvs, err := r.recordBurnOperation(opID, symbol, f.owner, f.withdraw.GetAmount(), f.burnHash)
	require.NoError(t, err)
	applyStateKV(t, state, kvs)
	return f
}

func (f *bumpWithdrawFeeFixture) request(feeRate int64) *rtypes.BumpWithdrawFee {
	return &rtypes.BumpWithdrawFee{WithdrawalTxHash: f.burnHash, FeeRate: feeRate}
}

func TestBumpWithdrawFee_CheckTxValidation(t *testing.T) {
	f := newBumpWithdrawFeeFixture(t)

	// Missing withdrawal hash / sender.
	require.ErrorIs(t, f.r.checkBumpWithdrawFee(f.owner, "tx", nil), ErrInvalidBumpWithdrawFeeRequest)
	require.ErrorIs(t, f.r.checkBumpWithdrawFee(f.owner, "tx", &rtypes.BumpWithdrawFee{}), ErrInvalidBumpWithdrawFeeRequest)
	require.ErrorIs(t, f.r.checkBumpWithdrawFee("", "tx", f.request(20)), ErrInvalidBumpWithdrawFeeRequest)

	// Unknown burn.
	require.ErrorIs(t, f.r.checkBumpWithdrawFee(f.owner, "tx", &rtypes.BumpWithdrawFee{
		WithdrawalTxHash: []byte("missing"), FeeRate: 20,
	}), ErrWithdrawNotExist)

	// Only the withdrawal's original sender may bump it.
	other, _ := util.Genaddress()
	require.ErrorIs(t, f.r.checkBumpWithdrawFee(other, "tx", f.request(20)), ErrBumpWithdrawFeeNotOwner)

	// The rate must be strictly above the original withdrawal rate.
	require.ErrorIs(t, f.r.checkBumpWithdrawFee(f.owner, "tx", f.request(10)), ErrBumpWithdrawFeeRateNotHigher)
	require.ErrorIs(t, f.r.checkBumpWithdrawFee(f.owner, "tx", f.request(9)), ErrBumpWithdrawFeeRateNotHigher)

	// And within the fee-rate cap.
	require.ErrorIs(t, f.r.checkBumpWithdrawFee(f.owner, "tx", f.request(maxBtcFeeRate+1)), ErrInvalidWithdrawFeeRate)

	// A higher, in-range rate is accepted.
	require.NoError(t, f.r.checkBumpWithdrawFee(f.owner, "tx", f.request(11)))
	require.NoError(t, f.r.checkBumpWithdrawFee(f.owner, "tx", f.request(maxBtcFeeRate)))

	// Settled and released are both terminal.
	require.NoError(t, f.state.Set(formatWithdrawFinalKey(f.burnHash), withdrawFinalSettled))
	require.ErrorIs(t, f.r.checkBumpWithdrawFee(f.owner, "tx", f.request(20)), ErrWithdrawAlreadyFinal)
	require.NoError(t, f.state.Set(formatWithdrawFinalKey(f.burnHash), withdrawFinalReleased))
	require.ErrorIs(t, f.r.checkBumpWithdrawFee(f.owner, "tx", f.request(20)), ErrWithdrawAlreadyFinal)
}

func TestBumpWithdrawFee_ExecStoresAndLogs(t *testing.T) {
	f := newBumpWithdrawFeeFixture(t)
	height := int64(5)
	f.r.SetEnv(height, 0, 0)

	receipt, err := f.r.Exec_BumpWithdrawFee(f.request(20), signedTestTx("bump-1"), 3)
	require.NoError(t, err)
	require.Len(t, receipt.GetKV(), 3)
	applyStateKV(t, f.state, receipt.GetKV())

	require.Len(t, receipt.GetLogs(), 1)
	require.Equal(t, int32(rtypes.TyBumpWithdrawFeeLog), receipt.GetLogs()[0].GetTy())
	item := &rtypes.BumpWithdrawFeeItem{}
	require.NoError(t, types.Decode(receipt.GetLogs()[0].GetLog(), item))
	require.Equal(t, f.burnHash, item.GetWithdrawalTxHash())
	require.Equal(t, f.owner, item.GetOwner())
	require.Equal(t, int64(20), item.GetFeeRate())
	require.Equal(t, int64(10), item.GetOldFeeRate())
	require.Equal(t, height, item.GetHeight())
	require.Equal(t, int32(3), item.GetIndex())

	// The latest-per-burn record and the per-height index both point at the accepted item.
	latest := &rtypes.BumpWithdrawFeeItem{}
	require.NoError(t, readDB(f.state, formatBumpWithdrawFeeLatestKey(f.burnHash), latest))
	require.Equal(t, item.GetTxHash(), latest.GetTxHash())
	indexed := &rtypes.BumpWithdrawFeeItem{}
	require.NoError(t, readDB(f.state, formatHeightIndexKey(bumpWithdrawFeeRequestKeyPrefix, height, 0), indexed))
	require.Equal(t, item.GetTxHash(), indexed.GetTxHash())

	// A later request only has to beat the original rate: a lower rate than the previous bump
	// is accepted, and the latest record is overwritten.
	f.r.SetEnv(height+2, 0, 0)
	receipt2, err := f.r.Exec_BumpWithdrawFee(f.request(15), signedTestTx("bump-2"), 4)
	require.NoError(t, err)
	applyStateKV(t, f.state, receipt2.GetKV())
	item2 := &rtypes.BumpWithdrawFeeItem{}
	require.NoError(t, types.Decode(receipt2.GetLogs()[0].GetLog(), item2))
	require.Equal(t, int64(15), item2.GetFeeRate())
	require.Equal(t, int64(10), item2.GetOldFeeRate())
	require.NoError(t, readDB(f.state, formatBumpWithdrawFeeLatestKey(f.burnHash), latest))
	require.Equal(t, int64(15), latest.GetFeeRate())
}

func TestBumpWithdrawFee_QueryFiltersByHeight(t *testing.T) {
	f := newBumpWithdrawFeeFixture(t)

	heightA, heightB := int64(5), int64(7)
	f.r.SetEnv(heightA, 0, 0)
	recA, err := f.r.Exec_BumpWithdrawFee(f.request(20), signedTestTx("bump-a"), 0)
	require.NoError(t, err)
	applyStateKV(t, f.state, recA.GetKV())

	f.r.SetEnv(heightB, 0, 0)
	recB, err := f.r.Exec_BumpWithdrawFee(f.request(30), signedTestTx("bump-b"), 2)
	require.NoError(t, err)
	applyStateKV(t, f.state, recB.GetKV())

	// StartHeight excludes the earlier request.
	list, err := f.r.Query_ListBumpWithdrawFeeRequests(&rtypes.ReqListPendingTx{
		StartHeight: heightB, EndHeight: heightB, Count: 10,
	})
	require.NoError(t, err)
	require.Len(t, list.GetItems(), 1)
	require.Equal(t, heightB, list.GetItems()[0].GetHeight())
	require.Equal(t, int32(2), list.GetItems()[0].GetIndex())
	require.Equal(t, int64(30), list.GetItems()[0].GetFeeRate())

	// Both requests in the full range, ascending by height.
	list, err = f.r.Query_ListBumpWithdrawFeeRequests(&rtypes.ReqListPendingTx{
		StartHeight: 0, EndHeight: heightB, Count: 10,
	})
	require.NoError(t, err)
	require.Len(t, list.GetItems(), 2)
	require.Equal(t, heightA, list.GetItems()[0].GetHeight())
	require.Equal(t, heightB, list.GetItems()[1].GetHeight())

	// Count is a hard cap.
	list, err = f.r.Query_ListBumpWithdrawFeeRequests(&rtypes.ReqListPendingTx{
		StartHeight: 0, EndHeight: heightB, Count: 1,
	})
	require.NoError(t, err)
	require.Len(t, list.GetItems(), 1)

	// StartIndex skips requests with a lower block transaction index at StartHeight.
	list, err = f.r.Query_ListBumpWithdrawFeeRequests(&rtypes.ReqListPendingTx{
		StartHeight: heightB, EndHeight: heightB, StartIndex: 3, Count: 10,
	})
	require.NoError(t, err)
	require.Empty(t, list.GetItems())

	// Invalid arguments are rejected.
	_, err = f.r.Query_ListBumpWithdrawFeeRequests(&rtypes.ReqListPendingTx{StartHeight: 0, Count: 0})
	require.ErrorIs(t, err, types.ErrInvalidParam)
	_, err = f.r.Query_ListBumpWithdrawFeeRequests(&rtypes.ReqListPendingTx{StartHeight: 5, EndHeight: 1, Count: 1})
	require.ErrorIs(t, err, types.ErrInvalidParam)
}

func TestBumpWithdrawFee_CheckTxDispatch(t *testing.T) {
	f := newBumpWithdrawFeeFixture(t)
	action := &rtypes.RgbxAction{
		Ty:    rtypes.TyBumpWithdrawFee,
		Value: &rtypes.RgbxAction_BumpWithdrawFee{BumpWithdrawFee: f.request(20)},
	}
	tx := &types.Transaction{Payload: types.Encode(action)}
	tx.Sign(types.SECP256K1, testPriv)
	require.NoError(t, f.r.CheckTx(tx, 0))

	action.Value = &rtypes.RgbxAction_BumpWithdrawFee{BumpWithdrawFee: f.request(5)}
	tx.Payload = types.Encode(action)
	require.ErrorIs(t, f.r.CheckTx(tx, 0), ErrBumpWithdrawFeeRateNotHigher)
}

func TestBumpWithdrawFee_ExecDispatch(t *testing.T) {
	f := newBumpWithdrawFeeFixture(t)
	f.r.SetEnv(9, 0, 0)
	tx, err := f.r.GetExecutorType().CreateTransaction(rtypes.NameBumpWithdrawFeeAction, f.request(20))
	require.NoError(t, err)
	tx.Sign(types.SECP256K1, testPriv)
	receipt, err := f.r.Exec(tx, 0)
	require.NoError(t, err)
	require.Len(t, receipt.GetLogs(), 1)
	require.Equal(t, int32(rtypes.TyBumpWithdrawFeeLog), receipt.GetLogs()[0].GetTy())
}

func TestBumpWithdrawFee_QueryUsesRegisteredName(t *testing.T) {
	f := newBumpWithdrawFeeFixture(t)
	f.r.SetEnv(1, 0, 0)
	rec, err := f.r.Exec_BumpWithdrawFee(f.request(20), signedTestTx("bump-dispatch"), 0)
	require.NoError(t, err)
	applyStateKV(t, f.state, rec.GetKV())

	msg, err := f.r.Query("ListBumpWithdrawFeeRequests", types.Encode(&rtypes.ReqListPendingTx{
		StartHeight: 0, EndHeight: 1, Count: 10,
	}))
	require.NoError(t, err)
	list, ok := msg.(*rtypes.BumpWithdrawFeeList)
	require.True(t, ok)
	require.Len(t, list.GetItems(), 1)
}
