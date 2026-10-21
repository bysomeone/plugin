package types

import (
	"crypto/sha256"

	"github.com/33cn/chain33/types"
	"google.golang.org/protobuf/proto"
)

// UnlockSignDomain 是「放弃提现」签名的用途标签。
//
// 为什么需要它：同一把 TSS 组公钥同时签「充值」和「放弃提现」两种消息。即使两种消息体
// 形状不同，未来任何一次字段调整都可能让两段编码相撞，而签名一旦相撞就能被从一种用途
// 挪到另一种（例如拿充值签名去解锁）。把用途标签作为前缀明确写进被签字节，两类签名在
// 任何情况下都不可互换。
const UnlockSignDomain = "rgbx/unlock/v1"

// UnlockSignDigest 计算「放弃提现」的 TSS 组签名摘要：
//
//	C = sha256(UnlockSignDomain || 0x00 || types.Encode(UnlockAsset{unlockSig: nil}))
//
// 链上验签与桥侧签名**必须调用同一个函数**：签名端与验签端各写一份是上一类问题的来源
// （充值侧的 computeDepositSignMessage / computeRgb20DepositMsg 就是两份，靠测试对齐）。
//
// 被签内容覆盖 burnTxHash、assetSymbol、amount、owner 四个字段，因此签名绑定到具体某
// 一笔提现、具体金额、具体收款人，不能挪用到别的 pending 上。unlockSig 自身不参与摘要
// （否则无法验证）。
func UnlockSignDigest(msg *UnlockAsset) []byte {
	if msg == nil {
		msg = &UnlockAsset{}
	}
	payload := proto.Clone(msg).(*UnlockAsset)
	payload.UnlockSig = nil

	h := sha256.New()
	h.Write([]byte(UnlockSignDomain))
	h.Write([]byte{0})
	h.Write(types.Encode(payload))
	return h.Sum(nil)
}
