package rgb20

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	pb "github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20/pb"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// testDepositTxData 构造一笔最小但**规范编码**的 BTC 付款交易（无 witness、reader 恰好消费完）。
// nonce 用来区分不同的付款交易（不同 txid）。签名节点侧对 TxData 的规范性校验（A3）要求这种编码。
func testDepositTxData(t *testing.T, nonce uint32) []byte {
	t.Helper()
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{byte(nonce)}, Index: nonce}, nil, nil))
	tx.AddTxOut(wire.NewTxOut(int64(1000+int(nonce)), []byte{0x51}))
	var buf bytes.Buffer
	require.NoError(t, tx.SerializeNoWitness(&buf))
	return buf.Bytes()
}

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
	spvTxids   []string // BuildSpvProof 收到的 txid（回归：首次归因必须传结算后的 txid）
	spvProof   *SpvProof
	sig        []byte
	signedPSBT []byte
	// signErr 非空则提现签名失败（驱动"签名节点拒签"的协调者侧归类）。
	signErr error
	// signPSBTReq 最近一次提现签名请求（协调者下发的提现上下文）。
	signPSBTReq *WithdrawSignRequest
	// bestHeight/err 本地 BTC best height（深度门控用）；minConfs 链上 rgbx 最小确认数 N。
	bestHeight  uint64
	bestErr     error
	minConfs    uint64
	minConfsErr error
	signCalls   int // 签名轮次被驱动的次数（断言"重试只重发、不重签"）
	submitCalls int // 提交尝试次数（含失败）
	// submitAttempts 全部提交尝试（含失败）；submitted 仅成功的那些。
	submitAttempts []*rtypes.DepositAsset
	submitErr      error
	// depthSet 是否用 setDepth 显式设置过深度门控输入（未设置时给"总是够深"的测试默认值）。
	depthSet bool
	// signFn 可按次改写签名结果（默认返回 sig 或固定值）。
	signFn func(*DepositSignPayload) ([]byte, error)
	// broadcastErr 非空则提现广播失败（驱动"广播失败 → 重试"的事故路径）。
	broadcastErr error
	// broadcastCalls 广播尝试次数。
	broadcastCalls int
	// sweepSignCalls/sweepSignErr 扫集签名次数与失败注入（C4）。
	sweepSignCalls int
	sweepSignErr   error
	// rawBroadcasts 已定稿交易的广播（txid 列表，C4 扫集用）。
	rawBroadcasts []string
	// withdrawState 该笔提现落盘的本地状态（E9-A 的状态门；空 = 从未处理到广播）。
	withdrawState []byte
	// depositScripts 已登记的用户 P2WSH 充值脚本（pkScript hex → userID，C3 输入归属核对用）。
	depositScripts map[string]string
}

// registerDepositScript 登记一个用户充值脚本（模拟桥发放地址时写 watch 集）。
func (f *fakeBridge) registerDepositScript(pkScript []byte, userID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.depositScripts == nil {
		f.depositScripts = make(map[string]string)
	}
	f.depositScripts[hex.EncodeToString(pkScript)] = userID
}

// IsUserDepositScript 已登记的用户 P2WSH 充值脚本（见 Chain33Bridge 注释）。
func (f *fakeBridge) IsUserDepositScript(pkScript []byte) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	userID, ok := f.depositScripts[hex.EncodeToString(pkScript)]
	return userID, ok
}

// 深度门控的测试默认值：未显式配置时"永远够深"，不让门控干扰与它无关的用例。
const (
	defaultTestBtcBestHeight = uint64(1_000_000)
	defaultTestRgbxMinConfs  = uint64(6)
)

func (f *fakeBridge) GetMainChainHeight() int64 { return 100 }

