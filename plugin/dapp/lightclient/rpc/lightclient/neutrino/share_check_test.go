package neutrino

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/33cn/chain33/system/crypto/tss/cggmp"
	"github.com/33cn/chain33/types"
	typesmocks "github.com/33cn/chain33/types/mocks"
	"github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// ----------------------------------------------------------------------------
// 测试替身
// ----------------------------------------------------------------------------

// testPubkey 生成一对测试密钥，返回公钥与压缩编码。
func testPubkey(t *testing.T) (*btcec.PublicKey, []byte) {
	t.Helper()
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pub := priv.PubKey()
	return pub, pub.SerializeCompressed()
}

// p2wpkhScript 由公钥构造 P2WPKH pkScript（= 链上 CrossChainInfo.PkScript 的形态）。
func p2wpkhScript(pub *btcec.PublicKey) []byte {
	return append([]byte{txscript.OP_0, 0x14}, btcutil.Hash160(pub.SerializeCompressed())...)
}

// dkgResultForPubkey 由公钥坐标构造 CGGMP DKGResult（tss.go saveDKGToDB 落盘的正是这个结构，
// JSON 编码 —— 切换 CGGMP 后落盘格式从 proto 变成 JSON，这里必须跟着同一口径，
// 否则测的是"能读一份实际不存在的格式"）。
func dkgResultForPubkey(pub *btcec.PublicKey) *cggmp.DKGResult {
	x := pub.X().Bytes()
	y := pub.Y().Bytes()
	return &cggmp.DKGResult{PubX: x, PubY: y}
}

// putTssRecord 往测试 DB 的 tss bucket 写一条记录（同一份编码口径：JSON）。
func putTssRecord(t *testing.T, db walletdb.DB, key string, value interface{}) {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		bucket, err := tx.CreateTopLevelBucket([]byte(tssBucketName))
		if err != nil {
			return err
		}
		return bucket.Put([]byte(key), data)
	}))
}

// newChainInfoMock 返回一个主链 grpc 替身：按 symbol 查 CrossChainInfo，未登记即"链上还没有"。
//
// 未登记时必须复刻执行器的**真实应答**：IsOk=true + **空结构体**（不是 IsOk=false）——
// executor/query.go 的 Query_GetCrossChainInfo 对不存在的 symbol 就是回空记录 + nil error，
// 该契约被 executor/query_test.go 钉住。早先这个替身回 IsOk=false，与真实契约不符：
// 它让"链上还没有"看起来是个查询错误，恰好掩盖了"空记录被误判成链上是另一把钥"的死锁
// （见 client.go 的 crossChainInfoAbsentOnChain）。
func newChainInfoMock(infos map[string]*rtypes.CrossChainInfo) *typesmocks.Chain33Client {
	m := &typesmocks.Chain33Client{}
	m.On("QueryChain", mock.Anything, mock.Anything).Return(
		func(_ context.Context, in *types.ChainExecutor, _ ...grpc.CallOption) (*types.Reply, error) {
			req := &types.ReqString{}
			if err := types.Decode(in.GetParam(), req); err != nil {
				return &types.Reply{IsOk: false, Msg: []byte(err.Error())}, nil
			}
			info := infos[req.GetData()]
			if info == nil {
				// 链上没有该 symbol：空记录 + nil error（执行器的既有契约）。
				info = &rtypes.CrossChainInfo{}
			}
			return &types.Reply{IsOk: true, Msg: types.Encode(info)}, nil
		}, nil)
	return m
}

