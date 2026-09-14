package executor

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/33cn/chain33/client/mocks"
	"github.com/33cn/chain33/common/crypto"
	dbm "github.com/33cn/chain33/common/db"
	"github.com/33cn/chain33/types"
	"github.com/33cn/chain33/util"
	ltypes "github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

func TestLightclientCheckTxBasic(t *testing.T) {
	cli := newLightclient().(*lightclient)
	addr, priv := util.Genaddress()
	lightCfg.CommitAddress = addr
	lightCfg.BtcNetName = "regtest"

	tx := &types.Transaction{Payload: []byte("invalid")}
	require.Equal(t, ErrDecodeAction, cli.CheckTx(tx, 0))

	action := &ltypes.LightClientAction{Ty: 999}
	tx.Payload = types.Encode(action)
	tx.Sign(types.SECP256K1, priv)
	require.Equal(t, types.ErrActionNotSupport, cli.CheckTx(tx, 0))
}

func TestMapBtcHeaderVerifyErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want error
	}{
		{
			name: "difficulty",
			err:  blockchain.RuleError{ErrorCode: blockchain.ErrUnexpectedDifficulty, Description: "bad bits"},
			want: ErrBtcTargetBits,
		},
		{
			name: "time too old",
			err:  blockchain.RuleError{ErrorCode: blockchain.ErrTimeTooOld, Description: "too old"},
			want: ErrBtcHeaderTimeTooOld,
		},
		{
			name: "time too new",
			err:  blockchain.RuleError{ErrorCode: blockchain.ErrTimeTooNew, Description: "too new"},
			want: ErrBtcHeaderTimeTooNew,
		},
		{
			name: "invalid time precision",
			err:  blockchain.RuleError{ErrorCode: blockchain.ErrInvalidTime, Description: "invalid time"},
			want: ErrBtcHeaderInvalidTime,
		},
		{
			name: "fallback",
			err:  errors.New("other"),
			want: ErrBtcHeaderVerify,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, mapBtcHeaderVerifyErr(tc.err))
		})
	}
}

