package neutrino

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/33cn/chain33/types"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcwallet/walletdb"
)

// 手工放弃提现（Abandon）流程的桥侧部分。
//
// 链上的 AbandonWithdraw 只是**存证**：提案的受理完全由各节点用本地白名单决定。受理后进入
// abandoning 单向状态（永不自动清除），由协调者复用既有 Unlock 通道把锁定金额退回台账 owner。
// 与资产无关：BTC 与 RGB20 走同一条流程；RGB20 的自动解锁路径（permanent 失败）保持不变。

const (
	// abandonPollInterval 链上提案的轮询间隔。
	abandonPollInterval = 10 * time.Second
	// abandonQueryCount 单页查询条数（与 pullPendingTx 同一量级）。
	abandonQueryCount = 128
	// abandonHeightScanWindow 单次查询覆盖的高度窗口。链上实现每次最多遍历
	// maxAbandonHeightScan（= maxListCount = 1000）个高度，这里取更小的窗口，避免依赖该上限，
	// 也避免"窗口内没有提案 ⇒ 返回空"被误判为"已经扫完"。
	abandonHeightScanWindow = 500
	// abandonMaxWindowsPerPoll 单轮最多扫多少个高度窗口：第一次部署/游标丢失时从头追赶，
	// 不让一次 poll 长时间占住。
	abandonMaxWindowsPerPoll = 200

	// withdrawFinalSettled / withdrawFinalReleased 链上终态值（GetWithdrawFinalState 的返回）。
	withdrawFinalSettled  = "settled"
	withdrawFinalReleased = "released"
)

var (
	// withdrawStatusAbandoning 手工放弃已受理。单向：永不自动清除，直到进程生命周期结束也保留；
	// 是否收尾只看链上终态键（settled / released）。
	withdrawStatusAbandoning = []byte("abandoning")

	// abandonCursorBucket 保存提案扫描游标（height:index），重启后从断点继续。
	abandonCursorBucket = []byte("rgbx-abandon-cursor")
	abandonCursorKey    = []byte("cursor")
)

// isAbandoningState 该本地状态是否处于手工放弃流程。
func isAbandoningState(state []byte) bool {
	return bytes.Equal(state, withdrawStatusAbandoning)
}

// settlementAllowed 结算重试是否仍被允许。两类重试路径（BTC / RGB20）都过这一道门。
func settlementAllowed(state []byte) bool {
	return !isAbandoningState(state)
}

// isAbandonOperator 提案人是否在本地白名单内。地址区分大小写，按原样逐字比对。
func isAbandonOperator(operators []string, proposer string) bool {
	proposer = strings.TrimSpace(proposer)
	if proposer == "" {
		return false
	}
	for _, op := range operators {
		if strings.TrimSpace(op) == proposer {
			return true
		}
	}
	return false
}

// evaluateAbandonProposal 是受理判定的纯逻辑（便于单测）。返回 nil 表示受理；否则为拒绝原因。
//
// 全部通过才受理：
//   - 提案人在白名单内；
//   - 链上 Withdraw payload 存在且成形；
//   - pending 是未确认的提现，台账 owner 与提案 owner 逐字相等；
//   - 链上终态键为空（settled/released 都算已终态）；
//   - 本节点从未产出过付款签名。
func evaluateAbandonProposal(item *rtypes.AbandonWithdrawItem, pending *rtypes.PendingTx,
	withdraw *rtypes.WithdrawAsset, terminal string, operators []string, signedPayment bool) error {

	if item == nil || len(item.GetBurnTxHash()) == 0 {
		return fmt.Errorf("empty burn tx hash")
	}
	if !isAbandonOperator(operators, item.GetProposer()) {
		return fmt.Errorf("proposer %q is not a configured abandon operator", item.GetProposer())
	}
	if withdraw == nil || withdraw.GetAssetSymbol() == "" {
		return fmt.Errorf("withdraw payload missing or malformed")
	}
	if pending == nil {
		return fmt.Errorf("pending withdraw not found")
	}
	if pending.GetActionType() != rtypes.TyWithdrawAsset {
		return fmt.Errorf("pending action type %d is not a withdraw", pending.GetActionType())
	}
	if pending.GetConfirmed() {
		return fmt.Errorf("withdraw already confirmed")
	}
	if terminal != "" {
		return fmt.Errorf("withdraw already terminal on chain: %s", terminal)
	}
	if item.GetOwner() == "" {
		return fmt.Errorf("proposal owner is empty")
	}
	if item.GetOwner() != pending.GetFromAddress() {
		return fmt.Errorf("owner mismatch: proposal=%q ledger=%q", item.GetOwner(), pending.GetFromAddress())
	}
	if signedPayment {
		return fmt.Errorf("a payment signature was already produced for this burn")
	}
	return nil
}

