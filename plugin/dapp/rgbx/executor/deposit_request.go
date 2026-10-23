package executor

import (
	"errors"
	"fmt"

	"github.com/33cn/chain33/types"
	"github.com/33cn/plugin/plugin/dapp/rgbx/depositrequest"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
)

/*
 * User-signed deposit requests on the mint path.
 *
 * An RGB20 deposit invoice is issued off chain by the bridge, so the chain has no record that
 * the mint's recipient ever asked for it. The mint therefore carries the user's signed request:
 * the signature covers the chain title, request id, address, asset, amount and timestamp. The
 * chain verifies it here, which means neither the bridge nor the gateway can redirect a mint or
 * change its amount. The request id is recorded on success and rejected if reused.
 *
 * BTC deposits keep their on-chain binding (RegisterDepositScript), so this check applies to
 * RGB20 assets only.
 */

var (
	// ErrInvalidDepositRequest the mint carries no usable signed request (missing fields, bad
	// signature, or content that does not match the deposit payload).
	ErrInvalidDepositRequest = errors.New("invalid deposit request")
	// ErrDepositRequestReplay the request id was already used by an earlier mint.
	ErrDepositRequestReplay = errors.New("deposit request already used")
)

// formatDepositRequestUsedKey is the consensus-state marker written when an RGB20 mint
// succeeds. Request ids are restricted to [A-Za-z0-9_-] by the signing package, so they are
// safe inside a key.
func formatDepositRequestUsedKey(requestID string) []byte {
	return []byte(KeyPrefixStateDB + "deposit-request-used-" + requestID)
}

// checkDepositRequest verifies the signed request and rejects replays. It is called from both
// CheckTx and Exec: the latter may be reached without the mempool check.
func (r *rgbx) checkDepositRequest(deposit *rtypes.DepositAsset) error {
	if deposit == nil || deposit.GetRequestId() == "" {
		elog.Error("checkDepositRequest missing request")
		return ErrInvalidDepositRequest
	}
	api := r.GetAPI()
	if api == nil || api.GetConfig() == nil {
		elog.Error("checkDepositRequest chain config unavailable")
		return ErrInvalidDepositRequest
	}
	req := depositrequest.Request{
		RequestID:   deposit.GetRequestId(),
		Chain33Addr: deposit.GetDepositAddress(),
		AssetSymbol: deposit.GetAssetSymbol(),
		Amount:      deposit.GetAmount(),
		Timestamp:   deposit.GetRequestTimestamp(),
	}
	if err := depositrequest.Verify(api.GetConfig().GetTitle(), req,
		deposit.GetRequestPubkey(), deposit.GetRequestSignature()); err != nil {
		elog.Error("checkDepositRequest verify", "requestId", req.RequestID,
			"addr", req.Chain33Addr, "symbol", req.AssetSymbol, "amount", req.Amount, "err", err)
		return fmt.Errorf("%w: %v", ErrInvalidDepositRequest, err)
	}
	_, err := r.GetStateDB().Get(formatDepositRequestUsedKey(req.RequestID))
	switch {
	case err == nil:
		elog.Error("checkDepositRequest replay", "requestId", req.RequestID)
		return ErrDepositRequestReplay
	case errors.Is(err, types.ErrNotFound):
		return nil
	default:
		elog.Error("checkDepositRequest read used marker", "requestId", req.RequestID, "err", err)
		return err
	}
}

// depositRequestUsedKV records the request id once the mint succeeds.
func depositRequestUsedKV(deposit *rtypes.DepositAsset) *types.KeyValue {
	return &types.KeyValue{
		Key:   formatDepositRequestUsedKey(deposit.GetRequestId()),
		Value: []byte("used"),
	}
}
