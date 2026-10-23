package rgb20

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/btcsuite/btcwallet/walletdb"
)

// withdrawConsignmentBucket 保存 chain33 提现 burn 哈希 -> 原始 consignment 字节。
//
// 值必须是**桥实际交付的那份字节**：确认交易里登记的哈希（sha256）由这份字节算出，
// 前端/审计据此把链上哈希与外部服务收到的 consignment 对上。落盘先于上传，也先于广播。
var withdrawConsignmentBucket = []byte("rgb20-withdraw-consignment")

const (
	// defaultConsignmentPublishTimeout 单次上传的默认超时。
	defaultConsignmentPublishTimeout = 10 * time.Second
	// consignmentPublishRetryBase 上传失败后的首次退避间隔（之后指数增长）。
	consignmentPublishRetryBase = 5 * time.Second
	// maxConsignmentPublishRetryInterval 退避间隔上限：再失败也不超过它，保证外部服务
	// 恢复后能在有限时间内补上，同时不刷屏。
	maxConsignmentPublishRetryInterval = 5 * time.Minute
)

// ConsignmentPublishConfig RGB20 提现 consignment 的外部交付配置。
//
// URL 为空 = 只落盘、不上传（日志 WARN）。上传是**尽力而为**的旁路：失败/超时只影响
// 交付，绝不影响提现广播或链上确认。
type ConsignmentPublishConfig struct {
	// URL 接收原始 consignment 字节的 HTTP 端点（POST）。
	URL string `json:"url"`
	// Token 非空时以 `Authorization: Bearer <token>` 发送。
	Token string `json:"token"`
	// TimeoutSeconds 单次 HTTP 请求超时；<=0 用默认值（10s）。
	TimeoutSeconds int `json:"timeoutSeconds"`
}

func (c ConsignmentPublishConfig) timeout() time.Duration {
	if c.TimeoutSeconds <= 0 {
		return defaultConsignmentPublishTimeout
	}
	return time.Duration(c.TimeoutSeconds) * time.Second
}

// SaveWithdrawConsignment 按 chain33 burn 哈希保存原始 consignment 字节（PutOnce 幂等）。
//
//   - 已存在且逐字节相同：幂等成功（提现重试会用同一份 consignment 重放）；
//   - 已存在但字节不同：拒绝覆盖并返回错误。哈希必须来自**实际保存的字节**，覆盖写会让
//     链上登记的哈希与外部服务收到的那份对不上，而且同一笔 burn 的两份 consignment 本身
//     就是异常（说明构建结果漂移）；
//   - 存储失败：原样返回错误。调用方只记 ERROR，不让提现失败。
func (a *Adapter) SaveWithdrawConsignment(chain33Hash, consignment []byte) error {
	if len(chain33Hash) == 0 {
		return fmt.Errorf("empty chain33 withdraw hash")
	}
	if len(consignment) == 0 {
		return fmt.Errorf("empty consignment")
	}
	existing, err := a.store.Get(withdrawConsignmentBucket, chain33Hash)
	switch {
	case err == nil:
		if bytes.Equal(existing, consignment) {
			return nil
		}
		return fmt.Errorf("refuse to overwrite consignment for withdraw %s: saved %d bytes, got %d bytes",
			hex.EncodeToString(chain33Hash), len(existing), len(consignment))
	case errors.Is(err, walletdb.ErrBucketNotFound):
		// 首次保存（bucket 或键不存在都走这里）。
	default:
		return err
	}
	return a.store.Put(withdrawConsignmentBucket, chain33Hash, consignment)
}

// GetWithdrawConsignment 读取该笔提现保存的原始 consignment 字节。
func (a *Adapter) GetWithdrawConsignment(chain33Hash []byte) ([]byte, error) {
	return a.store.Get(withdrawConsignmentBucket, chain33Hash)
}

// WithdrawConsignmentHash 返回该笔提现**实际保存的** consignment 字节的 sha256。
// 本地没有记录（或记录为空）时返回 nil —— 调用方据此提交不带哈希的确认，而不是猜一个值。
func (a *Adapter) WithdrawConsignmentHash(chain33Hash []byte) []byte {
	data, err := a.GetWithdrawConsignment(chain33Hash)
	if err != nil || len(data) == 0 {
		return nil
	}
	sum := sha256.Sum256(data)
	return sum[:]
}

