package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/33cn/chain33/client/mocks"
	"github.com/33cn/chain33/common/db"
	"github.com/33cn/chain33/types"
	"github.com/33cn/chain33/util"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

/*
 * Covers the on-chain side of Unlock (#70): the escape hatch that refunds the amount still
 * locked for an unsettled withdrawal burn.
 *
 * The key property under test is mutual exclusion of the two terminal states: a burn can be
 * settled (Confirm paid it out) or released (Unlock refunded it), never both. Everything else
 * (digest binding, group-signature authorization, ledger matching) exists to make that safe.
 */

const unlockTestSymbol = "usdt"

type unlockFixture struct {
	r        *rgbx
	state    db.DB
	priv     *btcec.PrivateKey
	burnHash []byte
	owner    string
	amount   int64
	msg      *rtypes.UnlockAsset
}

func signUnlockMsg(t *testing.T, priv *btcec.PrivateKey, msg *rtypes.UnlockAsset) []byte {
	t.Helper()
	return ecdsa.Sign(priv, rtypes.UnlockSignDigest(msg)).Serialize()
}

// cloneUnlock returns a message clone with a fresh signature.
func cloneUnlock(t *testing.T, priv *btcec.PrivateKey, msg *rtypes.UnlockAsset) *rtypes.UnlockAsset {
	t.Helper()
	out := proto.Clone(msg).(*rtypes.UnlockAsset)
	out.UnlockSig = signUnlockMsg(t, priv, out)
	return out
}

// newUnlockFixture builds a locked-but-unsettled withdrawal: payload, immutable burn ledger
// record, funded lock address, and a correctly signed Unlock message.
func newUnlockFixture(t *testing.T) *unlockFixture {
	t.Helper()
	dir, state, _ := util.CreateTestDB()
	t.Cleanup(func() { util.CloseTestDB(dir, state) })

	api := &mocks.QueueProtocolAPI{}
	api.On("GetConfig").Return(types.NewChain33Config(types.GetDefaultCfgstring()))
	r := newRgbx().(*rgbx)
	r.SetAPI(api)
	r.SetStateDB(state)

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	// The group public key is read from CrossChainInfo, so register it for this symbol.
	setCrossChainInfoForTest(t, state, unlockTestSymbol, rtypes.CrossChainAssetRgb20, 6,
		priv.PubKey().SerializeCompressed())

	owner, _ := util.Genaddress()
	f := &unlockFixture{
		r:        r,
		state:    state,
		priv:     priv,
		burnHash: []byte("unlock-burn-tx"),
		owner:    owner,
		amount:   100,
	}
	require.NoError(t, state.Set(formatPayloadKey(f.burnHash), types.Encode(&rtypes.WithdrawAsset{
		AssetSymbol: unlockTestSymbol,
		Amount:      f.amount,
	})))
	symbol := ensureCrossChainSymbol(unlockTestSymbol)
	opID, err := rtypes.BurnOperationID(symbol, f.burnHash)
	require.NoError(t, err)
	kvs, err := r.recordBurnOperation(opID, symbol, owner, f.amount, f.burnHash)
	require.NoError(t, err)
	applyStateKV(t, state, kvs)

	acc, err := r.newAccount(symbol)
	require.NoError(t, err)
	_, err = acc.Mint(r.crossChainLockAddress(acc), f.amount)
	require.NoError(t, err)

	f.msg = &rtypes.UnlockAsset{
		BurnTxHash:  f.burnHash,
		AssetSymbol: unlockTestSymbol,
		Amount:      f.amount,
		Owner:       owner,
	}
	f.msg.UnlockSig = signUnlockMsg(t, priv, f.msg)
	return f
}

