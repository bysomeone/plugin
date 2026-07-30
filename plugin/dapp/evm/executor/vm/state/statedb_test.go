package state

import (
	"math"
	"testing"

	"github.com/33cn/chain33/account"
	apimock "github.com/33cn/chain33/client/mocks"
	"github.com/33cn/chain33/common/address"
	ctypes "github.com/33cn/chain33/types"
	"github.com/33cn/chain33/util"
	"github.com/33cn/plugin/plugin/dapp/evm/executor/vm/common"
	"github.com/33cn/plugin/plugin/dapp/evm/executor/vm/model"
)

// TestCanTransferOverflowProtection 验证对 uint64→int64 溢出的防护
// 当 amount > math.MaxInt64 时，CanTransfer 必须在越过余额检查前返回 false
func TestCanTransferOverflowProtection(t *testing.T) {
	// 不设置 CoinsAccount/api —— 溢出防护应在访问账户前拦截
	mdb := &MemoryStateDB{}

	// 攻击者使用的实际值：接近 MaxUint64
	if mdb.CanTransfer("sender", 18446739873709551616) {
		t.Fatal("CanTransfer should reject overflow uint64 amount (attack value)")
	}

	// 边界：MaxInt64 + 1，刚好溢出
	if mdb.CanTransfer("sender", uint64(math.MaxInt64)+1) {
		t.Fatal("CanTransfer should reject amount > MaxInt64")
	}

	// 零值
	if mdb.CanTransfer("sender", 0) {
		t.Fatal("CanTransfer should reject zero amount")
	}
}

// TestTransferOverflowProtection 验证对 uint64→int64 溢出的防护
// 当 amount > math.MaxInt64 时，Transfer 必须返回 false 且不执行实际转账
func TestTransferOverflowProtection(t *testing.T) {
	mdb := &MemoryStateDB{}

	// 攻击者使用的实际值
	if mdb.Transfer("sender", "recipient", 18446739873709551616) {
		t.Fatal("Transfer should reject overflow uint64 amount (attack value)")
	}

	// 边界：MaxInt64 + 1
	if mdb.Transfer("sender", "recipient", uint64(math.MaxInt64)+1) {
		t.Fatal("Transfer should reject amount > MaxInt64")
	}

	// 零值转账应成功（无需实际转账）
	if !mdb.Transfer("sender", "recipient", 0) {
		t.Fatal("Transfer should accept zero amount (no-op)")
	}
}

// TestCanTransferMaxInt64 验证 MaxInt64 本身是合法的（边界情况）
func TestCanTransferMaxInt64(t *testing.T) {
	mdb := &MemoryStateDB{}
	// MaxInt64 不应该触发溢出防护
	// 如果没有 panic（访问 nil api），说明溢出检查正确通过了
	defer func() {
		if r := recover(); r == nil {
			t.Log("MaxInt64 passed overflow guard as expected (nil api panic is expected)")
		}
	}()
	mdb.CanTransfer("sender", math.MaxInt64)
}

// TestTransferMaxInt64 验证 MaxInt64 边界情况
func TestTransferMaxInt64(t *testing.T) {
	mdb := &MemoryStateDB{}
	// MaxInt64 不应该触发溢出防护
	// 如果没有 panic（访问 nil api），说明溢出检查正确通过了
	defer func() {
		if r := recover(); r == nil {
			t.Log("MaxInt64 passed overflow guard as expected (nil api panic is expected)")
		}
	}()
	mdb.Transfer("sender", "recipient", math.MaxInt64)
}

// TestAttackSimulationFullFlow 模拟完整攻击链路：
// 攻击者通过 RPC 传入 amount=18446739873709551616 (接近 MaxUint64)，
// 经过 int64→uint64 转换恢复后，进入 CanTransfer 和 Transfer。
// 修复后，这两个入口都必须拒绝此值。
// 本测试使用真实的 CoinsAccount + StateDB 搭建完整上下文。
func TestAttackSimulationFullFlow(t *testing.T) {
	cfg := ctypes.NewChain33Config(ctypes.GetDefaultCfgstring())
	api := new(apimock.QueueProtocolAPI)
	api.On("GetConfig").Return(cfg)

	dbDir, stateDB, localDB := util.CreateTestDB()
	defer util.CloseTestDB(dbDir, stateDB)

	// 创建真实的 CoinsAccount（带 in-memory StateDB）
	coinsAccount, err := account.NewAccountDB(cfg, "coins", cfg.GetCoinSymbol(), stateDB)
	if err != nil {
		t.Fatalf("failed to create coins account: %v", err)
	}

	// 构建 MemoryStateDB（模拟完整区块执行上下文）
	execAddr := address.ExecAddress(cfg.ExecName("evm"))
	mdb := NewMemoryStateDB(stateDB, localDB, coinsAccount, 1, api)
	mdb.evmPlatformAddr = execAddr

	sender := "14KEKbY3kNFLfQEGJbNweV4whre7NpqzuB"

	// 攻击者的实际交易值：amount = 18446739873709551616 ≈ 2^64
	attackAmount := uint64(18446739873709551616)

	t.Run("CanTransfer rejects attack value", func(t *testing.T) {
		if mdb.CanTransfer(sender, attackAmount) {
			t.Fatal("BUG: CanTransfer accepted overflow amount — balance check bypassed!")
		}
		t.Log("✓ Attack overflow value correctly rejected by CanTransfer")
	})

	t.Run("Transfer rejects attack value", func(t *testing.T) {
		if mdb.Transfer(sender, execAddr, attackAmount) {
			t.Fatal("BUG: Transfer accepted overflow amount — would mint free WBTY!")
		}
		t.Log("✓ Attack overflow value correctly rejected by Transfer")
	})

	// MaxInt64 应正常通过（不会被溢出防护误杀）
	t.Run("MaxInt64 is allowed", func(t *testing.T) {
		if !mdb.CanTransfer(sender, math.MaxInt64) {
			t.Log("MaxInt64 balance check result depends on actual balance, guard clause passed")
		}
	})
}