func (f *fakeBridge) BuildSpvProof(txid string) (*SpvProof, error) {
	// 与真实实现一致（neutrino/rgb20deposit.go:31）：空 txid 直接失败。首次归因若拿着
	// Settle() 之前的陈旧记录调用，就会命中这里（"build spv proof: empty txid"）。
	if txid == "" {
		return nil, fmt.Errorf("empty txid")
	}
	f.mu.Lock()
	f.spvTxids = append(f.spvTxids, txid)
	f.mu.Unlock()
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
	f.submitCalls++
	f.submitAttempts = append(f.submitAttempts, dep)
	if f.submitErr != nil {
		return f.submitErr
	}
	f.submitted = append(f.submitted, dep)
	return nil
}

// submitCallCount 提交尝试次数（含失败的提交：用来断言"深度不够时根本不提交"）。
func (f *fakeBridge) submitCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.submitCalls
}

// submitAttemptList 全部提交尝试（含失败），用于比对"重发的是同一份已签对象"。
func (f *fakeBridge) submitAttemptList() []*rtypes.DepositAsset {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*rtypes.DepositAsset, len(f.submitAttempts))
	copy(out, f.submitAttempts)
	return out
}

func (f *fakeBridge) VerifyDepositSpv(*rtypes.BtcTxProof) error {
	return nil // 测试：SPV 视为有效
}

func (f *fakeBridge) SubmitConfirm(*rtypes.ConfirmTx) error { return nil }

func (f *fakeBridge) SignDepositMessage(p *DepositSignPayload) ([]byte, error) {
	f.mu.Lock()
	f.signCalls++
	fn := f.signFn
	sig := f.sig
	f.mu.Unlock()
	if fn != nil {
		return fn(p)
	}
	if sig != nil {
		return sig, nil
	}
	return []byte("threshold-sig"), nil
}

// signCallCount 签名轮次被驱动的次数。
func (f *fakeBridge) signCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.signCalls
}

func (f *fakeBridge) SignPsbt(req *WithdrawSignRequest) ([]byte, error) {
	f.mu.Lock()
	f.signPSBTReq = req
	signErr := f.signErr
	f.mu.Unlock()
	if signErr != nil {
		return nil, signErr
	}
	if f.signedPSBT != nil {
		return f.signedPSBT, nil
	}
	return req.Psbt, nil
}

func (f *fakeBridge) SignPsbtTestOnly(psbtBytes []byte) ([]byte, error) {
	if f.signedPSBT != nil {
		return f.signedPSBT, nil
	}
	return psbtBytes, nil
}

// signRequest 取最近一次提现签名请求（断言协调者下发的上下文是否正确）。
func (f *fakeBridge) signRequest() *WithdrawSignRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.signPSBTReq
}

// BtcBestHeight 本地 best height（深度门控用）。
// 未用 setDepth 显式配置时给一个"深度永远够"的默认值：既有用例关心的是别的路径，不该被门控挡住；
// 深度门控自己的用例一律显式 setDepth。
func (f *fakeBridge) BtcBestHeight() (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bestErr != nil {
		return 0, f.bestErr
	}
	if !f.depthSet {
		return defaultTestBtcBestHeight, nil
	}
	return f.bestHeight, nil
}

// RgbxMinBtcConfirmations 链上 rgbx 最小确认数 N（B8）。
func (f *fakeBridge) RgbxMinBtcConfirmations() (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.minConfsErr != nil {
		return 0, f.minConfsErr
	}
	if !f.depthSet {
		// 未显式配置时用生产默认值（[exec.sub.rgbx].minBtcConfirmations 默认 6）。
		return defaultTestRgbxMinConfs, nil
	}
	return f.minConfs, nil
}

// setDepth 一次设置深度门控的两个输入（best 与链上 N）。
func (f *fakeBridge) setDepth(best, minConfs uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bestHeight = best
	f.minConfs = minConfs
	f.depthSet = true
}

// setDepthErrs 让深度门控的两个输入各自失败（fail-closed 用例）。
func (f *fakeBridge) setDepthErrs(bestErr, minConfsErr error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bestErr = bestErr
	f.minConfsErr = minConfsErr
	f.depthSet = true
}

