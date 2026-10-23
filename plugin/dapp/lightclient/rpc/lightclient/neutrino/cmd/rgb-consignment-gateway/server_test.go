package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testTxID  = "9ccf8491394e6aa2bec99f08ee8a856879505e07791b3aac79f56177808eb49f"
	testTxID2 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type testRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type testRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *testRPCError   `json:"error"`
}

func testConfig(t *testing.T) config {
	t.Helper()
	return config{
		listen:    "127.0.0.1:0",
		dataDir:   t.TempDir(),
		path:      defaultPath,
		maxSize:   1 << 20,
		ttl:       time.Hour,
		rateLimit: 0,
	}
}

func newTestServer(t *testing.T, mutate func(*config)) (*gateway, *httptest.Server, *bytes.Buffer) {
	t.Helper()
	cfg := testConfig(t)
	if mutate != nil {
		mutate(&cfg)
	}
	return startGateway(t, cfg)
}

func startGateway(t *testing.T, cfg config) (*gateway, *httptest.Server, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	g, err := newGateway(cfg, log.New(&buf, "", 0))
	require.NoError(t, err)
	ts := httptest.NewServer(g.handler())
	t.Cleanup(ts.Close)
	return g, ts, &buf
}

func doRequest(t *testing.T, ts *httptest.Server, req *http.Request) (*http.Response, testRPCResponse) {
	t.Helper()
	res, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	var out testRPCResponse
	if len(body) > 0 && strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		require.NoError(t, json.Unmarshal(body, &out), "response body: %s", body)
	}
	return res, out
}

func postJSON(t *testing.T, ts *httptest.Server, body string) (*http.Response, testRPCResponse) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+defaultPath, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	return doRequest(t, ts, req)
}

func postJSONHeader(t *testing.T, ts *httptest.Server, body string, headers map[string]string) (*http.Response, testRPCResponse) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+defaultPath, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return doRequest(t, ts, req)
}

func rpcCall(t *testing.T, ts *httptest.Server, method string, params any) (*http.Response, testRPCResponse) {
	t.Helper()
	payload := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		payload["params"] = params
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	return postJSON(t, ts, string(body))
}

func postMultipart(t *testing.T, ts *httptest.Server, fields map[string]string, file []byte, includeFile bool) (*http.Response, testRPCResponse) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		require.NoError(t, mw.WriteField(k, fields[k]))
	}
	if includeFile {
		fw, err := mw.CreateFormFile("file", "consignment.bin")
		require.NoError(t, err)
		_, err = fw.Write(file)
		require.NoError(t, err)
	}
	require.NoError(t, mw.Close())

	req, err := http.NewRequest(http.MethodPost, ts.URL+defaultPath, &buf)
	require.NoError(t, err)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return doRequest(t, ts, req)
}

func consignmentFields(recipient, txid string, vout *int) map[string]string {
	params := map[string]any{"recipient_id": recipient, "txid": txid}
	if vout != nil {
		params["vout"] = *vout
	}
	raw, _ := json.Marshal(params)
	return map[string]string{
		"jsonrpc": "2.0",
		"id":      "1",
		"method":  "consignment.post",
		"params":  string(raw),
	}
}

func requireResult[T any](t *testing.T, resp testRPCResponse, want T) {
	t.Helper()
	require.Nil(t, resp.Error, "unexpected error: %+v", resp.Error)
	var got T
	require.NoError(t, json.Unmarshal(resp.Result, &got), "result: %s", resp.Result)
	require.Equal(t, want, got)
}

func requireRPCError(t *testing.T, resp testRPCResponse, code int) *testRPCError {
	t.Helper()
	require.NotNil(t, resp.Error, "expected error %d", code)
	require.Equal(t, code, resp.Error.Code)
	require.NotEmpty(t, resp.Error.Message)
	return resp.Error
}

func TestServerInfo(t *testing.T) {
	_, ts, _ := newTestServer(t, nil)

	res, resp := rpcCall(t, ts, "server.info", nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, "2.0", resp.JSONRPC)
	require.Nil(t, resp.Error)

	var info struct {
		ProtocolVersion string `json:"protocol_version"`
		Version         string `json:"version"`
		Uptime          int64  `json:"uptime"`
	}
	require.NoError(t, json.Unmarshal(resp.Result, &info))
	assert.Equal(t, "0.2", info.ProtocolVersion)
	assert.NotEmpty(t, info.Version)
	assert.GreaterOrEqual(t, info.Uptime, int64(0))
}

