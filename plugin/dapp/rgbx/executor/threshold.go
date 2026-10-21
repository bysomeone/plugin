package executor

import (
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/33cn/chain33/types"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"google.golang.org/protobuf/proto"
)

var (
	// ErrInvalidThresholdSig invalid RGB20 TSS group signature.
	ErrInvalidThresholdSig = errors.New("invalid rgb20 threshold signature")
)

// computeDepositSignMessage 计算 RGB20 充值 TSS 签名的消息：
// C = sha256(types.Encode(DepositAsset{thresholdSig:nil}))（确定性 protobuf，B1 精化）。
// 注意：签名节点与合约必须用同一编码规则；此处对去除 thresholdSig 的 DepositAsset 做 Encode。
func computeDepositSignMessage(dep *rtypes.DepositAsset) []byte {
	depWithoutSig := proto.Clone(dep).(*rtypes.DepositAsset)
	depWithoutSig.ThresholdSig = nil
	raw := types.Encode(depWithoutSig)
	h := sha256.Sum256(raw)
	return h[:]
}

// verifyGroupSignature verifies a DER-encoded secp256k1 signature over digest with the
// compressed group public key stored in CrossChainInfo.
//
// btcec is used directly regardless of chain33 VerifyBytes, which applies another SHA256
// internally (double-hashing trap). Every malformed/missing input is reported as
// ErrInvalidThresholdSig so callers only need to distinguish "valid" from "not valid".
func verifyGroupSignature(pub, sig, digest []byte) error {
	if len(sig) == 0 {
		elog.Error("verifyGroupSignature empty sig")
		return ErrInvalidThresholdSig
	}
	if len(pub) == 0 {
		elog.Error("verifyGroupSignature empty pubkey")
		return ErrInvalidThresholdSig
	}
	pubKey, err := btcec.ParsePubKey(pub)
	if err != nil {
		elog.Error("verifyGroupSignature parse pubkey", "err", err)
		return ErrInvalidThresholdSig
	}
	signature, err := ecdsa.ParseDERSignature(sig)
	if err != nil {
		elog.Error("verifyGroupSignature parse sig", "err", err)
		return ErrInvalidThresholdSig
	}
	if !signature.Verify(digest, pubKey) {
		elog.Error("verifyGroupSignature verify failed", "digest", fmt.Sprintf("%x", digest))
		return ErrInvalidThresholdSig
	}
	return nil
}

// verifyThresholdSig verifies the RGB20 deposit TSS signature over computeDepositSignMessage.
// pub is the TSS group public key (compressed secp256k1) from CrossChainInfo.
func verifyThresholdSig(pub []byte, dep *rtypes.DepositAsset) error {
	if err := verifyGroupSignature(pub, dep.GetThresholdSig(), computeDepositSignMessage(dep)); err != nil {
		elog.Error("verifyThresholdSig failed", "symbol", dep.GetAssetSymbol(), "err", err)
		return err
	}
	return nil
}

// verifyUnlockSig verifies the TSS group signature authorizing a withdrawal unlock.
// The digest is derived by rtypes.UnlockSignDigest, which domain-separates it from the
// deposit signature so a deposit signature can never be replayed as an unlock (and vice versa).
func verifyUnlockSig(pub []byte, msg *rtypes.UnlockAsset) error {
	if err := verifyGroupSignature(pub, msg.GetUnlockSig(), rtypes.UnlockSignDigest(msg)); err != nil {
		elog.Error("verifyUnlockSig failed", "symbol", msg.GetAssetSymbol(), "err", err)
		return err
	}
	return nil
}