// newClientWithLocalDKG 造一个"本地已有 DKG 结果"的中继客户端（DB 里写入 dkgResult）。
func newClientWithLocalDKG(t *testing.T, dkgResult *cggmp.DKGResult) *neutrinoClient {
	t.Helper()
	dir := t.TempDir()
	_, db, err := openWalletDB(dir, "share_check.db")
	if err != nil {
		t.Skipf("walletdb/bdb unavailable: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	putTssRecord(t, db, dkgResultKey, dkgResult)

	n := &neutrinoClient{ctx: context.Background()}
	n.neutrinoCfg.Database = db
	return n
}

// ----------------------------------------------------------------------------
// 纯比对逻辑
// ----------------------------------------------------------------------------

// Test_checkShareAgainstChainInfos 覆盖判定的三种情形：
// 一致、不一致（链上有 pubkey / 只有 pkScript 两条判据）、以及各种"不判定"。
func Test_checkShareAgainstChainInfos(t *testing.T) {
	localPub, localPubBytes := testPubkey(t)
	_, otherPubBytes := testPubkey(t)
	otherPubScript := p2wpkhScript(otherPubFromBytes(t, otherPubBytes))

	tests := []struct {
		name      string
		symbols   []string
		infos     map[string]*rtypes.CrossChainInfo
		wantCount int
	}{
		{
			name:    "rgb20 pubkey matches",
			symbols: []string{rtypes.BTCSymbol},
			infos: map[string]*rtypes.CrossChainInfo{
				rtypes.BTCSymbol: {TssAddress: "bcrt1qlocal", Pubkey: localPubBytes,
					PkScript: p2wpkhScript(localPub)},
			},
			wantCount: 0,
		},
		{
			name:    "btc pkscript matches (no chain pubkey)",
			symbols: []string{rtypes.BTCSymbol},
			infos: map[string]*rtypes.CrossChainInfo{
				rtypes.BTCSymbol: {TssAddress: "bcrt1qlocal", PkScript: p2wpkhScript(localPub)},
			},
			wantCount: 0,
		},
		{
			name:    "rgb20 pubkey mismatch",
			symbols: []string{"RGB20_USDT"},
			infos: map[string]*rtypes.CrossChainInfo{
				"RGB20_USDT": {TssAddress: "bcrt1qchain", Pubkey: otherPubBytes},
			},
			wantCount: 1,
		},
		{
			name:    "btc pkscript mismatch",
			symbols: []string{rtypes.BTCSymbol},
			infos: map[string]*rtypes.CrossChainInfo{
				rtypes.BTCSymbol: {TssAddress: "bcrt1qchain", PkScript: otherPubScript},
			},
			wantCount: 1,
		},
		{
			name:      "chain info absent -> undecided",
			symbols:   []string{rtypes.BTCSymbol, "RGB20_USDT"},
			infos:     map[string]*rtypes.CrossChainInfo{},
			wantCount: 0,
		},
		{
			name:    "tss address empty -> not committed yet",
			symbols: []string{"RGB20_USDT"},
			infos: map[string]*rtypes.CrossChainInfo{
				"RGB20_USDT": {Pubkey: otherPubBytes},
			},
			wantCount: 0,
		},
		{
			name:    "no comparable on-chain field -> undecided",
			symbols: []string{"RGB20_USDT"},
			infos: map[string]*rtypes.CrossChainInfo{
				"RGB20_USDT": {TssAddress: "bcrt1qlocal", PkScript: []byte{txscript.OP_TRUE}},
			},
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query := func(symbol string) (*rtypes.CrossChainInfo, error) {
				return tt.infos[symbol], nil
			}
			mismatches := checkShareAgainstChainInfos(localPub, tt.symbols, query)
			require.Len(t, mismatches, tt.wantCount)
			for _, m := range mismatches {
				require.Contains(t, m, "localGroup")
				require.Contains(t, m, "chain")
			}
		})
	}
}

// Test_checkShareAgainstChainInfos_queryError 查询不可用（主链 hang / 插件未启用）时不判定。
func Test_checkShareAgainstChainInfos_queryError(t *testing.T) {
	localPub, _ := testPubkey(t)
	query := func(string) (*rtypes.CrossChainInfo, error) {
		return nil, fmt.Errorf("query chain timeout")
	}
	require.Empty(t, checkShareAgainstChainInfos(localPub, []string{rtypes.BTCSymbol, "RGB20_USDT"}, query))
}

// ----------------------------------------------------------------------------
// 本地 DKG 结果读取
// ----------------------------------------------------------------------------

// Test_loadTssGroupPubKeyFromDB 本地还没有 DKG 结果 → (nil, nil)（情形②）；写入后可读回同一个组公钥。
func Test_loadTssGroupPubKeyFromDB(t *testing.T) {
	dir := t.TempDir()
	_, db, err := openWalletDB(dir, "empty.db")
	if err != nil {
		t.Skipf("walletdb/bdb unavailable: %v", err)
	}
	defer db.Close()

	// 无 DB（未初始化）：不判定，不 panic。
	require.Nil(t, func() *btcec.PublicKey {
		pub, err := (&neutrinoClient{}).loadTssGroupPubKeyFromDB()
		require.NoError(t, err)
		return pub
	}())

	n := &neutrinoClient{}
	n.neutrinoCfg.Database = db

	pub, err := n.loadTssGroupPubKeyFromDB()
	require.NoError(t, err)
	require.Nil(t, pub, "空 DB 应当读不到 DKG 结果")

	want, _ := testPubkey(t)
	putTssRecord(t, db, dkgResultKey, dkgResultForPubkey(want))

	got, err := n.loadTssGroupPubKeyFromDB()
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, want.SerializeCompressed(), got.SerializeCompressed())
}

