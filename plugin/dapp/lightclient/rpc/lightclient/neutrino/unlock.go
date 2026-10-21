package neutrino

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/33cn/chain33/types"
	ltypes "github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
)

// transactionTypeRgbxUnlock 放弃提现（Unlock）的 TSS 签名通知类型。
//
// Payload 是协调者声明的 unlockSignPayload（被签内容的声称值）；签名节点不接受这些声称值，
// 而是按 burnTxHash 从链上重新读出 pending 与 Withdraw payload（真值），逐项比对后才签
// 自己派生的 UnlockAsset。与提现 PSBT 签名一样，通知类型本身就是路由：没有"Payload 为空就
// 跳过核对"的旁路。
const transactionTypeRgbxUnlock = "rgbx-unlock"

// unlockSignPayload 协调者下发给签名节点的"被签内容"声称值。
//
// 它不参与签名：双方各自用链上事实重建 UnlockAsset，再对 rtypes.UnlockSignDigest 签名。
// 带上它只是为了让"协调者声称什么"与"链上说什么"的偏离成为一个可判定的拒绝理由。
type unlockSignPayload struct {
	BurnTxHash  string `json:"burnTxHash"` // hex
	AssetSymbol string `json:"assetSymbol"`
	Amount      int64  `json:"amount"`
	Owner       string `json:"owner"`
}

// withdrawStatusUnlockPending 该提现已被判定为永久失败、且尚未产生付款签名，桥已请求放弃
// 提现（Unlock）。它不是终态：链上的 settled/released 键才是权威，提交成功前按调度周期重试。
var withdrawStatusUnlockPending = []byte("unlock-pending")

// withdrawStatusUnlockSubmitted 本地已把 Unlock 提交到主链（或链上已有终态）。仅用于停止
// 本地重复签名/提交，**不是**终态标记；终态只能由链上键判定。
var withdrawStatusUnlockSubmitted = []byte("unlock-submitted")

// withdrawHasPaymentSignature 报告该 pending 是否已经产生过付款签名/广播。
//
// 判据只用现有状态键：
//   - withdrawStatusSent 在签名并广播成功之后写入（BTC 侧 processWithdrawRequest、RGB20 侧
//     BroadcastTx）；
//   - withdrawStatusConfirmed 在链上结算确认之后写入。
//
// 两者任一存在 ⇒ 签名已经产出：此时绝不解锁（否则签名交易一旦落地就是双付），只能继续重试
// 结算。注意反向不成立（签名成功但广播失败时还没有状态），这是现有状态机已知的窗口；本路径
// 按既定契约以这两个键为准，不引入新的持久化信号。
func withdrawHasPaymentSignature(state []byte) bool {
	return bytes.Equal(state, withdrawStatusSent) || bytes.Equal(state, withdrawStatusConfirmed)
}

// unlockEligible 是"这笔提现可以走 Unlock"的完整判定：失败被明确判为永久（unrecoverable）
// 且尚未产生付款签名。永久判定由调用方从 rgb20.IsUnrecoverableWithdraw 得到——不用超时。
func unlockEligible(unrecoverable bool, state []byte) bool {
	return unrecoverable && !withdrawHasPaymentSignature(state)
}

