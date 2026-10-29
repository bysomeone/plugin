package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Bridge metrics aggregation (#85).
//
// Each bridge pushes a small runtime snapshot to POST /bridge/metrics; the gateway keeps only
// the latest snapshot per node, in memory. Nothing is persisted: a gateway restart clears the
// view and the next push fills it again. GET /metrics renders the current view in Prometheus
// text exposition format so an operator's scraper can alert on bridge state without giving the
// bridge an inbound port.
//
// The snapshot schema is fixed by the bridge (rgb20/metrics.go). The gateway validates only
// what it needs to key and bound the data: a non-empty node_id of at most 128 bytes, a JSON
// object, and a body no larger than 64 KiB.

const (
	bridgeMetricsMaxBody     = 64 << 10
	bridgeMetricsMaxNodeID   = 128
	bridgeMetricsUpWindow    = 120 * time.Second
	bridgeMetricsContentType = "text/plain; version=0.0.4; charset=utf-8"
)

// bridgeMetricsChain mirrors the bridge's chain section.
type bridgeMetricsChain struct {
	Title        string `json:"title"`
	Height       int64  `json:"height"`
	BTCHeaderTip int64  `json:"btc_header_tip"`
}

// bridgeMetricsSidecar mirrors the bridge's sidecar section.
type bridgeMetricsSidecar struct {
	Reachable bool `json:"reachable"`
}

// bridgeMetricsBreaker mirrors the bridge's breaker section.
type bridgeMetricsBreaker struct {
	Tripped bool   `json:"tripped"`
	Reason  string `json:"reason"`
}

// bridgeMetricsGauges mirrors the bridge's gauge section.
type bridgeMetricsGauges struct {
	OutboxPending int64 `json:"outbox_pending"`
}

// bridgeMetricsSnapshot is the accepted POST body and the stored value.
type bridgeMetricsSnapshot struct {
	NodeID        string               `json:"node_id"`
	Timestamp     int64                `json:"timestamp"`
	UptimeSeconds int64                `json:"uptime_seconds"`
	Chain         bridgeMetricsChain   `json:"chain"`
	Sidecar       bridgeMetricsSidecar `json:"sidecar"`
	Breaker       bridgeMetricsBreaker `json:"breaker"`
	Counters      map[string]int64     `json:"counters"`
	Gauges        bridgeMetricsGauges  `json:"gauges"`
}

// storedMetrics is a snapshot plus the gateway-side receive time. Embedding keeps the JSON
// shape flat: {"node_id":..., ..., "received_at":...}.
type storedMetrics struct {
	bridgeMetricsSnapshot
	ReceivedAt int64 `json:"received_at"`
}

// metricsStore keeps the latest snapshot per node_id. All access is under mu; the data set is
// tiny (one entry per bridge node).
type metricsStore struct {
	mu       sync.Mutex
	nodes    map[string]*storedMetrics
	received bool
}

func newMetricsStore() *metricsStore {
	return &metricsStore{nodes: make(map[string]*storedMetrics)}
}

// put replaces the latest snapshot for its node and records when it arrived.
func (s *metricsStore) put(snap *bridgeMetricsSnapshot, now time.Time) {
	if s == nil || snap == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes[snap.NodeID] = &storedMetrics{
		bridgeMetricsSnapshot: *snap,
		ReceivedAt:            now.Unix(),
	}
	s.received = true
}

// list returns a copy of the stored snapshots, sorted by node id for deterministic output.
func (s *metricsStore) list() []storedMetrics {
	if s == nil {
		return []storedMetrics{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]storedMetrics, 0, len(s.nodes))
	for _, n := range s.nodes {
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

// receivedAny reports whether any snapshot has arrived since this process started. It feeds
// the healthz "metrics" field only; freshness is exposed by rgbx_bridge_up.
func (s *metricsStore) receivedAny() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.received
}

// decodeBridgeMetrics reads at most 64 KiB and requires a JSON object. json.Unmarshal into a
// struct would also reject arrays, but an explicit object check keeps the contract (and the
// error message) independent of struct shape.
func decodeBridgeMetrics(r *http.Request, out *bridgeMetricsSnapshot) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, bridgeMetricsMaxBody+1))
	if err != nil {
		return err
	}
	if len(body) > bridgeMetricsMaxBody {
		return errors.New("body too large")
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("payload must be a JSON object")
	}
	return json.Unmarshal(trimmed, out)
}

