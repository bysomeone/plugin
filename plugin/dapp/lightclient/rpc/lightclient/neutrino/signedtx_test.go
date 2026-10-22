package neutrino

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/neutrino"
	"github.com/stretchr/testify/require"
)

// memSignedWithdrawTxStore is an in-memory signedWithdrawTxStore with fault injection.
type memSignedWithdrawTxStore struct {
	txs    map[string][]byte
	putErr error
	getErr error
	allErr error
	delErr error
}

func (m *memSignedWithdrawTxStore) PutOnce(burnTxHash, rawTx []byte) error {
	if m.putErr != nil {
		return m.putErr
	}
	if m.txs == nil {
		m.txs = make(map[string][]byte)
	}
	key := string(burnTxHash)
	if _, ok := m.txs[key]; ok {
		return nil
	}
	m.txs[key] = append([]byte(nil), rawTx...)
	return nil
}

func (m *memSignedWithdrawTxStore) Get(burnTxHash []byte) ([]byte, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	raw := m.txs[string(burnTxHash)]
	if raw == nil {
		return nil, nil
	}
	return append([]byte(nil), raw...), nil
}

func (m *memSignedWithdrawTxStore) All() (map[string][]byte, error) {
	if m.allErr != nil {
		return nil, m.allErr
	}
	out := make(map[string][]byte, len(m.txs))
	for k, v := range m.txs {
		out[k] = append([]byte(nil), v...)
	}
	return out, nil
}

func (m *memSignedWithdrawTxStore) Delete(burnTxHash []byte) error {
	if m.delErr != nil {
		return m.delErr
	}
	delete(m.txs, string(burnTxHash))
	return nil
}

// testSignedWithdrawTx builds a witness-bearing withdrawal-shaped transaction.
func testSignedWithdrawTx(nonce byte) *wire.MsgTx {
	tx := wire.NewMsgTx(wire.TxVersion)
	txIn := wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{nonce}, Index: uint32(nonce)}, nil, nil)
	txIn.Sequence = withdrawInputSequence
	txIn.Witness = wire.TxWitness{{0x30, nonce}, {0x02, nonce}}
	tx.AddTxIn(txIn)
	pkScript := append([]byte{txscript.OP_0, 0x14}, bytes.Repeat([]byte{nonce}, 20)...)
	tx.AddTxOut(wire.NewTxOut(int64(10_000+int(nonce)), pkScript))
	return tx
}

func mustSerializeTx(t *testing.T, tx *wire.MsgTx) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, tx.Serialize(&buf))
	return buf.Bytes()
}

// TestBuildWithdrawTx_inputsSignalRBF verifies every withdrawal input is marked opt-in RBF
// (0xfffffffd) and locktime stays zero.
func TestBuildWithdrawTx_inputsSignalRBF(t *testing.T) {
	params := chaincfg.RegressionNetParams
	poolScript := append([]byte{txscript.OP_0, 0x14}, bytes.Repeat([]byte{0x11}, 20)...)
	b := &btcWallet{
		chainParams: params,
		tssPkScript: poolScript,
		client:      &neutrinoClient{deposits: newDepositScriptSet()},
		selectInputsFn: func(target, feeRate btcutil.Amount, sticky *UTXO) ([]*UTXO, btcutil.Amount, error) {
			return []*UTXO{
				{OutPoint: wire.OutPoint{Hash: chainhash.Hash{1}, Index: 0}, Amount: 100_000, PkScript: poolScript},
				{OutPoint: wire.OutPoint{Hash: chainhash.Hash{2}, Index: 1}, Amount: 50_000, PkScript: poolScript},
			}, 150_000, nil
		},
	}

	tx, _, locked, err := b.buildWithdrawTx(&withdrawRequest{
		chain33WithdrawHash: bytes.Repeat([]byte{0x07}, 32),
		amount:              100_000,
		feeRate:             1,
		toAddress:           "bcrt1qw508d6qejxtdg4y5r3zarvary0c5xw7kygt080",
	})
	require.NoError(t, err)
	require.Len(t, locked, 2)
	require.Len(t, tx.TxIn, 2)
	for i, txIn := range tx.TxIn {
		require.Equal(t, uint32(wire.MaxTxInSequenceNum-2), txIn.Sequence, "input %d must signal RBF", i)
	}
	require.Zero(t, tx.LockTime, "RBF must not introduce a locktime")
}

