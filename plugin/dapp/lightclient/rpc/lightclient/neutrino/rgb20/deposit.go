package rgb20

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
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
// C = sha256(types.Encode(DepositAsset{thresholdSig:nil}))。
type DepositSignPayload struct {
	Deposit        *rtypes.DepositAsset `json:"deposit"`        // thresholdSig 为空
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
		log.Error("pollTransfersOnce", "err", "sidecar not connected")
		return
	}
	rsp, err := a.sidecar.Load().ListTransfers(a.ctx, &pb.ListTransfersRequest{StatusFilter: "settled"})
	if err != nil {
		log.Error("pollTransfersOnce ListTransfers", "err", err)
		return
	}
	log.Info("pollTransfersOnce", "settledTransfers", len(rsp.Transfers))
	for _, t := range rsp.Transfers {
		if err := a.onSettledTransfer(t); err != nil {
			// 深度门控 / 签名产物缺失这两类是"已知状态"，submitDeposit 内部已按 txid 限流记过一次，
			// 调用方不再每 30s 刷一条 ERROR。
			if isDepositRetryNote(err) {
				log.Debug("pollTransfersOnce deposit retry deferred", "receiveId", t.ReceiveId, "err", err)
				continue
			}
			log.Error("pollTransfersOnce onSettledTransfer", "receiveId", t.ReceiveId, "err", err)
			continue
		}
	}
}

// onSettledTransfer 按 receive_id 归因并推进充值流程。
// 已归因但未 minted 的记录会在后续轮询中持续重试 submitDeposit：
//   - 签名产物已落盘 → 只重发，不再驱动签名轮次（不再与签名节点的"已签集合"冲突）；
//   - 没有签名产物 → 先做本地深度门控（链上可见深度还不够就不签、不提交）、再签一次并落盘提交。
//
// 于是重试只剩两种：等深度（廉价、无签名）与真失败（网络/主链暂时不可用）。
func (a *Adapter) onSettledTransfer(t *pb.TransferState) error {
	if t == nil || t.ReceiveId == "" {
		return fmt.Errorf("invalid transfer state")
	}
	rec, err := a.receives.Get(t.ReceiveId)
	if err != nil {
		// 非本桥发起的 receive（侧车自建），跳过
		log.Debug("onSettledTransfer receive not found", "receiveId", t.ReceiveId, "err", err)
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
		// Settle 只更新存储，上面取到的 rec 仍是结算前的副本（Txid/Vout/Seal 都还是空值）。
		// 必须重新读取：否则 submitDeposit 会拿着空 txid 去构造 SPV proof，首次归因必然失败
		// （"build spv proof: empty txid"），要等下一轮 30s 轮询拿到 settled 记录才自愈。
		rec, err = a.receives.Get(t.ReceiveId)
		if err != nil {
			return err
		}
	}
	// 触发充值提交（SPV + TSS 签 deposit + 提交；已有落盘签名产物则只重发）。失败返回 error，
	// 由 pollTransfersOnce 记录，下轮轮询继续重试（rec.Status 已是 settled，走上面的重试分支）。
	if a.bridge != nil {
		return a.submitDeposit(rec)
	}
	return nil
}

