package rgb20

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	pb "github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20/pb"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	// withdrawTxidBucket 保存 RGB20 提现交易 txid -> chain33 提现交易哈希 的映射。
	// 提现确认后按 txid 关联到 pending（H4：放弃 OP_RETURN correlation）。
	withdrawTxidBucket = []byte("rgb20-withdraw-txid")
	// withdrawStickySealBucket 保存 chain33 提现哈希 -> 绑定的 sticky seal outpoint。
	withdrawStickySealBucket = []byte("rgb20-withdraw-sticky-seal")
)

// WithdrawRequest RGB20 提现请求（对应 rgbx Withdraw 的 pending tx）。
type WithdrawRequest struct {
	Chain33TxHash    []byte // Chain33 提现交易哈希
	Amount           int64  // 资产金额（最小单位）
	FeeRate          int64  // sat/vB
	RecipientInvoice string // 用户 RGB 钱包 invoice
	AssetSymbol      string // RGB20_USDT
	TxBlockHeight    int64  // Chain33 提现交易高度（同步高度门槛基准）
}

// WithdrawResult 提现结果。
type WithdrawResult struct {
	Psbt          []byte
	Consignment   []byte
	Txid          string
	RecipientSeal string
	ChangeSeal    string
}

// WithdrawSignPayload 是 RGB20 提现签名轮次下发给 TSS 组的提现上下文（**协调者的声称值**）。
//
// 签名节点不能只信这些字段：金额/费率/高度门槛/收款 invoice 都必须与链上 pending（按
// Chain33TxHash 查得的真值）逐项比对，不一致即拒签（见 neutrino.handleRgb20WithdrawSign）。
// 之所以还要带上它们，是为了让"协调者声称什么"与"链上说什么"的偏离本身成为一个可判定的
// 拒绝理由，而不是默默按链上真值签（那会让签名节点对一笔自己没核对过的提现背书）。
type WithdrawSignPayload struct {
	Chain33TxHash    []byte `json:"chain33TxHash"`    // chain33 提现交易哈希
	Amount           int64  `json:"amount"`           // 资产金额（最小单位）
	FeeRate          int64  `json:"feeRate"`          // sat/vB
	TxBlockHeight    int64  `json:"txBlockHeight"`    // chain33 提现交易高度（同步高度门槛基准）
	RecipientInvoice string `json:"recipientInvoice"` // 用户 RGB 钱包 invoice
}

// WithdrawSignRequest RGB20 提现签名请求（协调者 → TSS 组）：
// 提现上下文 + 待签 PSBT + consignment（签名节点独立校验所需的全部材料）。
type WithdrawSignRequest struct {
	WithdrawSignPayload
	Psbt        []byte
	Consignment []byte
}

// WithdrawSignReject 签名节点拒签回执（经 TSS 通知链回传协调者）。
//
// 只在**确定性拒签**（UnrecoverableWithdrawError）时发出：否则本节点拒签只表现为"组签名
// 超时"，协调者会把该笔当可重试、每秒重试刷屏。Class/Reason 由协调者还原成
// UnrecoverableWithdrawError 后走 retryRgb20Withdraw 的"停止重试"路径。
type WithdrawSignReject struct {
	Chain33TxHash []byte `json:"chain33TxHash"`
	Class         string `json:"class"`
	Reason        string `json:"reason"`
	Rejector      string `json:"rejector"` // 拒签节点的 peer id（协调者据此核对来源）
}

// ValidateWithdrawRequest RGB20 提现交叉核对请求（BL-4/HR-3，签名节点用）。
type ValidateWithdrawRequest struct {
	Psbt                  []byte
	Consignment           []byte
	ExpectedAmount        int64
	ExpectedRecipientSeal string
	ExpectedClosedSeals   []string
	MinSyncedHeight       uint64
	// FeeRate sat/vB：签名节点据此核对提现交易手续费在合理范围（防桥自有费输入被超收）。
	FeeRate int64
	// CheckSpentSeals 选填回调：在**全部交叉核对通过之后**、用同一份已验证事实同步调用，
	// 参数是本笔实际花掉且被 consignment 关闭的 RGB seal 集合（详见 spentSealsOf）。
	// 签名节点用它做 sticky seal 核对（E9-B）；协调者用它取回同一集合去记账。
	// 回调返回错误即整次校验失败（拒签），错误原样上抛。
	//
	// 之所以做成回调而不是让调用方再算一次：sticky 核对必须落在**这一份**已验证的
	// consignment 上，否则两次 ValidateConsignment 之间侧车状态可能变化（TOCTOU）。
	CheckSpentSeals func(spentSeals []string) error
}

// rgb20RecipientDustCap 提现交易中允许离开桥控制（非 TSS 脚本）的输出金额上限（sats）。
// RGB 提现收款输出只承载 dust（侧车硬编码 546 sat）使接收方 UTXO 可花；其它 BTC 必须
// 找零回 TSS。此上限防止"费输入"被用来向任意地址超付（extfiltrate）。
const rgb20RecipientDustCap int64 = 100_000

// UnrecoverableWithdrawError 标记"重试永远不可能成功"的 RGB20 提现失败：链上 pending 指向的
// 状态在侧车已不存在（最典型的是账本被重建后资产被重新发行 → pending 的 invoice 编码的是老
// asset_id，而 asset id 由 genesis seal 派生，重试不可能"变回来"）；或者签名侧 sticky seal
// 与本笔实际花掉的 seal 不一致（见 unrecoverableClassStickySealMismatch）。调用方据此停止
// 重试并落盘状态，而不是每秒重试、无限刷屏。
//
// 注意：这不改变任何资金处置 —— 该 pending 会留在链上（用户已锁仓的资产如何处置属产品决策，
// 桥不做自动退款）。
type UnrecoverableWithdrawError struct {
	// Class 机器可判定的失败分类（如 "asset-contract-mismatch"），用于日志/状态检索。
	Class string
	// Reason 侧车给出的原始错误。
	Reason error
}

