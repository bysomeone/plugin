package neutrino

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/33cn/chain33/types"
	typesmocks "github.com/33cn/chain33/types/mocks"
	ltypes "github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/neutrino"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

const bumpTestAddress = "bcrt1qw508d6qejxtdg4y5r3zarvary0c5xw7kygt080"

func bumpTestPoolScript() []byte {
	return append([]byte{txscript.OP_0, 0x14}, bytes.Repeat([]byte{0x42}, 20)...)
}

func bumpTestBurnHash() []byte { return bytes.Repeat([]byte{0x07}, 32) }

func bumpTestUTXO(nonce byte, amount btcutil.Amount) *UTXO {
	return &UTXO{
		OutPoint: wire.OutPoint{Hash: chainhash.Hash{nonce}, Index: uint32(nonce)},
		Amount:   amount,
		PkScript: bumpTestPoolScript(),
	}
}

// TestTssSignNotify_bumpFieldsRoundTrip pins the regenerated proto: the three bump fields must
// survive the codec used on the P2P topic.
func TestTssSignNotify_bumpFieldsRoundTrip(t *testing.T) {
	in := &ltypes.TssSignNotify{
		TxType:            transactionTypeWithdraw,
		PrevBtcTxData:     []byte{0x01, 0x02},
		PrevInputAmounts:  []int64{10, 20},
		BumpRequestTxHash: []byte("request"),
	}
	out := &ltypes.TssSignNotify{}
	require.NoError(t, types.Decode(types.Encode(in), out))
	require.Equal(t, in.PrevBtcTxData, out.PrevBtcTxData)
	require.Equal(t, in.PrevInputAmounts, out.PrevInputAmounts)
	require.Equal(t, in.BumpRequestTxHash, out.BumpRequestTxHash)
}

// TestBuildWithdrawReplacementTx_capsRateAndKeepsDust verifies the requested rate is capped by the
// largest rate that still leaves the user output above dust, and that the sticky input is reused.
func TestBuildWithdrawReplacementTx_capsRateAndKeepsDust(t *testing.T) {
	poolScript := bumpTestPoolScript()
	sticky := bumpTestUTXO(2, 80_000)
	extra := bumpTestUTXO(1, 40_000)
	b := &btcWallet{
		chainParams: chaincfg.RegressionNetParams,
		tssPkScript: poolScript,
		client:      &neutrinoClient{deposits: newDepositScriptSet()},
		selectInputsFn: func(target, feeRate btcutil.Amount, gotSticky *UTXO) ([]*UTXO, btcutil.Amount, error) {
			require.Equal(t, btcutil.Amount(100_000), target)
			require.Equal(t, btcutil.Amount(0), feeRate, "receiver-pays selection must not pre-reserve the fee")
			require.NotNil(t, gotSticky)
			return []*UTXO{extra, sticky}, extra.Amount + sticky.Amount, nil
		},
	}
	req := &withdrawRequest{
		chain33WithdrawHash: bumpTestBurnHash(),
		amount:              100_000,
		toAddress:           bumpTestAddress,
		stickyUTXO:          sticky,
		bump: &withdrawBumpContext{
			requestTxHash:    []byte("bump-request"),
			requestedFeeRate: 10_000, // far above what dust allows
			appliedFeeRate:   1,
		},
	}

	tx, inputAmounts, selected, err := b.buildWithdrawTx(req)
	require.NoError(t, err)
	require.Len(t, selected, 2)
	require.True(t, containsOutPoint(selected, sticky.OutPoint), "the replacement must spend the bound sticky input")

	baseSize, err := withdrawFeeBaseSize(tx, poolScript)
	require.NoError(t, err)
	effective := int64(req.bump.effectiveFeeRate)
	require.Greater(t, effective, int64(req.bump.appliedFeeRate))
	maxFeasible := maxWithdrawFeeRate(100_000, baseSize)
	require.Equal(t, maxFeasible, effective, "the requested rate must be capped at the dust-feasible rate")
	require.Less(t, effective, int64(req.bump.requestedFeeRate))

	var totalIn, totalOut int64
	for _, amount := range inputAmounts {
		totalIn += amount
	}
	for _, out := range tx.TxOut {
		totalOut += out.Value
	}
	require.Equal(t, int64(baseSize)*effective, totalIn-totalOut, "fee must equal baseSize*effective")
	require.Equal(t, btcutil.Amount(effective), req.feeRate, "validation must use the capped rate")

	destAddr, err := btcutil.DecodeAddress(bumpTestAddress, &chaincfg.RegressionNetParams)
	require.NoError(t, err)
	destScript, err := txscript.PayToAddrScript(destAddr)
	require.NoError(t, err)
	var destValue int64 = -1
	for _, out := range tx.TxOut {
		if bytes.Equal(out.PkScript, destScript) {
			destValue = out.Value
		}
	}
	require.GreaterOrEqual(t, destValue, int64(minChangeAmount), "the user output must stay at or above dust")
	require.Equal(t, int64(req.amount)-totalIn+totalOut, destValue)
}