// Test_rgbx_unlockSignDigest the digest ignores unlockSig (otherwise it could not be verified)
// and commits to every field that decides what the refund does.
func Test_rgbx_unlockSignDigest(t *testing.T) {
	base := func() *rtypes.UnlockAsset {
		return &rtypes.UnlockAsset{
			BurnTxHash:  []byte("burn"),
			AssetSymbol: "usdt",
			Amount:      10,
			Owner:       "owner",
		}
	}
	digest := rtypes.UnlockSignDigest(base())
	require.Len(t, digest, sha256.Size)

	withSig := base()
	withSig.UnlockSig = []byte("any-signature")
	require.Equal(t, digest, rtypes.UnlockSignDigest(withSig), "unlockSig must not be part of the digest")

	for name, mutate := range map[string]func(*rtypes.UnlockAsset){
		"burnTxHash":  func(m *rtypes.UnlockAsset) { m.BurnTxHash = []byte("other-burn") },
		"assetSymbol": func(m *rtypes.UnlockAsset) { m.AssetSymbol = "other" },
		"amount":      func(m *rtypes.UnlockAsset) { m.Amount++ },
		"owner":       func(m *rtypes.UnlockAsset) { m.Owner = "other-owner" },
	} {
		t.Run(name, func(t *testing.T) {
			m := base()
			mutate(m)
			require.NotEqual(t, digest, rtypes.UnlockSignDigest(m))
		})
	}
}

func Test_rgbx_verifyUnlockSig(t *testing.T) {
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pub := priv.PubKey().SerializeCompressed()

	msg := &rtypes.UnlockAsset{
		BurnTxHash:  []byte("burn"),
		AssetSymbol: unlockTestSymbol,
		Amount:      10,
		Owner:       "owner",
	}
	msg.UnlockSig = signUnlockMsg(t, priv, msg)
	require.NoError(t, verifyUnlockSig(pub, msg))

	// A different group key must not authorize this message.
	otherPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	require.ErrorIs(t, verifyUnlockSig(otherPriv.PubKey().SerializeCompressed(), msg), ErrInvalidThresholdSig)

	// Changing any signed field invalidates the signature.
	tampered := proto.Clone(msg).(*rtypes.UnlockAsset)
	tampered.Amount++
	require.ErrorIs(t, verifyUnlockSig(pub, tampered), ErrInvalidThresholdSig)

	// Missing signature / public key are rejected.
	require.ErrorIs(t, verifyUnlockSig(pub, &rtypes.UnlockAsset{}), ErrInvalidThresholdSig)
	require.ErrorIs(t, verifyUnlockSig(nil, msg), ErrInvalidThresholdSig)
}

