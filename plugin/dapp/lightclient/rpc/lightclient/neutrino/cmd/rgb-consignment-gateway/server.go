package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	protocolVersion = "0.2"
	softwareVersion = "0.2.0"

	maxRecipientIDLen = 256
	maxFieldSize      = 64 << 10
	jsonBodyLimit     = 1 << 20
	multipartOverhead = 4 << 20
)

type config struct {
	listen     string
	dataDir    string
	path       string
	maxSize    int64
	ttl        time.Duration
	rateLimit  int
	trustProxy bool

	// chainTitle is the chain title signed into every deposit request.
	chainTitle string
	// bridgeToken authenticates the bridge on /bridge/* endpoints. Empty disables the bridge
	// API (deposit requests cannot be served without it).
	bridgeToken string
	// eventsToken is an optional read-only token for the audit ledger: it may GET events but
	// never POST them, so a statistics/audit backend does not need the bridge's write secret.
	// Empty means only bridgeToken works for reads.
	eventsToken string
	// Optional TLS termination. When clientCA is set, /bridge/* additionally requires a
	// verified client certificate (mTLS).
	tlsCert  string
	tlsKey   string
	clientCA string

	// eventsRetention bounds how long bridge events are kept on disk.
	eventsRetention time.Duration
}

type gateway struct {
	cfg      config
	store    *store
	deposits *depositStore
	events   *eventStore
	limiter  *rateLimiter
	logger   *log.Logger
	started  time.Time
}

func newGateway(cfg config, logger *log.Logger) (*gateway, error) {
	if logger == nil {
		logger = log.Default()
	}
	st, err := openStore(cfg.dataDir, cfg.ttl, logger)
	if err != nil {
		return nil, err
	}
	deposits, err := openDepositStore(cfg.dataDir)
	if err != nil {
		return nil, err
	}
	events, err := openEventStore(cfg.dataDir, cfg.eventsRetention)
	if err != nil {
		return nil, err
	}
	return &gateway{
		cfg:      cfg,
		store:    st,
		deposits: deposits,
		events:   events,
		limiter:  newRateLimiter(cfg.rateLimit, time.Minute),
		logger:   logger,
		started:  time.Now(),
	}, nil
}

func (g *gateway) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(g.cfg.path, g.handleRPC)
	// Deposit requests: the user posts a signed request and polls for its invoice; the bridge
	// consumes requests over SSE and posts invoices back.
	mux.HandleFunc("/deposit/requests", g.handleDepositRequestPost)
	mux.HandleFunc("/deposit/requests/", g.handleDepositRequestGet)
	mux.HandleFunc("/bridge/deposit/requests/stream", g.requireBridgeAuth(g.handleBridgeStream))
	mux.HandleFunc("/bridge/deposit/invoices", g.requireBridgeAuth(g.handleBridgeInvoice))
	// Bridge event ledger: the bridge posts audit facts, an operator's statistics backend reads
	// them by cursor.
	mux.HandleFunc("/bridge/events", g.requireEventReadAuth(g.handleBridgeEvents))
	mux.HandleFunc("/bridge/events/query", g.requireEventReadAuth(g.handleBridgeEventGet))
	return mux
}

// handleRPC serves one JSON-RPC call. Every request is logged with the method,
// a masked recipient ID and the outcome; consignment bytes are never logged.
func (g *gateway) handleRPC(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	method, recipient, result := "-", "-", "ok"
	defer func() {
		g.logger.Printf("request method=%s recipient=%s result=%s duration=%s",
			method, maskRecipient(recipient), result, time.Since(start).Round(time.Microsecond))
	}()

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		result = "method-not-allowed"
		return
	}
	if !g.limiter.allow(clientIP(r, g.cfg.trustProxy)) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		result = "rate-limited"
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	req, file, rerr := g.readRequest(w, r)
	if req.Method != "" {
		method = req.Method
	}
	recipient = recipientHint(req.Params)
	if rerr != nil {
		result = fmt.Sprintf("error %d", rerr.code)
		writeRPCError(w, req.ID, rerr, paramsData(req.Params))
		return
	}

	out, rerr := g.dispatch(req, file)
	if rerr != nil {
		result = fmt.Sprintf("error %d", rerr.code)
		writeRPCError(w, req.ID, rerr, paramsData(req.Params))
		return
	}
	writeRPCResult(w, req.ID, out)
}

