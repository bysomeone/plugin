package rgb20

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Bridge runtime metrics (#85).
//
// The gateway cannot see inside the bridge process, and the chain itself does not carry
// counters such as "how many deposits settled since this process started" or "is the payout
// breaker open". The bridge therefore pushes a small snapshot to the gateway, which aggregates
// the latest snapshot per node and exposes it in Prometheus text format.
//
// The counters are process-lifetime totals: they start at zero on every restart and are never
// persisted. That is intentional — a restart resets a node's view, and the gateway (which is
// restarted independently) keeps only the latest snapshot in memory. Long-horizon history
// belongs to Prometheus, which scrapes the gateway's /metrics endpoint.
//
// Pushing is best effort and never touches the money path: a dedicated goroutine reads the
// counters and posts them, so an unreachable gateway only produces a WARN per interval.

const (
	// defaultGatewayMetricsInterval is used when the configuration key is absent.
	// A configured zero or negative duration disables pushing instead.
	defaultGatewayMetricsInterval = 30 * time.Second
	// gatewayMetricsPostTimeout bounds one push. It is shorter than the interval so a hung
	// gateway never overlaps the next tick.
	gatewayMetricsPostTimeout = 5 * time.Second
)

// Counter names. They are also the event types recorded by RecordEvent, except for
// payout_mismatch, which has no ledger event of its own (the withdrawal is refused, not settled).
const (
	metricDepositSettled      = "deposit.settled"
	metricDepositMinted       = "deposit.minted"
	metricDepositRejected     = "deposit.rejected"
	metricWithdrawLocked      = "withdraw.locked"
	metricWithdrawBroadcast   = "withdraw.broadcast"
	metricWithdrawConsignment = "withdraw.consignment"
	metricWithdrawSettled     = "withdraw.settled"
	metricWithdrawUnlocked    = "withdraw.unlocked"
	metricWithdrawBumped      = "withdraw.bumped"
	metricSweepCompleted      = "sweep.completed"
	metricPayoutMismatch      = "payout_mismatch"
)

// bridgeMetricCounterNames is the fixed counter set reported in every snapshot. It is listed
// explicitly (rather than derived from observed events) so the gateway always emits a complete
// series, including zeros.
var bridgeMetricCounterNames = []string{
	metricDepositSettled,
	metricDepositMinted,
	metricDepositRejected,
	metricWithdrawLocked,
	metricWithdrawBroadcast,
	metricWithdrawConsignment,
	metricWithdrawSettled,
	metricWithdrawUnlocked,
	metricWithdrawBumped,
	metricSweepCompleted,
	metricPayoutMismatch,
}

// bridgeMetricCounters holds process-lifetime event counters. The map is built once and never
// mutated, so concurrent readers need no lock; the values are atomics.
type bridgeMetricCounters struct {
	values map[string]*atomic.Int64
}

func newBridgeMetricCounters() *bridgeMetricCounters {
	c := &bridgeMetricCounters{values: make(map[string]*atomic.Int64, len(bridgeMetricCounterNames))}
	for _, name := range bridgeMetricCounterNames {
		c.values[name] = new(atomic.Int64)
	}
	return c
}

// inc counts one recorded event. Unknown kinds are ignored: the snapshot has a fixed schema.
func (c *bridgeMetricCounters) inc(kind string) {
	if c == nil {
		return
	}
	if v, ok := c.values[kind]; ok {
		v.Add(1)
	}
}

// snapshot returns a copy of every counter, including zeros.
func (c *bridgeMetricCounters) snapshot() map[string]int64 {
	out := make(map[string]int64, len(bridgeMetricCounterNames))
	if c == nil {
		for _, name := range bridgeMetricCounterNames {
			out[name] = 0
		}
		return out
	}
	for _, name := range bridgeMetricCounterNames {
		out[name] = c.values[name].Load()
	}
	return out
}

// bridgeMetricsChain is the chain section of the snapshot.
type bridgeMetricsChain struct {
	Title        string `json:"title"`
	Height       int64  `json:"height"`
	BTCHeaderTip int64  `json:"btc_header_tip"`
}

// bridgeMetricsSidecar is the sidecar section of the snapshot.
type bridgeMetricsSidecar struct {
	Reachable bool `json:"reachable"`
}

// bridgeMetricsBreaker is the withdrawal breaker section of the snapshot.
type bridgeMetricsBreaker struct {
	Tripped bool   `json:"tripped"`
	Reason  string `json:"reason"`
}

// bridgeMetricsGauges is the gauge section of the snapshot.
type bridgeMetricsGauges struct {
	OutboxPending int64 `json:"outbox_pending"`
}

// bridgeMetricsSnapshot is the wire contract posted to POST {gateway}/bridge/metrics.
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

// metricsSnapshot builds one point-in-time snapshot from local state. Every source is
// best-effort: a missing bridge or an unavailable height degrades to zero, never to an error.
func (a *Adapter) metricsSnapshot() *bridgeMetricsSnapshot {
	if a == nil {
		return nil
	}
	now := time.Now()
	nodeID := a.cfg.NodeID
	if nodeID == "" {
		nodeID = a.cfg.ChainTitle
	}
	if nodeID == "" {
		nodeID = "rgb-bridge"
	}
	snap := &bridgeMetricsSnapshot{
		NodeID:        nodeID,
		Timestamp:     now.Unix(),
		UptimeSeconds: int64(now.Sub(a.startedAt) / time.Second),
		Chain:         bridgeMetricsChain{Title: a.cfg.ChainTitle},
		Counters:      a.metrics.snapshot(),
	}
	if a.bridge != nil {
		snap.Chain.Height = a.bridge.GetMainChainHeight()
		if tip, err := a.bridge.BtcBestHeight(); err == nil {
			snap.Chain.BTCHeaderTip = int64(tip)
		}
	}
	snap.Sidecar.Reachable = a.IsConnected()
	snap.Breaker.Tripped = a.breaker.isTripped()
	snap.Breaker.Reason = a.breaker.tripReason()
	if a.events != nil {
		snap.Gauges.OutboxPending = a.events.pending()
	}
	return snap
}

// runMetricsPusher posts a snapshot every interval until the adapter stops. It logs failures at
// WARN and retries on the next tick; it never retries in a tight loop.
func (a *Adapter) runMetricsPusher(interval time.Duration) {
	defer a.wg.Done()
	base, err := gatewayBaseURL(a.cfg.TransportGateway)
	if err != nil {
		log.Error("rgb20 metrics pusher disabled: invalid transportGateway", "err", err)
		return
	}
	endpoint := base + "/bridge/metrics"
	client := &http.Client{Timeout: gatewayMetricsPostTimeout}
	log.Info("rgb20 metrics pusher started", "url", endpoint, "interval", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		a.pushMetrics(a.ctx, endpoint, client)
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// pushMetrics posts one snapshot. Any failure is logged and swallowed: metrics must never
// influence deposit or withdrawal processing.
func (a *Adapter) pushMetrics(ctx context.Context, endpoint string, client *http.Client) {
	snap := a.metricsSnapshot()
	if snap == nil {
		return
	}
	body, err := json.Marshal(snap)
	if err != nil {
		log.Warn("rgb20 metrics snapshot encode failed", "err", err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		log.Warn("rgb20 metrics push request failed", "err", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.GatewayToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		log.Warn("rgb20 metrics push failed; retrying next interval", "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		log.Warn("rgb20 metrics push rejected; retrying next interval",
			"status", resp.StatusCode, "body", strings.TrimSpace(string(msg)))
	}
}

// pending returns the number of events still waiting in the local outbox. It is read on the
// metrics goroutine only and is cheap (the outbox is small and bounded by the retry window).
func (r *eventRecorder) pending() int64 {
	if r == nil || r.store == nil {
		return 0
	}
	var n int64
	if err := r.store.ForEach([]byte(bridgeEventOutboxBucket), func(k, v []byte) error {
		n++
		return nil
	}); err != nil {
		log.Warn("rgb20 metrics outbox count failed", "err", err)
		return 0
	}
	return n
}
