package executor

import (
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/33cn/chain33/system/dapp"
	"github.com/33cn/chain33/types"
	ltypes "github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
)

/*
 * Per-user BTC deposit address registration.
 *
 * The address is deterministic: it is derived from (tx.From(), BTC CrossChainInfo.pubkey)
 * by the frozen p2wsh-deposit-v1 spec (types/p2wsh_deposit.go). Registration makes the
 * address visible to the bridge and to the user: the executor stores the item on chain
 * and returns it in the receipt log.
 *
 * Querying only reads the registry; it never derives on the fly. An address returned by
 * derivation but not registered has never been announced to the bridge, so showing it to
 * a user could make them deposit into a script the bridge does not watch.
 *
 * Registration is idempotent: a second RegisterDepositScript from the same tx.From()
 * returns the stored item without writing anything. The registered item is immutable
 * (the TSS pubkey never changes after DKG; re-keygen would invalidate every address).
 */

const (
	// depositScriptKeyPrefix indexes the registration by user id (the chain33 address that
	// is also the P2WSH derivation label): mavl-rgbx-deposit-script-<userID>.
	depositScriptKeyPrefix = KeyPrefixStateDB + "deposit-script-"
	// depositScriptIndexKeyPrefix indexes registrations by (height, per-height sequence) so
	// the height-range query can enumerate them. StateDB has no List (same as abandon-req-).
	depositScriptIndexKeyPrefix = KeyPrefixStateDB + "deposit-script-idx-"
	// depositScriptHeightCountKeyPrefix stores how many registrations exist at a height.
	depositScriptHeightCountKeyPrefix = KeyPrefixStateDB + "deposit-script-count-"
	// maxDepositScriptHeightScan bounds how many heights one listing call walks; callers page
	// by advancing StartHeight (same bound as the abandon listing).
	maxDepositScriptHeightScan = int64(maxListCount)
)

// formatDepositScriptKey is the registry key of one user's deposit address.
func formatDepositScriptKey(userID string) []byte {
	return []byte(depositScriptKeyPrefix + userID)
}

// formatDepositScriptIndexKey indexes a registration by (height, per-height sequence).
func formatDepositScriptIndexKey(height, sequence int64) []byte {
	return []byte(depositScriptIndexKeyPrefix + dapp.HeightIndexStr(height, sequence))
}

// formatDepositScriptHeightCountKey stores how many registrations exist at a height.
func formatDepositScriptHeightCountKey(height int64) []byte {
	return []byte(fmt.Sprintf("%s%020d", depositScriptHeightCountKeyPrefix, height))
}

