package neutrino

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/33cn/chain33/types"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/walletdb"
)

/*
 * BTC 提现的"用户请求加价 → 桥重签替换交易"闭环（桥侧协调者）。
 *
 * 链上的 BumpWithdrawFee 只记录用户请求并发出事件；真正重签发生在链下。本文件按高度区间
 * 轮询 ListBumpWithdrawFeeRequests（与 deposit_registry/abandon 同一套游标模式），把每条请求
 * 转成一次加价替换：
 *
 *   - 只处理 BTC 提现。RGB20 提现的 seal 绑在 BTC tx 输出上，替换等于作废整笔 RGB 转账，
 *     因此直接跳过（Debug）。
 *   - 用户请求的费率是**上限**：实际费率 = min(请求费率, 让用户输出仍 >= dust 的最大费率)。
 *     封顶后若不能比已生效费率至少高 1 sat/vB（BIP125），本次不签，只记 WARN，不写终态。
 *   - 构造/校验/签名/广播复用 processWithdrawRequest 的同一条路径，替换只额外保证复用
 *     sticky 输入（互斥）、输出仍付原地址且金额 = 原 pending 金额 - 新手续费、OP_RETURN
 *     承诺仍是原 burn。
 *   - 已生效费率按 burn 落盘，重启后仍单调；替换交易的最终字节也覆盖写回重播记录。
 *
 * 桥节点不对外提供服务，因此**不新增查询 RPC**：结果只通过日志/告警体现。
 */

const (
	// bumpWithdrawFeePollInterval 链上加价请求的常规轮询间隔。
	bumpWithdrawFeePollInterval = 10 * time.Second
	// bumpWithdrawFeeMaxPollInterval 查询连续失败时的退避上限。
	bumpWithdrawFeeMaxPollInterval = 5 * time.Minute
	// bumpWithdrawFeeQueryCount 单页查询条数（与 abandon/deposit 查询同一量级）。
	bumpWithdrawFeeQueryCount = 128
	// bumpWithdrawFeeHeightScanWindow 单次查询覆盖的高度窗口。
	bumpWithdrawFeeHeightScanWindow = 500
	// bumpWithdrawFeeMaxWindowsPerPoll 单轮最多扫多少个高度窗口。
	bumpWithdrawFeeMaxWindowsPerPoll = 200
	// bumpWithdrawFeeRetryMaxDelay 单条请求处理失败后的最大重试间隔。
	bumpWithdrawFeeRetryMaxDelay = 10 * time.Minute
	// bumpWithdrawFeeQueryTimeout 单次主链查询的超时。主链 QueryChain 在并发下可能永久 hang；
	// 签名节点是在处理签名通知时查询链上加价请求的，不能让它卡住整个 TSS 通知循环。
	bumpWithdrawFeeQueryTimeout = 8 * time.Second
)

var (
	// bumpWithdrawFeeCursorBucket 保存请求扫描游标（height:index），重启后从断点继续。
	bumpWithdrawFeeCursorBucket = []byte("rgbx-bump-withdraw-fee-cursor")
	bumpWithdrawFeeCursorKey    = []byte("cursor")
)

// bumpWithdrawFeeProcessor 按高度区间拉取链上加价请求并逐条处理。
// query/handle 可注入（单测不依赖链/钱包）。失败条目不阻塞其余条目，按指数退避重试。
type bumpWithdrawFeeProcessor struct {
	query  func(startHeight, startIndex, endHeight int64) (*rtypes.BumpWithdrawFeeList, error)
	handle func(item *rtypes.BumpWithdrawFeeItem) error

	nextHeight int64
	nextIndex  int64
	retrying   []*bumpWithdrawFeeRetry
}

type bumpWithdrawFeeRetry struct {
	item      *rtypes.BumpWithdrawFeeItem
	attempts  int
	nextRetry time.Time
}