// setSubmitErr 让后续提交失败（驱动"提交失败 → 只重发"的重试用例）。
func (f *fakeBridge) setSubmitErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submitErr = err
}

func (f *fakeBridge) BroadcastTx(_ []byte, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.broadcastCalls++
	return f.broadcastErr
}

// SignSweepPsbt 扫集签名（C4）：假实现原样返回，签名内容由扫描用例自己核对。
func (f *fakeBridge) SignSweepPsbt(psbtBytes []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sweepSignCalls++
	if f.sweepSignErr != nil {
		return nil, f.sweepSignErr
	}
	return psbtBytes, nil
}

// BroadcastRawTx 广播已定稿的扫集交易（C4）。
func (f *fakeBridge) BroadcastRawTx(rawTx []byte, txid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rawBroadcasts = append(f.rawBroadcasts, txid)
	return f.broadcastErr
}

// broadcastCallCount 广播尝试次数（驱动"重试也走广播"的用例）。
func (f *fakeBridge) broadcastCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.broadcastCalls
}

// setBroadcastErr 让后续广播失败（驱动"广播失败 → 重试"的事故路径）。
func (f *fakeBridge) setBroadcastErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.broadcastErr = err
}

// WithdrawState 该笔提现落盘的本地状态（空 = 从未处理到广播）。见 Chain33Bridge 注释。
func (f *fakeBridge) WithdrawState(_ []byte) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.withdrawState
}

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
		// 与生产一致：头链保留深度 B 取 blockConfirmations（生产配置默认为 6）。
		HeaderRelayConfirmations: 6,
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

	// 轮询归因（首次归因即应完成铸造：submitDeposit 用的是 Settle 之后的最新记录）
	adapter.pollTransfersOnce()

	updated, err := adapter.receives.Get(rec.ReceiveID)
	require.NoError(t, err)
	require.Equal(t, ReceiveStatusMinted, updated.Status)
	require.NotEmpty(t, updated.Txid)

	// seal 索引：收款 seal 已在首次归因中提升为 minted
	require.True(t, adapter.IsSealOutpoint(updated.Seal))
	require.False(t, adapter.seals.IsPendingMint(updated.Seal))
	require.Len(t, adapter.seals.ListMinted("RGB20_USDT"), 1)

	// 已知 RGB txid 已记录
	require.True(t, adapter.IsKnownRgbTxid(updated.Txid))
}

