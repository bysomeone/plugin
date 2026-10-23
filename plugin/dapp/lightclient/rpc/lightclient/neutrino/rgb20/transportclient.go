package rgb20

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// RGB HTTP JSON-RPC protocol v0.2 client.
//
// The bridge acts as a *client* of a consignment gateway in both directions:
//   - deposits (bridge is the payee): `consignment.get(recipient_id)` →
//     sidecar validation → `ack.post`;
//   - withdrawals (bridge is the payer): `consignment.post` to the endpoint carried by the
//     user's invoice → `ack.get` until the payee records its verdict.
//
// `media.*` is not implemented on either side.

const (
	// transportProtocolVersion is the only protocol version this client speaks.
	transportProtocolVersion = "0.2"
	// transportResponseLimit caps a JSON-RPC response body. The gateway's default consignment
	// limit is 16 MiB; base64 inflates that to ~21.4 MiB, so leave room for framing.
	transportResponseLimit = 64 << 20
	// transportRequestTimeout bounds one JSON-RPC call.
	transportRequestTimeout = 30 * time.Second
)

// ErrConsignmentNotFound means the gateway has no consignment for this recipient yet
// (protocol error -400). It is a normal "not delivered yet" state, not a failure.
var ErrConsignmentNotFound = errors.New("consignment not found")

// TransportRPCError is a JSON-RPC error returned by the gateway.
type TransportRPCError struct {
	Code    int
	Message string
}

func (e *TransportRPCError) Error() string {
	return fmt.Sprintf("gateway JSON-RPC error %d: %s", e.Code, e.Message)
}

// transportHTTPError is a non-2xx HTTP response (or a malformed body) from the gateway.
type transportHTTPError struct {
	StatusCode int
	Reason     string
}

func (e *transportHTTPError) Error() string {
	return fmt.Sprintf("gateway HTTP %d: %s", e.StatusCode, e.Reason)
}

// TransportClient talks RGB HTTP JSON-RPC v0.2 to a consignment gateway.
type TransportClient struct {
	endpoint   string
	httpClient *http.Client
	nextID     atomic.Uint64
}

// NormalizeTransportEndpoint turns an invoice transport into an HTTP URL.
//
// The RGB invoice transport scheme is `rpc://` (plain JSON-RPC) or `rpcs://` (TLS), e.g.
// `rpcs://gw.example.com/0.2/json-rpc`; `http`/`https` are accepted as-is. Other transports
// (`rest`, `ws`, …) are not spoken by this client and report ok=false so the caller can skip
// them instead of failing the whole delivery.
func NormalizeTransportEndpoint(raw string) (endpoint string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "rpc":
		u.Scheme = "http"
	case "https", "rpcs":
		u.Scheme = "https"
	default:
		return "", false
	}
	return u.String(), true
}

// NewTransportClient builds a client for an endpoint such as
// `http://gw:8080/0.2/json-rpc` or `rpcs://gw.example.com/0.2/json-rpc`. A nil client uses the
// default timeout.
func NewTransportClient(endpoint string, httpClient *http.Client) (*TransportClient, error) {
	if endpoint == "" {
		return nil, errors.New("transport gateway endpoint empty")
	}
	normalized, ok := NormalizeTransportEndpoint(endpoint)
	if !ok {
		return nil, fmt.Errorf("transport gateway endpoint %q: unsupported scheme or host", endpoint)
	}
	endpoint = normalized
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("transport gateway endpoint %q: %w", endpoint, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("transport gateway endpoint %q: missing host", endpoint)
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: transportRequestTimeout}
	}
	return &TransportClient{endpoint: endpoint, httpClient: httpClient}, nil
}

// ServerInfo is the `server.info` result.
type ServerInfo struct {
	ProtocolVersion string `json:"protocol_version"`
	Version         string `json:"version"`
	Uptime          int64  `json:"uptime"`
}

