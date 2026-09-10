package rgb20

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

func (f *fakeBridge) TSSAddress() string {
	return "bcrt1qxy2kgdygjrsqtzq2n0yrf2493p83kkfjhx0wlh"
}

func (f *fakeBridge) TSSPkScript() []byte {
	// P2WPKH scriptPubKey = OP_0 <20-byte hash160>（与 TSSAddress 对应的脚本）。
	return []byte{0x00, 0x14, 0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09,
		0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13}
}

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

// buildWithdrawValidationPSBT 构造一个能通过 ValidateWithdrawPsbt 其余检查的未签 PSBT：
// 单输入（TSS 脚本 UTXO）+ 一个收款 dust 输出（非 TSS，离开桥控制）+ 找零回 TSS。
func buildWithdrawValidationPSBT(t *testing.T, sealOutpoint string) []byte {
	t.Helper()
	tss := (&fakeBridge{}).TSSPkScript()
	op, err := wire.NewOutPointFromString(sealOutpoint)
	require.NoError(t, err)
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(wire.NewTxIn(op, nil, nil))
	tx.AddTxOut(wire.NewTxOut(546, []byte{0x51})) // 收款输出（dust）
	tx.AddTxOut(wire.NewTxOut(4000, tss))         // 找零回 TSS
	p, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)
	p.Inputs[0].WitnessUtxo = &wire.TxOut{Value: 5000, PkScript: tss}
	var buf bytes.Buffer
	require.NoError(t, p.Serialize(&buf))
	return buf.Bytes()
}

// crossCheckWithdrawValidation 构造一份"关闭 change seal"的侧车校验结果（其余字段满足门槛）。
func crossCheckWithdrawValidation(closedSeal string) *pb.ConsignmentValidation {
	return &pb.ConsignmentValidation{
		Valid:        true,
		Amount:       1000,
		SyncedHeight: 200,
		ClosedSeals:  []string{closedSeal},
	}
}

// Test_WithdrawValidate_RefreshSealStatusFromSidecar 锁住 afcb7b934 的修复：
// seal 生命周期的权威在侧车——提现的 change seal 由侧车 sync() 在其上链后提升为 minted，
// 而本地 SealIndex 只在 FinalizeWithdrawal 时把它登记为 pending-mint，此后没有路径提升。
// 因此校验前必须用侧车 ListSeals 视图对齐本地状态，否则第二笔提现会被 HR-5
// （closed seal ... is pending-mint）永久拒绝，"连续两笔提现"必失败。
//
// 反向同样要锁住：侧车仍报 pending-mint、或侧车读不到（fail-closed 降级）时，必须按本地
// 视图拒绝，而不能放行未确认的 seal。
func Test_WithdrawValidate_RefreshSealStatusFromSidecar(t *testing.T) {
	const sealOutpoint = "1111111111111111111111111111111111111111111111111111111111111111:0"

	cases := []struct {
		name          string
		sidecarStatus string
		listSealsErr  error
		wantErr       string
		wantLocal     string
	}{
		{
			name:          "sidecar minted promotes local pending-mint",
			sidecarStatus: SealStatusMinted,
			wantLocal:     SealStatusMinted,
		},
		{
			name:          "sidecar still pending-mint keeps HR-5 rejection",
			sidecarStatus: SealStatusPendingMint,
			wantErr:       "pending-mint",
			wantLocal:     SealStatusPendingMint,
		},
		{
			name:         "sidecar unavailable degrades fail-closed",
			listSealsErr: errors.New("sidecar down"),
			wantErr:      "pending-mint",
			wantLocal:    SealStatusPendingMint,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := NewMockSidecar()
			mock.ValidateResp = []*pb.ConsignmentValidation{crossCheckWithdrawValidation(sealOutpoint)}
			mock.ListSealsErr = tc.listSealsErr
			if tc.sidecarStatus != "" {
				mock.seals[sealOutpoint] = &pb.SealInfo{Outpoint: sealOutpoint, Status: tc.sidecarStatus}
			}
			adapter, cleanup := newTestAdapter(t, mock, &fakeBridge{})
			defer cleanup()

			// 本地视图：该 change seal 由上一笔提现的 FinalizeWithdrawal 登记为 pending-mint。
			require.NoError(t, adapter.seals.Add(&Seal{
				Outpoint:    sealOutpoint,
				AssetSymbol: "RGB20_USDT",
				Amount:      1000,
				Status:      SealStatusPendingMint,
			}))

			err := adapter.ValidateWithdrawPsbt(&ValidateWithdrawRequest{
				Psbt:            buildWithdrawValidationPSBT(t, sealOutpoint),
				Consignment:     []byte("consignment"),
				ExpectedAmount:  1000,
				MinSyncedHeight: 100,
			})
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
			}

			seal, ok := adapter.seals.Get(sealOutpoint)
			require.True(t, ok)
			require.Equal(t, tc.wantLocal, seal.Status)
		})
	}
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