func Test_rgbx_checkUnlock(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		f := newUnlockFixture(t)
		require.NoError(t, f.r.checkUnlock("tx", f.msg))
	})

	t.Run("settled burn rejected", func(t *testing.T) {
		f := newUnlockFixture(t)
		require.NoError(t, f.state.Set(formatWithdrawFinalKey(f.burnHash), withdrawFinalSettled))
		require.ErrorIs(t, f.r.checkUnlock("tx", f.msg), ErrWithdrawAlreadyFinal)
	})

	t.Run("released burn rejected", func(t *testing.T) {
		f := newUnlockFixture(t)
		require.NoError(t, f.state.Set(formatWithdrawFinalKey(f.burnHash), withdrawFinalReleased))
		require.ErrorIs(t, f.r.checkUnlock("tx", f.msg), ErrWithdrawAlreadyFinal)
	})

	t.Run("owner mismatch rejected", func(t *testing.T) {
		f := newUnlockFixture(t)
		msg := cloneUnlock(t, f.priv, f.msg)
		msg.Owner = "someone-else"
		msg.UnlockSig = signUnlockMsg(t, f.priv, msg)
		require.ErrorIs(t, f.r.checkUnlock("tx", msg), ErrOperationOwnerMismatch)
	})

	t.Run("amount mismatch rejected", func(t *testing.T) {
		f := newUnlockFixture(t)
		msg := cloneUnlock(t, f.priv, f.msg)
		msg.Amount++
		msg.UnlockSig = signUnlockMsg(t, f.priv, msg)
		require.ErrorIs(t, f.r.checkUnlock("tx", msg), ErrOperationAmountMismatch)
	})

	t.Run("symbol case must match exactly", func(t *testing.T) {
		f := newUnlockFixture(t)
		msg := cloneUnlock(t, f.priv, f.msg)
		msg.AssetSymbol = formatSymbol(f.msg.AssetSymbol)
		msg.UnlockSig = signUnlockMsg(t, f.priv, msg)
		require.ErrorIs(t, f.r.checkUnlock("tx", msg), ErrOperationMismatch)
	})

	t.Run("unknown burn rejected", func(t *testing.T) {
		f := newUnlockFixture(t)
		msg := cloneUnlock(t, f.priv, f.msg)
		msg.BurnTxHash = []byte("never-locked")
		msg.UnlockSig = signUnlockMsg(t, f.priv, msg)
		require.ErrorIs(t, f.r.checkUnlock("tx", msg), ErrWithdrawNotExist)
	})

	t.Run("non-burn ledger record rejected", func(t *testing.T) {
		f := newUnlockFixture(t)
		symbol := ensureCrossChainSymbol(unlockTestSymbol)
		opID, err := rtypes.BurnOperationID(symbol, f.burnHash)
		require.NoError(t, err)
		require.NoError(t, f.state.Set(formatOperationRecordKey(opID), types.Encode(&rtypes.OperationRecord{
			OperationId:   opID,
			Kind:          rtypes.OperationKindMint,
			Symbol:        formatSymbol(symbol),
			Amount:        f.amount,
			Owner:         f.owner,
			Chain33TxHash: f.burnHash,
		})))
		require.ErrorIs(t, f.r.checkUnlock("tx", f.msg), ErrOperationMismatch)
	})

	t.Run("insufficient locked balance rejected", func(t *testing.T) {
		f := newUnlockFixture(t)
		symbol := ensureCrossChainSymbol(unlockTestSymbol)
		acc, err := f.r.newAccount(symbol)
		require.NoError(t, err)
		_, err = acc.Burn(f.r.crossChainLockAddress(acc), f.amount-1)
		require.NoError(t, err)
		require.ErrorIs(t, f.r.checkUnlock("tx", f.msg), types.ErrInsufficientBalance)
	})

	t.Run("empty burn hash rejected", func(t *testing.T) {
		f := newUnlockFixture(t)
		require.ErrorIs(t, f.r.checkUnlock("tx", &rtypes.UnlockAsset{}), ErrInvalidUnlockRequest)
	})

	t.Run("final key read error fails closed", func(t *testing.T) {
		f := newUnlockFixture(t)
		// Only the terminal-state key fails; every earlier check goes through the real DB.
		f.r.SetStateDB(&e14DedupReadErrKV{
			KV:      f.state,
			failKey: formatWithdrawFinalKey(f.burnHash),
			err:     errors.New("storage unavailable"),
		})
		err := f.r.checkUnlock("tx", f.msg)
		require.Error(t, err, "a terminal-key read failure must reject, not be treated as not-final")
		require.False(t, errors.Is(err, types.ErrNotFound))
	})

	t.Run("empty signature rejected", func(t *testing.T) {
		f := newUnlockFixture(t)
		msg := proto.Clone(f.msg).(*rtypes.UnlockAsset)
		msg.UnlockSig = nil
		require.ErrorIs(t, f.r.checkUnlock("tx", msg), ErrInvalidThresholdSig)
	})

	t.Run("signature by a different group key rejected", func(t *testing.T) {
		f := newUnlockFixture(t)
		other, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		msg := &rtypes.UnlockAsset{
			BurnTxHash:  f.burnHash,
			AssetSymbol: unlockTestSymbol,
			Amount:      f.amount,
			Owner:       f.owner,
		}
		msg.UnlockSig = signUnlockMsg(t, other, msg)
		require.ErrorIs(t, f.r.checkUnlock("tx", msg), ErrInvalidThresholdSig)
	})

	t.Run("missing cross chain info rejected", func(t *testing.T) {
		f := newUnlockFixture(t)
		require.NoError(t, f.state.Delete(formatCrossChainInfoKey(unlockTestSymbol)))
		require.ErrorIs(t, f.r.checkUnlock("tx", f.msg), ErrGetCrossChainInfo)
	})

	t.Run("missing ledger record rejected", func(t *testing.T) {
		f := newUnlockFixture(t)
		symbol := ensureCrossChainSymbol(unlockTestSymbol)
		opID, err := rtypes.BurnOperationID(symbol, f.burnHash)
		require.NoError(t, err)
		require.NoError(t, f.state.Delete(formatOperationRecordKey(opID)))
		require.ErrorIs(t, f.r.checkUnlock("tx", f.msg), ErrOperationNotExist)
	})
}

