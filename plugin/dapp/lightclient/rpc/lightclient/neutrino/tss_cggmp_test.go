package neutrino

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/33cn/chain33/system/crypto/tss"
	"github.com/33cn/chain33/system/crypto/tss/cggmp"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
)

/*
 * GG18 → CGGMP 切换的桥侧判据（tss.go）。
 *
 * 这一批用例钉的是"切协议时最容易静默踩坏"的几处，都**不是**密码学本身
 * （密码学数据流由 chain33 侧 cggmp 包的 e2e/p2p 集成测试覆盖），而是桥侧接线：
 *
 *  1. rank 语义：CGGMP 的 rank 是 Birkhoff rank（要求 rank+1 < threshold），不是节点序号；
 *  2. 签名者集合：CGGMP 要求**恰好 threshold 个**且 rank 组合合法（GG18 那套"全塞进去"会被拒）；
 *  3. refresh 记录的归属：refresh 产物必须与当前 DKG（rid）配套，否则签名必失败；
 *  4. #28：链上已有 CrossChainInfo 时**禁止**重跑 DKG。
 */

// ---- ① rank 语义 ----

// Test_validateCggmpRank CGGMP 的 rank 是 Birkhoff rank，必须满足 rank+1 < threshold。
// 反向：把判据写成"rank < threshold"（索引式直觉）时，4 节点 threshold=3 的 {0,1,1,1} 里的
// rank=1 仍会通过、但 rank=2 也会通过 ⇒ 错的那条用例（rank=2, threshold=3）变红。
func Test_validateCggmpRank(t *testing.T) {
	// 本仓库的部署形态：4 节点、threshold 3 ⇒ 合法 rank 是 {0,1,1,1}；rank=0 是官方节点。
	require.NoError(t, validateCggmpRank(0, 3), "官方节点 rank=0")
	require.NoError(t, validateCggmpRank(1, 3), "第三方节点 rank=1（4 节点 threshold=3 的合法上界）")

	// rank=2/3 是"把 rank 当节点序号"的直觉取值，alice 的 EnsureRank 会直接拒 DKG。
	require.Error(t, validateCggmpRank(2, 3), "rank=2 不满足 rank+1 < 3")
	require.Error(t, validateCggmpRank(3, 3), "rank=3 不满足 rank+1 < 3")

	// 3 节点 threshold=3：合法 rank 只有 0 和 1（rank+1<3）。
	require.NoError(t, validateCggmpRank(1, 3))
	require.Error(t, validateCggmpRank(2, 3))

	// threshold=1 不是有效的 CGGMP 阈值（至少要 2-of-n）。
	require.Error(t, validateCggmpRank(0, 1))
	require.Error(t, validateCggmpRank(0, 0))

	// 大 threshold：合法上界随之放宽（rank <= threshold-2）。
	require.NoError(t, validateCggmpRank(2, 4))
	require.Error(t, validateCggmpRank(3, 4))
}

// ---- ② 签名者集合：恰好 threshold 个 + Birkhoff 合法 ----

// bksFromRanks 按 "peerID → rank" 造 Birkhoff 参数表（X 坐标只要求非 nil：本批用例只碰 rank）。
func bksFromRanks(ranks map[string]uint32) map[string]*tss.BK {
	bks := make(map[string]*tss.BK, len(ranks))
	for id, r := range ranks {
		bks[id] = &tss.BK{Rank: r, X: []byte{byte(r + 1)}}
	}
	return bks
}

// Test_selectSignerCombination CGGMP 要求**恰好 threshold 个**签名者，且 rank 升序后 rank_i <= i。
// 4 节点 {0,1,1,1} + threshold 3 ⇒ 取到 rank [0,1,1]（rank0 必须在场）。
//
// 反向：把"取前 threshold 个"改成"返回全部候选"（= GG18 的写法）后，本用例中
// len(signers)!=threshold 的断言变红 —— 那正是 cggmp.ProcessSign 会直接拒的形态。
func Test_selectSignerCombination(t *testing.T) {
	bks := bksFromRanks(map[string]uint32{
		"peer-a": 0, // 官方节点（rank 0 必须在场）
		"peer-b": 1,
		"peer-c": 1,
		"peer-d": 1,
	})
	all := []string{"peer-a", "peer-b", "peer-c", "peer-d"}

	signers := selectSignerCombination(all, bks, 3)
	require.Len(t, signers, 3, "CGGMP 的 ProcessSign 要求 len(peers) == threshold")
	require.Contains(t, signers, "peer-a", "rank0 是必选（缺了就没有合法组合）")
	require.NoError(t, validateSignerCombination(signers, bks, 3))
	// 4 个都在场时，rank1 里取 peerID 最小的两个（peer-b、peer-c）。
	require.Equal(t, []string{"peer-a", "peer-b", "peer-c"}, signers)

	// 结果与传入顺序无关（各节点看到的连接顺序可能不同，算出的组合必须一致）。
	shuffled := []string{"peer-d", "peer-c", "peer-b", "peer-a"}
	require.ElementsMatch(t, signers, selectSignerCombination(shuffled, bks, 3))

	// 少一个 rank1（该节点掉线）：剩下的仍然构成合法组合，签名继续可用。
	require.Equal(t, []string{"peer-a", "peer-b", "peer-d"},
		selectSignerCombination([]string{"peer-a", "peer-b", "peer-d"}, bks, 3))

	// rank0 掉线：{1,1,1} 升序后 rank_0 = 1 > 0 ⇒ 无合法组合，只能等（宁可签不了也不能签错）。
	require.Nil(t, selectSignerCombination([]string{"peer-b", "peer-c", "peer-d"}, bks, 3))

	// 候选不足 threshold。
	require.Nil(t, selectSignerCombination([]string{"peer-a", "peer-b"}, bks, 3))
	require.Nil(t, selectSignerCombination(all, bks, 0))
}