// handleUnrecoverableRgb20Withdraw 处理"重试永远不可能成功"的 RGB20 提现：在未产生付款签名
// 的前提下改为放弃提现（Unlock），把锁定金额退回发起人。返回"是否继续重试结算"。
//
// 已经产生付款签名时返回 true：按契约，签名已产出只能继续重试结算，绝不解锁。
func (n *neutrinoClient) handleUnrecoverableRgb20Withdraw(p *rtypes.PendingTx, class string, cause error) bool {
	txHash := hex.EncodeToString(p.GetTxHash())
	state := n.getWithdrawState(p.GetTxHash())
	if !unlockEligible(true, state) {
		log.Error("withdrawalProcessor rgb20 withdraw UNRECOVERABLE but payment signature already produced, refuse to unlock",
			"txHash", txHash, "assetSymbol", p.GetAssetSymbol(), "state", string(state),
			"class", class, "err", cause)
		// A signed payment may still land; the only safe path is to keep retrying settlement.
		return true
	}

	// Record the unlock request before doing anything slow, so a restart between here and the
	// submission retries the unlock instead of falling back to the (permanently failing) settle path.
	if err := n.setWithdrawState(p.GetTxHash(), withdrawStatusUnlockPending); err != nil {
		log.Error("withdrawalProcessor setWithdrawState unlock-pending", "txHash", txHash, "err", err)
	}
	log.Error("withdrawalProcessor rgb20 withdraw UNRECOVERABLE, requesting unlock (refund)",
		"txHash", txHash, "assetSymbol", p.GetAssetSymbol(), "class", class, "err", cause)
	if err := n.tryUnlockWithdraw(p); err != nil {
		log.Error("withdrawalProcessor unlock failed, will retry", "txHash", txHash, "err", err)
	}
	return false
}

// buildUnlockAsset 从链上事实重建被签的 UnlockAsset（协调者用）。
//
// 字段来源：burnTxHash 是 chain33 提现交易哈希；assetSymbol 取 Withdraw payload 的**原始**
// 值（链上 Exec_Unlock 逐字比对，pending 里的符号是 formatSymbol 后的结果，不能替代）；
// amount/owner 取锁定时写入的台账口径（Withdraw 金额 + 交易发起人，与 PendingTx 一致）。
func (n *neutrinoClient) buildUnlockAsset(p *rtypes.PendingTx) (*rtypes.UnlockAsset, error) {
	withdraw, err := n.getRgbxWithdrawAsset(p.GetTxHash())
	if err != nil {
		return nil, fmt.Errorf("read withdraw payload: %w", err)
	}
	return newUnlockAsset(p, withdraw)
}

// newUnlockAsset 纯组装与一致性校验（便于单测）。金额/发起人必须与 Withdraw payload 完全一致：
// 任何不一致都说明"这笔 pending 描述的不是这条 Withdraw"，fail-closed。
func newUnlockAsset(p *rtypes.PendingTx, withdraw *rtypes.WithdrawAsset) (*rtypes.UnlockAsset, error) {
	if p == nil || withdraw == nil {
		return nil, fmt.Errorf("nil pending or withdraw payload")
	}
	if len(p.GetTxHash()) == 0 {
		return nil, fmt.Errorf("empty burn tx hash")
	}
	if p.GetActionType() != rtypes.TyWithdrawAsset {
		return nil, fmt.Errorf("pending action type %d is not a withdraw", p.GetActionType())
	}
	if p.GetConfirmed() {
		return nil, fmt.Errorf("withdraw %s already confirmed", hex.EncodeToString(p.GetTxHash()))
	}
	if withdraw.GetAssetSymbol() == "" {
		return nil, fmt.Errorf("withdraw %s has empty asset symbol", hex.EncodeToString(p.GetTxHash()))
	}
	if withdraw.GetAmount() != p.GetAmount() {
		return nil, fmt.Errorf("withdraw %s amount mismatch: payload=%d pending=%d",
			hex.EncodeToString(p.GetTxHash()), withdraw.GetAmount(), p.GetAmount())
	}
	if p.GetFromAddress() == "" {
		return nil, fmt.Errorf("withdraw %s has empty owner", hex.EncodeToString(p.GetTxHash()))
	}
	return &rtypes.UnlockAsset{
		BurnTxHash:  p.GetTxHash(),
		AssetSymbol: withdraw.GetAssetSymbol(),
		Amount:      withdraw.GetAmount(),
		Owner:       p.GetFromAddress(),
	}, nil
}

