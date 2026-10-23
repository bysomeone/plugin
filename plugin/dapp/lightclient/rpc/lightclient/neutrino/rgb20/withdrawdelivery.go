package rgb20

import (
	"context"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	pb "github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20/pb"
)

// 提现 consignment 的标准 transport 交付（桥是付款方）。
//
// 与充值相反：充值侧桥是收款方，从网关 `consignment.get` 拉取；提现侧桥是付款方，按用户
// invoice 携带的 `transports` 投递 `consignment.post`，再轮询 `ack.get` 拿到收款方结论。
// 投递是**尽力而为**的旁路：它不影响 BTC 交易的签名、广播与链上结算——钱已经在链上锁定，
// 交付失败只意味着这份转移凭证还要重试（侧车本地与外部网关都保留了字节）。
//
// invoice 没有可用 transport（例如用户钱包只能给出文件 / 二维码）时回退到配置的
// `consignmentPublish.url`（见 consignment.go），没有配置则只落盘并告警。

const (
	// defaultWithdrawAckWait 等待收款方 ACK 的窗口。
	defaultWithdrawAckWait = 2 * time.Minute
	// defaultWithdrawAckPoll 窗口内查询 ack.get 的间隔。
	defaultWithdrawAckPoll = 5 * time.Second
)

// deliverWithdrawConsignmentAsync 启动一次标准 transport 交付（不阻塞调用方）。
//
// 返回 false 表示没有启动（适配器已停止 / 空 consignment / 侧车不可用）；调用方与测试据此区分。
// goroutine 不在 a.wg 里登记，与 publishConsignmentAsync 同一口径：它只靠 a.ctx 终止。
func (a *Adapter) deliverWithdrawConsignmentAsync(withdrawHash, consignment []byte,
	invoice, txid, recipientSealOutpoint string) bool {

	sc := a.sidecar.Load()
	if sc == nil {
		log.Warn("rgb20 withdraw delivery not started: sidecar unavailable",
			"withdrawHash", hex.EncodeToString(withdrawHash))
		return false
	}
	if len(consignment) == 0 {
		log.Warn("rgb20 withdraw delivery skipped: empty consignment",
			"withdrawHash", hex.EncodeToString(withdrawHash))
		return false
	}
	if a.ctx.Err() != nil {
		log.Warn("rgb20 withdraw delivery not started: adapter stopped",
			"withdrawHash", hex.EncodeToString(withdrawHash))
		return false
	}

	go func() {
		parsed, err := sc.ParseInvoice(a.ctx, &pb.ParseInvoiceRequest{Invoice: invoice})
		if err != nil {
			// The invoice was parsed by the sidecar during BuildWithdrawal, so this is a sidecar
			// outage rather than a bad invoice. Fall back to the configured upload endpoint: the
			// consignment still has to leave the bridge somehow.
			log.Error("rgb20 withdraw delivery: parse invoice failed, falling back to consignmentPublish",
				"withdrawHash", hex.EncodeToString(withdrawHash), "err", err)
			a.publishConsignmentAsync(withdrawHash, consignment)
			return
		}
		endpoints := withdrawTransportEndpoints(parsed.GetTransports())
		recipientID := parsed.GetRecipientId()
		vout := sealOutpointVout(recipientSealOutpoint)
		if len(endpoints) > 0 && recipientID != "" {
			deliverWithdrawWithRetry(a.ctx, endpoints, recipientID, txid, vout,
				consignment, consignmentPublishRetryBase, maxConsignmentPublishRetryInterval,
				defaultWithdrawAckWait, defaultWithdrawAckPoll)
			return
		}
		// No usable user transport: standard wallets cannot import a raw consignment, so there is
		// no user-facing delivery to make. Record it on our gateway instead (content-addressed,
		// retrievable by recipient_id) so it is not only in this node's local store.
		if gatewayEndpoint, ok := NormalizeTransportEndpoint(a.cfg.TransportGateway); ok && recipientID != "" {
			log.Info("rgb20 withdraw delivery: invoice carries no usable transport; recording on the gateway",
				"withdrawHash", hex.EncodeToString(withdrawHash), "recipientId", recipientID)
			recordWithdrawConsignmentWithRetry(a.ctx, gatewayEndpoint, recipientID, txid, vout,
				consignment, consignmentPublishRetryBase, maxConsignmentPublishRetryInterval)
			return
		}
		log.Info("rgb20 withdraw delivery: no transport and no gateway to record on, falling back to consignmentPublish",
			"withdrawHash", hex.EncodeToString(withdrawHash),
			"transports", len(parsed.GetTransports()), "recipientId", recipientID)
		a.publishConsignmentAsync(withdrawHash, consignment)
	}()
	return true
}