// TestBuildWithdrawReplacementTx_rejectsWhenRateCannotRise verifies a capped rate that is not
// strictly above the applied rate yields errWithdrawBumpRateNotHigher (no signing attempt).
func TestBuildWithdrawReplacementTx_rejectsWhenRateCannotRise(t *testing.T) {
	sticky := bumpTestUTXO(2, 200_000)
	b := &btcWallet{
		chainParams: chaincfg.RegressionNetParams,
		tssPkScript: bumpTestPoolScript(),
		client:      &neutrinoClient{deposits: newDepositScriptSet()},
		selectInputsFn: func(target, feeRate btcutil.Amount, gotSticky *UTXO) ([]*UTXO, btcutil.Amount, error) {
			return []*UTXO{sticky}, sticky.Amount, nil
		},
	}
	req := &withdrawRequest{
		chain33WithdrawHash: bumpTestBurnHash(),
		amount:              100_000,
		toAddress:           bumpTestAddress,
		stickyUTXO:          sticky,
		bump: &withdrawBumpContext{
			requestTxHash:    []byte("bump-request"),
			requestedFeeRate: 5_000,
			appliedFeeRate:   500, // above anything dust allows here
		},
	}
	_, _, _, err := b.buildWithdrawTx(req)
	require.ErrorIs(t, err, errWithdrawBumpRateNotHigher)
}

// bumpHandlerFake is the injectable state for bumpWithdrawFeeHandler tests.
type bumpHandlerFake struct {
	pending    *rtypes.PendingTx
	rgb20      bool
	state      []byte
	sticky     *UTXO
	prevRaw    []byte
	applied    btcutil.Amount
	hasApplied bool
	amounts    []int64

	processes  int
	processErr error
	lastReq    *withdrawRequest
}

func (f *bumpHandlerFake) handler() *bumpWithdrawFeeHandler {
	return &bumpWithdrawFeeHandler{deps: bumpWithdrawFeeHandlerDeps{
		getPending: func([]byte) (*rtypes.PendingTx, error) { return f.pending, nil },
		isRgb20:    func(string) bool { return f.rgb20 },
		getState:   func([]byte) []byte { return f.state },
		getSticky:  func([]byte) *UTXO { return f.sticky },
		getSignedTx: func([]byte) ([]byte, bool, error) {
			return f.prevRaw, len(f.prevRaw) > 0, nil
		},
		getAppliedRate: func([]byte) (btcutil.Amount, bool) { return f.applied, f.hasApplied },
		loadInputAmounts: func([]byte) ([]int64, bool) {
			return f.amounts, f.amounts != nil
		},
		process: func(req *withdrawRequest) error {
			f.processes++
			f.lastReq = req
			if f.processErr != nil {
				return f.processErr
			}
			f.applied = req.bump.requestedFeeRate
			f.hasApplied = true
			return nil
		},
	}}
}