// TestSignedWithdrawTxStore_persistsAcrossRestart verifies the final witness-bearing bytes
// survive a database close/reopen and that PutOnce keeps the first record.
func TestSignedWithdrawTxStore_persistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	_, db, err := openWalletDB(dir, "neutrino.db")
	require.NoError(t, err)

	burn := []byte("burn-restart")
	tx := testSignedWithdrawTx(1)
	wantRaw := mustSerializeTx(t, tx)

	n := &neutrinoClient{neutrinoCfg: neutrino.Config{Database: db}}
	require.NoError(t, n.persistSignedWithdrawTx(burn, tx))
	require.NoError(t, db.Close())

	// Restart: reopen the same database on disk and read the record back.
	exists, reopened, err := openWalletDB(dir, "neutrino.db")
	require.NoError(t, err)
	require.True(t, exists)
	defer func() { _ = reopened.Close() }()

	n2 := &neutrinoClient{neutrinoCfg: neutrino.Config{Database: reopened}}
	records, err := n2.signedWithdrawTxStorage().All()
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, wantRaw, records[string(burn)], "the exact broadcast bytes must be persisted")

	decoded, err := decodeSignedWithdrawTx(records[string(burn)])
	require.NoError(t, err)
	require.Equal(t, tx.TxHash(), decoded.TxHash())
	require.Len(t, decoded.TxIn[0].Witness, 2, "witness must round-trip")

	// PutOnce is idempotent: a retry must not replace the already recorded transaction.
	require.NoError(t, n2.persistSignedWithdrawTx(burn, testSignedWithdrawTx(2)))
	raw, err := n2.signedWithdrawTxStorage().Get(burn)
	require.NoError(t, err)
	require.Equal(t, wantRaw, raw)
}

// TestReplaySignedWithdrawTxs_afterRestart verifies replay is driven by the persisted
// record when the in-memory pending map is empty, and never alters the transaction.
func TestReplaySignedWithdrawTxs_afterRestart(t *testing.T) {
	store := &memSignedWithdrawTxStore{}
	n := &neutrinoClient{signedTxs: store}
	b := &btcWallet{client: n, pendingTxs: make(map[chainhash.Hash]*btcPendingTx)}

	burn := []byte("burn-replay")
	tx := testSignedWithdrawTx(3)
	require.NoError(t, n.persistSignedWithdrawTx(burn, tx))

	var sent []*wire.MsgTx
	b.sendRawTxFn = func(tx *wire.MsgTx) error {
		sent = append(sent, tx)
		return nil
	}
	b.replaySignedWithdrawTxs()

	require.Len(t, sent, 1)
	require.Equal(t, tx.TxHash(), sent[0].TxHash())
	require.Equal(t, mustSerializeTx(t, tx), mustSerializeTx(t, sent[0]), "replay must not change the transaction")
	raw, err := store.Get(burn)
	require.NoError(t, err)
	require.NotNil(t, raw, "an unconfirmed record stays for the next tick")
}

// TestReplaySignedWithdrawTxs_prefersMemoryPending verifies the in-memory pending tx wins
// over the persisted copy when both describe the same burn.
func TestReplaySignedWithdrawTxs_prefersMemoryPending(t *testing.T) {
	store := &memSignedWithdrawTxStore{}
	n := &neutrinoClient{signedTxs: store}
	burn := []byte("burn-prefer-memory")
	persisted := testSignedWithdrawTx(4)
	pendingTx := testSignedWithdrawTx(5)

	b := &btcWallet{
		client: n,
		pendingTxs: map[chainhash.Hash]*btcPendingTx{
			pendingTx.TxHash(): {
				tx:                    pendingTx,
				txType:                transactionTypeWithdraw,
				chain33WithdrawTxHash: burn,
				blockHeight:           -1,
			},
		},
	}
	require.NoError(t, n.persistSignedWithdrawTx(burn, persisted))

	var sent []*wire.MsgTx
	b.sendRawTxFn = func(tx *wire.MsgTx) error {
		sent = append(sent, tx)
		return nil
	}
	b.replaySignedWithdrawTxs()

	require.Len(t, sent, 1)
	require.Equal(t, pendingTx.TxHash(), sent[0].TxHash())
}

