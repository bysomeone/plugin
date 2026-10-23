package rgb20

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/33cn/plugin/plugin/dapp/rgbx/depositrequest"
)

// 充值请求的网关订阅（桥是 SSE 客户端，不监听任何端口）。
//
// 用户在网关提交带签名的充值请求 → 网关通过 SSE 推给桥 → 桥生成 invoice 并回写网关 →
// 用户的应用从网关取票。桥在出票前验签（权威校验仍在铸造时的链上执行）：伪造请求不能
// 让桥为错误的收款地址出票，否则用户付款后铸造会被链上拒绝。
//
// 投递语义是 at-least-once：断线重连时网关重放所有还没有 invoice 的请求，桥按 request_id
// 幂等（receiveRequestIndexBucket）。

const (
	depositStreamRetryBase = time.Second
	depositStreamRetryMax  = 30 * time.Second
)

// depositRequestEvent is the JSON payload the gateway sends in one SSE event.
type depositRequestEvent struct {
	RequestID   string `json:"request_id"`
	Chain33Addr string `json:"chain33_addr"`
	AssetSymbol string `json:"asset_symbol"`
	Amount      int64  `json:"amount"`
	Timestamp   int64  `json:"timestamp"`
	PublicKey   string `json:"pubkey"`
	Signature   string `json:"signature"`
}

// gatewayBaseURL returns scheme://host from the configured transport gateway URL. The /bridge/*
// endpoints hang off the same origin; only the path is dropped.
func gatewayBaseURL(transportGateway string) (string, error) {
	u, err := url.Parse(transportGateway)
	if err != nil {
		return "", fmt.Errorf("transportGateway %q: %w", transportGateway, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("transportGateway %q: not an http(s) URL", transportGateway)
	}
	return u.Scheme + "://" + u.Host, nil
}

// runDepositRequestStream keeps the subscription alive across gateway restarts.
func (a *Adapter) runDepositRequestStream() {
	defer a.wg.Done()
	base, err := gatewayBaseURL(a.cfg.TransportGateway)
	if err != nil {
		log.Error("rgb20 deposit request stream disabled", "err", err)
		return
	}
	streamURL := base + "/bridge/deposit/requests/stream"
	invoiceURL := base + "/bridge/deposit/invoices"
	// No Client.Timeout: the stream is long lived and ends with the context or the connection.
	client := &http.Client{}
	log.Info("rgb20 deposit request stream started", "url", streamURL)

	delay := depositStreamRetryBase
	for {
		err := a.consumeDepositStream(a.ctx, client, streamURL, invoiceURL)
		if a.ctx.Err() != nil {
			return
		}
		log.Warn("rgb20 deposit request stream ended; reconnecting", "err", err, "retryIn", delay)
		select {
		case <-a.ctx.Done():
			return
		case <-time.After(delay):
		}
		delay *= 2
		if delay > depositStreamRetryMax {
			delay = depositStreamRetryMax
		}
	}
}

// consumeDepositStream reads the SSE stream until it fails. A returned error makes the caller
// reconnect; the gateway replays every request that has no invoice yet.
func (a *Adapter) consumeDepositStream(ctx context.Context, client *http.Client,
	streamURL, invoiceURL string) error {

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.GatewayToken)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("stream status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue // event name, comment (ping) or blank separator
		}
		event := &depositRequestEvent{}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), event); err != nil {
			log.Warn("rgb20 deposit request stream: undecodable event", "err", err)
			continue
		}
		if err := a.handleDepositRequestEvent(ctx, client, invoiceURL, event); err != nil {
			// Closing and reconnecting replays the request; nothing is lost.
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return io.EOF
}

// handleDepositRequestEvent is idempotent per request id.
func (a *Adapter) handleDepositRequestEvent(ctx context.Context, client *http.Client,
	invoiceURL string, event *depositRequestEvent) error {

	if event == nil || event.RequestID == "" {
		return nil
	}
	if rec, err := a.receives.GetByRequestID(event.RequestID); err == nil {
		// Already handled (replay after a reconnect): republish the invoice in case the gateway
		// did not persist our previous response.
		return a.publishDepositInvoice(ctx, client, invoiceURL, rec)
	}

	pubKey, err := hex.DecodeString(strings.TrimPrefix(event.PublicKey, "0x"))
	if err != nil {
		log.Error("rgb20 deposit request rejected: pubkey is not hex", "requestId", event.RequestID, "err", err)
		return nil
	}
	signature, err := hex.DecodeString(strings.TrimPrefix(event.Signature, "0x"))
	if err != nil {
		log.Error("rgb20 deposit request rejected: signature is not hex", "requestId", event.RequestID, "err", err)
		return nil
	}
	req := depositrequest.Request{
		RequestID:   event.RequestID,
		Chain33Addr: event.Chain33Addr,
		AssetSymbol: event.AssetSymbol,
		Amount:      event.Amount,
		Timestamp:   event.Timestamp,
	}
	if err := depositrequest.Verify(a.cfg.ChainTitle, req, pubKey, signature); err != nil {
		// A bad signature is a permanent answer, not a transient failure: do not reconnect-loop
		// over it. The user's request stays pending at the gateway and is never invoiced.
		log.Error("rgb20 deposit request rejected: signature verification failed",
			"requestId", event.RequestID, "addr", event.Chain33Addr, "err", err)
		return nil
	}

	rec, err := a.CreateReceive(ctx, &DepositRequest{
		RequestID:           req.RequestID,
		AssetSymbol:         req.AssetSymbol,
		Amount:              req.Amount,
		Chain33Addr:         req.Chain33Addr,
		RequestPubKeyHex:    event.PublicKey,
		RequestSignatureHex: event.Signature,
		RequestTimestamp:    req.Timestamp,
	})
	if err != nil {
		return fmt.Errorf("create receive for request %s: %w", req.RequestID, err)
	}
	log.Info("rgb20 deposit request invoiced", "requestId", req.RequestID,
		"receiveId", rec.ReceiveID, "addr", req.Chain33Addr, "amount", req.Amount)
	return a.publishDepositInvoice(ctx, client, invoiceURL, rec)
}

// publishDepositInvoice posts the sidecar's invoice back to the gateway.
func (a *Adapter) publishDepositInvoice(ctx context.Context, client *http.Client,
	invoiceURL string, rec *ReceiveRecord) error {

	body, err := json.Marshal(map[string]string{
		"request_id":   rec.RequestID,
		"invoice":      rec.Invoice,
		"receive_id":   rec.ReceiveID,
		"recipient_id": rec.RecipientID,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, invoiceURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.cfg.GatewayToken)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("publish invoice %s: status %d: %s", rec.RequestID, resp.StatusCode,
			strings.TrimSpace(string(respBody)))
	}
	return nil
}
