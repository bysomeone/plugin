package neutrino

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

/*
 * C2 阶段 1：BTC 的 CommitDKG 必须带 33 字节压缩 TSS 群公钥。
 *
 * 背景（C1 起链上规则变了）：checkCommitDKG 对**所有** symbol（含 BTC/XBTC）都要求 33 字节压缩
 * 公钥，并校验 hash160(pubkey)==pkScript[2:]。桥侧不带 pubkey 的提交会被 ErrInvalidDkgAddress 拒，
 * 而旧实现把它们当"无限重试"咽掉 → 桥的 TSS init 静默卡在提交 CommitDKG（E2E 里表现为起不来）。
 *
 * 这里是桥侧的**发送端**测试：载荷必须过链上同一套判据（用链上同一个
 * rtypes.ParseDepositTssPubKey + 同一份 hash160 绑定核对，不是另写一份"看起来一样"的检查）。
 */

// ---- 阶段 1：CommitDKG 载荷必须带 33 字节压缩公钥（否则链上必拒）----

// chainCommitDKGPubkeyGate 复刻 checkCommitDKG 的公钥判据（executor/checktx.go）：
// 只接受 33 字节压缩公钥（rtypes.ParseDepositTssPubKey，即链上同一函数），且
// hash160(pubkey) == pkScript[2:]（pkScript 必须是 22 字节的 P2WPKH）。
func chainCommitDKGPubkeyGate(payload *rtypes.CommitDKG) error {
	pub, err := rtypes.ParseDepositTssPubKey(payload.GetPubkey())
	if err != nil {
		return err
	}
	pkScript := payload.GetPkScript()
	if len(pkScript) != 22 || pkScript[0] != txscript.OP_0 || pkScript[1] != 0x14 ||
		!bytes.Equal(btcutil.Hash160(pub), pkScript[2:]) {
		return errPubNotBoundToScript
	}
	return nil
}

var errPubNotBoundToScript = errors.New("pubkey not bound to pkScript")

func newTestTssService(t *testing.T) *tssService {
	t.Helper()
	priv, _ := btcec.PrivKeyFromBytes([]byte{
		0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00,
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
	})
	pub := priv.PubKey()
	params := &chaincfg.RegressionNetParams
	addr, err := btcutil.NewAddressWitnessPubKeyHash(btcutil.Hash160(pub.SerializeCompressed()), params)
	require.NoError(t, err)
	pkScript, err := txscript.PayToAddrScript(addr)
	require.NoError(t, err)
	return &tssService{tssPublicKey: pub, tssAddress: addr, pkScript: pkScript}
}

// TestBuildCommitDKGPayload_acceptedByChainGate 桥组装的 BTC CommitDKG 载荷必须过链上公钥判据。
func TestBuildCommitDKGPayload_acceptedByChainGate(t *testing.T) {
	svc := newTestTssService(t)

	payload := svc.buildCommitDKGPayload(rtypes.BTCSymbol)
	require.NotNil(t, payload, "本地 DKG 结果完整时必须给出载荷")
	assert.Equal(t, rtypes.BTCSymbol, payload.GetAssetSymbol())
	assert.Equal(t, 33, len(payload.GetPubkey()), "必须是 33 字节压缩公钥")
	assert.Equal(t, svc.tssPublicKey.SerializeCompressed(), payload.GetPubkey())
	require.NoError(t, chainCommitDKGPubkeyGate(payload), "带 pubkey 的载荷必须被链上接受")

	// 反向：不带 pubkey（改动前的形态）被链上拒 —— 这正是 E2E 里"卡在提交 CommitDKG"的原因。
	withoutPubkey := &rtypes.CommitDKG{
		AssetSymbol: payload.GetAssetSymbol(),
		DkgAddress:  payload.GetDkgAddress(),
		PkScript:    payload.GetPkScript(),
	}
	assert.ErrorIs(t, chainCommitDKGPubkeyGate(withoutPubkey), rtypes.ErrInvalidTssPubKey)

	// 反向：非压缩公钥也不行（同一把钥的另一种编码会派生出另一个充值地址）。
	uncompressed := svc.tssPublicKey.SerializeUncompressed()
	assert.Equal(t, 65, len(uncompressed))
	assert.ErrorIs(t, chainCommitDKGPubkeyGate(&rtypes.CommitDKG{
		AssetSymbol: payload.GetAssetSymbol(),
		DkgAddress:  payload.GetDkgAddress(),
		PkScript:    payload.GetPkScript(),
		Pubkey:      uncompressed,
	}), rtypes.ErrInvalidTssPubKey)

	// 反向：公钥与 pkScript 不配套（另一个 DKG 结果）也被拒。
	otherPriv, _ := btcec.PrivKeyFromBytes([]byte{0x42})
	assert.Error(t, chainCommitDKGPubkeyGate(&rtypes.CommitDKG{
		AssetSymbol: payload.GetAssetSymbol(),
		DkgAddress:  payload.GetDkgAddress(),
		PkScript:    payload.GetPkScript(),
		Pubkey:      otherPriv.PubKey().SerializeCompressed(),
	}))
}