// TestReplaySignedWithdrawTxs_skipsConfirmed verifies confirmed withdrawals are not
// replayed and their replay record is removed.
func TestReplaySignedWithdrawTxs_skipsConfirmed(t *testing.T) {
	dir := t.TempDir()
	_, db, err := openWalletDB(dir, "neutrino.db")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	t.Run("local state confirmed", func(t *testing.T) {
		store := &memSignedWithdrawTxStore{}
		n := &neutrinoClient{signedTxs: store, neutrinoCfg: neutrino.Config{Database: db}}
		b := &btcWallet{client: n, pendingTxs: make(map[chainhash.Hash]*btcPendingTx)}

		burn := []byte("burn-confirmed-state")
		require.NoError(t, n.persistSignedWithdrawTx(burn, testSignedWithdrawTx(6)))
		require.NoError(t, n.setWithdrawState(burn, withdrawStatusConfirmed))

		sent := 0
		b.sendRawTxFn = func(*wire.MsgTx) error { sent++; return nil }
		b.replaySignedWithdrawTxs()

		require.Zero(t, sent, "a confirmed withdrawal must not be replayed")
		raw, err := store.Get(burn)
		require.NoError(t, err)
		require.Nil(t, raw, "the replay record is useless after confirmation")
	})

	t.Run("bitcoin height in pending", func(t *testing.T) {
		store := &memSignedWithdrawTxStore{}
		n := &neutrinoClient{signedTxs: store, neutrinoCfg: neutrino.Config{Database: db}}
		burn := []byte("burn-confirmed-height")
		tx := testSignedWithdrawTx(7)
		require.NoError(t, n.persistSignedWithdrawTx(burn, tx))
		b := &btcWallet{
			client: n,
			pendingTxs: map[chainhash.Hash]*btcPendingTx{
				tx.TxHash(): {
					tx:                    tx,
					txType:                transactionTypeWithdraw,
					chain33WithdrawTxHash: burn,
					blockHeight:           100,
				},
			},
		}
		sent := 0
		b.sendRawTxFn = func(*wire.MsgTx) error { sent++; return nil }
		b.replaySignedWithdrawTxs()

		require.Zero(t, sent)
		raw, err := store.Get(burn)
		require.NoError(t, err)
		require.NotNil(t, raw, "the record is kept until the burn reaches the confirmed state")
	})
}

// TestReplaySignedWithdrawTxs_skipsTerminalStates verifies abandoned, unlock and
// unrecoverable withdrawals are never replayed (and their records are kept).
func TestReplaySignedWithdrawTxs_skipsTerminalStates(t *testing.T) {
	dir := t.TempDir()
	_, db, err := openWalletDB(dir, "neutrino.db")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	states := map[string][]byte{
		"abandoning":    withdrawStatusAbandoning,
		"unlock-pend":   withdrawStatusUnlockPending,
		"unlock-sub":    withdrawStatusUnlockSubmitted,
		"unrecoverable": withdrawStatusUnrecoverable,
	}
	for name, state := range states {
		t.Run(name, func(t *testing.T) {
			store := &memSignedWithdrawTxStore{}
			n := &neutrinoClient{signedTxs: store, neutrinoCfg: neutrino.Config{Database: db}}
			b := &btcWallet{client: n, pendingTxs: make(map[chainhash.Hash]*btcPendingTx)}

			burn := []byte("burn-terminal-" + name)
			require.NoError(t, n.persistSignedWithdrawTx(burn, testSignedWithdrawTx(8)))
			require.NoError(t, n.setWithdrawState(burn, state))

			sent := 0
			b.sendRawTxFn = func(*wire.MsgTx) error { sent++; return nil }
			b.replaySignedWithdrawTxs()

			require.Zero(t, sent, "terminal/abandoned withdrawals must not be replayed")
			raw, err := store.Get(burn)
			require.NoError(t, err)
			require.NotNil(t, raw, "records for non-confirmed terminal states are kept for observability")
		})
	}
}