func TestLightclientCheckBtcHeaders(t *testing.T) {
	dir, stateDB, localDB := util.CreateTestDB()
	defer util.CloseTestDB(dir, stateDB)

	cli := newLightclient().(*lightclient)
	setupTestDriver(t, cli)
	cli.SetStateDB(stateDB)
	cli.SetLocalDB(localDB)

	commitAddr, commitPriv := util.Genaddress()
	_, illegalPriv := util.Genaddress()
	lightCfg.CommitAddress = commitAddr
	lightCfg.BtcNetName = "regtest"

	regtest := &chaincfg.RegressionNetParams
	ts := types.Now().Add(-time.Hour)
	prev := mineBtcHeader(t, nil, 100, regtest.PowLimitBits, ts)
	next := mineBtcHeader(t, prev, 101, regtest.PowLimitBits, ts.Add(time.Minute))

	require.NoError(t, stateDB.Set(btcLastHeaderKey(), types.Encode(prev)))
	require.NoError(t, localDB.Set(btcHeaderKey(prev.Height), types.Encode(prev)))

	t.Run("illegal commit address", func(t *testing.T) {
		tx := buildCheckTx(t, &ltypes.BtcHeaders{Headers: []*ltypes.BtcHeader{next}}, illegalPriv)
		require.Equal(t, ErrIllegalCommitAddress, cli.CheckTx(tx, 0))
	})

	t.Run("empty headers", func(t *testing.T) {
		tx := buildCheckTx(t, &ltypes.BtcHeaders{}, commitPriv)
		require.Equal(t, types.ErrInvalidParam, cli.CheckTx(tx, 0))
	})

	t.Run("nil header", func(t *testing.T) {
		tx := buildCheckTx(t, &ltypes.BtcHeaders{Headers: []*ltypes.BtcHeader{nil}}, commitPriv)
		require.Equal(t, types.ErrInvalidParam, cli.CheckTx(tx, 0))
	})

	t.Run("unknown ancestor", func(t *testing.T) {
		// B3/B4：首个头的父块必须是 canonical 链最近窗口里的已知区块。这里把 prevHash 改成零值，
		// 高度对得上（100）但 hash 对不上，属于"分叉点无从确认"，fail-closed。
		bad := cloneHeader(next)
		bad.PreviousHash = chainhash.Hash{}.String()
		tx := buildCheckTx(t, &ltypes.BtcHeaders{Headers: []*ltypes.BtcHeader{bad}}, commitPriv)
		require.Equal(t, ErrBtcHeaderUnknownAncestor, cli.CheckTx(tx, 0))
	})

	t.Run("batch internal disorder", func(t *testing.T) {
		// 批内头之间必须严格连续（高度 +1 且 prevHash 对得上）。
		third := mineBtcHeader(t, next, 102, regtest.PowLimitBits, ts.Add(3*time.Minute))
		bad := cloneHeader(third)
		bad.PreviousHash = chainhash.Hash{}.String()
		tx := buildCheckTx(t, &ltypes.BtcHeaders{Headers: []*ltypes.BtcHeader{next, bad}}, commitPriv)
		require.Equal(t, ErrBtcHeaderDisorder, cli.CheckTx(tx, 0))
	})

	t.Run("invalid wire header", func(t *testing.T) {
		bad := cloneHeader(next)
		bad.MerkleRoot = "not-a-hash"
		tx := buildCheckTx(t, &ltypes.BtcHeaders{Headers: []*ltypes.BtcHeader{bad}}, commitPriv)
		require.Equal(t, ErrToBtcWireHeader, cli.CheckTx(tx, 0))
	})

	t.Run("invalid header hash", func(t *testing.T) {
		bad := cloneHeader(next)
		bad.Hash = prev.Hash
		tx := buildCheckTx(t, &ltypes.BtcHeaders{Headers: []*ltypes.BtcHeader{bad}}, commitPriv)
		require.Equal(t, ErrInvalidBtcBlockHash, cli.CheckTx(tx, 0))
	})

	t.Run("target bits mismatch", func(t *testing.T) {
		badBitsHeader := mineBtcHeader(t, prev, 101, regtest.PowLimitBits-1, ts.Add(2*time.Minute))
		tx := buildCheckTx(t, &ltypes.BtcHeaders{Headers: []*ltypes.BtcHeader{badBitsHeader}}, commitPriv)
		require.Equal(t, ErrBtcTargetBits, cli.CheckTx(tx, 0))
	})

	t.Run("success", func(t *testing.T) {
		tx := buildCheckTx(t, &ltypes.BtcHeaders{Headers: []*ltypes.BtcHeader{next}}, commitPriv)
		require.NoError(t, cli.CheckTx(tx, 0))
	})
}

func TestLightclientCheckBtcHeadersBootstrap(t *testing.T) {
	dir, stateDB, localDB := util.CreateTestDB()
	defer util.CloseTestDB(dir, stateDB)

	cli := newLightclient().(*lightclient)
	setupTestDriver(t, cli)
	cli.SetStateDB(stateDB)
	cli.SetLocalDB(localDB)

	commitAddr, commitPriv := util.Genaddress()
	lightCfg.CommitAddress = commitAddr
	lightCfg.BtcNetName = "regtest"

	regtest := &chaincfg.RegressionNetParams
	ts := types.Now().Add(-time.Hour)
	// 现有 CI 链就是从 btcHeaderStartHeight=1（即 regtest 创世之后第一个块）长起来的：
	// 首个头的 previousHash 必须等于创世 hash，新增的锚点校验必须仍然放行。
	h1 := mineBtcHeaderFrom(t, regtest.GenesisHash.String(), 1, regtest.PowLimitBits, ts)
	h2 := mineBtcHeader(t, h1, 2, regtest.PowLimitBits, ts.Add(time.Minute))

	tx := buildCheckTx(t, &ltypes.BtcHeaders{Headers: []*ltypes.BtcHeader{h1, h2}}, commitPriv)
	require.NoError(t, cli.CheckTx(tx, 0))
}

