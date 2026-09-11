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
	depositUsedKeyPrefix      = KeyPrefixStateDB + "deposited-"
	// withdrawUsedKeyPrefix 提现侧已消费（已结算）burn 集合前缀，与 depositUsedKeyPrefix 对称。
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

func formatDepositUsedKey(txData []byte) []byte {
	hash := sha256.Sum256(txData)
	return append([]byte(depositUsedKeyPrefix), hash[:]...)
}

// formatWithdrawUsedKey 按提现销毁（chain33 Withdraw 交易）哈希索引已结算的提现（S3）。
// 与 formatDepositUsedKey 完全对称：充值防双铸，提现防同一 burn 重复放款。
// 唯一标识取 burn 的 chain33 交易哈希（= payload 键所用的同一个 id），
// 它在链上稳定、不随重组变化，且是 ConfirmTx 绑定的对象。
func formatWithdrawUsedKey(burnTxHash []byte) []byte {
	hash := sha256.Sum256(burnTxHash)
	return append([]byte(withdrawUsedKeyPrefix), hash[:]...)
}

func formatSymbol(symbol string) string {
	return strings.ToUpper(symbol)
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