// ConsignmentResult is the `consignment.get` result. Consignment is decoded from the protocol's
// base64 field.
type ConsignmentResult struct {
	Consignment []byte
	TxID        string
	Vout        *uint32
}

// ServerInfo queries the gateway's protocol/software version.
func (c *TransportClient) ServerInfo(ctx context.Context) (*ServerInfo, error) {
	var info ServerInfo
	if err := c.call(ctx, "server.info", map[string]any{}, &info); err != nil {
		return nil, err
	}
	if info.ProtocolVersion != transportProtocolVersion {
		// Not fatal for a client (the method set is narrow), but the operator must see it.
		log.Warn("transport gateway reports an unexpected protocol version",
			"endpoint", c.endpoint, "want", transportProtocolVersion, "got", info.ProtocolVersion)
	}
	return &info, nil
}

// ConsignmentGet fetches the consignment delivered for recipientID.
// It returns ErrConsignmentNotFound when the gateway has none yet.
func (c *TransportClient) ConsignmentGet(ctx context.Context, recipientID string) (*ConsignmentResult, error) {
	if recipientID == "" {
		return nil, errors.New("recipient_id empty")
	}
	var raw struct {
		Consignment string  `json:"consignment"`
		TxID        string  `json:"txid"`
		Vout        *uint32 `json:"vout"`
	}
	err := c.call(ctx, "consignment.get", map[string]any{"recipient_id": recipientID}, &raw)
	if err != nil {
		var rpcErr *TransportRPCError
		if errors.As(err, &rpcErr) && rpcErr.Code == -400 {
			return nil, ErrConsignmentNotFound
		}
		return nil, err
	}
	consignment, err := base64.StdEncoding.DecodeString(raw.Consignment)
	if err != nil {
		return nil, fmt.Errorf("gateway consignment is not valid base64: %w", err)
	}
	if len(consignment) == 0 {
		return nil, errors.New("gateway returned an empty consignment")
	}
	return &ConsignmentResult{Consignment: consignment, TxID: raw.TxID, Vout: raw.Vout}, nil
}

// AckPost records the validation outcome for recipientID. The gateway's `created` flag is
// returned as-is: false means the same ACK value was already recorded (an idempotent repeat).
func (c *TransportClient) AckPost(ctx context.Context, recipientID string, ack bool) (bool, error) {
	if recipientID == "" {
		return false, errors.New("recipient_id empty")
	}
	var created bool
	err := c.call(ctx, "ack.post", map[string]any{"recipient_id": recipientID, "ack": ack}, &created)
	if err != nil {
		return false, err
	}
	return created, nil
}