// TestBtcHeadersBootstrapAnchor 覆盖 B5：bootstrap 时首个头必须锚定到该网络的真实链。
func TestBtcHeadersBootstrapAnchor(t *testing.T) {
	regtest := &chaincfg.RegressionNetParams
	genesisHash := regtest.GenesisHash.String()
	ts := types.Now().Add(-time.Hour)

	// 每轮用独立的 DB，避免状态互相污染。
	newCli := func(t *testing.T) (*lightclient, dbm.KV, func()) {
		t.Helper()
		dir, stateDB, localDB := util.CreateTestDB()
		cli := newLightclient().(*lightclient)
		setupTestDriver(t, cli)
		cli.SetStateDB(stateDB)
		cli.SetLocalDB(localDB)
		commitAddr, commitPriv := util.Genaddress()
		lightCfg.CommitAddress = commitAddr
		lightCfg.BtcNetName = "regtest"
		t.Cleanup(func() { util.CloseTestDB(dir, stateDB) })
		return cli, localDB, func() { _ = commitPriv }
	}

	t.Run("self-mined first header on a bogus parent is rejected", func(t *testing.T) {
		cli, _, _ := newCli(t)
		commitAddr, commitPriv := util.Genaddress()
		lightCfg.CommitAddress = commitAddr
		// 攻击者自造链：Bits 直接取网络最大目标（powLimit），秒挖出头，且父块不是创世。
		orphan := mineBtcHeaderFrom(t, chainhash.Hash{}.String(), 1, regtest.PowLimitBits, ts)
		tx := buildCheckTx(t, &ltypes.BtcHeaders{Headers: []*ltypes.BtcHeader{orphan}}, commitPriv)
		require.Equal(t, ErrBtcHeaderNoAnchor, cli.CheckTx(tx, 0))
	})

	t.Run("first header directly on regtest genesis is accepted", func(t *testing.T) {
		cli, _, _ := newCli(t)
		commitAddr, commitPriv := util.Genaddress()
		lightCfg.CommitAddress = commitAddr
		h1 := mineBtcHeaderFrom(t, genesisHash, 1, regtest.PowLimitBits, ts)
		tx := buildCheckTx(t, &ltypes.BtcHeaders{Headers: []*ltypes.BtcHeader{h1}}, commitPriv)
		require.NoError(t, cli.CheckTx(tx, 0))
	})

	t.Run("first header on a known checkpoint is accepted", func(t *testing.T) {
		cli, _, _ := newCli(t)
		commitAddr, commitPriv := util.Genaddress()
		lightCfg.CommitAddress = commitAddr

		checkpoint := mineBtcHeaderFrom(t, genesisHash, 4, regtest.PowLimitBits, ts)
		btcCheckpointTable[regtest.Net] = map[uint64]string{4: checkpoint.Hash}
		defer delete(btcCheckpointTable, regtest.Net)

		// 高度 5、父块正好是锚点 4，且 localDB 里没有任何历史头。
		h5 := mineBtcHeader(t, checkpoint, 5, regtest.PowLimitBits, ts.Add(5*time.Minute))
		tx := buildCheckTx(t, &ltypes.BtcHeaders{Headers: []*ltypes.BtcHeader{h5}}, commitPriv)
		require.NoError(t, cli.CheckTx(tx, 0))
	})

	t.Run("first header found on a different checkpoint hash is rejected", func(t *testing.T) {
		cli, _, _ := newCli(t)
		commitAddr, commitPriv := util.Genaddress()
		lightCfg.CommitAddress = commitAddr

		checkpoint := mineBtcHeaderFrom(t, genesisHash, 4, regtest.PowLimitBits, ts)
		btcCheckpointTable[regtest.Net] = map[uint64]string{4: checkpoint.Hash}
		defer delete(btcCheckpointTable, regtest.Net)

		// 父块 hash 与锚点不符：锚点表命中失败，且 localDB 无历史可回溯。
		other := mineBtcHeaderFrom(t, chainhash.Hash{}.String(), 5, regtest.PowLimitBits, ts.Add(5*time.Minute))
		tx := buildCheckTx(t, &ltypes.BtcHeaders{Headers: []*ltypes.BtcHeader{other}}, commitPriv)
		require.Equal(t, ErrBtcHeaderNoAnchor, cli.CheckTx(tx, 0))
	})

	t.Run("first header traceable through localdb to genesis is accepted", func(t *testing.T) {
		cli, localDB, _ := newCli(t)
		commitAddr, commitPriv := util.Genaddress()
		lightCfg.CommitAddress = commitAddr

		// localDB 里已有 1..4 的连续头（stateDB tip 丢失但 localDB 保留的场景）。
		h1 := mineBtcHeaderFrom(t, genesisHash, 1, regtest.PowLimitBits, ts)
		h2 := mineBtcHeader(t, h1, 2, regtest.PowLimitBits, ts.Add(time.Minute))
		h3 := mineBtcHeader(t, h2, 3, regtest.PowLimitBits, ts.Add(2*time.Minute))
		h4 := mineBtcHeader(t, h3, 4, regtest.PowLimitBits, ts.Add(3*time.Minute))
		for _, h := range []*ltypes.BtcHeader{h1, h2, h3, h4} {
			require.NoError(t, localDB.Set(btcHeaderKey(h.Height), types.Encode(h)))
		}

		h5 := mineBtcHeader(t, h4, 5, regtest.PowLimitBits, ts.Add(4*time.Minute))
		tx := buildCheckTx(t, &ltypes.BtcHeaders{Headers: []*ltypes.BtcHeader{h5}}, commitPriv)
		require.NoError(t, cli.CheckTx(tx, 0))
	})

	t.Run("broken localdb chain does not anchor", func(t *testing.T) {
		cli, localDB, _ := newCli(t)
		commitAddr, commitPriv := util.Genaddress()
		lightCfg.CommitAddress = commitAddr

		// localDB 在高度 3 处断链（缺一个头），回溯必须失败。
		h1 := mineBtcHeaderFrom(t, genesisHash, 1, regtest.PowLimitBits, ts)
		h2 := mineBtcHeader(t, h1, 2, regtest.PowLimitBits, ts.Add(time.Minute))
		h4 := &ltypes.BtcHeader{Height: 4, Hash: "deadbeef", PreviousHash: h2.Hash}
		require.NoError(t, localDB.Set(btcHeaderKey(h1.Height), types.Encode(h1)))
		require.NoError(t, localDB.Set(btcHeaderKey(h2.Height), types.Encode(h2)))
		require.NoError(t, localDB.Set(btcHeaderKey(h4.Height), types.Encode(h4)))

		h5 := mineBtcHeader(t, h4, 5, regtest.PowLimitBits, ts.Add(4*time.Minute))
		tx := buildCheckTx(t, &ltypes.BtcHeaders{Headers: []*ltypes.BtcHeader{h5}}, commitPriv)
		require.Equal(t, ErrBtcHeaderNoAnchor, cli.CheckTx(tx, 0))
	})

	t.Run("first header on a non-genesis network is rejected", func(t *testing.T) {
		cli, _, _ := newCli(t)
		commitAddr, commitPriv := util.Genaddress()
		lightCfg.CommitAddress = commitAddr

		// 用 mainnet 创世 hash 冒充 regtest 链的父块：网络不匹配，同样拒绝。
		h1 := mineBtcHeaderFrom(t, chaincfg.MainNetParams.GenesisHash.String(), 1, regtest.PowLimitBits, ts)
		tx := buildCheckTx(t, &ltypes.BtcHeaders{Headers: []*ltypes.BtcHeader{h1}}, commitPriv)
		require.Equal(t, ErrBtcHeaderNoAnchor, cli.CheckTx(tx, 0))
	})
}