// submitDeposit 推进一笔充值的铸造，分两条路径（重试也走这里）：
//
//	已有落盘的签名产物 → 只重发（不再驱动签名轮次，不与签名节点的"已签集合"冲突）；
//	没有签名产物       → 校验链上可见深度够不够（够才签）→ 签名 → **落盘** → 提交。
//
// 顺序上的两个关键约定：
//   - **先落盘、再提交**：提交前产物必须已持久化，否则重启/崩溃后会落进"已签集合里有、落盘产物没有"
//     的异常态（见 checkSignedArtifactMissing）；
//   - **深度门控在签名之前**：链上注定拒绝的提交不该消耗一轮 GG18（30s 起）。
//
// 提交失败（含被链上拒绝）不丢产物：下一轮轮询直接重发同一份已签对象。
func (a *Adapter) submitDeposit(rec *ReceiveRecord) error {
	if rec == nil {
		return fmt.Errorf("nil receive record")
	}
	if len(rec.Consignment) == 0 {
		return fmt.Errorf("consignment not provided for receive %s", rec.ReceiveID)
	}
	if a.bridge == nil {
		return fmt.Errorf("chain33 bridge not set")
	}

	// 1) 已有签名产物：只重发（不再驱动签名轮次）。
	art, err := a.depositSigs.Get(rec.Txid)
	if err != nil {
		return fmt.Errorf("load signed deposit: %w", err)
	}
	if art != nil {
		return a.submitSignedDeposit(rec, art)
	}

	// 2) 没有签名产物：异常态检查（本节点已签过这笔交易，产物却不在本地）。
	if err := a.checkSignedArtifactMissing(rec.Txid); err != nil {
		return err
	}

	// 3) 构造 SPV 证明（深度门控要知道付款交易高度 H）。
	proof, err := a.bridge.BuildSpvProof(rec.Txid)
	if err != nil {
		return fmt.Errorf("build spv proof: %w", err)
	}
	// 4) 本地深度门控（签名前的这一次）：链上可见深度不够就等，不签名、不提交。
	// 提交前 submitSignedDeposit 还会再核对一次（最贴近动作的那次），这里拦的是"白跑一轮 GG18"。
	if err := a.checkSubmitDepth(proof.BlockHeight); err != nil {
		return err
	}

	// 5) 签名轮次（一整轮 GG18，代价最高的那一步）。
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
		// thresholdSig 暂空，签名轮次后填充
	}
	sessionID := fmt.Sprintf("rgbx-deposit-%s-%d", rec.Txid, time.Now().UnixNano())
	payload := &DepositSignPayload{
		Deposit:        dep,
		Consignment:    rec.Consignment,
		ReceiveID:      rec.ReceiveID,
		Chain33Addr:    rec.Chain33Addr,
		SessionID:      sessionID,
		BtcBlockHeight: proof.BlockHeight,
		BtcBlockHash:   proof.BlockHash,
		BtcTxIndex:     proof.TxIndex,
	}
	sig, err := a.bridge.SignDepositMessage(payload)
	if err != nil {
		return fmt.Errorf("sign deposit message: %w", err)
	}
	dep.ThresholdSig = sig

	// 6) 落盘（先落盘、再提交）：产物是"重试只重发"的唯一依据，丢一次就要重跑签名轮次，
	// 而重跑会被签名节点的已签集合拒绝 —— 所以落盘失败就不提交，下一轮重试落盘。
	art = &SignedDepositArtifact{
		Txid:      rec.Txid,
		ReceiveID: rec.ReceiveID,
		Height:    proof.BlockHeight,
		SessionID: sessionID,
		Deposit:   dep,
	}
	if err := a.depositSigs.Put(art); err != nil {
		return a.persistArtifactFailed(rec, err)
	}
	return a.submitSignedDeposit(rec, art)
}

// persistArtifactFailed 落盘失败：限流记一条 ERROR 并返回错误（调用方不提交）。
func (a *Adapter) persistArtifactFailed(rec *ReceiveRecord, err error) error {
	if a.depositNotes.allow("persist-sig:" + rec.Txid) {
		log.Error("submitDeposit persist signed deposit failed, withholding submission (the signature is kept in "+
			"memory and the write is retried next round; a restart before it succeeds needs operator intervention)",
			"receiveId", rec.ReceiveID, "txid", rec.Txid, "err", err)
	}
	return fmt.Errorf("persist signed deposit: %w", err)
}

// submitSignedDeposit 提交一份已签产物（首次提交与后续重发共用），成功后推进本地状态。
func (a *Adapter) submitSignedDeposit(rec *ReceiveRecord, art *SignedDepositArtifact) error {
	if art == nil || art.Deposit == nil {
		return fmt.Errorf("invalid signed deposit artifact for receive %s", rec.ReceiveID)
	}
	// 产物必须先持久化：否则一次重启就落到"已签集合有、产物没有"的异常态。
	if err := a.depositSigs.Put(art); err != nil {
		return a.persistArtifactFailed(rec, err)
	}
	// 重发同样受深度门控：提交前链上可见深度不够就再等一轮（重发本身廉价，但被拒的链上交易会白花手续费）。
	if err := a.checkSubmitDepth(art.Height); err != nil {
		return err
	}
	if err := a.bridge.SubmitDeposit(art.Deposit); err != nil {
		if isDepositAlreadyConsumed(err) {
			// 链上按 btc-txid 去重拒绝（重复证明）：说明这笔付款交易的铸造链上已经认过 —— 多半是上一次
			// 提交其实进了链，而本地没把 minted 记上。按已铸造处理，否则会每 30s 重发一次、永远停在 settled。
			log.Info("submitDeposit: the chain already consumed this deposit proof, treat the receive as minted",
				"receiveId", rec.ReceiveID, "txid", rec.Txid, "err", err)
			return a.markDepositMinted(rec)
		}
		return fmt.Errorf("submit deposit: %w", err)
	}
	return a.markDepositMinted(rec)
}

