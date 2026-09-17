package rgb20

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20/pb"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

/*
 * C3：签名节点交叉核对里的**输入脚本归属**。
 *
 * 提现 PSBT 的输入现在有两种合法形态：
 *   - 主池 TSS P2WPKH（桥直接控制）—— 原有判据；
 *   - **已登记**的用户 P2WSH 充值脚本 —— C3 新增。它的 program 同样是 TSS 群钥的脚本
 *     （`<push userID> OP_DROP <push tssPub> OP_CHECKSIG`），只有 GG18 群签名能花，
 *     所以必须被认作"受桥控制"；但判别依据是**登记**（桥发放过地址、在 watch 集里），
 *     不是形态 —— witnessScript 在花费前不可见，形态在链上根本不可判定。
 *
 * 三种输入一律拒绝：未登记的 P2WSH、已登记但 PSBT 没带 witness_script、以及其它任意脚本。
 */

// p2wshVectorsPath C1 的冻结向量（与 neutrino 包的 deposit_p2wsh_test.go 同源）。
// 从这里（neutrino/rgb20/）往上 5 层到 plugin/dapp/，再进 rgbx/types/testdata。
const p2wshVectorsPath = "../../../../../rgbx/types/testdata/p2wsh_deposit_vectors.json"

// loadP2WSHVector 读 C1 冻结向量的第 0 组（主向量）：userID + tssPub + 派生物。
func loadP2WSHVector(t *testing.T) (userID string, tssPub, witnessScript, pkScript []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(p2wshVectorsPath))
	require.NoError(t, err, "读 C1 的冻结向量失败（路径变了就要同步改这里）")
	var doc struct {
		Spec    string `json:"spec"`
		Vectors []struct {
			UserID        string `json:"userID"`
			TssPubKey     string `json:"tssPubKey"`
			WitnessScript string `json:"witnessScript"`
			PkScript      string `json:"pkScript"`
		} `json:"vectors"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.Equal(t, rtypes.P2WSHDepositSpecV1, doc.Spec)
	require.NotEmpty(t, doc.Vectors)
	v := doc.Vectors[0]
	tssPub, err = hex.DecodeString(v.TssPubKey)
	require.NoError(t, err)
	witnessScript, err = hex.DecodeString(v.WitnessScript)
	require.NoError(t, err)
	pkScript, err = hex.DecodeString(v.PkScript)
	require.NoError(t, err)

	// 自校验：向量确实是我们这套派生函数的输出（否则下面的用例测的就不是真脚本）。
	derivedWS, err := rtypes.DeriveDepositWitnessScript(v.UserID, tssPub)
	require.NoError(t, err)
	require.Equal(t, derivedWS, witnessScript)
	require.True(t, rtypes.IsDepositWitnessScriptShape(witnessScript, tssPub),
		"向量里的 witnessScript 必须是本项目的充值脚本形态")
	return v.UserID, tssPub, witnessScript, pkScript
}

// buildWithdrawPSBTWithWitnessScript 构造单输入的提现 PSBT：输入 prevout 脚本/面额/witness_script
// 由参数给出，其余布局与 buildAnchoredWithdrawPSBT 一致（OP_RETURN + 收款 dust + 找零回 TSS）。
func buildWithdrawPSBTWithWitnessScript(t *testing.T, sealOutpoint string, inputScript []byte, inputValue int64,
	witnessScript []byte) []byte {
	t.Helper()
	tss := (&fakeBridge{}).TSSPkScript()
	op, err := wire.NewOutPointFromString(sealOutpoint)
	require.NoError(t, err)
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(wire.NewTxIn(op, nil, nil))
	tx.AddTxOut(wire.NewTxOut(0, []byte{txscript.OP_RETURN, 0x01}))
	tx.AddTxOut(wire.NewTxOut(546, []byte{0x51}))
	tx.AddTxOut(wire.NewTxOut(4000, tss))
	p, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)
	p.Inputs[0].WitnessUtxo = &wire.TxOut{Value: inputValue, PkScript: inputScript}
	if len(witnessScript) > 0 {
		p.Inputs[0].WitnessScript = witnessScript
	}
	// 走一遍线格式：PSBT_IN_WITNESS_SCRIPT 必须真的编码进去（签名节点看到的是字节，不是内存结构）。
	var buf bytes.Buffer
	require.NoError(t, p.Serialize(&buf))
	roundTripped, err := psbt.NewFromRawBytes(bytes.NewReader(buf.Bytes()), false)
	require.NoError(t, err)
	if len(witnessScript) > 0 {
		require.Equal(t, witnessScript, roundTripped.Inputs[0].WitnessScript,
			"witness_script 必须能从 PSBT 字节里读回来")
	}
	var out bytes.Buffer
	require.NoError(t, roundTripped.Serialize(&out))
	return out.Bytes()
}

// p2wshWithdrawAdapter 造一个带指定登记状态的适配器 + 一份能通过其余全部核对的校验请求。
// register=true 时把该充值脚本登记进 fakeBridge 的 watch 集（模拟桥发放过地址）。
func p2wshWithdrawAdapter(t *testing.T, psbtBytes []byte, inputScript []byte,
	userID string, register bool) (*Adapter, func()) {
	t.Helper()
	bridge := &fakeBridge{}
	if register {
		bridge.registerDepositScript(inputScript, userID)
	}
	mock := NewMockSidecar()
	mock.ValidateResp = []*pb.ConsignmentValidation{
		anchoredConsignment(t, psbtBytes, 4000, []string{coverageSealOutpoint},
			anchoredSeal{vout: 1, amount: 1000}, anchoredSeal{vout: 2, amount: 4000}),
	}
	return newTestAdapter(t, mock, bridge)
}

func p2wshWithdrawRequest(psbtBytes []byte) *ValidateWithdrawRequest {
	return &ValidateWithdrawRequest{
		Psbt:            psbtBytes,
		Consignment:     []byte("consignment"),
		ExpectedAmount:  1000,
		MinSyncedHeight: 100,
		FeeRate:         1,
	}
}

// Test_WithdrawValidate_RegisteredUserDepositScriptInput 已登记的用户 P2WSH 充值脚本作为输入
// 必须被接受 —— 这正是"充值进来的 BTC 能被花掉"在签名节点侧的判据。
func Test_WithdrawValidate_RegisteredUserDepositScriptInput(t *testing.T) {
	userID, _, witnessScript, pkScript := loadP2WSHVector(t)
	psbtBytes := buildWithdrawPSBTWithWitnessScript(t, coverageSealOutpoint, pkScript, 5000, witnessScript)

	adapter, cleanup := p2wshWithdrawAdapter(t, psbtBytes, pkScript, userID, true)
	defer cleanup()

	require.NoError(t, adapter.ValidateWithdrawPsbt(p2wshWithdrawRequest(psbtBytes)),
		"已登记的用户 P2WSH 充值脚本输入必须被认作受桥（TSS 群钥）控制")
}

// Test_WithdrawValidate_UnregisteredP2WSHInputRejected 未登记的 P2WSH 输入必须拒：
// 登记与否是签名节点**自己的**判断，不采信协调者下发的 PSBT 自称"这是用户充值脚本"。
func Test_WithdrawValidate_UnregisteredP2WSHInputRejected(t *testing.T) {
	userID, _, witnessScript, pkScript := loadP2WSHVector(t)
	psbtBytes := buildWithdrawPSBTWithWitnessScript(t, coverageSealOutpoint, pkScript, 5000, witnessScript)

	// 不登记（watch 集为空）——即使脚本形态完全正确、witness_script 也带全了。
	adapter, cleanup := p2wshWithdrawAdapter(t, psbtBytes, pkScript, userID, false)
	defer cleanup()

	err := adapter.ValidateWithdrawPsbt(p2wshWithdrawRequest(psbtBytes))
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a TSS-controlled utxo")
}

// Test_WithdrawValidate_RegisteredDepositScriptWithoutWitnessScriptRejected 已登记但 PSBT 没带
// witness_script 必须拒：没有它签名节点就无法（在 BIP143 下）算出该输入的 sighash，
// 放行等于给一个"签不出可用签名"的输入背书。
func Test_WithdrawValidate_RegisteredDepositScriptWithoutWitnessScriptRejected(t *testing.T) {
	userID, _, _, pkScript := loadP2WSHVector(t)
	psbtBytes := buildWithdrawPSBTWithWitnessScript(t, coverageSealOutpoint, pkScript, 5000, nil)

	adapter, cleanup := p2wshWithdrawAdapter(t, psbtBytes, pkScript, userID, true)
	defer cleanup()

	err := adapter.ValidateWithdrawPsbt(p2wshWithdrawRequest(psbtBytes))
	require.Error(t, err)
	require.Contains(t, err.Error(), "no witness script")
}
