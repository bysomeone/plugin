package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Bridge event ledger.
//
// The bridge records facts that the chain does not expose directly: which consignment belongs
// to which withdrawal, what the bridge paid in BTC miner fees, when a sweep consolidated user
// deposits. The gateway only mirrors them, it is not the authoritative ledger; events are
// append-only, idempotent by id, and retained for a bounded period.
//
// The bridge posts one event per HTTP request. The same event may be posted again after a
// failed response; the id makes the write idempotent.

const (
	bridgeEventMaxBody      = 256 << 10
	bridgeEventMaxIDLen     = 160
	bridgeEventMaxTypeLen   = 64
	bridgeEventMaxLimit     = 1000
	bridgeEventDefaultLimit = 200
)

// bridgeEventTypes is the accepted vocabulary. Unknown types are rejected so the ledger cannot
// silently fill with typos from a misconfigured bridge.
var bridgeEventTypes = map[string]struct{}{
	"deposit.settled":      {},
	"deposit.minted":       {},
	"deposit.rejected":     {},
	"withdraw.locked":      {},
	"withdraw.broadcast":   {},
	"withdraw.consignment": {},
	"withdraw.settled":     {},
	"withdraw.unlocked":    {},
	"withdraw.bumped":      {},
	"sweep.completed":      {},
	"treasury.snapshot":    {},
}