func TestConsignmentPostIdempotencyAndGet(t *testing.T) {
	_, ts, _ := newTestServer(t, nil)
	consignment := []byte("raw-consignment-bytes")
	vout := 3

	_, resp := postMultipart(t, ts, consignmentFields("recipient-one", testTxID, &vout), consignment, true)
	requireResult(t, resp, true)

	_, resp = postMultipart(t, ts, consignmentFields("recipient-one", testTxID, &vout), consignment, true)
	requireResult(t, resp, false)

	_, resp = rpcCall(t, ts, "consignment.get", map[string]any{"recipient_id": "recipient-one"})
	require.Nil(t, resp.Error)
	var got struct {
		Consignment string `json:"consignment"`
		TxID        string `json:"txid"`
		Vout        *int   `json:"vout"`
	}
	require.NoError(t, json.Unmarshal(resp.Result, &got))
	assert.Equal(t, base64.StdEncoding.EncodeToString(consignment), got.Consignment)
	assert.Equal(t, testTxID, got.TxID)
	require.NotNil(t, got.Vout)
	assert.Equal(t, 3, *got.Vout)

	// A different file for the same recipient ID must not overwrite.
	_, resp = postMultipart(t, ts, consignmentFields("recipient-one", testTxID, &vout), []byte("other-bytes"), true)
	requireRPCError(t, resp, -101)

	_, resp = rpcCall(t, ts, "consignment.get", map[string]any{"recipient_id": "recipient-one"})
	require.NoError(t, json.Unmarshal(resp.Result, &got))
	assert.Equal(t, base64.StdEncoding.EncodeToString(consignment), got.Consignment)

	// Without vout the response omits the field entirely.
	_, resp = postMultipart(t, ts, consignmentFields("recipient-two", testTxID2, nil), []byte("second"), true)
	requireResult(t, resp, true)
	_, resp = rpcCall(t, ts, "consignment.get", map[string]any{"recipient_id": "recipient-two"})
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(resp.Result, &fields))
	assert.NotContains(t, fields, "vout")
	assert.Equal(t, "second", mustDecodeBase64(t, fields["consignment"]))
}

func TestConsignmentPostValidation(t *testing.T) {
	_, ts, _ := newTestServer(t, nil)
	voutZero := 0
	// vout 0 must be preserved, not dropped by omitempty.
	_, resp := postMultipart(t, ts, consignmentFields("vout-zero", testTxID, &voutZero), []byte("x"), true)
	requireResult(t, resp, true)
	_, resp = rpcCall(t, ts, "consignment.get", map[string]any{"recipient_id": "vout-zero"})
	var got struct {
		Vout *int `json:"vout"`
	}
	require.NoError(t, json.Unmarshal(resp.Result, &got))
	require.NotNil(t, got.Vout)
	assert.Equal(t, 0, *got.Vout)

	tests := []struct {
		name   string
		params map[string]any
		code   int
	}{
		{"missing recipient", map[string]any{"txid": testTxID}, -302},
		{"empty recipient", map[string]any{"recipient_id": "", "txid": testTxID}, -202},
		{"recipient too long", map[string]any{"recipient_id": strings.Repeat("a", maxRecipientIDLen+1), "txid": testTxID}, -202},
		{"missing txid", map[string]any{"recipient_id": "r"}, -304},
		{"short txid", map[string]any{"recipient_id": "r", "txid": "abcd"}, -203},
		{"non-hex txid", map[string]any{"recipient_id": "r", "txid": strings.Repeat("g", 64)}, -203},
		{"negative vout", map[string]any{"recipient_id": "r", "txid": testTxID, "vout": -1}, -204},
		{"fractional vout", map[string]any{"recipient_id": "r", "txid": testTxID, "vout": 1.5}, -204},
		{"string vout", map[string]any{"recipient_id": "r", "txid": testTxID, "vout": "0"}, -204},
		{"bool vout", map[string]any{"recipient_id": "r", "txid": testTxID, "vout": true}, -204},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, resp := rpcCall(t, ts, "consignment.post", tc.params)
			requireRPCError(t, resp, tc.code)
		})
	}

	t.Run("missing file", func(t *testing.T) {
		_, resp := rpcCall(t, ts, "consignment.post", map[string]any{"recipient_id": "r", "txid": testTxID})
		requireRPCError(t, resp, -303)
	})

	t.Run("empty file", func(t *testing.T) {
		_, resp := postMultipart(t, ts, consignmentFields("r", testTxID, nil), nil, true)
		requireRPCError(t, resp, -303)
	})

	t.Run("oversized file", func(t *testing.T) {
		_, small, _ := newTestServer(t, func(c *config) { c.maxSize = 16 })
		res, resp := postMultipart(t, small, consignmentFields("r", testTxID, nil), bytes.Repeat([]byte("x"), 17), true)
		assert.Equal(t, http.StatusRequestEntityTooLarge, res.StatusCode)
		requireRPCError(t, resp, -32600)
		assert.Contains(t, resp.Error.Message, "too large")
	})
}

