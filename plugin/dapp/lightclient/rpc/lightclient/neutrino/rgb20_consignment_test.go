package neutrino

import (
	"crypto/sha256"
	"testing"

	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/stretchr/testify/require"
)

// fakeConsignmentSource 让确认载荷的组装脱离完整提现流程可测。
type fakeConsignmentSource struct {
	rgb  bool
	hash []byte
}

func (f fakeConsignmentSource) IsRgb20WithdrawTxid(string) bool       { return f.rgb }
func (f fakeConsignmentSource) WithdrawConsignmentHash([]byte) []byte { return f.hash }

// Test_attachRgb20ConsignmentHash covers how the confirm payload gets its consignment hash:
// only RGB20 withdrawals, only a 32-byte hash, and a missing local record leaves it empty
// (the confirm is still submitted — settlement must not wait for delivery).
func Test_attachRgb20ConsignmentHash(t *testing.T) {
	pending := &btcPendingTx{txHash: chainhash.Hash{0x01}, chain33WithdrawTxHash: []byte("burn")}
	sum := sha256.Sum256([]byte("consignment-bytes"))

	t.Run("rgb20 with a saved consignment hash", func(t *testing.T) {
		confirm := &rtypes.ConfirmTx{}
		attachRgb20ConsignmentHashFrom(confirm, pending, fakeConsignmentSource{rgb: true, hash: sum[:]})
		require.Equal(t, sum[:], confirm.GetConsignmentHash())
	})

	t.Run("rgb20 without a local record submits an empty hash", func(t *testing.T) {
		confirm := &rtypes.ConfirmTx{}
		attachRgb20ConsignmentHashFrom(confirm, pending, fakeConsignmentSource{rgb: true})
		require.Empty(t, confirm.GetConsignmentHash())
	})

	t.Run("malformed hash is treated as missing", func(t *testing.T) {
		confirm := &rtypes.ConfirmTx{}
		attachRgb20ConsignmentHashFrom(confirm, pending, fakeConsignmentSource{rgb: true, hash: make([]byte, 31)})
		require.Empty(t, confirm.GetConsignmentHash())
	})

	t.Run("btc withdrawal never gets a hash", func(t *testing.T) {
		confirm := &rtypes.ConfirmTx{}
		attachRgb20ConsignmentHashFrom(confirm, pending, fakeConsignmentSource{rgb: false, hash: sum[:]})
		require.Empty(t, confirm.GetConsignmentHash())
	})

	t.Run("nil inputs are tolerated", func(t *testing.T) {
		attachRgb20ConsignmentHashFrom(nil, pending, fakeConsignmentSource{rgb: true, hash: sum[:]})
		attachRgb20ConsignmentHashFrom(&rtypes.ConfirmTx{}, nil, fakeConsignmentSource{rgb: true, hash: sum[:]})
		attachRgb20ConsignmentHashFrom(&rtypes.ConfirmTx{}, pending, nil)
	})
}
