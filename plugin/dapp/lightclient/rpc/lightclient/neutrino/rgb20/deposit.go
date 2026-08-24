package rgb20

import (
	"context"
	"fmt"
	"time"

	pb "github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20/pb"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
)

// DepositRequest 充值请求（用户经桥 HTTP API 发起）。
type DepositRequest struct {
	RequestID   string // 桥侧请求 ID
	AssetSymbol string // RGB20 资产符号（如 RGB20_USDT）
	Amount      int64  // 请求金额（资产最小单位）
	Chain33Addr string // 用户 Chain33 地址（铸造目标）
}

// DepositSignPayload rgb20-deposit 签名轮次消息（BL-3/HR-6）。
// 主节点经 TssSignNotify.Payload 下发（JSON 编码）；签名节点独立验证后签
// C = sha256(types.Encode(DepositAsset{threshold_sig:nil}))。
type DepositSignPayload struct {
	Deposit        *rtypes.DepositAsset `json:"deposit"`        // threshold_sig 为空
	Consignment    []byte               `json:"consignment"`    // 侧车结算产生的 consignment
	ReceiveID      string               `json:"receiveId"`      // 归因 receive
	Chain33Addr    string               `json:"chain33Addr"`    // 地址绑定
	SessionID      string               `json:"sessionId"`      // 唯一 session ID（HR-6）
	BtcBlockHeight uint64               `json:"btcBlockHeight"` // 付款交易所在 BTC 高度
	BtcBlockHash   string               `json:"btcBlockHash"`
	BtcTxIndex     uint32               `json:"btcTxIndex"`
}

// SpvProof BTC 交易存在性证明（由 neutrino 主包通过 lightclient 构造）。
type SpvProof struct {
	TxData      []byte
	BlockHash   string
	BlockHeight uint64
	TxIndex     uint32
	MerkleProof [][]byte
}

// CreateReceive 创建充值 receive：调侧车 CreateReceive 并记录 receive_id↔Chain33 请求。
func (a *Adapter) CreateReceive(ctx context.Context, req *DepositRequest) (*ReceiveRecord, error) {
	if a.sidecar.Load() == nil {
		return nil, fmt.Errorf("sidecar not started")
	}
	if req == nil || req.AssetSymbol == "" || req.Amount <= 0 || req.Chain33Addr == "" {
		return nil, fmt.Errorf("invalid deposit request")
	}
	contract, ok := a.reg.Get(req.AssetSymbol)
	if !ok {
		return nil, fmt.Errorf("asset not registered: %s", req.AssetSymbol)
	}
	minConfs := a.cfg.MinConfirmations
	if minConfs == 0 {
		minConfs = 6
	}
	rsp, err := a.sidecar.Load().CreateReceive(ctx, &pb.CreateReceiveRequest{
		AssetSymbol:      contract.sidecarAssetSymbol(),
		Amount:           req.Amount,
		MinConfirmations: minConfs,
	})
	if err != nil {
		return nil, fmt.Errorf("sidecar CreateReceive: %w", err)
	}
	rec := &ReceiveRecord{
		ReceiveID:   rsp.ReceiveId,
		RequestID:   req.RequestID,
		AssetSymbol: req.AssetSymbol,
		Chain33Addr: req.Chain33Addr,
		Amount:      req.Amount,
		Status:      ReceiveStatusCreated,
		Invoice:     rsp.Invoice,
	}
	if err := a.receives.Put(rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// ProvideConsignment 交付 consignment 并触发侧车结算。
func (a *Adapter) ProvideConsignment(ctx context.Context, consignment []byte, receiveID string) (*pb.TransferState, error) {
	if a.sidecar.Load() == nil {
		return nil, fmt.Errorf("sidecar not started")
	}
	if err := a.receives.SetConsignment(receiveID, consignment); err != nil {
		return nil, err
	}
	return a.sidecar.Load().ProvideConsignment(ctx, &pb.ProvideConsignmentRequest{
		Consignment:   consignment,
		ReceiveIdHint: receiveID,
	})
}

// pollTransfers 轮询侧车 ListTransfers，结算 settled 的充值并触发铸造。
func (a *Adapter) pollTransfers() {
	defer a.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			a.pollTransfersOnce()
		}
	}
}