// abandonProcessor 轮询链上提案并按高度区间推进游标；processed 记录已处理过的提案 txHash，
// 避免重复受理（受理本身也是幂等的：已处于 abandoning 的提案会被直接跳过）。
//
// retrying 是本进程内"已受理、尚未解锁成功"的 burn 集合：pending 提现只在启动后的全量拉取里
// 投递一次（游标随后前进），所以解锁重试必须由这里驱动，不能指望 withdrawReqChan 再投递。
type abandonProcessor struct {
	query func(startHeight, startIndex, endHeight int64) (*rtypes.AbandonWithdrawList, error)
	// handle 处理一条提案；返回 true 表示该 burn 已处于 abandoning，需要继续重试解锁。
	handle func(item *rtypes.AbandonWithdrawItem) bool
	// retry 推进一笔已受理的放弃；返回 true 表示下个周期继续重试。
	retry      func(burnTxHash []byte) bool
	nextHeight int64
	nextIndex  int64
	processed  map[string]struct{}
	retrying   [][]byte
}

// addRetry 登记一笔需要重试解锁的 burn（去重）。
func (p *abandonProcessor) addRetry(burnTxHash []byte) {
	if len(burnTxHash) == 0 {
		return
	}
	for _, h := range p.retrying {
		if bytes.Equal(h, burnTxHash) {
			return
		}
	}
	p.retrying = append(p.retrying, burnTxHash)
}

// retryPending 推进本轮所有待重试的 burn；链上已到终态的移出集合。
func (p *abandonProcessor) retryPending() {
	if p.retry == nil {
		return
	}
	kept := p.retrying[:0]
	for _, h := range p.retrying {
		if p.retry(h) {
			kept = append(kept, h)
		}
	}
	p.retrying = kept
}

// pollOnce 从当前游标扫到 endHeight（每次最多 abandonMaxWindowsPerPoll 个高度窗口）。
// 查询失败不推进游标，下个周期重试。
func (p *abandonProcessor) pollOnce(endHeight int64) {
	if p.processed == nil {
		p.processed = make(map[string]struct{})
	}
	for windows := 0; windows < abandonMaxWindowsPerPoll && p.nextHeight <= endHeight; windows++ {
		windowEnd := p.nextHeight + abandonHeightScanWindow - 1
		if windowEnd > endHeight {
			windowEnd = endHeight
		}
		list, err := p.query(p.nextHeight, p.nextIndex, windowEnd)
		if err != nil {
			log.Warn("abandonProcessor list proposals failed", "startHeight", p.nextHeight,
				"startIndex", p.nextIndex, "endHeight", windowEnd, "err", err)
			return
		}
		items := list.GetItems()
		for _, item := range items {
			key := hex.EncodeToString(item.GetTxHash())
			if _, done := p.processed[key]; done {
				continue
			}
			p.processed[key] = struct{}{}
			if p.handle != nil && p.handle(item) {
				p.addRetry(item.GetBurnTxHash())
			}
		}
		// A full page may end in the middle of the window: continue from the last item.
		if len(items) >= abandonQueryCount {
			last := items[len(items)-1]
			if last.GetHeight() >= p.nextHeight {
				p.nextHeight = last.GetHeight()
				p.nextIndex = int64(last.GetIndex()) + 1
				continue
			}
		}
		// Otherwise the whole window was scanned (empty windows included): move past it.
		p.nextHeight = windowEnd + 1
		p.nextIndex = 0
	}
}