func (e *UnrecoverableWithdrawError) Error() string {
	return fmt.Sprintf("unrecoverable rgb20 withdrawal (%s): %v", e.Class, e.Reason)
}

func (e *UnrecoverableWithdrawError) Unwrap() error { return e.Reason }

// IsUnrecoverableWithdraw 判定 err 是否为不可恢复的提现失败，并返回其分类。
func IsUnrecoverableWithdraw(err error) (string, bool) {
	var e *UnrecoverableWithdrawError
	if errors.As(err, &e) {
		return e.Class, true
	}
	return "", false
}

// NewUnrecoverableWithdrawError 按分类名重建不可恢复错误。
//
// 用途是跨进程传回：签名节点的拒签原因经 TSS 通知链回到协调者时只剩 JSON（class + reason
// 文本），类型信息已丢失，协调者据此把它还原成同一套 UnrecoverableWithdrawError，走
// retryRgb20Withdraw 的"停止重试"路径。
func NewUnrecoverableWithdrawError(class string, reason error) *UnrecoverableWithdrawError {
	if class == "" {
		class = "permanent"
	}
	return &UnrecoverableWithdrawError{Class: class, Reason: reason}
}

// 判定"不可恢复"的两条依据（fail-closed，只收窄不放宽）：
//  1. 侧车 gRPC 状态码 FailedPrecondition —— 侧车用它显式标记"重试无法成功"（engine
//     PermanentError，见 rgb-sidecar service.rs）；
//  2. 侧车文本里出现下面这些"目标状态已不存在"的固定说法 —— 兼容尚未带该状态码的侧车
//     （另一份侧车源码 / 未升级的部署），两者语义一致，不会把暂时性失败误判为永久失败。
//
// 刻意不包含 "insufficient ...": 桥的持仓可以随后续充值变得充足，那是可恢复的。
const (
	unrecoverableClassAssetMismatch = "asset-contract-mismatch"
	unrecoverableClassAssetGone     = "asset-not-issued"
	// unrecoverableClassStickySealMismatch 本笔 chain33 burn 绑定的 RGB seal 集合与既有记录
	// 不一致（E9）：要么签名节点算不出本笔花了哪个 seal，要么与"上一次为同一笔 burn 签名时"
	// 绑定的 seal 不同。重试改变不了这个绑定关系（正确做法是维持原 seal 重放同一笔交易），
	// 继续重试只会把同一笔 burn 换成另一组 seal 再付一次。
	unrecoverableClassStickySealMismatch = "sticky-seal-mismatch"

	unrecoverableMarkerContractMismatch = "!= asset contract "
	unrecoverableMarkerAssetNotIssued   = "not issued"
	// unrecoverableMarkerStickySealMismatch 是分类名本身：UnrecoverableWithdrawError.Error()
	// 会把 class 打进文本（"unrecoverable rgb20 withdrawal (sticky-seal-mismatch): ..."），
	// 因此签名节点的拒签原因即使经 TSS 通知链/日志跨进程回来、类型信息已丢失，也能按这段
	// 措辞被认出来（见 classifyWithdrawSignError）。
	unrecoverableMarkerStickySealMismatch = unrecoverableClassStickySealMismatch
)

// NewStickySealMismatchError 构造 sticky-seal-mismatch 分类的不可恢复错误。
// 签名节点据此把"拒签原因"回传给协调者（经 TSS 通知链），协调者按 class 归类、停止重试。
func NewStickySealMismatchError(reason error) *UnrecoverableWithdrawError {
	return &UnrecoverableWithdrawError{Class: unrecoverableClassStickySealMismatch, Reason: reason}
}

// classifyWithdrawSidecarError 把侧车 BuildWithdrawal 的失败分为"可重试"与"不可恢复"。
func classifyWithdrawSidecarError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case status.Code(err) == codes.FailedPrecondition:
		// 侧车已显式标记；按下述文本进一步定类（纯标记性，class 缺失时用通用分类）。
		if strings.Contains(msg, unrecoverableMarkerContractMismatch) {
			return &UnrecoverableWithdrawError{Class: unrecoverableClassAssetMismatch, Reason: err}
		}
		if strings.Contains(msg, unrecoverableMarkerAssetNotIssued) {
			return &UnrecoverableWithdrawError{Class: unrecoverableClassAssetGone, Reason: err}
		}
		return &UnrecoverableWithdrawError{Class: "permanent", Reason: err}
	case strings.Contains(msg, unrecoverableMarkerContractMismatch):
		return &UnrecoverableWithdrawError{Class: unrecoverableClassAssetMismatch, Reason: err}
	case strings.Contains(msg, unrecoverableMarkerAssetNotIssued):
		return &UnrecoverableWithdrawError{Class: unrecoverableClassAssetGone, Reason: err}
	}
	return err
}

// classifyWithdrawSignError 把 TSS 签名失败按同样的口径分类：
//   - 已经是 UnrecoverableWithdrawError（本地签名节点自己的拒绝，或协调者已还原过）→ 原样返回；
//   - 否则按固定措辞识别（错误经 TSS 通知链/日志跨进程回来时只剩文本）。
//
// 与侧车错误的分类刻意分开：签名失败里绝大多数是超时等暂时性原因，不能误判为永久失败。
func classifyWithdrawSignError(err error) error {
	if err == nil {
		return nil
	}
	var u *UnrecoverableWithdrawError
	if errors.As(err, &u) {
		return err
	}
	if strings.Contains(err.Error(), unrecoverableMarkerStickySealMismatch) {
		return &UnrecoverableWithdrawError{Class: unrecoverableClassStickySealMismatch, Reason: err}
	}
	return err
}

// resolveChangeAddress 返回 RGB20 提现找零地址：优先 config.changeAddress，留空则用桥
// TSS P2WPKH 地址自动填充（config.go 注释承诺的语义；DKG 完成后 TSS 地址才可用）。
func (a *Adapter) resolveChangeAddress() string {
	if a.cfg.ChangeAddress != "" {
		return a.cfg.ChangeAddress
	}
	if a.bridge != nil {
		return a.bridge.TSSAddress()
	}
	return ""
}