func newBumpHandlerFake(t *testing.T) *bumpHandlerFake {
	t.Helper()
	prev := testSignedWithdrawTx(5)
	return &bumpHandlerFake{
		pending: &rtypes.PendingTx{
			ActionType:    rtypes.TyWithdrawAsset,
			Amount:        100_000,
			FeeRate:       1,
			TxHash:        bumpTestBurnHash(),
			AssetSymbol:   rtypes.BTCSymbol,
			TargetAddress: bumpTestAddress,
			TxBlockHeight: 10,
		},
		state:   withdrawStatusSent,
		sticky:  bumpTestUTXO(2, 80_000),
		prevRaw: mustSerializeTx(t, prev),
		amounts: []int64{20_000},
	}
}

func bumpTestItem(rate int64) *rtypes.BumpWithdrawFeeItem {
	return &rtypes.BumpWithdrawFeeItem{
		TxHash:           []byte("bump-request"),
		WithdrawalTxHash: bumpTestBurnHash(),
		Owner:            "owner",
		FeeRate:          rate,
		OldFeeRate:       1,
		Height:           11,
	}
}

// TestBumpWithdrawFeeHandler_skipsRgb20 verifies RGB20 withdrawals are never replaced.
func TestBumpWithdrawFeeHandler_skipsRgb20(t *testing.T) {
	fake := newBumpHandlerFake(t)
	fake.rgb20 = true
	require.NoError(t, fake.handler().handle(bumpTestItem(10)))
	require.Zero(t, fake.processes)
}

// TestBumpWithdrawFeeHandler_retriesWithoutSticky verifies a request that arrives before the
// initial withdrawal has bound its sticky input is retried rather than dropped.
func TestBumpWithdrawFeeHandler_retriesWithoutSticky(t *testing.T) {
	fake := newBumpHandlerFake(t)
	fake.sticky = nil
	require.Error(t, fake.handler().handle(bumpTestItem(10)))
	require.Zero(t, fake.processes)
}

// TestBumpWithdrawFeeHandler_idempotent verifies the same on-chain request is applied once even
// if the poller (or a restart replay) hands it over again.
func TestBumpWithdrawFeeHandler_idempotent(t *testing.T) {
	fake := newBumpHandlerFake(t)
	h := fake.handler()
	item := bumpTestItem(10)

	require.NoError(t, h.handle(item))
	require.Equal(t, 1, fake.processes)
	require.Equal(t, int64(10), int64(fake.lastReq.bump.requestedFeeRate))

	// Same request a second time: the applied rate is now 10, so it is stale.
	require.NoError(t, h.handle(item))
	require.Equal(t, 1, fake.processes, "a repeated request must not sign again")
}

// TestBumpWithdrawFeeHandler_cappedRateIsNotAnError verifies a request the bridge cannot honor is
// reported (WARN) and skipped, not retried forever and not recorded as a failure.
func TestBumpWithdrawFeeHandler_cappedRateIsNotAnError(t *testing.T) {
	fake := newBumpHandlerFake(t)
	fake.processErr = errWithdrawBumpRateNotHigher
	require.NoError(t, fake.handler().handle(bumpTestItem(50)))
	require.Equal(t, 1, fake.processes)
}

// TestBumpWithdrawFeeHandler_transientErrorIsRetried verifies a signing/broadcast failure is
// returned so the processor keeps it in the retry queue.
func TestBumpWithdrawFeeHandler_transientErrorIsRetried(t *testing.T) {
	fake := newBumpHandlerFake(t)
	want := errors.New("tss session timeout")
	fake.processErr = want
	require.ErrorIs(t, fake.handler().handle(bumpTestItem(10)), want)
}

