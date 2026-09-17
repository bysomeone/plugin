package neutrino

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
)

/*
 * 充值地址发放 HTTP（C2）。
 *
 * 职责只有一个：**按需把用户的 P2WSH 充值脚本纳入 watch 集并返回地址**。地址本身是纯函数
 * （谁都能离线算，见 deposit_address.go），但"钱包得先 watch 它才能看见充值"这件事必须发生在
 * 桥这一侧，所以用户必须先来要一次地址 —— 这就是本入口存在的原因，不是"托管地址簿"。
 *
 * 与 rgb20 的 HTTP（adapter 的 serveHTTP）同风格：明文 HTTP，POST/GET 收 JSON 结构、
 * 返回 {code,message,data}。它的定位是**桥的内部/运维接口**，部署时应只对可信网段开放
 * （配置键 neutrino.depositAddressListen 为空则完全不开）。
 */

// depositAddressHTTPRequest 充值地址请求：chain33Addr = 用户在本链上的地址（= P2WSH 派生里的 userID）。
type depositAddressHTTPRequest struct {
	Chain33Addr string `json:"chain33Addr"`
}

type depositAddressHTTPResponse struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// depositAddressInfo 发放结果：地址 + program（执行器侧重建时比的就是它，便于对账/排障）。
type depositAddressInfo struct {
	UserID    string `json:"userID"`    // = chain33 地址串（原样字节即脚本里的 userID）
	Address   string `json:"address"`   // bech32 P2WSH 充值地址（本网络）
	PkScript  string `json:"pkScript"`  // 34 字节 program 的 hex（OP_0 <sha256(witnessScript)>）
	Spec      string `json:"spec"`      // 派生规格版本标签（链下约定，脚本里不写版本字节）
	WatchSize int    `json:"watchSize"` // 当前 watch 集大小（运维观察"随用户数增长"的代价）
}

// serveDepositAddressHTTP 启动充值地址发放 HTTP 服务（阻塞；由 Start 以 goroutine 拉起）。
func (n *neutrinoClient) serveDepositAddressHTTP(listen string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rgbx/v1/btc-deposit-address", n.handleDepositAddressRequest)
	srv := &http.Server{Addr: listen, Handler: mux}
	go func() {
		<-n.ctx.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	log.Info("serveDepositAddressHTTP start", "listen", listen)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("serveDepositAddressHTTP", "listen", listen, "err", err)
	}
}

// handleDepositAddressRequest 发放（并 watch）某用户的 BTC 充值地址。
//
//	GET  /rgbx/v1/btc-deposit-address?chain33Addr=<chain33 地址>
//	POST /rgbx/v1/btc-deposit-address  {"chain33Addr":"<chain33 地址>"}
//
// 幂等：同一地址重复请求返回同一个结果，watch 集只增长一次。
func (n *neutrinoClient) handleDepositAddressRequest(w http.ResponseWriter, r *http.Request) {
	var chain33Addr string
	switch r.Method {
	case http.MethodGet:
		chain33Addr = strings.TrimSpace(r.URL.Query().Get("chain33Addr"))
	case http.MethodPost:
		req := &depositAddressHTTPRequest{}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(req); err != nil {
			writeDepositHTTP(w, http.StatusBadRequest, "bad json: "+err.Error(), nil)
			return
		}
		chain33Addr = strings.TrimSpace(req.Chain33Addr)
	default:
		writeDepositHTTP(w, http.StatusMethodNotAllowed, "method not allowed", nil)
		return
	}
	if chain33Addr == "" {
		writeDepositHTTP(w, http.StatusBadRequest, "chain33Addr required", nil)
		return
	}
	if n.bw == nil {
		writeDepositHTTP(w, http.StatusServiceUnavailable, "btc wallet is not started yet", nil)
		return
	}
	addr, err := n.bw.ensureUserDepositScript(chain33Addr)
	if err != nil {
		// 这里不区分"参数错"与"内部错"（后者含 watch 集已满）：都返回 400/503 的明确错误文案，
		// 让调用方知道**没有**拿到地址（而不是拿到一个没被 watch 的地址）。
		log.Error("handleDepositAddressRequest ensure deposit script", "chain33Addr", chain33Addr, "err", err)
		writeDepositHTTP(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	pkScript, err := rtypes.DeriveDepositPkScript(chain33Addr, n.bw.depositTssPubKey())
	if err != nil {
		writeDepositHTTP(w, http.StatusInternalServerError, "derive pkScript: "+err.Error(), nil)
		return
	}
	writeDepositHTTP(w, http.StatusOK, "ok", depositAddressInfo{
		UserID:    chain33Addr,
		Address:   addr,
		PkScript:  hex.EncodeToString(pkScript),
		Spec:      rtypes.P2WSHDepositSpecV1,
		WatchSize: n.bw.depositScripts.size(),
	})
}

func writeDepositHTTP(w http.ResponseWriter, code int, message string, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(depositAddressHTTPResponse{Code: code, Message: message, Data: data})
}