// resolveTssScript 返回桥 TSS P2WPKH pkScript（交叉核对输入/输出归属用）。取桥接口的
// TSSPkScript；config.changeAddress 为空时与 resolveChangeAddress 同源（同一 TSS 公钥）。
func (a *Adapter) resolveTssScript() []byte {
	if a.bridge != nil {
		if s := a.bridge.TSSPkScript(); len(s) > 0 {
			return s
		}
	}
	return nil
}

// Withdraw 提现编排：invoice → 侧车 BuildWithdrawal(PSBT) → 交叉核对 → TSS 签 → Finalize → txid↔pending → 广播。
// 全程持 withdrawMu 串行，避免并发花同一 seal。
func (a *Adapter) Withdraw(ctx context.Context, req *WithdrawRequest) (*WithdrawResult, error) {
	a.withdrawMu.Lock()
	defer a.withdrawMu.Unlock()

	if a.sidecar.Load() == nil {
		return nil, fmt.Errorf("sidecar not started")
	}
	if req == nil || req.Amount <= 0 || req.RecipientInvoice == "" {
		return nil, fmt.Errorf("invalid withdraw request")
	}
	contract, ok := a.reg.Get(req.AssetSymbol)
	if !ok {
		return nil, fmt.Errorf("asset not registered: %s", req.AssetSymbol)
	}

	// 选 seal（仅 minted）：侧车 BuildWithdrawal 内部按资产余额选择关闭的 seal；
	// Go 侧通过 ValidateWithdrawPsbt 交叉核对 closed_seals 里没有 pending-mint。
	// 找零地址：config.changeAddress 留空则由桥 TSS 地址自动填充（durable 修复）。
	changeAddr := a.resolveChangeAddress()
	if changeAddr == "" {
		return nil, fmt.Errorf("change address not configured (set rgb20.changeAddress or wait for TSS address)")
	}
	// 与 BTC 侧同构的状态门（bitcoin.go 的 withdrawReqChan 分支：state 非空且 stickyUTXO 为空
	// 则跳过重试）：状态非空 = 本笔已经走到过广播（见 neutrino.BroadcastTx 落盘 withdrawStatusSent），
	// 而 sticky 记录为空 = 我们已经无法重建"上一次到底花了哪组 seal"（记录丢失/存储被清）——此时
	// 按当前账本重新选 seal 构建正是 E9 的双付形态（同一笔 burn 付两次，链上只结算一次），
	// 只能停下（fail-closed，且判为不可恢复：重试不会让那条记录回来）。
	if a.bridge != nil {
		if state := a.bridge.WithdrawState(req.Chain33TxHash); len(state) > 0 && a.GetStickySeal(req.Chain33TxHash) == "" {
			return nil, NewStickySealMismatchError(fmt.Errorf(
				"withdraw state %q but no sticky seal recorded", string(state)))
		}
	}
	// E9-A：已有 sticky 记录时把它作为 input_seals 下发，侧车按这组 seal **重放**上一次的构建
	// （同一份未签 PSBT、同一 txid）。没有它的话，重试会按当前账本（原 seal 已 consumed、找零 seal
	// 已 minted）重新选 seal，产出另一笔独立有效的转移——用户得两份资产、链上只结算一次。
	// 空记录（首次）⇒ 不下发，侧车走常规选 seal 路径。
	rsp, err := a.sidecar.Load().BuildWithdrawal(ctx, &pb.BuildWithdrawalRequest{
		AssetSymbol:      contract.sidecarAssetSymbol(),
		AssetId:          contract.AssetID,
		Amount:           req.Amount,
		RecipientInvoice: req.RecipientInvoice,
		ChangeAddress:    changeAddr,
		FeeRate:          uint32(req.FeeRate),
		InputSeals:       decodeStickySeals(a.GetStickySeal(req.Chain33TxHash)),
	})
	if err != nil {
		// 分类在这一层做（而不是调用方）：只有这里看得到侧车的原始 gRPC 错误码。
		// 可重试的失败原样返回；不可恢复的失败包成 UnrecoverableWithdrawError 供调用方停下。
		return nil, fmt.Errorf("sidecar BuildWithdrawal: %w", classifyWithdrawSidecarError(err))
	}

	// 构造签名节点交叉核对所需参数，先由主节点做一遍相同校验。
	// stickySeals 必须由**同一份已验证事实**推出（PSBT 输入 ∩ consignment 关闭的 seal）：
	// 协调者与签名节点用同一口径，否则同一笔提现在两侧记出不同的值、签名节点会把协调者
	// 自己的合法提现也拒掉（见 CheckSpentSeals 的注释）。
	var stickySeals []string
	valReq := &ValidateWithdrawRequest{
		Psbt:            rsp.Psbt,
		Consignment:     rsp.Consignment,
		ExpectedAmount:  req.Amount,
		MinSyncedHeight: uint64(req.TxBlockHeight),
		FeeRate:         req.FeeRate,
		CheckSpentSeals: func(spentSeals []string) error {
			stickySeals = spentSeals
			return nil
		},
	}
	if err := a.ValidateWithdrawPsbt(valReq); err != nil {
		return nil, fmt.Errorf("validate withdrawal: %w", err)
	}
	// sticky-seal：持久化该笔提现绑定的 seal（重试时不可改选；已有记录必须一致，绝不覆盖）。
	// 这一层同时是「构建后比对」：stickySeals 由**本份**已验证的 PSBT 与 consignment 推出
	// （PSBT 输入 ∩ closed_seals），与既有记录不一致即判不可恢复——即使侧车没有按 input_seals
	// 重放（旧版侧车/记录缺失导致重选），也绝不会让同一笔 burn 换一组 seal 悄悄付第二次。
	if err := a.persistStickySeal(req.Chain33TxHash, stickySeals); err != nil {
		return nil, err
	}

	// TSS 签名（主节点下发 PSBT+consignment+提现上下文，签名节点独立核对后 signPsbt）。
	if a.bridge == nil {
		return nil, fmt.Errorf("chain33 bridge not set")
	}
	signedPsbt, err := a.bridge.SignPsbt(&WithdrawSignRequest{
		WithdrawSignPayload: WithdrawSignPayload{
			Chain33TxHash:    req.Chain33TxHash,
			Amount:           req.Amount,
			FeeRate:          req.FeeRate,
			TxBlockHeight:    req.TxBlockHeight,
			RecipientInvoice: req.RecipientInvoice,
		},
		Psbt:        rsp.Psbt,
		Consignment: rsp.Consignment,
	})
	if err != nil {
		// 签名失败的错误经 TSS 通知链回来（可能是签名节点的确定性拒签回执），按同一套口径
		// 分类：确定性拒签 ⇒ 不可恢复（停止重试），其余保持可重试。
		return nil, fmt.Errorf("sign psbt: %w", classifyWithdrawSignError(err))
	}

	// Finalize（send_end）。
	fin, err := a.sidecar.Load().FinalizeWithdrawal(ctx, &pb.FinalizeWithdrawalRequest{PsbtSigned: signedPsbt})
	if err != nil {
		return nil, fmt.Errorf("sidecar FinalizeWithdrawal: %w", err)
	}

	// txid ↔ chain33 提现哈希 映射（跨重启，H4）。
	if err := a.putTxidMap(fin.Txid, req.Chain33TxHash); err != nil {
		return nil, err
	}
	// 已知 RGB txid（排除 BTC 提现路径，HR-2）。
	if err := a.putKnownWithdrawTxid(fin.Txid); err != nil {
		return nil, err
	}
	// change seal 进入 pending-mint。
	if fin.ChangeSealOutpoint != "" {
		_ = a.seals.Add(&Seal{
			Outpoint:    fin.ChangeSealOutpoint,
			AssetSymbol: req.AssetSymbol,
			AssetID:     contract.AssetID,
			Status:      SealStatusPendingMint,
		})
	}

	// 广播。
	if err := a.bridge.BroadcastTx(signedPsbt, fin.Txid); err != nil {
		return nil, fmt.Errorf("broadcast: %w", err)
	}

	return &WithdrawResult{
		Psbt:          rsp.Psbt,
		Consignment:   rsp.Consignment,
		Txid:          fin.Txid,
		RecipientSeal: fin.RecipientSealOutpoint,
		ChangeSeal:    fin.ChangeSealOutpoint,
	}, nil
}

