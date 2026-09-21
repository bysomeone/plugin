package rgb20

import (
	"context"
	"errors"
	"testing"

	pb "github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20/pb"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

/*
 * 充值路径的**合约身份**校验（任务 #58）。
 *
 * 攻击形态：另造一个 ticker 也注册成 "USDT" 的合约，把它的 consignment 当作"一笔 USDT 充值"交付给桥。
 * 金额/收款 seal/付款交易 SPV 都可以是真的，唯一不对的是**合约**（asset_id）——桥若只看 symbol，
 * 链上就会按 symbol 铸出真 USDT。
 *
 * 因此断言三件事：
 *  ① 侧车结算回来的 asset_id 与配置不符 → **结算落账前**就拒（不 Settle、不登记 seal，更不铸造）；
 *  ② 即使本地已经有一条身份不符的记录（升级前落账 / 将来新代码路径写下），**提交铸造前**也要拒
 *     —— 包括"已签产物只重发"那条不做任何侧车调用的重试路径；
 *  ③ 签名节点（签 C 之前的独立校验）同样拒 —— 铸造必须带 threshold sig，这一关绕不过去。
 *  相符时一律放行（见 AcceptsConsignmentForTheRegisteredContract 与既有用例）。
 *
 * 注意既有 fixture 的合约都**没有配 assetId**，走的是"未配置 ⇒ 无从比对、告警放行"那条路
 * （见 TestVerifyAssetContract_UnconfiguredAssetIdIsUnverified）；下面这些用例显式配上 assetId，
 * 让校验真的生效。
 */

const (
	// testRegisteredAssetID 配置里声明的合约 id（"我方"合约）。
	testRegisteredAssetID = "rgb:registered-usdt"
	// testForeignAssetID 另一个同 ticker 的合约 id（伪造方）。
	testForeignAssetID = "rgb:foreign-usdt"
	// testMockSidecarAssetID 假侧车为 symbol RGB20_USDT 结算出来的 asset_id
	// （mock.go 的 CreateReceive：fmt.Sprintf("rgb:asset-%s", req.AssetSymbol)，且未配 sidecarSymbol）。
	testMockSidecarAssetID = "rgb:asset-RGB20_USDT"
)

// newAssetIdentityTestAdapter 造一个**配了 assetId** 的适配器（连假侧车、起后台协程，与 newTestAdapter 同构）。
func newAssetIdentityTestAdapter(t *testing.T, mock *MockSidecar, bridge Chain33Bridge, assetID string) (*Adapter, func()) {
	t.Helper()
	sock, cleanup := StartTestSidecar(t, mock)
	adapter, err := NewAdapter(Config{
		SidecarAddr: sock,
		Precision:   6,
		Contracts: []Contract{
			{Symbol: "RGB20_USDT", AssetID: assetID, Precision: 6, MinDeposit: 100, MinWithdraw: 100},
		},
		ChangeAddress:            "bcrt1qxxxx",
		HeaderRelayConfirmations: 6,
	}, newMemStore())
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

// newAssetIdentitySubmitAdapter 只造适配器 + 注入桥接（不连侧车、不起后台协程）：直接驱动 submitDeposit。
func newAssetIdentitySubmitAdapter(t *testing.T, bridge Chain33Bridge, assetID string) *Adapter {
	t.Helper()
	adapter, err := NewAdapter(Config{
		SidecarAddr: "test-no-sidecar",
		Contracts: []Contract{
			{Symbol: "RGB20_USDT", AssetID: assetID, Precision: 6, MinDeposit: 100, MinWithdraw: 100},
		},
		HeaderRelayConfirmations: uint32(testHeaderConfs),
	}, NewMemStore())
	require.NoError(t, err)
	adapter.SetBridge(bridge)
	return adapter
}

// seedSettledReceiveWithAsset 造一条"已结算、等待铸造"的充值记录，其 seal 记着给定的 asset_id。
func seedSettledReceiveWithAsset(t *testing.T, a *Adapter, receiveID, txid string, amount int64, assetID string) *ReceiveRecord {
	t.Helper()
	seal := FormatOutpoint(txid, 0)
	rec := &ReceiveRecord{
		ReceiveID:   receiveID,
		RequestID:   "req-" + receiveID,
		AssetSymbol: "RGB20_USDT",
		Chain33Addr: "1JnYYeefMhWsXvZyvjCKPZK7eYQdFpzDsk",
		Amount:      amount,
		Status:      ReceiveStatusSettled,
		Consignment: []byte("consignment-bytes"),
		Txid:        txid,
		Vout:        0,
		Seal:        seal,
	}
	require.NoError(t, a.receives.Put(rec))
	require.NoError(t, a.seals.Add(&Seal{
		Outpoint: seal, AssetID: assetID, AssetSymbol: rec.AssetSymbol,
		Amount: amount, Status: SealStatusPendingMint,
	}))
	return rec
}

// mockTransfer 取假侧车里该 receive 的转账状态（副本）。
func mockTransfer(t *testing.T, m *MockSidecar, receiveID string) *pb.TransferState {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.transfers[receiveID]
	require.True(t, ok, "mock sidecar has no transfer for %s", receiveID)
	return proto.Clone(st).(*pb.TransferState)
}

// setMockTransferAsset 改写假侧车结算出来的 asset_id（模拟"结算的是另一个同 ticker 的合约"）。
func setMockTransferAsset(t *testing.T, m *MockSidecar, receiveID, assetID string) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	require.Contains(t, m.transfers, receiveID)
	m.transfers[receiveID].AssetId = assetID
}

// TestDepositAssetIdentity_RejectsForeignContractAtSettlement ①：侧车报的 asset_id 不是配置里该 symbol
// 声明的合约 ⇒ **落账前**拒绝：不 Settle、不登记 pending-mint seal、不铸造（轮询那条自动提交路径同样拦下）。
func TestDepositAssetIdentity_RejectsForeignContractAtSettlement(t *testing.T) {
	bridge := &fakeBridge{}
	mock := NewMockSidecar()
	adapter, cleanup := newAssetIdentityTestAdapter(t, mock, bridge, testRegisteredAssetID)
	defer cleanup()

	rec, err := adapter.DepositFlow(context.Background(), &DepositRequest{
		RequestID:   "req-1",
		AssetSymbol: "RGB20_USDT",
		Amount:      1000,
		Chain33Addr: "1JnYYeefMhWsXvZyvjCKPZK7eYQdFpzDsk",
	})
	require.NoError(t, err)

	// 这笔结算属于**另一个**合约（同 ticker 的伪造合约）。
	setMockTransferAsset(t, mock, rec.ReceiveID, testForeignAssetID)
	st, err := adapter.ProvideConsignment(context.Background(), []byte("consignment-bytes"), rec.ReceiveID)
	require.NoError(t, err)
	require.Equal(t, "settled", st.Status, "侧车自己认为结算了 —— 这正是要防的形态")

	// 归因：返回的错误必须能看出是"合约身份不符"（而不是混进通用校验失败）。
	settled := mockTransfer(t, mock, rec.ReceiveID)
	err = adapter.onSettledTransfer(settled)
	require.Error(t, err)
	require.True(t, errors.Is(err, errAssetContractMismatch), "应报合约身份不符, got %v", err)

	// 轮询路径（服务自动提交充值那条）同样被拦：本地不落账、不登记 seal、不签名、不铸造。
	adapter.pollTransfersOnce()
	require.Equal(t, ReceiveStatusCreated, mustReceiveStatus(t, adapter, rec.ReceiveID))
	require.Empty(t, adapter.seals.All(), "身份不符的 consignment 连 seal 都不该登记")
	require.Equal(t, 0, bridge.signCallCount(), "身份不符不得驱动签名轮次")
	require.Equal(t, 0, bridge.submitCallCount(), "身份不符不得提交铸造")
	require.False(t, adapter.IsKnownRgbTxid(settled.Txid), "身份不符不得进入已知 RGB txid 集合")
}

// TestDepositAssetIdentity_AcceptsConsignmentForTheRegisteredContract 相符时放行：asset_id 与配置一致
// ⇒ 结算 + 铸造一次即成（与既有用例同一条路径，只是这次校验真的生效）。
func TestDepositAssetIdentity_AcceptsConsignmentForTheRegisteredContract(t *testing.T) {
	bridge := &fakeBridge{}
	mock := NewMockSidecar()
	adapter, cleanup := newAssetIdentityTestAdapter(t, mock, bridge, testMockSidecarAssetID)
	defer cleanup()

	rec, err := adapter.DepositFlow(context.Background(), &DepositRequest{
		RequestID:   "req-1",
		AssetSymbol: "RGB20_USDT",
		Amount:      1000,
		Chain33Addr: "1JnYYeefMhWsXvZyvjCKPZK7eYQdFpzDsk",
	})
	require.NoError(t, err)
	settled, err := adapter.ProvideConsignment(context.Background(), []byte("consignment-bytes"), rec.ReceiveID)
	require.NoError(t, err)
	require.Equal(t, "settled", settled.Status)

	adapter.pollTransfersOnce()
	require.Equal(t, ReceiveStatusMinted, mustReceiveStatus(t, adapter, rec.ReceiveID))
	require.Equal(t, 1, bridge.submitCallCount())
	require.Len(t, bridge.submitAttemptList(), 1)
}

// TestDepositAssetIdentity_RejectsForeignContractBeforeMinting ②：本地已有一条身份不符的"待铸造"记录
// （升级前落账 / 将来某条新代码路径写下的）⇒ 提交铸造前仍然拒绝，**包括已签产物只重发那条重试路径**。
func TestDepositAssetIdentity_RejectsForeignContractBeforeMinting(t *testing.T) {
	bridge := &fakeBridge{}
	adapter := newAssetIdentitySubmitAdapter(t, bridge, testRegisteredAssetID)
	rec := seedSettledReceiveWithAsset(t, adapter, "recv-foreign", testDepositTxid, 1000, testForeignAssetID)
	// 深度门控给足：拒绝必须来自合约身份这一条，而不是"等深度"。
	bridge.setDepth(testRequiredBest+10, testRgbxConfs)

	err := adapter.submitDeposit(rec)
	require.Error(t, err)
	require.True(t, errors.Is(err, errAssetContractMismatch), "应报合约身份不符, got %v", err)
	require.Equal(t, 0, bridge.signCallCount(), "身份不符不得驱动签名轮次（白花一轮 CGGMP）")
	require.Equal(t, 0, bridge.submitCallCount(), "身份不符不得提交铸造")
	require.Equal(t, 0, adapter.depositSigs.Len(), "身份不符不得落盘签名产物")
	require.Equal(t, ReceiveStatusSettled, mustReceiveStatus(t, adapter, rec.ReceiveID))

	// 已签产物只重发：该路径不做任何侧车调用，但同样要过这一关。
	art := &SignedDepositArtifact{
		Txid: rec.Txid, ReceiveID: rec.ReceiveID, Height: 100,
		Deposit: &rtypes.DepositAsset{
			Amount: rec.Amount, DepositAddress: rec.Chain33Addr,
			AssetSymbol: rec.AssetSymbol, ThresholdSig: []byte("threshold-sig"),
		},
	}
	require.NoError(t, adapter.depositSigs.Put(art))
	err = adapter.submitSignedDeposit(rec, art)
	require.Error(t, err)
	require.True(t, errors.Is(err, errAssetContractMismatch), "重发路径同样不得绕过, got %v", err)
	require.Equal(t, 0, bridge.submitCallCount(), "重发路径也不得提交")

	// 同一份记录换成相符的 asset_id：放行、提交一次。
	ok := seedSettledReceiveWithAsset(t, adapter, "recv-ok", testDepositTxid+"2", 1000, testRegisteredAssetID)
	require.NoError(t, adapter.submitDeposit(ok))
	require.Equal(t, 1, bridge.submitCallCount())
	require.Equal(t, ReceiveStatusMinted, mustReceiveStatus(t, adapter, ok.ReceiveID))
}

// TestDepositAssetIdentity_SigningNodeRejectsForeignContract ③：签名节点侧（签 C 之前的独立校验）。
// 铸造必须带 threshold sig、且每个签名节点各自校验 ⇒ 协调者侧代码路径怎么变都签不出来。
func TestDepositAssetIdentity_SigningNodeRejectsForeignContract(t *testing.T) {
	mock := NewMockSidecar()
	adapter, cleanup := newAssetIdentityTestAdapter(t, mock, &fakeBridge{}, testRegisteredAssetID)
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
			TxProof:        &rtypes.BtcTxProof{TxData: testDepositTxData(t, 1), BlockHeight: 100},
		},
		Consignment:    []byte("consignment"),
		ReceiveID:      "recv-1",
		Chain33Addr:    "addr",
		BtcBlockHeight: 100,
	}

	// 侧车校验通过、但 consignment 属于另一个合约 ⇒ 拒签，且错误可识别。
	mock.ValidateResp = []*pb.ConsignmentValidation{
		{Valid: true, Amount: 1000, AssetId: testForeignAssetID, SyncedHeight: 200},
	}
	err := adapter.ValidateDepositConsignment(payload)
	require.Error(t, err)
	require.True(t, errors.Is(err, errAssetContractMismatch), "应报合约身份不符, got %v", err)

	// 同一份消息换成配置里声明的合约 ⇒ 放行（不回归）。
	mock.ValidateResp = []*pb.ConsignmentValidation{
		{Valid: true, Amount: 1000, AssetId: testRegisteredAssetID, SyncedHeight: 200},
	}
	require.NoError(t, adapter.ValidateDepositConsignment(payload))
}

