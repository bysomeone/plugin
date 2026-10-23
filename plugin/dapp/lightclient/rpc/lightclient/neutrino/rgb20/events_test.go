package rgb20

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestEventRecorderPostsAndClearsOutbox(t *testing.T) {
	var (
		mu     sync.Mutex
		events []bridgeEventWire
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bridge/events" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer token-0123456789" {
			t.Errorf("missing bearer token")
		}
		ev := bridgeEventWire{}
		if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
			t.Errorf("decode: %v", err)
		}
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store := newMemStore()
	cfg := Config{TransportGateway: srv.URL + "/0.2/json-rpc", GatewayToken: "token-0123456789"}
	r := newEventRecorder(cfg, store)
	if r == nil {
		t.Fatal("recorder is nil with gateway configured")
	}
	defer r.stop()

	r.record("withdraw.locked", "abc", map[string]any{"chain33_tx_hash": "abc", "amount": 7})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(events)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	if events[0].Type != "withdraw.locked" || events[0].ID != bridgeEventID("withdraw.locked", "abc") {
		t.Fatalf("unexpected event %+v", events[0])
	}
	// The outbox entry is deleted after the gateway acknowledges it.
	if err := store.ForEach([]byte(bridgeEventOutboxBucket), func(k, v []byte) error {
		t.Errorf("outbox still holds %s", k)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestEventRecorderKeepsEventWhenGatewayFails(t *testing.T) {
	var (
		mu       sync.Mutex
		attempts int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	store := newMemStore()
	r := newEventRecorder(Config{TransportGateway: srv.URL, GatewayToken: "token-0123456789"}, store)
	if r == nil {
		t.Fatal("recorder is nil")
	}
	defer r.stop()
	r.record("deposit.minted", "r1", map[string]any{"receive_id": "r1"})

	// Wait until the first attempt has failed; the entry must still be in the outbox.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := attempts
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	n := attempts
	mu.Unlock()
	if n == 0 {
		t.Fatal("the recorder never attempted delivery")
	}
	kept := false
	if err := store.ForEach([]byte(bridgeEventOutboxBucket), func(k, v []byte) error {
		kept = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !kept {
		t.Fatal("event was dropped after a retryable failure")
	}
}

func TestEventRecorderDisabledWithoutGateway(t *testing.T) {
	if r := newEventRecorder(Config{}, newMemStore()); r != nil {
		t.Fatal("recorder must be nil without a gateway")
	}
	// A nil recorder is a safe no-op.
	var a *Adapter
	a.RecordEvent("deposit.minted", "x", nil)
}