func TestLightclientExecBtcHeaders(t *testing.T) {
	t.Run("get last header error", func(t *testing.T) {
		dir, stateDB, _ := util.CreateTestDB()
		defer util.CloseTestDB(dir, stateDB)
		cli := newLightclient().(*lightclient)
		setupTestDriver(t, cli)
		cli.SetStateDB(stateDB)
		// Corrupt data to force decode error.
		require.NoError(t, stateDB.Set(btcLastHeaderKey(), []byte("bad-data")))

		headers := &ltypes.BtcHeaders{Headers: []*ltypes.BtcHeader{{Height: 1, Hash: chainhash.Hash{}.String()}}}
		recp, err := cli.Exec_BtcHeaders(headers, &types.Transaction{}, 0)
		require.Nil(t, recp)
		require.Equal(t, ErrBtcGetLastHeader, err)
	})

	t.Run("success", func(t *testing.T) {
		dir, stateDB, _ := util.CreateTestDB()
		defer util.CloseTestDB(dir, stateDB)
		cli := newLightclient().(*lightclient)
		setupTestDriver(t, cli)
		cli.SetStateDB(stateDB)

		prev := &ltypes.BtcHeader{Height: 100, Hash: "prevhash", Bits: int64(chaincfg.RegressionNetParams.PowLimitBits)}
		require.NoError(t, stateDB.Set(btcLastHeaderKey(), types.Encode(prev)))
		commit := &ltypes.BtcHeader{Height: 101, Hash: "commithash", PreviousHash: prev.Hash,
			Bits: int64(chaincfg.RegressionNetParams.PowLimitBits), Confirmations: 6}

		recp, err := cli.Exec_BtcHeaders(&ltypes.BtcHeaders{Headers: []*ltypes.BtcHeader{commit}}, &types.Transaction{}, 0)
		require.NoError(t, err)
		require.EqualValues(t, types.ExecOk, recp.Ty)
		// B3/B4：切换 canonical tip 时同时写 tip 与链索引窗口
		require.Len(t, recp.KV, 2)
		require.Equal(t, btcLastHeaderKey(), recp.KV[0].Key)
		require.Equal(t, btcChainStateKey(), recp.KV[1].Key)

		saved := &ltypes.BtcHeader{}
		require.NoError(t, types.Decode(recp.KV[0].Value, saved))
		require.Equal(t, commit.Height, saved.Height)
		require.Equal(t, commit.Hash, saved.Hash)

		state := &ltypes.BtcChainState{}
		require.NoError(t, types.Decode(recp.KV[1].Value, state))
		require.Equal(t, []uint64{100, 101}, chainHeights(state))
		require.Equal(t, commit.Hash, state.GetNodes()[1].GetHash())
		require.NotEmpty(t, state.GetNodes()[1].GetWork, "每个候选节点都要带累积工作量")

		require.Len(t, recp.Logs, 1)
		log := &ltypes.BtcHeadersLog{}
		require.NoError(t, types.Decode(recp.Logs[0].Log, log))
		require.Equal(t, prev.Height, log.LastHeight)
		require.Equal(t, prev.Hash, log.LastHash)
		require.Equal(t, commit.Height, log.CommitHeight)
		require.Equal(t, commit.Hash, log.CommitHash)
		require.Equal(t, commit.Confirmations, log.Confirmations)
		// B3/B4 新增字段：扩展是"挂载在旧 tip 上"的切换
		require.EqualValues(t, 100, log.ForkHeight)
		require.True(t, log.CanonicalSwitched)
		require.EqualValues(t, btcReorgRuleVersion, log.ReorgRuleVersion)
		require.NotEmpty(t, log.TipWork)
	})
}

