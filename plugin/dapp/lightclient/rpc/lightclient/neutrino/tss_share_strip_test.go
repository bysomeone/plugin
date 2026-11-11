package neutrino

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/33cn/chain33/system/crypto/tss"
	"github.com/33cn/chain33/system/crypto/tss/cggmp"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/stretchr/testify/require"
)

/*
 * 首次 refresh 成功后抹掉 DKG 根 share（ensureRefreshResult / stripDKGShare）。
 *
 * 这一批用例钉的是**顺序与恢复边界**，不是密码学：
 *  1. refresh 材料确认落盘之后才抹 share，且抹掉后 DKG 记录仍能重新装载；
 *  2. refresh 落盘失败时绝不抹 share（否则本节点永久失去重跑 refresh 的能力）；
 *  3. 记录里本来就没有 share 时幂等跳过（不重跑 refresh）；
 *  4. 既无 share 又无 refresh 记录时 fail-closed（重跑 refresh 注定失败，只能从备份恢复）。
 */

// newTssStripTestClient 造一个带 DB 与链参数的中继客户端，并返回 DB 路径（供关闭后重新打开核对）。
func newTssStripTestClient(t *testing.T) (*neutrinoClient, string) {
	t.Helper()
	dir := t.TempDir()
	_, db, err := openWalletDB(dir, "tss-strip.db")
	if err != nil {
		t.Skipf("walletdb/bdb unavailable: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	n := &neutrinoClient{ctx: context.Background()}
	n.neutrinoCfg.Database = db
	n.neutrinoCfg.ChainParams = chaincfg.RegressionNetParams
	return n, filepath.Join(dir, "tss-strip.db")
}

// stripTestDKG 造一个带根 share 与公开参数的 DKG 结果（PubX/PubY 必须是曲线上的点）。
func stripTestDKG(t *testing.T, rid []byte) *cggmp.DKGResult {
	t.Helper()
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pub := priv.PubKey()
	return &cggmp.DKGResult{
		PubX:  pub.X().Bytes(),
		PubY:  pub.Y().Bytes(),
		Share: bytes.Repeat([]byte{0x2a}, 32),
		Rid:   rid,
		Bks: map[string]*tss.BK{
			"peer-a": {Rank: 0, X: []byte{0x01}},
			"peer-b": {Rank: 1, X: []byte{0x02}},
		},
		PartialPubKeys: map[string]*cggmp.ECPoint{
			"peer-a": {X: pub.X().Bytes(), Y: pub.Y().Bytes()},
			"peer-b": {X: pub.X().Bytes(), Y: pub.Y().Bytes()},
		},
	}
}

func stubRefresh(t *testing.T, fn func(peers []string, threshold uint32, dkgRes *cggmp.DKGResult,
	sessionID string, opts ...cggmp.Option) (*cggmp.RefreshResult, error)) {
	t.Helper()
	orig := processRefresh
	processRefresh = fn
	t.Cleanup(func() { processRefresh = orig })
}

// readStoredDKG 从已打开的 DB 读回 DKG 记录（写入方的写锁被同一进程持有，不能另开只读句柄）。
func readStoredDKG(t *testing.T, db walletdb.DB) *cggmp.DKGResult {
	t.Helper()
	var raw []byte
	require.NoError(t, walletdb.View(db, func(tx walletdb.ReadTx) error {
		bucket := tx.ReadBucket([]byte(tssBucketName))
		require.NotNil(t, bucket)
		raw = bucket.Get([]byte(dkgResultKey))
		return nil
	}))
	var dkg cggmp.DKGResult
	require.NoError(t, json.Unmarshal(raw, &dkg))
	return &dkg
}

// readStoredDKGFile 在写入方已关闭后，从文件重新打开只读 DB 读回 DKG 记录。
func readStoredDKGFile(t *testing.T, path string) *cggmp.DKGResult {
	t.Helper()
	db, err := walletdb.Open("bdb", path, true, 5*time.Second, false)
	require.NoError(t, err)
	defer db.Close()
	return readStoredDKG(t, db)
}

// Test_ensureRefreshResult_stripsDKGShareAfterPersistingRefresh
// refresh 成功且材料落盘后：内存与持久化记录里的 Share 都被清掉，公开参数保留，
// 且同一份记录能被重新装载、refresh 记录仍可读回。
//
// 反向：没有 stripDKGShare 时，本用例的 require.Empty(svc.dkgResult.Share) 与
// reloaded 断言都会变红（记录里仍有根 share）。
func Test_ensureRefreshResult_stripsDKGShareAfterPersistingRefresh(t *testing.T) {
	client, _ := newTssStripTestClient(t)
	rid := []byte("rid-strip")
	dkg := stripTestDKG(t, rid)
	putTssRecord(t, client.neutrinoCfg.Database, dkgResultKey, dkg)

	wantPubX, wantPubY := append([]byte(nil), dkg.PubX...), append([]byte(nil), dkg.PubY...)
	wantBks, wantPPKs := dkg.Bks, dkg.PartialPubKeys

	svc := &tssService{
		client:    client,
		cfg:       tssConfig{Threshold: 2, Peers: []string{"peer-a", "peer-b"}},
		dkgResult: dkg,
	}
	stubRefresh(t, func(peers []string, threshold uint32, dkgRes *cggmp.DKGResult,
		sessionID string, opts ...cggmp.Option) (*cggmp.RefreshResult, error) {
		require.Equal(t, rid, dkgRes.Rid)
		require.Equal(t, refreshSessionName(rid), sessionID)
		require.NotEmpty(t, dkgRes.Share, "refresh 必须用 DKG 根 share 参与协议")
		return &cggmp.RefreshResult{Share: []byte{0x09}, YSecret: []byte{0x0a}}, nil
	})

	require.NoError(t, svc.ensureRefreshResult())

	require.Empty(t, svc.dkgResult.Share, "refresh 落盘后内存里的根 share 必须被抹掉")
	require.Equal(t, wantPubX, svc.dkgResult.PubX)
	require.Equal(t, wantPubY, svc.dkgResult.PubY)
	require.Equal(t, wantBks, svc.dkgResult.Bks)
	require.Equal(t, wantPPKs, svc.dkgResult.PartialPubKeys)
	require.Equal(t, rid, svc.dkgResult.Rid)

	// 持久化记录里也不能再有 share；重新装载必须成功（签名与链上核对只需要公开参数）。
	stored := readStoredDKG(t, client.neutrinoCfg.Database)
	require.Empty(t, stored.Share)
	require.Equal(t, wantPubX, stored.PubX)

	reloaded := &tssService{client: client}
	require.NoError(t, reloaded.loadDKGFromDB())
	require.Empty(t, reloaded.dkgResult.Share)
	require.Equal(t, rid, reloaded.dkgResult.Rid)
	require.NoError(t, reloaded.loadRefreshFromDB(), "refresh 记录必须仍在且仍与当前 rid 配套")
	require.Equal(t, []byte{0x09}, reloaded.refreshResult.Share)
}

// Test_ensureRefreshResult_keepsShareWhenRefreshPersistFails
// refresh 协议成功但落盘失败时，绝不能 strip：DKG 根 share 是重跑 refresh 的前提，
// 丢掉它而 refresh 又没落盘 = 本节点永久失去恢复能力。
//
// 反向：把 saveRefreshToDB() 的返回值忽略、无条件 strip 时，本用例的两条 share 非空断言变红。
func Test_ensureRefreshResult_keepsShareWhenRefreshPersistFails(t *testing.T) {
	client, dbPath := newTssStripTestClient(t)
	rid := []byte("rid-persist-fail")
	dkg := stripTestDKG(t, rid)
	putTssRecord(t, client.neutrinoCfg.Database, dkgResultKey, dkg)

	svc := &tssService{
		client:    client,
		cfg:       tssConfig{Threshold: 2, Peers: []string{"peer-a", "peer-b"}},
		dkgResult: dkg,
	}
	stubRefresh(t, func(peers []string, threshold uint32, dkgRes *cggmp.DKGResult,
		sessionID string, opts ...cggmp.Option) (*cggmp.RefreshResult, error) {
		return &cggmp.RefreshResult{Share: []byte{0x09}}, nil
	})

	// 关掉 DB 让所有写入失败；同时取消客户端 context，让失败后的重试循环立刻退出
	// （生产语义是"保持原样继续重试"，测试只需要观察第一轮失败后的状态）。
	ctx, cancel := context.WithCancel(context.Background())
	client.ctx = ctx
	cancel()
	require.NoError(t, client.neutrinoCfg.Database.Close())

	require.NoError(t, svc.ensureRefreshResult())
	require.NotEmpty(t, svc.dkgResult.Share, "落盘失败时必须保留内存里的根 share")
	require.Nil(t, svc.refreshResult, "未落盘的 refresh 结果不能当成可用材料")

	stored := readStoredDKGFile(t, dbPath)
	require.NotEmpty(t, stored.Share, "落盘失败时持久化记录里的根 share 也必须原样保留")
}

// Test_ensureRefreshResult_idempotentWhenShareAlreadyStripped
// 记录里已经没 share（上次已抹掉）且 refresh 记录可用时：直接载入，幂等跳过，不重跑 refresh。
//
// 反向：去掉 stripDKGShare 里的 dkgSharePresent 判空（或对空 share 也走 strip 分支）不会报错，
// 但把"先载入 refresh 再判断"写反（没 share 就当材料缺失）时，本用例会以错误返回或触发
// processRefresh 调用而变红。
func Test_ensureRefreshResult_idempotentWhenShareAlreadyStripped(t *testing.T) {
	client, _ := newTssStripTestClient(t)
	rid := []byte("rid-idempotent")
	dkg := stripTestDKG(t, rid)
	dkg.Share = nil
	putTssRecord(t, client.neutrinoCfg.Database, dkgResultKey, dkg)
	putTssRecord(t, client.neutrinoCfg.Database, refreshResultKey, &refreshRecord{
		DkgRid: hex.EncodeToString(rid),
		Result: &cggmp.RefreshResult{Share: []byte{0x0b}},
	})

	svc := &tssService{
		client:    client,
		cfg:       tssConfig{Threshold: 2, Peers: []string{"peer-a", "peer-b"}},
		dkgResult: dkg,
	}
	stubRefresh(t, func(peers []string, threshold uint32, dkgRes *cggmp.DKGResult,
		sessionID string, opts ...cggmp.Option) (*cggmp.RefreshResult, error) {
		t.Fatal("share 已抹掉且有可用 refresh 记录时不得重跑 refresh")
		return nil, nil
	})

	require.NoError(t, svc.ensureRefreshResult())
	require.Empty(t, svc.dkgResult.Share)
	require.Equal(t, []byte{0x0b}, svc.refreshResult.Share)
}

// Test_ensureRefreshResult_failClosedWithoutShareAndRefresh
// 既没有 refresh 记录、DKG 记录里也没有根 share：重跑 refresh 不可能成功（ProcessRefresh 要根
// share），必须 fail-closed 并给出"从同代离线备份恢复"的可操作指引，而不是进入无限重试。
//
// 反向：改动前这里会进入 ProcessRefresh 重试循环（本用例会调用到被 stub 的 processRefresh 并
// 由 t.Fatal 变红），且返回 nil error。
func Test_ensureRefreshResult_failClosedWithoutShareAndRefresh(t *testing.T) {
	client, _ := newTssStripTestClient(t)
	rid := []byte("rid-lost")
	dkg := stripTestDKG(t, rid)
	dkg.Share = nil
	putTssRecord(t, client.neutrinoCfg.Database, dkgResultKey, dkg)

	svc := &tssService{
		client:    client,
		cfg:       tssConfig{Threshold: 2, Peers: []string{"peer-a", "peer-b"}},
		dkgResult: dkg,
	}
	stubRefresh(t, func(peers []string, threshold uint32, dkgRes *cggmp.DKGResult,
		sessionID string, opts ...cggmp.Option) (*cggmp.RefreshResult, error) {
		t.Fatal("没有根 share 时不可能重跑 refresh，不应进入重试循环")
		return nil, nil
	})

	err := svc.ensureRefreshResult()
	require.ErrorIs(t, err, errTssKeyMaterialLost)
	require.ErrorContains(t, err, "offline backup", "错误信息必须给出恢复路径")
	require.ErrorContains(t, err, tssBucketName)
	require.ErrorContains(t, err, "new DKG", "必须明确禁止重跑 DKG（会换掉链上已钉住的群公钥）")
	require.Empty(t, svc.dkgResult.Share)
}