// tryUnlockWithdraw 走一轮多节点 TSS 组签名，然后把 Unlock 提交到主链。
//
// 签名必须由门限组完成（授权只来自组签名，链上不看提交者是谁）：先广播签名通知让其它签名
// 节点独立核对后入场，本节点再加入同一轮 CGGMP 签名。任一步失败都返回错误，由调度器重试；
// 重复提交被链上以 ErrWithdrawAlreadyFinal 拒绝，说明该 burn 已有终态，本地停止重试。
func (n *neutrinoClient) tryUnlockWithdraw(p *rtypes.PendingTx) error {
	if p == nil || len(p.GetTxHash()) == 0 {
		return fmt.Errorf("unlock: empty burn tx hash")
	}
	if n.tss == nil {
		return fmt.Errorf("unlock: tss service not available")
	}
	if p.GetConfirmed() {
		return fmt.Errorf("unlock: withdraw %s already confirmed", hex.EncodeToString(p.GetTxHash()))
	}

	msg, err := n.buildUnlockAsset(p)
	if err != nil {
		return fmt.Errorf("unlock: build message: %w", err)
	}
	signers := n.tss.waitForSufficientSigners()
	if len(signers) == 0 {
		return fmt.Errorf("unlock: no tss signers available")
	}

	// Same ordering as the deposit/withdraw rounds: publish the notification first so every
	// signer validates the burn independently, then join the local CGGMP round.
	claimed, err := json.Marshal(&unlockSignPayload{
		BurnTxHash:  hex.EncodeToString(msg.GetBurnTxHash()),
		AssetSymbol: msg.GetAssetSymbol(),
		Amount:      msg.GetAmount(),
		Owner:       msg.GetOwner(),
	})
	if err != nil {
		return fmt.Errorf("unlock: marshal payload: %w", err)
	}
	n.tss.pubMsg(tssSignNotifyTopic, types.Encode(&ltypes.TssSignNotify{
		TxType:  transactionTypeRgbxUnlock,
		Payload: claimed,
		Signers: signers,
	}))

	sessionName := unlockSessionName(msg)
	res := n.tss.signMsg(rtypes.UnlockSignDigest(msg), sessionName, signers)
	if res.err != nil {
		return fmt.Errorf("unlock: tss sign: %w", res.err)
	}
	msg.UnlockSig = res.sig

	if _, err := n.submitMainChainTx(rtypes.RgbxX, rtypes.NameUnlockAssetAction, msg); err != nil {
		if !isWithdrawAlreadyFinalError(err) {
			return fmt.Errorf("unlock: submit: %w", err)
		}
		// The chain already holds a terminal state (settled or released). Chain state is the
		// authority; re-signing the same message every cycle cannot change it.
		log.Info("unlock: chain already holds a terminal state for this withdraw, stop resubmitting",
			"txHash", hex.EncodeToString(msg.GetBurnTxHash()))
	}

	// Deliberately not a terminal marker: settled/released on chain is the only terminal truth.
	// This only stops the local scheduler from signing and submitting the same unlock again.
	if err := n.setWithdrawState(p.GetTxHash(), withdrawStatusUnlockSubmitted); err != nil {
		log.Error("unlock: setWithdrawState unlock-submitted", "txHash", hex.EncodeToString(p.GetTxHash()), "err", err)
	}
	return nil
}

// unlockSessionName CGGMP 会话名：必须由**被签内容**派生，协调者与签名节点各自算出的名字要一致。
// 用完整摘要而不是随机值，重试同一笔时会复用同一个名字（与 PSBT 路径同一口径）。
func unlockSessionName(msg *rtypes.UnlockAsset) string {
	return "rgbx-unlock-" + hex.EncodeToString(rtypes.UnlockSignDigest(msg))
}

// isWithdrawAlreadyFinalError 链上是否已有该 burn 的终态。错误经 gRPC 回包只保留文本，因此按
// 执行器的固定措辞识别（executor.ErrWithdrawAlreadyFinal = "withdraw already final"）。
func isWithdrawAlreadyFinalError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "withdraw already final")
}

