package rgb20

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"testing"

	pb "github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20/pb"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

func validValidation(synced uint64) *pb.ConsignmentValidation {
	return &pb.ConsignmentValidation{
		Valid:        true,
		Amount:       1000,
		SyncedHeight: synced,
	}
}

// buildTestPSBT 构造一个含 1 输入 2 输出的未签 PSBT。
func buildTestPSBT(t *testing.T) []byte {
	t.Helper()
	tx := wire.NewMsgTx(wire.TxVersion)
	op := wire.OutPoint{Index: 0}
	tx.AddTxIn(wire.NewTxIn(&op, nil, nil))
	tx.AddTxOut(wire.NewTxOut(1000, []byte{0x51}))
	tx.AddTxOut(wire.NewTxOut(2000, []byte{0x51}))
	p, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)
	p.Inputs[0].WitnessUtxo = &wire.TxOut{Value: 5000, PkScript: []byte{0x51}}
	var buf bytes.Buffer
	require.NoError(t, p.Serialize(&buf))
	return buf.Bytes()
}

// fakeBridge 实现 Chain33Bridge，供单测记录调用。
type fakeBridge struct {
	mu         sync.Mutex
	submitted  []*rtypes.DepositAsset
	spvProof   *SpvProof
	sig        []byte
	signedPSBT []byte
}

func (f *fakeBridge) GetMainchainHeight() int64 { return 100 }

func (f *fakeBridge) BuildSpvProof(string) (*SpvProof, error) {
	if f.spvProof == nil {
		return &SpvProof{
			TxData:      []byte("tx"),
			BlockHash:   "deadbeef",
			BlockHeight: 100,
			TxIndex:     0,
		}, nil
	}
	return f.spvProof, nil
}

func (f *fakeBridge) SubmitDeposit(dep *rtypes.DepositAsset) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submitted = append(f.submitted, dep)
	return nil
}

func (f *fakeBridge) VerifyDepositSpv(*rtypes.BtcTxProof) error {
	return nil // 测试：SPV 视为有效
}

func (f *fakeBridge) SubmitConfirm(*rtypes.ConfirmTx) error { return nil }

func (f *fakeBridge) SignDepositMessage(*DepositSignPayload) ([]byte, error) {
	if f.sig != nil {
		return f.sig, nil
	}
	return []byte("threshold-sig"), nil
}

func (f *fakeBridge) SignPsbt(psbtBytes []byte) ([]byte, error) {
	if f.signedPSBT != nil {
		return f.signedPSBT, nil
	}
	return psbtBytes, nil
}

func (f *fakeBridge) GetBtcTipHeight() int64               { return 200 }
func (f *fakeBridge) BroadcastTx(_ []byte, _ string) error { return nil }

func newTestAdapter(t *testing.T, mock *MockSidecar, bridge Chain33Bridge) (*Adapter, func()) {
	t.Helper()
	sock, cleanup := StartTestSidecar(t, mock)
	cfg := Config{
		SidecarAddr: sock,
		Precision:   6,
		Contracts: []Contract{
			{Symbol: "RGB20_USDT", Precision: 6, MinDeposit: 100, MinWithdraw: 100},
		},
		ChangeAddress: "bcrt1qxxxx",
	}
	adapter, err := NewAdapter(cfg, newMemStore())
	require.NoError(t, err)
	if bridge != nil {
		adapter.SetBridge(bridge)
	}
	require.NoError(t, adapter.Connect(context.Background()))
	require.NoError(t, adapter.Start(context.Background()))
	return adapter, func() {
		adapter.Stop()
		cleanup()
	}
}

func Test_SealIndex_StateMachine(t *testing.T) {
	idx := newSealIndex(newMemStore())
	op := "0000...:0"

	require.False(t, idx.IsSealOutpoint(op))
	err := idx.Add(&Seal{Outpoint: op, AssetSymbol: "RGB20_USDT", Amount: 100})
	require.NoError(t, err)
	require.True(t, idx.IsSealOutpoint(op))
	require.True(t, idx.IsPendingMint(op))
	require.Empty(t, idx.ListMinted("RGB20_USDT"))

	// pending-mint → minted
	require.NoError(t, idx.MarkMinted(op))
	require.False(t, idx.IsPendingMint(op))
	minted := idx.ListMinted("RGB20_USDT")
	require.Len(t, minted, 1)
	require.Equal(t, SealStatusMinted, minted[0].Status)

	// minted → consumed
	require.NoError(t, idx.MarkConsumed(op))
	require.Empty(t, idx.ListMinted("RGB20_USDT"))
}