// depositAlreadyConsumedMarker 链上"这笔付款交易已被用于铸造"的判据：rgbx 执行器按 btc-txid 去重
// （checkDeposit 查 formatDepositUsedTxIDKey，返回 ErrDuplicateDepositProof = "duplicate deposit proof"）。
// 与 neutrino 侧提现重试按 "already confirmed" 子串把重复提交当幂等成功是同一做法（同仓既有约定）。
const depositAlreadyConsumedMarker = "duplicate deposit proof"

func isDepositAlreadyConsumed(err error) bool {
	return err != nil && strings.Contains(err.Error(), depositAlreadyConsumedMarker)
}

// markDepositMinted 铸造成功后的本地状态推进：seal 置 minted、receive 置 minted、清掉落盘产物。
func (a *Adapter) markDepositMinted(rec *ReceiveRecord) error {
	if err := a.seals.MarkMinted(rec.Seal); err != nil {
		return err
	}
	if err := a.receives.UpdateStatus(rec.ReceiveID, ReceiveStatusMinted); err != nil {
		return err
	}
	// 已 minted 的记录不会再被 submitDeposit 碰到，产物留着只会只增不删。
	if err := a.depositSigs.Delete(rec.Txid); err != nil {
		log.Error("markDepositMinted delete signed deposit artifact", "receiveId", rec.ReceiveID,
			"txid", rec.Txid, "err", err)
	}
	return nil
}

// errDepositDepthPending 提交前的本地深度门控未过（不是故障，等链上深度长够）。
var errDepositDepthPending = errors.New("deposit awaiting on-chain btc confirmations")

// errDepositGateUnavailable 门控算不出来（取不到 best / 读不到链上 N）——fail-closed，同样不提交。
var errDepositGateUnavailable = errors.New("deposit depth gate unavailable")

// errSignedArtifactMissing 异常态：签名节点已登记这笔付款交易（本节点已签过），但本地没有签名产物。
var errSignedArtifactMissing = errors.New("signed deposit artifact missing")

// isDepositRetryNote 判断错误是不是"已知的等待/异常状态"（submitDeposit 已自行限流记录，调用方别再刷屏）。
func isDepositRetryNote(err error) bool {
	return errors.Is(err, errDepositDepthPending) ||
		errors.Is(err, errDepositGateUnavailable) ||
		errors.Is(err, errSignedArtifactMissing)
}

// requiredSubmitHeight 本地 depth 门控的阈值：中继本地 best 达到该高度才允许提交一份高度为
// proofHeight 的充值证明。返回 ok=false 表示溢出（调用方按"等"处理）。
//
// 推导（核对过代码事实，与 rgbx 执行器 checkBtcConfirmations 的链上判据严格对齐）：
//
//	链上：canonical tip >= H + N - 1        （N = [exec.sub.rgbx].minBtcConfirmations）
//	链上 tip 的来源：中继提交头链时只提交到 best - B（B = blockConfirmations，
//	  neutrino/bitcoin.go 的 btcConfirmedHeight；中继的充值通知阈值同样基于 best：
//	  btcwallet.go updateTransactionConfirmations 要求 confirmations = best - H + 1 >= B）
//	⇒ best >= H + B + N - 1
//
// 语义：本地 best 的确认数达到 B + N 才提交（等价于提交那一刻链上可见深度 >= N）。
func requiredSubmitHeight(proofHeight, headerConfs, minConfs uint64) (uint64, bool) {
	extra := headerConfs + minConfs
	if extra < headerConfs { // B + N 自身溢出
		return 0, false
	}
	if extra == 0 {
		// B + N 都为 0（都没配置）：链上不做深度校验，本地也不设门槛。
		return proofHeight, true
	}
	if extra-1 > math.MaxUint64-proofHeight { // proofHeight + extra - 1 溢出
		return 0, false
	}
	return proofHeight + extra - 1, true
}