func (g *gateway) readRequest(w http.ResponseWriter, r *http.Request) (rpcRequest, []byte, *rpcError) {
	ct := r.Header.Get("Content-Type")
	if mt, _, err := mime.ParseMediaType(ct); err == nil && strings.EqualFold(mt, "multipart/form-data") {
		return g.readMultipart(w, r)
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "multipart/form-data") {
		return rpcRequest{}, nil, rpcErrorf(-32600, "Invalid Request")
	}
	return g.readJSON(w, r)
}

func (g *gateway) readJSON(w http.ResponseWriter, r *http.Request) (rpcRequest, []byte, *rpcError) {
	body := http.MaxBytesReader(w, r.Body, jsonBodyLimit)
	data, err := io.ReadAll(body)
	if err != nil {
		if isMaxBytesError(err) {
			return rpcRequest{}, nil, tooLargeError()
		}
		return rpcRequest{}, nil, rpcErrorf(-32700, "Parse error")
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return rpcRequest{}, nil, rpcErrorf(-32700, "Parse error")
	}
	if trimmed[0] == '[' {
		return rpcRequest{}, nil, rpcErrorf(-32600, "Batch requests are not supported")
	}
	var probe json.RawMessage
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return rpcRequest{}, nil, rpcErrorf(-32700, "Parse error")
	}
	var raw struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return rpcRequest{}, nil, rpcErrorf(-32600, "Invalid Request")
	}
	req := rpcRequest{ID: idOrNull(raw.ID), Method: raw.Method}
	if raw.Method == "" {
		return req, nil, rpcErrorf(-32600, "Invalid Request")
	}
	p, rerr := newParams(raw.Params)
	if rerr != nil {
		return req, nil, rerr
	}
	req.Params = p
	return req, nil, nil
}

func (g *gateway) readMultipart(w http.ResponseWriter, r *http.Request) (rpcRequest, []byte, *rpcError) {
	r.Body = http.MaxBytesReader(w, r.Body, g.cfg.maxSize+multipartOverhead)
	mr, err := r.MultipartReader()
	if err != nil {
		return rpcRequest{}, nil, rpcErrorf(-32600, "Invalid Request")
	}

	fields := make(map[string]string)
	var file []byte
	fileSeen := false
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			if isMaxBytesError(err) {
				return rpcRequest{}, nil, tooLargeError()
			}
			return rpcRequest{}, nil, rpcErrorf(-32600, "Invalid Request")
		}
		name := part.FormName()
		switch {
		case name == "file":
			// Only the first file part is meaningful; drain the rest.
			if fileSeen {
				_ = part.Close()
				continue
			}
			fileSeen = true
			data, tooLarge, err := readPart(part, g.cfg.maxSize)
			if err != nil {
				if isMaxBytesError(err) {
					return rpcRequest{}, nil, tooLargeError()
				}
				return rpcRequest{}, nil, rpcErrorf(-32600, "Invalid Request")
			}
			if tooLarge {
				return rpcRequest{}, nil, tooLargeError()
			}
			file = data
		case name == "":
			_ = part.Close()
		case len(name) > 128:
			_ = part.Close()
			return rpcRequest{}, nil, rpcErrorf(-32600, "Invalid Request")
		default:
			data, tooLarge, err := readPart(part, maxFieldSize)
			if err != nil {
				if isMaxBytesError(err) {
					return rpcRequest{}, nil, tooLargeError()
				}
				return rpcRequest{}, nil, rpcErrorf(-32600, "Invalid Request")
			}
			if tooLarge {
				return rpcRequest{}, nil, rpcErrorf(-32600, "Invalid Request")
			}
			fields[name] = string(data)
		}
	}

	id := json.RawMessage("null")
	if v, ok := fields["id"]; ok {
		id = parseIDField(v)
	}
	method := fields["method"]
	if method == "" {
		return rpcRequest{ID: id}, nil, rpcErrorf(-32600, "Invalid Request")
	}
	req := rpcRequest{ID: id, Method: method}
	rawParams, hasParams := multipartParams(fields)
	if hasParams {
		p, rerr := newParams(rawParams)
		if rerr != nil {
			return req, nil, rerr
		}
		req.Params = p
	} else {
		req.Params = &params{obj: map[string]json.RawMessage{}, raw: json.RawMessage("null")}
	}
	return req, file, nil
}

