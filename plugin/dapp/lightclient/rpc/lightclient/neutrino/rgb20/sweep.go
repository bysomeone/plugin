package rgb20

import (
	"bytes"
	"context"
	"fmt"

	pb "github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20/pb"
	"github.com/btcsuite/btcd/btcutil/psbt"
)

/*
 * 扫集（C4）：把散在用户 P2WSH 充值地址上的 BTC 归集回**主池**，让提现重新有 BTC 可花。
 *
 * 它不改变任何 RGB 账（入账在铸币那一刻已经完成），纯粹是 UTXO 整理 —— 所以它**不需要** chain33
 * 上的任何上下文（没有 burn、没有 pending）。这一点决定了签名节点的核对方式：提现要拿链上
 * pending 当真值逐项比对（E11），而扫集没有真值可对，只能核对"这笔交易有没有把桥的钱挪出桥"——
 * 而那恰好可以由 PSBT 自己判定：
 *
 *   - 每个输入都必须是桥控制的脚本（主池，或**已登记**的用户充值脚本，且带自己的 witnessScript）；
 *   - 每个输出都必须回到主池脚本（不变式：桥的自有付款不得落到用户 P2WSH，规格 §2.3(b)）；
 *   - 手续费在合理区间内（唯一离开桥控制的只有矿工费）。
 *
 * 三条都是**只读 PSBT 就能判定的**，因此签名节点不需要信任协调者的任何声称值。侧车在广播前
 * （FinalizeSweep）会用同一组判据再算一遍 —— 构建方与签名方/广播方可能不是同一个节点。
 */

// SweepRequest 一次扫集请求（协调者侧参数）。
type SweepRequest struct {
	FeeRate          uint32 // sat/vB
	MinUtxos         uint32 // 低于这个数不值得扫（返回 NothingToSweep）
	MinConfirmations uint32 // 只归集达到该确认数的充值 UTXO
}

// SweepResult 一次扫集的结果。
type SweepResult struct {
	// Txid 扫集交易（NothingToSweep 时为空）。
	Txid string
	// InputCount 被归集的输入笔数（0 = 没有值得扫的）。
	InputCount uint32
	InputValue uint64
	Fee        uint64
}

// ValidateSweepRequest 签名节点对一笔扫集签名请求的核对输入。
type ValidateSweepRequest struct {
	Psbt []byte
	// FeeRate 用于费用区间的上界（sat/vB）。0 = 跳过区间检查（仍做"输出不离开桥"的检查）。
	FeeRate uint32
}

// Sweep 编排一次扫集：侧车构造 → TSS 签名 → 侧车复核并定稿 → 广播。
//
// `input_count == 0`（没有值得扫的 UTXO）返回 (nil, nil)：对闲时 ticker 而言"没什么可做"不是错误。
//
// 扫集不写任何本地状态：它不动 RGB 账、不动 chain33，失败重试的代价只是一次 RPC。也正因如此
// 它**不需要**提现那套 sticky/幂等记录（重复广播同一笔会被节点按"已在 mempool"拒绝，
// BroadcastRawTx 把它当成功）。
func (a *Adapter) Sweep(ctx context.Context, req *SweepRequest) (*SweepResult, error) {
	sc := a.sidecar.Load()
	if sc == nil {
		return nil, fmt.Errorf("sidecar not started")
	}
	if req == nil {
		return nil, fmt.Errorf("nil sweep request")
	}
	built, err := sc.BuildSweep(ctx, &pb.BuildSweepRequest{
		FeeRate:          req.FeeRate,
		MinUtxos:         req.MinUtxos,
		MinConfirmations: req.MinConfirmations,
	})
	if err != nil {
		return nil, fmt.Errorf("build sweep: %w", err)
	}
	if built.GetInputCount() == 0 {
		return nil, nil
	}
	if len(built.GetPsbt()) == 0 {
		return nil, fmt.Errorf("sidecar returned input_count=%d but no psbt", built.GetInputCount())
	}
	if a.bridge == nil {
		return nil, fmt.Errorf("no chain33 bridge for sweep signing")
	}
	signed, err := a.bridge.SignSweepPsbt(built.GetPsbt())
	if err != nil {
		return nil, fmt.Errorf("sign sweep: %w", err)
	}
	fin, err := sc.FinalizeSweep(ctx, &pb.FinalizeSweepRequest{PsbtSigned: signed})
	if err != nil {
		return nil, fmt.Errorf("finalize sweep: %w", err)
	}
	if len(fin.GetRawTx()) == 0 {
		return nil, fmt.Errorf("sidecar returned an empty sweep tx")
	}
	if err := a.bridge.BroadcastRawTx(fin.GetRawTx(), fin.GetTxid()); err != nil {
		return nil, fmt.Errorf("broadcast sweep %s: %w", fin.GetTxid(), err)
	}
	return &SweepResult{
		Txid:       fin.GetTxid(),
		InputCount: built.GetInputCount(),
		InputValue: built.GetInputValue(),
		Fee:        built.GetFee(),
	}, nil
}

