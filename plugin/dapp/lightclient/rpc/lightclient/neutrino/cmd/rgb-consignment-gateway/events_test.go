package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newEventTestGateway(t *testing.T) (*httptest.Server, *gateway) {
	t.Helper()
	g, err := newGateway(config{
		listen:      "127.0.0.1:0",
		dataDir:     t.TempDir(),
		path:        "/0.2/json-rpc",
		maxSize:     1 << 20,
		ttl:         time.Hour,
		rateLimit:   0,
		chainTitle:  testChainTitle,
		bridgeToken: testBridgeTok,
	}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("newGateway: %v", err)
	}
	ts := httptest.NewServer(g.handler())
	t.Cleanup(ts.Close)
	return ts, g
}

func postEvent(t *testing.T, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/bridge/events", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testBridgeTok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func queryEvents(t *testing.T, url, query string) map[string]any {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url+"/bridge/events/query"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testBridgeTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("query status %d: %s", resp.StatusCode, body)
	}
	out := map[string]any{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestBridgeEventPostIsIdempotentAndOrdered(t *testing.T) {
	ts, _ := newEventTestGateway(t)
	first := `{"id":"a","type":"withdraw.broadcast","payload":{"btc_txid":"ff"}}`
	resp := postEvent(t, ts.URL, first)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first post status %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Same id with different content must not create a second row.
	resp = postEvent(t, ts.URL, `{"id":"a","type":"withdraw.broadcast","payload":{"btc_txid":"00"}}`)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Event-Duplicate") != "true" {
		t.Fatalf("duplicate post status %d duplicate=%q", resp.StatusCode, resp.Header.Get("X-Event-Duplicate"))
	}
	resp.Body.Close()

	resp = postEvent(t, ts.URL, `{"id":"b","type":"sweep.completed","payload":{}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second post status %d", resp.StatusCode)
	}
	resp.Body.Close()

	got := queryEvents(t, ts.URL, "")
	events, _ := got["events"].([]any)
	if len(events) != 2 {
		t.Fatalf("want 2 events, got %d (%v)", len(events), got)
	}
	firstEvent, _ := events[0].(map[string]any)
	if firstEvent["id"] != "a" {
		t.Fatalf("unexpected first event %v", firstEvent)
	}
	payload, _ := firstEvent["payload"].(map[string]any)
	if payload["btc_txid"] != "ff" {
		t.Fatalf("duplicate overwrote the original payload: %v", payload)
	}
	next, _ := got["next_cursor"].(float64)
	if int64(next) != 2 {
		t.Fatalf("next_cursor = %v, want 2", got["next_cursor"])
	}

	// Cursor returns only what came after it.
	got = queryEvents(t, ts.URL, "?cursor=1")
	events, _ = got["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("want 1 event after cursor, got %d", len(events))
	}
	second, _ := events[0].(map[string]any)
	if second["id"] != "b" {
		t.Fatalf("unexpected event after cursor: %v", second)
	}
}

func TestBridgeEventValidation(t *testing.T) {
	ts, _ := newEventTestGateway(t)
	cases := []struct {
		name string
		body string
	}{
		{"unknown type", `{"id":"x","type":"deposit.bogus"}`},
		{"missing id", `{"type":"deposit.settled"}`},
		{"payload not an object", `{"id":"x","type":"deposit.settled","payload":[1,2]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := postEvent(t, ts.URL, tc.body)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", resp.StatusCode)
			}
		})
	}
	// The bridge token is required.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/bridge/events",
		bytes.NewBufferString(`{"id":"x","type":"deposit.settled"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status %d, want 401", resp.StatusCode)
	}
}

func TestEventReadTokenIsReadOnly(t *testing.T) {
	g, err := newGateway(config{
		listen:      "127.0.0.1:0",
		dataDir:     t.TempDir(),
		path:        "/0.2/json-rpc",
		maxSize:     1 << 20,
		ttl:         time.Hour,
		rateLimit:   0,
		chainTitle:  testChainTitle,
		bridgeToken: testBridgeTok,
		eventsToken: "readonly-token-0123456789",
	}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("newGateway: %v", err)
	}
	ts := httptest.NewServer(g.handler())
	t.Cleanup(ts.Close)

	do := func(method, path, token, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, ts.URL+path, bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// The read-only token may query (empty ledger is fine)...
	resp := do(http.MethodGet, "/bridge/events?limit=1", "readonly-token-0123456789", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read-only GET status %d", resp.StatusCode)
	}
	resp.Body.Close()
	// ...but must not append.
	resp = do(http.MethodPost, "/bridge/events", "readonly-token-0123456789",
		`{"id":"x","type":"sweep.completed","payload":{}}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("read-only POST status %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()
	// The bridge token keeps full access.
	resp = do(http.MethodPost, "/bridge/events", testBridgeTok,
		`{"id":"x","type":"sweep.completed","payload":{}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bridge POST status %d", resp.StatusCode)
	}
	resp.Body.Close()
	// An unknown token is rejected.
	resp = do(http.MethodGet, "/bridge/events", "wrong-token-0123456789", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown token status %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestEventStorePrunesExpiredEventsAndKeepsCursorMonotonic(t *testing.T) {
	dir := t.TempDir()
	st, err := openEventStore(dir, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	now := time.Now()
	if _, _, err := st.append(&bridgeEvent{ID: "old", Type: "deposit.settled"}, now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.append(&bridgeEvent{ID: "new", Type: "deposit.settled"}, now); err != nil {
		t.Fatal(err)
	}
	if n, err := st.prune(now); err != nil || n != 1 {
		t.Fatalf("prune = %d, %v; want 1, nil", n, err)
	}
	got := st.since(0, 10)
	if len(got) != 1 || got[0].ID != "new" {
		t.Fatalf("unexpected events after prune: %+v", got)
	}
	// Sequence numbers are never reused, so an operator's cursor stays valid.
	if _, _, err := st.append(&bridgeEvent{ID: "newer", Type: "deposit.settled"}, now); err != nil {
		t.Fatal(err)
	}
	if got := st.since(2, 10); len(got) != 1 || got[0].Seq != 3 {
		t.Fatalf("cursor invariant broken: %+v", got)
	}
	// Reopening keeps the stored order and ids.
	if err := st.close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openEventStore(dir, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	got = reopened.since(0, 10)
	if len(got) != 2 || got[0].ID != "new" || got[1].ID != "newer" {
		t.Fatalf("reopen lost events: %+v", got)
	}
}