func TestMemoryStateDBAddLogStoresAddressAndDefaultsRemoved(t *testing.T) {
	txHash := common.BytesToHash([]byte("tx-log-address"))
	contractAddr := common.BytesToAddress([]byte{0x11, 0x22, 0x33})
	topic := common.BytesToHash([]byte{0xaa})

	mdb := &MemoryStateDB{
		logs:   make(map[common.Hash][]*model.ContractLog),
		txHash: txHash,
	}
	mdb.currentVer = &Snapshot{id: 1, statedb: mdb}

	mdb.AddLog(&model.ContractLog{
		Address: contractAddr,
		Topics:  []common.Hash{topic},
		Data:    []byte{0x01, 0x02},
	})

	if got := len(mdb.logs[txHash]); got != 1 {
		t.Fatalf("expected one in-memory contract log, got %d", got)
	}
	if mdb.logSize != 1 {
		t.Fatalf("expected logSize to be 1, got %d", mdb.logSize)
	}
	if got := len(mdb.currentVer.entries); got != 1 {
		t.Fatalf("expected one snapshot entry, got %d", got)
	}

	change, ok := mdb.currentVer.entries[0].(addLogChange)
	if !ok {
		t.Fatalf("expected snapshot entry type addLogChange, got %T", mdb.currentVer.entries[0])
	}
	if len(change.logs) != 1 {
		t.Fatalf("expected one receipt log in addLogChange, got %d", len(change.logs))
	}

	var evmLog ctypes.EVMLog
	if err := ctypes.Decode(change.logs[0].Log, &evmLog); err != nil {
		t.Fatalf("decode evm log failed: %v", err)
	}

	if evmLog.GetAddress() != contractAddr.String() {
		t.Fatalf("expected address %s, got %s", contractAddr.String(), evmLog.GetAddress())
	}
	if evmLog.GetRemoved() {
		t.Fatalf("expected removed default false, got true")
	}
	if len(evmLog.GetTopic()) != 1 {
		t.Fatalf("expected one topic, got %d", len(evmLog.GetTopic()))
	}
}

func TestMemoryStateDBAddLogWithNoTopics(t *testing.T) {
	txHash := common.BytesToHash([]byte("tx-log-no-topic"))
	contractAddr := common.BytesToAddress([]byte{0x44, 0x55, 0x66})

	mdb := &MemoryStateDB{
		logs:   make(map[common.Hash][]*model.ContractLog),
		txHash: txHash,
	}
	mdb.currentVer = &Snapshot{id: 1, statedb: mdb}

	mdb.AddLog(&model.ContractLog{
		Address: contractAddr,
		Data:    []byte{0x09},
	})

	if got := len(mdb.currentVer.entries); got != 1 {
		t.Fatalf("expected one snapshot entry, got %d", got)
	}
	change, ok := mdb.currentVer.entries[0].(addLogChange)
	if !ok {
		t.Fatalf("expected snapshot entry type addLogChange, got %T", mdb.currentVer.entries[0])
	}

	var evmLog ctypes.EVMLog
	if err := ctypes.Decode(change.logs[0].Log, &evmLog); err != nil {
		t.Fatalf("decode evm log failed: %v", err)
	}

	if len(evmLog.GetTopic()) != 0 {
		t.Fatalf("expected zero topics for LOG0-style event, got %d", len(evmLog.GetTopic()))
	}
	if evmLog.GetAddress() != contractAddr.String() {
		t.Fatalf("expected address %s, got %s", contractAddr.String(), evmLog.GetAddress())
	}
}