// TestReplaySignedWithdrawTxs_alreadyKnownIsNotAnError verifies "already known" errors are
// treated as the expected outcome of a replay: the record is kept and nothing breaks.
func TestReplaySignedWithdrawTxs_alreadyKnownIsNotAnError(t *testing.T) {
	for _, msg := range []string{
		"sendrawtransaction: txn-already-known",
		"-27: Transaction already in the mempool",
	} {
		t.Run(msg, func(t *testing.T) {
			store := &memSignedWithdrawTxStore{}
			n := &neutrinoClient{signedTxs: store}
			b := &btcWallet{client: n, pendingTxs: make(map[chainhash.Hash]*btcPendingTx)}

			burn := []byte("burn-already-known")
			require.NoError(t, n.persistSignedWithdrawTx(burn, testSignedWithdrawTx(9)))
			b.sendRawTxFn = func(*wire.MsgTx) error { return errors.New(msg) }

			require.NotPanics(t, b.replaySignedWithdrawTxs)
			raw, err := store.Get(burn)
			require.NoError(t, err)
			require.NotNil(t, raw, "the record must survive a benign already-known reply")
		})
	}
}

// TestReplaySignedWithdrawTxs_otherErrorsKeepRecord verifies transient broadcast failures
// are not treated as already-known and do not drop the record.
func TestReplaySignedWithdrawTxs_otherErrorsKeepRecord(t *testing.T) {
	store := &memSignedWithdrawTxStore{}
	n := &neutrinoClient{signedTxs: store}
	b := &btcWallet{client: n, pendingTxs: make(map[chainhash.Hash]*btcPendingTx)}

	burn := []byte("burn-transient")
	require.NoError(t, n.persistSignedWithdrawTx(burn, testSignedWithdrawTx(10)))
	b.sendRawTxFn = func(*wire.MsgTx) error { return errors.New("missing inputs") }

	require.NotPanics(t, b.replaySignedWithdrawTxs)
	raw, err := store.Get(burn)
	require.NoError(t, err)
	require.NotNil(t, raw)
}

// TestBroadcastErrorIsAlreadyKnown covers the error classifier used by the replay.
func TestBroadcastErrorIsAlreadyKnown(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("sendrawtransaction: txn-already-known"), true},
		{errors.New("Transaction already in the mempool"), true},
		{errors.New("missing inputs"), false},
		{errors.New("connection reset by peer"), false},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, broadcastErrorIsAlreadyKnown(tc.err), "err=%v", tc.err)
	}
}

// TestPersistFailureDoesNotFailBroadcast verifies a failed persistence of the signed tx
// neither undoes nor masks the already-completed broadcast, and that a failed broadcast
// does not persist anything.
func TestPersistFailureDoesNotFailBroadcast(t *testing.T) {
	burn := []byte("burn-persist-fail")
	tx := testSignedWithdrawTx(11)

	t.Run("persist failure", func(t *testing.T) {
		store := &memSignedWithdrawTxStore{putErr: errors.New("disk full")}
		n := &neutrinoClient{signedTxs: store}
		sent := 0
		b := &btcWallet{client: n, sendRawTxFn: func(*wire.MsgTx) error { sent++; return nil }}

		require.NotPanics(t, func() {
			require.NoError(t, b.broadcastSignedWithdrawTx(tx, tx.TxHash().String(), burn))
		})
		require.Equal(t, 1, sent, "the broadcast must still happen and succeed")
	})

	t.Run("broadcast failure does not persist", func(t *testing.T) {
		store := &memSignedWithdrawTxStore{}
		n := &neutrinoClient{signedTxs: store}
		b := &btcWallet{client: n, sendRawTxFn: func(*wire.MsgTx) error {
			return fmt.Errorf("rejected")
		}}

		require.Error(t, b.broadcastSignedWithdrawTx(tx, tx.TxHash().String(), burn))
		raw, err := store.Get(burn)
		require.NoError(t, err)
		require.Nil(t, raw)
	})
}
