package executor

import (
	"bytes"
	"errors"
	"testing"

	"github.com/33cn/chain33/client/mocks"
	"github.com/33cn/chain33/common/merkle"
	"github.com/33cn/chain33/types"
	"github.com/33cn/chain33/util"
	ltypes "github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func Test_btcProof2String_and_merkleProof2String(t *testing.T) {
	proof := &rtypes.BtcTxProof{
		BlockHeight: 12,
		BlockHash:   "abc",
		TxIndex:     3,
		TxData:      []byte{1, 2},
	}
	s := btcProof2String(proof)
	require.Contains(t, s, "12")
	require.Contains(t, s, "abc")
	require.Contains(t, s, "3")
	require.Contains(t, s, "0102")

	require.Equal(t, "", merkleProof2String(nil))
	require.Contains(t, merkleProof2String([][]byte{{0xaa}, {0xbb}}), "aa")
}

// Test_hasWithdrawCommitment 提现侧的 OP_RETURN 承诺仍保留（充值侧已随硬切删除）。
func Test_hasWithdrawCommitment(t *testing.T) {
	data := []byte("rgbx:test")
	script, err := txscript.NullDataScript(data)
	require.NoError(t, err)
	tx := &wire.MsgTx{}
	tx.TxOut = append(tx.TxOut, wire.NewTxOut(0, script))
	require.True(t, hasExpectedOpReturnData(tx, data))
	require.False(t, hasExpectedOpReturnData(tx, []byte("other")))

	chain33Hash := []byte{9, 8, 7}
	wdData := append([]byte(withdrawCommitmentPrefix), chain33Hash...)
	wdScript, err := txscript.NullDataScript(wdData)
	require.NoError(t, err)
	txWd := &wire.MsgTx{}
	txWd.TxOut = append(txWd.TxOut, wire.NewTxOut(0, wdScript))
	require.True(t, hasWithdrawCommitment(txWd, chain33Hash))

	// 充值承诺前缀已删除：同样形态的 rgbx:deposit: 输出不再被任何人认（只作为历史记录）
	depData := append([]byte("rgbx:deposit:"), []byte("addr")...)
	depScript, err := txscript.NullDataScript(depData)
	require.NoError(t, err)
	txDep := &wire.MsgTx{}
	txDep.TxOut = append(txDep.TxOut, wire.NewTxOut(0, depScript))
	require.False(t, hasExpectedOpReturnData(txDep, append([]byte(withdrawCommitmentPrefix), []byte("addr")...)))
}