// TestBumpWithdrawFeeProcessor_pollingCursor verifies query failure does not advance the cursor,
// successful windows advance past their end, and later requests are picked up from the cursor.
func TestBumpWithdrawFeeProcessor_pollingCursor(t *testing.T) {
	items := []*rtypes.BumpWithdrawFeeItem{
		{TxHash: []byte("a"), WithdrawalTxHash: []byte("burn-a"), Height: 10, Index: 0},
		{TxHash: []byte("b"), WithdrawalTxHash: []byte("burn-b"), Height: 10, Index: 1},
	}
	fail := true
	var handled []string
	p := &bumpWithdrawFeeProcessor{
		query: func(startHeight, startIndex, endHeight int64) (*rtypes.BumpWithdrawFeeList, error) {
			if fail {
				return nil, errors.New("chain query unavailable")
			}
			out := make([]*rtypes.BumpWithdrawFeeItem, 0)
			for _, item := range items {
				if item.Height < startHeight || (item.Height == startHeight && int64(item.Index) < startIndex) ||
					item.Height > endHeight {
					continue
				}
				out = append(out, item)
			}
			return &rtypes.BumpWithdrawFeeList{Items: out}, nil
		},
		handle: func(item *rtypes.BumpWithdrawFeeItem) error {
			handled = append(handled, string(item.TxHash))
			return nil
		},
	}
	now := time.Now()

	require.False(t, p.pollOnce(30, now))
	require.Empty(t, handled)
	require.Zero(t, p.nextHeight)

	fail = false
	require.True(t, p.pollOnce(30, now))
	require.Equal(t, []string{"a", "b"}, handled)
	require.Equal(t, int64(31), p.nextHeight, "an exhausted window advances past its end")

	p.pollOnce(30, now)
	require.Equal(t, []string{"a", "b"}, handled, "already scanned requests must not be processed again")

	items = append(items, &rtypes.BumpWithdrawFeeItem{TxHash: []byte("c"), WithdrawalTxHash: []byte("burn-c"), Height: 40})
	p.pollOnce(45, now)
	require.Equal(t, []string{"a", "b", "c"}, handled)
}

// TestBumpWithdrawFeeProcessor_retryBackoff verifies failed items are retried with backoff and
// removed once they succeed.
func TestBumpWithdrawFeeProcessor_retryBackoff(t *testing.T) {
	item := &rtypes.BumpWithdrawFeeItem{TxHash: []byte("a"), WithdrawalTxHash: []byte("burn-a"), Height: 10}
	attempts := 0
	p := &bumpWithdrawFeeProcessor{
		handle: func(*rtypes.BumpWithdrawFeeItem) error {
			attempts++
			if attempts < 2 {
				return errors.New("temporary")
			}
			return nil
		},
	}
	now := time.Now()
	p.handleItem(item, now)
	require.Equal(t, 1, attempts)
	require.Len(t, p.retrying, 1)

	p.retryPending(now.Add(time.Second))
	require.Equal(t, 1, attempts, "backoff must delay the retry")

	p.retryPending(now.Add(bumpWithdrawFeePollInterval + time.Second))
	require.Equal(t, 2, attempts)
	require.Empty(t, p.retrying)
}

// newBumpSigner 造一个只依赖链上查询的签名节点：QueryChain 返回给定的加价请求，
// GetLastHeader 返回固定高度。
func newBumpSigner(t *testing.T, item *rtypes.BumpWithdrawFeeItem) (*tssService, *neutrinoClient) {
	t.Helper()
	chain := &typesmocks.Chain33Client{}
	chain.On("GetLastHeader", mock.Anything, mock.Anything).Return(&types.Header{Height: 100}, nil)
	chain.On("QueryChain", mock.Anything, mock.Anything).Return(
		func(_ context.Context, in *types.ChainExecutor, _ ...grpc.CallOption) (*types.Reply, error) {
			require.Equal(t, "ListBumpWithdrawFeeRequests", in.GetFuncName())
			list := &rtypes.BumpWithdrawFeeList{Items: []*rtypes.BumpWithdrawFeeItem{item}}
			return &types.Reply{IsOk: true, Msg: types.Encode(list)}, nil
		}, nil)
	n := &neutrinoClient{ctx: context.Background(), mainChainGrpc: chain}
	return &tssService{client: n, pkScript: bumpTestPoolScript()}, n
}