func TestParamsEncodings(t *testing.T) {
	_, ts, _ := newTestServer(t, nil)

	// JSON-encoded params string on a normal JSON body.
	_, resp := postMultipart(t, ts, consignmentFields("r1", testTxID, nil), []byte("data"), true)
	requireResult(t, resp, true)
	_, resp = postJSON(t, ts, `{"jsonrpc":"2.0","id":"str","method":"ack.get","params":"{\"recipient_id\":\"r1\"}"}`)
	require.Nil(t, resp.Error)
	assert.Equal(t, "null", string(resp.Result))
	assert.JSONEq(t, `"str"`, string(resp.ID))

	// Multipart with flattened params[key] fields and a numeric-looking
	// recipient ID that must stay a string.
	fields := map[string]string{
		"jsonrpc":              "2.0",
		"id":                   "7",
		"method":               "consignment.post",
		"params[recipient_id]": "1234567890",
		"params[txid]":         testTxID,
		"params[vout]":         "2",
	}
	_, resp = postMultipart(t, ts, fields, []byte("flattened"), true)
	requireResult(t, resp, true)

	_, resp = rpcCall(t, ts, "consignment.get", map[string]any{"recipient_id": "1234567890"})
	require.Nil(t, resp.Error)
	var got struct {
		Consignment string `json:"consignment"`
		Vout        *int   `json:"vout"`
	}
	require.NoError(t, json.Unmarshal(resp.Result, &got))
	assert.Equal(t, "flattened", mustDecodeBase64String(t, got.Consignment))
	require.NotNil(t, got.Vout)
	assert.Equal(t, 2, *got.Vout)
}

func TestMultipartIDNull(t *testing.T) {
	_, ts, _ := newTestServer(t, nil)
	fields := consignmentFields("id-null", testTxID, nil)
	fields["id"] = "null"
	_, resp := postMultipart(t, ts, fields, []byte("data"), true)
	requireResult(t, resp, true)
	assert.Equal(t, "null", string(resp.ID))
}

func TestAckLifecycle(t *testing.T) {
	_, ts, _ := newTestServer(t, nil)
	recipient := "ack-recipient"

	_, resp := rpcCall(t, ts, "ack.post", map[string]any{"recipient_id": recipient, "ack": true})
	requireRPCError(t, resp, -300)

	_, resp = postMultipart(t, ts, consignmentFields(recipient, testTxID, nil), []byte("consignment"), true)
	requireResult(t, resp, true)

	_, resp = rpcCall(t, ts, "ack.get", map[string]any{"recipient_id": recipient})
	require.Nil(t, resp.Error)
	assert.Equal(t, "null", string(resp.Result))

	_, resp = rpcCall(t, ts, "ack.post", map[string]any{"recipient_id": recipient, "ack": true})
	requireResult(t, resp, true)

	_, resp = rpcCall(t, ts, "ack.get", map[string]any{"recipient_id": recipient})
	requireResult(t, resp, true)

	_, resp = rpcCall(t, ts, "ack.post", map[string]any{"recipient_id": recipient, "ack": true})
	requireResult(t, resp, false)

	_, resp = rpcCall(t, ts, "ack.post", map[string]any{"recipient_id": recipient, "ack": false})
	requireRPCError(t, resp, -100)

	_, resp = rpcCall(t, ts, "ack.get", map[string]any{"recipient_id": recipient})
	requireResult(t, resp, true)

	_, resp = rpcCall(t, ts, "ack.get", map[string]any{"recipient_id": "unknown"})
	requireRPCError(t, resp, -400)

	_, resp = rpcCall(t, ts, "ack.post", map[string]any{"recipient_id": recipient})
	requireRPCError(t, resp, -300)

	_, resp = rpcCall(t, ts, "ack.post", map[string]any{"recipient_id": recipient, "ack": "yes"})
	requireRPCError(t, resp, -200)
}