func TestLightclientExecLocalBtcHeaders(t *testing.T) {
	dir, stateDB, localDB := util.CreateTestDB()
	defer util.CloseTestDB(dir, stateDB)

	cli := newLightclient().(*lightclient)
	setupTestDriver(t, cli)
	cli.SetStateDB(stateDB)
	cli.SetLocalDB(localDB)

	h1 := &ltypes.BtcHeader{Height: 11, Hash: "h11"}
	h2 := &ltypes.BtcHeader{Height: 12, Hash: "h12"}
	// 旧 canonical 头：高度 11..12 上原来的头，切换后它们的可查性必须被撤掉
	old1 := &ltypes.BtcHeader{Height: 11, Hash: "old11"}
	old2 := &ltypes.BtcHeader{Height: 12, Hash: "old12"}
	require.NoError(t, localDB.Set(btcHeaderKey(old1.Height), types.Encode(old1)))
	require.NoError(t, localDB.Set(btcHeaderHashHeightKey(old1.Hash), types.Encode(&types.Int64{Data: int64(old1.Height)})))
	require.NoError(t, localDB.Set(btcHeaderKey(old2.Height), types.Encode(old2)))

	tx := &types.Transaction{Execer: []byte(ltypes.LightclientX)}
	receiptData := &types.ReceiptData{Logs: []*types.ReceiptLog{
		{Ty: ltypes.TyBtcHeadersLog, Log: types.Encode(&ltypes.BtcHeadersLog{
			LastHeight: 12, ForkHeight: 10, CanonicalSwitched: true,
			TipWork: big.NewInt(1).Bytes(), ReorgRuleVersion: btcReorgRuleVersion,
		})},
	}}
	dbSet, err := cli.ExecLocal_BtcHeaders(&ltypes.BtcHeaders{Headers: []*ltypes.BtcHeader{h1, h2}}, tx, receiptData, 0)
	require.NoError(t, err)
	require.NotNil(t, dbSet)

	require.True(t, hasKV(dbSet.KV, btcHeaderKey(h1.Height)))
	require.True(t, hasKV(dbSet.KV, btcHeaderHashHeightKey(h1.Hash)))
	require.True(t, hasKV(dbSet.KV, btcHeaderKey(h2.Height)))
	require.True(t, hasKV(dbSet.KV, btcHeaderHashHeightKey(h2.Hash)))
	// 被替换掉的旧 canonical 高度（11、12 = [forkHeight+1, lastHeight]）与旧 hash 索引一起删除
	require.True(t, hasKV(dbSet.KV, btcHeaderHashHeightKey(old1.Hash)))
	require.Equal(t, []byte(nil), kvValue(dbSet.KV, btcHeaderKey(old1.Height)))
	require.Equal(t, []byte(nil), kvValue(dbSet.KV, btcHeaderKey(old2.Height)))
}