// handleBridgeMetricsPost stores the latest snapshot from one bridge.
func (g *gateway) handleBridgeMetricsPost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if g.metrics == nil {
		http.Error(w, "metrics aggregation is not configured", http.StatusServiceUnavailable)
		return
	}
	snap := &bridgeMetricsSnapshot{}
	if err := decodeBridgeMetrics(r, snap); err != nil {
		http.Error(w, "invalid metrics snapshot: "+err.Error(), http.StatusBadRequest)
		return
	}
	snap.NodeID = strings.TrimSpace(snap.NodeID)
	if snap.NodeID == "" || len(snap.NodeID) > bridgeMetricsMaxNodeID {
		http.Error(w, "node_id must be 1..128 bytes", http.StatusBadRequest)
		return
	}
	g.metrics.put(snap, time.Now())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true, "node_id": snap.NodeID})
}

// handleBridgeMetricsLatest returns every stored snapshot. The bridge token or the read-only
// events token may read it (see requireEventReadAuth).
func (g *gateway) handleBridgeMetricsLatest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if g.metrics == nil {
		http.Error(w, "metrics aggregation is not configured", http.StatusServiceUnavailable)
		return
	}
	nodes := g.metrics.list()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"nodes": nodes, "count": len(nodes)})
}

// handleMetrics renders the aggregated view as Prometheus text exposition. The endpoint is
// unauthenticated by design: it exposes only aggregate operational state, and the gateway is
// expected to listen on loopback or a private network (see README).
func (g *gateway) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", bridgeMetricsContentType)
	w.Header().Set("Cache-Control", "no-store")

	now := time.Now()
	nodes := g.metrics.list()
	var b strings.Builder

	writePromFamily(&b, "rgbx_bridge_up", "gauge",
		"Whether the bridge published a snapshot within the last 120 seconds (1) or not (0).")
	for _, n := range nodes {
		age := now.Unix() - n.ReceivedAt
		up := float64(0)
		if age <= int64(bridgeMetricsUpWindow/time.Second) {
			up = 1
		}
		writePromSample(&b, "rgbx_bridge_up", []promLabel{{"node", n.NodeID}}, up)
	}

	writePromFamily(&b, "rgbx_bridge_snapshot_age_seconds", "gauge",
		"Seconds since the gateway received this node's latest snapshot.")
	for _, n := range nodes {
		age := now.Unix() - n.ReceivedAt
		writePromSample(&b, "rgbx_bridge_snapshot_age_seconds",
			[]promLabel{{"node", n.NodeID}}, float64(age))
	}

	writePromFamily(&b, "rgbx_bridge_uptime_seconds", "gauge",
		"Seconds since the bridge adapter started (process lifetime).")
	for _, n := range nodes {
		writePromSample(&b, "rgbx_bridge_uptime_seconds",
			[]promLabel{{"node", n.NodeID}}, float64(n.UptimeSeconds))
	}

	writePromFamily(&b, "rgbx_bridge_chain_height", "gauge",
		"Latest main-chain height reported by the bridge (0 when unavailable).")
	for _, n := range nodes {
		writePromSample(&b, "rgbx_bridge_chain_height",
			[]promLabel{{"node", n.NodeID}}, float64(n.Chain.Height))
	}

	writePromFamily(&b, "rgbx_bridge_btc_header_tip", "gauge",
		"Bridge's BTC header chain tip (0 when unavailable).")
	for _, n := range nodes {
		writePromSample(&b, "rgbx_bridge_btc_header_tip",
			[]promLabel{{"node", n.NodeID}}, float64(n.Chain.BTCHeaderTip))
	}

	writePromFamily(&b, "rgbx_bridge_breaker_tripped", "gauge",
		"Withdrawal circuit breaker state (1 = tripped, no new withdrawals signed).")
	for _, n := range nodes {
		value := float64(0)
		if n.Breaker.Tripped {
			value = 1
		}
		writePromSample(&b, "rgbx_bridge_breaker_tripped",
			[]promLabel{{"node", n.NodeID}}, value)
	}

	writePromFamily(&b, "rgbx_bridge_sidecar_reachable", "gauge",
		"Whether the bridge's RGB sidecar connection is established (1) or not (0).")
	for _, n := range nodes {
		value := float64(0)
		if n.Sidecar.Reachable {
			value = 1
		}
		writePromSample(&b, "rgbx_bridge_sidecar_reachable",
			[]promLabel{{"node", n.NodeID}}, value)
	}

	writePromFamily(&b, "rgbx_bridge_events_total", "counter",
		"Bridge audit events recorded since process start, by event type.")
	for _, n := range nodes {
		types := make([]string, 0, len(n.Counters))
		for typ := range n.Counters {
			types = append(types, typ)
		}
		sort.Strings(types)
		for _, typ := range types {
			writePromSample(&b, "rgbx_bridge_events_total",
				[]promLabel{{"node", n.NodeID}, {"type", typ}}, float64(n.Counters[typ]))
		}
	}

	writePromFamily(&b, "rgbx_bridge_outbox_pending", "gauge",
		"Events waiting in the bridge's local outbox for delivery to the gateway.")
	for _, n := range nodes {
		writePromSample(&b, "rgbx_bridge_outbox_pending",
			[]promLabel{{"node", n.NodeID}}, float64(n.Gauges.OutboxPending))
	}

	count, lastSeq := int64(0), int64(0)
	if g.events != nil {
		count, lastSeq = g.events.stats()
	}
	// Retained count decreases when the ledger is pruned, so it is a gauge rather than a counter.
	writePromFamily(&b, "rgb_gateway_events_retained", "gauge",
		"Events currently retained in the gateway's audit ledger.")
	writePromSample(&b, "rgb_gateway_events_retained", nil, float64(count))

	writePromFamily(&b, "rgb_gateway_event_last_seq", "gauge",
		"Highest sequence number assigned by the gateway's audit ledger.")
	writePromSample(&b, "rgb_gateway_event_last_seq", nil, float64(lastSeq))

	writePromFamily(&b, "rgb_gateway_uptime_seconds", "gauge",
		"Seconds since the gateway process started.")
	writePromSample(&b, "rgb_gateway_uptime_seconds", nil, time.Since(g.started).Seconds())

	writable := float64(0)
	if g.store != nil && dataDirWritable(g.store.dir) {
		writable = 1
	}
	writePromFamily(&b, "rgb_gateway_data_dir_writable", "gauge",
		"Whether the gateway data directory accepts writes (1) or not (0).")
	writePromSample(&b, "rgb_gateway_data_dir_writable", nil, writable)

	_, _ = io.WriteString(w, b.String())
}

// promLabel is one sample label. Labels are written in sorted order for deterministic output.
type promLabel struct {
	Name  string
	Value string
}

func writePromFamily(b *strings.Builder, name, typ, help string) {
	b.WriteString("# HELP ")
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(help)
	b.WriteByte('\n')
	b.WriteString("# TYPE ")
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(typ)
	b.WriteByte('\n')
}

func writePromSample(b *strings.Builder, name string, labels []promLabel, value float64) {
	b.WriteString(name)
	if len(labels) > 0 {
		sorted := append([]promLabel(nil), labels...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
		b.WriteByte('{')
		for i, l := range sorted {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(l.Name)
			b.WriteString(`="`)
			b.WriteString(promEscapeLabel(l.Value))
			b.WriteByte('"')
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(strconv.FormatFloat(value, 'g', -1, 64))
	b.WriteByte('\n')
}

// promEscapeLabel escapes a label value according to the Prometheus exposition format:
// backslash, double quote and line feed.
func promEscapeLabel(v string) string {
	return strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"\n", `\n`,
	).Replace(v)
}