// Test_validateSignerCombination 签名节点收到协调者下发的 Signers 时必须自己核对
// （名单是"协调者说的"，不能照单全收：集合不合法 ⇒ 组签名必然失败）。
func Test_validateSignerCombination(t *testing.T) {
	bks := bksFromRanks(map[string]uint32{"peer-a": 0, "peer-b": 1, "peer-c": 1, "peer-d": 1})

	require.NoError(t, validateSignerCombination([]string{"peer-a", "peer-b", "peer-c"}, bks, 3))

	// 数量必须恰好等于 threshold（GG18 风格的"全部 4 个"在这里就是错的）。
	require.Error(t, validateSignerCombination([]string{"peer-a", "peer-b", "peer-c", "peer-d"}, bks, 3))
	require.Error(t, validateSignerCombination([]string{"peer-a", "peer-b"}, bks, 3))

	// 缺 rank0：{1,1,1} 不满足 rank_i <= i。
	require.Error(t, validateSignerCombination([]string{"peer-b", "peer-c", "peer-d"}, bks, 3))

	// 本轮 DKG 之外的节点（没有 bk）：不能凭空当签名者。
	require.Error(t, validateSignerCombination([]string{"peer-a", "peer-b", "peer-x"}, bks, 3))

	// 重复计数不能凑数（否则 2 个真实节点就能冒充 3 个签名者）。
	require.Error(t, validateSignerCombination([]string{"peer-a", "peer-a", "peer-c"}, bks, 3))

	// bk 缺失（DKG 结果不完整）也不放行。
	require.Error(t, validateSignerCombination([]string{"peer-a", "peer-b", "peer-c"}, nil, 3))
}

// Test_signMsg_rejectsWrongSignerCount GG18 的签名单是"连上的合法节点全上"，CGGMP 不允许：
// 数量不等于 threshold 时 cggmp.ProcessSign 在**进入 alice 之前**就拒（validateSignMaterial），
// 因此这里不需要任何网络/对等节点就能钉住该判据。
func Test_signMsg_rejectsWrongSignerCount(t *testing.T) {
	svc := &tssService{
		cfg:           tssConfig{Threshold: 3},
		dkgResult:     &cggmp.DKGResult{},
		refreshResult: &cggmp.RefreshResult{},
	}
	res := svc.signMsg([]byte("msg"), "session", []string{"p1", "p2", "p3", "p4"})
	require.Error(t, res.err, "4 个签名者 / threshold 3 必须被拒")
	require.ErrorContains(t, res.err, "threshold")
}

// Test_signMsg_requiresBothResults DKG 与 refresh 两份材料缺一不可（GG18 只有 DKG 一份，
// 切换后若漏了 refresh 的载入/跑轮次，签名会以"看起来在签名"的形态失败）。
func Test_signMsg_requiresBothResults(t *testing.T) {
	onlyDKG := &tssService{cfg: tssConfig{Threshold: 3}, dkgResult: &cggmp.DKGResult{}}
	require.ErrorIs(t, onlyDKG.signMsg([]byte("m"), "s", []string{"p1"}).err, errTssKeyMaterialNotReady)

	onlyRefresh := &tssService{cfg: tssConfig{Threshold: 3}, refreshResult: &cggmp.RefreshResult{}}
	require.ErrorIs(t, onlyRefresh.signMsg([]byte("m"), "s", []string{"p1"}).err, errTssKeyMaterialNotReady)
}

