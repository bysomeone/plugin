package neutrino

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/walletdb"
)

// signedWithdrawTxBucket holds the final raw transaction (witness included) of signed
// withdrawals, keyed by the chain33 burn tx hash. The record exists only to drive the
// periodic rebroadcast of signed-but-unconfirmed withdrawals; it is removed once the
// burn is confirmed.
//
// It is deliberately separate from signedPaymentBucket: that one is the permanent
// "this node signed a payment for this burn, never refund it" marker and must never be
// deleted. Dropping a replay record here does not weaken that invariant.
var signedWithdrawTxBucket = []byte("rgbx-signed-withdraw-tx")

// signedTxReplayInterval is how often signed-but-unconfirmed withdrawals are pushed to
// the bitcoin network again. A transaction signed by the bridge is safe to rebroadcast
// verbatim: the bytes never change, so a replay can only be accepted once.
const signedTxReplayInterval = 5 * time.Minute

// signedWithdrawTxStore is the persistence surface for replayable signed withdrawals.
// Production uses walletdb; tests inject an in-memory or failing implementation.
type signedWithdrawTxStore interface {
	// PutOnce is idempotent: an existing record for the burn is left untouched.
	PutOnce(burnTxHash, rawTx []byte) error
	// Get returns nil when there is no record.
	Get(burnTxHash []byte) ([]byte, error)
	// All returns every record, keyed by burn tx hash.
	All() (map[string][]byte, error)
	Delete(burnTxHash []byte) error
}

type walletSignedWithdrawTxStore struct {
	db walletdb.DB
}

func (s *walletSignedWithdrawTxStore) PutOnce(burnTxHash, rawTx []byte) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("signed withdraw tx store is not initialized")
	}
	return walletdb.Update(s.db, func(tx walletdb.ReadWriteTx) error {
		bucket, err := tx.CreateTopLevelBucket(signedWithdrawTxBucket)
		if err != nil {
			return err
		}
		if bucket.Get(burnTxHash) != nil {
			return nil
		}
		value := append([]byte(nil), rawTx...)
		return bucket.Put(burnTxHash, value)
	})
}

func (s *walletSignedWithdrawTxStore) Get(burnTxHash []byte) ([]byte, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("signed withdraw tx store is not initialized")
	}
	var raw []byte
	err := walletdb.View(s.db, func(tx walletdb.ReadTx) error {
		bucket := tx.ReadBucket(signedWithdrawTxBucket)
		if bucket == nil {
			return nil
		}
		if value := bucket.Get(burnTxHash); value != nil {
			raw = append([]byte(nil), value...)
		}
		return nil
	})
	return raw, err
}

func (s *walletSignedWithdrawTxStore) All() (map[string][]byte, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("signed withdraw tx store is not initialized")
	}
	records := make(map[string][]byte)
	err := walletdb.View(s.db, func(tx walletdb.ReadTx) error {
		bucket := tx.ReadBucket(signedWithdrawTxBucket)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(k, v []byte) error {
			records[string(k)] = append([]byte(nil), v...)
			return nil
		})
	})
	return records, err
}

func (s *walletSignedWithdrawTxStore) Delete(burnTxHash []byte) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("signed withdraw tx store is not initialized")
	}
	return walletdb.Update(s.db, func(tx walletdb.ReadWriteTx) error {
		bucket := tx.ReadWriteBucket(signedWithdrawTxBucket)
		if bucket == nil {
			return nil
		}
		return bucket.Delete(burnTxHash)
	})
}

// signedWithdrawTxStorage returns the store used by this node (tests may inject one).
func (n *neutrinoClient) signedWithdrawTxStorage() signedWithdrawTxStore {
	if n.signedTxs != nil {
		return n.signedTxs
	}
	return &walletSignedWithdrawTxStore{db: n.neutrinoCfg.Database}
}

// persistSignedWithdrawTx writes the final transaction bytes once per burn hash.
func (n *neutrinoClient) persistSignedWithdrawTx(burnTxHash []byte, tx *wire.MsgTx) error {
	if n == nil {
		return fmt.Errorf("signed withdraw tx: no bridge to persist the transaction")
	}
	if len(burnTxHash) == 0 {
		return fmt.Errorf("signed withdraw tx: empty burn tx hash")
	}
	if tx == nil {
		return fmt.Errorf("signed withdraw tx: nil transaction")
	}
	var buf bytes.Buffer
	buf.Grow(tx.SerializeSize())
	if err := tx.Serialize(&buf); err != nil {
		return fmt.Errorf("signed withdraw tx: serialize %s: %w", tx.TxHash(), err)
	}
	if err := n.signedWithdrawTxStorage().PutOnce(burnTxHash, buf.Bytes()); err != nil {
		return fmt.Errorf("signed withdraw tx: persist %s: %w", hex.EncodeToString(burnTxHash), err)
	}
	return nil
}

// persistSignedWithdrawTxBestEffort is called only after a successful broadcast. A failed
// write must not turn that broadcast into an error, but it leaves the node without a replay
// record, so it is logged at ERROR level.
func (n *neutrinoClient) persistSignedWithdrawTxBestEffort(burnTxHash []byte, tx *wire.MsgTx) {
	if err := n.persistSignedWithdrawTx(burnTxHash, tx); err != nil {
		btcTxid := ""
		if tx != nil {
			btcTxid = tx.TxHash().String()
		}
		log.Error("persistSignedWithdrawTx failed after broadcast: the signed tx will not be replayed if it is dropped",
			"burnTxHash", hex.EncodeToString(burnTxHash), "btcTxid", btcTxid, "err", err)
	}
}