// TestBtcHeadersPerTxLimit 覆盖 B7：单笔交易的头数上限。
func TestBtcHeadersPerTxLimit(t *testing.T) {
	dir, stateDB, localDB := util.CreateTestDB()
	defer util.CloseTestDB(dir, stateDB)

	cli := newLightclient().(*lightclient)
	setupTestDriver(t, cli)
	cli.SetStateDB(stateDB)
	cli.SetLocalDB(localDB)

	commitAddr, commitPriv := util.Genaddress()
	lightCfg.CommitAddress = commitAddr
	lightCfg.BtcNetName = "regtest"

	// 高度逐个 +1 但不挖矿：上限校验发生在 PoW/锚点校验之前，只要求 hash 非空。
	batch := func(n int) []*ltypes.BtcHeader {
		headers := make([]*ltypes.BtcHeader, 0, n)
		for i := 1; i <= n; i++ {
			headers = append(headers, &ltypes.BtcHeader{
				Height:       uint64(i),
				Hash:         fmt.Sprintf("hash-%d", i),
				PreviousHash: fmt.Sprintf("hash-%d", i-1),
			})
		}
		return headers
	}

	t.Run("at limit falls through to the next check", func(t *testing.T) {
		// 64 个头不会被上限拒绝（会因自造 hash 在后面的锚点校验被拒，这里只断言不是 ErrBtcHeadersTooMany）。
		tx := buildCheckTx(t, &ltypes.BtcHeaders{Headers: batch(maxBtcHeadersPerTx)}, commitPriv)
		require.Equal(t, ErrBtcHeaderNoAnchor, cli.CheckTx(tx, 0))
	})

	t.Run("over limit is rejected", func(t *testing.T) {
		tx := buildCheckTx(t, &ltypes.BtcHeaders{Headers: batch(maxBtcHeadersPerTx + 1)}, commitPriv)
		require.Equal(t, ErrBtcHeadersTooMany, cli.CheckTx(tx, 0))
	})

	t.Run("relayer batch size is unaffected", func(t *testing.T) {
		tx := buildCheckTx(t, &ltypes.BtcHeaders{Headers: batch(16)}, commitPriv)
		require.NotEqual(t, ErrBtcHeadersTooMany, cli.CheckTx(tx, 0))
	})
}

// TestBtcHeadersDuplicateHeight 覆盖 B7：同一高度重复提交/跳高被拒。
func TestBtcHeadersDuplicateHeight(t *testing.T) {
	dir, stateDB, localDB := util.CreateTestDB()
	defer util.CloseTestDB(dir, stateDB)

	cli := newLightclient().(*lightclient)
	setupTestDriver(t, cli)
	cli.SetStateDB(stateDB)
	cli.SetLocalDB(localDB)

	commitAddr, commitPriv := util.Genaddress()
	lightCfg.CommitAddress = commitAddr
	lightCfg.BtcNetName = "regtest"

	t.Run("duplicate height in one tx", func(t *testing.T) {
		dup := []*ltypes.BtcHeader{
			{Height: 10, Hash: "hash-a", PreviousHash: "hash-0"},
			{Height: 10, Hash: "hash-b", PreviousHash: "hash-a"},
		}
		tx := buildCheckTx(t, &ltypes.BtcHeaders{Headers: dup}, commitPriv)
		require.Equal(t, ErrBtcHeaderDuplicateHeight, cli.CheckTx(tx, 0))
	})

	t.Run("skipped height in one tx", func(t *testing.T) {
		gap := []*ltypes.BtcHeader{
			{Height: 10, Hash: "hash-a", PreviousHash: "hash-0"},
			{Height: 12, Hash: "hash-b", PreviousHash: "hash-a"},
		}
		tx := buildCheckTx(t, &ltypes.BtcHeaders{Headers: gap}, commitPriv)
		require.Equal(t, ErrBtcHeaderDuplicateHeight, cli.CheckTx(tx, 0))
	})
}