// readPart reads at most limit bytes and reports whether the part was larger.
func readPart(part *multipart.Part, limit int64) ([]byte, bool, error) {
	defer part.Close()
	data, err := io.ReadAll(io.LimitReader(part, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > limit {
		return nil, true, nil
	}
	return data, false, nil
}

// multipartParams accepts the canonical "params" text field (a JSON object or
// a JSON-encoded string) and the flattened "params[key]" form used by some
// proxies.
func multipartParams(fields map[string]string) (json.RawMessage, bool) {
	if v, ok := fields["params"]; ok {
		return json.RawMessage(v), true
	}
	nested := make(map[string]json.RawMessage)
	for key, value := range fields {
		if !strings.HasPrefix(key, "params[") || !strings.HasSuffix(key, "]") {
			continue
		}
		name := key[len("params[") : len(key)-1]
		if name == "" {
			continue
		}
		nested[name] = bracketValue(name, value)
	}
	if len(nested) == 0 {
		return nil, false
	}
	encoded, err := json.Marshal(nested)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

func bracketValue(name, value string) json.RawMessage {
	switch name {
	case "recipient_id", "txid", "attachment_id":
		encoded, _ := json.Marshal(value)
		return encoded
	case "ack":
		if value == "true" || value == "false" {
			return json.RawMessage(value)
		}
	case "vout":
		if _, err := strconv.ParseUint(value, 10, 32); err == nil {
			return json.RawMessage(value)
		}
	}
	if json.Valid([]byte(value)) {
		return json.RawMessage(value)
	}
	encoded, _ := json.Marshal(value)
	return encoded
}

// parseIDField turns a multipart id field into a JSON-RPC id. The canonical
// client sends the literal text "null"; numeric ids stay numeric.
func parseIDField(s string) json.RawMessage {
	s = strings.TrimSpace(s)
	if s == "" {
		return json.RawMessage("null")
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	encoded, _ := json.Marshal(s)
	return encoded
}

func (g *gateway) dispatch(req rpcRequest, file []byte) (any, *rpcError) {
	switch req.Method {
	case "server.info":
		return map[string]any{
			"protocol_version": protocolVersion,
			"version":          softwareVersion,
			"uptime":           int64(time.Since(g.started) / time.Second),
		}, nil
	case "consignment.post":
		return g.consignmentPost(req.Params, file)
	case "consignment.get":
		return g.consignmentGet(req.Params)
	case "ack.post":
		return g.ackPost(req.Params)
	case "ack.get":
		return g.ackGet(req.Params)
	case "media.post", "media.get":
		return nil, rpcErrorf(-32601, "Method not found")
	default:
		return nil, rpcErrorf(-32601, "Method not found")
	}
}

func (g *gateway) consignmentPost(p *params, file []byte) (any, *rpcError) {
	recipient, rerr := recipientIDParam(p)
	if rerr != nil {
		return nil, rerr
	}
	txid, rerr := txIDParam(p)
	if rerr != nil {
		return nil, rerr
	}
	vout, rerr := voutParam(p)
	if rerr != nil {
		return nil, rerr
	}
	if len(file) == 0 {
		return nil, rpcErrorf(-303, "Missing file")
	}
	if int64(len(file)) > g.cfg.maxSize {
		return nil, tooLargeError()
	}

	created, err := g.store.post(recipient, txid, vout, file)
	switch {
	case errors.Is(err, errFileConflict):
		return nil, rpcErrorf(-101, "Cannot change uploaded file")
	case err != nil:
		g.logger.Printf("consignment.post storage error: %v", err)
		return nil, rpcErrorf(-32603, "Internal error")
	}
	return created, nil
}

type consignmentResult struct {
	Consignment string  `json:"consignment"`
	TxID        string  `json:"txid"`
	Vout        *uint32 `json:"vout,omitempty"`
}

func (g *gateway) consignmentGet(p *params) (any, *rpcError) {
	recipient, rerr := recipientIDParam(p)
	if rerr != nil {
		return nil, rerr
	}
	e, data, err := g.store.get(recipient)
	switch {
	case errors.Is(err, errNotFound):
		return nil, rpcErrorf(-400, "Consignment file not found")
	case err != nil:
		g.logger.Printf("consignment.get storage error: %v", err)
		return nil, rpcErrorf(-32603, "Internal error")
	}
	return consignmentResult{
		Consignment: base64.StdEncoding.EncodeToString(data),
		TxID:        e.TxID,
		Vout:        e.Vout,
	}, nil
}

func (g *gateway) ackPost(p *params) (any, *rpcError) {
	recipient, rerr := recipientIDParam(p)
	if rerr != nil {
		return nil, rerr
	}
	ack, rerr := ackParam(p)
	if rerr != nil {
		return nil, rerr
	}
	txid, rerr := optionalTxIDParam(p)
	if rerr != nil {
		return nil, rerr
	}
	created, err := g.store.postAck(recipient, txid, ack)
	switch {
	case errors.Is(err, errNotFound):
		return nil, rpcErrorf(-300, "Missing ACK")
	case errors.Is(err, errAckConflict):
		return nil, rpcErrorf(-100, "Cannot change ACK")
	case err != nil:
		g.logger.Printf("ack.post storage error: %v", err)
		return nil, rpcErrorf(-32603, "Internal error")
	}
	return created, nil
}

func (g *gateway) ackGet(p *params) (any, *rpcError) {
	recipient, rerr := recipientIDParam(p)
	if rerr != nil {
		return nil, rerr
	}
	txid, rerr := optionalTxIDParam(p)
	if rerr != nil {
		return nil, rerr
	}
	ack, err := g.store.getAck(recipient, txid)
	switch {
	case errors.Is(err, errNotFound):
		return nil, rpcErrorf(-400, "Consignment file not found")
	case err != nil:
		g.logger.Printf("ack.get storage error: %v", err)
		return nil, rpcErrorf(-32603, "Internal error")
	}
	if ack == nil {
		return nil, nil
	}
	return *ack, nil
}

// params is a parsed JSON-RPC params object. raw keeps the normalised form for
// error.data, so a JSON-encoded string is echoed as the object it encodes.
type params struct {
	obj map[string]json.RawMessage
	raw json.RawMessage
}

func newParams(raw json.RawMessage) (*params, *rpcError) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return &params{obj: map[string]json.RawMessage{}, raw: json.RawMessage("null")}, nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil, rpcErrorf(-32602, "Invalid params")
		}
		trimmed = bytes.TrimSpace([]byte(s))
		if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
			return &params{obj: map[string]json.RawMessage{}, raw: json.RawMessage("null")}, nil
		}
	}
	if trimmed[0] != '{' {
		return nil, rpcErrorf(-32602, "Invalid params")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return nil, rpcErrorf(-32602, "Invalid params")
	}
	if obj == nil {
		obj = map[string]json.RawMessage{}
	}
	return &params{obj: obj, raw: append(json.RawMessage(nil), trimmed...)}, nil
}