// loadAbandonCursor 读提案扫描断点。缺失/损坏一律从头开始（重复处理是幂等的）。
func (n *neutrinoClient) loadAbandonCursor() (int64, int64) {
	if n.neutrinoCfg.Database == nil {
		return 0, 0
	}
	var height, index int64
	err := walletdb.View(n.neutrinoCfg.Database, func(tx walletdb.ReadTx) error {
		bucket := tx.ReadBucket(abandonCursorBucket)
		if bucket == nil {
			return nil
		}
		value := bucket.Get(abandonCursorKey)
		if value == nil {
			return nil
		}
		_, err := fmt.Sscanf(string(value), "%d:%d", &height, &index)
		return err
	})
	if err != nil {
		log.Warn("abandonWorker read cursor failed, restarting from height 0", "err", err)
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

// saveAbandonCursor 记录提案扫描断点（只在查询成功后推进）。
func (n *neutrinoClient) saveAbandonCursor(height, index int64) {
	if n.neutrinoCfg.Database == nil {
		return
	}
	err := walletdb.Update(n.neutrinoCfg.Database, func(tx walletdb.ReadWriteTx) error {
		bucket, err := tx.CreateTopLevelBucket(abandonCursorBucket)
		if err != nil {
			return err
		}
		return bucket.Put(abandonCursorKey, []byte(fmt.Sprintf("%d:%d", height, index)))
	})
	if err != nil {
		log.Error("abandonWorker save cursor failed", "height", height, "index", index, "err", err)
	}
}

// loadAbandoningBurns 从本地 withdraw state 重建"已受理放弃"的 burn 集合。
// 它是重启恢复的权威来源：提案扫描游标可能落后，但已落盘的 abandoning 记录必须继续解锁。
func (n *neutrinoClient) loadAbandoningBurns() [][]byte {
	if n.neutrinoCfg.Database == nil {
		return nil
	}
	var burns [][]byte
	err := walletdb.View(n.neutrinoCfg.Database, func(tx walletdb.ReadTx) error {
		bucket := tx.ReadBucket(withdrawStateBucket)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(k, v []byte) error {
			if v == nil || !isAbandoningState(v) {
				return nil
			}
			key := make([]byte, len(k))
			copy(key, k)
			burns = append(burns, key)
			return nil
		})
	})
	if err != nil {
		log.Error("abandonWorker load abandoning burns failed", "err", err)
		return nil
	}
	return burns
}

// abandonWorker 运行在协调者（官方节点）上：该节点同时负责结算重试与解锁，手工放弃必须与它
// 共用同一套状态机，否则会出现"一个进程还在付款、另一个进程已经退款"。
//
// 它是手工路径的唯一驱动者：每轮先发现/受理提案，再推进已受理 burn 的解锁重试。重启后从
// 持久游标继续扫提案，并从本地 abandoning 状态重建重试集合（不依赖提案是否在扫描窗口内）。
func (n *neutrinoClient) abandonWorker() {
	// 已受理的 burn 从本地状态恢复：即使提案扫描游标丢了、或白名单被临时清空，
	// 也不能让一笔已经承诺退款的提现卡住。
	p := &abandonProcessor{
		query:     n.listAbandonRequests,
		handle:    n.handleAbandonProposal,
		retry:     n.retryAbandonedWithdraw,
		processed: make(map[string]struct{}),
	}
	for _, burn := range n.loadAbandoningBurns() {
		p.addRetry(burn)
	}
	p.nextHeight, p.nextIndex = n.loadAbandonCursor()

	if len(n.cfg.AbandonOperators) == 0 && len(p.retrying) == 0 {
		log.Info("abandonWorker disabled: no abandonOperators configured")
		return
	}
	if len(n.cfg.AbandonOperators) == 0 {
		log.Warn("abandonWorker: abandonOperators is empty, only retrying already-accepted abandons")
	}
	ticker := time.NewTicker(abandonPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-ticker.C:
			if len(n.cfg.AbandonOperators) > 0 {
				endHeight := n.getMainChainHeight()
				if endHeight > 0 {
					prevHeight, prevIndex := p.nextHeight, p.nextIndex
					p.pollOnce(endHeight)
					if p.nextHeight != prevHeight || p.nextIndex != prevIndex {
						n.saveAbandonCursor(p.nextHeight, p.nextIndex)
					}
				}
			}
			p.retryPending()
		}
	}
}