// TestVerifyAssetContract_UnconfiguredAssetIdIsUnverified 未配置 `contracts.assetId` 时无从比对：
// 记一条 WARN 后放行。**这是配置缺口，不是"已验证"** —— 该行为被显式钉在这里，将来若改成 fail-closed，
// 这个用例会红，改的时候必须是有意为之（同时要给 E2E/regtest 的配置补上 assetId）。
func TestVerifyAssetContract_UnconfiguredAssetIdIsUnverified(t *testing.T) {
	adapter := newAssetIdentitySubmitAdapter(t, &fakeBridge{}, "")
	require.NoError(t, adapter.verifyAssetContract("RGB20_USDT", testForeignAssetID),
		"未配置 assetId 时没有可比对的判据，只能降级放行")

	// 但 symbol 未注册、以及"配了 assetId / 侧车报空"这类**有判据**的情形仍然拒绝。
	err := adapter.verifyAssetContract("RGB20_UNKNOWN", testForeignAssetID)
	require.True(t, errors.Is(err, errAssetContractMismatch))

	mismatch := newAssetIdentitySubmitAdapter(t, &fakeBridge{}, testRegisteredAssetID)
	require.Error(t, mismatch.verifyAssetContract("RGB20_USDT", ""), "侧车报空 asset_id 时不得放行")
	require.NoError(t, mismatch.verifyAssetContract("RGB20_USDT", testRegisteredAssetID))
	require.NoError(t, mismatch.verifyAssetContract("RGB20_USDT", testRegisteredAssetID+"\n"),
		"两侧应做去空白比对（配置里带换行不该让所有充值都失败）")
}