func (a *Adapter) pollTransfersOnce() {
	if a.sidecar.Load() == nil {
		return
	}
	rsp, err := a.sidecar.Load().ListTransfers(a.ctx, &pb.ListTransfersRequest{StatusFilter: "settled"})
	if err != nil {
		return
	}
	for _, t := range rsp.Transfers {
		if err := a.onSettledTransfer(t); err != nil {
			continue
		}
	}
}

// onSettledTransfer 按 receive_id 归因并推进充值流程。
// 已归因但未 minted 的记录会在后续轮询中持续重试 submitDeposit（幂等），
// 保证 TSS/SPV/链上提交瞬时失败后能自动恢复。
func (a *Adapter) onSettledTransfer(t *pb.TransferState) error {
	if t == nil || t.ReceiveId == "" {
		return fmt.Errorf("invalid transfer state")
	}
	rec, err := a.receives.Get(t.ReceiveId)
	if err != nil {
		// 非本桥发起的 receive（侧车自建），跳过
		return nil
	}
	if rec.Status == ReceiveStatusMinted {
		return nil
	}
	if rec.Status != ReceiveStatusSettled {
		// 首次归因：记录付款交易与打开的收款 seal。
		seal := FormatOutpoint(t.Txid, t.Vout)
		if err := a.receives.Settle(t.ReceiveId, t.Txid, t.Vout, seal); err != nil {
			return err
		}
		// seal 索引：新打开的收款 seal 进入 pending-mint（HR-5）
		if err := a.seals.Add(&Seal{
			Outpoint:    seal,
			AssetID:     t.AssetId,
			AssetSymbol: rec.AssetSymbol,
			Amount:      t.Amount,
			Status:      SealStatusPendingMint,
		}); err != nil {
			return err
		}
	}
	// 触发充值提交（SPV + TSS 签 deposit + 提交）。失败返回 error，由 pollTransfersOnce 记录，
	// 下轮轮询继续重试（rec.Status 已是 settled，走上面的重试分支）。
	if a.bridge != nil {
		return a.submitDeposit(rec)
	}
	return nil
}

// submitDeposit 组 DepositAsset（threshold_sig 空）→ 构造签名消息 → TSS 签 → 提交 → seal 置 minted。
func (a *Adapter) submitDeposit(rec *ReceiveRecord) error {
	if rec == nil {
		return fmt.Errorf("nil receive record")
	}
	if len(rec.Consignment) == 0 {
		return fmt.Errorf("consignment not provided for receive %s", rec.ReceiveID)
	}
	proof, err := a.bridge.BuildSpvProof(rec.Txid)
	if err != nil {
		return fmt.Errorf("build spv proof: %w", err)
	}
	dep := &rtypes.DepositAsset{
		Amount:         rec.Amount,
		DepositAddress: rec.Chain33Addr,
		AssetSymbol:    rec.AssetSymbol,
		TxProof: &rtypes.BtcTxProof{
			TxData:      proof.TxData,
			BlockHash:   proof.BlockHash,
			BlockHeight: proof.BlockHeight,
			TxIndex:     proof.TxIndex,
			MerkleProof: proof.MerkleProof,
		},
		// threshold_sig 暂空，签名轮次后填充
	}
	payload := &DepositSignPayload{
		Deposit:        dep,
		Consignment:    rec.Consignment,
		ReceiveID:      rec.ReceiveID,
		Chain33Addr:    rec.Chain33Addr,
		SessionID:      fmt.Sprintf("rgbx-deposit-%s-%d", rec.Txid, time.Now().UnixNano()),
		BtcBlockHeight: proof.BlockHeight,
		BtcBlockHash:   proof.BlockHash,
		BtcTxIndex:     proof.TxIndex,
	}
	sig, err := a.bridge.SignDepositMessage(payload)
	if err != nil {
		return fmt.Errorf("sign deposit message: %w", err)
	}
	dep.ThresholdSig = sig
	if err := a.bridge.SubmitDeposit(dep); err != nil {
		return fmt.Errorf("submit deposit: %w", err)
	}
	if err := a.seals.MarkMinted(rec.Seal); err != nil {
		return err
	}
	return a.receives.UpdateStatus(rec.ReceiveID, ReceiveStatusMinted)
}