// ValidateSweepPsbt 签名节点对一笔扫集 PSBT 的**全部判定**（不签名、不落盘、不广播）：
// 见文件头。任一环节失败即拒签。
//
// 与 ValidateWithdrawPsbt 的关键差别：这里没有 chain33 pending 可对，全部判据都来自 PSBT 本身与
// 本节点自己的事实（TSS 脚本、已登记的用户充值脚本）。这不是放松：扫集本身就只允许"钱在桥内部
// 挪动"，能把钱挪出桥的只有矿工费，而矿工费有上界。
func (a *Adapter) ValidateSweepPsbt(req *ValidateSweepRequest) error {
	if req == nil || len(req.Psbt) == 0 {
		return fmt.Errorf("invalid sweep validate request")
	}
	p, err := psbt.NewFromRawBytes(bytes.NewReader(req.Psbt), false)
	if err != nil {
		return fmt.Errorf("decode psbt: %w", err)
	}
	if p.UnsignedTx == nil {
		return fmt.Errorf("psbt missing unsigned tx")
	}
	if len(p.Inputs) == 0 {
		return fmt.Errorf("sweep psbt has no input")
	}
	if len(p.Inputs) != len(p.UnsignedTx.TxIn) {
		return fmt.Errorf("psbt input count mismatch")
	}
	tssScript := a.resolveTssScript()
	if len(tssScript) == 0 {
		return fmt.Errorf("tss script unavailable for sweep cross-check")
	}

	// ---- 输入侧：每一笔都必须是桥控制的脚本 ----
	// 扫集只花用户充值脚本的 UTXO（主池的 UTXO 不是"待归集对象"），所以这里比提现更严：
	// **主池脚本也不接受** —— 接受它等于允许协调者把主池的币混进扫集交易，凭空多出一个
	// "谁都能发起的、花主池 UTXO 的交易"形态。
	var totalInput int64
	for i := range p.Inputs {
		var prevScript []byte
		switch {
		case p.Inputs[i].WitnessUtxo != nil:
			prevScript = p.Inputs[i].WitnessUtxo.PkScript
			totalInput += p.Inputs[i].WitnessUtxo.Value
		case p.Inputs[i].NonWitnessUtxo != nil:
			op := p.UnsignedTx.TxIn[i].PreviousOutPoint
			if int(op.Index) >= len(p.Inputs[i].NonWitnessUtxo.TxOut) {
				return fmt.Errorf("input %d non-witness utxo out of range", i)
			}
			prevScript = p.Inputs[i].NonWitnessUtxo.TxOut[op.Index].PkScript
			totalInput += p.Inputs[i].NonWitnessUtxo.TxOut[op.Index].Value
		default:
			return fmt.Errorf("input %d missing witness/non-witness utxo", i)
		}
		userID, ok := a.resolveUserDepositScript(prevScript)
		if !ok {
			return fmt.Errorf("sweep input %d (%s) is not a registered user deposit script",
				i, p.UnsignedTx.TxIn[i].PreviousOutPoint.String())
		}
		// 原生 P2WSH 的 witnessScript = BIP143 scriptCode，缺了就没有正确的 scriptCode。
		// （sha256(witnessScript) == prevout program 由 Go 的 resolvePsbtInputScriptCode 再核一遍。）
		if len(p.Inputs[i].WitnessScript) == 0 {
			return fmt.Errorf("sweep input %d is a user deposit script (userID %s) but carries no witness script",
				i, userID)
		}
	}

	// ---- 输出侧：**全部**回主池 ----
	// 不变式（规格 §2.3(b)）：桥的任何自有付款都不得落到用户 P2WSH —— 那笔付款会被该用户回头
	// 当成充值证明再认领一次（同一笔 BTC 两边入账）。扫集的输出就是"桥付给自己"，所以这里
	// 要求每一个输出都是主池脚本，没有例外（连 OP_RETURN 都不允许：扫集不带任何 RGB 承诺，
	// 多一个输出只是给别人多一个可乘之机）。
	var totalOutput int64
	for i, out := range p.UnsignedTx.TxOut {
		if !bytes.Equal(out.PkScript, tssScript) {
			return fmt.Errorf("sweep output %d pays %x which is not the main pool script: "+
				"bridge-owned payments must never land on a user P2WSH deposit script", i, out.PkScript)
		}
		totalOutput += out.Value
	}

	// ---- 手续费区间 ----
	// 唯一离开桥控制的就是矿工费，给它一个上界（防协调者把扫集的币几乎全烧成费）。
	if req.FeeRate > 0 {
		fee := totalInput - totalOutput
		expectedFee := (int64(p.UnsignedTx.SerializeSize()) + int64(len(p.UnsignedTx.TxIn))*108) * int64(req.FeeRate)
		if fee < 0 {
			return fmt.Errorf("sweep outputs (%d) exceed inputs (%d)", totalOutput, totalInput)
		}
		if fee > 2*expectedFee+1000 {
			return fmt.Errorf("sweep fee too high: fee=%d cap=%d", fee, 2*expectedFee+1000)
		}
	}
	return nil
}