// ValidateWithdrawPsbt 签名节点交叉核对（BL-4/HR-3，放宽版）：
//  1. 侧车 ValidateConsignment（确定性、只读）校验 RGB 状态转移 + 金额；
//  2. 所有 PSBT 输入的 prevout 都是 TSS 脚本（受桥控制）：closed RGB seal 之外多出的
//     输入只能是桥自有 BTC 费输入（如 deposit 找零 UTXO），不可能花非 TSS 的币；
//     注：consignment 的 closed_seals 含完整历史（spent seal 的前序转移），故不做
//     closed_seals ⊆ PSBT 输入的严格核对（deposit 校验同样不做该核对）。
//  3. 收款输出是唯一离开桥控制的输出且只承载 dust（其余 BTC 找零回 TSS），手续费在合理
//     范围（防 fee 输入被超收 / 超付 / 抽走桥 BTC）——这一侧核对的正是「桥可支配的 BTC
//     （PSBT 全部输入）≥ 离开桥控制的输出 + 手续费」。
//  4. 可支配额覆盖（S1）：签名节点自己算出「可支配额」= 本笔实际动用的 seal 面额合计，
//     核对它覆盖提现额，且发给用户的资产不超过链上 pending 提现额（防超付）；同步高度 ≥ 门槛。
//     算不出可支配额一律拒绝（fail-closed），不采信侧车预计算出的单点数字（S1/A2）。
func (a *Adapter) ValidateWithdrawPsbt(req *ValidateWithdrawRequest) error {
	if a.sidecar.Load() == nil {
		return fmt.Errorf("sidecar unavailable")
	}
	if req == nil || len(req.Psbt) == 0 || len(req.Consignment) == 0 {
		return fmt.Errorf("invalid validate request")
	}
	p, err := psbt.NewFromRawBytes(bytes.NewReader(req.Psbt), false)
	if err != nil {
		return fmt.Errorf("decode psbt: %w", err)
	}
	if p.UnsignedTx == nil {
		return fmt.Errorf("psbt missing unsigned tx")
	}
	psbtInputs := psbtInputOutpoints(p)
	// outpoint -> 输入下标，并累计各输入 prevout 值（witness_utxo / non_witness_utxo）。
	opIndex := make(map[string]int, len(p.Inputs))
	var totalInput int64
	for i := range p.Inputs {
		if i >= len(p.UnsignedTx.TxIn) {
			return fmt.Errorf("psbt input count mismatch")
		}
		op := p.UnsignedTx.TxIn[i].PreviousOutPoint
		opIndex[op.String()] = i
		switch {
		case p.Inputs[i].WitnessUtxo != nil:
			totalInput += p.Inputs[i].WitnessUtxo.Value
		case p.Inputs[i].NonWitnessUtxo != nil:
			if int(op.Index) >= len(p.Inputs[i].NonWitnessUtxo.TxOut) {
				return fmt.Errorf("input %d non-witness utxo out of range", i)
			}
			totalInput += p.Inputs[i].NonWitnessUtxo.TxOut[op.Index].Value
		default:
			return fmt.Errorf("input %d missing witness/non-witness utxo", i)
		}
	}

	v, err := a.sidecar.Load().ValidateConsignment(a.ctx, &pb.ValidateConsignmentRequest{
		Consignment:           req.Consignment,
		ExpectedAmount:        req.ExpectedAmount,
		ExpectedRecipientSeal: []byte(req.ExpectedRecipientSeal),
		ExpectedClosedSeals:   req.ExpectedClosedSeals,
	})
	if err != nil {
		return fmt.Errorf("validate consignment: %w", err)
	}
	if !v.Valid {
		return fmt.Errorf("consignment invalid: %s", v.ErrorMessage)
	}

	// ---- 输入侧交叉核对 ----
	// 每个输入都必须受桥（TSS）控制：RGB seal 输入与 fee 输入都在 TSS 单脚本钱包下，prevout
	// 脚本 == TSS 脚本即证明其受桥控制（非 TSS 的 UTXO 无法被 GG18 签名）。fee 输入（如
	// deposit 找零）只携带 BTC、不携带 RGB 状态；若官方节点夹带其它 TSS 状态 seal 作 fee
	// 输入，其状态会因未在 consignment 中关闭而丢失——该风险由 build_transfer 只选非 seal
	// UTXO 作费输入 + 签名节点对金额/输出结构的核对兜底（见输出侧），与 deposit 校验一致。
	tssScript := a.resolveTssScript()
	if len(tssScript) == 0 {
		return fmt.Errorf("tss script unavailable for input cross-check")
	}
	for _, key := range psbtInputs {
		i := opIndex[key]
		var prevScript []byte
		if p.Inputs[i].WitnessUtxo != nil {
			prevScript = p.Inputs[i].WitnessUtxo.PkScript
		} else if p.Inputs[i].NonWitnessUtxo != nil {
			op := p.UnsignedTx.TxIn[i].PreviousOutPoint
			prevScript = p.Inputs[i].NonWitnessUtxo.TxOut[op.Index].PkScript
		}
		if !bytes.Equal(prevScript, tssScript) {
			return fmt.Errorf("input %s is not a TSS-controlled utxo", key)
		}
	}
	// 用侧车的 seal 状态双向对齐本地 SealIndex（提升 pending-mint→minted；退休 consumed），
	// 再判定 pending-mint。提现产生的 change seal 在侧车侧由 sync() 在其上链后提升为 minted，
	// 但 Go 侧只在 FinalizeWithdrawal 时把它登记为 pending-mint（见 Withdraw），此后再没有任何
	// 路径提升或退休它；不刷新的话，下一笔提现会因为这里（HR-5）被永久拒绝，即"连续两笔提现"
	// 必失败，且本地会永久残留与侧车分叉的垃圾条目。
	// 侧车的 ListSeals 读的是同一份账本，且本函数前一步的 BuildWithdrawal 已经 sync 过，
	// 因此这里只读、不触发一次昂贵的钱包重扫。
	sealView := a.refreshSealStatuses()

	// 任何 closed seal 若为 pending-mint 则拒绝（HR-5：不能花未确认的充值 seal）
	for _, cs := range v.ClosedSeals {
		if a.seals.IsPendingMint(cs) {
			return fmt.Errorf("closed seal %s is pending-mint", cs)
		}
	}

	// ---- 输出侧防超付（extfiltrate）----
	// RGB 提现只有收款输出离开桥控制（非 TSS 脚本），且只承载 dust；其余 BTC 必须找零回 TSS。
	// 这样 fee 输入（可能很大）的余额只会回流 TSS，桥不会被抽走。
	extIdx := -1
	var extVal int64
	for i, out := range p.UnsignedTx.TxOut {
		if isOpReturnScript(out.PkScript) {
			continue
		}
		if bytes.Equal(out.PkScript, tssScript) {
			continue // 找零回 TSS
		}
		if extIdx >= 0 {
			return fmt.Errorf("more than one non-TSS output (vout %d and %d)", extIdx, i)
		}
		extIdx = i
		extVal = out.Value
	}
	if extIdx < 0 {
		return fmt.Errorf("no recipient (non-TSS) output in psbt")
	}
	if extVal > rgb20RecipientDustCap {
		return fmt.Errorf("non-TSS output value %d exceeds dust cap %d", extVal, rgb20RecipientDustCap)
	}

	// ---- 手续费合理范围（防费输入被超收）----
	// 这里核对的正是覆盖式的 BTC 侧：桥可支配的 BTC（= PSBT 全部输入，且上面已逐笔确认受
	// TSS 控制）≥ 离开桥控制的输出 + 手续费；手续费再给一个上下界，防止费输入被超收
	// （fee < 0 即输出超过输入，等于把桥的 BTC 赔进去）。
	var totalOutput int64
	for _, out := range p.UnsignedTx.TxOut {
		totalOutput += out.Value
	}
	fee := totalInput - totalOutput
	expectedFee := (int64(p.UnsignedTx.SerializeSize()) + int64(len(p.UnsignedTx.TxIn))*108) * req.FeeRate
	if req.FeeRate > 0 && (fee < 0 || fee > 2*expectedFee+1000) {
		return fmt.Errorf("invalid fee: fee=%d cap=%d", fee, 2*expectedFee+1000)
	}

	// ---- 可支配额覆盖（S1/A2）----
	// 签名节点自己求出「可支配额」并核对覆盖，不采信侧车预计算出的单点数字：侧车的
	// ConsignmentValidation.amount 是「历史中第一个 TSS 脚本 opened seal」的面额（充值收据
	// 口径），对提现只会随 bundle 顺序落到「找零 seal」或「更早的充值收据」上——纯 genesis、
	// 提现额 > 持仓一半时它就是找零，比提现额小，合法的提现被
	// "withdraw amount exceeds sealed balance" 误拒（A2）。
	cov, err := a.resolveWithdrawCoverage(p, v, tssScript, sealView)
	if err != nil {
		// 算不出可支配额一律拒绝（fail-closed）：宁可拒绝，也不退化成采信侧车的单个数字。
		return fmt.Errorf("resolve withdraw coverage: %w", err)
	}
	// 防超付：离开桥控制（发给用户）的资产不得超过链上 pending 的提现额。提现的发出额由
	// pending 的 targetAddress(invoice) 决定（sidecar build_transfer 按 invoice 金额发送），
	// 而链上只销毁了 pending 的金额——发出多于销毁额即桥自己亏。
	if cov.Leaving > req.ExpectedAmount {
		return fmt.Errorf("withdraw payout exceeds pending amount: payout=%d pending=%d", cov.Leaving, req.ExpectedAmount)
	}
	// 被花 seal 面额（可支配额）必须覆盖提现额。本桥不收取资产手续费（只有 BTC 矿工费，
	// 见 RGB_USDT_PRODUCT.md），BTC 侧的「桥可支配的 BTC ≥ 离开桥控制的输出 + 手续费」由
	// 上面的手续费检查独立核对。
	if cov.Spendable < req.ExpectedAmount {
		return fmt.Errorf("withdraw amount exceeds sealed balance: sealed=%d expected=%d", cov.Spendable, req.ExpectedAmount)
	}
	// 同步高度门槛（HR-3）
	if v.SyncedHeight < req.MinSyncedHeight {
		return fmt.Errorf("sidecar synced height too low: %d < %d", v.SyncedHeight, req.MinSyncedHeight)
	}
	// 全部交叉核对已通过：把「本笔实际花掉且被 consignment 关闭的 RGB seal 集合」交给调用方
	// （签名节点据此做 sticky 核对 E9-B；协调者据此记账）。用同一份已验证事实，避免 TOCTOU。
	if req.CheckSpentSeals != nil {
		if err := req.CheckSpentSeals(spentSealsOf(psbtInputs, v.GetClosedSeals())); err != nil {
			return err
		}
	}
	return nil
}

