package neutrino

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/33cn/chain33/types"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/stretchr/testify/require"
)

/*
 * 跨链资产精度的链上回填 + 核对（ensureCrossChainAssetPrecisions）。
 *
 * 这里覆盖三件事，每件都对应一种真实故障形态：
 *   1. 链上还是"尚未回填"哨兵 → 提交一次回填，链上出现真实值后收敛（正常路径）；
 *   2. 链上已经是真实值 → 不提交（幂等，不往 mempool 灌重复载荷）；
 *   3. 链上的值与合约声明的不一致 / 类别不符 → **拒绝启动**（静态分歧，重试不会变）。
 */

// fakePrecisionChain 是"链上 crossChainInfo + crossChainMeta 提交"的替身。
type fakePrecisionChain struct {
	mu        sync.Mutex
	infos     map[string]*rtypes.CrossChainInfo
	submitErr error
	submits   map[string]int
}

func newFakePrecisionChain() *fakePrecisionChain {
	return &fakePrecisionChain{
		infos:   make(map[string]*rtypes.CrossChainInfo),
		submits: make(map[string]int),
	}
}

// query 复刻执行器契约：symbol 不存在 ⇒ 空记录 + nil error（由调用方翻译成"还没上链"）。
func (c *fakePrecisionChain) query(symbol string) (*rtypes.CrossChainInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if info, ok := c.infos[symbol]; ok {
		return info, nil
	}
	return &rtypes.CrossChainInfo{}, nil
}

// submit 复刻 checkCrossChainMeta/Exec_CrossChainMeta 的口径：只接受"尚未回填"或同值幂等。
func (c *fakePrecisionChain) submit(_ string, _ string, payload types.Message) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	meta, ok := payload.(*rtypes.CrossChainAssetMeta)
	if !ok {
		return "", errors.New("unexpected payload")
	}
	if c.submitErr != nil {
		return "", c.submitErr
	}
	symbol := meta.GetAssetSymbol()
	c.submits[symbol]++
	info, ok := c.infos[symbol]
	if !ok {
		return "", errors.New("cross chain info not found")
	}
	switch {
	case info.GetPrecision() == rtypes.PrecisionUnknown, info.GetPrecision() == meta.GetPrecision():
		info.Precision = meta.GetPrecision()
	default:
		return "", errors.New("cross chain asset precision already set")
	}
	return "fake-meta-tx", nil
}

func (c *fakePrecisionChain) set(info *rtypes.CrossChainInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.infos[info.GetAssetSymbol()] = info
}

func (c *fakePrecisionChain) precision(symbol string) uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.infos[symbol].GetPrecision()
}

func (c *fakePrecisionChain) submitCount(symbol string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.submits[symbol]
}

// rgb20Record 一条"已由 CommitDKG 建好、精度尚未回填"的 RGB20 资产记录。
func rgb20Record(symbol string, precision uint32) *rtypes.CrossChainInfo {
	return &rtypes.CrossChainInfo{
		AssetSymbol: symbol,
		TssAddress:  "tb1qtestdkgaddress",
		Pubkey:      []byte{0x02, 0x01},
		AssetKind:   uint32(rtypes.CrossChainAssetRgb20),
		Precision:   precision,
	}
}

// TestEnsureCrossChainAssetPrecisions_backfillsTheSentinel 链上是"尚未回填"哨兵时，
// 提交一次回填，并在链上出现真实值之后收敛返回（不做第二次提交）。
func TestEnsureCrossChainAssetPrecisions_backfillsTheSentinel(t *testing.T) {
	const symbol = "RGB20_USDT"
	chain := newFakePrecisionChain()
	chain.set(rgb20Record(symbol, rtypes.PrecisionUnknown))

	ctx, cancel := context.WithTimeout(context.Background(), 8*crossChainPrecisionVerifyInterval)
	defer cancel()
	n := &neutrinoClient{}
	n.ensureCrossChainAssetPrecisionsWith(ctx, []string{symbol}, map[string]uint32{symbol: 6},
		chain.query, chain.submit)

	require.Equal(t, uint32(6), chain.precision(symbol), "链上精度必须被回填成合约声明的值")
	require.Equal(t, 1, chain.submitCount(symbol), "只提交一次（下一轮从链上读到已回填即收敛）")
}

