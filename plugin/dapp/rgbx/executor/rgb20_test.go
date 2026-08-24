package executor

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"github.com/33cn/chain33/client/mocks"
	"github.com/33cn/chain33/types"
	"github.com/33cn/chain33/util"
	ltypes "github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
	paratypes "github.com/33cn/plugin/plugin/dapp/paracross/types"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func testNetParams() *chaincfg.Params {
	return &chaincfg.TestNet3Params
}

func mockGuardianAPI(t *testing.T, commitAddr string) *mocks.QueueProtocolAPI {
	t.Helper()
	api := &mocks.QueueProtocolAPI{}
	api.On("GetConfig").Return(testCfg)
	api.On("Query", ltypes.LightclientX, "GetBtcNetName", mock.Anything).Return(&types.ReplyString{Data: "testnet3"}, nil)
	api.On("Query", paratypes.ParaX, "GetNodeGroupStatus", mock.Anything).Return(
		&paratypes.ParaNodeGroupStatus{TargetAddrs: commitAddr}, nil)
	return api
}

func buildMinimalBtcTx(t *testing.T) []byte {
	t.Helper()
	var tx wire.MsgTx
	tx.Version = 2
	buf := bytes.NewBuffer(make([]byte, 0, tx.SerializeSizeStripped()))
	require.NoError(t, tx.SerializeNoWitness(buf))
	return buf.Bytes()
}

// Test_thresholdSig_verify 验证 threshold_sig 验签路径（B1：btcec 直验 C，禁用 VerifyBytes 双重哈希）。
func Test_thresholdSig_verify(t *testing.T) {
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pub := priv.PubKey().SerializeCompressed()

	dep := &rtypes.DepositAsset{
		Amount:         1000,
		DepositAddress: "addr",
		AssetSymbol:    rtypes.RGB20USDTSymbol,
		TxProof: &rtypes.BtcTxProof{
			TxData:      []byte("txdata"),
			BlockHash:   "hash",
			BlockHeight: 100,
			TxIndex:     0,
		},
	}
	msg := computeDepositSignMessage(dep)
	require.Len(t, msg, 32)

	sig := ecdsa.Sign(priv, msg)
	der := sig.Serialize()

	good := proto.Clone(dep).(*rtypes.DepositAsset)
	good.ThresholdSig = der
	require.NoError(t, verifyThresholdSig(pub, good))

	// 签名错误应失败（截断的 DER 无法解析）
	bad := proto.Clone(dep).(*rtypes.DepositAsset)
	bad.ThresholdSig = der[:len(der)-1]
	require.Error(t, verifyThresholdSig(pub, bad))

	// 空签名/空公钥失败
	empty := proto.Clone(dep).(*rtypes.DepositAsset)
	require.Error(t, verifyThresholdSig(pub, empty))
	require.Error(t, verifyThresholdSig(nil, good))
}

// Test_computeDepositSignMessage_deterministic 消息确定性：去掉 threshold_sig 前后一致。
func Test_computeDepositSignMessage_deterministic(t *testing.T) {
	dep := &rtypes.DepositAsset{
		Amount:         1,
		DepositAddress: "a",
		AssetSymbol:    rtypes.RGB20USDTSymbol,
		TxProof:        &rtypes.BtcTxProof{TxData: []byte("x")},
	}
	m1 := computeDepositSignMessage(dep)
	dep.ThresholdSig = []byte("sig")
	m2 := computeDepositSignMessage(dep)
	require.Equal(t, m1, m2)

	// 与 sha256(types.Encode(DepositAsset{threshold_sig:nil})) 一致
	dep.ThresholdSig = nil
	raw := types.Encode(dep)
	h := sha256.Sum256(raw)
	require.Equal(t, h[:], m1)
}

// Test_formatDkgConfirmationsKey_bySymbol BL-2：同地址不同 symbol 的确认集合互不污染。
func Test_formatDkgConfirmationsKey_bySymbol(t *testing.T) {
	addr := "bcrt1qtsaddress"
	btcKey := formatDkgConfirmationsKey("BTC", addr)
	rgbKey := formatDkgConfirmationsKey(rtypes.RGB20USDTSymbol, addr)
	require.NotEqual(t, btcKey, rgbKey)
	require.NotEqual(t, btcKey, formatDkgConfirmationsKey("BTC", "other"))
}

// Test_checkCommitDKG_rgb20_pubkey RGB20 CommitDKG 校验 hash160(pubkey)==pkScript[2:]。
func Test_checkCommitDKG_rgb20_pubkey(t *testing.T) {
	r := newRgbx()
	action := &rtypes.RgbxAction{}
	action.Ty = rtypes.TyCommitDKGAction
	tx := &types.Transaction{}
	tx.Sign(types.SECP256K1, testPriv)
	value := &rtypes.RgbxAction_CommitDKG{}
	action.Value = value

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pub := priv.PubKey().SerializeCompressed()
	pubHash := btcutil.Hash160(pub)
	// P2WPKH script：OP_0 PUSH20 <hash>
	pkScript := append([]byte{txscript.OP_0, 0x14}, pubHash...)
	waddr, err := btcutil.NewAddressWitnessPubKeyHash(pubHash, testNetParams())
	require.NoError(t, err)

	otherPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	tcArr := []struct {
		name     string
		pubkey   []byte
		pkScript []byte
		wantErr  error
	}{
		{"ok", pub, pkScript, nil},
		{"empty pubkey", nil, pkScript, ErrInvalidDkgAddress},
		{"wrong pubkey", otherPriv.PubKey().SerializeCompressed(), pkScript, ErrInvalidDkgAddress},
	}

	dir, state, _ := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	api := mockGuardianAPI(t, testCommitAddr)
	r.SetAPI(api)
	r.SetStateDB(state)

	for _, tc := range tcArr {
		t.Run(tc.name, func(t *testing.T) {
			value.CommitDKG = &rtypes.CommitDKG{
				AssetSymbol: rtypes.RGB20USDTSymbol,
				DkgAddress:  waddr.String(),
				PkScript:    tc.pkScript,
				Pubkey:      tc.pubkey,
			}
			tx.Payload = types.Encode(action)
			err := r.CheckTx(tx, 0)
			require.Equal(t, tc.wantErr, err)
		})
	}
}