// depositScriptHeightCount reads the number of registrations at a height (missing = 0).
func (r *rgbx) depositScriptHeightCount(height int64) (int64, error) {
	if r.GetStateDB() == nil {
		return 0, nil
	}
	value, err := r.GetStateDB().Get(formatDepositScriptHeightCountKey(height))
	if errors.Is(err, types.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	count := &types.Int64{}
	if err = types.Decode(value, count); err != nil {
		elog.Error("depositScriptHeightCount decode", "height", height, "err", err)
		return 0, err
	}
	return count.GetData(), nil
}

// checkRegisterDepositScript is the cheap CheckTx gate. The expensive part (deriving the
// pkScript and the bech32 address, which needs the lightclient network name) is left to
// Exec. The group pubkey must exist and be a valid compressed key: without it no user
// address can be derived, so accepting the transaction would only fail later.
func (r *rgbx) checkRegisterDepositScript(txHash, userID string) error {
	if userID == "" {
		elog.Error("checkRegisterDepositScript empty user id", "txHash", txHash)
		return ErrInvalidDepositScriptRequest
	}
	info, err := r.getCrossChainInfo(rtypes.BTCSymbol)
	if err != nil {
		elog.Error("checkRegisterDepositScript getCrossChainInfo", "txHash", txHash,
			"symbol", rtypes.BTCSymbol, "err", err)
		return ErrGetCrossChainInfo
	}
	if _, err = rtypes.ParseDepositTssPubKey(info.GetPubkey()); err != nil {
		elog.Error("checkRegisterDepositScript invalid tss pubkey", "txHash", txHash,
			"symbol", rtypes.BTCSymbol, "pubkeyLen", len(info.GetPubkey()), "err", err)
		return ErrInvalidCrossChainInfo
	}
	return nil
}

// deriveDepositScriptItem derives the address and pkScript for a user. It reads the BTC
// group pubkey from consensus state and the BTC network name from the lightclient query,
// matching the helpers used by the deposit/withdraw paths.
func (r *rgbx) deriveDepositScriptItem(userID string) (*rtypes.DepositScriptItem, error) {
	info, err := r.getCrossChainInfo(rtypes.BTCSymbol)
	if err != nil {
		elog.Error("deriveDepositScriptItem getCrossChainInfo", "symbol", rtypes.BTCSymbol, "err", err)
		return nil, ErrGetCrossChainInfo
	}
	tssPub, err := rtypes.ParseDepositTssPubKey(info.GetPubkey())
	if err != nil {
		elog.Error("deriveDepositScriptItem invalid tss pubkey", "symbol", rtypes.BTCSymbol,
			"pubkeyLen", len(info.GetPubkey()), "err", err)
		return nil, ErrInvalidCrossChainInfo
	}
	pkScript, err := rtypes.DeriveDepositPkScript(userID, tssPub)
	if err != nil {
		elog.Error("deriveDepositScriptItem derive pkScript", "userID", userID, "err", err)
		return nil, ErrInvalidDepositScriptRequest
	}
	netName, err := r.getBtcNetName()
	if err != nil {
		elog.Error("deriveDepositScriptItem getBtcNetName", "userID", userID, "err", err)
		return nil, err
	}
	address, err := rtypes.DeriveDepositAddress(userID, tssPub, ltypes.GetBtcChainParams(netName))
	if err != nil {
		elog.Error("deriveDepositScriptItem derive address", "userID", userID,
			"netName", netName, "err", err)
		return nil, ErrInvalidDepositScriptRequest
	}
	return &rtypes.DepositScriptItem{
		UserID:   userID,
		Address:  address,
		PkScript: hex.EncodeToString(pkScript),
		Spec:     rtypes.P2WSHDepositSpecV1,
	}, nil
}

// Exec_RegisterDepositScript binds the sender's deposit address on chain.
//
// The user id is tx.From(); it is the same raw byte string used as the P2WSH derivation
// label, so the receipt and every later deposit check agree on the owner.
// If a registration already exists, it is returned unchanged: no KV is written, so the
// per-height index and its counter are never duplicated.
func (r *rgbx) Exec_RegisterDepositScript(_ *rtypes.RegisterDepositScript, tx *types.Transaction, index int) (*types.Receipt, error) {
	txHash := hex.EncodeToString(tx.Hash())
	userID := tx.From()
	if userID == "" {
		elog.Error("Exec_RegisterDepositScript empty user id", "txHash", txHash)
		return nil, ErrInvalidDepositScriptRequest
	}
	key := formatDepositScriptKey(userID)

	registered := &rtypes.DepositScriptItem{}
	if err := readDB(r.GetStateDB(), key, registered); err == nil {
		elog.Info("Exec_RegisterDepositScript already registered", "txHash", txHash,
			"userID", userID, "address", registered.GetAddress(), "height", registered.GetHeight())
		return &types.Receipt{
			Ty: types.ExecOk,
			Logs: []*types.ReceiptLog{
				{Ty: rtypes.TyDepositScriptLog, Log: types.Encode(registered)},
			},
		}, nil
	} else if !errors.Is(err, types.ErrNotFound) {
		elog.Error("Exec_RegisterDepositScript read registration", "txHash", txHash,
			"userID", userID, "err", err)
		return nil, err
	}

	item, err := r.deriveDepositScriptItem(userID)
	if err != nil {
		return nil, err
	}
	height := r.GetHeight()
	item.Height = height
	item.Index = int32(index)

	sequence, err := r.depositScriptHeightCount(height)
	if err != nil {
		return nil, err
	}
	elog.Info("Exec_RegisterDepositScript registered", "txHash", txHash, "userID", userID,
		"address", item.GetAddress(), "pkScript", item.GetPkScript(), "spec", item.GetSpec(),
		"height", height, "index", index)
	return &types.Receipt{
		Ty: types.ExecOk,
		KV: []*types.KeyValue{
			{Key: key, Value: types.Encode(item)},
			{Key: formatDepositScriptIndexKey(height, sequence), Value: types.Encode(item)},
			{Key: formatDepositScriptHeightCountKey(height), Value: types.Encode(&types.Int64{Data: sequence + 1})},
		},
		Logs: []*types.ReceiptLog{
			{Ty: rtypes.TyDepositScriptLog, Log: types.Encode(item)},
		},
	}, nil
}

// Query_GetDepositAddress returns the registered deposit address of a user id.
//
// Read-only: it never derives. An unregistered user yields types.ErrNotFound so callers
// can tell "not bound yet" apart from an error, and never show an address the bridge has
// not been told about.
func (r *rgbx) Query_GetDepositAddress(req *types.ReqString) (*rtypes.DepositScriptItem, error) {
	userID := req.GetData()
	if userID == "" {
		return nil, types.ErrInvalidParam
	}
	item := &rtypes.DepositScriptItem{}
	if err := readDB(r.GetStateDB(), formatDepositScriptKey(userID), item); err != nil {
		if !errors.Is(err, types.ErrNotFound) {
			elog.Error("Query_GetDepositAddress get registration", "userID", userID, "err", err)
		}
		return nil, err
	}
	return item, nil
}

// Query_ListDepositRegistrations lists registrations in a height range. StartIndex skips
// registrations whose block transaction index is lower at StartHeight. EndHeight = 0
// means "up to the current height". StateDB has no List, so at most
// maxDepositScriptHeightScan heights are walked per call; callers page by advancing
// StartHeight. The storage layout and paging rules mirror Query_ListAbandonRequests.
func (r *rgbx) Query_ListDepositRegistrations(req *rtypes.ReqListPendingTx) (*rtypes.DepositScriptList, error) {
	if req.GetCount() <= 0 || req.GetCount() > maxListCount {
		return nil, types.ErrInvalidParam
	}
	start := req.GetStartHeight()
	if start < 0 {
		return nil, types.ErrInvalidParam
	}
	end := req.GetEndHeight()
	if end > 0 && end < start {
		return nil, types.ErrInvalidParam
	}
	if end <= 0 {
		end = r.GetHeight()
		if end < start {
			end = start + maxDepositScriptHeightScan - 1
		}
	}
	if end-start+1 > maxDepositScriptHeightScan {
		end = start + maxDepositScriptHeightScan - 1
	}
	reply := &rtypes.DepositScriptList{}
	for height := start; height <= end; height++ {
		count, err := r.depositScriptHeightCount(height)
		if err != nil {
			return nil, err
		}
		for sequence := int64(0); sequence < count; sequence++ {
			item := &rtypes.DepositScriptItem{}
			if err = readDB(r.GetStateDB(), formatDepositScriptIndexKey(height, sequence), item); err != nil {
				elog.Error("Query_ListDepositRegistrations read item", "height", height,
					"sequence", sequence, "err", err)
				return nil, err
			}
			if height == start && int64(item.GetIndex()) < req.GetStartIndex() {
				continue
			}
			reply.Items = append(reply.Items, item)
			if int32(len(reply.Items)) >= req.GetCount() {
				return reply, nil
			}
		}
	}
	return reply, nil
}