// enqueueRetry 登记/更新一条处理失败的请求（按请求 txHash 去重）。
func (p *bumpWithdrawFeeProcessor) enqueueRetry(item *rtypes.BumpWithdrawFeeItem, now time.Time) {
	if item == nil {
		return
	}
	key := string(item.GetTxHash())
	for _, retry := range p.retrying {
		if string(retry.item.GetTxHash()) == key {
			retry.attempts++
			retry.nextRetry = now.Add(bumpWithdrawFeeRetryDelay(retry.attempts))
			return
		}
	}
	p.retrying = append(p.retrying, &bumpWithdrawFeeRetry{
		item:      item,
		attempts:  1,
		nextRetry: now.Add(bumpWithdrawFeeRetryDelay(1)),
	})
}

// bumpWithdrawFeeRetryDelay 指数退避，上限 bumpWithdrawFeeRetryMaxDelay。
func bumpWithdrawFeeRetryDelay(attempts int) time.Duration {
	if attempts <= 1 {
		return bumpWithdrawFeePollInterval
	}
	delay := bumpWithdrawFeePollInterval
	for i := 1; i < attempts; i++ {
		delay *= 2
		if delay >= bumpWithdrawFeeRetryMaxDelay {
			return bumpWithdrawFeeRetryMaxDelay
		}
	}
	return delay
}

// handleItem 处理一条请求；失败进入重试集合，不影响同批其余条目。
func (p *bumpWithdrawFeeProcessor) handleItem(item *rtypes.BumpWithdrawFeeItem, now time.Time) {
	if err := p.handle(item); err != nil {
		log.Warn("bumpWithdrawFee request failed, will retry with backoff",
			"bumpRequestTxHash", hex.EncodeToString(item.GetTxHash()),
			"withdrawalTxHash", hex.EncodeToString(item.GetWithdrawalTxHash()), "err", err)
		p.enqueueRetry(item, now)
	}
}

// retryPending 重试到期的失败条目；成功即移出集合。
func (p *bumpWithdrawFeeProcessor) retryPending(now time.Time) {
	if len(p.retrying) == 0 {
		return
	}
	kept := p.retrying[:0]
	for _, retry := range p.retrying {
		if now.Before(retry.nextRetry) {
			kept = append(kept, retry)
			continue
		}
		if err := p.handle(retry.item); err != nil {
			retry.attempts++
			retry.nextRetry = now.Add(bumpWithdrawFeeRetryDelay(retry.attempts))
			log.Warn("bumpWithdrawFee retry still failing",
				"bumpRequestTxHash", hex.EncodeToString(retry.item.GetTxHash()), "attempts", retry.attempts, "err", err)
			kept = append(kept, retry)
			continue
		}
	}
	p.retrying = kept
}

// pollOnce 从当前游标扫到 endHeight，返回查询是否成功（失败不推进游标）。
func (p *bumpWithdrawFeeProcessor) pollOnce(endHeight int64, now time.Time) bool {
	for windows := 0; windows < bumpWithdrawFeeMaxWindowsPerPoll && p.nextHeight <= endHeight; windows++ {
		windowEnd := p.nextHeight + bumpWithdrawFeeHeightScanWindow - 1
		if windowEnd > endHeight {
			windowEnd = endHeight
		}
		list, err := p.query(p.nextHeight, p.nextIndex, windowEnd)
		if err != nil {
			log.Warn("bumpWithdrawFee list requests failed, retry next round",
				"startHeight", p.nextHeight, "startIndex", p.nextIndex, "endHeight", windowEnd, "err", err)
			return false
		}
		items := list.GetItems()
		for _, item := range items {
			p.handleItem(item, now)
		}
		// A full page may end in the middle of the window: continue from the last item.
		if len(items) >= bumpWithdrawFeeQueryCount {
			last := items[len(items)-1]
			if last.GetHeight() >= p.nextHeight {
				p.nextHeight = last.GetHeight()
				p.nextIndex = int64(last.GetIndex()) + 1
				continue
			}
		}
		p.nextHeight = windowEnd + 1
		p.nextIndex = 0
	}
	return true
}