// Test_checkDeposit_rgb20_thresholdSig RGB20 deposit 跳过 commitment、验 threshold_sig。
func Test_checkDeposit_rgb20_thresholdSig(t *testing.T) {
	r := newRgbx()
	action := &rtypes.RgbxAction{}
	action.Ty = rtypes.TyDepositAsset
	tx := &types.Transaction{}
	value := &rtypes.RgbxAction_Deposit{}
	action.Value = value

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pub := priv.PubKey().SerializeCompressed()
	depAddr, _ := util.Genaddress()

	// 构造有效的 SPV：单交易区块，header.merkleRoot == txid。
	btcTx := wire.NewMsgTx(wire.TxVersion)
	btcTx.AddTxOut(wire.NewTxOut(1000, []byte{txscript.OP_0, 0x14}))
	var buf bytes.Buffer
	require.NoError(t, btcTx.SerializeNoWitness(&buf))
	txData := buf.Bytes()
	txid := btcTx.TxHash()

	dep := &rtypes.DepositAsset{
		Amount:         1000,
		DepositAddress: depAddr,
		AssetSymbol:    rtypes.RGB20USDTSymbol,
		TxProof: &rtypes.BtcTxProof{
			TxData:      txData,
			BlockHeight: 100,
			BlockHash:   "hash1",
			TxIndex:     0,
			MerkleProof: nil,
		},
	}
	msg := computeDepositSignMessage(dep)
	sig := ecdsa.Sign(priv, msg)
	dep.ThresholdSig = sig.Serialize()

	dir, state, _ := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	api := mockGuardianAPI(t, testCommitAddr)
	api.On("Query", ltypes.LightclientX, "GetBtcHeader", mock.Anything).Return(
		&ltypes.BtcHeader{Hash: "hash1", Height: 100, MerkleRoot: txid.String()}, nil)
	r.SetAPI(api)
	r.SetStateDB(state)
	require.Nil(t, state.Set(formatCrossChainInfoKey(rtypes.RGB20USDTSymbol), types.Encode(&rtypes.CrossChainInfo{
		AssetSymbol: rtypes.RGB20USDTSymbol,
		Pubkey:      pub,
	})))

	// RGB20 分支：SPV 通过 + threshold_sig 验签通过，CheckTx 返回 nil。
	value.Deposit = dep
	tx.Payload = types.Encode(action)
	require.NoError(t, r.CheckTx(tx, 0))

	// 篡改 threshold_sig 应失败
	bad := proto.Clone(dep).(*rtypes.DepositAsset)
	bad.ThresholdSig = []byte("bad")
	value.Deposit = bad
	tx.Payload = types.Encode(action)
	require.ErrorIs(t, r.CheckTx(tx, 0), ErrInvalidThresholdSig)
}

// Test_execCommitDKG_rgb20_pubkey Exec_CommitDKG 存 pubkey，形成 CrossChainInfo。
func Test_execCommitDKG_rgb20_pubkey(t *testing.T) {
	r := newRgbx().(*rgbx)
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pub := priv.PubKey().SerializeCompressed()
	pubHash := btcutil.Hash160(pub)
	pkScript := append([]byte{txscript.OP_0, 0x14}, pubHash...)
	waddr, err := btcutil.NewAddressWitnessPubKeyHash(pubHash, testNetParams())
	require.NoError(t, err)

	dir, state, _ := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	api := mockGuardianAPI(t, testCommitAddr)
	r.SetAPI(api)
	r.SetStateDB(state)

	commit := &rtypes.CommitDKG{
		AssetSymbol: rtypes.RGB20USDTSymbol,
		DkgAddress:  waddr.String(),
		PkScript:    pkScript,
		Pubkey:      pub,
	}
	tx := &types.Transaction{}
	// tx.From() 必须是守护者地址（testCommitAddr）。
	tx.Sign(types.SECP256K1, testPriv)

	receipt, err := r.Exec_CommitDKG(commit, tx, 0)
	require.NoError(t, err)
	// 直接调用 Exec_XXX 不会自动落 receipt KV，需手动应用到 state DB。
	for _, kv := range receipt.KV {
		require.NoError(t, state.Set(kv.Key, kv.Value))
	}

	info := &rtypes.CrossChainInfo{}
	err = readDB(r.GetStateDB(), formatCrossChainInfoKey(rtypes.RGB20USDTSymbol), info)
	require.NoError(t, err)
	require.Equal(t, pub, info.GetPubkey())
	require.Equal(t, waddr.String(), info.GetTssAddress())
}