// Test_rgbx_validateDepositTxContent P2WSH 充值绑定：按 (充值地址, tssPub) 重建 program 再核金额。
//
// 用例覆盖 C0 §2.2 的单测要点：派生一致 / 通过 / 金额差 1 sat / 另一个用户 / 另一把 tssPub /
// 一笔 tx 付两个用户各自只算自己那份 / 非规范（非派生脚本）输出不被计入。
func Test_rgbx_validateDepositTxContent(t *testing.T) {
	r := newRgbx()
	dir, state, _ := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	api := &mocks.QueueProtocolAPI{}
	r.SetAPI(api)
	api.On("GetConfig").Return(types.NewChain33Config(types.GetDefaultCfgstring()))
	r.SetStateDB(state)

	userA := newP2WSHDepositFixture(t, frozenVectorA)
	userB := newP2WSHDepositFixture(t, frozenVectorB)

	btcTx := &wire.MsgTx{}
	btcTx.TxOut = append(btcTx.TxOut,
		wire.NewTxOut(600, userA.pkScript),
		wire.NewTxOut(400, userB.pkScript),
		wire.NewTxOut(5000, []byte{0x51}), // 与任何用户的 P2WSH 无关的输出
	)

	// 1) 没有 CrossChainInfo → 取不到 tssPub，直接拒
	err := r.(*rgbx).validateDepositTxContent("h1", userA.deposit(600), btcTx)
	require.Equal(t, ErrGetCrossChainInfo, err)

	// 2) CrossChainInfo 存在但没有 pubkey（旧状态/未带 pubkey 的 CommitDKG）→ 拒，且不留兼容分支
	require.NoError(t, state.Set(formatCrossChainInfoKey("btc"), types.Encode(&rtypes.CrossChainInfo{
		AssetSymbol: "BTC", PkScript: []byte{0x51},
	})))
	err = r.(*rgbx).validateDepositTxContent("h1", userA.deposit(600), btcTx)
	require.Equal(t, ErrInvalidCrossChainInfo, err)

	// 3) pubkey 合法（= 用户 A 的那把）：各自只算自己那份
	require.NoError(t, state.Set(formatCrossChainInfoKey("btc"), types.Encode(userA.crossChainInfo([]byte{0x51}))))
	require.NoError(t, r.(*rgbx).validateDepositTxContent("h1", userA.deposit(600), btcTx))

	// 4) 用户 B 在同一个 symbol 下：另一把 tssPub ⇒ 执行器重建不出 B 的脚本（B 的地址属于另一个 symbol）
	err = r.(*rgbx).validateDepositTxContent("h1", userB.deposit(400), btcTx)
	require.Equal(t, ErrInvalidDepositScript, err)

	// 5) 金额多 1 / 少 1 sat → ErrInvalidDepositAmount（分母只含该用户自己的那段）
	err = r.(*rgbx).validateDepositTxContent("h1", userA.deposit(601), btcTx)
	require.Equal(t, ErrInvalidDepositAmount, err)
	err = r.(*rgbx).validateDepositTxContent("h1", userA.deposit(599), btcTx)
	require.Equal(t, ErrInvalidDepositAmount, err)

	// 6) 把与本次充值无关的输出（付给别的脚本）算进来也没用：金额只看派生脚本那段
	bigTx := &wire.MsgTx{}
	bigTx.TxOut = append(bigTx.TxOut,
		wire.NewTxOut(600, userA.pkScript),
		wire.NewTxOut(100000, []byte{0x51}),
	)
	require.NoError(t, r.(*rgbx).validateDepositTxContent("h1", userA.deposit(600), bigTx))
	require.Equal(t, ErrInvalidDepositAmount, r.(*rgbx).validateDepositTxContent("h1", userA.deposit(100600), bigTx))

	// 7) 完全没有付给该用户的输出 → ErrInvalidDepositScript（与"金额不对"区分开）
	noOutput := &wire.MsgTx{}
	noOutput.TxOut = append(noOutput.TxOut, wire.NewTxOut(600, userB.pkScript))
	err = r.(*rgbx).validateDepositTxContent("h1", userA.deposit(600), noOutput)
	require.Equal(t, ErrInvalidDepositScript, err)

	// 8) 同一地址、旧世代的 tssPub（改写 CrossChainInfo 模拟 re-key）→ 重建不出原脚本 → 拒
	oldGen := newP2WSHDepositFixture(t, frozenVectorB)
	info := userA.crossChainInfo([]byte{0x51})
	info.Pubkey = oldGen.tssPub
	require.NoError(t, state.Set(formatCrossChainInfoKey("btc"), types.Encode(info)))
	err = r.(*rgbx).validateDepositTxContent("h1", userA.deposit(600), noOutput)
	require.Equal(t, ErrInvalidDepositScript, err)
}

