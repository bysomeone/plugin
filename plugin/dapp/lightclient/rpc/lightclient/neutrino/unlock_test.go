package neutrino

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// 本文件覆盖"放弃提现（Unlock）"的桥侧判定：什么条件下允许解锁、被签消息怎么从链上事实
// 重建、签名节点如何核对协调者的声称值，以及摘要口径能否被 btcec 验签（与链上验签同源）。

func testWithdrawPending() *rtypes.PendingTx {
	return &rtypes.PendingTx{
		ActionType:  rtypes.TyWithdrawAsset,
		TxHash:      bytes.Repeat([]byte{0x11}, 32),
		Amount:      12_345,
		FromAddress: "1OwnerAddress",
		AssetSymbol: "RGB20_USDT",
	}
}

func testWithdrawPayload() *rtypes.WithdrawAsset {
	return &rtypes.WithdrawAsset{
		Amount:          12_345,
		DestinationAddr: "rgb:invoice",
		AssetSymbol:     "rgb20_usdt", // 原始拼写：链上 Exec_Unlock 逐字比对
		FeeRate:         2,
	}
}

// Test_unlockEligible 触发判定的三条硬要求：
//   - 永久失败（unrecoverable）且没有付款签名 ⇒ 允许解锁；
//   - 瞬时失败 ⇒ 不进解锁（继续重试结算）；
//   - 已 sent / confirmed（付款签名已产出）⇒ 绝不解锁。
func Test_unlockEligible(t *testing.T) {
	permanent := errors.Join(errors.New("sidecar"),
		rgb20.NewUnrecoverableWithdrawError("asset-gone", errors.New("asset not issued")))
	transient := errors.New("sidecar unavailable")

	_, unrecoverable := rgb20.IsUnrecoverableWithdraw(permanent)
	require.True(t, unrecoverable, "permanent 错误必须被归类为不可恢复")
	_, unrecoverable = rgb20.IsUnrecoverableWithdraw(transient)
	require.False(t, unrecoverable, "瞬时错误不得被归类为不可恢复")

	cases := []struct {
		name          string
		err           error
		state         []byte
		wantUnlock    bool
		wantReasoning string
	}{
		{"permanent unsigned", permanent, nil, true, "no signature yet"},
		{"permanent broadcasted", permanent, withdrawStatusSent, false, "payment signature exists"},
		{"permanent confirmed", permanent, withdrawStatusConfirmed, false, "payment settled"},
		{"transient unsigned", transient, nil, false, "retry settlement"},
		{"transient broadcasted", transient, withdrawStatusSent, false, "retry settlement"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, unrecoverable := rgb20.IsUnrecoverableWithdraw(tc.err)
			require.Equal(t, tc.wantUnlock, unlockEligible(unrecoverable, tc.state), tc.wantReasoning)
		})
	}
}

// Test_newUnlockAsset 消息必须逐字来自链上事实：symbol 用 Withdraw payload 的原始值，
// amount/owner 与 pending（= 锁定时写的台账）一致；任何不一致都 fail-closed。
func Test_newUnlockAsset(t *testing.T) {
	pending := testWithdrawPending()
	withdraw := testWithdrawPayload()

	msg, err := newUnlockAsset(pending, withdraw)
	require.NoError(t, err)
	require.Equal(t, pending.GetTxHash(), msg.GetBurnTxHash())
	require.Equal(t, withdraw.GetAssetSymbol(), msg.GetAssetSymbol(), "must use the withdraw payload spelling")
	require.NotEqual(t, pending.GetAssetSymbol(), msg.GetAssetSymbol(), "pending symbol is normalized and must not be used")
	require.Equal(t, pending.GetAmount(), msg.GetAmount())
	require.Equal(t, pending.GetFromAddress(), msg.GetOwner())
	require.Empty(t, msg.GetUnlockSig(), "the message being signed must not carry a signature")

	t.Run("amount mismatch", func(t *testing.T) {
		bad := testWithdrawPayload()
		bad.Amount = pending.GetAmount() - 1
		_, err := newUnlockAsset(pending, bad)
		require.ErrorContains(t, err, "amount mismatch")
	})
	t.Run("already confirmed", func(t *testing.T) {
		confirmed := testWithdrawPending()
		confirmed.Confirmed = true
		_, err := newUnlockAsset(confirmed, withdraw)
		require.ErrorContains(t, err, "already confirmed")
	})
	t.Run("not a withdraw action", func(t *testing.T) {
		other := testWithdrawPending()
		other.ActionType = rtypes.TyDepositAsset
		_, err := newUnlockAsset(other, withdraw)
		require.ErrorContains(t, err, "not a withdraw")
	})
	t.Run("empty owner", func(t *testing.T) {
		noOwner := testWithdrawPending()
		noOwner.FromAddress = ""
		_, err := newUnlockAsset(noOwner, withdraw)
		require.ErrorContains(t, err, "empty owner")
	})
}

