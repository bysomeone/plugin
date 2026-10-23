package rgb20

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/33cn/chain33/common/log/log15"
	"github.com/stretchr/testify/require"
)

func newConsignmentTestAdapter(t *testing.T, cfg Config) *Adapter {
	t.Helper()
	if cfg.SidecarAddr == "" {
		cfg.SidecarAddr = "/tmp/nonexistent.sock"
	}
	a, err := NewAdapter(cfg, NewMemStore())
	require.NoError(t, err)
	t.Cleanup(a.Stop)
	return a
}

// Test_SaveWithdrawConsignment_Idempotent covers PutOnce semantics: the same bytes are accepted
// repeatedly, different bytes never overwrite the saved copy (the confirm hash must describe the
// bytes actually saved), and the hash is derived from those saved bytes.
func Test_SaveWithdrawConsignment_Idempotent(t *testing.T) {
	a := newConsignmentTestAdapter(t, Config{})
	burn := []byte("chain33-burn-hash")
	first := []byte("first-consignment-bytes")

	require.NoError(t, a.SaveWithdrawConsignment(burn, first))
	require.NoError(t, a.SaveWithdrawConsignment(burn, first), "same bytes are idempotent")

	saved, err := a.GetWithdrawConsignment(burn)
	require.NoError(t, err)
	require.Equal(t, first, saved)

	sum := sha256.Sum256(first)
	require.Equal(t, sum[:], a.WithdrawConsignmentHash(burn))

	// 不同的字节：拒绝覆盖，已保存的字节与哈希保持不变。
	require.Error(t, a.SaveWithdrawConsignment(burn, []byte("different-bytes")))
	saved, err = a.GetWithdrawConsignment(burn)
	require.NoError(t, err)
	require.Equal(t, first, saved)
	require.Equal(t, sum[:], a.WithdrawConsignmentHash(burn))

	// 没有记录的 burn：哈希为空（调用方据此提交不带哈希的确认）。
	require.Nil(t, a.WithdrawConsignmentHash([]byte("never-saved")))
	_, err = a.GetWithdrawConsignment([]byte("never-saved"))
	require.Error(t, err)

	// 空输入是错误（不能为"空 consignment"记一个 sha256）。
	require.Error(t, a.SaveWithdrawConsignment(nil, first))
	require.Error(t, a.SaveWithdrawConsignment(burn, nil))
}

// Test_IsRgb20WithdrawTxid distinguishes withdrawals (marker "withdraw") from deposit receives
// (value = receiveID): only withdrawals need a consignment hash in their Confirm.
func Test_IsRgb20WithdrawTxid(t *testing.T) {
	a := newConsignmentTestAdapter(t, Config{})
	require.False(t, a.IsRgb20WithdrawTxid(""))
	require.False(t, a.IsRgb20WithdrawTxid("unknown"))

	require.NoError(t, a.putKnownWithdrawTxid("withdraw-txid"))
	require.True(t, a.IsRgb20WithdrawTxid("withdraw-txid"))
	require.True(t, a.IsKnownRgbTxid("withdraw-txid"))

	// 充值收款 txid：已知 RGB 交易，但不是提现。
	require.NoError(t, a.store.Put(txidBucket, []byte("deposit-txid"), []byte("receive-1")))
	require.True(t, a.IsKnownRgbTxid("deposit-txid"))
	require.False(t, a.IsRgb20WithdrawTxid("deposit-txid"))
}

// Test_PublishConsignment_RetriesUntilSuccess uses a real HTTP server: two failures followed by a
// 2xx, and asserts the request contract (raw body, headers, bearer token).
func Test_PublishConsignment_RetriesUntilSuccess(t *testing.T) {
	var attempts int32
	var mu sync.Mutex
	var gotBody []byte
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		mu.Lock()
		gotBody = body
		gotHeaders = r.Header.Clone()
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	burn := []byte("burn-hash")
	consignment := []byte("raw consignment bytes")
	cfg := ConsignmentPublishConfig{URL: srv.URL, Token: "secret", TimeoutSeconds: 1}
	publishConsignmentWithRetry(context.Background(), srv.Client(), cfg, burn, consignment,
		5*time.Millisecond, 20*time.Millisecond)

	require.Equal(t, int32(3), atomic.LoadInt32(&attempts))
	sum := sha256.Sum256(consignment)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, consignment, gotBody)
	require.Equal(t, "application/octet-stream", gotHeaders.Get("Content-Type"))
	require.Equal(t, hex.EncodeToString(sum[:]), gotHeaders.Get("X-Consignment-Hash"))
	require.Equal(t, hex.EncodeToString(burn), gotHeaders.Get("X-Withdraw-Hash"))
	require.Equal(t, "Bearer secret", gotHeaders.Get("Authorization"))
}

// Test_PublishConsignment_StopsOnContextCancel pins the "never blocks settlement" half: a
// permanently failing endpoint must not spin forever; cancellation stops the retry loop.
func Test_PublishConsignment_StopsOnContextCancel(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		publishConsignmentWithRetry(ctx, srv.Client(),
			ConsignmentPublishConfig{URL: srv.URL, TimeoutSeconds: 1},
			[]byte("burn"), []byte("consignment"), 5*time.Millisecond, 20*time.Millisecond)
	}()
	// 等到至少发生两次尝试再取消（证明是"失败后重试"而不是一次就退）。
	require.Eventually(t, func() bool { return atomic.LoadInt32(&attempts) >= 2 },
		time.Second, 5*time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publish retry did not stop after context cancellation")
	}
}

// Test_PublishConsignmentAsync_UnconfiguredURLWarns confirms the local-only mode: no upload is
// started, and a WARN explains why. Local saving is independent and still works.
func Test_PublishConsignmentAsync_UnconfiguredURLWarns(t *testing.T) {
	a := newConsignmentTestAdapter(t, Config{})

	oldHandler := log.GetHandler()
	defer log.SetHandler(oldHandler)
	var mu sync.Mutex
	var warns []string
	log.SetHandler(log15.FuncHandler(int(log15.LvlDebug), func(r *log15.Record) error {
		mu.Lock()
		defer mu.Unlock()
		if r.Lvl == log15.LvlWarn {
			warns = append(warns, r.Msg)
		}
		return nil
	}))

	burn := []byte("burn")
	consignment := []byte("consignment")
	require.False(t, a.publishConsignmentAsync(burn, consignment),
		"an empty url must not start an upload")
	mu.Lock()
	require.Contains(t, warns, "rgb20 consignment publish is not configured; consignment kept locally only")
	mu.Unlock()

	// 本地保存不受上传配置影响。
	require.NoError(t, a.SaveWithdrawConsignment(burn, consignment))
	sum := sha256.Sum256(consignment)
	require.Equal(t, sum[:], a.WithdrawConsignmentHash(burn))
}