// BuildDepositSignMessage 构造 rgb20-deposit 签名消息（主节点侧）。
func (a *Adapter) BuildDepositSignMessage(rec *ReceiveRecord, consignment []byte, blockHeight uint64, blockHash string, txIndex uint32) (*DepositSignPayload, error) {
	if rec == nil {
		return nil, fmt.Errorf("nil receive record")
	}
	dep := &rtypes.DepositAsset{
		Amount:         rec.Amount,
		DepositAddress: rec.Chain33Addr,
		AssetSymbol:    rec.AssetSymbol,
		// threshold_sig 空；TxProof 由主节点通过 SPV 补齐后填入
	}
	return &DepositSignPayload{
		Deposit:        dep,
		Consignment:    consignment,
		ReceiveID:      rec.ReceiveID,
		Chain33Addr:    rec.Chain33Addr,
		SessionID:      fmt.Sprintf("rgbx-deposit-%s-%d", rec.Txid, time.Now().UnixNano()),
		BtcBlockHeight: blockHeight,
		BtcBlockHash:   blockHash,
		BtcTxIndex:     txIndex,
	}, nil
}

// ValidateDepositConsignment 签名节点对 rgb20-deposit 消息做独立校验（BL-3）：
// 去重 + 地址绑定 + 金额匹配 + 侧车 ValidateConsignment + 同步高度门槛。
func (a *Adapter) ValidateDepositConsignment(payload *DepositSignPayload) error {
	if payload == nil || payload.Deposit == nil {
		return fmt.Errorf("invalid rgb20-deposit payload")
	}
	if len(payload.Consignment) == 0 {
		return fmt.Errorf("empty consignment")
	}
	rec, err := a.receives.Get(payload.ReceiveID)
	if err != nil {
		return fmt.Errorf("receive not found: %s", payload.ReceiveID)
	}
	// 去重：本节点已 minted 则拒绝（杜绝重复铸造）
	if rec.Status == ReceiveStatusMinted {
		return fmt.Errorf("receive %s already minted", payload.ReceiveID)
	}
	// 地址绑定：deposit 目标地址必须等于充值请求的 Chain33 地址
	if rec.Chain33Addr != payload.Deposit.DepositAddress {
		return fmt.Errorf("address binding mismatch: rec=%s dep=%s", rec.Chain33Addr, payload.Deposit.DepositAddress)
	}
	// 金额匹配
	if rec.Amount != payload.Deposit.Amount {
		return fmt.Errorf("amount mismatch: rec=%d dep=%d", rec.Amount, payload.Deposit.Amount)
	}
	// SPV 独立验证（对 lightclient 头）：签名节点必须能独立确认付款交易上链。
	if a.bridge == nil {
		return fmt.Errorf("chain33 bridge not set (spv verification unavailable)")
	}
	if err := a.bridge.VerifyDepositSpv(payload.Deposit.TxProof); err != nil {
		return fmt.Errorf("spv verify: %w", err)
	}
	if a.sidecar.Load() == nil {
		return fmt.Errorf("sidecar unavailable")
	}
	v, err := a.sidecar.Load().ValidateConsignment(a.ctx, &pb.ValidateConsignmentRequest{
		Consignment:           payload.Consignment,
		ExpectedAmount:        rec.Amount,
		ExpectedRecipientSeal: []byte(rec.Seal),
	})
	if err != nil {
		return fmt.Errorf("validate consignment: %w", err)
	}
	if !v.Valid {
		return fmt.Errorf("consignment invalid: %s", v.ErrorMessage)
	}
	// 同步高度门槛（HR-3）：侧车索引器高度必须覆盖付款交易所在高度
	if v.SyncedHeight < payload.BtcBlockHeight {
		return fmt.Errorf("sidecar synced height too low: %d < %d", v.SyncedHeight, payload.BtcBlockHeight)
	}
	return nil
}