// dropSignedWithdrawTx removes the replay record after the burn is confirmed. The
// signed-payment marker in signedPaymentBucket is intentionally left untouched.
func (n *neutrinoClient) dropSignedWithdrawTx(burnTxHash []byte) {
	if n == nil || len(burnTxHash) == 0 {
		return
	}
	if err := n.signedWithdrawTxStorage().Delete(burnTxHash); err != nil {
		log.Error("dropSignedWithdrawTx", "burnTxHash", hex.EncodeToString(burnTxHash), "err", err)
	}
}

// signedTxReplayEligible reports whether a burn's local state still allows rebroadcasting.
// Empty state means the broadcast happened but the state write did not, so the signed tx is
// still replayable. Every other non-"broadcasted" state is terminal for replay purposes
// (confirmed, unrecoverable, unlock-*, abandoning).
func signedTxReplayEligible(state []byte) bool {
	if len(state) == 0 {
		return true
	}
	return bytes.Equal(state, withdrawStatusSent)
}

// broadcastErrorIsAlreadyKnown reports errors that mean "the node already has this tx"
// (mempool or chain). Those are the expected outcome of a periodic rebroadcast.
func broadcastErrorIsAlreadyKnown(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already") || strings.Contains(msg, "txn-already-known")
}

// pendingWithdrawByBurnHash finds the in-memory pending entry for a burn. The pending map
// is owned by the monitorTransactions goroutine, which also runs the replay, so no lock is
// needed here.
func (b *btcWallet) pendingWithdrawByBurnHash(burnTxHash []byte) *btcPendingTx {
	for _, pending := range b.pendingTxs {
		if pending == nil || pending.tx == nil {
			continue
		}
		if pending.txType == transactionTypeWithdraw && bytes.Equal(pending.chain33WithdrawTxHash, burnTxHash) {
			return pending
		}
	}
	return nil
}

// signedWithdrawTxForReplay prefers the in-memory pending transaction (it is the one the
// wallet is already tracking) and falls back to the persisted bytes after a restart.
func signedWithdrawTxForReplay(pending *btcPendingTx, raw []byte) (*wire.MsgTx, error) {
	if pending != nil && pending.tx != nil {
		return pending.tx, nil
	}
	var tx wire.MsgTx
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		return nil, fmt.Errorf("decode persisted signed withdraw tx: %w", err)
	}
	return &tx, nil
}

// replaySignedWithdrawTxs rebroadcasts every signed withdrawal that has not been confirmed.
// It is driven by persisted records, so it also works after a process restart when the
// in-memory pending map is empty. The transaction bytes are never modified.
func (b *btcWallet) replaySignedWithdrawTxs() {
	if b == nil || b.client == nil {
		return
	}
	if b.client.signedTxs == nil && b.client.neutrinoCfg.Database == nil {
		return
	}
	records, err := b.client.signedWithdrawTxStorage().All()
	if err != nil {
		log.Error("replaySignedWithdrawTxs read records", "err", err)
		return
	}
	for burnKey, raw := range records {
		burnTxHash := []byte(burnKey)
		state := b.client.getWithdrawState(burnTxHash)
		if !signedTxReplayEligible(state) {
			// Confirmation is the only state after which the replay record has no
			// purpose. Other terminal states keep the record for observability.
			if bytes.Equal(state, withdrawStatusConfirmed) {
				b.client.dropSignedWithdrawTx(burnTxHash)
			}
			continue
		}

		pending := b.pendingWithdrawByBurnHash(burnTxHash)
		if pending != nil && (pending.blockHeight > 0 || pending.confirmations > 0) {
			// The tx is already in a block: do not replay. Keep the record until the burn
			// reaches the locally confirmed state, so the exact bytes remain available
			// through the confirmation window (including a shallow reorg).
			log.Debug("replaySignedWithdrawTxs tx already confirmed by height, skip",
				"burnTxHash", burnKey, "btcTxid", pending.txHash.String())
			continue
		}

		tx, err := signedWithdrawTxForReplay(pending, raw)
		if err != nil {
			log.Error("replaySignedWithdrawTxs decode record", "burnTxHash", burnKey, "err", err)
			continue
		}
		btcTxid := tx.TxHash()
		err = b.sendRawTransaction(tx)
		switch {
		case err == nil:
			log.Debug("replaySignedWithdrawTxs rebroadcast", "burnTxHash", burnKey, "btcTxid", btcTxid.String())
		case broadcastErrorIsAlreadyKnown(err):
			log.Debug("replaySignedWithdrawTxs already known to the node",
				"burnTxHash", burnKey, "btcTxid", btcTxid.String(), "err", err)
		default:
			log.Warn("replaySignedWithdrawTxs rebroadcast failed",
				"burnTxHash", burnKey, "btcTxid", btcTxid.String(), "err", err)
		}
	}
}

// decodeSignedWithdrawTx is a small helper used by tests and diagnostics.
func decodeSignedWithdrawTx(raw []byte) (*wire.MsgTx, error) {
	var tx wire.MsgTx
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		return nil, err
	}
	return &tx, nil
}
