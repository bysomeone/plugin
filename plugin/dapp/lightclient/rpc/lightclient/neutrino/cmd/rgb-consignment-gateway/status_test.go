package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func getDepositStatus(t *testing.T, ts string, id string) (*http.Response, depositStatusWire) {
	t.Helper()
	resp, err := http.Get(ts + "/deposit/requests/" + id + "/status")
	require.NoError(t, err)
	defer resp.Body.Close()
	var out depositStatusWire
	if resp.StatusCode == http.StatusOK {
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &out), "body: %s", body)
	}
	return resp, out
}

func invoiceDeposit(t *testing.T, ts, id string) {
	t.Helper()
	resp, out := postDepositJSON(t, ts+"/bridge/deposit/invoices", testBridgeTok, map[string]any{
		"request_id": id, "invoice": "rgb:" + id, "receive_id": "rcv-" + id, "recipient_id": "bcrt:dest",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode, "invoice: %s", out)
}

func postDepositEvent(t *testing.T, ts, body string) {
	t.Helper()
	resp := postEvent(t, ts, body)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestDepositRequestStatus(t *testing.T) {
	ts, _ := newEventTestGateway(t)

	// pending: request accepted, bridge has not invoiced it yet.
	pending := signDeposit(t, "req-status-pending", 500)
	resp, out := postDepositJSON(t, ts.URL+"/deposit/requests", "", pending)
	require.Equal(t, http.StatusOK, resp.StatusCode, "post pending: %s", out)
	resp, status := getDepositStatus(t, ts.URL, "req-status-pending")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, depositRequestStatusPending, status.Status)
	require.Empty(t, status.Invoice)

	// invoiced: the bridge published the invoice.
	minted := signDeposit(t, "req-status-mint", 1000)
	postDepositJSON(t, ts.URL+"/deposit/requests", "", minted)
	invoiceDeposit(t, ts.URL, "req-status-mint")
	_, status = getDepositStatus(t, ts.URL, "req-status-mint")
	require.Equal(t, depositRequestStatusInvoiced, status.Status)
	require.Equal(t, "rgb:req-status-mint", status.Invoice)
	require.Equal(t, "rcv-req-status-mint", status.ReceiveID)

	// A rejected event must not win over a newer minted event for the same request.
	postDepositEvent(t, ts.URL, `{"id":"status-mint-old","type":"deposit.rejected","payload":{"request_id":"req-status-mint","reason":"stale"}}`)
	postDepositEvent(t, ts.URL, `{"id":"status-mint-new","type":"deposit.minted","payload":{"request_id":"req-status-mint","btc_txid":"aabbccdd","amount":1000,"receive_id":"rcv-req-status-mint"}}`)
	_, status = getDepositStatus(t, ts.URL, "req-status-mint")
	require.Equal(t, depositRequestStatusMinted, status.Status)
	require.Equal(t, "aabbccdd", status.BTCTxID)
	require.Equal(t, int64(1000), status.Amount)
	require.Empty(t, status.Reason)

	// rejected: the chain refused the mint.
	rejected := signDeposit(t, "req-status-reject", 700)
	postDepositJSON(t, ts.URL+"/deposit/requests", "", rejected)
	invoiceDeposit(t, ts.URL, "req-status-reject")
	postDepositEvent(t, ts.URL, `{"id":"status-reject","type":"deposit.rejected","payload":{"request_id":"req-status-reject","btc_txid":"deadbeef","amount":700,"reason":"asset identity mismatch"}}`)
	_, status = getDepositStatus(t, ts.URL, "req-status-reject")
	require.Equal(t, depositRequestStatusRejected, status.Status)
	require.Equal(t, "asset identity mismatch", status.Reason)
	require.Equal(t, "deadbeef", status.BTCTxID)

	// An event for one request must not affect another, and a pending request is never resolved
	// from the ledger (the scan only applies once the request is invoiced).
	postDepositEvent(t, ts.URL, `{"id":"status-other","type":"deposit.minted","payload":{"request_id":"req-status-pending"}}`)
	_, status = getDepositStatus(t, ts.URL, "req-status-reject")
	require.Equal(t, depositRequestStatusRejected, status.Status)
	resp, status = getDepositStatus(t, ts.URL, "req-status-pending")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, depositRequestStatusPending, status.Status, "pending requests are not resolved from the ledger")
}

func TestDepositRequestStatusUnknownID(t *testing.T) {
	ts, _ := newEventTestGateway(t)
	resp, _ := getDepositStatus(t, ts.URL, "does-not-exist")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	resp, err := http.Get(ts.URL + "/deposit/requests/does-not-exist")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	// The plain endpoint must not treat "status" as a suffix of an existing id.
	resp, err = http.Get(ts.URL + "/deposit/requests/does-not-exist/other")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHealthz(t *testing.T) {
	_, ts, _ := newTestServer(t, func(c *config) {
		c.chainTitle = testChainTitle
		c.bridgeToken = testBridgeTok
	})

	resp, err := http.Get(ts.URL + "/healthz")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	var got map[string]any
	require.NoError(t, json.Unmarshal(body, &got), "body: %s", body)
	assert.Equal(t, "ok", got["status"])
	assert.Equal(t, true, got["deposit_api"])
	assert.Equal(t, true, got["events"])
	assert.Equal(t, true, got["data_dir_writable"])

	// Nothing sensitive is exposed.
	assert.Len(t, got, 4, "healthz must only report the four documented fields: %v", got)
}

func TestHealthzDegradedWhenDataDirIsNotWritable(t *testing.T) {
	g, ts, _ := newTestServer(t, nil)
	// Removing the directory makes the write probe fail even when the test runs as root.
	require.NoError(t, os.RemoveAll(g.store.dir))

	resp, err := http.Get(ts.URL + "/healthz")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	var got map[string]any
	require.NoError(t, json.Unmarshal(body, &got), "body: %s", body)
	assert.Equal(t, "degraded", got["status"])
	assert.Equal(t, false, got["data_dir_writable"])
}