// withdrawTransportEndpoints keeps the endpoints this client can speak to and normalises their
// schemes. Unsupported transports (rest, websockets, …) are skipped, not fatal: the invoice may
// list several and only one needs to work.
func withdrawTransportEndpoints(transports []string) []string {
	endpoints := make([]string, 0, len(transports))
	seen := make(map[string]struct{}, len(transports))
	for _, transport := range transports {
		endpoint, ok := NormalizeTransportEndpoint(transport)
		if !ok {
			log.Warn("rgb20 withdraw delivery: skipping unsupported invoice transport", "transport", transport)
			continue
		}
		if _, dup := seen[endpoint]; dup {
			continue
		}
		seen[endpoint] = struct{}{}
		endpoints = append(endpoints, endpoint)
	}
	return endpoints
}

// sealOutpointVout parses the recipient vout from a `txid:vout` seal descriptor.
func sealOutpointVout(outpoint string) *uint32 {
	i := strings.LastIndex(outpoint, ":")
	if i < 0 {
		return nil
	}
	v, err := strconv.ParseUint(outpoint[i+1:], 10, 32)
	if err != nil {
		return nil
	}
	vout := uint32(v)
	return &vout
}

// deliverWithdrawWithRetry posts the consignment until one endpoint accepts it, then waits for
// the payee's ACK. base/max/ackWait/ackPoll are parameters so tests can use millisecond scales.
func deliverWithdrawWithRetry(ctx context.Context, endpoints []string, recipientID, txid string,
	vout *uint32, consignment []byte, base, max, ackWait, ackPoll time.Duration) {

	if base <= 0 {
		base = consignmentPublishRetryBase
	}
	if max < base {
		max = base
	}
	for attempt := 1; ; attempt++ {
		endpoint, err := postConsignmentToAnyEndpoint(ctx, endpoints, recipientID, txid, vout, consignment)
		if err == nil {
			log.Info("rgb20 withdraw consignment delivered",
				"endpoint", endpoint, "recipientId", recipientID, "txid", txid,
				"bytes", len(consignment), "attempts", attempt)
			awaitWithdrawAck(ctx, endpoint, recipientID, ackWait, ackPoll)
			return
		}
		if ctx.Err() != nil {
			log.Warn("rgb20 withdraw delivery stopped",
				"recipientId", recipientID, "txid", txid, "attempts", attempt-1, "err", err)
			return
		}
		if !IsRetryableTransportError(err) {
			// The endpoint answered and refused the post (e.g. -101: a different object is
			// already stored for this recipient). Retrying cannot change that verdict.
			log.Error("rgb20 withdraw consignment refused by the transport; not retrying",
				"recipientId", recipientID, "txid", txid, "attempts", attempt, "err", err)
			return
		}
		interval := consignmentRetryInterval(base, max, attempt)
		log.Warn("rgb20 withdraw consignment delivery failed; retrying",
			"recipientId", recipientID, "txid", txid, "attempt", attempt,
			"retryIn", interval, "err", err)
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			log.Warn("rgb20 withdraw delivery stopped",
				"recipientId", recipientID, "txid", txid, "attempts", attempt, "err", err)
			return
		case <-timer.C:
		}
	}
}