func Test_rgbx_CheckTx_unlockDispatch(t *testing.T) {
	f := newUnlockFixture(t)
	action := &rtypes.RgbxAction{
		Ty:    rtypes.TyUnlockAsset,
		Value: &rtypes.RgbxAction_Unlock{Unlock: f.msg},
	}
	tx := &types.Transaction{Payload: types.Encode(action)}
	tx.Sign(types.SECP256K1, testPriv)

	// The submitter is intentionally arbitrary: authorization is the group signature only.
	require.NoError(t, f.r.CheckTx(tx, 0))
}

func Test_rgbx_Exec_Unlock(t *testing.T) {
	f := newUnlockFixture(t)
	symbol := ensureCrossChainSymbol(unlockTestSymbol)
	acc, err := f.r.newAccount(symbol)
	require.NoError(t, err)
	lockAddr := f.r.crossChainLockAddress(acc)
	require.Equal(t, f.amount, acc.LoadAccount(lockAddr).GetBalance())
	require.Equal(t, int64(0), acc.LoadAccount(f.owner).GetBalance())

	receipt, err := f.r.Exec_Unlock(f.msg, &types.Transaction{}, 0)
	require.NoError(t, err)
	// The framework commits receipt KV; the test applies it explicitly (account DB also writes
	// its own keys through, so this is the terminal-state key that matters here).
	applyStateKV(t, f.state, receipt.KV)

	// Tokens move from the shared lock address back to the original owner.
	require.Equal(t, int64(0), acc.LoadAccount(lockAddr).GetBalance())
	require.Equal(t, f.amount, acc.LoadAccount(f.owner).GetBalance())

	// Terminal state is released, and the operation is reported as not settled.
	value, err := f.state.Get(formatWithdrawFinalKey(f.burnHash))
	require.NoError(t, err)
	require.Equal(t, withdrawFinalReleased, value)
	require.False(t, f.r.isOperationSettled(&rtypes.OperationRecord{
		Kind:          rtypes.OperationKindBurn,
		Chain33TxHash: f.burnHash,
	}))

	// The receipt log identifies the burn, the recipient, and the amount.
	var logged *rtypes.UnlockAsset
	for _, log := range receipt.Logs {
		if log.Ty != rtypes.TyUnlockAssetLog {
			continue
		}
		logged = &rtypes.UnlockAsset{}
		require.NoError(t, types.Decode(log.Log, logged))
	}
	require.NotNil(t, logged, "unlock receipt must carry a TyUnlockAssetLog")
	require.Equal(t, f.burnHash, logged.GetBurnTxHash())
	require.Equal(t, f.owner, logged.GetOwner())
	require.Equal(t, f.amount, logged.GetAmount())
	require.Empty(t, logged.GetUnlockSig(), "the receipt log must not repeat the signature")

	// A burn has exactly one terminal state: repeating the refund is rejected.
	_, err = f.r.Exec_Unlock(f.msg, &types.Transaction{}, 0)
	require.ErrorIs(t, err, ErrWithdrawAlreadyFinal)

	// Burned supply is untouched: the burn was never settled, so minted - burned is unchanged.
	supply, err := f.r.loadOperationSupply(symbol)
	require.NoError(t, err)
	require.Equal(t, int64(0), supply.GetBurned())
}

// Test_rgbx_Exec_unlockDispatch exercises the framework dispatch path: action name "Unlock"
// must resolve to Exec_Unlock and the payload must round-trip through CreateTransaction.
func Test_rgbx_Exec_unlockDispatch(t *testing.T) {
	f := newUnlockFixture(t)
	tx, err := f.r.GetExecutorType().CreateTransaction(rtypes.NameUnlockAssetAction, f.msg)
	require.NoError(t, err)
	tx.Sign(types.SECP256K1, testPriv)

	receipt, err := f.r.Exec(tx, 0)
	require.NoError(t, err)
	applyStateKV(t, f.state, receipt.KV)

	symbol := ensureCrossChainSymbol(unlockTestSymbol)
	acc, err := f.r.newAccount(symbol)
	require.NoError(t, err)
	require.Equal(t, int64(0), acc.LoadAccount(f.r.crossChainLockAddress(acc)).GetBalance())
	require.Equal(t, f.amount, acc.LoadAccount(f.owner).GetBalance())
}

