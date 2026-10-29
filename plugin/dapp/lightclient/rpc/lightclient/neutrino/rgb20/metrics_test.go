package rgb20

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeMetricsBridge overrides only the two methods the snapshot reads. Embedding the interface
// keeps the fake small; any other call would panic on the nil embedded value, which is exactly
// what a regression should do.
type fakeMetricsBridge struct {
	Chain33Bridge
	height int64
	tip    uint64
	tipErr error
}

func (f *fakeMetricsBridge) GetMainChainHeight() int64 { return f.height }
func (f *fakeMetricsBridge) BtcBestHeight() (uint64, error) {
	return f.tip, f.tipErr
}

func newMetricsTestAdapter(t *testing.T, cfg Config) *Adapter {
	t.Helper()
	if cfg.SidecarAddr == "" {
		cfg.SidecarAddr = "127.0.0.1:1"
	}
	a, err := NewAdapter(cfg, NewMemStore())
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	return a
}

func TestMetricsCountersFollowRecordEvent(t *testing.T) {
	a := newMetricsTestAdapter(t, Config{})

	a.RecordEvent(metricDepositMinted, "r1", nil)
	a.RecordEvent(metricDepositMinted, "r2", nil)
	a.RecordEvent(metricWithdrawLocked, "w1", nil)
	a.RecordEvent(metricPayoutMismatch, "m1", nil)
	a.RecordEvent("not.a.counter", "x", nil)

	got := a.metrics.snapshot()
	if got[metricDepositMinted] != 2 {
		t.Fatalf("deposit.minted = %d, want 2", got[metricDepositMinted])
	}
	if got[metricWithdrawLocked] != 1 {
		t.Fatalf("withdraw.locked = %d, want 1", got[metricWithdrawLocked])
	}
	if got[metricPayoutMismatch] != 1 {
		t.Fatalf("payout_mismatch = %d, want 1", got[metricPayoutMismatch])
	}
	// Every documented counter is present, including zeros.
	for _, name := range bridgeMetricCounterNames {
		if _, ok := got[name]; !ok {
			t.Fatalf("counter %q missing from snapshot", name)
		}
	}
	if _, ok := got["not.a.counter"]; ok {
		t.Fatal("unknown event kind leaked into the fixed counter schema")
	}
}

func TestMetricsSnapshotFields(t *testing.T) {
	a := newMetricsTestAdapter(t, Config{
		NodeID:     "node-1",
		ChainTitle: "user.p.test.",
	})
	a.startedAt = time.Now().Add(-2 * time.Minute)
	a.SetBridge(&fakeMetricsBridge{height: 1234, tip: 99})
	a.breaker.recordMismatch("burn-1") // threshold 3 by default, so force the tripped state
	a.breaker.tripped = true
	a.metrics.inc(metricDepositSettled)

	snap := a.metricsSnapshot()
	if snap.NodeID != "node-1" {
		t.Fatalf("node_id = %q", snap.NodeID)
	}
	if snap.Timestamp == 0 {
		t.Fatal("timestamp is zero")
	}
	if snap.UptimeSeconds < 119 || snap.UptimeSeconds > 121 {
		t.Fatalf("uptime_seconds = %d, want ~120", snap.UptimeSeconds)
	}
	if snap.Chain.Title != "user.p.test." || snap.Chain.Height != 1234 || snap.Chain.BTCHeaderTip != 99 {
		t.Fatalf("chain = %+v", snap.Chain)
	}
	if snap.Sidecar.Reachable {
		t.Fatal("sidecar reported reachable without a connection")
	}
	if !snap.Breaker.Tripped || snap.Breaker.Reason == "" {
		t.Fatalf("breaker = %+v, want tripped with a reason", snap.Breaker)
	}
	if snap.Counters[metricDepositSettled] != 1 {
		t.Fatalf("deposit.settled = %d", snap.Counters[metricDepositSettled])
	}

	// A missing bridge degrades heights to zero instead of failing.
	a.SetBridge(nil)
	plain := a.metricsSnapshot()
	if plain.Chain.Height != 0 || plain.Chain.BTCHeaderTip != 0 {
		t.Fatalf("chain heights without a bridge = %+v, want zeros", plain.Chain)
	}

	// An unavailable BTC tip also degrades to zero.
	a.SetBridge(&fakeMetricsBridge{height: 7, tipErr: errors.New("not synced")})
	noTip := a.metricsSnapshot()
	if noTip.Chain.Height != 7 || noTip.Chain.BTCHeaderTip != 0 {
		t.Fatalf("chain = %+v, want height 7 and tip 0", noTip.Chain)
	}
}

func TestMetricsSnapshotJSONContract(t *testing.T) {
	a := newMetricsTestAdapter(t, Config{NodeID: "node-1", ChainTitle: "user.p.test."})
	body, err := json.Marshal(a.metricsSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"node_id", "timestamp", "uptime_seconds", "chain", "sidecar",
		"breaker", "counters", "gauges"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("snapshot is missing %q: %s", key, body)
		}
	}
}

func TestMetricsPusherPostsToGateway(t *testing.T) {
	type request struct {
		path string
		auth string
		body []byte
	}
	got := make(chan request, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- request{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: body}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"accepted":true}`)
	}))
	defer srv.Close()

	a := newMetricsTestAdapter(t, Config{
		TransportGateway:       srv.URL + "/0.2/json-rpc",
		GatewayToken:           "token-0123456789",
		GatewayMetricsInterval: 20 * time.Millisecond,
		NodeID:                 "node-x",
		ChainTitle:             "user.p.test.",
	})
	a.metrics.inc(metricDepositMinted)
	a.wg.Add(1)
	go a.runMetricsPusher(a.cfg.GatewayMetricsInterval)

	select {
	case req := <-got:
		if req.path != "/bridge/metrics" {
			t.Fatalf("path = %q, want /bridge/metrics", req.path)
		}
		if req.auth != "Bearer token-0123456789" {
			t.Fatalf("authorization = %q", req.auth)
		}
		var snap bridgeMetricsSnapshot
		if err := json.Unmarshal(req.body, &snap); err != nil {
			t.Fatalf("body is not a snapshot: %v (%s)", err, req.body)
		}
		if snap.NodeID != "node-x" {
			t.Fatalf("node_id = %q", snap.NodeID)
		}
		if snap.Counters[metricDepositMinted] != 1 {
			t.Fatalf("deposit.minted = %d", snap.Counters[metricDepositMinted])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the pusher never posted a snapshot")
	}
	a.Stop()
}

func TestMetricsPushFailureDoesNotPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	a := newMetricsTestAdapter(t, Config{
		TransportGateway: srv.URL,
		GatewayToken:     "token-0123456789",
		NodeID:           "node-x",
	})
	// A refused push is logged and swallowed; the assertion is that this returns at all.
	a.pushMetrics(context.Background(), srv.URL+"/bridge/metrics", srv.Client())
}

func TestEventRecorderPendingCount(t *testing.T) {
	store := NewMemStore()
	r := &eventRecorder{store: store}
	if got := r.pending(); got != 0 {
		t.Fatalf("pending = %d, want 0", got)
	}
	for _, key := range []string{"a", "b", "c"} {
		if err := store.Put([]byte(bridgeEventOutboxBucket), []byte(key), []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.pending(); got != 3 {
		t.Fatalf("pending = %d, want 3", got)
	}
	var nilRecorder *eventRecorder
	if got := nilRecorder.pending(); got != 0 {
		t.Fatalf("nil recorder pending = %d", got)
	}
}
