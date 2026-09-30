package rgb20

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	pb "github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20/pb"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/stretchr/testify/require"
)

// 精度是金额换算的唯一依据，而金额换算的错法是**静默**的：配错一个数，用户以为在提 1 USDT，
// 实际动的是 100 USDT 对应的最小单位。所以这里每一条都必须是"拒绝",而不是"取一个默认值"。

// TestPrecisionUnknownCannotCollideWithARealPrecision 哨兵值必须在精度空间之外。
//
// 这条测试的真正作用是**跨语言**的：Rust 侧 `service::PRECISION_UNKNOWN` 是 u32::MAX，Go 侧
// `PrecisionUnknown` 是 math.MaxUint32，两边必须相等（proto3 的 uint32 没有 presence，"未知"只能用
// 哨兵承载）。改一侧改另一侧时，这里会立刻失败。
func TestPrecisionUnknownCannotCollideWithARealPrecision(t *testing.T) {
	require.Equal(t, uint32(math.MaxUint32), PrecisionUnknown)
	// 真实精度不可能到哨兵值：RGB 的 Precision 上限是 Atto=18，本链口径上限是 rtypes.MaxPrecision。
	require.Greater(t, PrecisionUnknown, uint32(rtypes.MaxPrecision))
	require.NotEqual(t, uint32(0), PrecisionUnknown, "0 是合法精度（不可分割），不能当未知")
	var allRealPrecisions []uint32
	for p := uint32(0); p <= 18; p++ {
		allRealPrecisions = append(allRealPrecisions, p)
	}
	require.NotContains(t, allRealPrecisions, PrecisionUnknown)
}

// verifyOne 用一个假侧车报告 `reported`，核对配置精度 `configured`，返回核对结果。
func verifyOne(t *testing.T, assetID string, reported uint32, configured uint32) error {
	t.Helper()
	mock := NewMockSidecar()
	seedMockAsset(mock, assetID, reported)
	adapter, cleanup := newTestAdapterNoContractCheck(t, mock, assetID, configured)
	defer cleanup()
	return adapter.VerifyContractPrecisions(context.Background())
}

// newTestAdapterNoContractCheck 造适配器并拨号（不走 Connect 的精度核对），供精度核对用例自己驱动。
func newTestAdapterNoContractCheck(t *testing.T, mock *MockSidecar, assetID string, configured uint32) (*Adapter, func()) {
	t.Helper()
	sock, cleanup := StartTestSidecar(t, mock)
	adapter, err := NewAdapter(Config{
		SidecarAddr: sock,
		Contracts: []Contract{
			{Symbol: "RGB20_USDT", AssetID: assetID, Precision: configured, MinDeposit: 100, MinWithdraw: 100},
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

// 非 8 的精度必须**照用**：合约说 6，配置说 6，换算就该按 10^6 走（USDT 的真实形态）。
func TestVerifyContractPrecisionsAcceptsANonDefaultPrecision(t *testing.T) {
	require.NoError(t, verifyOne(t, "rgb:usdt-6", 6, 6))
}

// 配置与合约对不上 = 那个静默 100 倍误差本身：必须拒绝，且错误里要能看出两个值。
func TestVerifyContractPrecisionsRefusesAConfigContractMismatch(t *testing.T) {
	err := verifyOne(t, "rgb:usdt-6", 6, 8)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrContractPrecisionMismatch), "must be a hard refusal: %v", err)
	require.Contains(t, err.Error(), "configured precision=8")
	require.Contains(t, err.Error(), "declares precision=6")
}

// 合约没声明精度（schema 里没有 `spec` global）时，侧车照旧采纳它，但桥**不替它猜**：
// 未知必须响亮地失败，而不是退回 8（旧行为）或 0。
func TestVerifyContractPrecisionsRefusesAnUndeclaredPrecision(t *testing.T) {
	err := verifyOne(t, "rgb:no-spec", PrecisionUnknown, 8)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrContractPrecisionMismatch), "must be a hard refusal: %v", err)
	require.Contains(t, err.Error(), "does not declare its precision")
}