// AckGet returns the payee's verdict for recipientID. A nil verdict means the payee has not
// recorded one yet (the protocol answers null, or -400 when nothing has been posted).
func (c *TransportClient) AckGet(ctx context.Context, recipientID string) (*bool, error) {
	if recipientID == "" {
		return nil, errors.New("recipient_id empty")
	}
	raw, err := c.callRaw(ctx, "ack.get", map[string]any{"recipient_id": recipientID})
	if err != nil {
		var rpcErr *TransportRPCError
		if errors.As(err, &rpcErr) && rpcErr.Code == -400 {
			return nil, nil
		}
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var ack bool
	if err := json.Unmarshal(raw, &ack); err != nil {
		return nil, fmt.Errorf("ack.get: decode result: %w", err)
	}
	return &ack, nil
}

// ConsignmentPost delivers a consignment to the payee's endpoint for recipientID.
// The protocol carries the file as a multipart part and reports whether this post created the
// stored file; a repeated identical post answers false (idempotent).
func (c *TransportClient) ConsignmentPost(ctx context.Context, recipientID, txid string,
	vout *uint32, consignment []byte) (bool, error) {

	if recipientID == "" {
		return false, errors.New("recipient_id empty")
	}
	if txid == "" {
		return false, errors.New("txid empty")
	}
	if len(consignment) == 0 {
		return false, errors.New("consignment empty")
	}
	params := map[string]any{"recipient_id": recipientID, "txid": txid}
	if vout != nil {
		params["vout"] = *vout
	}
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return false, fmt.Errorf("encode consignment.post params: %w", err)
	}

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for _, field := range []struct{ name, value string }{
		{"jsonrpc", "2.0"},
		{"id", fmt.Sprintf("%d", c.nextID.Add(1))},
		{"method", "consignment.post"},
		{"params", string(paramsJSON)},
	} {
		if err := w.WriteField(field.name, field.value); err != nil {
			return false, fmt.Errorf("encode consignment.post field %s: %w", field.name, err)
		}
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="consignment.bin"`)
	header.Set("Content-Type", "application/octet-stream")
	part, err := w.CreatePart(header)
	if err != nil {
		return false, fmt.Errorf("encode consignment.post file part: %w", err)
	}
	if _, err := part.Write(consignment); err != nil {
		return false, fmt.Errorf("encode consignment.post body: %w", err)
	}
	if err := w.Close(); err != nil {
		return false, fmt.Errorf("encode consignment.post body: %w", err)
	}

	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, transportRequestTimeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, &body)
	if err != nil {
		return false, fmt.Errorf("build consignment.post request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Accept", "application/json")

	raw, err := c.doRequest(req, "consignment.post")
	if err != nil {
		return false, err
	}
	var created bool
	if len(raw) == 0 || string(raw) == "null" {
		return false, nil
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		return false, fmt.Errorf("consignment.post: decode result: %w", err)
	}
	return created, nil
}

// call performs one JSON-RPC 2.0 request and decodes its result into out.
func (c *TransportClient) call(ctx context.Context, method string, params any, out any) error {
	raw, err := c.callRaw(ctx, method, params)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if len(raw) == 0 || string(raw) == "null" {
		return fmt.Errorf("%s: empty result", method)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: decode result: %w", method, err)
	}
	return nil
}

// callRaw performs one JSON-RPC request and returns the raw `result` (which may be null).
func (c *TransportClient) callRaw(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, transportRequestTimeout)
		defer cancel()
	}
	body, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      uint64 `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{
		JSONRPC: "2.0",
		ID:      c.nextID.Add(1),
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return nil, fmt.Errorf("encode %s request: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build %s request: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return c.doRequest(req, method)
}

// doRequest sends a prepared request and unwraps the JSON-RPC envelope.
func (c *TransportClient) doRequest(req *http.Request, method string) (json.RawMessage, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, transportResponseLimit))
	if err != nil {
		return nil, fmt.Errorf("%s: read response: %w", method, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &transportHTTPError{StatusCode: resp.StatusCode, Reason: truncate(string(data), 256)}
	}
	var rpcResp struct {
		JSONRPC string          `json:"jsonrpc"`
		Result  json.RawMessage `json:"result"`
		Error   *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &rpcResp); err != nil {
		return nil, &transportHTTPError{StatusCode: resp.StatusCode, Reason: "malformed JSON-RPC response: " + err.Error()}
	}
	if rpcResp.Error != nil {
		return nil, &TransportRPCError{Code: rpcResp.Error.Code, Message: rpcResp.Error.Message}
	}
	return rpcResp.Result, nil
}

// IsRetryableTransportError reports whether a gateway error is worth retrying. Network errors,
// non-2xx HTTP responses and the gateway's internal-error code are transient; protocol-level
// answers (missing/invalid parameters, conflicting ACK, not found) are not.
func IsRetryableTransportError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrConsignmentNotFound) {
		return false
	}
	var rpcErr *TransportRPCError
	if errors.As(err, &rpcErr) {
		return rpcErr.Code == -32603
	}
	// transportHTTPError and wrapped network errors (url.Error, context deadline, EOF) are all
	// potentially transient.
	return true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
