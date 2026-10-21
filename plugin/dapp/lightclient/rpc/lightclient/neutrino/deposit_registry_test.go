package neutrino

import (
	"errors"
	"fmt"
	"testing"

	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/stretchr/testify/require"
)

// 本文件覆盖桥侧"链上充值登记 → watch 集"的轮询：转换、游标推进、单条失败隔离、
// 幂等，以及游标/失败集合的持久化（重启恢复）。

func testDepositRegistration(userID string, height int64, index int32) *rtypes.DepositScriptItem {
	return &rtypes.DepositScriptItem{
		UserID:   userID,
		Address:  "bcrt1q" + userID,
		PkScript: "0014" + userID,
		Spec:     rtypes.P2WSHDepositSpecV1,
		Height:   height,
		Index:    index,
	}
}

// fakeDepositQuery 按 (height,index) 游标过滤并按单页条数截断，语义与链上查询一致。
func fakeDepositQuery(items *[]*rtypes.DepositScriptItem, fail *bool) func(int64, int64, int64) (*rtypes.DepositScriptList, error) {
	return func(startHeight, startIndex, endHeight int64) (*rtypes.DepositScriptList, error) {
		if *fail {
			return nil, errors.New("chain query unavailable")
		}
		out := make([]*rtypes.DepositScriptItem, 0, depositRegistryQueryCount)
		for _, it := range *items {
			if it.GetHeight() < startHeight ||
				(it.GetHeight() == startHeight && int64(it.GetIndex()) < startIndex) ||
				it.GetHeight() > endHeight {
				continue
			}
			out = append(out, it)
			if len(out) >= depositRegistryQueryCount {
				break
			}
		}
		return &rtypes.DepositScriptList{Items: out}, nil
	}
}

// Test_depositRegistryProcessor_pollsRegistrations 登记被转成 ensure 调用；查询失败不推进游标；
// 游标越过已扫窗口后不再重复处理，新登记从断点继续。
func Test_depositRegistryProcessor_pollsRegistrations(t *testing.T) {
	items := []*rtypes.DepositScriptItem{
		testDepositRegistration("addrA", 10, 0),
		testDepositRegistration("addrB", 10, 1),
		testDepositRegistration("addrC", 25, 0),
	}
	fail := true
	var ensured []string
	p := &depositRegistryProcessor{
		query:  fakeDepositQuery(&items, &fail),
		ensure: func(userID string) error { ensured = append(ensured, userID); return nil },
	}

	// Query failure: cursor must not move, nothing is imported.
	p.pollOnce(30)
	require.Empty(t, ensured)
	require.Equal(t, int64(0), p.nextHeight)
	require.Equal(t, int64(0), p.nextIndex)

	fail = false
	p.pollOnce(30)
	require.Equal(t, []string{"addrA", "addrB", "addrC"}, ensured)
	require.Equal(t, int64(31), p.nextHeight, "an exhausted window advances past its end")
	require.Equal(t, int64(0), p.nextIndex)

	// Same range again: cursor is past the window, so nothing is re-imported.
	p.pollOnce(30)
	require.Equal(t, []string{"addrA", "addrB", "addrC"}, ensured)

	// A later registration is picked up from the advanced cursor.
	items = append(items, testDepositRegistration("addrD", 40, 0))
	p.pollOnce(45)
	require.Equal(t, []string{"addrA", "addrB", "addrC", "addrD"}, ensured)
	require.Equal(t, int64(46), p.nextHeight)
}

// Test_depositRegistryProcessor_singleFailureDoesNotBlockOthers 单条导入失败（典型：watch 集已满）
// 既不影响同批其余登记，也不让扫描停住；失败项进重试集合，下个周期成功后被清除。
func Test_depositRegistryProcessor_singleFailureDoesNotBlockOthers(t *testing.T) {
	items := []*rtypes.DepositScriptItem{
		testDepositRegistration("addrA", 10, 0),
		testDepositRegistration("addrB", 10, 1),
		testDepositRegistration("addrC", 10, 2),
	}
	fail := false
	var ensured []string
	var failed, done []string
	failB := true
	p := &depositRegistryProcessor{
		query: fakeDepositQuery(&items, &fail),
		ensure: func(userID string) error {
			ensured = append(ensured, userID)
			if failB && userID == "addrB" {
				return errors.New("deposit watch set is full (10 >= maxWatchedDepositScripts 10)")
			}
			return nil
		},
		markFailed: func(userID string) error { failed = append(failed, userID); return nil },
		markDone:   func(userID string) error { done = append(done, userID); return nil },
	}

	p.pollOnce(20)
	require.Equal(t, []string{"addrA", "addrB", "addrC"}, ensured, "a failing entry must not stop the rest")
	require.Equal(t, int64(21), p.nextHeight, "the cursor still advances past handled entries")
	require.Equal(t, []string{"addrB"}, p.retrying)
	require.Equal(t, []string{"addrB"}, failed, "the failure must be persisted for restart recovery")
	require.Empty(t, done)

	// Next round: the retry succeeds and leaves the set.
	failB = false
	p.retryPending()
	require.Empty(t, p.retrying)
	require.Equal(t, []string{"addrB"}, done)

	// Nothing is retried twice.
	p.retryPending()
	require.Empty(t, p.retrying)
	require.Equal(t, []string{"addrB"}, done)
}

