package executor

import (
	"encoding/hex"
	"testing"

	"github.com/33cn/chain33/common/db"
	"github.com/33cn/chain33/system/dapp"
	"github.com/33cn/chain33/types"
	"github.com/33cn/chain33/util"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/stretchr/testify/require"
)

/*
 * Covers abandon-withdraw proposals: the chain only records them, but it must be impossible
 * to record a proposal that names a different owner, an unknown burn, or an already-final
 * withdrawal. Multiple proposals for the same burn are allowed by design.
 */

type abandonFixture struct {
	r        *rgbx
	state    db.DB
	burnHash []byte
	owner    string
}

func newAbandonFixture(t *testing.T) *abandonFixture {
	t.Helper()
	r, state, _ := newBridgeConfigTestExec(t, bridgeConfigTestChainCfg())
	owner, _ := util.Genaddress()
	f := &abandonFixture{r: r, state: state, burnHash: []byte("abandon-burn-tx"), owner: owner}
	require.NoError(t, state.Set(formatPayloadKey(f.burnHash), types.Encode(&rtypes.WithdrawAsset{
		AssetSymbol: "usdt",
		Amount:      100,
	})))
	symbol := ensureCrossChainSymbol("usdt")
	opID, err := rtypes.BurnOperationID(symbol, f.burnHash)
	require.NoError(t, err)
	kvs, err := r.recordBurnOperation(opID, symbol, owner, 100, f.burnHash)
	require.NoError(t, err)
	applyStateKV(t, state, kvs)
	return f
}

func (f *abandonFixture) request() *rtypes.AbandonWithdraw {
	return &rtypes.AbandonWithdraw{BurnTxHash: f.burnHash, Owner: f.owner, Reason: "stuck"}
}

func TestAbandon_CheckTxValidation(t *testing.T) {
	f := newAbandonFixture(t)

	// Empty burn hash / owner are rejected.
	require.ErrorIs(t, f.r.checkAbandon("tx", &rtypes.AbandonWithdraw{Owner: f.owner}), ErrInvalidAbandonRequest)
	require.ErrorIs(t, f.r.checkAbandon("tx", &rtypes.AbandonWithdraw{BurnTxHash: f.burnHash}), ErrInvalidAbandonRequest)

	// Unknown burn.
	require.ErrorIs(t, f.r.checkAbandon("tx", &rtypes.AbandonWithdraw{
		BurnTxHash: []byte("missing"), Owner: f.owner,
	}), ErrWithdrawNotExist)

	// Owner must match the ledger byte for byte.
	other, _ := util.Genaddress()
	require.ErrorIs(t, f.r.checkAbandon("tx", &rtypes.AbandonWithdraw{
		BurnTxHash: f.burnHash, Owner: other,
	}), ErrOperationOwnerMismatch)

	// A settled burn has nothing left to abandon.
	require.NoError(t, f.state.Set(formatWithdrawFinalKey(f.burnHash), withdrawFinalSettled))
	require.ErrorIs(t, f.r.checkAbandon("tx", f.request()), ErrWithdrawAlreadyFinal)

	// Released is terminal as well.
	require.NoError(t, f.state.Set(formatWithdrawFinalKey(f.burnHash), withdrawFinalReleased))
	require.ErrorIs(t, f.r.checkAbandon("tx", f.request()), ErrWithdrawAlreadyFinal)
}

func TestAbandon_CheckTxDispatch(t *testing.T) {
	f := newAbandonFixture(t)
	action := &rtypes.RgbxAction{
		Ty:    rtypes.TyAbandonWithdraw,
		Value: &rtypes.RgbxAction_Abandon{Abandon: f.request()},
	}
	tx := &types.Transaction{Payload: types.Encode(action)}
	require.NoError(t, f.r.CheckTx(tx, 0))

	action.Value = &rtypes.RgbxAction_Abandon{Abandon: &rtypes.AbandonWithdraw{BurnTxHash: f.burnHash}}
	tx.Payload = types.Encode(action)
	require.ErrorIs(t, f.r.CheckTx(tx, 0), ErrInvalidAbandonRequest)
}

func TestAbandon_ExecStoresAndLogs(t *testing.T) {
	f := newAbandonFixture(t)
	height := int64(5)
	f.r.SetEnv(height, 0, 0)

	req := f.request()
	receipt, err := f.r.Exec_Abandon(req, signedTestTx("abandon-1"), 3)
	require.NoError(t, err)
	require.Len(t, receipt.GetKV(), 2)
	applyStateKV(t, f.state, receipt.GetKV())

	require.Len(t, receipt.GetLogs(), 1)
	require.Equal(t, int32(rtypes.TyAbandonWithdrawLog), receipt.GetLogs()[0].GetTy())
	item := &rtypes.AbandonWithdrawItem{}
	require.NoError(t, types.Decode(receipt.GetLogs()[0].GetLog(), item))
	require.Equal(t, f.burnHash, item.GetBurnTxHash())
	require.Equal(t, f.owner, item.GetOwner())
	require.Equal(t, "stuck", item.GetReason())
	require.Equal(t, height, item.GetHeight())
	require.Equal(t, int32(3), item.GetIndex())

	// A second proposal for the same burn is allowed (the first valid one wins off-chain).
	require.NoError(t, f.r.checkAbandon("tx", f.request()))
	receipt2, err := f.r.Exec_Abandon(f.request(), signedTestTx("abandon-2"), 4)
	require.NoError(t, err)
	applyStateKV(t, f.state, receipt2.GetKV())
}

