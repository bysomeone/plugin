package types

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"strings"
)

/*
 * 全局 operationId（S2）
 *
 * 每笔充值（铸造）与提现（销毁）都对应一个 32 字节的全局操作 id。id **不是外部输入**，而是由
 * 链上已校验的数据**确定性派生**出来的：
 *
 *   - 充值：sha256(域标签 | 归一化 symbol | BTC 交易规范 txid(32B) | 充值地址 | 金额)；
 *   - 提现：sha256(域标签 | 归一化 symbol | burn 的 chain33 交易哈希)。
 *
 * 为什么是派生而不是"交易里带一个 operationId 字段"：
 *   - **不可伪造 / 不可顶替**。若 id 由提交方给出，任何人都可以申报一个与别人相同的 id（撞掉
 *     别人的台账记录）或一个从未发生过的 id；派生式 id 的每个输入都是链上校验过的
 *     （充值 txid 走 merkle + 严格解析、金额走 P2WSH 输出累加或 TSS 阈值签名），
 *     于是"同一个 id"⟺"同一笔操作"，哈希本身承担了唯一性与防伪。
 *   - **P2WSH 路径下没有可承载额外字段的承诺**（充值绑定完全由派生脚本承担，见
 *     executor/validate_proof.go 的"充值绑定"注释），加 proto 字段还得再论证它被签名覆盖；
 *     派生式不需要动交易格式。
 *   - 与既有身份口径一致：充值用**规范 txid**（E1）、提现用 **burn 的 chain33 交易哈希**（S3），
 *     都已是各自资金流的唯一身份，operationId 只是把它们**域分隔**后取 32 字节摘要，
 *     使"充值 op"与"提现 op"不会互相碰撞。
 *
 * 版本与兼容：域标签里带 "v1" —— 任何派生规则的变化都会让历史 id 全部改变，因此规则**冻结**，
 * 变更必须换标签（v2）而不是原地改（本链未上线，尚未产生线上数据，但规则一旦上线即不可改）。
 * 冻结向量见 operation_test.go。
 */

// 操作类别（对应 operationRecord.kind）。
const (
	// OperationKindMint 铸造：一笔充值把资产铸给用户。
	OperationKindMint int32 = 1
	// OperationKindBurn 销毁：一笔提现把用户的 wrapped 资产锁定、结算时销毁。
	OperationKindBurn int32 = 2
)

// OperationIDLen 全局 operationId 的字节长度（sha256 摘要）。
const OperationIDLen = 32

// ErrInvalidOperationIDInput operationId 派生的输入不合法（txid 长度、symbol、地址、金额）。
var ErrInvalidOperationIDInput = errors.New("invalid operation id input")

const (
	mintOperationDomain = "rgbx:opid:v1:mint:"
	burnOperationDomain = "rgbx:opid:v1:burn:"
)

// MintOperationID 派生"充值铸造"操作的全局 id。
//
// 输入必须是**已被链上校验过**的量（调用点见 executor 的 Exec_Deposit）：
//   - symbol：归一化后的跨链资产符号（如 "XBTC"、"XRGB20_USDT"）；
//   - btcTxID：付款那笔 BTC 交易的规范 txid（32 字节，来自严格解析，E1 口径）；
//   - depositAddress：充值归属的 chain33 地址串（原样字节，与 P2WSH 派生里的 userID 同一个量）；
//   - amount：实际到账的资产最小单位金额（P2WSH 路径=付给该用户脚本的输出累加；RGB20=TSS 阈值签名覆盖的金额）。
//
// 金额进入原像是有意为之：金额是这笔铸造的事实之一，写进 id 后"改金额"必然换 id，
// 台账记录无法被重新解释成另一个金额（见 executor/operation.go 的不变性说明）。
func MintOperationID(symbol string, btcTxID []byte, depositAddress string, amount int64) ([]byte, error) {
	if len(btcTxID) != OperationIDLen || symbol == "" || depositAddress == "" || amount <= 0 {
		return nil, ErrInvalidOperationIDInput
	}
	var amountBuf [8]byte
	binary.BigEndian.PutUint64(amountBuf[:], uint64(amount))

	h := sha256.New()
	writeOpIDField(h, []byte(mintOperationDomain))
	writeOpIDField(h, []byte(normalizeOperationSymbol(symbol)))
	writeOpIDField(h, btcTxID)
	writeOpIDField(h, []byte(depositAddress))
	writeOpIDField(h, amountBuf[:])
	return h.Sum(nil), nil
}

// BurnOperationID 派生"提现销毁"操作的全局 id。
//
// chain33TxHash 是 burn 那笔 chain33 交易（Withdraw）的哈希——与 S3 的已消费集合
// （executor 的 formatWithdrawUsedKey）取同一个身份，因此"台账里的提现 op"与"防重复放款的键"
// 一一对应，审计时两条记录可以互相印证。
func BurnOperationID(symbol string, chain33TxHash []byte) ([]byte, error) {
	if len(chain33TxHash) == 0 || symbol == "" {
		return nil, ErrInvalidOperationIDInput
	}
	h := sha256.New()
	writeOpIDField(h, []byte(burnOperationDomain))
	writeOpIDField(h, []byte(normalizeOperationSymbol(symbol)))
	writeOpIDField(h, chain33TxHash)
	return h.Sum(nil), nil
}

// normalizeOperationSymbol 归一化参与派生的 symbol。
//
// symbol 是 ASCII 白名单字符集（合约侧 checkMint/checkCommitDKG 用 isValidSymbolCharset 强制，
// 见 executor/kv.go 的 A6 说明），ToUpper 在该字符集上是单射，因此不同 symbol 不会归一化到同一结果。
// 与 executor 的 formatSymbol 同一个口径（这里独立实现是为了让 types 包不依赖执行器配置）。
func normalizeOperationSymbol(symbol string) string {
	return strings.ToUpper(symbol)
}

// writeOpIDField 以 varint 长度前缀写入一个字段。
//
// 定长前缀是必要的：若直接拼接，("ab","c") 与 ("a","bc") 会得到同一份原像，"不同输入撞同一 id"
// 就成了构造出来的事实。长度前缀让原像与输入序列一一对应。
func writeOpIDField(h hash.Hash, field []byte) {
	var lenBuf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(lenBuf[:], uint64(len(field)))
	_, _ = h.Write(lenBuf[:n])
	_, _ = h.Write(field)
}