// loadBumpWithdrawFeeCursor 读加价请求扫描断点。缺失/损坏一律从头开始（处理是幂等的）。
func (n *neutrinoClient) loadBumpWithdrawFeeCursor() (int64, int64) {
	if n.neutrinoCfg.Database == nil {
		return 0, 0
	}
	var height, index int64
	err := walletdb.View(n.neutrinoCfg.Database, func(tx walletdb.ReadTx) error {
		bucket := tx.ReadBucket(bumpWithdrawFeeCursorBucket)
		if bucket == nil {
			return nil
		}
		value := bucket.Get(bumpWithdrawFeeCursorKey)
		if value == nil {
			return nil
		}
		_, err := fmt.Sscanf(string(value), "%d:%d", &height, &index)
		return err
	})
	if err != nil {
		log.Warn("bumpWithdrawFee read cursor failed, restarting from height 0", "err", err)
		return 0, 0
	}
	if height < 0 {
		height = 0
	}
	if index < 0 {
		index = 0
	}
	return height, index
}

// saveBumpWithdrawFeeCursor 记录扫描断点（只在查询成功后推进）。
func (n *neutrinoClient) saveBumpWithdrawFeeCursor(height, index int64) {
	if n.neutrinoCfg.Database == nil {
		return
	}
	err := walletdb.Update(n.neutrinoCfg.Database, func(tx walletdb.ReadWriteTx) error {
		bucket, err := tx.CreateTopLevelBucket(bumpWithdrawFeeCursorBucket)
		if err != nil {
			return err
		}
		return bucket.Put(bumpWithdrawFeeCursorKey, []byte(fmt.Sprintf("%d:%d", height, index)))
	})
	if err != nil {
		log.Error("bumpWithdrawFee save cursor failed", "height", height, "index", index, "err", err)
	}
}

// listBumpWithdrawFeeRequests 按高度区间查询链上已记录的加价请求。
func (n *neutrinoClient) listBumpWithdrawFeeRequests(ctx context.Context, startHeight, startIndex, endHeight int64,
) (*rtypes.BumpWithdrawFeeList, error) {
	req := &rtypes.ReqListPendingTx{
		StartHeight: startHeight,
		StartIndex:  startIndex,
		EndHeight:   endHeight,
		Count:       bumpWithdrawFeeQueryCount,
	}
	reply, err := n.mainChainGrpc.QueryChain(ctx, &types.ChainExecutor{
		Driver:   rtypes.RgbxX,
		FuncName: "ListBumpWithdrawFeeRequests",
		Param:    types.Encode(req),
	})
	if err != nil {
		return nil, err
	}
	if !reply.GetIsOk() {
		return nil, fmt.Errorf("ListBumpWithdrawFeeRequests not ok: %s", string(reply.GetMsg()))
	}
	list := &rtypes.BumpWithdrawFeeList{}
	if err := types.Decode(reply.GetMsg(), list); err != nil {
		return nil, err
	}
	return list, nil
}

// getMainChainHeightBounded 带超时读取主链高度（签名通知路径不允许被主链查询卡死）。
func (n *neutrinoClient) getMainChainHeightBounded() (int64, error) {
	ctx, cancel := context.WithTimeout(n.ctx, bumpWithdrawFeeQueryTimeout)
	defer cancel()
	reply, err := n.mainChainGrpc.GetLastHeader(ctx, &types.ReqNil{})
	if err != nil {
		return 0, err
	}
	return reply.GetHeight(), nil
}

// listBumpWithdrawFeeRequestsBounded 是带超时的单次查询（协调者轮询与签名节点核对共用）。
func (n *neutrinoClient) listBumpWithdrawFeeRequestsBounded(startHeight, startIndex, endHeight int64,
) (*rtypes.BumpWithdrawFeeList, error) {
	ctx, cancel := context.WithTimeout(n.ctx, bumpWithdrawFeeQueryTimeout)
	defer cancel()
	return n.listBumpWithdrawFeeRequests(ctx, startHeight, startIndex, endHeight)
}