// withdrawCoverage 是本笔提现的覆盖视图（S1/A2），全部由签名节点自己求出。
type withdrawCoverage struct {
	// Spendable 可支配额：本笔交易实际动用的 seal 面额合计（资产最小单位）。
	Spendable int64
	// Leaving 离开桥控制的资产：锚定在本笔交易上、输出脚本不是 TSS 脚本的 opened seal 面额。
	Leaving int64
	// Returned 回流桥控制的资产：锚定在本笔交易上、输出脚本是 TSS 脚本的 opened seal 面额
	// （找零 seal / 新的桥侧 seal）。
	Returned int64
	// Anchored 锚定在本笔交易上的 opened seal 数。
	Anchored int
	// LedgerSpent 签名节点自己 seal 账本里可见的、本笔被花 seal 的面额合计（交叉核对用）。
	LedgerSpent int64
}

// resolveWithdrawCoverage 由签名节点自己手里的事实独立求出本笔提现的可支配额：
//
//	PSBT（即将签名的交易字节）        → 锚定 txid、每个 seal 的输出归属（TSS / 离开桥控制）
//	consignment（已通过 RGB 共识校验）→ opened seal 的 outpoint 与面额
//	自己的 seal 账本（ListSeals 视图）→ 被花 seal 的面额（上界交叉核对）
//
// 口径（修正 A2）：可支配额 = 本笔交易实际动用的 seal 面额合计，即锚定在「本笔 PSBT 交易」
// 上的 opened seal 面额之和。RGB 状态转移守恒（输入 = 输出）⇒ 它等于被花 seal 的面额合计。
// 只认锚定在本交易 txid 上的 opened seal，历史 bundle（更早的充值/提现）里的 opened seal
// 一律不计入——那正是「提现 > 持仓一半被误拒」的来源。seal 的归属由 PSBT 自己的输出脚本判定，
// 同样不看侧车的 recipient_seal 字段（它对提现是「历史中第一个 TSS 脚本 opened seal」）。
//
// 任一环节算不出来（consignment 没锚定在本交易上、seal 指向交易外的 vout、指向不可花输出、
// 面额为负）→ 返回错误，由调用方 fail-closed 拒绝。
func (a *Adapter) resolveWithdrawCoverage(p *psbt.Packet, v *pb.ConsignmentValidation, tssScript []byte, sealView map[string]*pb.SealInfo) (*withdrawCoverage, error) {
	if p.UnsignedTx == nil {
		return nil, fmt.Errorf("psbt missing unsigned tx")
	}
	// 本笔提现的锚定交易 = 待签名的这笔交易：侧车 build_transfer 以 PSBT 的 txid 作为状态转移的
	// witness id，收款/找零 seal 都锚在它上面（segwit 签名不改变 txid）。
	anchor := p.UnsignedTx.TxHash().String()
	prefix := anchor + ":"
	cov := &withdrawCoverage{}
	for _, seal := range v.GetOpenedSeals() {
		outpoint := seal.GetOutpoint()
		if !strings.HasPrefix(outpoint, prefix) {
			continue // 历史 bundle 打开的 seal：不属于本笔动用的资产
		}
		amount := seal.GetAmount()
		if amount < 0 {
			return nil, fmt.Errorf("invalid seal amount %d at %s", amount, outpoint)
		}
		vout, err := strconv.ParseUint(strings.TrimPrefix(outpoint, prefix), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid seal outpoint %s", outpoint)
		}
		if vout >= uint64(len(p.UnsignedTx.TxOut)) {
			return nil, fmt.Errorf("seal %s points outside the transaction", outpoint)
		}
		script := p.UnsignedTx.TxOut[vout].PkScript
		switch {
		case isOpReturnScript(script):
			// seal 锚在不可花输出上：资产进不去也出不来，结构非法。
			return nil, fmt.Errorf("seal %s anchored at an unspendable output", outpoint)
		case bytes.Equal(script, tssScript):
			cov.Returned += amount
		default:
			cov.Leaving += amount
		}
		cov.Anchored++
	}
	if cov.Anchored == 0 {
		return nil, fmt.Errorf("consignment opens no seal at this transaction %s", anchor)
	}
	cov.Spendable = cov.Leaving + cov.Returned

	// 交叉核对：用签名节点自己的 seal 账本再算一遍「被花 seal 面额」。只取既被本笔交易花掉
	// （PSBT 输入）、又被 consignment 关闭（closed_seals）的 outpoint——历史关闭的 seal 不是
	// 本笔的输入，本地账本里可能残留的陈旧 outpoint 也不在 closed_seals 里，两者都不会被计入。
	// 账本视图只可能「少算」（genesis seal 未经 Go 侧登记、侧车账本重建后不认识旧 outpoint、
	// ListSeals 读不到时按 fail-closed 降级），所以只核对上界：本地账本报出的面额合计不得超过
	// consignment 体现的转移总量，否则两边视图已分叉，宁可拒绝。
	inputs := make(map[string]struct{}, len(p.Inputs))
	for _, op := range psbtInputOutpoints(p) {
		inputs[op] = struct{}{}
	}
	for _, cs := range v.GetClosedSeals() {
		if _, ok := inputs[cs]; !ok {
			continue
		}
		if info, ok := sealView[cs]; ok && info.GetAmount() > 0 {
			cov.LedgerSpent += info.GetAmount()
		}
	}
	if cov.LedgerSpent > cov.Spendable {
		return nil, fmt.Errorf("seal face mismatch: local seals=%d consignment=%d", cov.LedgerSpent, cov.Spendable)
	}
	return cov, nil
}