func Test_rgbx_validateWithdrawTxContent(t *testing.T) {
	r := newRgbx()
	dir, state, _ := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	api := &mocks.QueueProtocolAPI{}
	r.SetAPI(api)
	api.On("GetConfig").Return(types.NewChain33Config(types.GetDefaultCfgstring()))
	api.On("Query", ltypes.LightclientX, "GetBtcNetName", mock.Anything).Return(&types.ReplyString{Data: "testnet3"}, nil)
	r.SetStateDB(state)

	destAddr, destScript := newTestnetWitnessAddr(t)
	tssScript := []byte{0x51, 0x20, 0xab, 0xcd}

	err := r.(*rgbx).validateWithdrawTxContent("h1", nil, &wire.MsgTx{})
	require.Equal(t, ErrPendingTxNotExist, err)

	pending := &rtypes.PendingTx{AssetSymbol: "btc", TargetAddress: destAddr, Amount: 50000}
	err = r.(*rgbx).validateWithdrawTxContent("h1", pending, &wire.MsgTx{})
	require.Equal(t, ErrInvalidCrossChainInfo, err)

	require.NoError(t, state.Set(formatCrossChainInfoKey("btc"), types.Encode(&rtypes.CrossChainInfo{
		AssetSymbol: "BTC",
		PkScript:    tssScript,
	})))

	pendingBad := &rtypes.PendingTx{AssetSymbol: "btc", TargetAddress: "not-valid", Amount: 50000}
	err = r.(*rgbx).validateWithdrawTxContent("h1", pendingBad, &wire.MsgTx{})
	require.Equal(t, ErrInvalidWithdrawDestination, err)

	btcTx := &wire.MsgTx{}
	btcTx.TxOut = append(btcTx.TxOut,
		wire.NewTxOut(40000, destScript),
		wire.NewTxOut(10000, []byte{0x76}), // unexpected script
	)
	err = r.(*rgbx).validateWithdrawTxContent("h1", pending, btcTx)
	require.Equal(t, ErrInvalidWithdrawDestinationScript, err)

	btcTx2 := &wire.MsgTx{}
	btcTx2.TxOut = append(btcTx2.TxOut,
		wire.NewTxOut(40000, destScript),
		wire.NewTxOut(10000, tssScript),
	)
	err = r.(*rgbx).validateWithdrawTxContent("h1", pending, btcTx2)
	require.NoError(t, err)

	btcTx3 := &wire.MsgTx{}
	btcTx3.TxOut = append(btcTx3.TxOut, wire.NewTxOut(0, destScript)) // destAmount 0
	btcTx3.TxOut = append(btcTx3.TxOut, wire.NewTxOut(60000, tssScript))
	err = r.(*rgbx).validateWithdrawTxContent("h1", pending, btcTx3)
	require.Equal(t, ErrInvalidWithdrawAmount, err)

	btcTx4 := &wire.MsgTx{}
	btcTx4.TxOut = append(btcTx4.TxOut, wire.NewTxOut(60000, destScript)) // > pending amount
	btcTx4.TxOut = append(btcTx4.TxOut, wire.NewTxOut(10000, tssScript))
	err = r.(*rgbx).validateWithdrawTxContent("h1", pending, btcTx4)
	require.Equal(t, ErrInvalidWithdrawAmount, err)
}

func newBtcTxProofFixture(t *testing.T) (*rgbx, *rtypes.BtcTxProof, string) {
	t.Helper()
	r := newRgbx().(*rgbx)
	var btcTx wire.MsgTx
	btcTx.Version = 2
	btcTx.TxOut = append(btcTx.TxOut, wire.NewTxOut(1000, []byte{0x51}))
	buf := new(bytes.Buffer)
	require.NoError(t, btcTx.SerializeNoWitness(buf))
	txID := btcTx.TxHash()
	leaves := [][]byte{txID.CloneBytes()}
	root := merkle.GetMerkleRoot(leaves)
	_, branch := merkle.GetMerkleRootAndBranch(leaves, 0)
	rootHash, err := chainhash.NewHash(root)
	require.NoError(t, err)
	proof := &rtypes.BtcTxProof{
		TxData:      buf.Bytes(),
		BlockHeight: 100,
		BlockHash:   "deadbeef",
		TxIndex:     0,
		MerkleProof: branch,
	}
	return r, proof, rootHash.String()
}

func Test_rgbx_validateBtcTxProof_emptyAndDecode(t *testing.T) {
	r := newRgbx().(*rgbx)
	_, err := r.validateBtcTxProof("tx1", nil)
	require.Equal(t, ErrInvalidBtcTxProof, err)
	_, err = r.validateBtcTxProof("tx1", &rtypes.BtcTxProof{TxData: []byte{0xff}})
	require.Equal(t, ErrInvalidBtcTxProof, err)
}

func Test_rgbx_validateBtcTxProof_getHeaderError(t *testing.T) {
	r, proof, _ := newBtcTxProofFixture(t)
	api := &mocks.QueueProtocolAPI{}
	api.On("GetConfig").Return(types.NewChain33Config(types.GetDefaultCfgstring()))
	api.On("Query", ltypes.LightclientX, "GetBtcHeader", mock.MatchedBy(func(req types.Message) bool {
		h, ok := req.(*ltypes.ReqGetBtcHeader)
		return ok && h != nil && h.Height == 100
	})).Return(nil, errors.New("nope"))
	r.SetAPI(api)
	_, err := r.validateBtcTxProof("tx1", proof)
	require.Equal(t, ErrGetBtcHeader, err)
}