func TestProtocolErrors(t *testing.T) {
	_, ts, _ := newTestServer(t, nil)

	t.Run("unknown method", func(t *testing.T) {
		_, resp := rpcCall(t, ts, "does.not.exist", nil)
		requireRPCError(t, resp, -32601)
	})
	t.Run("media not implemented", func(t *testing.T) {
		_, resp := rpcCall(t, ts, "media.post", map[string]any{"attachment_id": "x"})
		requireRPCError(t, resp, -32601)
		_, resp = rpcCall(t, ts, "media.get", map[string]any{"attachment_id": "x"})
		requireRPCError(t, resp, -32601)
	})
	t.Run("batch rejected", func(t *testing.T) {
		_, resp := postJSON(t, ts, `[{"jsonrpc":"2.0","id":1,"method":"server.info"}]`)
		requireRPCError(t, resp, -32600)
		assert.Contains(t, resp.Error.Message, "Batch")
	})
	t.Run("malformed json", func(t *testing.T) {
		_, resp := postJSON(t, ts, `{"jsonrpc":`)
		requireRPCError(t, resp, -32700)
	})
	t.Run("non object json", func(t *testing.T) {
		_, resp := postJSON(t, ts, `"hello"`)
		requireRPCError(t, resp, -32600)
	})
	t.Run("missing method", func(t *testing.T) {
		_, resp := postJSON(t, ts, `{"jsonrpc":"2.0","id":1}`)
		requireRPCError(t, resp, -32600)
	})
	t.Run("array params", func(t *testing.T) {
		_, resp := postJSON(t, ts, `{"jsonrpc":"2.0","id":1,"method":"server.info","params":[]}`)
		requireRPCError(t, resp, -32602)
	})
	t.Run("get not allowed", func(t *testing.T) {
		res, err := ts.Client().Get(ts.URL + defaultPath)
		require.NoError(t, err)
		defer res.Body.Close()
		assert.Equal(t, http.StatusMethodNotAllowed, res.StatusCode)
		assert.Equal(t, "POST", res.Header.Get("Allow"))
	})
	t.Run("unknown path", func(t *testing.T) {
		res, err := ts.Client().Post(ts.URL+"/unknown", "application/json", strings.NewReader(`{}`))
		require.NoError(t, err)
		defer res.Body.Close()
		assert.Equal(t, http.StatusNotFound, res.StatusCode)
	})
}

func TestRateLimit(t *testing.T) {
	_, ts, _ := newTestServer(t, func(c *config) { c.rateLimit = 2 })
	body := `{"jsonrpc":"2.0","id":1,"method":"server.info"}`

	for i := 0; i < 2; i++ {
		res, _ := postJSON(t, ts, body)
		require.Equal(t, http.StatusOK, res.StatusCode)
	}
	res, _ := postJSON(t, ts, body)
	assert.Equal(t, http.StatusTooManyRequests, res.StatusCode)
	assert.Equal(t, "60", res.Header.Get("Retry-After"))
}

func TestRateLimitTrustProxy(t *testing.T) {
	_, ts, _ := newTestServer(t, func(c *config) {
		c.rateLimit = 1
		c.trustProxy = true
	})
	body := `{"jsonrpc":"2.0","id":1,"method":"server.info"}`

	res, _ := postJSONHeader(t, ts, body, map[string]string{"X-Forwarded-For": "1.1.1.1"})
	require.Equal(t, http.StatusOK, res.StatusCode)
	res, _ = postJSONHeader(t, ts, body, map[string]string{"X-Forwarded-For": "2.2.2.2"})
	require.Equal(t, http.StatusOK, res.StatusCode)
	res, _ = postJSONHeader(t, ts, body, map[string]string{"X-Forwarded-For": "1.1.1.1"})
	assert.Equal(t, http.StatusTooManyRequests, res.StatusCode)
}

func TestTTLExpiryAndCleanup(t *testing.T) {
	g, ts, _ := newTestServer(t, nil)
	base := time.Now()
	g.store.now = func() time.Time { return base }

	_, resp := postMultipart(t, ts, consignmentFields("ttl-recipient", testTxID, nil), []byte("old"), true)
	requireResult(t, resp, true)
	_, resp = rpcCall(t, ts, "ack.post", map[string]any{"recipient_id": "ttl-recipient", "ack": true})
	requireResult(t, resp, true)

	// Past the TTL the entry is already invisible, even before cleanup runs.
	g.store.now = func() time.Time { return base.Add(2 * time.Hour) }
	_, resp = rpcCall(t, ts, "consignment.get", map[string]any{"recipient_id": "ttl-recipient"})
	requireRPCError(t, resp, -400)
	_, resp = rpcCall(t, ts, "ack.get", map[string]any{"recipient_id": "ttl-recipient"})
	requireRPCError(t, resp, -400)

	removed, err := g.store.cleanup()
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
	assertNoFiles(t, filepath.Join(g.store.dir, "blobs"))
	assertNoFiles(t, filepath.Join(g.store.dir, "index"))

	// After expiry the recipient ID can be reused with new content.
	_, resp = postMultipart(t, ts, consignmentFields("ttl-recipient", testTxID2, nil), []byte("new"), true)
	requireResult(t, resp, true)
}