func Test_Deposit_Attribution(t *testing.T) {
	mock := NewMockSidecar()
	bridge := &fakeBridge{}
	adapter, cleanup := newTestAdapter(t, mock, bridge)
	defer cleanup()

	rec, err := adapter.DepositFlow(context.Background(), &DepositRequest{
		RequestID:   "req-1",
		AssetSymbol: "RGB20_USDT",
		Amount:      1000,
		Chain33Addr: "1JnYYeefMhWsXvZyvjCKPZK7eYQdFpzDsk",
	})
	require.NoError(t, err)
	require.Equal(t, "created", rec.Status)
	require.NotEmpty(t, rec.Invoice)

	// 交付 consignment → 结算
	st, err := adapter.ProvideConsignment(context.Background(), []byte("consignment-bytes"), rec.ReceiveID)
	require.NoError(t, err)
	require.Equal(t, "settled", st.Status)

	// 轮询归因
	adapter.pollTransfersOnce()

	updated, err := adapter.receives.Get(rec.ReceiveID)
	require.NoError(t, err)
	require.Equal(t, ReceiveStatusSettled, updated.Status)
	require.NotEmpty(t, updated.Txid)

	// seal 索引：收款 seal 进入 pending-mint
	require.True(t, adapter.IsSealOutpoint(updated.Seal))
	require.True(t, adapter.seals.IsPendingMint(updated.Seal))

	// 已知 RGB txid 已记录
	require.True(t, adapter.IsKnownRgbTxid(updated.Txid))
}

func Test_ValidateDepositConsignment(t *testing.T) {
	mock := NewMockSidecar()
	mock.ValidateResp = []*pb.ConsignmentValidation{validValidation(200)}
	adapter, cleanup := newTestAdapter(t, mock, &fakeBridge{})
	defer cleanup()

	rec := &ReceiveRecord{
		ReceiveID:   "recv-1",
		AssetSymbol: "RGB20_USDT",
		Chain33Addr: "addr",
		Amount:      1000,
		Status:      ReceiveStatusSettled,
		Seal:        "aaaa:0",
	}
	require.NoError(t, adapter.receives.Put(rec))

	payload := &DepositSignPayload{
		Deposit: &rtypes.DepositAsset{
			Amount:         1000,
			DepositAddress: "addr",
			AssetSymbol:    "RGB20_USDT",
		},
		Consignment:    []byte("consignment"),
		ReceiveID:      "recv-1",
		Chain33Addr:    "addr",
		BtcBlockHeight: 100,
	}
	require.NoError(t, adapter.ValidateDepositConsignment(payload))

	// 地址绑定不匹配应拒绝
	bad := *payload
	bad.Deposit = &rtypes.DepositAsset{Amount: 1000, DepositAddress: "other", AssetSymbol: "RGB20_USDT"}
	require.Error(t, adapter.ValidateDepositConsignment(&bad))

	// 已 minted 去重
	rec.Status = ReceiveStatusMinted
	require.NoError(t, adapter.receives.Put(rec))
	require.Error(t, adapter.ValidateDepositConsignment(payload))
}

// Test_DepositSignPayload_JSONRoundTrip rgb20-deposit 消息经 JSON 在 P2P 通知中传输，
// proto 字段（DepositAsset/TxProof）必须无损往返。
func Test_DepositSignPayload_JSONRoundTrip(t *testing.T) {
	payload := &DepositSignPayload{
		Deposit: &rtypes.DepositAsset{
			Amount:         1000,
			DepositAddress: "addr",
			AssetSymbol:    rtypes.RGB20USDTSymbol,
			TxProof: &rtypes.BtcTxProof{
				TxData:      []byte("txdata"),
				BlockHash:   "hash",
				BlockHeight: 100,
				TxIndex:     0,
				MerkleProof: [][]byte{[]byte("proof")},
			},
		},
		Consignment:    []byte("consignment"),
		ReceiveID:      "recv-1",
		Chain33Addr:    "addr",
		SessionID:      "session-1",
		BtcBlockHeight: 100,
		BtcBlockHash:   "hash",
		BtcTxIndex:     0,
	}
	b, err := json.Marshal(payload)
	require.NoError(t, err)
	got := &DepositSignPayload{}
	require.NoError(t, json.Unmarshal(b, got))
	require.Equal(t, payload.Deposit.GetAmount(), got.Deposit.GetAmount())
	require.Equal(t, payload.Deposit.GetAssetSymbol(), got.Deposit.GetAssetSymbol())
	require.Equal(t, payload.Deposit.GetTxProof().GetTxData(), got.Deposit.GetTxProof().GetTxData())
	require.Equal(t, payload.Deposit.GetTxProof().GetBlockHeight(), got.Deposit.GetTxProof().GetBlockHeight())
	require.Equal(t, payload.Deposit.GetTxProof().GetMerkleProof(), got.Deposit.GetTxProof().GetMerkleProof())
	require.Equal(t, payload.ReceiveID, got.ReceiveID)
	require.Equal(t, payload.SessionID, got.SessionID)
	require.Equal(t, payload.Consignment, got.Consignment)
}

func Test_WithdrawStickySealAndTxidMap(t *testing.T) {
	adapter, err := NewAdapter(Config{SidecarAddr: "/tmp/nonexistent.sock"}, newMemStore())
	require.NoError(t, err)

	chain33Hash := []byte("chain33-hash")
	require.NoError(t, adapter.putTxidMap("btctxid", chain33Hash))
	got, err := adapter.GetChain33HashByTxid("btctxid")
	require.NoError(t, err)
	require.Equal(t, chain33Hash, got)

	require.NoError(t, adapter.persistStickySeal(chain33Hash, buildTestPSBT(t)))
	require.NotEmpty(t, adapter.GetStickySeal(chain33Hash))
}
