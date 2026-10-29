package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func metricsTestConfig() func(*config) {
	return func(c *config) {
		c.chainTitle = testChainTitle
		c.bridgeToken = testBridgeTok
		c.eventsToken = "readonly-token-0123456789"
	}
}

func metricsBody(t *testing.T, nodeID string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"node_id":        nodeID,
		"timestamp":      time.Now().Unix(),
		"uptime_seconds": 123,
		"chain": map[string]any{
			"title":          "user.p.test.",
			"height":         42,
			"btc_header_tip": 7,
		},
		"sidecar":  map[string]any{"reachable": true},
		"breaker":  map[string]any{"tripped": false, "reason": ""},
		"counters": map[string]int64{"deposit.minted": 3, "withdraw.locked": 1},
		"gauges":   map[string]int64{"outbox_pending": 2},
	})
	require.NoError(t, err)
	return body
}

func doMetricsRequest(t *testing.T, ts *httptest.Server, method, path, token string, body []byte) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, bytes.NewReader(body))
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, data
}

func TestBridgeMetricsPushAndReadback(t *testing.T) {
	_, ts, _ := newTestServer(t, metricsTestConfig())

	resp, body := doMetricsRequest(t, ts, http.MethodPost, "/bridge/metrics", testBridgeTok, metricsBody(t, "node-a"))
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	var accepted struct {
		Accepted bool   `json:"accepted"`
		NodeID   string `json:"node_id"`
	}
	require.NoError(t, json.Unmarshal(body, &accepted))
	assert.True(t, accepted.Accepted)
	assert.Equal(t, "node-a", accepted.NodeID)

	// The read-only events token may read the latest snapshots...
	resp, body = doMetricsRequest(t, ts, http.MethodGet, "/bridge/metrics/latest", "readonly-token-0123456789", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	var latest struct {
		Nodes []map[string]any `json:"nodes"`
		Count int              `json:"count"`
	}
	require.NoError(t, json.Unmarshal(body, &latest))
	require.Equal(t, 1, latest.Count)
	require.Len(t, latest.Nodes, 1)
	assert.Equal(t, "node-a", latest.Nodes[0]["node_id"])
	assert.EqualValues(t, 42, latest.Nodes[0]["chain"].(map[string]any)["height"])
	assert.NotNil(t, latest.Nodes[0]["received_at"])

	// ...but it must not push.
	resp, _ = doMetricsRequest(t, ts, http.MethodPost, "/bridge/metrics", "readonly-token-0123456789", metricsBody(t, "node-b"))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	// The bridge token may also read.
	resp, _ = doMetricsRequest(t, ts, http.MethodGet, "/bridge/metrics/latest", testBridgeTok, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// No token at all is rejected.
	resp, _ = doMetricsRequest(t, ts, http.MethodGet, "/bridge/metrics/latest", "", nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp, _ = doMetricsRequest(t, ts, http.MethodPost, "/bridge/metrics", "", metricsBody(t, "node-b"))
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// A later snapshot for the same node replaces the old one.
	resp, _ = doMetricsRequest(t, ts, http.MethodPost, "/bridge/metrics", testBridgeTok, metricsBody(t, "node-a"))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_, body = doMetricsRequest(t, ts, http.MethodGet, "/bridge/metrics/latest", testBridgeTok, nil)
	require.NoError(t, json.Unmarshal(body, &latest))
	assert.Equal(t, 1, latest.Count, "a node's later snapshot must replace the earlier one, not add a row")
}

func TestBridgeMetricsValidation(t *testing.T) {
	_, ts, _ := newTestServer(t, metricsTestConfig())

	cases := []struct {
		name string
		body []byte
	}{
		{"empty node id", []byte(`{"node_id":"","timestamp":1}`)},
		{"whitespace node id", []byte(`{"node_id":"   "}`)},
		{"node id too long", []byte(`{"node_id":"` + strings.Repeat("a", 129) + `"}`)},
		{"not an object", []byte(`[1,2,3]`)},
		{"invalid json", []byte(`{`)},
		{"empty body", []byte(``)},
		{"too large", append([]byte(`{"node_id":"n","pad":"`), append(bytes.Repeat([]byte("x"), bridgeMetricsMaxBody), []byte(`"}`)...)...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := doMetricsRequest(t, ts, http.MethodPost, "/bridge/metrics", testBridgeTok, tc.body)
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "body: %s", body)
		})
	}

	// A 128-byte node id is accepted (the boundary).
	resp, _ := doMetricsRequest(t, ts, http.MethodPost, "/bridge/metrics", testBridgeTok,
		[]byte(`{"node_id":"`+strings.Repeat("a", 128)+`"}`))
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Only POST is allowed.
	resp, _ = doMetricsRequest(t, ts, http.MethodGet, "/bridge/metrics", testBridgeTok, nil)
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

func TestMetricsEndpointPrometheus(t *testing.T) {
	g, ts, _ := newTestServer(t, metricsTestConfig())

	// A fresh node and a stale node, plus a node id that needs label escaping.
	fresh := &bridgeMetricsSnapshot{
		NodeID:        "fresh",
		UptimeSeconds: 99,
		Chain:         bridgeMetricsChain{Title: "user.p.test.", Height: 100, BTCHeaderTip: 50},
		Sidecar:       bridgeMetricsSidecar{Reachable: true},
		Breaker:       bridgeMetricsBreaker{Tripped: true, Reason: "mismatch"},
		Counters:      map[string]int64{"deposit.minted": 4, "deposit.rejected": 1},
		Gauges:        bridgeMetricsGauges{OutboxPending: 2},
	}
	stale := &bridgeMetricsSnapshot{
		NodeID:        "stale",
		UptimeSeconds: 10,
		Chain:         bridgeMetricsChain{Title: "user.p.test.", Height: 90},
	}
	escaped := &bridgeMetricsSnapshot{NodeID: "a\"b\\c\nd"}

	g.metrics.put(fresh, time.Now())
	g.metrics.put(stale, time.Now().Add(-3*bridgeMetricsUpWindow))
	g.metrics.put(escaped, time.Now())

	resp, body := doMetricsRequest(t, ts, http.MethodGet, "/metrics", "", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, bridgeMetricsContentType, resp.Header.Get("Content-Type"))
	text := string(body)

	for _, want := range []string{
		"# HELP rgbx_bridge_up ",
		"# TYPE rgbx_bridge_up gauge",
		`rgbx_bridge_up{node="fresh"} 1`,
		`rgbx_bridge_up{node="stale"} 0`,
		`rgbx_bridge_uptime_seconds{node="fresh"} 99`,
		`rgbx_bridge_chain_height{node="fresh"} 100`,
		`rgbx_bridge_btc_header_tip{node="fresh"} 50`,
		`rgbx_bridge_breaker_tripped{node="fresh"} 1`,
		`rgbx_bridge_sidecar_reachable{node="fresh"} 1`,
		`rgbx_bridge_sidecar_reachable{node="stale"} 0`,
		`rgbx_bridge_events_total{node="fresh",type="deposit.minted"} 4`,
		`rgbx_bridge_events_total{node="fresh",type="deposit.rejected"} 1`,
		`rgbx_bridge_outbox_pending{node="fresh"} 2`,
		"# TYPE rgb_gateway_events_retained gauge",
		"# TYPE rgb_gateway_event_last_seq gauge",
		"# TYPE rgb_gateway_uptime_seconds gauge",
		"# TYPE rgb_gateway_data_dir_writable gauge",
		`rgb_gateway_data_dir_writable 1`,
		// Label escaping: a"b\c<newline>d.
		`rgbx_bridge_up{node="a\"b\\c\nd"} 1`,
	} {
		assert.Contains(t, text, want)
	}

	// Every sample line must be "name{labels} value" or "name value"; a stray blank line is fine
	// but a malformed value (e.g. NaN formatting) is not.
	for _, line := range strings.Split(text, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		require.Len(t, fields, 2, "malformed sample line %q", line)
	}
}

func TestHealthzReportsMetricsSeen(t *testing.T) {
	_, ts, _ := newTestServer(t, metricsTestConfig())

	healthz := func() map[string]any {
		resp, body := doMetricsRequest(t, ts, http.MethodGet, "/healthz", "", nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var got map[string]any
		require.NoError(t, json.Unmarshal(body, &got))
		return got
	}

	assert.Equal(t, false, healthz()["metrics"], "metrics must be false before any snapshot arrives")
	resp, body := doMetricsRequest(t, ts, http.MethodPost, "/bridge/metrics", testBridgeTok, metricsBody(t, "node-a"))
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	assert.Equal(t, true, healthz()["metrics"], "metrics must be true after a snapshot arrives")
}