// buildBumpTx builds a replacement-shaped tx at the given rate: one input worth the pending
// amount, the burn OP_RETURN and the user output reduced by base*rate (no change output needed).
func buildBumpTx(t *testing.T, burn []byte, rate int64) (*wire.MsgTx, []int64, int) {
	t.Helper()
	b := &btcWallet{}
	baseSize := b.estimateTxSize(1, 1, withdrawOpReturnScriptLen)

	destAddr, err := btcutil.DecodeAddress(bumpTestAddress, &chaincfg.RegressionNetParams)
	require.NoError(t, err)
	destScript, err := txscript.PayToAddrScript(destAddr)
	require.NoError(t, err)

	tx := wire.NewMsgTx(wire.TxVersion)
	txIn := wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{0x51}, Index: 0}, nil, nil)
	txIn.Sequence = withdrawInputSequence
	tx.AddTxIn(txIn)

	payload := append([]byte(withdrawOpReturnPrefix), burn...)
	opScript, err := txscript.NullDataScript(payload)
	require.NoError(t, err)
	tx.AddTxOut(wire.NewTxOut(0, opScript))
	tx.AddTxOut(wire.NewTxOut(100_000-int64(baseSize)*rate, destScript))

	inputAmounts := []int64{100_000}
	return tx, inputAmounts, baseSize
}

func bumpSignerPending() *rtypes.PendingTx {
	return &rtypes.PendingTx{
		ActionType:    rtypes.TyWithdrawAsset,
		Amount:        100_000,
		FeeRate:       1,
		TxHash:        bumpTestBurnHash(),
		AssetSymbol:   rtypes.BTCSymbol,
		TargetAddress: bumpTestAddress,
		TxBlockHeight: 10,
	}
}

func bumpSignerNotify(t *testing.T, burn []byte, item *rtypes.BumpWithdrawFeeItem,
	prevTx *wire.MsgTx, prevAmounts []int64) *ltypes.TssSignNotify {
	t.Helper()
	return &ltypes.TssSignNotify{
		Payload:           burn,
		PrevBtcTxData:     mustSerializeTx(t, prevTx),
		PrevInputAmounts:  prevAmounts,
		BumpRequestTxHash: item.TxHash,
	}
}

// bumpSignerPrevTx builds a previous signed withdrawal sharing input {0x51,0}, with a small fee.
func bumpSignerPrevTx(t *testing.T) (*wire.MsgTx, []int64) {
	t.Helper()
	tx := wire.NewMsgTx(wire.TxVersion)
	txIn := wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{0x51}, Index: 0}, nil, nil)
	txIn.Sequence = withdrawInputSequence
	txIn.Witness = wire.TxWitness{{0x30, 0x01}, {0x02, 0x01}}
	tx.AddTxIn(txIn)
	tx.AddTxOut(wire.NewTxOut(99_900, bumpTestPoolScript()))
	return tx, []int64{100_000}
}

// TestValidateWithdrawReplacement_acceptsAndRejectsRate verifies signers recompute the effective
// rate from the tx and the on-chain request, and reject a claimed rate that does not match.
func TestValidateWithdrawReplacement_acceptsAndRejectsRate(t *testing.T) {
	burn := bumpTestBurnHash()
	item := &rtypes.BumpWithdrawFeeItem{
		TxHash: []byte("bump-request"), WithdrawalTxHash: burn, FeeRate: 10, OldFeeRate: 1,
	}
	signer, _ := newBumpSigner(t, item)
	pending := bumpSignerPending()
	req := pending2WithdrawRequest(pending)
	prev, prevAmounts := bumpSignerPrevTx(t)

	tx, amounts, _ := buildBumpTx(t, burn, 10)
	notify := bumpSignerNotify(t, burn, item, prev, prevAmounts)
	rate, err := signer.validateWithdrawReplacement(tx, amounts, req, pending, notify)
	require.NoError(t, err)
	require.Equal(t, btcutil.Amount(10), rate)

	// A tx that does not pay the on-chain requested rate must be rejected.
	wrongRate, wrongAmounts, _ := buildBumpTx(t, burn, 9)
	_, err = signer.validateWithdrawReplacement(wrongRate, wrongAmounts, req, pending, notify)
	require.Error(t, err)
	require.Contains(t, err.Error(), "outside the range implied by the on-chain request")
}