// 未知 ≠ 0：合约真的声明 0（不可分割资产）而配置也写 0 时，这是**一致**的，不能被当成未知拒掉，
// 否则精度 0 的资产永远起不来。
func TestVerifyContractPrecisionsAcceptsADeclaredZero(t *testing.T) {
	require.NoError(t, verifyOne(t, "rgb:indivisible", 0, 0))
}

// 配置说 0、合约说 6 也是不一致（典型形态：配置漏填 precision，Go 零值恰好是 0）。
func TestVerifyContractPrecisionsRefusesAZeroConfigAgainstADeclaredPrecision(t *testing.T) {
	err := verifyOne(t, "rgb:usdt-6", 6, 0)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrContractPrecisionMismatch))
	require.Contains(t, err.Error(), "configured precision=0")
}

// 超出链上口径能表达的精度（含别的哨兵值）一律不认：换算函数本身就拒绝 > MaxPrecision 的口径，
// 放行只会把矛盾推迟到某笔提现上。
func TestVerifyContractPrecisionsRefusesAnImplausiblePrecision(t *testing.T) {
	err := verifyOne(t, "rgb:weird", uint32(rtypes.MaxPrecision)+1, 8)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrContractPrecisionMismatch))
	require.Contains(t, err.Error(), "above this chain's maximum")
}

// 侧车不认识配置声明的合约（没 adopt、或重启后 Stock/ledger 没回来）：没有任何东西能证明精度，
// 拒绝。
func TestVerifyContractPrecisionsRefusesAnUnknownContract(t *testing.T) {
	mock := NewMockSidecar() // 一个合约都不报
	sock, cleanup := StartTestSidecar(t, mock)
	defer cleanup()
	adapter, err := NewAdapter(Config{
		SidecarAddr: sock,
		Contracts:   []Contract{{Symbol: "RGB20_USDT", AssetID: "rgb:nobody-knows", Precision: 6}},
	}, newMemStore())
	require.NoError(t, err)
	sc, err := NewSidecar(context.Background(), SidecarConfig{Addr: sock})
	require.NoError(t, err)
	defer func() { _ = sc.Close() }()
	adapter.sidecar.Store(sc)

	err = adapter.VerifyContractPrecisions(context.Background())
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrContractPrecisionMismatch))
	require.Contains(t, err.Error(), "knows no such contract")
}

// assetId 空 = "身份还没配"：无从确定是哪个合约，跳过（充值路径早已对空 assetId fail-closed）。
// 这条挡住的是"先起节点、后回填 assetId"的部署被启动期拦住（E2E 就是这个形状）。
func TestVerifyContractPrecisionsSkipsAnUnconfiguredAssetID(t *testing.T) {
	mock := NewMockSidecar()
	sock, cleanup := StartTestSidecar(t, mock)
	defer cleanup()
	adapter, err := NewAdapter(Config{
		SidecarAddr: sock,
		Contracts:   []Contract{{Symbol: "RGB20_USDT", AssetID: "", Precision: 6}},
	}, newMemStore())
	require.NoError(t, err)
	sc, err := NewSidecar(context.Background(), SidecarConfig{Addr: sock})
	require.NoError(t, err)
	defer func() { _ = sc.Close() }()
	adapter.sidecar.Store(sc)

	require.NoError(t, adapter.VerifyContractPrecisions(context.Background()))
}

// ListAssets 打不通是**可重试**的暂时性失败，不能被误判成"配置不一致而拒绝启动"
// （否则侧车重启的几十秒里每个节点都会 panic）。
func TestVerifyContractPrecisionsTreatsATransportFailureAsRetryable(t *testing.T) {
	mock := NewMockSidecar()
	mock.ListAssetsErr = errors.New("connection refused")
	adapter, cleanup := newTestAdapterNoContractCheck(t, mock, mockSidecarAssetID, 6)
	defer cleanup()

	err := adapter.VerifyContractPrecisions(context.Background())
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrContractPrecisionMismatch),
		"a transport failure must stay retryable, not become a start refusal: %v", err)
}

