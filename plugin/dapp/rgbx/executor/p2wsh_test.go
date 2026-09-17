package executor

import (
	"encoding/hex"
	"testing"

	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/stretchr/testify/require"
)

/*
 * 执行器侧复用的**冻结派生向量**（与 plugin/dapp/rgbx/types/testdata/p2wsh_deposit_vectors.json
 * 的 v1/v2 是同一组输入输出）。
 *
 * 为什么在执行器测试里再硬编码一份：链条上的判定必须与派生规格**逐字节**一致，
 * 而跨包读不到 types 的内嵌向量。这里把向量值当字面量钉住，任何一方漂移都会在自己那边变红；
 * newP2WSHDepositFixture 还会用实现重算一遍并与字面量比对，双重锚定。
 */
type p2wshFrozenVector struct {
	name        string
	userID      string // chain33 充值地址串（P2WSH 派生里的 userID，原样字节）
	tssPubHex   string // 33 字节压缩 TSS 群公钥
	pkScriptHex string // 该用户的 P2WSH program：OP_0 || push32(sha256(witnessScript))
}

var (
	// v1：userID 来自 seed "rgbx-p2wsh-vector-user-1"，tssPub = privkey 1 的压缩公钥（生成元）
	frozenVectorA = p2wshFrozenVector{
		name:        "v1-chain33-address",
		userID:      "1AmRYcURfDGxBhiaJAvEGdRkvkoM7ztn1u",
		tssPubHex:   "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798",
		pkScriptHex: "002078d9392c30b67443d288c548adde74e0eff45454048cf3e7cc2f82f43da70465",
	}
	// v2：第二个用户 + 另一把 TSS 群公钥
	frozenVectorB = p2wshFrozenVector{
		name:        "v2-second-chain33-address",
		userID:      "1MCurL8dSuJUWQKabUCGYXuM9Ypm5ftN9s",
		tssPubHex:   "03d1ee32d73a68fe4cef8b5736a0ec96bd4cd3395b3dfee7d6423acc857b286ec2",
		pkScriptHex: "00209913fa295a756b65bc623851526625df70a6dcdda2380054f5b9f51515f654a5",
	}
)

// p2wshDepositFixture 一个 (userID, tssPub) → P2WSH program 的派生结果。
type p2wshDepositFixture struct {
	depositAddr string
	tssPub      []byte
	pkScript    []byte
}

func newP2WSHDepositFixture(t *testing.T, v p2wshFrozenVector) *p2wshDepositFixture {
	t.Helper()
	tssPub, err := hex.DecodeString(v.tssPubHex)
	require.NoError(t, err)
	pkScript, err := rtypes.DeriveDepositPkScript(v.userID, tssPub)
	require.NoError(t, err)
	// 双重锚定：实现重算的结果必须与冻结字面量逐字节相同
	require.Equal(t, v.pkScriptHex, hex.EncodeToString(pkScript),
		"派生实现与冻结向量 %s 不一致（types/testdata/p2wsh_deposit_vectors.json）", v.name)
	return &p2wshDepositFixture{depositAddr: v.userID, tssPub: tssPub, pkScript: pkScript}
}

// crossChainInfo 该 symbol 的链上跨链信息：主池脚本（P2WPKH）+ TSS 群公钥。
func (f *p2wshDepositFixture) crossChainInfo(mainPoolScript []byte) *rtypes.CrossChainInfo {
	return &rtypes.CrossChainInfo{
		AssetSymbol: rtypes.BTCSymbol,
		PkScript:    mainPoolScript,
		Pubkey:      f.tssPub,
	}
}

// deposit 构造一份"付给 f 的 P2WSH program、金额 amount"的充值请求。
func (f *p2wshDepositFixture) deposit(amount int64) *rtypes.DepositAsset {
	return &rtypes.DepositAsset{
		Amount:         amount,
		DepositAddress: f.depositAddr,
		AssetSymbol:    "btc",
	}
}
