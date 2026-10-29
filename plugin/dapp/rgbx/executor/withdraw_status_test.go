package executor

import (
	"encoding/hex"
	"testing"

	"github.com/33cn/chain33/types"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/stretchr/testify/require"
)

// The status string is derived, never invented from logs: a consensus terminal state wins,
// a local pending record only means "still waiting", and no signal at all is unknown.
func TestDeriveWithdrawStatus(t *testing.T) {
	cases := []struct {
		name       string
		hasPending bool
		finalState string
		want       string
	}{
		{"burn still pending", true, "", withdrawStatusPending},
		{"settled", true, withdrawStatusSettled, withdrawStatusSettled},
		{"released", true, withdrawStatusReleased, withdrawStatusReleased},
		{"terminal without a local pending record", false, withdrawStatusSettled, withdrawStatusSettled},
		{"no local index and no terminal state", false, "", withdrawStatusUnknown},
		{"unrecognized stored value", false, "bogus", withdrawStatusUnknown},
		{"terminal state overrides pending", true, withdrawStatusReleased, withdrawStatusReleased},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, deriveWithdrawStatus(tc.hasPending, tc.finalState))
		})
	}
}

func TestQuery_GetWithdrawStatus(t *testing.T) {
	r, state, _ := newBridgeConfigTestExec(t, bridgeConfigTestChainCfg())
	local := r.GetLocalDB()
	burn := []byte("withdraw-status-burn")
	req := &types.ReqString{Data: hex.EncodeToString(burn)}

	// No local index, no terminal state: unknown rather than a made-up status.
	msg, err := r.Query_GetWithdrawStatus(req)
	require.NoError(t, err)
	status := msg.(*rtypes.WithdrawStatus)
	require.Equal(t, withdrawStatusUnknown, status.GetStatus())
	require.Nil(t, status.GetPending())
	require.Empty(t, status.GetFinalState())

	// A local pending record makes the burn visible as pending.
	require.NoError(t, local.Set(formatPendingTxKey(5, 2), types.Encode(&rtypes.PendingTx{
		TxBlockHeight: 5, TxIndex: 2, TxHash: burn, Amount: 42, AssetSymbol: "RGB20_USDT",
	})))
	require.NoError(t, local.Set(formatPendingTxHashKey(burn),
		types.Encode(&rtypes.TxBlockIndex{BlockHeight: 5, TxIndex: 2})))

	// The CLI accepts the 0x form the transaction hash is printed with.
	msg, err = r.Query_GetWithdrawStatus(&types.ReqString{Data: "0x" + req.GetData()})
	require.NoError(t, err)
	status = msg.(*rtypes.WithdrawStatus)
	require.Equal(t, withdrawStatusPending, status.GetStatus())
	require.Equal(t, int64(42), status.GetPending().GetAmount())
	require.Empty(t, status.GetFinalState())
	require.Empty(t, status.GetConsignmentHash())

	// Consensus state is authoritative: settled wins over the local pending record.
	require.NoError(t, state.Set(formatWithdrawFinalKey(burn), withdrawFinalSettled))
	require.NoError(t, state.Set(formatWithdrawConsignmentKey(burn), []byte{0xaa, 0xbb}))
	msg, err = r.Query_GetWithdrawStatus(req)
	require.NoError(t, err)
	status = msg.(*rtypes.WithdrawStatus)
	require.Equal(t, withdrawStatusSettled, status.GetStatus())
	require.Equal(t, "settled", status.GetFinalState())
	require.Equal(t, "aabb", status.GetConsignmentHash())
	require.NotNil(t, status.GetPending(), "the local record stays supplementary")

	// released means the burn was refunded through Unlock.
	require.NoError(t, state.Set(formatWithdrawFinalKey(burn), withdrawFinalReleased))
	msg, err = r.Query_GetWithdrawStatus(req)
	require.NoError(t, err)
	require.Equal(t, withdrawStatusReleased, msg.(*rtypes.WithdrawStatus).GetStatus())

	// A tombstoned local record is not reported as pending.
	confirmedBurn := []byte("withdraw-status-confirmed")
	require.NoError(t, local.Set(formatPendingTxKey(6, 1), types.Encode(&rtypes.PendingTx{Confirmed: true})))
	require.NoError(t, local.Set(formatPendingTxHashKey(confirmedBurn),
		types.Encode(&rtypes.TxBlockIndex{BlockHeight: 6, TxIndex: 1})))
	msg, err = r.Query_GetWithdrawStatus(&types.ReqString{Data: hex.EncodeToString(confirmedBurn)})
	require.NoError(t, err)
	status = msg.(*rtypes.WithdrawStatus)
	require.Nil(t, status.GetPending())
	require.Equal(t, withdrawStatusUnknown, status.GetStatus())

	for _, bad := range []string{"", "0x", "not-hex"} {
		_, err = r.Query_GetWithdrawStatus(&types.ReqString{Data: bad})
		require.ErrorIs(t, err, types.ErrInvalidParam, "input %q is not a burn tx hash", bad)
	}
}