func (p *params) lookup(key string) (json.RawMessage, bool) {
	if p == nil {
		return nil, false
	}
	v, ok := p.obj[key]
	return v, ok
}

func paramsData(p *params) any {
	if p == nil || len(p.raw) == 0 {
		return nil
	}
	return p.raw
}

func recipientHint(p *params) string {
	raw, ok := p.lookup("recipient_id")
	if !ok {
		return "-"
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || s == "" {
		return "-"
	}
	return s
}

func recipientIDParam(p *params) (string, *rpcError) {
	raw, ok := p.lookup("recipient_id")
	if !ok {
		return "", rpcErrorf(-302, "Missing recipient ID")
	}
	var id string
	if err := json.Unmarshal(raw, &id); err != nil || id == "" || len(id) > maxRecipientIDLen {
		return "", rpcErrorf(-202, "Invalid recipient ID")
	}
	return id, nil
}

func txIDParam(p *params) (string, *rpcError) {
	raw, ok := p.lookup("txid")
	if !ok {
		return "", rpcErrorf(-304, "Missing TXID")
	}
	var txid string
	if err := json.Unmarshal(raw, &txid); err != nil || !validTxID(txid) {
		return "", rpcErrorf(-203, "Invalid TXID")
	}
	return strings.ToLower(txid), nil
}

// optionalTxIDParam is txIDParam for methods where the txid narrows an
// otherwise recipient-scoped call (several consignments may share a recipient).
func optionalTxIDParam(p *params) (string, *rpcError) {
	raw, ok := p.lookup("txid")
	if !ok {
		return "", nil
	}
	var txid string
	if err := json.Unmarshal(raw, &txid); err != nil || !validTxID(txid) {
		return "", rpcErrorf(-203, "Invalid TXID")
	}
	return strings.ToLower(txid), nil
}

func voutParam(p *params) (*uint32, *rpcError) {
	raw, ok := p.lookup("vout")
	if !ok {
		return nil, nil
	}
	trimmed := bytes.TrimSpace(raw)
	// A JSON string is not an integer, even if its content looks numeric.
	if len(trimmed) == 0 || trimmed[0] == '"' {
		return nil, rpcErrorf(-204, "Invalid vout")
	}
	var n json.Number
	if err := json.Unmarshal(trimmed, &n); err != nil {
		return nil, rpcErrorf(-204, "Invalid vout")
	}
	u, err := strconv.ParseUint(n.String(), 10, 32)
	if err != nil {
		return nil, rpcErrorf(-204, "Invalid vout")
	}
	v := uint32(u)
	return &v, nil
}

func ackParam(p *params) (bool, *rpcError) {
	raw, ok := p.lookup("ack")
	if !ok {
		return false, rpcErrorf(-300, "Missing ACK")
	}
	var ack bool
	if err := json.Unmarshal(raw, &ack); err != nil {
		return false, rpcErrorf(-200, "Invalid ACK")
	}
	return ack, nil
}

type rpcError struct {
	code     int
	message  string
	tooLarge bool
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("%d %s", e.code, e.message)
}

func rpcErrorf(code int, message string) *rpcError {
	return &rpcError{code: code, message: message}
}

func tooLargeError() *rpcError {
	return &rpcError{code: -32600, message: "Request too large", tooLarge: true}
}

type rpcRequest struct {
	ID     json.RawMessage
	Method string
	Params *params
}

type rpcSuccess struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result"`
}

type rpcFailure struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   rpcErrorObject  `json:"error"`
}

type rpcErrorObject struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeJSON(w, http.StatusOK, rpcSuccess{
		JSONRPC: "2.0",
		ID:      idOrNull(id),
		Result:  result,
	})
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, e *rpcError, data any) {
	status := http.StatusOK
	if e.tooLarge {
		status = http.StatusRequestEntityTooLarge
	}
	writeJSON(w, status, rpcFailure{
		JSONRPC: "2.0",
		ID:      idOrNull(id),
		Error: rpcErrorObject{
			Code:    e.code,
			Message: e.message,
			Data:    data,
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

func idOrNull(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

func isMaxBytesError(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}

// maskRecipient exposes at most the first eight characters of a recipient ID.
// Shorter IDs are cut in half, so no complete ID reaches the log.
func maskRecipient(id string) string {
	if id == "" || id == "-" {
		return "-"
	}
	n := 8
	if len(id) <= n {
		n = len(id) / 2
		if n == 0 {
			n = 1
		}
	}
	return id[:n] + "***"
}