func Test_rgbx_validateBtcTxProof_headerMismatch(t *testing.T) {
	r, proof, rootStr := newBtcTxProofFixture(t)
	api := &mocks.QueueProtocolAPI{}
	api.On("GetConfig").Return(types.NewChain33Config(types.GetDefaultCfgstring()))
	api.On("Query", ltypes.LightclientX, "GetBtcHeader", mock.Anything).Return(&ltypes.BtcHeader{
		Hash:       rootStr,
		Height:     101,
		MerkleRoot: rootStr,
	}, nil)
	r.SetAPI(api)
	_, err := r.validateBtcTxProof("tx1", proof)
	require.Equal(t, ErrInvalidBtcProofBlock, err)
}

func Test_rgbx_validateBtcTxProof_headerHashMismatch(t *testing.T) {
	r, proof, rootStr := newBtcTxProofFixture(t)
	api := &mocks.QueueProtocolAPI{}
	api.On("GetConfig").Return(types.NewChain33Config(types.GetDefaultCfgstring()))
	api.On("Query", ltypes.LightclientX, "GetBtcHeader", mock.Anything).Return(&ltypes.BtcHeader{
		Hash:       rootStr,
		Height:     100,
		MerkleRoot: rootStr,
	}, nil)
	r.SetAPI(api)
	_, err := r.validateBtcTxProof("tx1", proof)
	require.Equal(t, ErrInvalidBtcProofBlock, err)
}

func Test_rgbx_validateBtcTxProof_invalidMerkleRootStr(t *testing.T) {
	r, proof, _ := newBtcTxProofFixture(t)
	api := &mocks.QueueProtocolAPI{}
	api.On("GetConfig").Return(types.NewChain33Config(types.GetDefaultCfgstring()))
	api.On("Query", ltypes.LightclientX, "GetBtcHeader", mock.Anything).Return(&ltypes.BtcHeader{
		Hash:       "deadbeef",
		Height:     100,
		MerkleRoot: "gg",
	}, nil)
	r.SetAPI(api)
	_, err := r.validateBtcTxProof("tx1", proof)
	require.Equal(t, ErrInvalidBtcProofMerkle, err)
}

func Test_rgbx_validateBtcTxProof_invalidHeaderType(t *testing.T) {
	r, proof, _ := newBtcTxProofFixture(t)
	api := &mocks.QueueProtocolAPI{}
	api.On("GetConfig").Return(types.NewChain33Config(types.GetDefaultCfgstring()))
	api.On("Query", ltypes.LightclientX, "GetBtcHeader", mock.Anything).Return(&types.ReplyString{Data: "x"}, nil)
	r.SetAPI(api)
	_, err := r.validateBtcTxProof("tx1", proof)
	require.Equal(t, ErrGetBtcHeader, err)
}

func Test_rgbx_validateBtcTxProof_merkleBranchMismatch(t *testing.T) {
	r, proof, rootStr := newBtcTxProofFixture(t)
	badProof := &rtypes.BtcTxProof{
		TxData:      proof.TxData,
		BlockHeight: proof.BlockHeight,
		BlockHash:   proof.BlockHash,
		TxIndex:     proof.TxIndex,
		MerkleProof: [][]byte{bytes.Repeat([]byte{1}, 32)},
	}
	api := &mocks.QueueProtocolAPI{}
	api.On("GetConfig").Return(types.NewChain33Config(types.GetDefaultCfgstring()))
	api.On("Query", ltypes.LightclientX, "GetBtcHeader", mock.Anything).Return(&ltypes.BtcHeader{
		Hash:       "deadbeef",
		Height:     100,
		MerkleRoot: rootStr,
	}, nil)
	r.SetAPI(api)
	_, err := r.validateBtcTxProof("tx1", badProof)
	require.Equal(t, ErrInvalidBtcProofMerkle, err)
}

func Test_rgbx_validateBtcTxProof_success(t *testing.T) {
	r, proof, rootStr := newBtcTxProofFixture(t)
	api := &mocks.QueueProtocolAPI{}
	api.On("GetConfig").Return(types.NewChain33Config(types.GetDefaultCfgstring()))
	api.On("Query", ltypes.LightclientX, "GetBtcHeader", mock.Anything).Return(&ltypes.BtcHeader{
		Hash:       "deadbeef",
		Height:     100,
		MerkleRoot: rootStr,
	}, nil)
	r.SetAPI(api)
	tx, err := r.validateBtcTxProof("tx1", proof)
	require.NoError(t, err)
	require.Equal(t, int32(2), tx.Version)
}

