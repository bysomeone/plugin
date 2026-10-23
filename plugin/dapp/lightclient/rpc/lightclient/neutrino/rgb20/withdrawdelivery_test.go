package rgb20

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNormalizeTransportEndpoint(t *testing.T) {
	cases := []struct {
		raw  string
		want string
		ok   bool
	}{
		{"rpcs://gw.example.com/0.2/json-rpc", "https://gw.example.com/0.2/json-rpc", true},
		{"rpc://127.0.0.1:8080/0.2/json-rpc", "http://127.0.0.1:8080/0.2/json-rpc", true},
		{"https://gw.example.com/0.2/json-rpc", "https://gw.example.com/0.2/json-rpc", true},
		{"http://gw:8080/0.2/json-rpc", "http://gw:8080/0.2/json-rpc", true},
		{"rests://gw.example.com", "", false},
		{"wss://gw.example.com", "", false},
		{"", "", false},
		{"not a url", "", false},
	}
	for _, tc := range cases {
		got, ok := NormalizeTransportEndpoint(tc.raw)
		if ok != tc.ok || got != tc.want {
			t.Errorf("NormalizeTransportEndpoint(%q) = (%q, %v), want (%q, %v)", tc.raw, got, ok, tc.want, tc.ok)
		}
	}
}

func TestWithdrawTransportEndpointsSkipsUnsupportedAndDuplicates(t *testing.T) {
	got := withdrawTransportEndpoints([]string{
		"rpcs://gw.example.com/0.2/json-rpc",
		"https://gw.example.com/0.2/json-rpc",
		"wss://gw.example.com",
	})
	want := []string{"https://gw.example.com/0.2/json-rpc"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("withdrawTransportEndpoints = %v, want %v", got, want)
	}
}

func TestSealOutpointVout(t *testing.T) {
	if v := sealOutpointVout("ab12:1"); v == nil || *v != 1 {
		t.Fatalf("sealOutpointVout = %v, want 1", v)
	}
	for _, raw := range []string{"", "ab12", "ab12:", "ab12:x", "ab12:-1"} {
		if v := sealOutpointVout(raw); v != nil {
			t.Errorf("sealOutpointVout(%q) = %v, want nil", raw, *v)
		}
	}
}

const testTxID = "b1946ac92492d2347c6235b4d2611184a2d3f5a8f0b7e6d5c4b3a29180716253"

func TestConsignmentPostSendsMultipart(t *testing.T) {
	var gotMethod, gotRecipient, gotTxID string
	var gotVout uint32
	var gotFile []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		gotMethod = r.FormValue("method")
		var params struct {
			RecipientID string `json:"recipient_id"`
			TxID        string `json:"txid"`
			Vout        uint32 `json:"vout"`
		}
		if err := json.Unmarshal([]byte(r.FormValue("params")), &params); err != nil {
			t.Errorf("decode params: %v", err)
		}
		gotRecipient, gotTxID, gotVout = params.RecipientID, params.TxID, params.Vout
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Errorf("form file: %v", err)
		} else {
			gotFile, _ = io.ReadAll(f)
			_ = f.Close()
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":true}`))
	}))
	defer ts.Close()

	client, err := NewTransportClient(ts.URL+"/0.2/json-rpc", nil)
	if err != nil {
		t.Fatalf("NewTransportClient: %v", err)
	}
	vout := uint32(1)
	created, err := client.ConsignmentPost(context.Background(), "bcrt:bcrt1qtest", testTxID, &vout,
		[]byte("consignment-bytes"))
	if err != nil {
		t.Fatalf("ConsignmentPost: %v", err)
	}
	if !created {
		t.Fatal("ConsignmentPost created = false, want true")
	}
	if gotMethod != "consignment.post" {
		t.Errorf("method = %q", gotMethod)
	}
	if gotRecipient != "bcrt:bcrt1qtest" || gotTxID != testTxID || gotVout != 1 {
		t.Errorf("params = (%q, %q, %d)", gotRecipient, gotTxID, gotVout)
	}
	if string(gotFile) != "consignment-bytes" {
		t.Errorf("file = %q", gotFile)
	}
}

func TestAckGet(t *testing.T) {
	cases := []struct {
		name    string
		result  string
		want    *bool
		wantErr bool
	}{
		{"acked", `true`, boolPtr(true), false},
		{"rejected", `false`, boolPtr(false), false},
		{"pending", `null`, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":` + tc.result + `}`))
			}))
			defer ts.Close()
			client, err := NewTransportClient(ts.URL, nil)
			if err != nil {
				t.Fatalf("NewTransportClient: %v", err)
			}
			ack, err := client.AckGet(context.Background(), "bcrt:bcrt1qtest")
			if (err != nil) != tc.wantErr {
				t.Fatalf("AckGet err = %v, wantErr %v", err, tc.wantErr)
			}
			switch {
			case tc.want == nil && ack != nil:
				t.Fatalf("AckGet = %v, want nil", *ack)
			case tc.want != nil && (ack == nil || *ack != *tc.want):
				t.Fatalf("AckGet = %v, want %v", ack, *tc.want)
			}
		})
	}

	t.Run("not found is pending", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-400,"message":"Consignment file not found"}}`))
		}))
		defer ts.Close()
		client, err := NewTransportClient(ts.URL, nil)
		if err != nil {
			t.Fatalf("NewTransportClient: %v", err)
		}
		ack, err := client.AckGet(context.Background(), "bcrt:bcrt1qtest")
		if err != nil || ack != nil {
			t.Fatalf("AckGet = (%v, %v), want (nil, nil)", ack, err)
		}
	})
}

func TestRecordWithdrawConsignmentPostsToGateway(t *testing.T) {
	var gotRecipient, gotTxID string
	var gotFile []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		var params struct {
			RecipientID string `json:"recipient_id"`
			TxID        string `json:"txid"`
		}
		_ = json.Unmarshal([]byte(r.FormValue("params")), &params)
		gotRecipient, gotTxID = params.RecipientID, params.TxID
		f, _, err := r.FormFile("file")
		if err == nil {
			gotFile, _ = io.ReadAll(f)
			_ = f.Close()
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":true}`))
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	recordWithdrawConsignmentWithRetry(ctx, ts.URL, "bcrt:bcrt1qtest", testTxID, nil,
		[]byte("withdraw-bytes"), time.Millisecond, time.Millisecond)

	if gotRecipient != "bcrt:bcrt1qtest" || gotTxID != testTxID {
		t.Fatalf("params = (%q, %q)", gotRecipient, gotTxID)
	}
	if string(gotFile) != "withdraw-bytes" {
		t.Fatalf("file = %q", gotFile)
	}
}

func TestPostConsignmentToAnyEndpointFallsThrough(t *testing.T) {
	var hits []string
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, "bad")
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, "good")
		if !strings.Contains(r.Header.Get("Content-Type"), "multipart/form-data") {
			t.Errorf("content type = %q", r.Header.Get("Content-Type"))
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":true}`))
	}))
	defer good.Close()

	endpoint, err := postConsignmentToAnyEndpoint(context.Background(), []string{bad.URL, good.URL},
		"bcrt:bcrt1qtest", testTxID, nil, []byte("bytes"))
	if err != nil {
		t.Fatalf("postConsignmentToAnyEndpoint: %v", err)
	}
	if endpoint != good.URL {
		t.Fatalf("endpoint = %q, want %q", endpoint, good.URL)
	}
	if len(hits) != 2 || hits[0] != "bad" || hits[1] != "good" {
		t.Fatalf("hits = %v", hits)
	}
}

func boolPtr(v bool) *bool { return &v }