// handleUnlockSign 签名节点处理 unlock 轮次：按链上事实重建消息、逐项核对协调者的声称值，
// 通过后才签 rtypes.UnlockSignDigest（与链上验签共用同一个摘要函数）。
func (t *tssService) handleUnlockSign(notify *ltypes.TssSignNotify) error {
	if notify == nil || len(notify.Payload) == 0 {
		return fmt.Errorf("unlock sign notify without payload")
	}
	claim := &unlockSignPayload{}
	if err := json.Unmarshal(notify.Payload, claim); err != nil {
		return fmt.Errorf("decode unlock payload: %w", err)
	}
	burnHash, err := hex.DecodeString(claim.BurnTxHash)
	if err != nil || len(burnHash) == 0 {
		return fmt.Errorf("invalid unlock burn tx hash %q", claim.BurnTxHash)
	}
	if t.client == nil {
		return fmt.Errorf("no chain33 bridge")
	}

	// Truth from the chain, not from the coordinator's claim.
	pending, err := t.client.getRgbxPendingTxByHash(burnHash)
	if err != nil {
		return fmt.Errorf("read pending for unlock: %w", err)
	}
	withdraw, err := t.client.getRgbxWithdrawAsset(burnHash)
	if err != nil {
		return fmt.Errorf("read withdraw payload for unlock: %w", err)
	}
	if err := verifyUnlockClaim(claim, pending, withdraw); err != nil {
		return err
	}

	// Sign the locally derived message, never the claimed one.
	msg := &rtypes.UnlockAsset{
		BurnTxHash:  pending.GetTxHash(),
		AssetSymbol: withdraw.GetAssetSymbol(),
		Amount:      pending.GetAmount(),
		Owner:       pending.GetFromAddress(),
	}
	res := t.signMsg(rtypes.UnlockSignDigest(msg), unlockSessionName(msg), notify.Signers)
	if res.err != nil {
		return res.err
	}
	log.Debug("handleUnlockSign signed", "burnTxHash", claim.BurnTxHash, "signers", len(notify.Signers))
	return nil
}

// verifyUnlockClaim 签名节点侧的逐项核对（纯逻辑，便于单测）。
func verifyUnlockClaim(claim *unlockSignPayload, pending *rtypes.PendingTx, withdraw *rtypes.WithdrawAsset) error {
	if claim == nil || pending == nil || withdraw == nil {
		return fmt.Errorf("incomplete unlock claim")
	}
	if pending.GetConfirmed() {
		return fmt.Errorf("unlock sign refused: withdraw %s already confirmed", claim.BurnTxHash)
	}
	if pending.GetActionType() != rtypes.TyWithdrawAsset {
		return fmt.Errorf("unlock sign refused: pending action type %d is not a withdraw", pending.GetActionType())
	}
	if claim.BurnTxHash != hex.EncodeToString(pending.GetTxHash()) {
		return fmt.Errorf("unlock sign refused: burn tx hash mismatch")
	}
	if claim.AssetSymbol != withdraw.GetAssetSymbol() {
		return fmt.Errorf("unlock sign refused: symbol mismatch: claimed=%q onchain=%q",
			claim.AssetSymbol, withdraw.GetAssetSymbol())
	}
	if claim.Amount != withdraw.GetAmount() || claim.Amount != pending.GetAmount() {
		return fmt.Errorf("unlock sign refused: amount mismatch: claimed=%d withdraw=%d pending=%d",
			claim.Amount, withdraw.GetAmount(), pending.GetAmount())
	}
	if claim.Owner != pending.GetFromAddress() {
		return fmt.Errorf("unlock sign refused: owner mismatch: claimed=%q onchain=%q",
			claim.Owner, pending.GetFromAddress())
	}
	return nil
}