type bridgeEvent struct {
	Seq        int64           `json:"seq"`
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	OccurredAt int64           `json:"occurred_at"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

type eventStore struct {
	path      string
	retention time.Duration

	mu     sync.Mutex
	events []*bridgeEvent
	byID   map[string]*bridgeEvent
	next   int64
	file   *os.File
}

func openEventStore(dataDir string, retention time.Duration) (*eventStore, error) {
	dir := filepath.Join(dataDir, "bridge-events")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("bridge events dir: %w", err)
	}
	if retention <= 0 {
		retention = 90 * 24 * time.Hour
	}
	s := &eventStore{path: filepath.Join(dir, "events.jsonl"), retention: retention,
		byID: make(map[string]*bridgeEvent), next: 1}
	if err := s.load(); err != nil {
		return nil, err
	}
	if _, err := s.prune(time.Now()); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *eventStore) load() error {
	f, err := os.OpenFile(s.path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open bridge events: %w", err)
	}
	// Read through a separate handle so the append handle's offset is unaffected.
	rf, err := os.Open(s.path)
	if err != nil {
		f.Close()
		return err
	}
	scanner := bufio.NewScanner(rf)
	scanner.Buffer(make([]byte, 0, 64<<10), bridgeEventMaxBody)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		ev := &bridgeEvent{}
		if err := json.Unmarshal([]byte(line), ev); err != nil {
			rf.Close()
			f.Close()
			return fmt.Errorf("decode bridge event line: %w", err)
		}
		if ev.Seq <= 0 || ev.ID == "" {
			rf.Close()
			f.Close()
			return errors.New("bridge event line without seq or id")
		}
		s.events = append(s.events, ev)
		s.byID[ev.ID] = ev
		if ev.Seq >= s.next {
			s.next = ev.Seq + 1
		}
	}
	if err := scanner.Err(); err != nil {
		rf.Close()
		f.Close()
		return err
	}
	if err := rf.Close(); err != nil {
		f.Close()
		return err
	}
	s.file = f
	return nil
}

// append records ev, assigning the next sequence. A duplicate id is a no-op and returns the
// already stored sequence (the bridge retries after a lost response).
func (s *eventStore) append(ev *bridgeEvent, now time.Time) (*bridgeEvent, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.byID[ev.ID]; ok {
		return existing, false, nil
	}
	ev.Seq = s.next
	s.next++
	ev.OccurredAt = now.Unix()
	data, err := json.Marshal(ev)
	if err != nil {
		return nil, false, err
	}
	if _, err := s.file.Write(append(data, '\n')); err != nil {
		return nil, false, err
	}
	if err := s.file.Sync(); err != nil {
		return nil, false, err
	}
	s.events = append(s.events, ev)
	s.byID[ev.ID] = ev
	return ev, true, nil
}

// depositOutcome is the terminal fact the ledger holds for one deposit request: the mint was
// accepted (deposit.minted) or the chain refused it (deposit.rejected).
type depositOutcome struct {
	Type    string
	BTCTxID string
	Amount  int64
	Reason  string
}

// depositOutcome scans the ledger for the newest deposit.minted / deposit.rejected event whose
// payload names requestID. Matching is on the payload's request_id, never on the event id string
// (event ids are derived from the receive id and carry no request identity). A linear scan is
// fine: the slice is bounded by the retention window.
func (s *eventStore) depositOutcome(requestID string) (depositOutcome, bool) {
	if s == nil || requestID == "" {
		return depositOutcome{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.events) - 1; i >= 0; i-- {
		ev := s.events[i]
		if ev.Type != "deposit.minted" && ev.Type != "deposit.rejected" {
			continue
		}
		var payload struct {
			RequestID string `json:"request_id"`
			BTCTxID   string `json:"btc_txid"`
			Amount    int64  `json:"amount"`
			Reason    string `json:"reason"`
		}
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			continue
		}
		if payload.RequestID != requestID {
			continue
		}
		return depositOutcome{
			Type:    ev.Type,
			BTCTxID: payload.BTCTxID,
			Amount:  payload.Amount,
			Reason:  payload.Reason,
		}, true
	}
	return depositOutcome{}, false
}

// stats returns the number of retained events and the highest sequence assigned so far.
// Sequence numbers are never reused, so lastSeq is monotonic even after pruning.
func (s *eventStore) stats() (count, lastSeq int64) {
	if s == nil {
		return 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.events)), s.next - 1
}

// since returns up to limit events with seq > cursor.
func (s *eventStore) since(cursor int64, limit int) []*bridgeEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*bridgeEvent, 0, limit)
	for _, ev := range s.events {
		if ev.Seq <= cursor {
			continue
		}
		out = append(out, ev)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// prune drops events older than the retention window and rewrites the file so disk usage is
// bounded. Cursors stay monotonic: sequence numbers are never reused.
func (s *eventStore) prune(now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := now.Add(-s.retention).Unix()
	kept := s.events[:0]
	removed := 0
	for _, ev := range s.events {
		if ev.OccurredAt < cutoff {
			removed++
			continue
		}
		kept = append(kept, ev)
	}
	if removed == 0 {
		return 0, nil
	}
	s.events = kept
	s.byID = make(map[string]*bridgeEvent, len(kept))
	for _, ev := range kept {
		s.byID[ev.ID] = ev
	}

	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".events-*")
	if err != nil {
		return removed, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	w := bufio.NewWriter(tmp)
	for _, ev := range s.events {
		data, err := json.Marshal(ev)
		if err != nil {
			tmp.Close()
			return removed, err
		}
		if _, err := w.Write(append(data, '\n')); err != nil {
			tmp.Close()
			return removed, err
		}
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		return removed, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return removed, err
	}
	if err := tmp.Close(); err != nil {
		return removed, err
	}
	if s.file != nil {
		if err := s.file.Close(); err != nil {
			return removed, err
		}
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		f, oerr := os.OpenFile(s.path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
		if oerr == nil {
			s.file = f
		}
		return removed, err
	}
	f, err := os.OpenFile(s.path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return removed, err
	}
	s.file = f
	return removed, nil
}

func (s *eventStore) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

// handleBridgeEvents dispatches POST (append) and GET (query) on the ledger endpoint.
func (g *gateway) handleBridgeEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		g.handleBridgeEventGet(w, r)
		return
	}
	g.handleBridgeEventPost(w, r)
}

func (g *gateway) handleBridgeEventPost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if g.events == nil {
		http.Error(w, "event ledger is not configured", http.StatusServiceUnavailable)
		return
	}
	body := &bridgeEvent{}
	if err := decodeLimitedJSON(r, body, bridgeEventMaxBody); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateBridgeEvent(body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	stored, created, err := g.events.append(body, time.Now())
	if err != nil {
		g.logger.Printf("bridge event append failed type=%s id=%s err=%v", body.Type, body.ID, err)
		http.Error(w, "failed to store event", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Event-Seq", strconv.FormatInt(stored.Seq, 10))
	if !created {
		w.Header().Set("X-Event-Duplicate", "true")
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"seq": stored.Seq, "duplicate": !created})
}

func validateBridgeEvent(ev *bridgeEvent) error {
	if ev == nil {
		return errors.New("empty event")
	}
	if ev.ID == "" || len(ev.ID) > bridgeEventMaxIDLen {
		return fmt.Errorf("id must be 1..%d bytes", bridgeEventMaxIDLen)
	}
	if _, ok := bridgeEventTypes[ev.Type]; !ok {
		return fmt.Errorf("unknown event type %q", ev.Type)
	}
	if len(ev.Payload) > bridgeEventMaxBody {
		return errors.New("payload too large")
	}
	if len(ev.Payload) > 0 {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(ev.Payload, &probe); err != nil {
			return errors.New("payload must be a JSON object")
		}
	}
	return nil
}

func (g *gateway) handleBridgeEventGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if g.events == nil {
		http.Error(w, "event ledger is not configured", http.StatusServiceUnavailable)
		return
	}
	cursor := int64(0)
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 0 {
			http.Error(w, "cursor must be a non-negative integer", http.StatusBadRequest)
			return
		}
		cursor = v
	}
	limit := bridgeEventDefaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v <= 0 || v > bridgeEventMaxLimit {
			http.Error(w, fmt.Sprintf("limit must be 1..%d", bridgeEventMaxLimit), http.StatusBadRequest)
			return
		}
		limit = v
	}
	events := g.events.since(cursor, limit)
	next := cursor
	if len(events) > 0 {
		next = events[len(events)-1].Seq
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"events":      events,
		"next_cursor": next,
		"has_more":    len(events) == limit,
	})
}
