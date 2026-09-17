package neutrino

import (
	"bytes"
	"errors"
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
	svc2.commitDKGToChain(nil)
}
