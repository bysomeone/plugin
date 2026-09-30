package rgb20

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/stretchr/testify/require"
)

// 精度是金额换算的唯一依据，而金额换算的错法是**静默**的：配错一个数，用户以为在提 1 USDT，
// 实际动的是 100 USDT 对应的最小单位。所以这里每一条都必须是"拒绝"，而不是"取一个默认值"。

// TestPrecisionUnknownCannotCollideWithARealPrecision 哨兵值必须在精度空间之外。
//
// 这条测试的真正作用是**跨语言**的：Rust 侧 `service::PRECISION_UNKNOWN` 是 u32::MAX，Go 侧
// `PrecisionUnknown` 是 math.MaxUint32，两边必须相等（proto3 的 uint32 没有 presence，"未知"只能用
// 哨兵承载）。改一侧改另一侧时，这里会立刻失败。
//
// 同一个值也是**链上**"尚未回填"的标记（rtypes.PrecisionUnknown）：桥把侧车读到的精度写进
// crossChainInfo 之前，链上就是它。
func TestPrecisionUnknownCannotCollideWithARealPrecision(t *testing.T) {
	require.Equal(t, uint32(math.MaxUint32), PrecisionUnknown)
	require.Equal(t, rtypes.PrecisionUnknown, PrecisionUnknown, "链上与侧车必须用同一个哨兵")
	// 真实精度不可能到哨兵值：RGB 的 Precision 上限是 Atto=18，本链口径上限是 rtypes.MaxPrecision。
	require.Greater(t, PrecisionUnknown, uint32(rtypes.MaxPrecision))
	require.NotEqual(t, uint32(0), PrecisionUnknown, "0 是合法精度（不可分割），不能当未知")
	var allRealPrecisions []uint32
	for p := uint32(0); p <= 18; p++ {
		allRealPrecisions = append(allRealPrecisions, p)
	}
	require.NotContains(t, allRealPrecisions, PrecisionUnknown)
}

// contractPrecisionsOne 用一个假侧车报告 `reported`，读出该合约的精度（或错误）。
func contractPrecisionsOne(t *testing.T, assetID string, reported uint32) (map[string]uint32, error) {
	t.Helper()
	mock := NewMockSidecar()
	seedMockAsset(mock, assetID, reported)
	adapter, cleanup := newTestAdapterNoContractCheck(t, mock, assetID)
	defer cleanup()
	return adapter.ContractPrecisions(context.Background())
}

// newTestAdapterNoContractCheck 造适配器并拨号（不走 Connect 的精度核对），供精度用例自己驱动。
func newTestAdapterNoContractCheck(t *testing.T, mock *MockSidecar, assetID string) (*Adapter, func()) {
	t.Helper()
	sock, cleanup := StartTestSidecar(t, mock)
	adapter, err := NewAdapter(Config{
		SidecarAddr: sock,
		Contracts: []Contract{
			{Symbol: "RGB20_USDT", AssetID: assetID, MinDeposit: 100, MinWithdraw: 100},
		},
		ChangeAddress:            "bcrt1qxxxx",
		HeaderRelayConfirmations: 6,
	}, newMemStore())
	require.NoError(t, err)
	sc, err := NewSidecar(context.Background(), SidecarConfig{Addr: sock})
	require.NoError(t, err)
	adapter.sidecar.Store(sc)
	return adapter, func() {
		_ = sc.Close()
		cleanup()
	}
}

// 合约声明的精度必须**照用**：合约说 6，桥就该按 10^6 走（USDT 的真实形态），并且把这个值
// 以 chain33 侧符号为键报给调用方（链上 crossChainInfo 的键是同一个口径）。
func TestContractPrecisionsReadsTheDeclaredValue(t *testing.T) {
	precisions, err := contractPrecisionsOne(t, "rgb:usdt-6", 6)
	require.NoError(t, err)
	require.Equal(t, map[string]uint32{"RGB20_USDT": 6}, precisions)
}

// 声明 0（不可分割资产）是**合法精度**，不是"未知"，必须照用。
func TestContractPrecisionsAcceptsADeclaredZero(t *testing.T) {
	precisions, err := contractPrecisionsOne(t, "rgb:indivisible", 0)
	require.NoError(t, err)
	require.Equal(t, map[string]uint32{"RGB20_USDT": 0}, precisions)
}

// 合约没声明精度（schema 里没有 `spec` global）时，侧车照旧采纳它，但桥**不替它猜**：
// 未知必须响亮地失败，而不是退回 8（旧行为）或 0。
func TestContractPrecisionsRefusesAnUndeclaredPrecision(t *testing.T) {
	_, err := contractPrecisionsOne(t, "rgb:no-spec", PrecisionUnknown)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrContractPrecisionUnusable), "must be a hard refusal: %v", err)
	require.Contains(t, err.Error(), "does not declare its precision")
}

// 超出链上口径能表达的精度（含别的哨兵值）一律不认：换算函数本身就拒绝 > MaxPrecision 的口径，
// 放行只会把矛盾推迟到某笔提现上。
func TestContractPrecisionsRefusesAnImplausiblePrecision(t *testing.T) {
	_, err := contractPrecisionsOne(t, "rgb:weird", uint32(rtypes.MaxPrecision)+1)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrContractPrecisionUnusable))
	require.Contains(t, err.Error(), "above this chain's maximum")
}

