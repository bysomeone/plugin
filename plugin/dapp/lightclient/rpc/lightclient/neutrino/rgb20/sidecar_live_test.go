package rgb20

import (
	"context"
	"os"
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