// Test_Deposit_FirstAttribution_SubmitsFreshTxid 首次归因必须一次成功，且提交的是「含付款
// txid 的最新记录」，不是 Settle() 之前取到的陈旧副本。
//
// 回归：onSettledTransfer 在首次归因分支里先 Get 出 rec，再调 Settle()（只更新存储，不回写
// 本地副本），随后拿陈旧的 rec 去 submitDeposit → BuildSpvProof("") →
// "build spv proof: empty txid"，首次必失败、延迟 30s、下一轮 30s 轮询才自愈（三轮 harness
// 均复现的"充值首次归因必失败"）。
func Test_Deposit_FirstAttribution_SubmitsFreshTxid(t *testing.T) {
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

	// 用户付款并交付 consignment → 侧车 settle，返回含付款 txid 的状态
	settled, err := adapter.ProvideConsignment(context.Background(), []byte("consignment-bytes"), rec.ReceiveID)
	require.NoError(t, err)
	require.Equal(t, "settled", settled.Status)
	require.NotEmpty(t, settled.Txid)

	// 只驱动一轮归因（不等 30s ticker）：首次就必须成功
	adapter.pollTransfersOnce()

	// 1. SPV 证明请求拿到的是结算后的付款 txid（陈旧副本会传空串，被桥以 empty txid 拒绝）
	require.Equal(t, []string{settled.Txid}, bridge.spvTxids)
	// 2. 充值已提交给链上（首次即成功，无需重试）
	require.Len(t, bridge.submitted, 1)
	require.Equal(t, int64(1000), bridge.submitted[0].GetAmount())
	require.NotNil(t, bridge.submitted[0].GetTxProof())
	require.NotEmpty(t, bridge.submitted[0].GetTxProof().GetTxData())

	// 3. 本地状态一次性推进到 minted
	updated, err := adapter.receives.Get(rec.ReceiveID)
	require.NoError(t, err)
	require.Equal(t, ReceiveStatusMinted, updated.Status)
	require.Equal(t, settled.Txid, updated.Txid)

	// 4. 再轮询一轮不应重复提交（幂等）
	adapter.pollTransfersOnce()
	require.Len(t, bridge.submitted, 1)
	require.Len(t, bridge.spvTxids, 1)
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
			// SPV 校验要求一笔规范编码的付款交易（A3：reader 必须恰好消费完）。
			TxProof: &rtypes.BtcTxProof{TxData: testDepositTxData(t, 1), BlockHeight: 100},
		},
		Consignment:    []byte("consignment"),
		ReceiveID:      "recv-1",
		Chain33Addr:    "addr",
		BtcBlockHeight: 100,
	}
	require.NoError(t, adapter.ValidateDepositConsignment(payload))

	// 地址绑定不匹配应拒绝（TxProof 保持有效，确保拒绝来自地址绑定这一条）
	bad := *payload
	bad.Deposit = &rtypes.DepositAsset{Amount: 1000, DepositAddress: "other", AssetSymbol: "RGB20_USDT",
		TxProof: payload.Deposit.TxProof}
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
// opened seal 必须锚定在本笔 PSBT 的 txid 上：签名节点的可支配额核对（S1）按锚定关系确认
// consignment 与本笔待签交易相关，并据此算被花 seal 面额（见 withdraw_coverage_test.go）。
// 这里唯一的输出不是 TSS 脚本（vout 0）⇒ 记在"离开桥控制"一侧，面额 1000 = 提现额。
func crossCheckWithdrawValidation(t *testing.T, psbtBytes []byte, closedSeal string) *pb.ConsignmentValidation {
	t.Helper()
	return anchoredConsignment(t, psbtBytes, 1000, []string{closedSeal}, anchoredSeal{vout: 0, amount: 1000})
}

