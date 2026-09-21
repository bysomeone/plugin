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

// errAssetContractMismatch 充值路径上的**合约身份不符**：consignment 结算出来的资产不是配置里该 symbol
// 声明的那个合约。
//
// 攻击形态（任务 #58）：另造一个 ticker 也注册成 "USDT" 的合约，把它的 consignment 当作"一笔 USDT 充值"
// 交付给桥。桥若只看 symbol（金额/收款 seal/付款交易 SPV 都是真的），链上就会按 symbol 铸出**真** USDT。
// 因此每一条充值路径都必须核对合约身份，且核对的是 asset_id（合约 id）而不是 symbol（symbol 可重名）。
//
// 单独一个错误值，是为了让日志/告警一眼能看出是"合约身份不符"，而不是混进通用的校验失败里。
var errAssetContractMismatch = errors.New("deposit asset contract mismatch")

// errAssetIdUnconfigured 合约身份校验的另一种形态：不是"合约不符"，而是**配置缺口** —— 该 symbol 的
// `contracts.assetId` 没配，因此无从比对。单独一个错误值，让运维能分清"配置漏了"与"有人拿假合约来充"
// （两者的处置完全不同）。它同时被 errAssetContractMismatch 包住：对调用方而言都是"这笔充值的合约身份
// 不可信 ⇒ 拒绝"。
var errAssetIdUnconfigured = errors.New("contract assetId not configured")

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
//   - 签名产物已落盘 → 只重发，省掉一轮 CGGMP 签名（签名轮次是整条链路上最贵的一步）；
//   - 没有签名产物 → 先做本地深度门控（链上可见深度还不够就不签、不提交）、再签一次并落盘提交。
//     没有产物是可自愈的常态（首次归因、重启后产物被清等），重签一份逐字节相同的对象没有副作用。
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
		// 合约身份（#58）：侧车报的 asset_id 必须是配置里该 symbol 声明的合约。放在**落账之前**——
		// 不属于本合约的 consignment 连 Settle 都不该做、收款 seal 也不该登记（登记就成了 pending-mint，
		// 下一步就是被铸造）。
		if err := a.verifyAssetContract(rec.AssetSymbol, t.AssetId); err != nil {
			return err
		}
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
//	已有落盘的签名产物 → 只重发（省掉一轮 CGGMP 签名；重发的是"签的是什么就发什么"）；
//	没有签名产物       → 校验链上可见深度够不够（够才签）→ 签名 → **落盘** → 提交。
//
// 顺序上的两个关键约定：
//   - **先落盘、再提交**：提交前产物必须先持久化，否则一次重启就丢掉产物、要重跑一轮签名；
//   - **深度门控在签名之前**：链上注定拒绝的提交不该消耗一轮 CGGMP 签名（30s 起）。
//
// 提交失败（含被链上拒绝）不丢产物：下一轮轮询直接重发同一份已签对象。
// 产物真的丢了（落盘失败后重启、数据目录被清、从旧快照恢复）也能自愈：没有产物就走下面
// 构造 + 签名那条路径，重签一份完全相同的对象再提交 —— 充值重签是逐字节幂等的
// （签的是 C = sha256(Encode(DepositAsset{thresholdSig:nil}))，内容只有金额/地址/符号 + SPV 证明，
// 没有 nonce、时间戳或 UTXO 选择），链上仍按 txid 去重（formatDepositUsedTxIDKey）兜底。
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
	// 合约身份（#58）：本地登记的合约身份不对就到此为止 —— 省掉下面一整轮 CGGMP 签名。
	if err := a.verifyReceiveContract(rec); err != nil {
		return err
	}

	// 1) 已有签名产物：只重发。
	art, err := a.depositSigs.Get(rec.Txid)
	if err != nil {
		return fmt.Errorf("load signed deposit: %w", err)
	}
	if art != nil {
		return a.submitSignedDeposit(rec, art)
	}

	// 2) 构造 SPV 证明（深度门控要知道付款交易高度 H）。
	proof, err := a.bridge.BuildSpvProof(rec.Txid)
	if err != nil {
		return fmt.Errorf("build spv proof: %w", err)
	}
	// 3) 本地深度门控（签名前的这一次）：链上可见深度不够就等，不签名、不提交。
	// 提交前 submitSignedDeposit 还会再核对一次（最贴近动作的那次），这里拦的是"白跑一轮 CGGMP 签名"。
	if err := a.checkSubmitDepth(proof.BlockHeight); err != nil {
		return err
	}

	// 4) 签名轮次（一整轮 CGGMP 签名，代价最高的那一步）。
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

	// 5) 落盘（先落盘、再提交）：产物是"重试只重发"的依据 —— 有它在盘上就不必再跑一轮 CGGMP 签名；
	// 落盘失败就不提交，下一轮重试落盘（真丢了也能重签，见 submitDeposit 的说明）。
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
// 产物仍在内存缓存里，下一轮 Put 会重试落盘；若在落盘成功前进程重启，产物随之丢失，下一轮会
// 重新走一轮签名（重签幂等，只是白花一轮 CGGMP 签名），不会卡死这条记录。
func (a *Adapter) persistArtifactFailed(rec *ReceiveRecord, err error) error {
	if a.depositNotes.allow("persist-sig:" + rec.Txid) {
		log.Error("submitDeposit persist signed deposit failed, withholding submission (the signature is kept in "+
			"memory and the write is retried next round; losing it to a restart only costs one extra signing round)",
			"receiveId", rec.ReceiveID, "txid", rec.Txid, "err", err)
	}
	return fmt.Errorf("persist signed deposit: %w", err)
}

