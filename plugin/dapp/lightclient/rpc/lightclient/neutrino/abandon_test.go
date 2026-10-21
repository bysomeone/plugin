package neutrino

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"

	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/stretchr/testify/require"
)

// 本文件覆盖手工放弃提现的两块桥侧前提：已签付款的持久记录（含 fail-closed），
// 以及提案受理判定 / 轮询去重 / abandoning 单向性。

func testAbandonBurn() []byte { return bytes.Repeat([]byte{0x33}, 32) }

func testAbandonItem() *rtypes.AbandonWithdrawItem {
	return &rtypes.AbandonWithdrawItem{
		TxHash:     bytes.Repeat([]byte{0x44}, 32),
		BurnTxHash: testAbandonBurn(),
		Owner:      "1OwnerAddress",
		Reason:     "payment can never settle",
		Proposer:   "1OperatorAddress",
		Height:     10,
		Index:      1,
	}
}

// Test_evaluateAbandonProposal 受理判定必须全部通过：白名单、链上 payload、台账 owner、
// 终态键、已签付款，任一条不满足都拒绝。
func Test_evaluateAbandonProposal(t *testing.T) {
	operators := []string{"1OperatorAddress"}
	item := testAbandonItem()
	pending := testWithdrawPending()
	withdraw := testWithdrawPayload()

	require.NoError(t, evaluateAbandonProposal(item, pending, withdraw, "", operators, false))

	cases := []struct {
		name     string
		mutate   func(*rtypes.AbandonWithdrawItem, *rtypes.PendingTx, *rtypes.WithdrawAsset)
		terminal string
		signed   bool
		ops      []string
		wantErr  string
	}{
		{
			name: "proposer not whitelisted",
			mutate: func(i *rtypes.AbandonWithdrawItem, _ *rtypes.PendingTx, _ *rtypes.WithdrawAsset) {
				i.Proposer = "1Stranger"
			},
			ops:     operators,
			wantErr: "not a configured abandon operator",
		},
		{
			name:    "empty whitelist",
			mutate:  func(*rtypes.AbandonWithdrawItem, *rtypes.PendingTx, *rtypes.WithdrawAsset) {},
			ops:     nil,
			wantErr: "not a configured abandon operator",
		},
		{
			name: "withdraw payload missing",
			mutate: func(_ *rtypes.AbandonWithdrawItem, _ *rtypes.PendingTx, w *rtypes.WithdrawAsset) {
				*w = rtypes.WithdrawAsset{}
			},
			ops:     operators,
			wantErr: "withdraw payload missing or malformed",
		},
		{
			name: "owner mismatch",
			mutate: func(i *rtypes.AbandonWithdrawItem, _ *rtypes.PendingTx, _ *rtypes.WithdrawAsset) {
				i.Owner = "1SomeoneElse"
			},
			ops:     operators,
			wantErr: "owner mismatch",
		},
		{
			name:    "empty proposal owner",
			mutate:  func(i *rtypes.AbandonWithdrawItem, _ *rtypes.PendingTx, _ *rtypes.WithdrawAsset) { i.Owner = "" },
			ops:     operators,
			wantErr: "proposal owner is empty",
		},
		{
			name: "not a withdraw pending",
			mutate: func(_ *rtypes.AbandonWithdrawItem, p *rtypes.PendingTx, _ *rtypes.WithdrawAsset) {
				p.ActionType = rtypes.TyDepositAsset
			},
			ops:     operators,
			wantErr: "not a withdraw",
		},
		{
			name:    "withdraw already confirmed",
			mutate:  func(_ *rtypes.AbandonWithdrawItem, p *rtypes.PendingTx, _ *rtypes.WithdrawAsset) { p.Confirmed = true },
			ops:     operators,
			wantErr: "already confirmed",
		},
		{
			name:     "chain already settled",
			mutate:   func(*rtypes.AbandonWithdrawItem, *rtypes.PendingTx, *rtypes.WithdrawAsset) {},
			terminal: withdrawFinalSettled,
			ops:      operators,
			wantErr:  "already terminal on chain: settled",
		},
		{
			name:     "chain already released",
			mutate:   func(*rtypes.AbandonWithdrawItem, *rtypes.PendingTx, *rtypes.WithdrawAsset) {},
			terminal: withdrawFinalReleased,
			ops:      operators,
			wantErr:  "already terminal on chain: released",
		},
		{
			name:    "payment already signed",
			mutate:  func(*rtypes.AbandonWithdrawItem, *rtypes.PendingTx, *rtypes.WithdrawAsset) {},
			signed:  true,
			ops:     operators,
			wantErr: "payment signature was already produced",
		},
		{
			name:    "empty burn hash",
			mutate:  func(i *rtypes.AbandonWithdrawItem, _ *rtypes.PendingTx, _ *rtypes.WithdrawAsset) { i.BurnTxHash = nil },
			ops:     operators,
			wantErr: "empty burn tx hash",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i := testAbandonItem()
			p := testWithdrawPending()
			w := testWithdrawPayload()
			tc.mutate(i, p, w)
			err := evaluateAbandonProposal(i, p, w, tc.terminal, tc.ops, tc.signed)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

// Test_isAbandoningState 单向性：受理后结算永远让位；其它状态不受影响。
func Test_isAbandoningState(t *testing.T) {
	cases := []struct {
		state           []byte
		wantAbandoning  bool
		wantSettleAllow bool
	}{
		{nil, false, true},
		{withdrawStatusSent, false, true},
		{withdrawStatusConfirmed, false, true},
		{withdrawStatusUnlockPending, false, true},
		{withdrawStatusUnlockSubmitted, false, true},
		{withdrawStatusAbandoning, true, false},
	}
	for _, tc := range cases {
		require.Equal(t, tc.wantAbandoning, isAbandoningState(tc.state), "state=%q", tc.state)
		require.Equal(t, tc.wantSettleAllow, settlementAllowed(tc.state), "state=%q", tc.state)
	}
}

func Test_isAbandonOperator(t *testing.T) {
	ops := []string{" 1OperatorAddress ", ""}
	require.True(t, isAbandonOperator(ops, "1OperatorAddress"))
	require.True(t, isAbandonOperator(ops, " 1OperatorAddress "))
	require.False(t, isAbandonOperator(ops, "1operatoraddress"), "addresses are case-sensitive")
	require.False(t, isAbandonOperator(ops, ""))
	require.False(t, isAbandonOperator(nil, "1OperatorAddress"))
}

// Test_abandonProcessor_pollDedupAndAdvance 轮询按游标推进、重复的提案 txHash 只处理一次，
// 查询失败不推进游标。
func Test_abandonProcessor_pollDedupAndAdvance(t *testing.T) {
	itemA := testAbandonItem()
	itemA.TxHash = []byte("proposal-a")
	itemA.Height, itemA.Index = 10, 1
	itemB := testAbandonItem()
	itemB.TxHash = []byte("proposal-b")
	itemB.Height, itemB.Index = 10, 2

	all := []*rtypes.AbandonWithdrawItem{itemA, itemA, itemB}
	fail := true
	var queried []string
	var handled []string
	p := &abandonProcessor{
		query: func(startHeight, startIndex, endHeight int64) (*rtypes.AbandonWithdrawList, error) {
			queried = append(queried, hex.EncodeToString([]byte{byte(startHeight), byte(startIndex), byte(endHeight)}))
			if fail {
				return nil, errors.New("chain query unavailable")
			}
			var out []*rtypes.AbandonWithdrawItem
			for _, it := range all {
				if it.GetHeight() > startHeight ||
					(it.GetHeight() == startHeight && int64(it.GetIndex()) >= startIndex) {
					out = append(out, it)
				}
			}
			return &rtypes.AbandonWithdrawList{Items: out}, nil
		},
		handle: func(item *rtypes.AbandonWithdrawItem) bool {
			handled = append(handled, string(item.GetTxHash()))
			return false
		},
	}

	// Query failure: cursor must not move.
	p.pollOnce(20)
	require.Empty(t, handled)
	require.Equal(t, int64(0), p.nextHeight)
	require.Equal(t, int64(0), p.nextIndex)

	fail = false
	p.pollOnce(20)
	require.Equal(t, []string{"proposal-a", "proposal-b"}, handled, "duplicate proposal must be handled once")
	require.Equal(t, int64(21), p.nextHeight, "an exhausted window advances past its end")
	require.Equal(t, int64(0), p.nextIndex)

	// Same range again: cursor is past the window, so nothing is re-handled.
	p.pollOnce(20)
	require.Equal(t, []string{"proposal-a", "proposal-b"}, handled)

	// A later proposal is picked up from the advanced cursor.
	itemC := testAbandonItem()
	itemC.TxHash = []byte("proposal-c")
	itemC.Height, itemC.Index = 25, 1
	all = append(all, itemC)
	p.pollOnce(30)
	require.Equal(t, []string{"proposal-a", "proposal-b", "proposal-c"}, handled)
	require.NotEmpty(t, queried)
}

// Test_abandonProcessor_pagesFullWindow 一个高度窗口内的提案超过单页条数时，从最后一条继续，
// 不会因为 Count 截断而漏掉后面的提案。
func Test_abandonProcessor_pagesFullWindow(t *testing.T) {
	var items []*rtypes.AbandonWithdrawItem
	for i := 0; i < abandonQueryCount; i++ {
		item := testAbandonItem()
		item.TxHash = []byte(fmt.Sprintf("proposal-%03d", i))
		item.Height, item.Index = 5, int32(i)
		items = append(items, item)
	}
	lastInWindow := testAbandonItem()
	lastInWindow.TxHash = []byte("proposal-tail")
	lastInWindow.Height, lastInWindow.Index = 6, 0
	items = append(items, lastInWindow)

	var handled []string
	p := &abandonProcessor{
		query: func(startHeight, startIndex, endHeight int64) (*rtypes.AbandonWithdrawList, error) {
			var out []*rtypes.AbandonWithdrawItem
			for _, it := range items {
				if it.GetHeight() < startHeight ||
					(it.GetHeight() == startHeight && int64(it.GetIndex()) < startIndex) ||
					it.GetHeight() > endHeight {
					continue
				}
				out = append(out, it)
				if len(out) >= abandonQueryCount {
					break
				}
			}
			return &rtypes.AbandonWithdrawList{Items: out}, nil
		},
		handle: func(item *rtypes.AbandonWithdrawItem) bool {
			handled = append(handled, string(item.GetTxHash()))
			return false
		},
	}
	p.pollOnce(10)
	require.Len(t, handled, abandonQueryCount+1)
	require.Equal(t, "proposal-tail", handled[len(handled)-1])
	require.Equal(t, int64(11), p.nextHeight)
}

// Test_abandonProcessor_retryLifecycle 已受理的 burn 由处理器驱动解锁重试；重试返回 false
// （链上终态 / 已提交）后移出集合，不再重复解锁。
func Test_abandonProcessor_retryLifecycle(t *testing.T) {
	item := testAbandonItem()
	var retried []string
	keepRetrying := true
	p := &abandonProcessor{
		query: func(int64, int64, int64) (*rtypes.AbandonWithdrawList, error) {
			return &rtypes.AbandonWithdrawList{Items: []*rtypes.AbandonWithdrawItem{item}}, nil
		},
		handle: func(*rtypes.AbandonWithdrawItem) bool { return true },
		retry: func(burnTxHash []byte) bool {
			retried = append(retried, hex.EncodeToString(burnTxHash))
			return keepRetrying
		},
	}
	p.pollOnce(20)
	p.addRetry(item.GetBurnTxHash()) // duplicate registration must not double-retry
	require.Len(t, p.retrying, 1)

	p.retryPending()
	require.Equal(t, []string{hex.EncodeToString(item.GetBurnTxHash())}, retried)
	require.Len(t, p.retrying, 1)

	keepRetrying = false
	p.retryPending()
	require.Len(t, retried, 2)
	require.Empty(t, p.retrying, "finished burns must leave the retry set")
}

// Test_paymentSignatureMayExist 新记录与旧状态两条证据任一成立都必须拒绝放弃。
func Test_paymentSignatureMayExist(t *testing.T) {
	require.False(t, paymentSignatureMayExist(false, nil))
	require.True(t, paymentSignatureMayExist(true, nil), "durable record alone is enough")
	require.True(t, paymentSignatureMayExist(false, withdrawStatusSent), "legacy broadcasted state")
	require.True(t, paymentSignatureMayExist(false, withdrawStatusConfirmed), "legacy confirmed state")
	require.False(t, paymentSignatureMayExist(false, withdrawStatusUnlockPending))
	require.False(t, paymentSignatureMayExist(false, withdrawStatusAbandoning))
}

// Test_abandonWorker_recoveryState 重启恢复：本地 abandoning 记录被读回（解锁重试据此重建），
// 其它状态不进入集合；游标可持久化并读回。
func Test_abandonWorker_recoveryState(t *testing.T) {
	dir := t.TempDir()
	_, db, err := openWalletDB(dir, "abandon_recovery.db")
	if err != nil {
		t.Skipf("walletdb/bdb unavailable: %v", err)
	}
	defer db.Close()

	n := &neutrinoClient{}
	n.neutrinoCfg.Database = db
	burnA := bytes.Repeat([]byte{0x01}, 32)
	burnB := bytes.Repeat([]byte{0x02}, 32)
	burnC := bytes.Repeat([]byte{0x03}, 32)
	burnD := bytes.Repeat([]byte{0x04}, 32)
	require.NoError(t, n.setWithdrawState(burnA, withdrawStatusAbandoning))
	require.NoError(t, n.setWithdrawState(burnB, withdrawStatusAbandoning))
	require.NoError(t, n.setWithdrawState(burnC, withdrawStatusSent))
	require.NoError(t, n.setWithdrawState(burnD, withdrawStatusConfirmed))

	got := n.loadAbandoningBurns()
	require.Len(t, got, 2)
	require.ElementsMatch(t, [][]byte{burnA, burnB}, got)

	n.saveAbandonCursor(123, 7)
	height, index := n.loadAbandonCursor()
	require.Equal(t, int64(123), height)
	require.Equal(t, int64(7), index)
}

// failingSignedPaymentStore 注入读写失败。
type failingSignedPaymentStore struct{}

func (failingSignedPaymentStore) Has([]byte) (bool, error) {
	return false, errors.New("storage read failed")
}

func (failingSignedPaymentStore) PutOnce([]byte) error {
	return errors.New("storage write failed")
}

// Test_signedPayment_recordFailureBlocksRelease 写失败必须返回错误（调用方据此不返回/不广播签名）；
// 读失败必须按"已签名"处理（fail-closed），否则无法证明未签名时会错误放行解锁。
func Test_signedPayment_recordFailureBlocksRelease(t *testing.T) {
	n := &neutrinoClient{signedPayments: failingSignedPaymentStore{}}
	require.ErrorContains(t, n.recordSignedPayment([]byte("burn")), "storage write failed")
	require.True(t, n.hasSignedPayment([]byte("burn")), "failed durable write must still be remembered in-process")
	// A fresh process that cannot read the record must also fail closed.
	fresh := &neutrinoClient{signedPayments: failingSignedPaymentStore{}}
	require.True(t, fresh.hasSignedPayment([]byte("burn")), "unreadable record must be treated as signed")
}

// Test_signedPayment_persistAndReload 记录只写一次、永不清除，并且跨"重启"（重新打开数据库）可见。
func Test_signedPayment_persistAndReload(t *testing.T) {
	dir := t.TempDir()
	_, db, err := openWalletDB(dir, "signed_payment.db")
	if err != nil {
		t.Skipf("walletdb/bdb unavailable: %v", err)
	}
	burn := testAbandonBurn()

	n := &neutrinoClient{}
	n.neutrinoCfg.Database = db
	require.False(t, n.hasSignedPayment(burn))
	require.NoError(t, n.recordSignedPayment(burn))
	require.True(t, n.hasSignedPayment(burn))
	// Idempotent: writing the same burn again keeps the record and does not fail.
	require.NoError(t, n.recordSignedPayment(burn))
	require.True(t, n.hasSignedPayment(burn))
	require.False(t, n.hasSignedPayment(bytes.Repeat([]byte{0x55}, 32)))
	require.NoError(t, db.Close())

	_, db2, err := openWalletDB(dir, "signed_payment.db")
	require.NoError(t, err)
	defer db2.Close()
	reloaded := &neutrinoClient{}
	reloaded.neutrinoCfg.Database = db2
	require.True(t, reloaded.hasSignedPayment(burn), "signed payment record must survive a restart")
}