func Test_rgbx_checkWithdrawConfirm(t *testing.T) {
	r := newRgbx()
	api := &mocks.QueueProtocolAPI{}
	r.SetAPI(api)
	cfg := types.NewChain33Config(types.GetDefaultCfgstring())
	api.On("GetConfig").Return(cfg)
	api.On("Query", ltypes.LightclientX, "GetBtcNetName", mock.Anything).Return(&types.ReplyString{Data: "testnet3"}, nil)

	confirm := &rtypes.ConfirmTx{TxHash: []byte{1, 2, 3}}
	pending := &rtypes.PendingTx{AssetSymbol: "btc", Amount: 1000}
	destAddr, destPk := newTestnetWitnessAddr(t)
	pending.TargetAddress = destAddr
	tssScript := []byte{0x51, 0x20, 0x11, 0x22}

	dir, state, _ := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	r.SetStateDB(state)
	require.NoError(t, state.Set(formatCrossChainInfoKey("btc"), types.Encode(&rtypes.CrossChainInfo{
		AssetSymbol: "BTC",
		PkScript:    tssScript,
	})))

	commitData := append([]byte(withdrawCommitmentPrefix), confirm.GetTxHash()...)
	commitScript, err := txscript.NullDataScript(commitData)
	require.NoError(t, err)
	var btcTx wire.MsgTx
	btcTx.Version = 2
	btcTx.TxOut = append(btcTx.TxOut,
		wire.NewTxOut(0, commitScript),
		wire.NewTxOut(500, destPk),
		wire.NewTxOut(500, tssScript),
	)
	buf := new(bytes.Buffer)
	require.NoError(t, btcTx.SerializeNoWitness(buf))
	txID := btcTx.TxHash()
	leaves := [][]byte{txID.CloneBytes()}
	root := merkle.GetMerkleRoot(leaves)
	_, branch := merkle.GetMerkleRootAndBranch(leaves, 0)
	rootHash, err := chainhash.NewHash(root)
	require.NoError(t, err)
	proof := &rtypes.BtcTxProof{
		TxData:      buf.Bytes(),
		BlockHeight: 1,
		BlockHash:   "dead",
		TxIndex:     0,
		MerkleProof: branch,
	}
	confirm.BtcTxProof = proof

	api.On("Query", ltypes.LightclientX, "GetBtcHeader", mock.Anything).Return(&ltypes.BtcHeader{
		Hash:       "dead",
		Height:     1,
		MerkleRoot: rootHash.String(),
	}, nil)
	// B8：链上最小确认数（tip >= 1 + N - 1）。给足深度；深度规则本身见 btc_confirm_test.go。
	tip := &ltypes.BtcHeader{Hash: "tip", Height: 1 + uint64(defaultMinBtcConfirmations) - 1}
	api.On("Query", ltypes.LightclientX, "GetBtcLastHeader", mock.Anything).Return(tip, nil)

	err = r.(*rgbx).checkWithdrawConfirm("a", "b", confirm, pending)
	require.NoError(t, err)

	confirmMismatch := &rtypes.ConfirmTx{TxHash: []byte{9, 9, 9}, BtcTxProof: proof}
	err = r.(*rgbx).checkWithdrawConfirm("a", "b", confirmMismatch, pending)
	require.Equal(t, ErrInvalidBtcProofCommitment, err)

	// B8：承诺（OP_RETURN）是**永久性**判定、深度是**暂时性**判定；深度不足/头链回退时也必须先报承诺错误，
	// 否则"这份证明永远无效"会被报成"确认不足"，把运维引向等待。
	tip.Height = 0
	require.Equal(t, ErrInvalidBtcProofCommitment, r.(*rgbx).checkWithdrawConfirm("a", "b", confirmMismatch, pending))

	// 深度不足（tip = proof.Height - 1）时，合法证明同样被拒。
	require.ErrorIs(t, r.(*rgbx).checkWithdrawConfirm("a", "b", confirm, pending), ErrInsufficientBtcConfirmations)
	// 刚好满足（tip == proof.Height + N - 1）→ 通过。
	tip.Height = 1 + uint64(defaultMinBtcConfirmations) - 1
	require.NoError(t, r.(*rgbx).checkWithdrawConfirm("a", "b", confirm, pending))
	// 少 1 个确认 → 拒。
	tip.Height = 1 + uint64(defaultMinBtcConfirmations) - 2
	require.ErrorIs(t, r.(*rgbx).checkWithdrawConfirm("a", "b", confirm, pending), ErrInsufficientBtcConfirmations)
}

