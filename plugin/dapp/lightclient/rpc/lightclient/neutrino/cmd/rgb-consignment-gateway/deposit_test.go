package main

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/33cn/chain33/common/address"
	_ "github.com/33cn/chain33/system/address/btc"
	"github.com/33cn/plugin/plugin/dapp/rgbx/depositrequest"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
)

const (
	testChainTitle = "user.p.test."
	testBridgeTok  = "0123456789abcdef"
)

func newDepositTestGateway(t *testing.T) *httptest.Server {
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
	return ts
}

type signedBody struct {
	RequestID   string `json:"request_id"`
	Chain33Addr string `json:"chain33_addr"`
	AssetSymbol string `json:"asset_symbol"`
	Amount      int64  `json:"amount"`
	Timestamp   int64  `json:"timestamp"`
	PublicKey   string `json:"pubkey"`
	Signature   string `json:"signature"`
}

func signDeposit(t *testing.T, requestID string, amount int64) signedBody {
	t.Helper()
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatalf("new key: %v", err)
	}
	pub := priv.PubKey().SerializeCompressed()
	req := depositrequest.Request{
		RequestID:   requestID,
		Chain33Addr: address.PubKeyToAddr(-1, pub),
		AssetSymbol: "RGB20_USDT",
		Amount:      amount,
		Timestamp:   1_700_000_000,
	}
	sig := ecdsa.Sign(priv, depositrequest.MessageHash(testChainTitle, req)).Serialize()
	return signedBody{
		RequestID:   req.RequestID,
		Chain33Addr: req.Chain33Addr,
		AssetSymbol: req.AssetSymbol,
		Amount:      req.Amount,
		Timestamp:   req.Timestamp,
		PublicKey:   hex.EncodeToString(pub),
		Signature:   hex.EncodeToString(sig),
	}
}

func postDepositJSON(t *testing.T, url, token string, body any) (*http.Response, []byte) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp, respBody
}

func TestDepositRequestLifecycle(t *testing.T) {
	ts := newDepositTestGateway(t)
	body := signDeposit(t, "req-one", 1000)

	resp, out := postDepositJSON(t, ts.URL+"/deposit/requests", "", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post request: %d %s", resp.StatusCode, out)
	}

	getResp, err := http.Get(ts.URL + "/deposit/requests/req-one")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var got depositRequestWire
	_ = json.NewDecoder(getResp.Body).Decode(&got)
	getResp.Body.Close()
	if got.Status != depositRequestStatusPending || got.Invoice != "" {
		t.Fatalf("request before invoice = %+v", got)
	}

	// Idempotent repeat: same signed request, created=false.
	_, out = postDepositJSON(t, ts.URL+"/deposit/requests", "", body)
	if !strings.Contains(string(out), `"created":false`) {
		t.Fatalf("repeat request = %s", out)
	}

	// The bridge publishes the invoice.
	resp, out = postDepositJSON(t, ts.URL+"/bridge/deposit/invoices", testBridgeTok, map[string]any{
		"request_id": "req-one", "invoice": "rgb:test", "receive_id": "rcv-1", "recipient_id": "bcrt:bcrt1qtest",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post invoice: %d %s", resp.StatusCode, out)
	}
	getResp, _ = http.Get(ts.URL + "/deposit/requests/req-one")
	_ = json.NewDecoder(getResp.Body).Decode(&got)
	getResp.Body.Close()
	if got.Status != depositRequestStatusInvoiced || got.Invoice != "rgb:test" || got.ReceiveID != "rcv-1" {
		t.Fatalf("request after invoice = %+v", got)
	}

	// A different invoice for the same request is a conflict (the user may have paid the first).
	resp, _ = postDepositJSON(t, ts.URL+"/bridge/deposit/invoices", testBridgeTok, map[string]any{
		"request_id": "req-one", "invoice": "rgb:other", "receive_id": "rcv-2",
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("conflicting invoice: %d", resp.StatusCode)
	}
}

func TestDepositRequestRejectsTampering(t *testing.T) {
	ts := newDepositTestGateway(t)
	body := signDeposit(t, "req-tamper", 1000)
	body.Amount = 9999
	resp, out := postDepositJSON(t, ts.URL+"/deposit/requests", "", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("tampered amount accepted: %d %s", resp.StatusCode, out)
	}
}

func TestBridgeEndpointsRequireToken(t *testing.T) {
	ts := newDepositTestGateway(t)
	resp, _ := postDepositJSON(t, ts.URL+"/bridge/deposit/invoices", "", map[string]any{"request_id": "x"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invoice without token: %d", resp.StatusCode)
	}
	resp, _ = postDepositJSON(t, ts.URL+"/bridge/deposit/invoices", "wrong-token-0000", map[string]any{"request_id": "x"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invoice with wrong token: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/bridge/deposit/requests/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("stream without token: %d", resp.StatusCode)
	}
}

func TestBridgeStreamReplaysAndPushes(t *testing.T) {
	ts := newDepositTestGateway(t)

	// Backlog: created before the bridge connects.
	first := signDeposit(t, "req-backlog", 100)
	if resp, out := postDepositJSON(t, ts.URL+"/deposit/requests", "", first); resp.StatusCode != http.StatusOK {
		t.Fatalf("post backlog: %d %s", resp.StatusCode, out)
	}

	streamReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/bridge/deposit/requests/stream", nil)
	streamReq.Header.Set("Authorization", "Bearer "+testBridgeTok)
	streamResp, err := http.DefaultClient.Do(streamReq)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer streamResp.Body.Close()
	if streamResp.StatusCode != http.StatusOK {
		t.Fatalf("stream status: %d", streamResp.StatusCode)
	}
	reader := bufio.NewReader(streamResp.Body)
	if got := readStreamEvent(t, reader); got["request_id"] != "req-backlog" {
		t.Fatalf("backlog event = %v", got)
	}

	// Live: created after the bridge connected.
	second := signDeposit(t, "req-live", 200)
	if resp, out := postDepositJSON(t, ts.URL+"/deposit/requests", "", second); resp.StatusCode != http.StatusOK {
		t.Fatalf("post live: %d %s", resp.StatusCode, out)
	}
	if got := readStreamEvent(t, reader); got["request_id"] != "req-live" {
		t.Fatalf("live event = %v", got)
	}
}

// readStreamEvent reads one SSE data event, skipping comments (pings).
func readStreamEvent(t *testing.T, r *bufio.Reader) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read stream: %v", err)
		}
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		return event
	}
	t.Fatal("no event within the deadline")
	return nil
}