// TestCommitAddressRequired 覆盖 B2：commitAddress 留空时运行期也必须拒绝写头链。
func TestCommitAddressRequired(t *testing.T) {
	require.Error(t, validateCommitAddress(""))
	require.Contains(t, validateCommitAddress("").Error(), "commitAddress")
	require.NoError(t, validateCommitAddress("1KSBd17H7ZK8iT37aJztFB22XGwsPTdwE4"))

	dir, stateDB, localDB := util.CreateTestDB()
	defer util.CloseTestDB(dir, stateDB)

	cli := newLightclient().(*lightclient)
	setupTestDriver(t, cli)
	cli.SetStateDB(stateDB)
	cli.SetLocalDB(localDB)

	commitAddr, commitPriv := util.Genaddress()
	lightCfg.BtcNetName = "regtest"
	lightCfg.CommitAddress = commitAddr
	defer func() { lightCfg.CommitAddress = commitAddr }()

	// 留空后，任何人都不得再提交头（fail-closed），即便交易由原授权地址签名。
	lightCfg.CommitAddress = ""
	regtest := &chaincfg.RegressionNetParams
	h1 := mineBtcHeaderFrom(t, regtest.GenesisHash.String(), 1, regtest.PowLimitBits, types.Now().Add(-time.Hour))
	tx := buildCheckTx(t, &ltypes.BtcHeaders{Headers: []*ltypes.BtcHeader{h1}}, commitPriv)
	require.Equal(t, ErrIllegalCommitAddress, cli.CheckTx(tx, 0))

	lightCfg.CommitAddress = commitAddr
	tx = buildCheckTx(t, &ltypes.BtcHeaders{Headers: []*ltypes.BtcHeader{h1}}, commitPriv)
	require.NoError(t, cli.CheckTx(tx, 0))
}

// TestBtcChainContextCheckpoints 覆盖 B5 的锚点表：VerifyCheckpoint / FindPreviousCheckpoint 返回真实值。
func TestBtcChainContextCheckpoints(t *testing.T) {
	regtest := &chaincfg.RegressionNetParams
	hashAt := func(s string) *chainhash.Hash {
		h, err := chainhash.NewHashFromStr(s)
		require.NoError(t, err)
		return h
	}
	const (
		cp100 = "1111111111111111111111111111111111111111111111111111111111111111"
		cp200 = "2222222222222222222222222222222222222222222222222222222222222222"
	)

	// 未配置锚点表的网络：不做任何约束。
	plain := newBtcChainContext(regtest)
	require.True(t, plain.VerifyCheckpoint(100, hashAt(cp100)))
	ctx, err := plain.FindPreviousCheckpoint()
	require.NoError(t, err)
	require.Nil(t, ctx)

	btcCheckpointTable[regtest.Net] = map[uint64]string{100: cp100, 200: cp200}
	defer delete(btcCheckpointTable, regtest.Net)

	chainCtx := newBtcChainContext(regtest)
	require.Len(t, chainCtx.checkpoints, 2)

	require.True(t, chainCtx.VerifyCheckpoint(100, hashAt(cp100)))
	require.True(t, chainCtx.VerifyCheckpoint(200, hashAt(cp200)))
	require.False(t, chainCtx.VerifyCheckpoint(100, hashAt(cp200)))
	// 表里没有的高度不受约束。
	require.True(t, chainCtx.VerifyCheckpoint(150, hashAt(cp100)))

	chainCtx.setCheckHeight(250)
	prev, err := chainCtx.FindPreviousCheckpoint()
	require.NoError(t, err)
	require.NotNil(t, prev)
	require.EqualValues(t, 200, prev.Height())

	// 恰好等于锚点高度时，"之前的锚点"要退到上一个。
	chainCtx.setCheckHeight(200)
	prev, err = chainCtx.FindPreviousCheckpoint()
	require.NoError(t, err)
	require.NotNil(t, prev)
	require.EqualValues(t, 100, prev.Height())

	// 锚点之前（含未设置高度）无前置锚点。
	chainCtx.setCheckHeight(100)
	prev, err = chainCtx.FindPreviousCheckpoint()
	require.NoError(t, err)
	require.Nil(t, prev)

	chainCtx = newBtcChainContext(regtest)
	prev, err = chainCtx.FindPreviousCheckpoint()
	require.NoError(t, err)
	require.Nil(t, prev)
}