// isOpReturnScript 判断 pkScript 是否为不可花的 OP_RETURN 输出。
func isOpReturnScript(script []byte) bool {
	return len(script) > 0 && script[0] == txscript.OP_RETURN
}

// psbtInputOutpoints 提取 PSBT 输入 prevout outpoint 列表。
func psbtInputOutpoints(p *psbt.Packet) []string {
	out := make([]string, 0, len(p.UnsignedTx.TxIn))
	for _, in := range p.UnsignedTx.TxIn {
		out = append(out, in.PreviousOutPoint.String())
	}
	return out
}

// spentSealsOf 求「本笔实际花掉且被 consignment 关闭的 RGB seal 集合」：
// PSBT 输入 ∩ consignment 的 closed_seals。
//
// 口径说明（协调者记账与签名节点核对必须完全一致）：
//   - closed_seals 是侧车按**自己的账本**解析出的、本笔状态转移真正关闭的 seal（含历史
//     bundle 里更早关闭的 seal），故要与本笔 PSBT 的输入取交集才是"本笔花掉的"；
//   - 交集为空意味着这份 consignment 没有把任何 seal 绑到这笔 burn 上——E9 的漏洞形态本身
//     就是"一笔 chain33 burn ↔ 一组 RGB seal"的绑定缺失，因此调用方（CheckStickySeal /
//     persistStickySeal）对空集一律 fail-closed 拒绝，而不是当成"没有记录"放行。
func spentSealsOf(psbtInputs []string, closedSeals []string) []string {
	inPSBT := make(map[string]struct{}, len(psbtInputs))
	for _, op := range psbtInputs {
		inPSBT[op] = struct{}{}
	}
	out := make([]string, 0, len(closedSeals))
	for _, cs := range closedSeals {
		if _, ok := inPSBT[cs]; ok {
			out = append(out, cs)
		}
	}
	return out
}