// submitSignedDeposit 提交一份已签产物（首次提交与后续重发共用），成功后推进本地状态。
func (a *Adapter) submitSignedDeposit(rec *ReceiveRecord, art *SignedDepositArtifact) error {
	if art == nil || art.Deposit == nil {
		return fmt.Errorf("invalid signed deposit artifact for receive %s", rec.ReceiveID)
	}
	// 合约身份（#58）：**提交铸造前的最后一关**。任何走到铸造的路径都必经此处 —— 包括"已签产物只重发"
	// 的重试路径（它不做任何侧车调用），所以后续新增/调整代码路径也绕不开这一关。
	if err := a.verifyReceiveContract(rec); err != nil {
		return err
	}
	// 产物先持久化：为的是重启后还能只重发；真丢了也只是重签一轮，不影响正确性。
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

// isDepositRetryNote 判断错误是不是"已知的等待状态"（submitDeposit 已自行限流记录，调用方别再刷屏）。
func isDepositRetryNote(err error) bool {
	return errors.Is(err, errDepositDepthPending) ||
		errors.Is(err, errDepositGateUnavailable)
}

// --- 充值合约身份（#58）：settled/validated 的 asset_id 必须等于配置里该 symbol 声明的合约 ---

// verifyAssetContract 核对一笔充值携带的 asset_id 就是配置里该 symbol 声明的合约（`contracts.assetId`）。
//
// 两侧判据：
//
//	symbol  = 本地 receive 的 AssetSymbol（建 receive 时已按 Registry 校验过，不是攻击者新填的值）
//	assetID = 侧车结算/校验结果里的 asset_id（consignment 的真实合约 id）
//
// 调用点都是"钱要动"的边界：结算落账前（onSettledTransfer）、提交铸造前（submitSignedDeposit）、
// 签名节点签 C 之前（ValidateDepositConsignment）。
//
// **fail-closed**：`contracts.assetId` 没配时不是"跳过校验"，而是拒绝这笔充值（errAssetIdUnconfigured）
// —— "配置缺失 ⇒ 校验自动失效"正是要防的失败模式（那时任何同 ticker 的假合约都能铸出真资产）。
// 代价与恢复：配置缺口期间**所有**充值被拒（提现不受影响），修法就是给配置补上 assetId 再重启；
// 已落盘的签名产物不会丢，补好配置后下一轮轮询直接重发（见 submitDeposit 的"先落盘、再提交"）。
func (a *Adapter) verifyAssetContract(symbol, assetID string) error {
	contract, ok := a.reg.Get(symbol)
	if !ok {
		return fmt.Errorf("%w: asset not registered: %q", errAssetContractMismatch, symbol)
	}
	want := strings.TrimSpace(contract.AssetID)
	got := strings.TrimSpace(assetID)
	if want == "" {
		return fmt.Errorf("%w: %w: symbol=%s declares no assetId, so the consignment's contract identity "+
			"cannot be verified (set rgb20.contracts.assetId to the rgb: contract id; the deposit is "+
			"refused until then — this is a config gap, not a contract mismatch)",
			errAssetContractMismatch, errAssetIdUnconfigured, symbol)
	}
	if got == "" || !strings.EqualFold(got, want) {
		return fmt.Errorf("%w: symbol=%s configuredAssetId=%q sidecarReportedAssetId=%q "+
			"(the consignment belongs to a different contract than the one registered for this symbol)",
			errAssetContractMismatch, symbol, want, got)
	}
	return nil
}

// verifyReceiveContract 用**本地落账的事实**（receive 结算时登记的那枚 seal 的 asset_id）核对合约身份，
// 供提交铸造前调用。已签产物只重发这条重试路径不做任何侧车调用，但同样要过这里。
func (a *Adapter) verifyReceiveContract(rec *ReceiveRecord) error {
	if rec == nil {
		return fmt.Errorf("%w: nil receive record", errAssetContractMismatch)
	}
	seal, ok := a.seals.Get(rec.Seal)
	if !ok {
		return fmt.Errorf("%w: receive %s has no local seal record (seal=%q), cannot verify its contract identity",
			errAssetContractMismatch, rec.ReceiveID, rec.Seal)
	}
	return a.verifyAssetContract(rec.AssetSymbol, seal.AssetID)
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
// 为什么不能依赖"提交被拒后再重试"：被拒发生在签名轮次之后（TSS 已经白跑一轮 CGGMP 签名，30s 起），
// 每 30s 失败一次纯属浪费；在签名之前拦住，重试就只剩"等深度"这一件廉价的事。
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
// 本地 receive 已 minted 去重 + 地址绑定 + 金额匹配 + 侧车 ValidateConsignment + 同步高度门槛。
func (a *Adapter) ValidateDepositConsignment(payload *DepositSignPayload) error {
	if payload == nil || payload.Deposit == nil {
		return fmt.Errorf("invalid rgb20-deposit payload")
	}
	if len(payload.Consignment) == 0 {
		return fmt.Errorf("empty consignment")
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
	// 合约身份（#58）：consignment 必须属于配置里该 symbol 声明的合约。
	//
	// 这一关在**每个签名节点上各自独立执行**：链上按 symbol 铸造资产的前提是那份 DepositAsset 带一枚
	// threshold sig，任一签名节点在这里拒绝就签不出来 —— 所以协调者侧（以及将来新增的任何提交路径）
	// 都绕不过这一关，这是"即使代码路径变化也不被绕过"的那一层。
	identitySymbol := payload.Deposit.AssetSymbol
	if rec != nil {
		identitySymbol = rec.AssetSymbol
	}
	if err := a.verifyAssetContract(identitySymbol, v.AssetId); err != nil {
		return err
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
