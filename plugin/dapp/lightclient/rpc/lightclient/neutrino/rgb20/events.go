package rgb20

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Bridge event outbox.
//
// The bridge records audit facts the chain does not carry (which consignment belongs to which
// withdrawal, what the bridge paid in BTC miner fees, how a sweep was funded) and posts them to
// the gateway's append-only ledger. Delivery is at-least-once: an event is written to the local
// outbox first, posted, and deleted only after the gateway acknowledges it. The event id is
// derived from (type, key), so a retried or re-derived event collapses on the gateway instead of
// duplicating.
//
// Recording never blocks the money path: Record writes to the local store and signals a worker;
// the worker does all network I/O.
const (
	bridgeEventOutboxBucket = "bridge-event-outbox"
	bridgeEventRetryMin     = 2 * time.Second
	bridgeEventRetryMax     = 2 * time.Minute
	bridgeEventScanInterval = 30 * time.Second
	bridgeEventPostTimeout  = 10 * time.Second
)

type eventRecorder struct {
	endpoint string
	token    string
	store    KVStore
	client   *http.Client

	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}
	wg     sync.WaitGroup
}

// event wire shape (matches the gateway's ledger).
type bridgeEventWire struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// newEventRecorder returns nil when the gateway is not configured: callers treat a nil recorder
// as "record nothing".
func newEventRecorder(cfg Config, store KVStore) *eventRecorder {
	base, err := gatewayBaseURL(cfg.TransportGateway)
	if err != nil || cfg.GatewayToken == "" || store == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &eventRecorder{
		endpoint: base + "/bridge/events",
		token:    cfg.GatewayToken,
		store:    store,
		client:   &http.Client{Timeout: bridgeEventPostTimeout},
		ctx:      ctx,
		cancel:   cancel,
		wake:     make(chan struct{}, 1),
	}
	r.wg.Add(1)
	go r.run()
	return r
}

func (r *eventRecorder) stop() {
	if r == nil {
		return
	}
	r.cancel()
	r.wg.Wait()
}

// record enqueues an event. key makes the event id stable across retries; callers pass the
// chain-side identifier of the subject (receive id, burn tx hash, sweep txid).
func (r *eventRecorder) record(kind, key string, payload map[string]any) {
	if r == nil {
		return
	}
	id := bridgeEventID(kind, key)
	data, err := json.Marshal(payload)
	if err != nil {
		log.Error("bridge event encode failed", "type", kind, "key", key, "err", err)
		return
	}
	ev := &bridgeEventWire{ID: id, Type: kind, Payload: data}
	wire, err := json.Marshal(ev)
	if err != nil {
		log.Error("bridge event encode failed", "type", kind, "key", key, "err", err)
		return
	}
	if err := r.store.Put([]byte(bridgeEventOutboxBucket), []byte(id), wire); err != nil {
		log.Error("bridge event outbox write failed", "type", kind, "key", key, "err", err)
		return
	}
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// bridgeEventID derives a bounded, stable id from the event type and its subject key.
func bridgeEventID(kind, key string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + key))
	return kind + ":" + hex.EncodeToString(sum[:16])
}

func (r *eventRecorder) run() {
	defer r.wg.Done()
	ticker := time.NewTicker(bridgeEventScanInterval)
	defer ticker.Stop()
	delay := bridgeEventRetryMin
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-r.wake:
		case <-ticker.C:
		case <-timer.C:
		}
		if r.drain() {
			delay = bridgeEventRetryMin
		} else {
			delay *= 2
			if delay > bridgeEventRetryMax {
				delay = bridgeEventRetryMax
			}
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(delay)
	}
}

// drain posts every pending event. It returns false when the gateway is unreachable or refuses
// the event with a retryable status; the caller backs off.
//
// Entries are collected before posting: ForEach may hold the store's read lock, and post/delete
// must not run underneath it.
func (r *eventRecorder) drain() bool {
	var pending []struct{ key, val []byte }
	if err := r.store.ForEach([]byte(bridgeEventOutboxBucket), func(k, v []byte) error {
		entry := struct{ key, val []byte }{key: append([]byte(nil), k...), val: append([]byte(nil), v...)}
		pending = append(pending, entry)
		return nil
	}); err != nil {
		log.Warn("bridge event outbox read failed", "err", err)
		return false
	}
	ok := true
	for _, entry := range pending {
		ev := &bridgeEventWire{}
		if err := json.Unmarshal(entry.val, ev); err != nil {
			log.Error("bridge event outbox entry is not decodable; dropping", "err", err)
			if derr := r.store.Delete([]byte(bridgeEventOutboxBucket), entry.key); derr != nil {
				log.Error("bridge event outbox delete failed", "err", derr)
			}
			continue
		}
		retry, err := r.post(entry.val)
		if err != nil {
			log.Warn("bridge event delivery failed; will retry", "id", ev.ID, "retry", retry, "err", err)
			if retry {
				ok = false
				continue
			}
			// The gateway will never accept this payload (bad event); drop it so the outbox
			// does not stall behind one broken entry.
		}
		if err := r.store.Delete([]byte(bridgeEventOutboxBucket), entry.key); err != nil {
			log.Error("bridge event outbox delete failed", "id", ev.ID, "err", err)
		}
	}
	return ok
}

// post returns (retryable, error). A nil error means the gateway stored (or already had) it.
func (r *eventRecorder) post(body []byte) (bool, error) {
	req, err := http.NewRequestWithContext(r.ctx, http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return true, err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return false, nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden ||
		resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode >= 500:
		return true, fmt.Errorf("gateway status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	default:
		// 4xx: the event itself is refused. Retrying cannot help.
		return false, fmt.Errorf("gateway rejected event %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
}

// post returns (retryable, error). A nil error means the gateway stored (or already had) it.