// buildBumpTxWithChangeDust builds the same shape with an extra input whose value is a sub-dust
// leftover: the builder would donate it to the fee instead of creating a change output.
func buildBumpTxWithChangeDust(t *testing.T, burn []byte, rate int64, leftover int64) (*wire.MsgTx, []int64) {
	t.Helper()
	b := &btcWallet{}
	baseSize := b.estimateTxSize(2, 1, withdrawOpReturnScriptLen)
	destAddr, err := btcutil.DecodeAddress(bumpTestAddress, &chaincfg.RegressionNetParams)
	require.NoError(t, err)
	destScript, err := txscript.PayToAddrScript(destAddr)
	require.NoError(t, err)

	tx := wire.NewMsgTx(wire.TxVersion)
	shared := wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{0x51}, Index: 0}, nil, nil)
	shared.Sequence = withdrawInputSequence
	tx.AddTxIn(shared)
	extra := wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{0x52}, Index: 1}, nil, nil)
	extra.Sequence = withdrawInputSequence
	tx.AddTxIn(extra)
	opScript, err := txscript.NullDataScript(append([]byte(withdrawOpReturnPrefix), burn...))
	require.NoError(t, err)
	tx.AddTxOut(wire.NewTxOut(0, opScript))
	tx.AddTxOut(wire.NewTxOut(100_000-int64(baseSize)*rate, destScript))
	return tx, []int64{100_000, leftover}
}

// TestValidateWithdrawReplacement_toleratesDustLeftover verifies the sub-dust leftover that the
// builder donates to the fee is accepted (it does not change the user's output).
func TestValidateWithdrawReplacement_toleratesDustLeftover(t *testing.T) {
	burn := bumpTestBurnHash()
	item := &rtypes.BumpWithdrawFeeItem{
		TxHash: []byte("bump-request"), WithdrawalTxHash: burn, FeeRate: 10, OldFeeRate: 1,
	}
	signer, _ := newBumpSigner(t, item)
	pending := bumpSignerPending()
	req := pending2WithdrawRequest(pending)
	prev, prevAmounts := bumpSignerPrevTx(t)

	tx, amounts := buildBumpTxWithChangeDust(t, burn, 10, 300)
	rate, err := signer.validateWithdrawReplacement(tx, amounts, req, pending,
		bumpSignerNotify(t, burn, item, prev, prevAmounts))
	require.NoError(t, err)
	require.Equal(t, btcutil.Amount(10), rate)

	// More than dust donated means the fee is outside the accepted band.
	tx, amounts = buildBumpTxWithChangeDust(t, burn, 10, 600)
	_, err = signer.validateWithdrawReplacement(tx, amounts, req, pending,
		bumpSignerNotify(t, burn, item, prev, prevAmounts))
	require.Error(t, err)
}

// TestValidateWithdrawReplacement_rejectsNoSharedInput verifies the mutual-exclusion rule:
// a replacement that does not spend an input of the previous signed tx is rejected.
func TestValidateWithdrawReplacement_rejectsNoSharedInput(t *testing.T) {
	burn := bumpTestBurnHash()
	item := &rtypes.BumpWithdrawFeeItem{
		TxHash: []byte("bump-request"), WithdrawalTxHash: burn, FeeRate: 10, OldFeeRate: 1,
	}
	signer, _ := newBumpSigner(t, item)
	pending := bumpSignerPending()
	req := pending2WithdrawRequest(pending)

	prev, prevAmounts := bumpSignerPrevTx(t)
	prev.TxIn[0].PreviousOutPoint = wire.OutPoint{Hash: chainhash.Hash{0x99}, Index: 3}
	tx, amounts, _ := buildBumpTx(t, burn, 10)
	notify := bumpSignerNotify(t, burn, item, prev, prevAmounts)

	_, err := signer.validateWithdrawReplacement(tx, amounts, req, pending, notify)
	require.Error(t, err)
	require.Contains(t, err.Error(), "shares no input")
}

