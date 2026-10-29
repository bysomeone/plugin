package main

import (
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/33cn/plugin/plugin/dapp/rgbx/depositrequest"
)

// Deposit requests and invoices.
//
// A user asks the gateway for a deposit invoice by posting a signed request. The gateway
// verifies the signature (the same encoding the chain verifies at mint time), stores the
// request, and pushes it to the bridge over an SSE stream. The bridge creates the invoice and
// posts it back; the user's app polls the request by id.
//
// Trust model: the gateway can delay or drop a request (availability), but it cannot forge one
// or redirect the mint. A forged request fails signature verification here and on chain; a
// tampered amount or address fails the signed-content comparison on chain.

const (
	depositRequestStatusPending  = "pending"
	depositRequestStatusInvoiced = "invoiced"

	depositRequestMaxBody = 256 << 10
	// depositStreamBuffer is how many requests may queue for one stream subscriber before
	// further broadcasts are dropped. A reconnect replays every pending request, so a dropped
	// event costs latency, never correctness.
	depositStreamBuffer = 256
	depositStreamPing   = 20 * time.Second
)

type depositRequestRecord struct {
	RequestID   string `json:"request_id"`
	Chain33Addr string `json:"chain33_addr"`
	AssetSymbol string `json:"asset_symbol"`
	Amount      int64  `json:"amount"`
	Timestamp   int64  `json:"timestamp"`
	// PublicKeyHex / SignatureHex are kept verbatim: the bridge carries them into the mint
	// transaction, where the chain verifies the same signature.
	PublicKeyHex string `json:"pubkey"`
	SignatureHex string `json:"signature"`

	Status      string `json:"status"`
	Invoice     string `json:"invoice,omitempty"`
	ReceiveID   string `json:"receive_id,omitempty"`
	RecipientID string `json:"recipient_id,omitempty"`
	CreatedAt   int64  `json:"created_at"`
}

// depositRequestWire is the public shape returned to the user's app.
type depositRequestWire struct {
	RequestID   string `json:"request_id"`
	Chain33Addr string `json:"chain33_addr"`
	AssetSymbol string `json:"asset_symbol"`
	Amount      int64  `json:"amount"`
	Timestamp   int64  `json:"timestamp"`
	Status      string `json:"status"`
	Invoice     string `json:"invoice,omitempty"`
	ReceiveID   string `json:"receive_id,omitempty"`
	RecipientID string `json:"recipient_id,omitempty"`
}

func (r *depositRequestRecord) wire() depositRequestWire {
	return depositRequestWire{
		RequestID:   r.RequestID,
		Chain33Addr: r.Chain33Addr,
		AssetSymbol: r.AssetSymbol,
		Amount:      r.Amount,
		Timestamp:   r.Timestamp,
		Status:      r.Status,
		Invoice:     r.Invoice,
		ReceiveID:   r.ReceiveID,
		RecipientID: r.RecipientID,
	}
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

type depositStore struct {
	dir string

	mu   sync.RWMutex
	byID map[string]*depositRequestRecord
	subs map[chan *depositRequestRecord]struct{}
}

func openDepositStore(dataDir string) (*depositStore, error) {
	dir := filepath.Join(dataDir, "deposit-requests")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("deposit requests dir: %w", err)
	}
	s := &depositStore{dir: dir, byID: make(map[string]*depositRequestRecord),
		subs: make(map[chan *depositRequestRecord]struct{})}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read deposit requests: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("read deposit request %s: %w", name, err)
		}
		rec := &depositRequestRecord{}
		if err := json.Unmarshal(data, rec); err != nil {
			return nil, fmt.Errorf("decode deposit request %s: %w", name, err)
		}
		if rec.RequestID == "" {
			return nil, fmt.Errorf("deposit request %s has no request id", name)
		}
		s.byID[rec.RequestID] = rec
	}
	return s, nil
}

// put stores a new request. An existing request with the same id is returned unchanged
// (created=false): the user may retry the same signed request safely.
func (s *depositStore) put(rec *depositRequestRecord) (created bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.byID[rec.RequestID]; ok {
		*rec = *existing
		return false, nil
	}
	rec.Status = depositRequestStatusPending
	if rec.CreatedAt == 0 {
		rec.CreatedAt = time.Now().Unix()
	}
	if err := writeJSONAtomic(s.path(rec.RequestID), rec); err != nil {
		return false, err
	}
	s.byID[rec.RequestID] = rec
	s.broadcastLocked(rec)
	return true, nil
}

func (s *depositStore) get(id string) (*depositRequestRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.byID[id]
	if !ok {
		return nil, false
	}
	copied := *rec
	return &copied, true
}