// Test_WithdrawValidate_RefreshSealStatusFromSidecar 锁住 afcb7b934 的修复：
// seal 生命周期的权威在侧车——提现的 change seal 由侧车 sync() 在其上链后提升为 minted，
// 而本地 SealIndex 只在 FinalizeWithdrawal 时把它登记为 pending-mint，此后没有路径提升。
// 因此校验前必须用侧车 ListSeals 视图对齐本地状态，否则第二笔提现会被 HR-5
// （closed seal ... is pending-mint）永久拒绝，"连续两笔提现"必失败。
//
// 反向同样要锁住：侧车仍报 pending-mint、或侧车读不到（fail-closed 降级）时，必须按本地
// 视图拒绝，而不能放行未确认的 seal。
//
// 对账是双向的（退休方向）：侧车显式报 consumed 时必须把本地条目退休为 consumed（终态），
// 否则本地与侧车分叉后留下永久垃圾条目、无自愈路径；而侧车**不认识**该 outpoint
// （账本重建后大面积出现）不是"已消费"，绝不能据此清退。
func Test_WithdrawValidate_RefreshSealStatusFromSidecar(t *testing.T) {
	const sealOutpoint = "1111111111111111111111111111111111111111111111111111111111111111:0"

	cases := []struct {
		name          string
		sidecarStatus string // 空 = 侧车列表里没有该 outpoint（账本重建 / 本桥的账本不认识它）
		localStatus   string // 本地初始状态（空 = pending-mint）
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
		{
			name:          "sidecar consumed retires local pending-mint (no HR-5 rejection)",
			sidecarStatus: SealStatusConsumed,
			wantLocal:     SealStatusConsumed,
		},
		{
			name:          "sidecar consumed retires local minted",
			sidecarStatus: SealStatusConsumed,
			localStatus:   SealStatusMinted,
			wantLocal:     SealStatusConsumed,
		},
		{
			name:      "sidecar does not know the outpoint (ledger rebuilt) must not retire",
			wantErr:   "pending-mint",
			wantLocal: SealStatusPendingMint,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := NewMockSidecar()
			psbtBytes := buildWithdrawValidationPSBT(t, sealOutpoint)
			mock.ValidateResp = []*pb.ConsignmentValidation{crossCheckWithdrawValidation(t, psbtBytes, sealOutpoint)}
			mock.ListSealsErr = tc.listSealsErr
			if tc.sidecarStatus != "" {
				mock.seals[sealOutpoint] = &pb.SealInfo{Outpoint: sealOutpoint, Status: tc.sidecarStatus}
			}
			adapter, cleanup := newTestAdapter(t, mock, &fakeBridge{})
			defer cleanup()

			// 本地视图：该 change seal 由上一笔提现的 FinalizeWithdrawal 登记为 pending-mint
			// （或被前一次对账提升为 minted）。
			local := tc.localStatus
			if local == "" {
				local = SealStatusPendingMint
			}
			require.NoError(t, adapter.seals.Add(&Seal{
				Outpoint:    sealOutpoint,
				AssetSymbol: "RGB20_USDT",
				Amount:      1000,
				Status:      local,
			}))

			err := adapter.ValidateWithdrawPsbt(&ValidateWithdrawRequest{
				Psbt:            psbtBytes,
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
			// 护栏不因退休而削弱：登记过的 outpoint（含已 consumed）一律不进 BTC 费池。
			require.True(t, adapter.seals.IsSealOutpoint(sealOutpoint))
			if tc.wantLocal == SealStatusMinted {
				require.Len(t, adapter.seals.ListMinted("RGB20_USDT"), 1)
			} else {
				require.Empty(t, adapter.seals.ListMinted("RGB20_USDT"))
			}
		})
	}
}

// Test_SealIndex_MarkConsumed_Idempotent 退休是终态：重复 MarkConsumed 不改变状态，
// 也不把条目从费池排除名单里删掉。
func Test_SealIndex_MarkConsumed_Idempotent(t *testing.T) {
	idx := newSealIndex(newMemStore())
	op := "2222222222222222222222222222222222222222222222222222222222222222:1"
	require.NoError(t, idx.Add(&Seal{Outpoint: op, AssetSymbol: "RGB20_USDT", Amount: 1}))
	require.NoError(t, idx.MarkMinted(op))
	require.NoError(t, idx.MarkConsumed(op))
	require.NoError(t, idx.MarkConsumed(op)) // 幂等
	seal, ok := idx.Get(op)
	require.True(t, ok)
	require.Equal(t, SealStatusConsumed, seal.Status)
	require.True(t, idx.IsSealOutpoint(op))
	require.False(t, idx.IsPendingMint(op))
	require.Empty(t, idx.ListMinted("RGB20_USDT"))
	require.Len(t, idx.ListByStatus(SealStatusConsumed, "RGB20_USDT"), 1)
}

func Test_WithdrawStickySealAndTxidMap(t *testing.T) {
	adapter, err := NewAdapter(Config{SidecarAddr: "/tmp/nonexistent.sock"}, newMemStore())
	require.NoError(t, err)

	chain33Hash := []byte("chain33-hash")
	require.NoError(t, adapter.putTxidMap("btctxid", chain33Hash))
	got, err := adapter.GetChain33HashByTxid("btctxid")
	require.NoError(t, err)
	require.Equal(t, chain33Hash, got)

	const sealA = "1111111111111111111111111111111111111111111111111111111111111111:0"
	const sealB = "2222222222222222222222222222222222222222222222222222222222222222:1"
	require.NoError(t, adapter.persistStickySeal(chain33Hash, []string{sealA, sealB}))
	// 集合按 outpoint 字典序规范化（与输入顺序无关），便于逐字节比较。
	require.Equal(t, sealA+","+sealB, adapter.GetStickySeal(chain33Hash))
	require.NoError(t, adapter.persistStickySeal(chain33Hash, []string{sealB, sealA}))
	require.Equal(t, sealA+","+sealB, adapter.GetStickySeal(chain33Hash))

	// 已存在则绝不覆盖：换一组 seal 重试必须失败（E9：否则同一笔 burn 会被换成另一组 seal 再付一次）。
	err = adapter.persistStickySeal(chain33Hash, []string{sealA})
	require.Error(t, err)
	class, unrecoverable := IsUnrecoverableWithdraw(err)
	require.True(t, unrecoverable, "err=%v", err)
	require.Equal(t, unrecoverableClassStickySealMismatch, class)
	require.Equal(t, sealA+","+sealB, adapter.GetStickySeal(chain33Hash), "记录不得被覆盖")

	// 算不出绑定的 seal（空集）一律拒绝：否则本记录与签名侧核对都形同虚设。
	err = adapter.persistStickySeal([]byte("another-hash"), nil)
	require.Error(t, err)
	_, unrecoverable = IsUnrecoverableWithdraw(err)
	require.True(t, unrecoverable, "err=%v", err)
	require.Empty(t, adapter.GetStickySeal([]byte("another-hash")))
}

// Test_Withdraw_UnrecoverableClassification 锁住"该提现永远不可能成功"的判定（A1 生产侧）：
//   - 新版侧车在 gRPC 层显式标记 FailedPrecondition；
//   - 只回文本的侧车按固定措辞兜底（账本重建 → 资产被重发 → pending 的 invoice 指向老 asset id）；
//   - 暂时性失败（侧车持仓不足，后续充值可补）必须仍然可重试，不能误判成永久失败。
func Test_Withdraw_UnrecoverableClassification(t *testing.T) {
	cases := []struct {
		name          string
		buildErr      error
		wantClass     string
		unrecoverable bool
	}{
		{
			name:          "sidecar marks failed-precondition (invoice contract mismatch)",
			buildErr:      status.Error(codes.FailedPrecondition, "invoice contract rgb:aaa != asset contract rgb:bbb"),
			wantClass:     unrecoverableClassAssetMismatch,
			unrecoverable: true,
		},
		{
			name:          "legacy sidecar, text only (invoice contract mismatch)",
			buildErr:      errors.New("rpc error: code = Unknown desc = invoice contract rgb:aaa != asset contract rgb:bbb"),
			wantClass:     unrecoverableClassAssetMismatch,
			unrecoverable: true,
		},
		{
			name:          "legacy sidecar, text only (asset not issued)",
			buildErr:      errors.New("asset USDT not issued"),
			wantClass:     unrecoverableClassAssetGone,
			unrecoverable: true,
		},
		{
			name:          "transient: insufficient seals is retryable",
			buildErr:      errors.New("insufficient USDT: need 500000, have 0 (across 0 minted seals)"),
			unrecoverable: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := NewMockSidecar()
			mock.BuildErr = tc.buildErr
			adapter, cleanup := newTestAdapter(t, mock, &fakeBridge{})
			defer cleanup()

			_, err := adapter.WithdrawFlow(context.Background(), &WithdrawRequest{
				Chain33TxHash:    []byte("chain33-withdraw-hash"),
				Amount:           500000,
				RecipientInvoice: "rgb:invoice",
				AssetSymbol:      "RGB20_USDT",
			})
			require.Error(t, err)
			class, ok := IsUnrecoverableWithdraw(err)
			require.Equal(t, tc.unrecoverable, ok, "err=%v", err)
			require.Equal(t, tc.wantClass, class)
		})
	}
}