// Test_verifyUnlockClaim 签名节点只签"链上事实"：协调者声称的 symbol/amount/owner 必须与
// 按 burnTxHash 查到的 pending + Withdraw payload 完全一致，否则拒签。
func Test_verifyUnlockClaim(t *testing.T) {
	pending := testWithdrawPending()
	withdraw := testWithdrawPayload()
	claim := &unlockSignPayload{
		BurnTxHash:  hex.EncodeToString(pending.GetTxHash()),
		AssetSymbol: withdraw.GetAssetSymbol(),
		Amount:      pending.GetAmount(),
		Owner:       pending.GetFromAddress(),
	}
	require.NoError(t, verifyUnlockClaim(claim, pending, withdraw))

	t.Run("symbol mismatch", func(t *testing.T) {
		bad := *claim
		bad.AssetSymbol = pending.GetAssetSymbol()
		require.ErrorContains(t, verifyUnlockClaim(&bad, pending, withdraw), "symbol mismatch")
	})
	t.Run("amount mismatch", func(t *testing.T) {
		bad := *claim
		bad.Amount++
		require.ErrorContains(t, verifyUnlockClaim(&bad, pending, withdraw), "amount mismatch")
	})
	t.Run("owner mismatch", func(t *testing.T) {
		bad := *claim
		bad.Owner = "someone-else"
		require.ErrorContains(t, verifyUnlockClaim(&bad, pending, withdraw), "owner mismatch")
	})
	t.Run("already confirmed", func(t *testing.T) {
		confirmed := testWithdrawPending()
		confirmed.Confirmed = true
		require.ErrorContains(t, verifyUnlockClaim(claim, confirmed, withdraw), "already confirmed")
	})
	t.Run("not a withdraw", func(t *testing.T) {
		other := testWithdrawPending()
		other.ActionType = rtypes.TyDepositAsset
		require.ErrorContains(t, verifyUnlockClaim(claim, other, withdraw), "not a withdraw")
	})
}

// Test_unlockDigestVerifiable 摘要口径端到端：构造消息 → rtypes.UnlockSignDigest → btcec
// DER 签名 → 用组公钥验签。链上 verifyUnlockSig 用同一个摘要函数 + DER 解析，因此这条用例
// 证明"桥侧签的"与"链上验的"是同一份字节口径。
func Test_unlockDigestVerifiable(t *testing.T) {
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	msg, err := newUnlockAsset(testWithdrawPending(), testWithdrawPayload())
	require.NoError(t, err)

	digest := rtypes.UnlockSignDigest(msg)
	sig := ecdsa.Sign(priv, digest)
	der := sig.Serialize()

	parsed, err := ecdsa.ParseDERSignature(der)
	require.NoError(t, err)
	require.True(t, parsed.Verify(digest, priv.PubKey()), "signature must verify with the group public key")

	// unlockSig 不参与摘要（否则无法验签）。
	signed := proto.Clone(msg).(*rtypes.UnlockAsset)
	signed.UnlockSig = der
	require.Equal(t, digest, rtypes.UnlockSignDigest(signed))

	// 摘要绑定四个字段：任何一个变化都必须改变摘要（签名不能被挪用到另一笔/另一金额/另一收款人）。
	other := proto.Clone(msg).(*rtypes.UnlockAsset)
	other.Amount++
	require.NotEqual(t, digest, rtypes.UnlockSignDigest(other))
	other = proto.Clone(msg).(*rtypes.UnlockAsset)
	other.Owner = "someone-else"
	require.NotEqual(t, digest, rtypes.UnlockSignDigest(other))
	other = proto.Clone(msg).(*rtypes.UnlockAsset)
	other.AssetSymbol = "OTHER"
	require.NotEqual(t, digest, rtypes.UnlockSignDigest(other))
	other = proto.Clone(msg).(*rtypes.UnlockAsset)
	other.BurnTxHash = bytes.Repeat([]byte{0x22}, 32)
	require.NotEqual(t, digest, rtypes.UnlockSignDigest(other))
}

// Test_unlockSessionName deterministic: every signer must derive the same CGGMP session name
// from the same signed content.
func Test_unlockSessionName(t *testing.T) {
	msg, err := newUnlockAsset(testWithdrawPending(), testWithdrawPayload())
	require.NoError(t, err)
	require.Equal(t, unlockSessionName(msg), unlockSessionName(msg))

	other := proto.Clone(msg).(*rtypes.UnlockAsset)
	other.Amount++
	require.NotEqual(t, unlockSessionName(msg), unlockSessionName(other))
}
