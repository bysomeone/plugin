package types

import (
	"fmt"
	"strings"
	"testing"

	"github.com/33cn/chain33/types"
	"github.com/btcsuite/btcd/txscript"
	"github.com/stretchr/testify/require"
)

func Test_GetActionName(t *testing.T) {

	require.Equal(t, NameMintAction, GetActionName(TyMintAction))
	require.Equal(t, NameTransferAction, GetActionName(TyTransferAction))
	require.Equal(t, NameConfirmAction, GetActionName(TyConfirmAction))

	require.Equal(t, "unknownAction", GetActionName(0))
}

func Test_UtxoAddress(t *testing.T) {

	utxoAddr := "74503993e7c8d4280f6fbb99ae5aaa92231a1981a358e40f97e2b4f4dfbea13c:0"
	require.True(t, IsUtxoAddress(utxoAddr))
	out, err := NewOutPointFromString(utxoAddr)
	require.NoError(t, err)
	require.Equal(t, utxoAddr, out.ToString())
}

func Test_btcCommitment(t *testing.T) {

	tx := &types.Transaction{Execer: []byte("coins")}
	commit := &BtcCommitment{
		Action:  TyConfirmAction,
		Payload: tx.Hash(),
	}
	script, err := txscript.NullDataScript(types.Encode(commit))
	require.NoError(t, err)
	fmt.Println(len(script))
}

// Test_GetActionName_crossChainMeta 新增的回填动作必须有名字：GetActionName 是链上日志/查询与
// CLI 共用的动作名映射，漏了它会显示成 "unknownAction"。
func Test_GetActionName_crossChainMeta(t *testing.T) {
	require.Equal(t, NameCrossChainMetaAction, GetActionName(TyCrossChainMetaAction))
	require.Equal(t, "CrossChainMeta", NameCrossChainMetaAction)
	// 必须与 RgbxAction 的 oneof 字段名对齐（ExecTypeBase.SetChild 用 strings.ToLower 映射），
	// 否则链上根本派发不到 Exec_CrossChainMeta。
	require.Equal(t, "crosschainmeta", strings.ToLower(NameCrossChainMetaAction))
	require.Equal(t, int32(TyCrossChainMetaAction), actionMap[NameCrossChainMetaAction])
}

// Test_CrossChainAssetKind 类别判定必须只看链上登记的 assetKind（不再等价于某个符号字面量）。
func Test_CrossChainAssetKind(t *testing.T) {
	require.True(t, CrossChainAssetBtc.Valid())
	require.True(t, CrossChainAssetRgb20.Valid())
	require.False(t, CrossChainAssetKind(9).Valid())
	require.Equal(t, "BTC", CrossChainAssetBtc.String())
	require.Equal(t, "RGB20", CrossChainAssetRgb20.String())

	btc := &CrossChainInfo{AssetKind: uint32(CrossChainAssetBtc)}
	rgb := &CrossChainInfo{AssetKind: uint32(CrossChainAssetRgb20)}
	require.False(t, IsRgb20CrossChainAsset(btc))
	require.True(t, IsRgb20CrossChainAsset(rgb))
	// 零值（没登记过类别 / 记录缺失）按 BTC 类处理：这是唯一安全的默认 —— RGB20 的金额在
	// consignment 内、链上不做金额校验，把它误判成 RGB20 会直接跳过全部链上金额判据。
	require.False(t, IsRgb20CrossChainAsset(&CrossChainInfo{}))
	require.False(t, IsRgb20CrossChainAsset(nil))
	// 符号字面量不再是判据：同名符号只要链上类别是 BTC，就走 BTC 门禁。
	require.False(t, IsRgb20CrossChainAsset(&CrossChainInfo{
		AssetSymbol: RGB20USDTSymbol, AssetKind: uint32(CrossChainAssetBtc),
	}))
}

// Test_IsKnownPrecision "未知"与"精度 0"是两件事：0 是不可分割资产的合法精度。
func Test_IsKnownPrecision(t *testing.T) {
	require.True(t, IsKnownPrecision(0))
	require.True(t, IsKnownPrecision(MaxPrecision))
	require.False(t, IsKnownPrecision(PrecisionUnknown))
	require.Greater(t, PrecisionUnknown, uint32(MaxPrecision))
	require.Equal(t, uint32(8), BTCChainPrecision)
}