// IsRgb20WithdrawTxid 判断某个 BTC txid 是否为本桥发出的 RGB20 提现交易。
//
// 与 IsKnownRgbTxid 的区别：后者也包含充值收款交易（值是 receiveID），本方法只认提现
// （putKnownWithdrawTxid 写入的标记）。确认交易只有对提现才带 consignment 哈希。
func (a *Adapter) IsRgb20WithdrawTxid(txid string) bool {
	if a == nil || txid == "" {
		return false
	}
	val, err := a.store.Get(txidBucket, []byte(txid))
	return err == nil && string(val) == knownRgbWithdrawTxid
}

// publishConsignmentAsync 启动后台交付：先落盘之后调用，绝不阻塞提现广播/确认。
//
// 返回 false 表示没有启动上传（未配置 url / 空 consignment / 适配器已停止），调用方与测试
// 据此区分"只告警"路径。上传 goroutine 不在 Adapter.wg 里登记（避免与 Stop 的 Wait 竞争
// WaitGroup 计数）；它只靠 a.ctx 终止，且进程退出时本就无需等待一次旁路交付。
func (a *Adapter) publishConsignmentAsync(withdrawHash, consignment []byte) bool {
	cfg := a.cfg.ConsignmentPublish
	if cfg.URL == "" {
		log.Warn("rgb20 consignment publish is not configured; consignment kept locally only",
			"withdrawHash", hex.EncodeToString(withdrawHash))
		return false
	}
	if len(consignment) == 0 {
		log.Warn("rgb20 consignment publish skipped: empty consignment",
			"withdrawHash", hex.EncodeToString(withdrawHash))
		return false
	}
	if a.ctx.Err() != nil {
		log.Warn("rgb20 consignment publish not started: adapter stopped",
			"withdrawHash", hex.EncodeToString(withdrawHash))
		return false
	}
	go publishConsignmentWithRetry(a.ctx, a.publishClient, cfg, withdrawHash, consignment,
		consignmentPublishRetryBase, maxConsignmentPublishRetryInterval)
	return true
}

// publishConsignmentWithRetry 以指数退避重试上传，直到成功或 ctx 结束。
// base/max 作为参数传入，单测可用毫秒级间隔验证重试次数与终止条件。
func publishConsignmentWithRetry(ctx context.Context, client *http.Client, cfg ConsignmentPublishConfig,
	withdrawHash, consignment []byte, base, max time.Duration) {

	if base <= 0 {
		base = consignmentPublishRetryBase
	}
	if max < base {
		max = base
	}
	for attempt := 1; ; attempt++ {
		err := publishConsignment(ctx, client, cfg, withdrawHash, consignment)
		if err == nil {
			log.Info("rgb20 consignment published",
				"withdrawHash", hex.EncodeToString(withdrawHash), "bytes", len(consignment))
			return
		}
		if ctx.Err() != nil {
			log.Warn("rgb20 consignment publish stopped",
				"withdrawHash", hex.EncodeToString(withdrawHash), "attempts", attempt-1, "err", err)
			return
		}
		interval := consignmentRetryInterval(base, max, attempt)
		log.Warn("rgb20 consignment publish failed; retrying",
			"withdrawHash", hex.EncodeToString(withdrawHash), "attempt", attempt,
			"retryIn", interval, "err", err)
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			log.Warn("rgb20 consignment publish stopped",
				"withdrawHash", hex.EncodeToString(withdrawHash), "attempts", attempt, "err", err)
			return
		case <-timer.C:
		}
	}
}

// consignmentRetryInterval 第 attempt 次失败后的退避间隔：base * 2^(attempt-1)，封顶 max。
func consignmentRetryInterval(base, max time.Duration, attempt int) time.Duration {
	interval := base
	for i := 1; i < attempt && interval < max; i++ {
		interval *= 2
	}
	if interval > max {
		interval = max
	}
	return interval
}

// publishConsignment 执行一次上传。2xx 视为成功。
func publishConsignment(ctx context.Context, client *http.Client, cfg ConsignmentPublishConfig,
	withdrawHash, consignment []byte) error {

	if cfg.URL == "" {
		return fmt.Errorf("consignment publish url not configured")
	}
	reqCtx, cancel := context.WithTimeout(ctx, cfg.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, cfg.URL, bytes.NewReader(consignment))
	if err != nil {
		return fmt.Errorf("build publish request: %w", err)
	}
	sum := sha256.Sum256(consignment)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Consignment-Hash", hex.EncodeToString(sum[:]))
	req.Header.Set("X-Withdraw-Hash", hex.EncodeToString(withdrawHash))
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	if client == nil {
		client = &http.Client{Timeout: cfg.timeout()}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("publish consignment: %w", err)
	}
	defer resp.Body.Close()
	// 读掉少量响应体（连接复用），响应内容不参与判定。
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("publish consignment: unexpected status %s", resp.Status)
	}
	return nil
}