// findBumpWithdrawFeeRequest 在链上从 burn 所在高度向后查找指定的加价请求。
//
// 签名节点只能信链上事实：它按通知里的 requestTxHash 精确定位请求，并核对请求属于这笔 burn，
// 绝不使用通知里声称的费率。
func (n *neutrinoClient) findBumpWithdrawFeeRequest(burnTxHash []byte, fromHeight int64,
	requestTxHash []byte) (*rtypes.BumpWithdrawFeeItem, error) {

	if len(burnTxHash) == 0 || len(requestTxHash) == 0 {
		return nil, fmt.Errorf("bump request lookup needs a burn hash and a request hash")
	}
	endHeight, err := n.getMainChainHeightBounded()
	if err != nil || endHeight <= 0 {
		return nil, fmt.Errorf("cannot read the main chain height: %w", err)
	}
	if fromHeight < 0 {
		fromHeight = 0
	}
	if fromHeight > endHeight {
		fromHeight = endHeight
	}
	startHeight, startIndex := fromHeight, int64(0)
	for windows := 0; windows < bumpWithdrawFeeMaxWindowsPerPoll; windows++ {
		windowEnd := startHeight + bumpWithdrawFeeHeightScanWindow - 1
		if windowEnd > endHeight {
			windowEnd = endHeight
		}
		list, err := n.listBumpWithdrawFeeRequestsBounded(startHeight, startIndex, windowEnd)
		if err != nil {
			return nil, err
		}
		items := list.GetItems()
		for _, item := range items {
			if bytes.Equal(item.GetTxHash(), requestTxHash) &&
				bytes.Equal(item.GetWithdrawalTxHash(), burnTxHash) {
				return item, nil
			}
		}
		if len(items) >= bumpWithdrawFeeQueryCount {
			last := items[len(items)-1]
			if last.GetHeight() >= startHeight {
				startHeight = last.GetHeight()
				startIndex = int64(last.GetIndex()) + 1
				continue
			}
		}
		if windowEnd >= endHeight {
			break
		}
		startHeight = windowEnd + 1
		startIndex = 0
	}
	return nil, fmt.Errorf("on-chain bump request %s for burn %s was not found",
		hex.EncodeToString(requestTxHash), hex.EncodeToString(burnTxHash))
}

// bumpWithdrawFeeHandlerDeps 把处理器与真实链/钱包解耦，单测可直接注入。
type bumpWithdrawFeeHandlerDeps struct {
	getPending       func([]byte) (*rtypes.PendingTx, error)
	isRgb20          func(string) bool
	getState         func([]byte) []byte
	getSticky        func([]byte) *UTXO
	getSignedTx      func([]byte) ([]byte, bool, error)
	getAppliedRate   func([]byte) (btcutil.Amount, bool)
	loadInputAmounts func([]byte) ([]int64, bool)
	process          func(*withdrawRequest) error
	recordEvent      func(kind, key string, payload map[string]any)
}

type bumpWithdrawFeeHandler struct {
	deps bumpWithdrawFeeHandlerDeps
}

