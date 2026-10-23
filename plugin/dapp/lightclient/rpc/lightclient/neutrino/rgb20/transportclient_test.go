package rgb20

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// newJSONRPCServer starts a gateway stub that answers every request through fn.
func newJSONRPCServer(t *testing.T, fn func(method string, params map[string]any) (any, *TransportRPCError)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  map[string]any  `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		result, rpcErr := fn(req.Method, req.Params)
		w.Header().Set("Content-Type", "application/json")
		if rpcErr != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]any{"code": rpcErr.Code, "message": rpcErr.Message},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestTransportClient(t *testing.T, endpoint string) *TransportClient {
	t.Helper()
	c, err := NewTransportClient(endpoint, nil)
	require.NoError(t, err)
	return c
}

func TestTransportClient_ServerInfo(t *testing.T) {
	srv := newJSONRPCServer(t, func(method string, _ map[string]any) (any, *TransportRPCError) {
		if method != "server.info" {
			t.Errorf("unexpected method %q", method)
		}
		return map[string]any{"protocol_version": "0.2", "version": "0.2.0", "uptime": 12}, nil
	})
	info, err := newTestTransportClient(t, srv.URL).ServerInfo(context.Background())
	require.NoError(t, err)
	require.Equal(t, "0.2", info.ProtocolVersion)
	require.Equal(t, "0.2.0", info.Version)
	require.EqualValues(t, 12, info.Uptime)
}

func TestTransportClient_ConsignmentGet(t *testing.T) {
	payload := []byte("consignment-bytes")
	srv := newJSONRPCServer(t, func(method string, params map[string]any) (any, *TransportRPCError) {
		if method != "consignment.get" {
			t.Errorf("unexpected method %q", method)
		}
		if params["recipient_id"] != "bcrt:recipient" {
			t.Errorf("unexpected recipient_id %v", params["recipient_id"])
		}
		return map[string]any{
			"consignment": base64.StdEncoding.EncodeToString(payload),
			"txid":        strings.Repeat("ab", 32),
			"vout":        1,
		}, nil
	})
	got, err := newTestTransportClient(t, srv.URL).ConsignmentGet(context.Background(), "bcrt:recipient")
	require.NoError(t, err)
	require.Equal(t, payload, got.Consignment)
	require.Equal(t, strings.Repeat("ab", 32), got.TxID)
	require.NotNil(t, got.Vout)
	require.EqualValues(t, 1, *got.Vout)
}

func TestTransportClient_ConsignmentGetNotFound(t *testing.T) {
	srv := newJSONRPCServer(t, func(string, map[string]any) (any, *TransportRPCError) {
		return nil, &TransportRPCError{Code: -400, Message: "Consignment file not found"}
	})
	_, err := newTestTransportClient(t, srv.URL).ConsignmentGet(context.Background(), "r")
	require.ErrorIs(t, err, ErrConsignmentNotFound)
	require.False(t, IsRetryableTransportError(err), "absent consignment is a normal state, not a retryable failure")
}

func TestTransportClient_AckPost(t *testing.T) {
	var mu sync.Mutex
	var posted []bool
	created := true
	srv := newJSONRPCServer(t, func(method string, params map[string]any) (any, *TransportRPCError) {
		if method != "ack.post" {
			t.Errorf("unexpected method %q", method)
		}
		if params["recipient_id"] != "bcrt:recipient" {
			t.Errorf("unexpected recipient_id %v", params["recipient_id"])
		}
		ack, ok := params["ack"].(bool)
		if !ok {
			t.Errorf("ack is not a bool: %v", params["ack"])
		}
		mu.Lock()
		posted = append(posted, ack)
		c := created
		created = false // same value repeated: the gateway answers false
		mu.Unlock()
		return c, nil
	})
	c := newTestTransportClient(t, srv.URL)
	first, err := c.AckPost(context.Background(), "bcrt:recipient", "", true)
	require.NoError(t, err)
	require.True(t, first)
	second, err := c.AckPost(context.Background(), "bcrt:recipient", "", true)
	require.NoError(t, err)
	require.False(t, second)
	mu.Lock()
	require.Equal(t, []bool{true, true}, posted)
	mu.Unlock()
}

func TestTransportClient_ProtocolErrorIsNotRetryable(t *testing.T) {
	srv := newJSONRPCServer(t, func(string, map[string]any) (any, *TransportRPCError) {
		return nil, &TransportRPCError{Code: -100, Message: "Cannot change ACK"}
	})
	_, err := newTestTransportClient(t, srv.URL).AckPost(context.Background(), "r", "", false)
	require.Error(t, err)
	var rpcErr *TransportRPCError
	require.ErrorAs(t, err, &rpcErr)
	require.Equal(t, -100, rpcErr.Code)
	require.False(t, IsRetryableTransportError(err))
}

func TestTransportClient_HTTP5xxIsRetryable(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "unavailable", status)
			}))
			defer srv.Close()
			_, err := newTestTransportClient(t, srv.URL).ConsignmentGet(context.Background(), "r")
			require.Error(t, err)
			require.True(t, IsRetryableTransportError(err))
		})
	}
}

func TestTransportClient_RejectsBadEndpoint(t *testing.T) {
	for _, endpoint := range []string{"", "not-a-url", "ftp://gw:21/0.2/json-rpc", "http://"} {
		_, err := NewTransportClient(endpoint, nil)
		require.Error(t, err, "endpoint %q", endpoint)
	}
}

func TestTransportClient_EmptyRecipient(t *testing.T) {
	c := newTestTransportClient(t, "http://127.0.0.1:1/0.2/json-rpc")
	_, err := c.ConsignmentGet(context.Background(), "")
	require.Error(t, err)
	_, err = c.AckPost(context.Background(), "", "", true)
	require.Error(t, err)
}

var _ transportGateway = (*TransportClient)(nil)