func TestAbandon_QueryFiltersByHeight(t *testing.T) {
	f := newAbandonFixture(t)

	heightA, heightB := int64(5), int64(7)
	f.r.SetEnv(heightA, 0, 0)
	recA, err := f.r.Exec_Abandon(f.request(), signedTestTx("abandon-a"), 0)
	require.NoError(t, err)
	applyStateKV(t, f.state, recA.GetKV())

	f.r.SetEnv(heightB, 0, 0)
	recB, err := f.r.Exec_Abandon(f.request(), signedTestTx("abandon-b"), 2)
	require.NoError(t, err)
	applyStateKV(t, f.state, recB.GetKV())

	// StartHeight excludes the earlier proposal.
	list, err := f.r.Query_ListAbandonRequests(&rtypes.ReqListPendingTx{
		StartHeight: heightB, EndHeight: heightB, Count: 10,
	})
	require.NoError(t, err)
	require.Len(t, list.GetItems(), 1)
	require.Equal(t, heightB, list.GetItems()[0].GetHeight())
	require.Equal(t, int32(2), list.GetItems()[0].GetIndex())

	// Both proposals in the full range, ascending by height.
	list, err = f.r.Query_ListAbandonRequests(&rtypes.ReqListPendingTx{
		StartHeight: 0, EndHeight: heightB, Count: 10,
	})
	require.NoError(t, err)
	require.Len(t, list.GetItems(), 2)
	require.Equal(t, heightA, list.GetItems()[0].GetHeight())
	require.Equal(t, heightB, list.GetItems()[1].GetHeight())

	// Count is a hard cap.
	list, err = f.r.Query_ListAbandonRequests(&rtypes.ReqListPendingTx{
		StartHeight: 0, EndHeight: heightB, Count: 1,
	})
	require.NoError(t, err)
	require.Len(t, list.GetItems(), 1)

	// Invalid arguments are rejected.
	_, err = f.r.Query_ListAbandonRequests(&rtypes.ReqListPendingTx{StartHeight: 0, Count: 0})
	require.ErrorIs(t, err, types.ErrInvalidParam)
	_, err = f.r.Query_ListAbandonRequests(&rtypes.ReqListPendingTx{StartHeight: 5, EndHeight: 1, Count: 1})
	require.ErrorIs(t, err, types.ErrInvalidParam)
}

func TestAbandon_QueryUsesRegisteredName(t *testing.T) {
	f := newAbandonFixture(t)
	f.r.SetEnv(1, 0, 0)
	rec, err := f.r.Exec_Abandon(f.request(), signedTestTx("abandon-dispatch"), 0)
	require.NoError(t, err)
	applyStateKV(t, f.state, rec.GetKV())

	// The chain side calls "ListAbandonRequests"; the exported method name is the contract.
	msg, err := f.r.Query("ListAbandonRequests", types.Encode(&rtypes.ReqListPendingTx{
		StartHeight: 0, EndHeight: 1, Count: 10,
	}))
	require.NoError(t, err)
	list, ok := msg.(*rtypes.AbandonWithdrawList)
	require.True(t, ok)
	require.Len(t, list.GetItems(), 1)
	require.Equal(t, hex.EncodeToString(rec.GetKV()[0].GetKey()), hex.EncodeToString(formatAbandonRequestKey(1, 0)))
}

func TestAbandon_ExecDispatch(t *testing.T) {
	f := newAbandonFixture(t)
	f.r.SetEnv(9, 0, 0)
	tx, err := f.r.GetExecutorType().CreateTransaction(rtypes.NameAbandonWithdrawAction, f.request())
	require.NoError(t, err)
	tx.Sign(types.SECP256K1, testPriv)
	receipt, err := f.r.Exec(tx, 0)
	require.NoError(t, err)
	require.Len(t, receipt.GetLogs(), 1)
}

// Keep the dapp import meaningful: HeightIndexStr is the ordering shared with pendingTxs.
func TestAbandon_KeyOrdering(t *testing.T) {
	require.Less(t, string(formatAbandonRequestKey(1, 100)), string(formatAbandonRequestKey(2, 0)))
	require.Equal(t, dapp.HeightIndexStr(3, 4), string(formatAbandonRequestKey(3, 4)[len(abandonRequestKeyPrefix):]))
}
