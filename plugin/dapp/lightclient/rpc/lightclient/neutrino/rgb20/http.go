package rgb20

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/33cn/chain33/common/log/log15"
)

var log = log15.New("module", "rgb20")

// httpRequest 充值请求 HTTP 体。
type httpRequest struct {
	RequestID   string `json:"requestId"`
	AssetSymbol string `json:"assetSymbol"`
	Amount      int64  `json:"amount"`
	Chain33Addr string `json:"chain33Addr"`
}

type httpResponse struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// serveHTTP 启动 consignment 上传与充值请求 HTTP 服务。
func (a *Adapter) serveHTTP(listen string) {
	defer a.wg.Done()
	mux := http.NewServeMux()
	mux.HandleFunc("/rgbx/v1/consignment", a.handleConsignmentUpload)
	mux.HandleFunc("/rgbx/v1/deposit", a.handleDepositRequest)
	// 测试专用：用 TSS 对 PSBT 签名（Phase 5 E2E 的用户付款 PSBT 由链上 TSS 组签）。
	mux.HandleFunc("/rgbx/v1/sign-psbt", a.handleSignPsbt)
	srv := &http.Server{
		Addr:    listen,
		Handler: mux,
	}
	go func() {
		<-a.ctx.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("rgb20 http server", "listen", listen, "err", err)
	}
}

// handleConsignmentUpload 上传 consignment：按 receive_id 交付并触发侧车结算。
// POST /rgbx/v1/consignment  body: {"receiveId":"...","consignment":"<base64>"}
func (a *Adapter) handleConsignmentUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeHTTP(w, httpResponse{Code: http.StatusMethodNotAllowed, Message: "method not allowed"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeHTTP(w, httpResponse{Code: http.StatusBadRequest, Message: "read body: " + err.Error()})
		return
	}
	var req struct {
		ReceiveID   string `json:"receiveId"`
		Consignment string `json:"consignment"` // base64
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeHTTP(w, httpResponse{Code: http.StatusBadRequest, Message: "bad json: " + err.Error()})
		return
	}
	if req.ReceiveID == "" || req.Consignment == "" {
		writeHTTP(w, httpResponse{Code: http.StatusBadRequest, Message: "receiveId and consignment required"})
		return
	}
	consignment := decodeBase64(req.Consignment)
	if len(consignment) == 0 {
		writeHTTP(w, httpResponse{Code: http.StatusBadRequest, Message: "invalid consignment base64"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	st, err := a.ProvideConsignment(ctx, consignment, req.ReceiveID)
	if err != nil {
		writeHTTP(w, httpResponse{Code: http.StatusInternalServerError, Message: "provide consignment: " + err.Error()})
		return
	}
	writeHTTP(w, httpResponse{Code: http.StatusOK, Message: "ok", Data: st})
}

// handleDepositRequest 发起充值请求。
// POST /rgbx/v1/deposit body: {"requestId":"...","assetSymbol":"RGB20_USDT","amount":100,"chain33Addr":"..."}
func (a *Adapter) handleDepositRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeHTTP(w, httpResponse{Code: http.StatusMethodNotAllowed, Message: "method not allowed"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeHTTP(w, httpResponse{Code: http.StatusBadRequest, Message: "read body: " + err.Error()})
		return
	}
	req := &httpRequest{}
	if err := json.Unmarshal(body, req); err != nil {
		writeHTTP(w, httpResponse{Code: http.StatusBadRequest, Message: "bad json: " + err.Error()})
		return
	}
	if req.RequestID == "" {
		req.RequestID = fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	rec, err := a.DepositFlow(ctx, &DepositRequest{
		RequestID:   req.RequestID,
		AssetSymbol: strings.ToUpper(req.AssetSymbol),
		Amount:      req.Amount,
		Chain33Addr: req.Chain33Addr,
	})
	if err != nil {
		writeHTTP(w, httpResponse{Code: http.StatusInternalServerError, Message: "create receive: " + err.Error()})
		return
	}
	writeHTTP(w, httpResponse{Code: http.StatusOK, Message: "ok", Data: rec})
}

// handleSignPsbt 测试专用：用 TSS 组对 PSBT 签名（E2E 驱动用，非生产 RPC）。
// POST /rgbx/v1/sign-psbt  body: {"psbt":"<hex>"}  →  {"psbt":"<hex signed>"}
func (a *Adapter) handleSignPsbt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeHTTP(w, httpResponse{Code: http.StatusMethodNotAllowed, Message: "method not allowed"})
		return
	}
	if a.bridge == nil {
		writeHTTP(w, httpResponse{Code: http.StatusInternalServerError, Message: "bridge not set"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeHTTP(w, httpResponse{Code: http.StatusBadRequest, Message: "read body: " + err.Error()})
		return
	}
	var req struct {
		Psbt string `json:"psbt"` // hex
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Psbt == "" {
		writeHTTP(w, httpResponse{Code: http.StatusBadRequest, Message: "bad request: psbt (hex) required"})
		return
	}
	psbtBytes, err := decodeHex(req.Psbt)
	if err != nil {
		writeHTTP(w, httpResponse{Code: http.StatusBadRequest, Message: "invalid psbt hex: " + err.Error()})
		return
	}
	signed, err := a.bridge.SignPsbt(psbtBytes)
	if err != nil {
		writeHTTP(w, httpResponse{Code: http.StatusInternalServerError, Message: "sign psbt: " + err.Error()})
		return
	}
	writeHTTP(w, httpResponse{Code: http.StatusOK, Message: "ok", Data: map[string]string{"psbt": hex.EncodeToString(signed)}})
}

func writeHTTP(w http.ResponseWriter, resp httpResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.Code)
	_ = json.NewEncoder(w).Encode(resp)
}