// listAbandonRequests 按高度区间查询已上链的放弃提案。
//
// 用字符串函数名调用：链上查询（并行实现中）可能暂时不存在，但桥侧不应因此编译不过；
// 查询失败会在下个周期重试。
func (n *neutrinoClient) listAbandonRequests(startHeight, startIndex, endHeight int64) (*rtypes.AbandonWithdrawList, error) {
	req := &rtypes.ReqListPendingTx{
		StartHeight: startHeight,
		StartIndex:  startIndex,
		EndHeight:   endHeight,
		Count:       abandonQueryCount,
	}
	reply, err := n.mainChainGrpc.QueryChain(n.ctx, &types.ChainExecutor{
		Driver:   rtypes.RgbxX,
		FuncName: "ListAbandonRequests",
		Param:    types.Encode(req),
	})
	if err != nil {
		return nil, err
	}
	if !reply.GetIsOk() {
		return nil, fmt.Errorf("ListAbandonRequests not ok: %s", string(reply.GetMsg()))
	}
	list := &rtypes.AbandonWithdrawList{}
	if err := types.Decode(reply.GetMsg(), list); err != nil {
		return nil, err
	}
	return list, nil
}

// getWithdrawFinalState 读链上终态键（"" = 仍未结算/未退回）。它是 settled 与 released 的
// 唯一权威来源，本地状态只用于停止重复提交。
func (n *neutrinoClient) getWithdrawFinalState(burnTxHash []byte) (string, error) {
	reply, err := n.mainChainGrpc.QueryChain(n.ctx, &types.ChainExecutor{
		Driver:   rtypes.RgbxX,
		FuncName: "GetWithdrawFinalState",
		Param:    types.Encode(&types.ReqString{Data: hex.EncodeToString(burnTxHash)}),
	})
	if err != nil {
		return "", err
	}
	if !reply.GetIsOk() {
		return "", fmt.Errorf("GetWithdrawFinalState not ok: %s", string(reply.GetMsg()))
	}
	out := &types.ReplyString{}
	if err := types.Decode(reply.GetMsg(), out); err != nil {
		return "", err
	}
	return out.GetData(), nil
}

// handleAbandonProposal 逐条核对并（通过时）受理一笔手工放弃提案。返回 true 表示该 burn 处于
// abandoning，需要继续重试解锁（已受理过的提案也返回 true，用于重启后重建重试集合）。
//
// 受理是单向的：先持久化 abandoning 再解锁。持久化失败即拒绝受理（否则重启后会回到结算路径）。
func (n *neutrinoClient) handleAbandonProposal(item *rtypes.AbandonWithdrawItem) bool {
	if item == nil || len(item.GetBurnTxHash()) == 0 {
		log.Warn("abandon proposal without burn tx hash", "txHash", hex.EncodeToString(item.GetTxHash()))
		return false
	}
	burnTxHash := item.GetBurnTxHash()
	burnHex := hex.EncodeToString(burnTxHash)
	proposalHex := hex.EncodeToString(item.GetTxHash())

	if isAbandoningState(n.getWithdrawState(burnTxHash)) {
		log.Debug("abandon proposal already accepted", "proposalTxHash", proposalHex, "burnTxHash", burnHex)
		return true
	}

	withdraw, err := n.getRgbxWithdrawAsset(burnTxHash)
	if err != nil {
		log.Warn("abandon proposal rejected: withdraw payload unreadable",
			"proposalTxHash", proposalHex, "burnTxHash", burnHex, "err", err)
		return false
	}
	pending, err := n.getRgbxPendingTxByHash(burnTxHash)
	if err != nil {
		log.Warn("abandon proposal rejected: pending withdraw unreadable",
			"proposalTxHash", proposalHex, "burnTxHash", burnHex, "err", err)
		return false
	}
	terminal, err := n.getWithdrawFinalState(burnTxHash)
	if err != nil {
		log.Warn("abandon proposal rejected: final state unreadable",
			"proposalTxHash", proposalHex, "burnTxHash", burnHex, "err", err)
		return false
	}
	if err := evaluateAbandonProposal(item, pending, withdraw, terminal,
		n.cfg.AbandonOperators,
		paymentSignatureMayExist(n.hasSignedPayment(burnTxHash), n.getWithdrawState(burnTxHash))); err != nil {
		log.Warn("abandon proposal rejected", "proposalTxHash", proposalHex,
			"burnTxHash", burnHex, "proposer", item.GetProposer(), "reason", err)
		return false
	}

	// The decision above raced with the settlement path. Re-check the two facts that can change
	// under this lock — an in-flight payment signature and the chain's terminal key — then persist.
	// Payment attempts take the same lock and re-check the abandoning state before signing.
	n.paymentMu.Lock()
	defer n.paymentMu.Unlock()
	if isAbandoningState(n.getWithdrawState(burnTxHash)) {
		return true
	}
	if n.hasSignedPayment(burnTxHash) {
		log.Warn("abandon proposal rejected: a payment signature appeared before acceptance",
			"proposalTxHash", proposalHex, "burnTxHash", burnHex)
		return false
	}
	if withdrawHasPaymentSignature(n.getWithdrawState(burnTxHash)) {
		log.Warn("abandon proposal rejected: legacy withdraw state says a payment was broadcast",
			"proposalTxHash", proposalHex, "burnTxHash", burnHex)
		return false
	}
	if terminal, err = n.getWithdrawFinalState(burnTxHash); err != nil {
		log.Warn("abandon proposal rejected: final state unreadable before acceptance",
			"proposalTxHash", proposalHex, "burnTxHash", burnHex, "err", err)
		return false
	} else if terminal != "" {
		log.Info("abandon proposal rejected: withdraw became terminal before acceptance",
			"proposalTxHash", proposalHex, "burnTxHash", burnHex, "terminal", terminal)
		return false
	}
	if err := n.setWithdrawState(burnTxHash, withdrawStatusAbandoning); err != nil {
		log.Error("abandon proposal refused: cannot persist abandoning state",
			"proposalTxHash", proposalHex, "burnTxHash", burnHex, "err", err)
		return false
	}
	log.Info("abandon proposal accepted", "proposalTxHash", proposalHex, "burnTxHash", burnHex,
		"proposer", item.GetProposer(), "owner", item.GetOwner(), "reason", item.GetReason())
	return true
}

