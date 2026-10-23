package rgb20

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/33cn/chain33/common/log/log15"
)

var log = log15.New("module", "rgb20")

type httpResponse struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// serveTestHTTP 是 **仅 E2E** 使用的本地测试监听：只提供 sign-psbt（用 TSS 组签任意 PSBT，
// 模拟用户付款）。生产不配置 testListen，桥不提供任何 HTTP API。
//
// 地址必须是 loopback 或 unix socket：该端点等价于"用 TSS 组私钥签任意内容"，绝不能落在
// 外部可达的网卡上。
func (a *Adapter) serveTestHTTP(listen string) {
	defer a.wg.Done()
	if !testListenIsLocal(listen) {
		log.Error("rgb20 test listener refused: address must be loopback or a unix socket", "listen", listen)
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/rgbx/v1/sign-psbt", a.handleSignPsbt)
	srv := &http.Server{Handler: mux}

	var ln net.Listener
	var err error
	if strings.HasPrefix(listen, "unix://") || strings.HasPrefix(listen, "/") {
		path := strings.TrimPrefix(listen, "unix://")
		if strings.HasPrefix(path, "/") {
			// A stale socket file is expected after a crash; removing it is safe because we are
			// about to bind the same path.
			_ = os.Remove(path)
		}
		ln, err = net.Listen("unix", path)
	} else {
		ln, err = net.Listen("tcp", listen)
	}
	if err != nil {
		log.Error("rgb20 test listener", "listen", listen, "err", err)
		return
	}
	go func() {
		<-a.ctx.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	log.Info("rgb20 test listener started (sign-psbt only)", "listen", listen)
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Error("rgb20 test listener", "listen", listen, "err", err)
	}
}

// testListenIsLocal accepts loopback TCP addresses and unix sockets only.
func testListenIsLocal(listen string) bool {
	if strings.HasPrefix(listen, "unix://") || strings.HasPrefix(listen, "/") {
		return true
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

// handleSignPsbt 仅 E2E 使用：用 TSS 组对 PSBT 签名（无提现上下文，签名节点无从核对）。
// 必须由 [rpc.sub.light.neutrino.rgb20].testSignPsbt 显式打开（默认关闭），否则 403。
func (a *Adapter) handleSignPsbt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeHTTP(w, httpResponse{Code: http.StatusMethodNotAllowed, Message: "method not allowed"})
		return
	}
	if !a.TestSignEnabled() {
		writeHTTP(w, httpResponse{Code: http.StatusForbidden,
			Message: "test sign-psbt disabled: set rgb20.testSignPsbt to enable it in test environments"})
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
	signed, err := a.bridge.SignPsbtTestOnly(psbtBytes)
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
