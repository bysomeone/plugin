package executor

import (
	"crypto/sha256"
	"strings"

	"github.com/33cn/chain33/common/address"
	"github.com/33cn/chain33/common/db"
	"github.com/33cn/chain33/system/dapp"
	"github.com/33cn/chain33/types"
)

/*
 * 用户合约存取kv数据时，key值前缀需要满足一定规范
 * 即key = keyPrefix + userKey
 * 需要字段前缀查询时，使用’-‘作为分割符号
 */

const (
	//KeyPrefixStateDB state db key必须前缀
	KeyPrefixStateDB = "mavl-rgbx-"
	//KeyPrefixLocalDB local db的key必须前缀
	KeyPrefixLocalDB = "LODB-rgbx-"

	dkgConfirmationsKeyPrefix = KeyPrefixStateDB + "dkg-confirmations-"
	crossChainInfoKeyPrefix   = KeyPrefixStateDB + "crosschain-info-"
	// depositUsedTxIDKeyPrefix 充值已消费集合前缀（E1 修复）：key = 前缀 + btc txid。
	depositUsedTxIDKeyPrefix = KeyPrefixStateDB + "deposited-txid-"
	// withdrawUsedKeyPrefix 提现侧已消费（已结算）burn 集合前缀。
	withdrawUsedKeyPrefix = KeyPrefixStateDB + "withdrawn-"
)

// formatDkgConfirmationsKey 按 (symbol, dkgAddress) 索引确认集合（BL-2）。
// 旧实现仅按地址索引：BTC 与 RGB20 复用同一 TSS 地址时互相污染，导致 RGB20 CrossChainInfo 建不出或取错 pubkey。
func formatDkgConfirmationsKey(symbol, dkgAddress string) []byte {
	return []byte(dkgConfirmationsKeyPrefix + formatSymbol(symbol) + "-" + dkgAddress)
}

func formatCrossChainInfoKey(symbol string) []byte {
	return []byte(crossChainInfoKeyPrefix + formatSymbol(symbol))
}

// formatDepositUsedTxIDKey 以解析后 btc 交易的 txid 作为充值唯一标识（E1 修复）。
// 旧口径是对 TxData 原始字节取哈希，而那不是交易的规范身份：同一笔 BTC 交易的任意一份
// "字节不同但解析结果相同"的编码（尾部追加字节等，见 parseBtcTxIDStrict）都会得到不同的 key，
// 使重复检查失效。txid 由交易的规范序列化（无 witness）算出，对同一笔交易恒定，
// 因此"同一笔真实充值的另一份编码"必然命中同一 key。
func formatDepositUsedTxIDKey(txID []byte) []byte {
	return append([]byte(depositUsedTxIDKeyPrefix), txID...)
}

// formatWithdrawUsedKey 按提现销毁（chain33 Withdraw 交易）哈希索引已结算的提现（S3）。
// 与充值侧对称：充值防双铸，提现防同一 burn 重复放款。
// 唯一标识取 burn 的 chain33 交易哈希（= payload 键所用的同一个 id），
// 它在链上稳定、不随重组变化，且是 ConfirmTx 绑定的对象。
func formatWithdrawUsedKey(burnTxHash []byte) []byte {
	hash := sha256.Sum256(burnTxHash)
	return append([]byte(withdrawUsedKeyPrefix), hash[:]...)
}

// formatSymbol 归一化 symbol：symbol 是资产的唯一身份（asset key / account key / CrossChainInfo key），
// 大小写不敏感（"btc" 与 "BTC" 必须落到同一个 key），故统一取大写。
//
// 注意 strings.ToUpper 并非单射：非 ASCII 字符会与 ASCII 字母归一化到同一结果
// （如 "xſ"（U+017F）与 "xs" 都得到 "XS"），使两个不同的 symbol 撞同一个 key —— 可被用来
// 抢注/别名化另一个 symbol（A6）。因此所有进入共识状态的 symbol 必须先通过
// isValidSymbolCharset 校验，ToUpper 在该字符集上是单射。
func formatSymbol(symbol string) string {
	return strings.ToUpper(symbol)
}

// isValidSymbolCharset 校验 symbol 只含 ASCII 字母、数字与下划线（A6）。
// 该字符集是 ToUpper 单射的充分条件（每个字符只映射到自身或对应的大写 ASCII 字母，
// 且不同字符不会映射到同一结果），可排除 "ſ"→"S"、"ı"→"I"、全角/带音调字符等别名化输入。
// 下划线是既有合法 symbol 的一部分（rtypes.RGB20USDTSymbol = "RGB20_USDT"），故保留。
func isValidSymbolCharset(symbol string) bool {
	if symbol == "" {
		return false
	}
	for i := 0; i < len(symbol); i++ {
		c := symbol[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' {
			continue
		}
		return false
	}
	return true
}

func isCrossChainSymbol(symbol string) bool {
	return strings.HasPrefix(formatSymbol(symbol), rgbxCfg.CrossChainAssetPrefix)
}

func ensureCrossChainSymbol(symbol string) string {
	if isCrossChainSymbol(symbol) {
		return symbol
	}
	return formatCrossChainSymbol(symbol)
}

func formatCrossChainSymbol(symbol string) string {
	return rgbxCfg.CrossChainAssetPrefix + formatSymbol(symbol)
}

func formatPayloadKey(hash []byte) []byte {
	return append([]byte(KeyPrefixStateDB+"payload-"), hash...)
}

func formatAssetKey(symbol string) []byte {
	return append([]byte(KeyPrefixStateDB+"asset-"), formatSymbol(symbol)...)
}

const pendingTxKeyPrefix = KeyPrefixLocalDB + "pendtx-"
const pendingTxByFromPrefix = KeyPrefixLocalDB + "pendbyfrom-"

func formatPendingTxKey(height, txIndex int64) []byte {

	return []byte(pendingTxKeyPrefix + dapp.HeightIndexStr(height, txIndex))
}

func formatPendingTxFromKey(fromAddr string) []byte {
	return append([]byte(pendingTxByFromPrefix), address.FormatAddrKey(fromAddr)...)
}

const confirmedHeightKey = KeyPrefixLocalDB + "confirmed-height"

func readDB(kdb db.KV, key []byte, result types.Message) error {

	val, err := kdb.Get(key)
	if err != nil {
		return err
	}
	return types.Decode(val, result)
}