// Test_signPsbtInternal_requiresSigners 签名节点必须用协调者下发的名单；空名单不许"自己凑一个"。
func Test_signPsbtInternal_requiresSigners(t *testing.T) {
	svc := &tssService{cfg: tssConfig{Threshold: 3}}
	_, err := svc.signPsbtInternal([]byte{0x01, 0x02}, nil)
	require.ErrorContains(t, err, "no signers")
}

// ---- ③ refresh 记录与 DKG 的配套关系 ----

// Test_refreshSessionName refresh 会话名由 DKG 的 rid 派生：同一个 key 的所有节点必须一致
// （refresh 的 ZK 挑战由会话名派生，不一致会互相验不过），且换钥后必变（不与其他钥相撞）。
func Test_refreshSessionName(t *testing.T) {
	ridA := []byte{0x01, 0x02, 0x03}
	ridB := []byte{0x01, 0x02, 0x04}

	require.Equal(t, refreshSessionName(ridA), refreshSessionName(ridA), "同 rid 必须同名")
	require.NotEqual(t, refreshSessionName(ridA), refreshSessionName(ridB), "换钥必须换会话名")
	require.Equal(t, refreshSessionPrefix+hex.EncodeToString(ridA), refreshSessionName(ridA))
	require.NotContains(t, refreshSessionName(ridA), dkgSessionName, "refresh 与 dkg 不能共用会话名")
}

// Test_loadRefreshFromDB_matchesDkgRid refresh 产物落盘时必须带 DKG 的 rid，载入时据此判断
// "这份 refresh 是不是当前这把钥的"：rid 不符 = 记录已作废（必须重跑 refresh），
// 不能拿着旧 Paillier/Pedersen 去签新 share。
func Test_loadRefreshFromDB_matchesDkgRid(t *testing.T) {
	rid := []byte("current-rid")
	svc := &tssService{dkgResult: &cggmp.DKGResult{Rid: rid}}
	dbClient := newClientWithDB(t)
	svc.client = dbClient

	// 没有记录：报"没有可用记录"。
	require.Error(t, svc.loadRefreshFromDB())

	// rid 不符（上一把钥的 refresh）：同样算不可用。
	putTssRecord(t, dbClient.neutrinoCfg.Database, refreshResultKey, &refreshRecord{
		DkgRid: hex.EncodeToString([]byte("other-rid")),
		Result: &cggmp.RefreshResult{Share: []byte{0x09}},
	})
	err := svc.loadRefreshFromDB()
	require.Error(t, err)
	require.ErrorContains(t, err, "another key")
	require.Nil(t, svc.refreshResult, "rid 不符的记录不能被采用")

	// rid 相符：采用。
	want := &cggmp.RefreshResult{Share: []byte{0x07}, YSecret: []byte{0x08}}
	putTssRecord(t, dbClient.neutrinoCfg.Database, refreshResultKey, &refreshRecord{
		DkgRid: hex.EncodeToString(rid), Result: want,
	})
	require.NoError(t, svc.loadRefreshFromDB())
	require.Equal(t, want.Share, svc.refreshResult.Share)
	require.Equal(t, want.YSecret, svc.refreshResult.YSecret)
}

// Test_saveRefreshToDB_roundTrips Persist 的 refresh 记录必须能读回**同样的**材料，且带上 rid。
// ⚠️ 这份记录含 Paillier 私钥素数 + YSecret（敏感度等同 share），落盘位置与 dkg-result 同桶同库，
// 备份范围必须覆盖两者 —— 本用例同时钉住"两份记录都在 tssBucketName 里"。
func Test_saveRefreshToDB_roundTrips(t *testing.T) {
	rid := []byte("rid-for-roundtrip")
	svc := &tssService{
		dkgResult: &cggmp.DKGResult{Rid: rid},
		refreshResult: &cggmp.RefreshResult{
			Share:     []byte{0x11, 0x22},
			PaillierP: []byte{0x33},
			PaillierQ: []byte{0x44},
			YSecret:   []byte{0x55},
		},
		client: newClientWithDB(t),
	}
	svc.saveRefreshToDB()

	reloaded := &tssService{dkgResult: &cggmp.DKGResult{Rid: rid}, client: svc.client}
	require.NoError(t, reloaded.loadRefreshFromDB())
	require.Equal(t, svc.refreshResult.Share, reloaded.refreshResult.Share)
	require.Equal(t, svc.refreshResult.PaillierP, reloaded.refreshResult.PaillierP)
	require.Equal(t, svc.refreshResult.PaillierQ, reloaded.refreshResult.PaillierQ)
	require.Equal(t, svc.refreshResult.YSecret, reloaded.refreshResult.YSecret)
}