// handle 处理一条链上加价请求。返回 error 表示可重试的失败；nil 表示成功或按规则跳过。
func (h *bumpWithdrawFeeHandler) handle(item *rtypes.BumpWithdrawFeeItem) error {
	if item == nil || len(item.GetWithdrawalTxHash()) == 0 || item.GetFeeRate() <= 0 {
		return fmt.Errorf("invalid bump withdraw fee request")
	}
	burnTxHash := item.GetWithdrawalTxHash()
	pending, err := h.deps.getPending(burnTxHash)
	if err != nil {
		return err
	}
	if pending == nil {
		return fmt.Errorf("pending withdraw %s not found", hex.EncodeToString(burnTxHash))
	}
	if pending.GetConfirmed() {
		log.Debug("bumpWithdrawFee skip confirmed withdraw", "withdrawalTxHash", hex.EncodeToString(burnTxHash))
		return nil
	}
	// RGB20 的提现 seal 绑在 BTC tx 输出上；替换会作废整笔 RGB 转移，因此不处理。
	if h.deps.isRgb20(pending.GetAssetSymbol()) {
		log.Debug("bumpWithdrawFee skip rgb20 withdraw (its seal is bound to the BTC tx)",
			"withdrawalTxHash", hex.EncodeToString(burnTxHash), "assetSymbol", pending.GetAssetSymbol())
		return nil
	}
	// 只有"已广播未确认"（或状态尚未落盘）的提现允许替换；已确认/放弃/解锁一律不碰。
	if state := h.deps.getState(burnTxHash); !signedTxReplayEligible(state) {
		log.Debug("bumpWithdrawFee skip withdraw in a non-replayable state",
			"withdrawalTxHash", hex.EncodeToString(burnTxHash), "state", string(state))
		return nil
	}
	sticky := h.deps.getSticky(burnTxHash)
	if sticky == nil {
		// 首次提现可能还在签名/广播中：此时没有可安全复用的互斥输入。按可重试处理，
		// 避免"加价请求先到、初次提现后到"时把用户请求永久丢掉。
		return fmt.Errorf("withdraw %s has no bound sticky input yet", hex.EncodeToString(burnTxHash))
	}
	applied := btcutil.Amount(pending.GetFeeRate())
	if stored, ok := h.deps.getAppliedRate(burnTxHash); ok && stored > applied {
		applied = stored
	}
	requested := btcutil.Amount(item.GetFeeRate())
	if requested <= applied {
		log.Debug("bumpWithdrawFee skip stale request: rate is not above the applied rate",
			"withdrawalTxHash", hex.EncodeToString(burnTxHash),
			"requestedFeeRate", requested, "appliedFeeRate", applied)
		return nil
	}
	prevRaw, ok, err := h.deps.getSignedTx(burnTxHash)
	if err != nil {
		return err
	}
	if !ok {
		// 初次提现尚未产生已签交易：可重试（用户可能先提交了加价请求）。
		return fmt.Errorf("withdraw %s has no previous signed transaction yet", hex.EncodeToString(burnTxHash))
	}
	var prevTx wire.MsgTx
	if err := prevTx.Deserialize(bytes.NewReader(prevRaw)); err != nil {
		log.Warn("bumpWithdrawFee skip withdraw with an undecodable previous signed tx",
			"withdrawalTxHash", hex.EncodeToString(burnTxHash), "err", err)
		return nil
	}
	prevAmounts, ok := h.deps.loadInputAmounts(burnTxHash)
	if !ok || len(prevAmounts) != len(prevTx.TxIn) {
		log.Warn("bumpWithdrawFee skip withdraw: previous input amounts are unavailable "+
			"(signers cannot verify the absolute-fee increase)", "withdrawalTxHash", hex.EncodeToString(burnTxHash))
		return nil
	}

	req := pending2WithdrawRequest(pending)
	req.stickyUTXO = sticky
	req.feeRate = requested
	req.bump = &withdrawBumpContext{
		requestTxHash:    item.GetTxHash(),
		requestedFeeRate: requested,
		appliedFeeRate:   applied,
		prevSignedTx:     &prevTx,
		prevInputAmounts: prevAmounts,
	}
	if err := h.deps.process(req); err != nil {
		if errors.Is(err, errWithdrawBumpRateNotHigher) {
			// 用户请求无法兑现（被 dust 上限封顶后仍不高于已生效费率）：不签、不写终态。
			log.Warn("ALERT withdraw fee bump cannot be applied: the requested rate cannot raise the "+
				"applied rate by at least 1 sat/vB while keeping the user output above dust",
				"withdrawalTxHash", hex.EncodeToString(burnTxHash),
				"requestedFeeRate", requested, "appliedFeeRate", applied,
				"bumpRequestTxHash", hex.EncodeToString(item.GetTxHash()))
			return nil
		}
		return err
	}
	log.Info("bumpWithdrawFee replacement applied",
		"withdrawalTxHash", hex.EncodeToString(burnTxHash),
		"requestedFeeRate", requested, "effectiveFeeRate", req.bump.effectiveFeeRate,
		"previousFeeRate", applied, "bumpRequestTxHash", hex.EncodeToString(item.GetTxHash()))
	if h.deps.recordEvent != nil {
		h.deps.recordEvent("withdraw.bumped", hex.EncodeToString(burnTxHash), map[string]any{
			"chain33_tx_hash":    hex.EncodeToString(burnTxHash),
			"asset_symbol":       pending.GetAssetSymbol(),
			"amount":             pending.GetAmount(),
			"previous_fee_rate":  applied,
			"requested_fee_rate": requested,
			"effective_fee_rate": req.bump.effectiveFeeRate,
			"bump_request_tx":    hex.EncodeToString(item.GetTxHash()),
		})
	}
	return nil
}