// 侧车不认识已注册的合约（没 adopt、或重启后 ledger 没回来）：没有任何东西能证明精度，拒绝。
func TestContractPrecisionsRefusesAnUnknownContract(t *testing.T) {
	mock := NewMockSidecar() // 一个合约都不报
	sock, cleanup := StartTestSidecar(t, mock)
	defer cleanup()
	adapter, err := NewAdapter(Config{
		SidecarAddr: sock,
		Contracts:   []Contract{{Symbol: "RGB20_USDT", AssetID: "rgb:nobody-knows"}},
	}, newMemStore())
	require.NoError(t, err)
	sc, err := NewSidecar(context.Background(), SidecarConfig{Addr: sock})
	require.NoError(t, err)
	defer func() { _ = sc.Close() }()
	adapter.sidecar.Store(sc)

	_, err = adapter.ContractPrecisions(context.Background())
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrContractPrecisionUnusable))
	require.Contains(t, err.Error(), "knows no such contract")
}

// assetId 空 = "身份还没配"：无从确定是哪个合约，跳过（充值路径早已对空 assetId fail-closed）。
// 这条挡住的是"先起节点、后回填 assetId"的部署被启动期拦住（E2E 就是这个形状）。
func TestContractPrecisionsSkipsAnUnconfiguredAssetID(t *testing.T) {
	mock := NewMockSidecar()
	sock, cleanup := StartTestSidecar(t, mock)
	defer cleanup()
	adapter, err := NewAdapter(Config{
		SidecarAddr: sock,
		Contracts:   []Contract{{Symbol: "RGB20_USDT", AssetID: ""}},
	}, newMemStore())
	require.NoError(t, err)
	sc, err := NewSidecar(context.Background(), SidecarConfig{Addr: sock})
	require.NoError(t, err)
	defer func() { _ = sc.Close() }()
	adapter.sidecar.Store(sc)

	precisions, err := adapter.ContractPrecisions(context.Background())
	require.NoError(t, err)
	require.Empty(t, precisions, "空 assetId 的合约不产出精度，也不报错")
}

// ListAssets 打不通是**可重试**的暂时性失败，不能被误判成"合约不可用而拒绝启动"
// （否则侧车重启的几十秒里每个节点都会 panic）。
func TestContractPrecisionsTreatsATransportFailureAsRetryable(t *testing.T) {
	mock := NewMockSidecar()
	mock.ListAssetsErr = errors.New("connection refused")
	adapter, cleanup := newTestAdapterNoContractCheck(t, mock, mockSidecarAssetID)
	defer cleanup()

	_, err := adapter.ContractPrecisions(context.Background())
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrContractPrecisionUnusable),
		"a transport failure must stay retryable, not become a start refusal: %v", err)
}

// 一次把所有不可用的合约都报出来：运维改一轮，而不是改一个重启一次。
func TestContractPrecisionsReportsEveryProblemAtOnce(t *testing.T) {
	mock := NewMockSidecar()
	seedMockAsset(mock, "rgb:a", 6)
	seedMockAsset(mock, "rgb:b", PrecisionUnknown)
	sock, cleanup := StartTestSidecar(t, mock)
	defer cleanup()
	adapter, err := NewAdapter(Config{
		SidecarAddr: sock,
		Contracts: []Contract{
			{Symbol: "A", AssetID: "rgb:a"},       // 可用
			{Symbol: "B", AssetID: "rgb:b"},       // 未声明精度
			{Symbol: "C", AssetID: "rgb:missing"}, // 侧车不认识
		},
	}, newMemStore())
	require.NoError(t, err)
	sc, err := NewSidecar(context.Background(), SidecarConfig{Addr: sock})
	require.NoError(t, err)
	defer func() { _ = sc.Close() }()
	adapter.sidecar.Store(sc)

	_, err = adapter.ContractPrecisions(context.Background())
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrContractPrecisionUnusable))
	require.Contains(t, err.Error(), "2 contract(s) cannot be served")
	for _, want := range []string{"symbol=B", "symbol=C"} {
		require.True(t, strings.Contains(err.Error(), want), "missing %s in: %v", want, err)
	}
}

// Connect 的语义：精度不可用时**不留下**"已连接"的状态，否则 IsConnected()（所有业务的入口）
// 会放行一个从没核对过精度的桥。
func TestConnectRefusesToStayConnectedOnAnUnusablePrecision(t *testing.T) {
	mock := NewMockSidecar()
	seedMockAsset(mock, mockSidecarAssetID, PrecisionUnknown) // 合约没声明精度
	sock, cleanup := StartTestSidecar(t, mock)
	defer cleanup()
	adapter, err := NewAdapter(Config{
		SidecarAddr: sock,
		Contracts:   []Contract{{Symbol: "RGB20_USDT", AssetID: mockSidecarAssetID}},
	}, newMemStore())
	require.NoError(t, err)

	err = adapter.Connect(context.Background())
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrContractPrecisionUnusable))
	require.Nil(t, adapter.Sidecar(), "核对没过就不该留住连接")
	require.False(t, adapter.IsConnected())
}