// checkSubmitDepth 本地深度门控（提交前）：链上可见深度还不够时返回 errDepositDepthPending，
// 调用方**不得**继续签名/提交，等下一轮轮询再试（那时 best 通常已经长够，一次提交即成）。
//
// 为什么不能依赖"提交被拒后再重试"：被拒发生在签名轮次之后（TSS 已经白跑一轮 GG18，30s 起），
// 而重试又会撞上签名节点的已签集合 —— 于是变成"每 30s 失败一次、记录永不 minted"（默认 TTL 下永久卡死）。
//
// fail-closed：取不到 best（neutrino 还没同步出 best block）或链上 N 查不到（主链未就绪/旧执行器）时
// 一律按"还不够"处理 —— 算不出深度就不提交，绝不用猜测值凑门控。
func (a *Adapter) checkSubmitDepth(proofHeight uint64) error {
	best, err := a.bridge.BtcBestHeight()
	if err != nil {
		return a.gateUnavailable(proofHeight, "local btc best height unavailable", err)
	}
	minConfs, err := a.bridge.RgbxMinBtcConfirmations()
	if err != nil {
		return a.gateUnavailable(proofHeight, "on-chain rgbx min btc confirmations unavailable", err)
	}
	headerConfs := uint64(a.cfg.HeaderRelayConfirmations)
	required, ok := requiredSubmitHeight(proofHeight, headerConfs, minConfs)
	if !ok {
		return a.gateUnavailable(proofHeight, "overflow computing the required best height",
			fmt.Errorf("headerRelayConfs=%d onchainMinConfs=%d", headerConfs, minConfs))
	}
	if best < required {
		if a.depositNotes.allow(fmt.Sprintf("depth:%d", proofHeight)) {
			log.Info("submitDeposit waiting for the on-chain visible btc confirmations before submitting "+
				"(the header chain lags the local best by blockConfirmations, so submitting now would be rejected by B8)",
				"bestHeight", best, "requiredBestHeight", required, "proofHeight", proofHeight,
				"headerRelayConfs", headerConfs, "onchainMinConfs", minConfs)
		}
		return fmt.Errorf("%w: best=%d required=%d proofHeight=%d headerRelayConfs=%d onchainMinConfs=%d",
			errDepositDepthPending, best, required, proofHeight, headerConfs, minConfs)
	}
	return nil
}

// gateUnavailable 门控输入取不到（fail-closed：不提交），限流记一条 ERROR 后返回。
func (a *Adapter) gateUnavailable(proofHeight uint64, reason string, cause error) error {
	if a.depositNotes.allow(fmt.Sprintf("gate:%d:%s", proofHeight, reason)) {
		log.Error("submitDeposit cannot evaluate the on-chain confirmation depth, withholding submission "+
			"(fail-closed; the on-chain lightclient may be unreachable or too old to serve the "+
			"rgbx min-confirmations query)",
			"proofHeight", proofHeight, "reason", reason, "err", cause)
	}
	return fmt.Errorf("%w: %s (proofHeight=%d): %w", errDepositGateUnavailable, reason, proofHeight, cause)
}