// TestValidateWithdrawReplacement_capsAtDustLimit verifies the signer accepts the dust-capped
// rate (not the raw requested rate) when the request is above what the output can pay.
func TestValidateWithdrawReplacement_capsAtDustLimit(t *testing.T) {
	burn := bumpTestBurnHash()
	item := &rtypes.BumpWithdrawFeeItem{
		TxHash: []byte("bump-request"), WithdrawalTxHash: burn, FeeRate: 50_000, OldFeeRate: 1,
	}
	signer, _ := newBumpSigner(t, item)
	pending := bumpSignerPending()
	req := pending2WithdrawRequest(pending)
	prev, prevAmounts := bumpSignerPrevTx(t)

	_, _, baseSize := buildBumpTx(t, burn, 1)
	capped := maxWithdrawFeeRate(pending.GetAmount(), baseSize)
	require.Greater(t, capped, int64(1))
	tx, amounts, _ := buildBumpTx(t, burn, capped)

	rate, err := signer.validateWithdrawReplacement(tx, amounts, req, pending,
		bumpSignerNotify(t, burn, item, prev, prevAmounts))
	require.NoError(t, err)
	require.Equal(t, btcutil.Amount(capped), rate)
}

// TestReplaceSignedWithdrawTx_monotonicAcrossRestart verifies the applied rate survives a restart
// and refuses a lower/equal replacement, while a higher one overwrites the replay record.
func TestReplaceSignedWithdrawTx_monotonicAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	_, db, err := openWalletDB(dir, "bump.db")
	require.NoError(t, err)

	burn := bumpTestBurnHash()
	first := testSignedWithdrawTx(1)
	n := &neutrinoClient{neutrinoCfg: neutrino.Config{Database: db}}
	require.NoError(t, n.persistSignedWithdrawTx(burn, first))

	second := testSignedWithdrawTx(2)
	require.NoError(t, n.replaceSignedWithdrawTx(burn, second, 10, 1))
	raw, err := n.signedWithdrawTxStorage().Get(burn)
	require.NoError(t, err)
	require.Equal(t, mustSerializeTx(t, second), raw, "the replay record must hold the newest version")

	require.NoError(t, db.Close())
	_, reopened, err := openWalletDB(dir, "bump.db")
	require.NoError(t, err)
	defer reopened.Close()
	restarted := &neutrinoClient{neutrinoCfg: neutrino.Config{Database: reopened}}

	applied, ok := restarted.getAppliedWithdrawFeeRate(burn)
	require.True(t, ok)
	require.Equal(t, btcutil.Amount(10), applied)

	require.Error(t, restarted.replaceSignedWithdrawTx(burn, testSignedWithdrawTx(3), 10, applied),
		"the applied rate must stay monotonic across restarts")
	third := testSignedWithdrawTx(3)
	require.NoError(t, restarted.replaceSignedWithdrawTx(burn, third, 11, applied))
	raw, err = restarted.signedWithdrawTxStorage().Get(burn)
	require.NoError(t, err)
	require.Equal(t, mustSerializeTx(t, third), raw)
}

// TestSignedWithdrawInputAmountsRoundTrip verifies the coordinator persists the input amounts the
// next bump needs to hand to the other signers.
func TestSignedWithdrawInputAmountsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	_, db, err := openWalletDB(dir, "bump-inputs.db")
	require.NoError(t, err)
	defer db.Close()

	burn := bumpTestBurnHash()
	n := &neutrinoClient{neutrinoCfg: neutrino.Config{Database: db}}
	require.NoError(t, n.saveSignedWithdrawInputAmounts(burn, []int64{10, 20, 30}))
	amounts, ok := n.loadSignedWithdrawInputAmounts(burn)
	require.True(t, ok)
	require.Equal(t, []int64{10, 20, 30}, amounts)
}