// newBumpWithdrawFeeHandler 用真实节点依赖装配处理器。
func (n *neutrinoClient) newBumpWithdrawFeeHandler() *bumpWithdrawFeeHandler {
	return &bumpWithdrawFeeHandler{deps: bumpWithdrawFeeHandlerDeps{
		getPending: n.getRgbxPendingTxByHash,
		isRgb20:    n.isRgb20Asset,
		getState:   n.getWithdrawState,
		getSticky:  n.getWithdrawStickyUTXO,
		getSignedTx: func(burnTxHash []byte) ([]byte, bool, error) {
			raw, err := n.signedWithdrawTxStorage().Get(burnTxHash)
			if err != nil {
				return nil, false, err
			}
			return raw, len(raw) > 0, nil
		},
		getAppliedRate:   n.getAppliedWithdrawFeeRate,
		loadInputAmounts: n.loadSignedWithdrawInputAmounts,
		process:          n.processWithdrawRequest,
		recordEvent:      n.rgb20.RecordEvent,
	}}
}

// bumpWithdrawFeeWorker 运行在协调者（官方节点）上：轮询链上加价请求并逐条重签替换。
func (n *neutrinoClient) bumpWithdrawFeeWorker() {
	if n == nil || n.bw == nil {
		return
	}
	p := &bumpWithdrawFeeProcessor{
		query:  n.listBumpWithdrawFeeRequestsBounded,
		handle: n.newBumpWithdrawFeeHandler().handle,
	}
	p.nextHeight, p.nextIndex = n.loadBumpWithdrawFeeCursor()
	log.Info("bumpWithdrawFeeWorker start", "startHeight", p.nextHeight, "startIndex", p.nextIndex)

	delay := bumpWithdrawFeePollInterval
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-n.ctx.Done():
			return
		case now := <-timer.C:
			p.retryPending(now)
			ok := false
			if endHeight := n.getMainChainHeight(); endHeight > 0 {
				prevHeight, prevIndex := p.nextHeight, p.nextIndex
				ok = p.pollOnce(endHeight, now)
				if p.nextHeight != prevHeight || p.nextIndex != prevIndex {
					n.saveBumpWithdrawFeeCursor(p.nextHeight, p.nextIndex)
				}
			}
			// 查询失败按指数退避，避免链不可用时刷屏；成功后回到常规间隔。
			if ok {
				delay = bumpWithdrawFeePollInterval
			} else {
				delay *= 2
				if delay > bumpWithdrawFeeMaxPollInterval {
					delay = bumpWithdrawFeeMaxPollInterval
				}
			}
			timer.Reset(delay)
		}
	}
}
