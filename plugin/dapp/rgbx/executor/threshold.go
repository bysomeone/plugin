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
	// ErrInvalidThresholdSig RGB20 充值 TSS 阈值签名无效。
	ErrInvalidThresholdSig = errors.New("invalid rgb20 threshold signature")
)

// computeDepositSignMessage 计算 RGB20 充值 TSS 签名的消息：
// C = sha256(types.Encode(DepositAsset{threshold_sig:nil}))（确定性 protobuf，B1 精化）。
// 注意：签名节点与合约必须用同一编码规则；此处对去除 threshold_sig 的 DepositAsset 做 Encode。
func computeDepositSignMessage(dep *rtypes.DepositAsset) []byte {
	depWithoutSig := proto.Clone(dep).(*rtypes.DepositAsset)
	depWithoutSig.ThresholdSig = nil
	raw := types.Encode(depWithoutSig)
	h := sha256.Sum256(raw)
	return h[:]
}

// verifyThresholdSig 用 btcec 直接验签，禁用 chain33 VerifyBytes（其内部多一层 SHA256，双重哈希陷阱）。
// pub 为 CrossChainInfo 中存储的 TSS 组公钥（压缩 secp256k1）。
func verifyThresholdSig(pub []byte, dep *rtypes.DepositAsset) error {
	if len(dep.GetThresholdSig()) == 0 {
		elog.Error("verifyThresholdSig empty sig", "symbol", dep.GetAssetSymbol())
		return ErrInvalidThresholdSig
	}
	if len(pub) == 0 {
		elog.Error("verifyThresholdSig empty pubkey", "symbol", dep.GetAssetSymbol())
		return ErrInvalidThresholdSig
	}
	pubKey, err := btcec.ParsePubKey(pub)
	if err != nil {
		elog.Error("verifyThresholdSig parse pubkey", "symbol", dep.GetAssetSymbol(), "err", err)
		return ErrInvalidThresholdSig
	}
	sig, err := ecdsa.ParseDERSignature(dep.GetThresholdSig())
	if err != nil {
		elog.Error("verifyThresholdSig parse sig", "symbol", dep.GetAssetSymbol(), "err", err)
		return ErrInvalidThresholdSig
	}
	msg := computeDepositSignMessage(dep)
	if !sig.Verify(msg, pubKey) {
		elog.Error("verifyThresholdSig verify failed", "symbol", dep.GetAssetSymbol(),
			"msg", fmt.Sprintf("%x", msg))
		return ErrInvalidThresholdSig
	}
	return nil
}