func buildCheckTx(t *testing.T, headers *ltypes.BtcHeaders, priv crypto.PrivKey) *types.Transaction {
	t.Helper()
	action := &ltypes.LightClientAction{
		Ty: ltypes.TyBtcHeadersAction,
		Value: &ltypes.LightClientAction_BtcHeaders{
			BtcHeaders: headers,
		},
	}
	tx := &types.Transaction{Payload: types.Encode(action)}
	tx.Sign(types.SECP256K1, priv)
	return tx
}

func mineBtcHeader(t *testing.T, prev *ltypes.BtcHeader, height uint64, bits uint32, ts time.Time) *ltypes.BtcHeader {
	t.Helper()

	prevHash := chainhash.Hash{}.String()
	if prev != nil {
		prevHash = prev.Hash
	}
	return mineBtcHeaderFrom(t, prevHash, height, bits, ts)
}

// mineBtcHeaderFrom 挖一个指定 previousHash 的头。bootstrap 场景下首个头的 previousHash
// 必须是该网络的创世 hash（见 checkBootstrapAnchor）。
func mineBtcHeaderFrom(t *testing.T, prevHash string, height uint64, bits uint32, ts time.Time) *ltypes.BtcHeader {
	t.Helper()

	pre, err := chainhash.NewHashFromStr(prevHash)
	require.NoError(t, err)
	merkle := chainhash.DoubleHashH([]byte{byte(height), byte(height >> 8)})

	head := &wire.BlockHeader{
		Version:    1,
		PrevBlock:  *pre,
		MerkleRoot: merkle,
		Timestamp:  ts,
		Bits:       bits,
	}
	target := blockchain.CompactToBig(bits)
	for {
		hash := head.BlockHash()
		if blockchain.HashToBig(&hash).Cmp(target) <= 0 {
			return &ltypes.BtcHeader{
				Hash:          hash.String(),
				Height:        height,
				Version:       uint32(head.Version),
				MerkleRoot:    head.MerkleRoot.String(),
				Time:          head.Timestamp.Unix(),
				Nonce:         uint64(head.Nonce),
				Bits:          int64(head.Bits),
				PreviousHash:  head.PrevBlock.String(),
				Confirmations: 0,
			}
		}
		head.Nonce++
		if head.Nonce == 0 {
			head.Timestamp = head.Timestamp.Add(time.Second)
		}
	}
}

func hasKV(kvs []*types.KeyValue, key []byte) bool {
	for _, kv := range kvs {
		if bytes.Equal(kv.Key, key) {
			return true
		}
	}
	return false
}

// kvValue 取 key 对应的 value；key 不存在返回 (nil, false) 语义下的 nil（测试里只用于断言"删除"）。
func kvValue(kvs []*types.KeyValue, key []byte) []byte {
	for _, kv := range kvs {
		if bytes.Equal(kv.Key, key) {
			return kv.GetValue()
		}
	}
	return nil
}

func cloneHeader(h *ltypes.BtcHeader) *ltypes.BtcHeader {
	return &ltypes.BtcHeader{
		Hash:          h.GetHash(),
		Confirmations: h.GetConfirmations(),
		Height:        h.GetHeight(),
		Version:       h.GetVersion(),
		MerkleRoot:    h.GetMerkleRoot(),
		Time:          h.GetTime(),
		Nonce:         h.GetNonce(),
		Bits:          h.GetBits(),
		Difficulty:    h.GetDifficulty(),
		PreviousHash:  h.GetPreviousHash(),
		NextHash:      h.GetNextHash(),
	}
}

func setupTestDriver(t *testing.T, cli *lightclient) {
	t.Helper()
	api := &mocks.QueueProtocolAPI{}
	cfg := types.NewChain33Config(types.GetDefaultCfgstring())
	api.On("GetConfig").Return(cfg)
	cli.SetAPI(api)
}