// Test_checkWithdrawConfirm_burnReplayGuard S3：同一笔提现 burn 只能结算一次。
// 走真实 CheckTx 路径——dapp CheckTx 在出块执行阶段被每个节点重跑（chain33 executor/execenv.go Exec → CheckTx），
// 是共识强制点，因此这里的拒绝等价于"该 burn 不可能被放款两次"。
func Test_checkWithdrawConfirm_burnReplayGuard(t *testing.T) {
	r := newRgbx()
	tx := &types.Transaction{}
	tx.Sign(types.SECP256K1, testPriv) // from == rgbxCfg.CommitAddress

	// 单交易区块的有效 SPV：header.merkleRoot == btc txid
	btcTx := wire.NewMsgTx(wire.TxVersion)
	btcTx.AddTxOut(wire.NewTxOut(1000, []byte{txscript.OP_0, 0x14}))
	var buf bytes.Buffer
	require.NoError(t, btcTx.SerializeNoWitness(&buf))
	txid := btcTx.TxHash()

	api := mockGuardianAPI(t, testCommitAddr)
	api.On("Query", ltypes.LightclientX, "GetBtcHeader", mock.Anything).Return(
		&ltypes.BtcHeader{Hash: "hash1", Height: 100, MerkleRoot: txid.String()}, nil)
	dir, state, local := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	r.SetAPI(api)
	r.SetStateDB(state)
	r.SetLocalDB(local)

	burnA := []byte("burnA")
	burnB := []byte("burnB")
	withdraw := &rtypes.WithdrawAsset{AssetSymbol: rtypes.RGB20USDTSymbol, Amount: 100}
	for i, burn := range [][]byte{burnA, burnB} {
		require.NoError(t, state.Set(formatPayloadKey(burn), types.Encode(withdraw)))
		require.NoError(t, local.Set(formatPendingTxKey(3, int64(i)), types.Encode(&rtypes.PendingTx{
			ActionType:    rtypes.TyWithdrawAsset,
			TxBlockHeight: 3,
			TxIndex:       int64(i),
			TxHash:        burn,
			AssetSymbol:   rtypes.RGB20USDTSymbol,
		})))
	}

	action := &rtypes.RgbxAction{}
	action.Ty = rtypes.TyConfirmAction
	value := &rtypes.RgbxAction_Confirm{}
	action.Value = value
	confirmOf := func(burn []byte, idx int) *rtypes.ConfirmTx {
		return &rtypes.ConfirmTx{
			ActionType:    rtypes.TyWithdrawAsset,
			TxBlockHeight: 3,
			TxIndex:       int64(idx),
			TxHash:        burn,
			BtcTxProof: &rtypes.BtcTxProof{
				TxData:      buf.Bytes(),
				BlockHeight: 100,
				BlockHash:   "hash1",
				TxIndex:     0,
			},
		}
	}

	// 1) 首次确认通过（等价于升级时链上已存在的历史 pending：无 consumed 标记不构成误判）
	value.Confirm = confirmOf(burnA, 0)
	tx.Payload = types.Encode(action)
	require.NoError(t, r.CheckTx(tx, 0))

	// 2) 结算登记后，同一 burn 再次确认 → 明确错误码拒绝
	require.NoError(t, state.Set(formatWithdrawUsedKey(burnA), []byte("used")))
	tx.Payload = types.Encode(action)
	require.Equal(t, ErrWithdrawAlreadyConfirmed, r.CheckTx(tx, 0))

	// 3) 不同 burn 互不影响
	value.Confirm = confirmOf(burnB, 1)
	tx.Payload = types.Encode(action)
	require.NoError(t, r.CheckTx(tx, 0))
}

func Test_rgbx_decodeBtcAddressScript_empty(t *testing.T) {
	r := newRgbx()
	_, err := r.(*rgbx).decodeBtcAddressScript("")
	require.Equal(t, types.ErrInvalidAddress, err)
}

func Test_rgbx_getBtcNetName_queryError(t *testing.T) {
	r := newRgbx()
	api := &mocks.QueueProtocolAPI{}
	r.SetAPI(api)
	api.On("Query", ltypes.LightclientX, "GetBtcNetName", mock.Anything).Return(nil, errors.New("down"))
	_, err := r.(*rgbx).getBtcNetName()
	require.Error(t, err)
}