// encodeStickySeals 把 seal 集合编码成可落盘、可逐字节比较的规范形式：
// 去空、去重、按 outpoint 字典序升序、逗号分隔。用集合而非单个 outpoint 是因为侧车一次
// 提现可能花掉多个 seal（build_transfer 按面额拼凑）。
func encodeStickySeals(seals []string) string {
	seen := make(map[string]struct{}, len(seals))
	uniq := make([]string, 0, len(seals))
	for _, s := range seals {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		uniq = append(uniq, s)
	}
	sort.Strings(uniq)
	return strings.Join(uniq, ",")
}

// decodeStickySeals 是 encodeStickySeals 的逆：把落盘的规范形式还原成 outpoint 列表，
// 作为 BuildWithdrawalRequest.input_seals 下发给侧车（触发重放）。
func decodeStickySeals(recorded string) []string {
	if recorded == "" {
		return nil
	}
	parts := strings.Split(recorded, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// CheckStickySeal 签名节点侧 sticky seal 核对（E9-B）：一笔 chain33 burn 只能绑定一组 RGB seal。
//
//   - 本地无记录（本笔首次签名）⇒ 放行。记录必须由签名成功之后的 SetStickySeal 写入，否则
//     一次失败签名会把该笔提现的合法重试永久锁死；
//   - 有记录 ⇒ 必须与本笔实际花掉的 seal 集合完全一致，否则拒签（sticky-seal-mismatch）。
//
// 空集合一律拒签：本核对的全部意义就是把 burn 绑到 seal 上，放行空集等于本核对不存在。
func (a *Adapter) CheckStickySeal(chain33Hash []byte, spentSeals []string) error {
	if len(chain33Hash) == 0 {
		return NewStickySealMismatchError(fmt.Errorf("empty chain33 withdraw hash"))
	}
	want := encodeStickySeals(spentSeals)
	if want == "" {
		return NewStickySealMismatchError(fmt.Errorf("no rgb seal is spent by this withdrawal"))
	}
	recorded := a.GetStickySeal(chain33Hash)
	if recorded == "" {
		return nil // 首次：放行（见函数注释）
	}
	if recorded != want {
		return NewStickySealMismatchError(fmt.Errorf("sticky seal changed: recorded=%s spending=%s", recorded, want))
	}
	return nil
}

// SetStickySeal 落盘该笔提现绑定的 seal 集合。**只能在签名成功之后调用**（镜像 BTC 侧
// handleSignNotify → setWithdrawStickyUTXO）：签名前写会让一次失败签名锁死合法重试。
//
// 已存在且不一致时拒绝写入并报错：签名已经发生，此时能做的只是不再让记录被冲掉，
// 由调用方记录 ERROR 暴露异常。
func (a *Adapter) SetStickySeal(chain33Hash []byte, spentSeals []string) error {
	want := encodeStickySeals(spentSeals)
	if want == "" {
		return fmt.Errorf("no rgb seal to bind")
	}
	a.stickySealMu.Lock()
	defer a.stickySealMu.Unlock()
	if recorded := a.GetStickySeal(chain33Hash); recorded != "" && recorded != want {
		return NewStickySealMismatchError(fmt.Errorf("refuse to overwrite sticky seal: recorded=%s signed=%s", recorded, want))
	}
	return a.store.Put(withdrawStickySealBucket, chain33Hash, []byte(want))
}

// persistStickySeal 把该笔提现绑定的 RGB seal 集合落盘（键 = chain33 提现哈希）。
//
// **绝不覆盖**：已有记录时要求与本次构建出的集合完全一致，不一致即判为不可恢复失败。
// 这条是 E9 的提交侧防线——`Withdraw()` 每次重试都会重新构建（可能因账本推进选到另一组
// seal），覆盖写会把"上一份已签名/已发出"的绑定抹掉，让同一笔 burn 换一组 seal 再付一次
// （用户得两份资产、链上只结算一次，缺口由桥的 RGB 储备承担）。
func (a *Adapter) persistStickySeal(chain33Hash []byte, spentSeals []string) error {
	if len(chain33Hash) == 0 {
		return fmt.Errorf("empty chain33 withdraw hash")
	}
	want := encodeStickySeals(spentSeals)
	if want == "" {
		// fail-closed：算不出本笔绑定的 seal 就不提现（否则本记录与签名侧的核对都形同虚设）。
		return NewStickySealMismatchError(fmt.Errorf("no rgb seal is spent by this withdrawal"))
	}
	a.stickySealMu.Lock()
	defer a.stickySealMu.Unlock()
	recorded := a.GetStickySeal(chain33Hash)
	switch {
	case recorded == want:
		return nil // 重试构建出同一组 seal：幂等重放，合法
	case recorded != "":
		return NewStickySealMismatchError(fmt.Errorf("sticky seal changed: recorded=%s spending=%s", recorded, want))
	}
	return a.store.Put(withdrawStickySealBucket, chain33Hash, []byte(want))
}

// GetStickySeal 读取该笔提现绑定的 sticky seal。
func (a *Adapter) GetStickySeal(chain33Hash []byte) string {
	val, err := a.store.Get(withdrawStickySealBucket, chain33Hash)
	if err != nil {
		return ""
	}
	return string(val)
}

// putTxidMap 保存 txid -> chain33 提现哈希。
func (a *Adapter) putTxidMap(txid string, chain33Hash []byte) error {
	return a.store.Put(withdrawTxidBucket, []byte(txid), chain33Hash)
}

// GetChain33HashByTxid 按 BTC txid 取 chain33 提现哈希。
func (a *Adapter) GetChain33HashByTxid(txid string) ([]byte, error) {
	return a.store.Get(withdrawTxidBucket, []byte(txid))
}

// putKnownWithdrawTxid 将提现交易 txid 记为已知 RGB txid（排除 BTC 提现路径）。
func (a *Adapter) putKnownWithdrawTxid(txid string) error {
	return a.store.Put(txidBucket, []byte(txid), []byte("withdraw"))
}

// refreshSealStatuses 用侧车的 seal 视图**双向**对齐本地 SealIndex。
//
// 侧车是 seal 生命周期的权威：提现的 change seal 由侧车 sync() 在其锚定交易上链后提升为
// minted，被后一笔提现花掉时由 finalize_withdrawal 置为 consumed。Go 侧只在 FinalizeWithdrawal
// 之后把它登记为 pending-mint，此后没有任何路径提升或退休，因此本地与侧车一旦分叉（账本重建、
// 合约重发、reorg、锚定 tx 一直不上链）就会留下永久垃圾条目，且没有自愈路径。
//
// 判据只认侧车**显式报告**，因为 ListSeals 返回该资产的全部 seal（含 consumed——引擎从不从账本
// 里删除 seal，只改状态）：
//   - 侧车报 minted、本地 pending-mint → MarkMinted（保持既有行为：change seal 上链后可用）；
//   - 侧车报 consumed、本地仍挂 pending-mint/minted → MarkConsumed（退休，终态）；
//   - 侧车报 pending-mint，或**响应里没有这个 outpoint** → 不动本地。后者尤其关键：账本重建
//     （侧车 /data 被清空重建）后侧车会大面积"不认识"陈旧 outpoint，那不是"已消费"，按它清退
//     会把仍在链上背书的 seal 误退休——宁可留着垃圾条目也不清退。
//
// 退休只改状态、不删条目：IsSealOutpoint 对登记过的 outpoint 一律 true，consumed 条目仍留在
// BTC 费池排除名单里（HR-5 的护栏不因退休而削弱）。
// 两侧状态一致时本调用无副作用（提升/退休都幂等），因此可以安全地每次校验都跑。
// 只读侧车、不触发 sync：调用方（BuildWithdrawal）刚刚 sync 过同一份账本。
//
// 顺带把侧车报告的 seal 视图（outpoint → SealInfo）返回给调用方，供 S1 的可支配额交叉核对
// 复用，省掉一次 ListSeals 往返。视图可能不完整（某个资产符号读不到时按上面的 fail-closed
// 降级跳过该符号），因此只可用于「本地报出的面额不超过 consignment 体现的总量」这类上界核对。
func (a *Adapter) refreshSealStatuses() map[string]*pb.SealInfo {
	sc := a.sidecar.Load()
	if sc == nil {
		return nil
	}
	view := make(map[string]*pb.SealInfo)
	for _, symbol := range a.reg.Symbols() {
		contract, ok := a.reg.Get(symbol)
		if !ok {
			continue
		}
		rsp, err := sc.ListSeals(a.ctx, &pb.ListSealsRequest{AssetSymbol: contract.sidecarAssetSymbol()})
		if err != nil {
			// 刻意 fail-closed 降级：侧车读不到时保持本地视图（既不提升也不退休），随后
			// HR-5 的 pending-mint 检查会按本地状态拒绝，而不是拿一份可能过期的状态放行。
			continue
		}
		for _, s := range rsp.GetSeals() {
			outpoint := s.GetOutpoint()
			if outpoint == "" {
				continue
			}
			view[outpoint] = s
			local, ok := a.seals.Get(outpoint)
			if !ok {
				continue // 侧车知道、本地未登记：由充值/提现路径登记，不在此处补建
			}
			switch s.GetStatus() {
			case SealStatusMinted:
				if local.Status == SealStatusPendingMint {
					_ = a.seals.MarkMinted(outpoint)
				}
			case SealStatusConsumed:
				if local.Status != SealStatusConsumed {
					_ = a.seals.MarkConsumed(outpoint)
				}
			}
		}
	}
	return view
}