// TestBuildCommitDKGPayload_incompleteDKG 本地 DKG 结果不完整时不提交（宁可明说不提交，
// 也不提交一份链上必拒的载荷 —— 那会退化成无限重试）。
func TestBuildCommitDKGPayload_incompleteDKG(t *testing.T) {
	svc := newTestTssService(t)
	svc.tssAddress = nil
	assert.Nil(t, svc.buildCommitDKGPayload(rtypes.BTCSymbol))

	svc2 := &tssService{}
	assert.Nil(t, svc2.buildCommitDKGPayload(rtypes.BTCSymbol))
	// nil 载荷直接返回（不 panic、不空转）。
	svc2.commitDKGToChainWith(context.Background(), nil, nil, nil)
}

/*
 * 阶段 2：提交/核对循环的三种判据。
 *
 * 这三条是 DKG 阶段死锁的根因所在：判据必须落在**链上记录内容**上。
 * 曾经"链上无记录"（执行器回空记录 + nil error）与"链上是另一把钥"共用同一个 err == nil 分支，
 * 于是全新链上永远走不到提交分支 —— 表现为 DKG 阶段死锁（不是慢），E2E 里四个 para 只有一条
 * "carries a different tss pubkey" 日志、main 链上 CrossChainInfo 一直为空。
 */

// fakeChain 是"主链 CrossChainInfo"的替身：CommitDKG 被接受后记录在案（模拟 Exec_CommitDKG）。
type fakeChain struct {
	mu      sync.Mutex
	infos   map[string]*rtypes.CrossChainInfo
	submits int
}

func newFakeChain() *fakeChain {
	return &fakeChain{infos: make(map[string]*rtypes.CrossChainInfo)}
}

// query 复刻执行器 Query_GetCrossChainInfo 的真实契约：symbol 不存在时返回**空记录 + nil error**。
func (c *fakeChain) query(symbol string) (*rtypes.CrossChainInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if info, ok := c.infos[symbol]; ok {
		return info, nil
	}
	return &rtypes.CrossChainInfo{}, nil
}

// submit 复刻执行器 CheckTx/Exec 的口径：同一 symbol 已有记录 ⇒ ErrDuplicateDKGCommit。
func (c *fakeChain) submit(_ string, _ string, payload *rtypes.CommitDKG) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	symbol := payload.GetAssetSymbol()
	if _, ok := c.infos[symbol]; ok {
		return "", errors.New("duplicate dkg commit")
	}
	c.submits++
	c.infos[symbol] = &rtypes.CrossChainInfo{
		AssetSymbol: symbol,
		TssAddress:  payload.GetDkgAddress(),
		PkScript:    payload.GetPkScript(),
		Pubkey:      payload.GetPubkey(),
	}
	return "fake-tx-hash", nil
}

func (c *fakeChain) submitCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.submits
}

func (c *fakeChain) put(info *rtypes.CrossChainInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.infos[info.GetAssetSymbol()] = info
}

func (c *fakeChain) pubkey(symbol string) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.infos[symbol].GetPubkey()
}

// TestCommitDKGToChain_submitsWhenChainHasNoRecord 链上还没有该 symbol 的记录（执行器回空记录 + nil error）
// ⇒ 必须判定为"未提交"并**真的提交**，链上出现同一把钥后返回。
// 反向（修好前）：空记录被当成"链上是另一把钥"⇒ 永不提交 —— 用 ctx 超时兜住，避免测试挂死。
func TestCommitDKGToChain_submitsWhenChainHasNoRecord(t *testing.T) {
	svc := newTestTssService(t)
	chain := newFakeChain()

	ctx, cancel := context.WithTimeout(context.Background(), 4*commitDKGVerifyInterval)
	defer cancel()
	svc.commitDKGToChainWith(ctx, svc.buildCommitDKGPayload(rtypes.BTCSymbol), chain.query, chain.submit)

	require.Equal(t, 1, chain.submitCount(), "链上无记录时必须真的提交（且只提交一次）")
	require.Equal(t, svc.tssPublicKey.SerializeCompressed(), chain.pubkey(rtypes.BTCSymbol),
		"链上记录的必须是本地这把群公钥")
}

