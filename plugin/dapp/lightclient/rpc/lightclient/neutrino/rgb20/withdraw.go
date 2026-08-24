package rgb20

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	pb "github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20/pb"
	"github.com/btcsuite/btcd/btcutil/psbt"
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

// ValidateWithdrawRequest RGB20 提现交叉核对请求（BL-4/HR-3，签名节点用）。
type ValidateWithdrawRequest struct {
	Psbt                  []byte
	Consignment           []byte
	ExpectedAmount        int64
	ExpectedRecipientSeal string
	ExpectedClosedSeals   []string
	MinSyncedHeight       uint64
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
	rsp, err := a.sidecar.Load().BuildWithdrawal(ctx, &pb.BuildWithdrawalRequest{
		AssetSymbol:      contract.sidecarAssetSymbol(),
		AssetId:          contract.AssetID,
		Amount:           req.Amount,
		RecipientInvoice: req.RecipientInvoice,
		ChangeAddress:    a.cfg.ChangeAddress,
		FeeRate:          uint32(req.FeeRate),
	})
	if err != nil {
		return nil, fmt.Errorf("sidecar BuildWithdrawal: %w", err)
	}

	// 构造签名节点交叉核对所需参数，先由主节点做一遍相同校验。
	valReq := &ValidateWithdrawRequest{
		Psbt:            rsp.Psbt,
		Consignment:     rsp.Consignment,
		ExpectedAmount:  req.Amount,
		MinSyncedHeight: uint64(req.TxBlockHeight),
	}
	if err := a.ValidateWithdrawPsbt(valReq); err != nil {
		return nil, fmt.Errorf("validate withdrawal: %w", err)
	}
	// sticky-seal：持久化该笔提现绑定的 seal（重试时不可改选）。
	if err := a.persistStickySeal(req.Chain33TxHash, rsp.Psbt); err != nil {
		return nil, err
	}

	// TSS 签名（主节点下发 PSBT+consignment，签名节点独立校验后 signPsbt）。
	if a.bridge == nil {
		return nil, fmt.Errorf("chain33 bridge not set")
	}
	signedPsbt, err := a.bridge.SignPsbt(rsp.Psbt)
	if err != nil {
		return nil, fmt.Errorf("sign psbt: %w", err)
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

// ValidateWithdrawPsbt 签名节点交叉核对（BL-4/HR-3）：
//  1. 侧车 ValidateConsignment 返回 closed_seals / opened_seals / synced_height；
//  2. PSBT 输入 prevout 集合 == closed_seals；
//  3. 收款输出（recipient_seal 的 vout）在 PSBT 输出中；
//  4. 金额匹配；同步高度 ≥ 门槛。
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
	// 交叉核对：PSBT 输入 == closed seals
	if !sameStringSet(psbtInputs, v.ClosedSeals) {
		return fmt.Errorf("psbt inputs do not match closed seals: psbt=%v closed=%v", psbtInputs, v.ClosedSeals)
	}
	// 任何 closed seal 若为 pending-mint 则拒绝（HR-5）
	for _, cs := range v.ClosedSeals {
		if a.seals.IsPendingMint(cs) {
			return fmt.Errorf("closed seal %s is pending-mint", cs)
		}
	}
	// 收款输出 == 打开的收款 seal 的 vout
	if v.RecipientSeal != "" && !psbtHasOutputAt(p, v.RecipientSeal) {
		return fmt.Errorf("recipient seal %s not in psbt outputs", v.RecipientSeal)
	}
	// 金额匹配
	if v.Amount != req.ExpectedAmount {
		return fmt.Errorf("amount mismatch: consignment=%d expected=%d", v.Amount, req.ExpectedAmount)
	}
	// 同步高度门槛（HR-3）
	if v.SyncedHeight < req.MinSyncedHeight {
		return fmt.Errorf("sidecar synced height too low: %d < %d", v.SyncedHeight, req.MinSyncedHeight)
	}
	return nil
}

// psbtInputOutpoints 提取 PSBT 输入 prevout outpoint 列表。
func psbtInputOutpoints(p *psbt.Packet) []string {
	out := make([]string, 0, len(p.UnsignedTx.TxIn))
	for _, in := range p.UnsignedTx.TxIn {
		out = append(out, in.PreviousOutPoint.String())
	}
	return out
}

// psbtHasOutputAt 判断 recipient_seal（"txid:vout"）的 vout 是否在 PSBT 输出范围内。
func psbtHasOutputAt(p *psbt.Packet, recipientSeal string) bool {
	vout, err := parseOutpointVout(recipientSeal)
	if err != nil {
		return false
	}
	return int(vout) < len(p.UnsignedTx.TxOut)
}

func parseOutpointVout(s string) (uint32, error) {
	idx := strings.LastIndex(s, ":")
	if idx < 0 {
		return 0, fmt.Errorf("invalid outpoint: %s", s)
	}
	v, err := strconv.ParseUint(s[idx+1:], 10, 32)
	if err != nil {
		return 0, err
	}
	return uint32(v), nil
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sa := append([]string(nil), a...)
	sb := append([]string(nil), b...)
	sort.Strings(sa)
	sort.Strings(sb)
	for i := range sa {
		if sa[i] != sb[i] {
			return false
		}
	}
	return true
}

// persistStickySeal 从 PSBT 输入提取 seals，持久化绑定到 chain33 提现哈希。
func (a *Adapter) persistStickySeal(chain33Hash []byte, psbtBytes []byte) error {
	p, err := psbt.NewFromRawBytes(bytes.NewReader(psbtBytes), false)
	if err != nil {
		return fmt.Errorf("decode psbt: %w", err)
	}
	inputs := psbtInputOutpoints(p)
	if len(inputs) == 0 {
		return fmt.Errorf("no inputs in psbt")
	}
	// 最后一个输入作为 sticky seal（与 BTC 桥 sticky input 约定一致）。
	seal := inputs[len(inputs)-1]
	return a.store.Put(withdrawStickySealBucket, chain33Hash, []byte(seal))
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