// 一次把所有不一致都报出来：运维改一轮，而不是改一个重启一次。
func TestVerifyContractPrecisionsReportsEveryMismatchAtOnce(t *testing.T) {
	mock := NewMockSidecar()
	seedMockAsset(mock, "rgb:a", 6)
	seedMockAsset(mock, "rgb:b", PrecisionUnknown)
	sock, cleanup := StartTestSidecar(t, mock)
	defer cleanup()
	adapter, err := NewAdapter(Config{
		SidecarAddr: sock,
		Contracts: []Contract{
			{Symbol: "A", AssetID: "rgb:a", Precision: 8},       // 不一致
			{Symbol: "B", AssetID: "rgb:b", Precision: 6},       // 未声明
			{Symbol: "C", AssetID: "rgb:missing", Precision: 6}, // 侧车不认识
		},
	}, newMemStore())
	require.NoError(t, err)
	sc, err := NewSidecar(context.Background(), SidecarConfig{Addr: sock})
	require.NoError(t, err)
	defer func() { _ = sc.Close() }()
	adapter.sidecar.Store(sc)

	err = adapter.VerifyContractPrecisions(context.Background())
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrContractPrecisionMismatch))
	require.Contains(t, err.Error(), "3 contract(s) cannot be served")
	for _, want := range []string{"symbol=A", "symbol=B", "symbol=C"} {
		require.True(t, strings.Contains(err.Error(), want), "missing %s in: %v", want, err)
	}
}

// Connect 的语义：核对不通过时**不留下**"已连接"的状态，否则 IsConnected()（所有业务的入口）
// 会放行一个从未核对过精度的桥。
func TestConnectRefusesToStayConnectedOnAPrecisionMismatch(t *testing.T) {
	mock := NewMockSidecar()
	seedMockAsset(mock, mockSidecarAssetID, 6)
	sock, cleanup := StartTestSidecar(t, mock)
	defer cleanup()
	adapter, err := NewAdapter(Config{
		SidecarAddr: sock,
		Contracts: []Contract{
			{Symbol: "RGB20_USDT", AssetID: mockSidecarAssetID, Precision: 8}, // 配置与合约不一致
		},
	}, newMemStore())
	require.NoError(t, err)

	err = adapter.Connect(context.Background())
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrContractPrecisionMismatch))
	require.False(t, adapter.IsConnected(), "a refused precision check must not leave the adapter connected")
}

// 反向控制：一致的部署连得上，并且连接是"已核对"的。
func TestConnectStaysConnectedWhenPrecisionsAgree(t *testing.T) {
	mock := NewMockSidecar()
	seedMockAsset(mock, mockSidecarAssetID, 6)
	sock, cleanup := StartTestSidecar(t, mock)
	defer cleanup()
	adapter, err := NewAdapter(Config{
		SidecarAddr: sock,
		Contracts: []Contract{
			{Symbol: "RGB20_USDT", AssetID: mockSidecarAssetID, Precision: 6},
		},
	}, newMemStore())
	require.NoError(t, err)

	require.NoError(t, adapter.Connect(context.Background()))
	require.True(t, adapter.IsConnected())
}

// 没有配置任何合约（BTC-only 部署）时不做任何核对：不能让一个用不到 RGB20 的部署起不来。
func TestVerifyContractPrecisionsIsANoOpWithoutContracts(t *testing.T) {
	adapter, err := NewAdapter(Config{SidecarAddr: "/tmp/nonexistent.sock"}, newMemStore())
	require.NoError(t, err)
	require.NoError(t, adapter.VerifyContractPrecisions(context.Background()))
}

// 顺带把"资产信息里带精度"这条 wire 形状钉住：ListAssets 的返回项是我们读精度的唯一来源。
func TestListAssetsCarriesPrecision(t *testing.T) {
	mock := NewMockSidecar()
	seedMockAsset(mock, "rgb:x", 6)
	sock, cleanup := StartTestSidecar(t, mock)
	defer cleanup()
	sc, err := NewSidecar(context.Background(), SidecarConfig{Addr: sock})
	require.NoError(t, err)
	defer func() { _ = sc.Close() }()

	resp, err := sc.ListAssets(context.Background(), &pb.ListAssetsRequest{})
	require.NoError(t, err)
	require.Len(t, resp.GetAssets(), 1)
	require.Equal(t, uint32(6), resp.GetAssets()[0].GetPrecision())
}
