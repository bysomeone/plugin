package rgb20

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	pb "github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20/pb"
	"github.com/stretchr/testify/require"
)

// Test_SidecarLive_RoundTrip 是 Go↔Rust 真实互通测试：
// 用 rgb20.SidecarClient（Go gRPC 客户端，本包实现）调用运行中的 Rust 侧车。
// 侧车地址可用环境变量 RGB_SIDECAR_ADDR 覆盖（默认 127.0.0.1:50061）；
// 侧车不可达时跳过（不作为常规单测依赖）。
func Test_SidecarLive_RoundTrip(t *testing.T) {
	addr := os.Getenv("RGB_SIDECAR_ADDR")
	if addr == "" {
		addr = "127.0.0.1:50061"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sc, err := NewSidecar(ctx, SidecarConfig{Addr: addr, Timeout: 5 * time.Second})
	if err != nil {
		t.Skipf("sidecar not reachable at %s: %v", addr, err)
	}
	defer sc.Close()

	// ---- ListAssets round-trip ----
	assets, err := sc.ListAssets(ctx, &pb.ListAssetsRequest{})
	require.NoError(t, err, "ListAssets must succeed against live sidecar")
	t.Logf("ListAssets -> %d assets", len(assets.Assets))
	if len(assets.Assets) == 0 {
		t.Skip("sidecar has no issued assets; nothing to round-trip further")
	}
	symbol := assets.Assets[0].AssetSymbol
	for _, a := range assets.Assets {
		t.Logf("  asset symbol=%s asset_id=%s schema=%s precision=%d",
			a.AssetSymbol, a.AssetId, a.Schema, a.Precision)
	}

	// ---- GetBalance round-trip ----
	bal, err := sc.GetBalance(ctx, &pb.GetBalanceRequest{AssetSymbol: symbol})
	require.NoError(t, err, "GetBalance must succeed")
	t.Logf("GetBalance(%q) -> settled=%d pending=%d", symbol, bal.Settled, bal.Pending)

	// ---- ListSeals round-trip ----
	seals, err := sc.ListSeals(ctx, &pb.ListSealsRequest{AssetSymbol: symbol})
	require.NoError(t, err, "ListSeals must succeed")
	t.Logf("ListSeals(%q) -> %d seals", symbol, len(seals.Seals))
	for _, s := range seals.Seals {
		t.Logf("  seal outpoint=%s amount=%d status=%s", s.Outpoint, s.Amount, s.Status)
	}

	// ---- CreateReceive round-trip ----
	rec, err := sc.CreateReceive(ctx, &pb.CreateReceiveRequest{
		AssetSymbol:      symbol,
		Amount:           100,
		MinConfirmations: 1,
	})
	require.NoError(t, err, "CreateReceive must succeed")
	require.NotEmpty(t, rec.ReceiveId, "receive_id must be non-empty")
	require.NotEmpty(t, rec.Invoice, "invoice must be non-empty")
	t.Logf("CreateReceive(%q) -> receive_id=%s invoice=%q", symbol, rec.ReceiveId, rec.Invoice)

	// ---- ListTransfers round-trip (the created receive must appear) ----
	transfers, err := sc.ListTransfers(ctx, &pb.ListTransfersRequest{AssetSymbol: symbol})
	require.NoError(t, err, "ListTransfers must succeed")
	found := false
	for _, tr := range transfers.Transfers {
		t.Logf("  transfer receive_id=%s status=%s amount=%d asset_id=%s", tr.ReceiveId, tr.Status, tr.Amount, tr.AssetId)
		if tr.ReceiveId == rec.ReceiveId {
			found = true
		}
	}
	require.True(t, found, "created receive must appear in ListTransfers")
	t.Logf("GO<->RUST INTEROP OK: ListAssets/GetBalance/ListSeals/CreateReceive/ListTransfers all round-tripped")
}

// Test_AdapterLive_DepositFlow 用完整的 rgb20.Adapter（含 chain33 RGB20_USDT→侧车 USDT 符号映射）
// 对真实侧车发起一次充值请求（CreateReceive round-trip）。侧车不可达时跳过。
func Test_AdapterLive_DepositFlow(t *testing.T) {
	addr := os.Getenv("RGB_SIDECAR_ADDR")
	if addr == "" {
		addr = "127.0.0.1:50062"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	adapter, err := NewAdapter(Config{
		SidecarAddr: addr,
		Contracts: []Contract{
			{Symbol: "RGB20_USDT", SidecarSymbol: "USDT", Precision: 8},
		},
	}, NewMemStore())
	require.NoError(t, err)
	if err := adapter.Connect(ctx); err != nil {
		t.Skipf("sidecar not reachable at %s: %v", addr, err)
	}
	require.NoError(t, adapter.Start(ctx))
	defer adapter.Stop()

	rec, err := adapter.DepositFlow(ctx, &DepositRequest{
		RequestID:   "go-interop-1",
		AssetSymbol: "RGB20_USDT",
		Amount:      100,
		Chain33Addr: "1JnYYeefMhWsXvZyvjCKPZK7eYQdFpzDsk",
	})
	require.NoError(t, err, "adapter DepositFlow must succeed against live sidecar")
	require.NotEmpty(t, rec.ReceiveID)
	require.NotEmpty(t, rec.Invoice)
	t.Logf("ADAPTER-GO<->RUST OK: receive_id=%s invoice=%q", rec.ReceiveID, rec.Invoice)

	stored, err := adapter.ReceiveStore().Get(rec.ReceiveID)
	require.NoError(t, err)
	require.Equal(t, "RGB20_USDT", stored.AssetSymbol)
	require.Equal(t, rec.ReceiveID, stored.ReceiveID)
}

// Test_SidecarLive_GenesisOnlyWithdrawal 是"桥只持有 genesis seal"时的提现探针（A2 回归）。
//
// 此时 consignment 里唯一的 witness 就是**尚未广播**的提现锚定 tx：侧车若只从 btcd 解析它，
// recipient_amount 会算成 0，Go 桥 ValidateWithdrawPsbt 于是报
// "withdraw amount exceeds sealed balance: consignment=0 expected=…" —— 这正是"跳过充值直接
// 提现必失败、而全量流程（先充值、历史里有已上链的锚）能过"的原因。
//
// 需要的环境（由 CI harness 在 run_rgb20_env 之后、任何充值之前调用；侧车账本此刻只有 genesis
// 一个 seal ⇒ 无任何已上链历史锚）：
//
//	RGB_SIDECAR_ADDR            侧车 gRPC（默认 127.0.0.1:50061）
//	RGB_SIDECAR_ASSET_SYMBOL    侧车资产符号（默认 USDT）
//	RGB_SIDECAR_USER_INVOICE    用户收款 invoice（test-sim /sim/user_invoice，收款方在 TSS 之外）
//	RGB_SIDECAR_TSS_ADDRESS     桥 TSS P2WPKH 地址（chain33 getCrossChainInfo 的 tssAddress），提现找零
//	RGB_SIDECAR_WITHDRAW_AMOUNT 提现额（最小单位，默认 500000）
//
// 未设置 RGB_SIDECAR_USER_INVOICE 时跳过（不作为常规单测依赖）。
func Test_SidecarLive_GenesisOnlyWithdrawal(t *testing.T) {
	invoice := os.Getenv("RGB_SIDECAR_USER_INVOICE")
	if invoice == "" {
		t.Skip("RGB_SIDECAR_USER_INVOICE not set; genesis-only withdrawal probe not requested")
	}
	symbol := os.Getenv("RGB_SIDECAR_ASSET_SYMBOL")
	if symbol == "" {
		symbol = "USDT"
	}
	tssAddr := os.Getenv("RGB_SIDECAR_TSS_ADDRESS")
	require.NotEmpty(t, tssAddr, "RGB_SIDECAR_TSS_ADDRESS (chain33 tssAddress) is required")
	amount := int64(500000)
	if v := os.Getenv("RGB_SIDECAR_WITHDRAW_AMOUNT"); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		require.NoError(t, err, "RGB_SIDECAR_WITHDRAW_AMOUNT must be an integer")
		amount = parsed
	}
	addr := os.Getenv("RGB_SIDECAR_ADDR")
	if addr == "" {
		addr = "127.0.0.1:50061"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sc, err := NewSidecar(ctx, SidecarConfig{Addr: addr, Timeout: 20 * time.Second})
	if err != nil {
		t.Skipf("sidecar not reachable at %s: %v", addr, err)
	}
	defer sc.Close()

	build, err := sc.BuildWithdrawal(ctx, &pb.BuildWithdrawalRequest{
		AssetSymbol:      symbol,
		Amount:           amount,
		RecipientInvoice: invoice,
		ChangeAddress:    tssAddr,
		FeeRate:          20,
	})
	require.NoError(t, err, "BuildWithdrawal must succeed on a genesis-only ledger")
	require.NotEmpty(t, build.Psbt)
	require.NotEmpty(t, build.Consignment)

	// 侧车对这份"锚定 tx 尚在本地"的 consignment 做只读校验：amount 必须是桥侧被打开 seal 的
	// 面额（提现找零），而不是 0。
	v, err := sc.ValidateConsignment(ctx, &pb.ValidateConsignmentRequest{
		Consignment:    build.Consignment,
		ExpectedAmount: amount,
	})
	require.NoError(t, err, "ValidateConsignment must succeed")
	require.True(t, v.Valid, "genesis-only withdrawal consignment must validate: %s", v.ErrorMessage)
	t.Logf("consignment: amount=%d asset_id=%s opened_seals=%d closed_seals=%d synced_height=%d",
		v.Amount, v.AssetId, len(v.OpenedSeals), len(v.ClosedSeals), v.SyncedHeight)

	// 与 Go 桥完全相同的判据（withdraw.go ValidateWithdrawPsbt：v.Amount < expected ⇒ 拒绝提现）。
	require.GreaterOrEqual(t, v.Amount, amount,
		"consignment amount must cover the withdrawal; 0 here means the un-broadcast anchor was not resolved")
}