// Test_checkTssShareAgainstChain_skipsWithoutLocalDKG 情形②：本地没有 DKG 结果时自检直接放行，
// 不查链、不报错（首次启动 / DKG 还没跑）。
func Test_checkTssShareAgainstChain_skipsWithoutLocalDKG(t *testing.T) {
	n := &neutrinoClient{}
	n.cfg.Rgb20.Contracts = []rgb20Contract{{Symbol: "RGB20_USDT"}}
	require.NoError(t, n.checkTssShareAgainstChain())
}

// ----------------------------------------------------------------------------
// 自检入口（含 symbol 集合与错误包装）
// ----------------------------------------------------------------------------

// Test_checkTssShareAgainstChain 自检入口：一致放行、不一致返回带逃生阀提示的错误、
// 链上还没有 CrossChainInfo 时不判定。
func Test_checkTssShareAgainstChain(t *testing.T) {
	localPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	localPub := localPriv.PubKey()
	_, otherPubBytes := testPubkey(t)

	n := newClientWithLocalDKG(t, dkgResultForPubkey(localPub))
	n.cfg.Rgb20.Contracts = []rgb20Contract{{Symbol: "RGB20_USDT"}}

	// 一致：BTC 按 pkScript 比对，RGB20 按 pubkey 比对。
	n.mainChainGrpc = newChainInfoMock(map[string]*rtypes.CrossChainInfo{
		rtypes.BTCSymbol: {
			AssetSymbol: rtypes.BTCSymbol, TssAddress: "bcrt1qlocal", PkScript: p2wpkhScript(localPub)},
		"RGB20_USDT": {
			AssetSymbol: "RGB20_USDT", TssAddress: "bcrt1qlocal", Pubkey: localPub.SerializeCompressed()},
	})
	require.NoError(t, n.checkTssShareAgainstChain())

	// 不一致：链上 RGB20 的组公钥是另一把（share 丢失后 re-DKG / 换过钥 / 从别的环境恢复过数据）。
	n.mainChainGrpc = newChainInfoMock(map[string]*rtypes.CrossChainInfo{
		rtypes.BTCSymbol: {
			AssetSymbol: rtypes.BTCSymbol, TssAddress: "bcrt1qlocal", PkScript: p2wpkhScript(localPub)},
		"RGB20_USDT": {
			AssetSymbol: "RGB20_USDT", TssAddress: "bcrt1qchain", Pubkey: otherPubBytes},
	})
	err = n.checkTssShareAgainstChain()
	require.Error(t, err)
	require.IsType(t, &errTssShareMismatch{}, err)
	require.ErrorContains(t, err, "RGB20_USDT")
	require.ErrorContains(t, err, "localGroupPubkey")
	// 错误信息里必须带处置办法与逃生阀，否则运维照不出来。
	require.ErrorContains(t, err, "duplicate")
	require.ErrorContains(t, err, "tss.allowShareMismatch=true")

	// 不一致：BTC 的 pkScript 对不上（链上无 pubkey，退化为 hash160 比对）。
	n.mainChainGrpc = newChainInfoMock(map[string]*rtypes.CrossChainInfo{
		rtypes.BTCSymbol: {
			AssetSymbol: rtypes.BTCSymbol, TssAddress: "bcrt1qchain",
			PkScript: p2wpkhScript(otherPubFromBytes(t, otherPubBytes))},
	})
	err = n.checkTssShareAgainstChain()
	require.ErrorContains(t, err, "BTC")
	require.ErrorContains(t, err, "localGroupPubkeyHash160")

	// 情形①：链上还没有该 symbol 的 CrossChainInfo（DKG 尚未 commit）→ 不判定。
	n.mainChainGrpc = newChainInfoMock(nil)
	require.NoError(t, n.checkTssShareAgainstChain())
}