// Test_rgbx_releasedBurnCannotBeSettled is the double-payment guard: after Unlock refunds a
// burn, Confirm must not be able to pay it out as well.
func Test_rgbx_releasedBurnCannotBeSettled(t *testing.T) {
	f := newUnlockFixture(t)
	receipt, err := f.r.Exec_Unlock(f.msg, &types.Transaction{}, 0)
	require.NoError(t, err)
	applyStateKV(t, f.state, receipt.KV)

	err = f.r.checkWithdrawConfirm("tx", "confirm", &rtypes.ConfirmTx{TxHash: f.burnHash},
		&rtypes.PendingTx{AssetSymbol: unlockTestSymbol})
	require.ErrorIs(t, err, ErrWithdrawAlreadyConfirmed)
}

// Test_rgbx_Query_GetWithdrawFinalState is the read-only view the E2E uses to assert that the
// refund reached the chain. It must report nothing before a terminal state, then the exact
// stored value ("settled" or "released"), and reject malformed hash input.
func Test_rgbx_Query_GetWithdrawFinalState(t *testing.T) {
	f := newUnlockFixture(t)
	req := &types.ReqString{Data: hex.EncodeToString(f.burnHash)}

	reply, err := f.r.Query_GetWithdrawFinalState(req)
	require.NoError(t, err)
	require.Empty(t, reply.(*types.ReplyString).GetData(), "a locked burn has no terminal state yet")

	require.NoError(t, f.state.Set(formatWithdrawFinalKey(f.burnHash), withdrawFinalReleased))
	reply, err = f.r.Query_GetWithdrawFinalState(req)
	require.NoError(t, err)
	require.Equal(t, "released", reply.(*types.ReplyString).GetData())

	// The CLI renders hashes with a 0x prefix; the query tolerates it.
	reply, err = f.r.Query_GetWithdrawFinalState(&types.ReqString{Data: "0x" + req.GetData()})
	require.NoError(t, err)
	require.Equal(t, "released", reply.(*types.ReplyString).GetData())

	require.NoError(t, f.state.Set(formatWithdrawFinalKey(f.burnHash), withdrawFinalSettled))
	reply, err = f.r.Query_GetWithdrawFinalState(req)
	require.NoError(t, err)
	require.Equal(t, "settled", reply.(*types.ReplyString).GetData())

	for _, bad := range []string{"", "0x", "not-hex"} {
		_, err = f.r.Query_GetWithdrawFinalState(&types.ReqString{Data: bad})
		require.ErrorIs(t, err, types.ErrInvalidParam, "input %q is not a burn tx hash", bad)
	}
}

// Test_rgbx_Query_GetCrossChainLockAddress returns the same deterministic address the executor
// uses for locking and refunding, so the E2E can observe the locked balance.
func Test_rgbx_Query_GetCrossChainLockAddress(t *testing.T) {
	f := newUnlockFixture(t)
	reply, err := f.r.Query_GetCrossChainLockAddress(&types.ReqString{Data: unlockTestSymbol})
	require.NoError(t, err)

	symbol := ensureCrossChainSymbol(unlockTestSymbol)
	acc, err := f.r.newAccount(symbol)
	require.NoError(t, err)
	require.Equal(t, f.r.crossChainLockAddress(acc), reply.(*types.ReplyString).GetData())
	require.NotEmpty(t, reply.(*types.ReplyString).GetData())

	_, err = f.r.Query_GetCrossChainLockAddress(&types.ReqString{})
	require.ErrorIs(t, err, types.ErrInvalidParam)
}

// Test_rgbx_isOperationSettled_terminalStates settled reports true, released reports false.
func Test_rgbx_isOperationSettled_terminalStates(t *testing.T) {
	f := newUnlockFixture(t)
	rec := &rtypes.OperationRecord{Kind: rtypes.OperationKindBurn, Chain33TxHash: f.burnHash}

	require.False(t, f.r.isOperationSettled(rec), "no terminal key yet")

	require.NoError(t, f.state.Set(formatWithdrawFinalKey(f.burnHash), withdrawFinalSettled))
	require.True(t, f.r.isOperationSettled(rec))

	require.NoError(t, f.state.Set(formatWithdrawFinalKey(f.burnHash), withdrawFinalReleased))
	require.False(t, f.r.isOperationSettled(rec), "released means the burn was refunded, never settled")
}