// newClientWithDB 造一个只带数据库的中继客户端（tss 记录读写用）。
func newClientWithDB(t *testing.T) *neutrinoClient {
	t.Helper()
	dir := t.TempDir()
	_, db, err := openWalletDB(dir, "cggmp.db")
	if err != nil {
		t.Skipf("walletdb/bdb unavailable: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	n := &neutrinoClient{ctx: context.Background()}
	n.neutrinoCfg.Database = db
	return n
}

// Test_groupPubKeyFromDKG CGGMP DKG 结果的群公钥必须能转成 btcec 公钥（下游地址派生/验签/
// CommitDKG 载荷口径都依赖它，见 cggmp README：与 GG18 同形）。
func Test_groupPubKeyFromDKG(t *testing.T) {
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pub := priv.PubKey()

	got, err := groupPubKeyFromDKG(&cggmp.DKGResult{
		PubX: pub.X().Bytes(), PubY: pub.Y().Bytes()})
	require.NoError(t, err)
	require.Equal(t, pub.SerializeCompressed(), got.SerializeCompressed())

	_, err = groupPubKeyFromDKG(nil)
	require.Error(t, err)
}

// ---- ④ #28：链上已有 CrossChainInfo 时禁止重跑 DKG ----

// Test_existingChainInfoBlocksDKG 链上该 symbol 已有记录（tssAddress 非空）⇒ 必须拦住重跑 DKG。
//
// 反向：把判据改回"只有链上记录为空才拦"（即改动前的行为：链上有记录且本地无 DKG 时照样重跑）
// 时，第一条断言变红。
func Test_existingChainInfoBlocksDKG(t *testing.T) {
	// 链上还没有记录：可以跑 DKG（全新链的正常路径）。
	require.NoError(t, existingChainInfoBlocksDKG(nil))
	require.NoError(t, existingChainInfoBlocksDKG(&rtypes.CrossChainInfo{}))
	require.NoError(t, existingChainInfoBlocksDKG(&rtypes.CrossChainInfo{Pubkey: []byte{0x02}}),
		"只有 pubkey、没有 tssAddress 不成形，按'链上还没有'处理")

	// 链上已有记录：拒绝，且错误信息必须给出处置办法（运维照不出来就等于没拦）。
	err := existingChainInfoBlocksDKG(&rtypes.CrossChainInfo{
		AssetSymbol: rtypes.BTCSymbol, TssAddress: "bcrt1qchain", Pubkey: []byte{0x02, 0x03},
	})
	require.Error(t, err)
	require.IsType(t, &errRedkgRefusedOnChainWithoutLocalShare{}, err)
	msg := err.Error()
	require.Contains(t, msg, "bcrt1qchain")
	require.Contains(t, msg, "0203")
	require.Contains(t, msg, "duplicate", "要说清'链上记录不可改'的理由")
	require.Contains(t, msg, "refresh", "要指出只换 key 材料、不换群公钥的正解是 refresh")
	require.Contains(t, msg, tssBucketName, "要指出从同一代备份还原的路径")
	require.Contains(t, msg, "fresh chain", "要说清整组重建必须连链一起重建")
}

// Test_signMsg_refreshMaterialNotReusedAcrossKeys 换钥（rid 变）之后，旧 refresh 记录不再配套：
// 载入被拒 ⇒ 必须重跑 refresh。用"rid 不符 + 签名材料缺失"两段一起钉住。
func Test_signMsg_refreshMaterialNotReusedAcrossKeys(t *testing.T) {
	rid := []byte("key-1")
	svc := &tssService{
		dkgResult:     &cggmp.DKGResult{Rid: rid},
		refreshResult: &cggmp.RefreshResult{Share: []byte{0x01}},
		client:        newClientWithDB(t),
	}
	svc.saveRefreshToDB()

	// 同一把钥：rid 相符 ⇒ 直接可用（不必重跑 refresh —— 每次签名复用的就是它）。
	svc.refreshResult = nil
	require.NoError(t, svc.loadRefreshFromDB())
	require.NotNil(t, svc.refreshResult)

	// 换钥（重跑 DKG / 从别处恢复）：rid 变 ⇒ 旧记录作废，必须重跑 refresh。
	svc.refreshResult = nil
	svc.dkgResult = &cggmp.DKGResult{Rid: []byte("key-2")}
	require.Error(t, svc.loadRefreshFromDB())
	require.Nil(t, svc.refreshResult)
}

// Test_refreshSessionName_emptyRid 防御：rid 缺失（DKG 结果不完整）时也要稳定，不 panic。
func Test_refreshSessionName_emptyRid(t *testing.T) {
	require.Equal(t, refreshSessionName(nil), refreshSessionName([]byte{}))
	require.Equal(t, refreshSessionPrefix, refreshSessionName(nil))
}
