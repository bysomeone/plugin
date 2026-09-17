package rgb20

import (
	"bytes"
	"testing"

	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

/*
 * C4：签名节点对**扫集** PSBT 的交叉核对。
 *
 * 扫集没有 chain33 上下文（没有 burn、没有 pending），所以它能核对的不是"这笔提现对不对"，而是
 * "这笔交易有没有把桥的钱挪出桥"：
 *   - 每个输入都是**已登记**的用户 P2WSH 充值脚本，且带自己的 witnessScript（否则签不出）；
 *   - 每个输出都回到主池 —— 不变式（规格 §2.3(b)）：桥的自有付款不得落到用户 P2WSH，
 *     否则那笔付款会被该用户回头当充值证明再认领一次；
 *   - 手续费有上界（唯一允许离开桥控制的东西）。
 *
 * 与前一组用例（提现）最关键的差别是**主池输入不合法**：扫集只花用户充值脚本的 UTXO，
 * 接受主池输入等于凭空多出一个"谁都能发起的、花主池 UTXO 的交易"形态。
 */

// sweepDepositScripts 用同一个群公钥派生 two 组用户充值脚本（真实形态：一把群钥服务 N 个用户）。
func sweepDepositScripts(t *testing.T) (pkA, wsA, pkB, wsB []byte) {
	t.Helper()
	_, tssPub, wsA, pkA := loadP2WSHVector(t)
	// 第二个 userID：同一个群公钥的另一个用户，program 必然不同。
	userB := "1KSBd17H7ZK8iT37aJztFB22XGwsPTdwE4"
	var err error
	wsB, err = rtypes.DeriveDepositWitnessScript(userB, tssPub)
	require.NoError(t, err)
	pkB, err = rtypes.DeriveDepositPkScript(userB, tssPub)
	require.NoError(t, err)
	require.False(t, bytes.Equal(pkA, pkB), "不同 userID 必须派生出不同的 program")
	return pkA, wsA, pkB, wsB
}

// sweepInput 扫集 PSBT 的一个输入（prevout 脚本 + 面额 + 可选的 witnessScript）。
type sweepInput struct {
	outpoint      wire.OutPoint
	pkScript      []byte
	value         int64
	witnessScript []byte
}

// registeredScript 一个"已登记的用户充值脚本"（pkScript → userID）。
type registeredScript struct {
	pkScript []byte
	userID   string
}

// buildSweepPSBT 构造一笔真机形态的扫集 PSBT：n 个用户充值脚本输入（各自带 witnessScript），
// 一个回主池的输出。`outs` 为 nil 时默认付给 TSS 脚本。
func buildSweepPSBT(t *testing.T, ins []sweepInput, outs []*wire.TxOut) []byte {
	t.Helper()
	tss := (&fakeBridge{}).TSSPkScript()
	if outs == nil {
		var total int64
		for _, in := range ins {
			total += in.value
		}
		outs = []*wire.TxOut{wire.NewTxOut(total-1000, tss)}
	}
	tx := wire.NewMsgTx(wire.TxVersion)
	for _, in := range ins {
		tx.AddTxIn(wire.NewTxIn(&in.outpoint, nil, nil))
	}
	for _, out := range outs {
		tx.AddTxOut(out)
	}
	p, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)
	for i, in := range ins {
		p.Inputs[i].WitnessUtxo = &wire.TxOut{Value: in.value, PkScript: in.pkScript}
		if len(in.witnessScript) > 0 {
			p.Inputs[i].WitnessScript = in.witnessScript
		}
	}
	// 走线格式：PSBT_IN_WITNESS_SCRIPT 必须真的编码进去（签名节点/侧车看到的是字节）。
	var buf bytes.Buffer
	require.NoError(t, p.Serialize(&buf))
	return buf.Bytes()
}

// sweepAdapter 一个登记了两个用户充值脚本的适配器。
func sweepAdapter(t *testing.T, registered ...registeredScript) (*Adapter, func()) {
	t.Helper()
	bridge := &fakeBridge{}
	for _, r := range registered {
		bridge.registerDepositScript(r.pkScript, r.userID)
	}
	return newTestAdapter(t, NewMockSidecar(), bridge)
}

// sweepOutpoint 一个确定性的输入 outpoint（扫集交易的输入只要彼此不同即可）。
func sweepOutpoint(t *testing.T, seed string, idx uint32) wire.OutPoint {
	t.Helper()
	op, err := wire.NewOutPointFromString(coverageSealOutpoint)
	require.NoError(t, err)
	op.Hash = chainhash.DoubleHashH([]byte(seed))
	op.Index = idx
	return *op
}

// Test_SweepValidate_SeveralUserScriptsIntoTheMainPool 两个不同用户脚本输入、输出回主池 ⇒ 通过。
// 这正是扫集存在的形态：一笔交易合并多个不同 witnessScript 的输入。
func Test_SweepValidate_SeveralUserScriptsIntoTheMainPool(t *testing.T) {
	userID, _, _, pkA := loadP2WSHVector(t)
	pkA, wsA, pkB, wsB := sweepDepositScripts(t)
	require.Equal(t, "1AmRYcURfDGxBhiaJAvEGdRkvkoM7ztn1u", userID)

	psbtBytes := buildSweepPSBT(t, []sweepInput{
		{outpoint: sweepOutpoint(t, "sweep-a", 0), pkScript: pkA, value: 20_000, witnessScript: wsA},
		{outpoint: sweepOutpoint(t, "sweep-b", 1), pkScript: pkB, value: 30_000, witnessScript: wsB},
	}, nil)

	adapter, cleanup := sweepAdapter(t,
		registeredScript{pkA, userID},
		registeredScript{pkB, "1KSBd17H7ZK8iT37aJztFB22XGwsPTdwE4"},
	)
	defer cleanup()

	require.NoError(t, adapter.ValidateSweepPsbt(&ValidateSweepRequest{Psbt: psbtBytes, FeeRate: 2}),
		"合并多个用户充值脚本、输出回主池的扫集必须被接受")
}