// checkSignedArtifactMissing 异常态检查：签名节点已登记这笔付款交易（= 本节点已为它签过），
// 但本地没有签名产物。
//
// 策略：**不重签，报错并停止推进这条记录**（fail-closed）。理由：
//   - 重签注定失败：签名成功 = 四个节点都签过，其它节点的已签集合会直接拒绝这一轮，本节点每 30s
//     白等一轮 GG18 超时 —— 正是本方案要消灭的行为；
//   - 走到这里说明"本节点产出过签名、但本地没有它的记录"（落盘失败且进程重启、数据目录被清、
//     从旧快照恢复），本地状态已不可信，静默重签只会把不一致掩盖过去；
//   - 有明确的自愈出口：把 signedDepositTTL 配成正数并重启（已签集合按 TTL 过期，过期后这条记录
//     不再算"已签"，重试会正常走签名），或从备份恢复该 bucket（rgb20-deposit-sig）。
func (a *Adapter) checkSignedArtifactMissing(txid string) error {
	if a.signed == nil {
		return nil
	}
	signed, err := a.signed.IsSigned(txid)
	if err != nil {
		// 判不了（链上高度取不到）时按"已签"处理，与已签集合自身的 fail-closed 取向一致。
		return fmt.Errorf("deposit depth gate: signed-set check tx %s: %w", txid, err)
	}
	if !signed {
		return nil
	}
	if a.depositNotes.allow("missing-sig:" + txid) {
		log.Error("submitDeposit: this node already signed this deposit tx but the signed artifact is missing "+
			"locally (lost signature artifact); not re-signing (the other signers' signed set would refuse it). "+
			"Set a positive rgb20.signedDepositTTL and restart to let the signed set expire, or restore the "+
			"rgb20-deposit-sig bucket from a backup",
			"txid", txid)
	}
	return fmt.Errorf("%w: txid=%s", errSignedArtifactMissing, txid)
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
		// thresholdSig 空；TxProof 由主节点通过 SPV 补齐后填入
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
// 去重（本节点已签集合 + 本地 receive 已 minted）+ 地址绑定 + 金额匹配 +
// 侧车 ValidateConsignment + 同步高度门槛。
func (a *Adapter) ValidateDepositConsignment(payload *DepositSignPayload) error {
	if payload == nil || payload.Deposit == nil {
		return fmt.Errorf("invalid rgb20-deposit payload")
	}
	if len(payload.Consignment) == 0 {
		return fmt.Errorf("empty consignment")
	}
	// A3（签名侧去重）：本节点已为这笔付款交易签过则直接拒绝，不进入签名轮次。
	// 原先的去重只有下面的"本地 receive 已 minted 则拒"，而 validator 节点的本地 store 里
	// 通常没有 receive 记录，那条检查对它们形同虚设。txid 严格从 TxProof.TxData 解析（见 signedset.go）。
	if err := a.CheckDepositSigned(payload); err != nil {
		return err
	}
	// 本地 receive 可能存在（官方节点）也可能不存在（validator 节点：receive 只在官方节点
	// 通过 CreateReceive 创建，validator 的本地 store 没有）。validator 节点退化为用
	// payload 携带的数据 + 侧车确定性校验，保证能独立验证 deposit（BL-3）。
	var rec *ReceiveRecord
	if r, err := a.receives.Get(payload.ReceiveID); err == nil {
		rec = r
	} else {
		log.Debug("ValidateDepositConsignment receive not local, using payload data", "receiveId", payload.ReceiveID)
	}
	if rec != nil {
		// 去重：本节点已 minted 则拒绝（杜绝重复铸造）
		if rec.Status == ReceiveStatusMinted {
			return fmt.Errorf("receive %s already minted", payload.ReceiveID)
		}
	}
	// 地址绑定：deposit 目标地址必须等于充值请求的 Chain33 地址
	reqAddr := payload.Chain33Addr
	if rec != nil {
		reqAddr = rec.Chain33Addr
	}
	if reqAddr != payload.Deposit.DepositAddress {
		return fmt.Errorf("address binding mismatch: req=%s dep=%s", reqAddr, payload.Deposit.DepositAddress)
	}
	// 金额匹配
	reqAmount := payload.Deposit.Amount
	if rec != nil {
		reqAmount = rec.Amount
	}
	if reqAmount != payload.Deposit.Amount {
		return fmt.Errorf("amount mismatch: req=%d dep=%d", reqAmount, payload.Deposit.Amount)
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
	var expectedSeal []byte
	if rec != nil {
		expectedSeal = []byte(rec.Seal)
	}
	v, err := a.sidecar.Load().ValidateConsignment(a.ctx, &pb.ValidateConsignmentRequest{
		Consignment:           payload.Consignment,
		ExpectedAmount:        reqAmount,
		ExpectedRecipientSeal: expectedSeal,
	})
	if err != nil {
		return fmt.Errorf("validate consignment: %w", err)
	}
	if !v.Valid {
		return fmt.Errorf("consignment invalid: %s", v.ErrorMessage)
	}
	// 金额匹配（侧车返回的 consignment 金额）
	if v.Amount != payload.Deposit.Amount {
		return fmt.Errorf("consignment amount mismatch: sidecar=%d dep=%d", v.Amount, payload.Deposit.Amount)
	}
	// 同步高度门槛（HR-3）：侧车索引器高度必须覆盖付款交易所在高度
	if v.SyncedHeight < payload.BtcBlockHeight {
		return fmt.Errorf("sidecar synced height too low: %d < %d", v.SyncedHeight, payload.BtcBlockHeight)
	}
	return nil
}