// recordWithdrawConsignmentWithRetry stores the consignment on our gateway under the recipient id
// (no ACK: the gateway is a record, not the payee). Retries until it is stored or the adapter
// stops; a failure never blocks the withdrawal.
func recordWithdrawConsignmentWithRetry(ctx context.Context, endpoint, recipientID, txid string,
	vout *uint32, consignment []byte, base, max time.Duration) {

	if base <= 0 {
		base = consignmentPublishRetryBase
	}
	if max < base {
		max = base
	}
	for attempt := 1; ; attempt++ {
		client, err := NewTransportClient(endpoint, nil)
		if err == nil {
			if _, err = client.ConsignmentPost(ctx, recipientID, txid, vout, consignment); err == nil {
				log.Info("rgb20 withdraw consignment recorded on the gateway",
					"endpoint", endpoint, "recipientId", recipientID, "txid", txid,
					"bytes", len(consignment), "attempts", attempt)
				return
			}
		}
		if ctx.Err() != nil {
			log.Warn("rgb20 withdraw consignment recording stopped",
				"recipientId", recipientID, "txid", txid, "attempts", attempt, "err", err)
			return
		}
		if !IsRetryableTransportError(err) {
			log.Error("rgb20 withdraw consignment recording refused; not retrying",
				"recipientId", recipientID, "txid", txid, "attempts", attempt, "err", err)
			return
		}
		interval := consignmentRetryInterval(base, max, attempt)
		log.Warn("rgb20 withdraw consignment recording failed; retrying",
			"recipientId", recipientID, "txid", txid, "attempt", attempt,
			"retryIn", interval, "err", err)
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// postConsignmentToAnyEndpoint tries the invoice transports in order and returns the first one
// that accepted the post. An endpoint that is down does not stop the others from being tried.
func postConsignmentToAnyEndpoint(ctx context.Context, endpoints []string, recipientID, txid string,
	vout *uint32, consignment []byte) (string, error) {

	var lastErr error
	for _, endpoint := range endpoints {
		client, err := NewTransportClient(endpoint, nil)
		if err != nil {
			lastErr = err
			continue
		}
		if _, err := client.ConsignmentPost(ctx, recipientID, txid, vout, consignment); err != nil {
			lastErr = err
			continue
		}
		return endpoint, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no usable transport endpoint")
	}
	return "", lastErr
}

// awaitWithdrawAck polls ack.get until the payee records a verdict or the window closes.
// A missing ACK is not a failure of the delivery: the consignment is stored at the endpoint and
// the payee may validate it later. It is logged so operators can see a payee that never reacts.
func awaitWithdrawAck(ctx context.Context, endpoint, recipientID string, wait, poll time.Duration) {
	client, err := NewTransportClient(endpoint, nil)
	if err != nil {
		log.Warn("rgb20 withdraw ack poll skipped: bad endpoint", "endpoint", endpoint, "err", err)
		return
	}
	if wait <= 0 {
		wait = defaultWithdrawAckWait
	}
	if poll <= 0 {
		poll = defaultWithdrawAckPoll
	}
	deadline := time.Now().Add(wait)
	for {
		ack, err := client.AckGet(ctx, recipientID)
		switch {
		case err == nil && ack != nil && *ack:
			log.Info("rgb20 withdraw consignment ACKed by the payee", "endpoint", endpoint, "recipientId", recipientID)
			return
		case err == nil && ack != nil:
			log.Error("rgb20 withdraw consignment rejected by the payee",
				"endpoint", endpoint, "recipientId", recipientID)
			return
		case err != nil && !IsRetryableTransportError(err):
			log.Warn("rgb20 withdraw ack poll stopped on a protocol error",
				"endpoint", endpoint, "recipientId", recipientID, "err", err)
			return
		}
		if time.Now().After(deadline) {
			log.Warn("rgb20 withdraw consignment delivered but not ACKed within the wait window",
				"endpoint", endpoint, "recipientId", recipientID, "waited", wait)
			return
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