// Test_SweepValidate_OutputToUserScriptRejected **不变式**：桥的自有付款不得落到用户 P2WSH。
// 扫集的输出就是"桥付给自己"，付给任何用户脚本都会被该用户回头当充值证明再认领一次。
func Test_SweepValidate_OutputToUserScriptRejected(t *testing.T) {
	userID, _, _, pkA := loadP2WSHVector(t)
	pkA, wsA, _, _ := sweepDepositScripts(t)

	// 一个"看起来很正常"的扫集，只是输出地址被换成了用户自己的充值脚本。
	psbtBytes := buildSweepPSBT(t, []sweepInput{
		{outpoint: sweepOutpoint(t, "sweep-c", 0), pkScript: pkA, value: 20_000, witnessScript: wsA},
	}, []*wire.TxOut{
		wire.NewTxOut(19_000, pkA),
	})

	adapter, cleanup := sweepAdapter(t, registeredScript{pkA, userID})
	defer cleanup()

	err := adapter.ValidateSweepPsbt(&ValidateSweepRequest{Psbt: psbtBytes, FeeRate: 2})
	require.Error(t, err)
	require.Contains(t, err.Error(), "must never land on a user P2WSH deposit script")
}

// Test_SweepValidate_MainPoolInputRejected 主池输入不是"待归集的充值"：扫集只花用户充值脚本，
// 接受主池输入会把扫集变成"谁都能发起的花主池 UTXO 的交易"。
func Test_SweepValidate_MainPoolInputRejected(t *testing.T) {
	userID, _, _, pkA := loadP2WSHVector(t)
	pkA, wsA, _, _ := sweepDepositScripts(t)
	tss := (&fakeBridge{}).TSSPkScript()

	psbtBytes := buildSweepPSBT(t, []sweepInput{
		{outpoint: sweepOutpoint(t, "sweep-d", 0), pkScript: pkA, value: 20_000, witnessScript: wsA},
		{outpoint: sweepOutpoint(t, "sweep-e", 1), pkScript: tss, value: 50_000},
	}, nil)

	adapter, cleanup := sweepAdapter(t, registeredScript{pkA, userID})
	defer cleanup()

	err := adapter.ValidateSweepPsbt(&ValidateSweepRequest{Psbt: psbtBytes, FeeRate: 2})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a registered user deposit script")
}

// Test_SweepValidate_UnregisteredAndWitnessScriptMissingRejected 两条 fail-closed：
// 未登记的脚本一律不认（登记是签名节点自己的判断，不采信 PSBT 自称），已登记但没带
// witnessScript 也拒（没有它就没有 BIP143 scriptCode，签出来的必然无效）。
func Test_SweepValidate_UnregisteredAndWitnessScriptMissingRejected(t *testing.T) {
	userID, _, _, pkA := loadP2WSHVector(t)
	pkA, wsA, _, _ := sweepDepositScripts(t)
	registered := registeredScript{pkA, userID}

	// (a) 完全没登记。
	unregistered := buildSweepPSBT(t, []sweepInput{
		{outpoint: sweepOutpoint(t, "sweep-f", 0), pkScript: pkA, value: 20_000, witnessScript: wsA},
	}, nil)
	adapter, cleanup := sweepAdapter(t)
	err := adapter.ValidateSweepPsbt(&ValidateSweepRequest{Psbt: unregistered, FeeRate: 2})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a registered user deposit script")
	cleanup()

	// (b) 登记了，但 PSBT 里没有 witnessScript。
	noWS := buildSweepPSBT(t, []sweepInput{
		{outpoint: sweepOutpoint(t, "sweep-g", 0), pkScript: pkA, value: 20_000},
	}, nil)
	adapter2, cleanup2 := sweepAdapter(t, registered)
	defer cleanup2()
	err = adapter2.ValidateSweepPsbt(&ValidateSweepRequest{Psbt: noWS, FeeRate: 2})
	require.Error(t, err)
	require.Contains(t, err.Error(), "carries no witness script")
}

// Test_SweepValidate_FeeCeiling 手续费上界：唯一允许离开桥控制的东西，不能被协调者抽干。
func Test_SweepValidate_FeeCeiling(t *testing.T) {
	userID, _, _, pkA := loadP2WSHVector(t)
	pkA, wsA, _, _ := sweepDepositScripts(t)
	tss := (&fakeBridge{}).TSSPkScript()

	// 输入 1_000_000，只留 1 sat 给主池：其余全成了矿工费。
	psbtBytes := buildSweepPSBT(t, []sweepInput{
		{outpoint: sweepOutpoint(t, "sweep-h", 0), pkScript: pkA, value: 1_000_000, witnessScript: wsA},
	}, []*wire.TxOut{wire.NewTxOut(1, tss)})

	adapter, cleanup := sweepAdapter(t, registeredScript{pkA, userID})
	defer cleanup()

	err := adapter.ValidateSweepPsbt(&ValidateSweepRequest{Psbt: psbtBytes, FeeRate: 2})
	require.Error(t, err)
	require.Contains(t, err.Error(), "sweep fee too high")

	// 反向：同一笔交易，把费率门槛抬到足够高就通过 —— 证明拒的是"费太高"这件事本身，
	// 而不是别的偶发原因（费率门槛是一个上界，越高的费率越松）。
	require.NoError(t, adapter.ValidateSweepPsbt(&ValidateSweepRequest{Psbt: psbtBytes, FeeRate: 100_000}))
}
