package rgb20

import (
	"bytes"
	"context"
	"fmt"

	pb "github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20/pb"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
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
	// FeeRate sat/vB：签名节点据此核对提现交易手续费在合理范围（防桥自有费输入被超收）。
	FeeRate int64
}

// rgb20RecipientDustCap 提现交易中允许离开桥控制（非 TSS 脚本）的输出金额上限（sats）。
// RGB 提现收款输出只承载 dust（侧车硬编码 546 sat）使接收方 UTXO 可花；其它 BTC 必须
// 找零回 TSS。此上限防止"费输入"被用来向任意地址超付（extfiltrate）。
const rgb20RecipientDustCap int64 = 100_000

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
	rsp, err := a.sidecar.Load().BuildWithdrawal(ctx, &pb.BuildWithdrawalRequest{
		AssetSymbol:      contract.sidecarAssetSymbol(),
		AssetId:          contract.AssetID,
		Amount:           req.Amount,
		RecipientInvoice: req.RecipientInvoice,
		ChangeAddress:    changeAddr,
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
		FeeRate:         req.FeeRate,
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
// ValidateWithdrawPsbt 签名节点交叉核对（BL-4/HR-3，放宽版）：
//  1. 侧车 ValidateConsignment（确定性、只读）校验 RGB 状态转移 + 金额；
//  2. 所有 PSBT 输入的 prevout 都是 TSS 脚本（受桥控制）：closed RGB seal 之外多出的
//     输入只能是桥自有 BTC 费输入（如 deposit 找零 UTXO），不可能花非 TSS 的币；
//     注：consignment 的 closed_seals 含完整历史（spent seal 的前序转移），故不做
//     closed_seals ⊆ PSBT 输入的严格核对（deposit 校验同样不做该核对）。
//  3. 收款输出是唯一离开桥控制的输出且只承载 dust（其余 BTC 找零回 TSS），手续费在合理
//     范围（防 fee 输入被超收 / 超付 / 抽走桥 BTC）；
//  4. 被花 seal 面额 ≥ 提现额（防超额提现）；同步高度 ≥ 门槛。
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
	// 用侧车的 seal 状态对齐本地 SealIndex，再判定 pending-mint。
	// 提现产生的 change seal 在侧车侧由 sync() 在其上链后提升为 minted，但 Go 侧只在
	// FinalizeWithdrawal 时把它登记为 pending-mint（见 Withdraw），此后再没有任何路径提升它；
	// 不刷新的话，下一笔提现会因为这里（HR-5）被永久拒绝，即"连续两笔提现"必失败。
	// 侧车的 ListSeals 读的是同一份账本，且本函数前一步的 BuildWithdrawal 已经 sync 过，
	// 因此这里只读、不触发一次昂贵的钱包重扫。
	a.refreshSealStatuses()

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
		if len(out.PkScript) > 0 && out.PkScript[0] == txscript.OP_RETURN {
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
	var totalOutput int64
	for _, out := range p.UnsignedTx.TxOut {
		totalOutput += out.Value
	}
	fee := totalInput - totalOutput
	expectedFee := (int64(p.UnsignedTx.SerializeSize()) + int64(len(p.UnsignedTx.TxIn))*108) * req.FeeRate
	if req.FeeRate > 0 && (fee < 0 || fee > 2*expectedFee+1000) {
		return fmt.Errorf("invalid fee: fee=%d cap=%d", fee, 2*expectedFee+1000)
	}

	// 金额上限：consignment 报的 amount 是"历史中第一个 TSS 脚本 opened seal"（充值收据），
	// 对提现而言 = 本次被花的 seal 面额（或前序 find 到的 TSS 面额），非提现发出额。
	// 提现的精确发出额由链上 pending 的 targetAddress(invoice) 决定（invoice 编码金额，
	// sidecar build_transfer 按 invoice 金额发送），此处校验"被花 seal ≥ 提现额"防止超额提现
	// （相对桥持有）。精确到收款方/金额的交叉核对需要把 invoice 传给签名节点（见报告，
	// 待办设计点）。诚实的顺序提现下：v.Amount(被花 seal) ≥ expected 恒成立。
	if v.Amount < req.ExpectedAmount {
		return fmt.Errorf("withdraw amount exceeds sealed balance: consignment=%d expected=%d", v.Amount, req.ExpectedAmount)
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
	// 取第一个输入作为 sticky seal：RGB seal 输入恒排在 PSBT 输入最前（build_transfer 先列
	// RGB seal、后追加桥自有费输入），最后一个输入可能是纯 BTC 费输入而非 RGB seal。
	seal := inputs[0]
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

// refreshSealStatuses 用侧车的 seal 视图对齐本地 SealIndex（pending-mint -> minted）。
//
// 侧车是 seal 生命周期的权威：提现的 change seal 由侧车 sync() 在其锚定交易上链后提升为
// minted。Go 侧只在 FinalizeWithdrawal 之后把它登记为 pending-mint，此后没有任何路径提升，
// 于是下一笔提现会被 HR-5 的 pending-mint 检查永久拒绝（连续的第二次提现必失败）。
// 两侧状态一致时本调用无副作用（MarkMinted 只提升 pending-mint），因此可以安全地每次校验都跑。
// 只读侧车、不触发 sync：调用方（BuildWithdrawal）刚刚 sync 过同一份账本。
func (a *Adapter) refreshSealStatuses() {
	sc := a.sidecar.Load()
	if sc == nil {
		return
	}
	for _, symbol := range a.reg.Symbols() {
		contract, ok := a.reg.Get(symbol)
		if !ok {
			continue
		}
		rsp, err := sc.ListSeals(a.ctx, &pb.ListSealsRequest{AssetSymbol: contract.sidecarAssetSymbol()})
		if err != nil {
			// 刻意 fail-closed 降级：侧车读不到时保持本地视图（不提升任何 seal），随后
			// HR-5 的 pending-mint 检查会按本地状态拒绝，而不是拿一份可能过期的状态放行。
			continue
		}
		for _, s := range rsp.GetSeals() {
			outpoint := s.GetOutpoint()
			if s.GetStatus() != SealStatusMinted || outpoint == "" {
				continue
			}
			if a.seals.IsPendingMint(outpoint) {
				_ = a.seals.MarkMinted(outpoint)
			}
		}
	}
}