// Test_depositRegistryProcessor_idempotentRepeatedUser 同一 userID 在链上重复出现时，
// 轮询器逐条转发（不做 processed 去重），最终只导入一次 —— 幂等由 ensure 保证。
func Test_depositRegistryProcessor_idempotentRepeatedUser(t *testing.T) {
	items := []*rtypes.DepositScriptItem{
		testDepositRegistration("addrA", 10, 0),
		testDepositRegistration("addrA", 12, 0),
	}
	fail := false
	calls, imports := 0, 0
	seen := make(map[string]bool)
	p := &depositRegistryProcessor{
		query: fakeDepositQuery(&items, &fail),
		// Mirrors ensureUserDepositScript: the first call imports, later calls are no-ops.
		ensure: func(userID string) error {
			calls++
			if !seen[userID] {
				seen[userID] = true
				imports++
			}
			return nil
		},
	}
	p.pollOnce(20)
	require.Equal(t, 2, calls, "every on-chain entry is forwarded to ensure")
	require.Equal(t, 1, imports, "one user → one watch entry, thanks to ensure's idempotency")
	require.Len(t, seen, 1)
	require.Empty(t, p.retrying)
}

// Test_depositRegistryProcessor_pagesFullWindow 一个高度窗口内的登记超过单页条数时，
// 从最后一条继续，不会因 Count 截断而漏掉后面的登记。
func Test_depositRegistryProcessor_pagesFullWindow(t *testing.T) {
	var items []*rtypes.DepositScriptItem
	for i := 0; i < depositRegistryQueryCount; i++ {
		items = append(items, testDepositRegistration(fmt.Sprintf("addr-%03d", i), 5, int32(i)))
	}
	items = append(items, testDepositRegistration("addr-tail", 6, 0))

	fail := false
	var ensured []string
	p := &depositRegistryProcessor{
		query:  fakeDepositQuery(&items, &fail),
		ensure: func(userID string) error { ensured = append(ensured, userID); return nil },
	}
	p.pollOnce(10)
	require.Len(t, ensured, depositRegistryQueryCount+1)
	require.Equal(t, "addr-tail", ensured[len(ensured)-1])
	require.Equal(t, int64(11), p.nextHeight, "the scanner continues past the paged window")
}

// Test_depositRegistry_persistence 游标与失败集合落盘：重启后从断点继续，失败项继续重试；
// 修复后清除。缺失 bucket 时游标从 0 开始。
func Test_depositRegistry_persistence(t *testing.T) {
	dir := t.TempDir()
	_, db, err := openWalletDB(dir, "deposit_registry.db")
	if err != nil {
		t.Skipf("walletdb/bdb unavailable: %v", err)
	}

	n := &neutrinoClient{}
	n.neutrinoCfg.Database = db
	height, index := n.loadDepositRegistryCursor()
	require.Zero(t, height)
	require.Zero(t, index)
	require.Empty(t, n.loadDepositRegistryRetries())

	n.saveDepositRegistryCursor(123, 7)
	require.NoError(t, n.markDepositRegistryRetry("addrA", true))
	require.NoError(t, n.markDepositRegistryRetry("addrB", true))
	// Recording the same failure again is harmless.
	require.NoError(t, n.markDepositRegistryRetry("addrA", true))
	require.NoError(t, db.Close())

	// "Restart": reopen the same database and rebuild the processor state.
	_, db2, err := openWalletDB(dir, "deposit_registry.db")
	require.NoError(t, err)
	defer db2.Close()
	restarted := &neutrinoClient{}
	restarted.neutrinoCfg.Database = db2

	height, index = restarted.loadDepositRegistryCursor()
	require.Equal(t, int64(123), height)
	require.Equal(t, int64(7), index)

	retries := restarted.loadDepositRegistryRetries()
	require.ElementsMatch(t, []string{"addrA", "addrB"}, retries)

	// The retry set seeds the processor, and a fixed entry is removed from disk.
	var ensured []string
	p := &depositRegistryProcessor{
		ensure:     func(userID string) error { ensured = append(ensured, userID); return nil },
		markDone:   func(userID string) error { return restarted.markDepositRegistryRetry(userID, false) },
		markFailed: func(userID string) error { return restarted.markDepositRegistryRetry(userID, true) },
	}
	p.seedRetries(retries)
	require.ElementsMatch(t, []string{"addrA", "addrB"}, p.retrying)
	p.retryPending()
	require.ElementsMatch(t, []string{"addrA", "addrB"}, ensured)
	require.Empty(t, p.retrying)
	require.Empty(t, restarted.loadDepositRegistryRetries())
}