// retryAbandonedWithdraw 推进一笔已受理的放弃：链上终态即收尾；否则只重试解锁，绝不回到结算。
// 返回 true 表示下个周期还需要重试。
func (n *neutrinoClient) retryAbandonedWithdraw(burnTxHash []byte) bool {
	state := n.getWithdrawState(burnTxHash)
	if !isAbandoningState(state) {
		return false
	}

	terminal, err := n.getWithdrawFinalState(burnTxHash)
	if err != nil {
		log.Warn("abandon: read final state failed, retry later",
			"burnTxHash", hex.EncodeToString(burnTxHash), "err", err)
		return true
	}
	switch terminal {
	case withdrawFinalSettled:
		// The payment won: settlement is the outcome, stop unlocking for good.
		log.Info("abandon: withdraw settled on chain, stop unlocking", "burnTxHash", hex.EncodeToString(burnTxHash))
		return false
	case withdrawFinalReleased:
		log.Info("abandon: withdraw already released on chain", "burnTxHash", hex.EncodeToString(burnTxHash))
		return false
	}

	pending, err := n.getRgbxPendingTxByHash(burnTxHash)
	if err != nil {
		log.Warn("abandon: read pending failed, retry later",
			"burnTxHash", hex.EncodeToString(burnTxHash), "err", err)
		return true
	}
	if pending.GetConfirmed() {
		log.Info("abandon: withdraw confirmed on chain, stop unlocking", "burnTxHash", hex.EncodeToString(burnTxHash))
		return false
	}
	// Defensive: a payment signature appeared after acceptance (should be impossible because both
	// sides share paymentMu). Unlocking now would risk a double payout, so stop and let settlement win.
	if n.hasSignedPayment(burnTxHash) {
		log.Error("abandon: payment signature exists, refuse to unlock",
			"burnTxHash", hex.EncodeToString(burnTxHash))
		return false
	}

	if err := n.tryUnlockWithdraw(pending); err != nil {
		log.Error("abandon: unlock failed, will retry",
			"burnTxHash", hex.EncodeToString(burnTxHash), "err", err)
		return true
	}
	// Keep the burn in the retry set until the chain reports released/settled: a successful local
	// submission is not proof the transaction landed, and resubmitting the same signed message is
	// harmless (the chain's terminal guard rejects duplicates).
	return true
}