func TestExpiredEntryReplacedOnPost(t *testing.T) {
	g, ts, _ := newTestServer(t, nil)
	base := time.Now()
	g.store.now = func() time.Time { return base }

	_, resp := postMultipart(t, ts, consignmentFields("reuse", testTxID, nil), []byte("first"), true)
	requireResult(t, resp, true)

	g.store.now = func() time.Time { return base.Add(2 * time.Hour) }
	_, resp = postMultipart(t, ts, consignmentFields("reuse", testTxID2, nil), []byte("second"), true)
	requireResult(t, resp, true)

	_, resp = rpcCall(t, ts, "consignment.get", map[string]any{"recipient_id": "reuse"})
	require.Nil(t, resp.Error)
	var got struct {
		Consignment string `json:"consignment"`
		TxID        string `json:"txid"`
	}
	require.NoError(t, json.Unmarshal(resp.Result, &got))
	assert.Equal(t, "second", mustDecodeBase64String(t, got.Consignment))
	assert.Equal(t, testTxID2, got.TxID)

	_, err := g.store.cleanup()
	require.NoError(t, err)
	entries, err := os.ReadDir(filepath.Join(g.store.dir, "blobs"))
	require.NoError(t, err)
	assert.Len(t, entries, 1, "only the live blob should remain")
}

func TestRestartRecovery(t *testing.T) {
	cfg := testConfig(t)
	consignment := []byte("durable-consignment")

	_, ts1, _ := startGateway(t, cfg)
	_, resp := postMultipart(t, ts1, consignmentFields("persisted", testTxID, nil), consignment, true)
	requireResult(t, resp, true)
	_, resp = rpcCall(t, ts1, "ack.post", map[string]any{"recipient_id": "persisted", "ack": true})
	requireResult(t, resp, true)
	ts1.Close()

	_, ts2, _ := startGateway(t, cfg)
	_, resp = postMultipart(t, ts2, consignmentFields("persisted", testTxID, nil), consignment, true)
	requireResult(t, resp, false)

	// The ACK is durable. The consignment itself is consumed once ACKed (a recipient may have
	// several outstanding consignments under one recipient_id, so get returns the next
	// undelivered one, not a file that was already validated).
	_, resp = rpcCall(t, ts2, "ack.get", map[string]any{"recipient_id": "persisted"})
	requireResult(t, resp, true)
	_, resp = rpcCall(t, ts2, "consignment.get", map[string]any{"recipient_id": "persisted"})
	require.NotNil(t, resp.Error)
	assert.Equal(t, -400, resp.Error.Code)
}

func TestContentAddressedDedup(t *testing.T) {
	g, ts, _ := newTestServer(t, nil)
	data := []byte("identical-bytes")

	_, resp := postMultipart(t, ts, consignmentFields("dedup-a", testTxID, nil), data, true)
	requireResult(t, resp, true)
	_, resp = postMultipart(t, ts, consignmentFields("dedup-b", testTxID2, nil), data, true)
	requireResult(t, resp, true)

	entries, err := os.ReadDir(filepath.Join(g.store.dir, "blobs"))
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

func TestAccessLogMasking(t *testing.T) {
	_, ts, buf := newTestServer(t, nil)
	recipient := "0123456789abcdef"
	secret := "SECRET-CONSIGNMENT-BYTES"

	_, resp := postMultipart(t, ts, consignmentFields(recipient, testTxID, nil), []byte(secret), true)
	requireResult(t, resp, true)

	out := buf.String()
	assert.Contains(t, out, "method=consignment.post")
	assert.Contains(t, out, "result=ok")
	assert.Contains(t, out, "recipient=01234567***")
	assert.NotContains(t, out, recipient)
	assert.NotContains(t, out, secret)
	assert.NotContains(t, out, testTxID)
}

func TestMaskRecipient(t *testing.T) {
	assert.Equal(t, "-", maskRecipient(""))
	assert.Equal(t, "-", maskRecipient("-"))
	assert.Equal(t, "a***", maskRecipient("abc"))
	assert.Equal(t, "abcd***", maskRecipient("abcdefgh"))
	assert.Equal(t, "abcdefgh***", maskRecipient("abcdefghi"))
}

func assertNoFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func mustDecodeBase64(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var s string
	require.NoError(t, json.Unmarshal(raw, &s))
	return mustDecodeBase64String(t, s)
}

func mustDecodeBase64String(t *testing.T, encoded string) string {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(encoded)
	require.NoError(t, err)
	return string(data)
}