// TestEnsureCrossChainAssetPrecisions_idempotentWhenAlreadyAligned 链上已等于合约精度：不提交。
func TestEnsureCrossChainAssetPrecisions_idempotentWhenAlreadyAligned(t *testing.T) {
	const symbol = "RGB20_USDT"
	chain := newFakePrecisionChain()
	chain.set(rgb20Record(symbol, 6))

	ctx, cancel := context.WithTimeout(context.Background(), 4*crossChainPrecisionVerifyInterval)
	defer cancel()
	n := &neutrinoClient{}
	n.ensureCrossChainAssetPrecisionsWith(ctx, []string{symbol}, map[string]uint32{symbol: 6},
		chain.query, chain.submit)

	require.Equal(t, 0, chain.submitCount(symbol), "已经一致就不该再提交")
}

// TestEnsureCrossChainAssetPrecisions_mismatchRefusesToStart 链上值与合约声明的值不一致 ⇒
// **拒绝启动**：链上那份是前端/CLI 换算金额的依据，与合约不一致时会静默按错的刻度动钱。
func TestEnsureCrossChainAssetPrecisions_mismatchRefusesToStart(t *testing.T) {
	const symbol = "RGB20_USDT"
	chain := newFakePrecisionChain()
	chain.set(rgb20Record(symbol, 8))

	n := &neutrinoClient{}
	require.PanicsWithError(t, (&errCrossChainPrecisionMismatchDetail{mismatches: []string{
		"symbol=RGB20_USDT: the chain records precision=8 but the contract declares precision=6 " +
			"(amounts would be off by 10^2)",
	}}).Error(), func() {
		n.ensureCrossChainAssetPrecisionsWith(context.Background(), []string{symbol},
			map[string]uint32{symbol: 6}, chain.query, chain.submit)
	})
}

// TestEnsureCrossChainAssetPrecisions_kindMismatchRefusesToStart 链上登记的类别与"本节点按
// RGB20 合约注册"矛盾 ⇒ 拒绝启动（此时充值会走错门禁分支：BTC 的 P2WSH 派生 vs RGB20 的
// thresholdSig，两条路的判据完全不同）。
func TestEnsureCrossChainAssetPrecisions_kindMismatchRefusesToStart(t *testing.T) {
	const symbol = "RGB20_USDT"
	chain := newFakePrecisionChain()
	info := rgb20Record(symbol, rtypes.PrecisionUnknown)
	info.AssetKind = uint32(rtypes.CrossChainAssetBtc)
	chain.set(info)

	n := &neutrinoClient{}
	require.Panics(t, func() {
		n.ensureCrossChainAssetPrecisionsWith(context.Background(), []string{symbol},
			map[string]uint32{symbol: 6}, chain.query, chain.submit)
	})
}

// TestEnsureCrossChainAssetPrecisions_waitsForTheRecord 记录还没上链（DKG 尚未提交）时等待而不是
// 失败 —— 那会自愈（出块后记录就在了）；提交回填没有对象，也不能 panic。
func TestEnsureCrossChainAssetPrecisions_waitsForTheRecord(t *testing.T) {
	const symbol = "RGB20_USDT"
	chain := newFakePrecisionChain() // 一条记录都没有

	ctx, cancel := context.WithTimeout(context.Background(), crossChainPrecisionVerifyInterval/2)
	defer cancel()
	done := make(chan struct{})
	n := &neutrinoClient{}
	go func() {
		defer close(done)
		n.ensureCrossChainAssetPrecisionsWith(ctx, []string{symbol}, map[string]uint32{symbol: 6},
			chain.query, chain.submit)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("waiting for the on-chain record must end with the context, not hang")
	}
	require.Equal(t, 0, chain.submitCount(symbol), "记录都还没有，回填无从谈起")
}

// TestEnsureCrossChainAssetPrecisions_submitFailureStaysRetryable 提交失败（钱包还没解锁、
// 主链不可用）不是拒绝启动的理由：下一轮重试，链上状态才是判据。
func TestEnsureCrossChainAssetPrecisions_submitFailureStaysRetryable(t *testing.T) {
	const symbol = "RGB20_USDT"
	chain := newFakePrecisionChain()
	chain.set(rgb20Record(symbol, rtypes.PrecisionUnknown))
	chain.submitErr = errors.New("wallet is locked")

	ctx, cancel := context.WithTimeout(context.Background(), crossChainPrecisionVerifyInterval/2)
	defer cancel()
	n := &neutrinoClient{}
	require.NotPanics(t, func() {
		n.ensureCrossChainAssetPrecisionsWith(ctx, []string{symbol}, map[string]uint32{symbol: 6},
			chain.query, chain.submit)
	})
	require.Equal(t, uint32(rtypes.PrecisionUnknown), chain.precision(symbol))
	require.Equal(t, 0, chain.submitCount(symbol), "失败的提交不计入「已投递」")
}