// TestCommitDKGToChain_doesNotSubmitWhenChainHasAnotherKey 链上已有该 symbol、但群公钥是另一把：
// 提交必然被 ErrDuplicateDKGCommit 拒（同一 symbol 只能提交一次），因此不提交，只报 stall。
func TestCommitDKGToChain_doesNotSubmitWhenChainHasAnotherKey(t *testing.T) {
	svc := newTestTssService(t)
	chain := newFakeChain()
	_, otherPub := testPubkey(t)
	chain.put(&rtypes.CrossChainInfo{
		AssetSymbol: rtypes.BTCSymbol, TssAddress: "bcrt1qchain", Pubkey: otherPub})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 第一轮核对就能看出"链上是另一把钥"，取消 ctx 让循环退出（生产上这个形态会一直报 stall，
	// 由运维介入 —— 不可自愈）。
	query := func(symbol string) (*rtypes.CrossChainInfo, error) {
		info, err := chain.query(symbol)
		cancel()
		return info, err
	}
	svc.commitDKGToChainWith(ctx, svc.buildCommitDKGPayload(rtypes.BTCSymbol), query, chain.submit)

	require.Zero(t, chain.submitCount(), "链上是另一把钥时不该空转提交（必被拒为 duplicate）")
	require.Equal(t, otherPub, chain.pubkey(rtypes.BTCSymbol), "链上记录不该被本地载荷覆盖")
}

// TestCommitDKGToChain_skipsSubmitWhenChainAlreadyHasSameKey 链上已有同一把钥 ⇒ 直接确认，不重复提交。
func TestCommitDKGToChain_skipsSubmitWhenChainAlreadyHasSameKey(t *testing.T) {
	svc := newTestTssService(t)
	chain := newFakeChain()
	chain.put(&rtypes.CrossChainInfo{
		AssetSymbol: rtypes.BTCSymbol, TssAddress: svc.tssAddress.EncodeAddress(),
		PkScript: svc.pkScript, Pubkey: svc.tssPublicKey.SerializeCompressed()})

	svc.commitDKGToChainWith(context.Background(), svc.buildCommitDKGPayload(rtypes.BTCSymbol),
		chain.query, chain.submit)

	require.Zero(t, chain.submitCount(), "链上已有同一把钥，不该重复提交")
}

// TestEnsureDKGOnChain_commitsEverySymbol 从 DB 载入 DKG 之后的核对/提交（Bug：载入即 return
// ⇒ 链上永远没有记录，E2E 表现为 wait_auto_dkg_commit 超时）。
// 覆盖 BTC + RGB20 两个 symbol：都必须被提交上去，且链上记录的 pubkey 是本地这把。
func TestEnsureDKGOnChain_commitsEverySymbol(t *testing.T) {
	svc := newTestTssService(t)
	chain := newFakeChain()
	symbols := []string{rtypes.BTCSymbol, "RGB20_USDT"}

	ctx, cancel := context.WithTimeout(context.Background(), 8*commitDKGVerifyInterval)
	defer cancel()
	svc.ensureDKGOnChainWith(ctx, symbols, chain.query, chain.submit)

	require.Equal(t, len(symbols), chain.submitCount(), "每个 symbol 都要提交（含 RGB20）")
	for _, symbol := range symbols {
		require.Equal(t, svc.tssPublicKey.SerializeCompressed(), chain.pubkey(symbol),
			"链上 %s 的群公钥必须是本地这把", symbol)
	}
}

// TestQueryCrossChainInfoBounded_emptyRecordMeansNotOnChain 空记录必须在查询层就被翻译成
// errCrossChainInfoNotOnChain —— 只按 error 判定的实现会把"链上还没有"漏进"另一把钥"分支。
func TestQueryCrossChainInfoBounded_emptyRecordMeansNotOnChain(t *testing.T) {
	n := &neutrinoClient{ctx: context.Background()}

	n.mainChainGrpc = newChainInfoMock(nil)
	_, err := n.queryCrossChainInfoBounded(rtypes.BTCSymbol)
	require.ErrorIs(t, err, errCrossChainInfoNotOnChain,
		"执行器对不存在的 symbol 回的是空记录 + nil error，必须判成'链上还没有'")

	localPub, _ := testPubkey(t)
	n.mainChainGrpc = newChainInfoMock(map[string]*rtypes.CrossChainInfo{
		rtypes.BTCSymbol: {AssetSymbol: rtypes.BTCSymbol, TssAddress: "bcrt1qlocal",
			Pubkey: localPub.SerializeCompressed()},
	})
	info, err := n.queryCrossChainInfoBounded(rtypes.BTCSymbol)
	require.NoError(t, err)
	require.Equal(t, "bcrt1qlocal", info.GetTssAddress())
	require.Equal(t, localPub.SerializeCompressed(), info.GetPubkey())
}