// Test_shareCheckSymbols 自检覆盖 BTC + 配置里的 RGB20 合约，去重且跳过空 symbol。
func Test_shareCheckSymbols(t *testing.T) {
	n := &neutrinoClient{}
	require.Equal(t, []string{rtypes.BTCSymbol}, n.shareCheckSymbols())

	n.cfg.Rgb20.Contracts = []rgb20Contract{
		{Symbol: "RGB20_USDT"}, {Symbol: ""}, {Symbol: "RGB20_USDT"}, {Symbol: "RGB20_XXX"},
	}
	require.Equal(t, []string{rtypes.BTCSymbol, "RGB20_USDT", "RGB20_XXX"}, n.shareCheckSymbols())
}

// Test_tssAllowShareMismatchConfig 逃生阀从 TOML 解析，且默认关闭（fail-closed）。
func Test_tssAllowShareMismatchConfig(t *testing.T) {
	const cfgTpl = `
[rpc.sub.light]
clients=["neutrino"]
commitAddr="14KEKbYtKKQm4wMthSK9J4La4nAiidGozt"

[rpc.sub.light.neutrino]
netName="regtest"

[rpc.sub.light.neutrino.tss]
peers=["peer1"]
threshold=2
rank=0
%s
`
	parse := func(extra string) config {
		cfg := types.NewChain33Config(types.MergeCfg(types.GetDefaultCfgstring(), fmt.Sprintf(cfgTpl, extra)))
		light := &lightclient.Config{}
		types.MustDecode(cfg.GetSubConfig().RPC["light"], light)
		sub, err := json.Marshal(light.Neutrino)
		require.NoError(t, err)
		subCfg := &config{}
		types.MustDecode(sub, subCfg)
		return *subCfg
	}

	require.False(t, parse("").Tss.AllowShareMismatch, "逃生阀默认必须关闭")
	require.False(t, parse("allowShareMismatch=false").Tss.AllowShareMismatch)
	require.True(t, parse("allowShareMismatch=true").Tss.AllowShareMismatch)
	require.Equal(t, uint32(2), parse("").Tss.Threshold)
}

// otherPubFromBytes 把压缩公钥还原成 *btcec.PublicKey（构造"另一把钥"的 pkScript 用）。
func otherPubFromBytes(t *testing.T, pubBytes []byte) *btcec.PublicKey {
	t.Helper()
	pub, err := btcec.ParsePubKey(pubBytes)
	require.NoError(t, err)
	return pub
}