func (s *depositStore) setInvoice(id, invoice, receiveID, recipientID string) (*depositRequestRecord, error) {
	if invoice == "" || receiveID == "" {
		return nil, errors.New("invoice and receive id are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byID[id]
	if !ok {
		return nil, errDepositNotFound
	}
	if rec.Status == depositRequestStatusInvoiced {
		// Idempotent repeat from a reconnecting bridge. A different invoice for the same
		// request is a conflict: the user may already have paid the first one.
		if rec.Invoice == invoice && rec.ReceiveID == receiveID {
			copied := *rec
			return &copied, nil
		}
		return nil, errDepositConflict
	}
	updated := *rec
	updated.Status = depositRequestStatusInvoiced
	updated.Invoice = invoice
	updated.ReceiveID = receiveID
	updated.RecipientID = recipientID
	if err := writeJSONAtomic(s.path(id), &updated); err != nil {
		return nil, err
	}
	s.byID[id] = &updated
	copied := updated
	return &copied, nil
}

// pending returns every request the bridge has not invoiced yet, oldest first.
func (s *depositStore) pending() []*depositRequestRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*depositRequestRecord, 0, len(s.byID))
	for _, rec := range s.byID {
		if rec.Status != depositRequestStatusPending {
			continue
		}
		copied := *rec
		out = append(out, &copied)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

func (s *depositStore) subscribe() chan *depositRequestRecord {
	ch := make(chan *depositRequestRecord, depositStreamBuffer)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch
}

func (s *depositStore) unsubscribe(ch chan *depositRequestRecord) {
	s.mu.Lock()
	delete(s.subs, ch)
	s.mu.Unlock()
}

func (s *depositStore) broadcastLocked(rec *depositRequestRecord) {
	copied := *rec
	for ch := range s.subs {
		select {
		case ch <- &copied:
		default: // subscriber is behind; it replays pending requests on its next connect
		}
	}
}

// cleanup drops requests older than ttl, invoiced or not. An invoiced request is retained so
// the app can keep showing it; ttl bounds that.
func (s *depositStore) cleanup(ttl time.Duration) (int, error) {
	cutoff := time.Now().Add(-ttl).Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for id, rec := range s.byID {
		if rec.CreatedAt >= cutoff {
			continue
		}
		if err := os.Remove(s.path(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return removed, err
		}
		delete(s.byID, id)
		removed++
	}
	return removed, nil
}

func (s *depositStore) path(id string) string {
	return filepath.Join(s.dir, id+".json")
}

// writeJSONAtomic writes v to path through a synced temporary file and a rename, so a crash
// never leaves a half-written request behind.
func writeJSONAtomic(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

var (
	errDepositNotFound = errors.New("deposit request not found")
	errDepositConflict = errors.New("deposit request already invoiced with different data")
)

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

type depositRequestBody struct {
	RequestID   string `json:"request_id"`
	Chain33Addr string `json:"chain33_addr"`
	AssetSymbol string `json:"asset_symbol"`
	Amount      int64  `json:"amount"`
	Timestamp   int64  `json:"timestamp"`
	PublicKey   string `json:"pubkey"`
	Signature   string `json:"signature"`
}

// handleDepositRequestPost accepts a signed deposit request (public endpoint).
func (g *gateway) handleDepositRequestPost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !g.depositEnabled() {
		http.Error(w, "deposit requests are not configured", http.StatusServiceUnavailable)
		return
	}
	if !g.limiter.allow(clientIP(r, g.cfg.trustProxy)) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	body := &depositRequestBody{}
	if err := decodeLimitedJSON(r, body, depositRequestMaxBody); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	pub, err := hex.DecodeString(strings.TrimPrefix(body.PublicKey, "0x"))
	if err != nil {
		http.Error(w, "pubkey is not hex", http.StatusBadRequest)
		return
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(body.Signature, "0x"))
	if err != nil {
		http.Error(w, "signature is not hex", http.StatusBadRequest)
		return
	}
	req := depositrequest.Request{
		RequestID:   body.RequestID,
		Chain33Addr: body.Chain33Addr,
		AssetSymbol: body.AssetSymbol,
		Amount:      body.Amount,
		Timestamp:   body.Timestamp,
	}
	if err := depositrequest.Verify(g.cfg.chainTitle, req, pub, sig); err != nil {
		g.logger.Printf("deposit request rejected: %v", err)
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	rec := &depositRequestRecord{
		RequestID:    req.RequestID,
		Chain33Addr:  req.Chain33Addr,
		AssetSymbol:  req.AssetSymbol,
		Amount:       req.Amount,
		Timestamp:    req.Timestamp,
		PublicKeyHex: body.PublicKey,
		SignatureHex: body.Signature,
		CreatedAt:    time.Now().Unix(),
	}
	created, err := g.deposits.put(rec)
	if err != nil {
		g.logger.Printf("deposit request storage error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"request_id": rec.RequestID,
		"status":     rec.Status,
		"created":    created,
	})
}

// handleDepositRequestGet returns one request and, once available, its invoice (public).
func (g *gateway) handleDepositRequestGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/deposit/requests/")
	if id == "" || strings.ContainsRune(id, '/') {
		http.NotFound(w, r)
		return
	}
	rec, ok := g.deposits.get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, rec.wire())
}

// handleBridgeStream pushes deposit requests to the bridge over SSE.
func (g *gateway) handleBridgeStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	// This response outlives the server's WriteTimeout; clear the deadline for this connection.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sub := g.deposits.subscribe()
	defer g.deposits.unsubscribe(sub)

	send := func(rec *depositRequestRecord) bool {
		data, err := json.Marshal(rec)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: request\ndata: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	// Replay the backlog first: a reconnect must not lose requests created while the bridge
	// was away.
	for _, rec := range g.deposits.pending() {
		if !send(rec) {
			return
		}
	}

	ping := time.NewTicker(depositStreamPing)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case rec := <-sub:
			if !send(rec) {
				return
			}
		case <-ping.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

type bridgeInvoiceBody struct {
	RequestID   string `json:"request_id"`
	Invoice     string `json:"invoice"`
	ReceiveID   string `json:"receive_id"`
	RecipientID string `json:"recipient_id"`
}

// handleBridgeInvoice attaches the invoice created by the bridge to its request.
func (g *gateway) handleBridgeInvoice(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body := &bridgeInvoiceBody{}
	if err := decodeLimitedJSON(r, body, depositRequestMaxBody); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	rec, err := g.deposits.setInvoice(body.RequestID, body.Invoice, body.ReceiveID, body.RecipientID)
	switch {
	case errors.Is(err, errDepositNotFound):
		http.Error(w, "request not found", http.StatusNotFound)
		return
	case errors.Is(err, errDepositConflict):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case err != nil:
		g.logger.Printf("deposit invoice storage error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"request_id": rec.RequestID, "status": rec.Status})
}

// requireBridgeAuth enforces the bridge shared secret on /bridge/* paths.
func (g *gateway) requireBridgeAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !g.bridgeAuthOK(w, r) {
			return
		}
		next(w, r)
	}
}

// requireEventReadAuth accepts the bridge token for every method, and the optional read-only
// events token for GET only. mTLS (when configured) still applies to both.
func (g *gateway) requireEventReadAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if g.cfg.eventsToken != "" && g.bearerMatches(r, g.cfg.eventsToken) {
			if g.cfg.clientCA != "" && (r.TLS == nil || len(r.TLS.VerifiedChains) == 0) {
				http.Error(w, "client certificate required", http.StatusForbidden)
				return
			}
			if r.Method != http.MethodGet {
				http.Error(w, "the events token is read-only", http.StatusForbidden)
				return
			}
			next(w, r)
			return
		}
		if !g.bridgeAuthOK(w, r) {
			return
		}
		next(w, r)
	}
}

// bearerMatches reports whether the request carries exactly this bearer token.
func (g *gateway) bearerMatches(r *http.Request, token string) bool {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) || token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(auth, prefix)), []byte(token)) == 1
}

// bridgeAuthOK writes the failure response and returns false when the caller is not allowed.
func (g *gateway) bridgeAuthOK(w http.ResponseWriter, r *http.Request) bool {
	if g.cfg.bridgeToken == "" || g.deposits == nil || g.cfg.chainTitle == "" {
		http.Error(w, "bridge API is not configured", http.StatusServiceUnavailable)
		return false
	}
	// When the gateway terminates TLS with a client CA, a verified certificate is required
	// in addition to the bearer token.
	if g.cfg.clientCA != "" && (r.TLS == nil || len(r.TLS.VerifiedChains) == 0) {
		http.Error(w, "client certificate required", http.StatusForbidden)
		return false
	}
	if !g.bearerMatches(r, g.cfg.bridgeToken) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="rgb-gateway-bridge"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func (g *gateway) depositEnabled() bool {
	return g.deposits != nil && g.cfg.chainTitle != ""
}

func decodeLimitedJSON(r *http.Request, out any, limit int64) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(body)) > limit {
		return errors.New("body too large")
	}
	return json.Unmarshal(body, out)
}
