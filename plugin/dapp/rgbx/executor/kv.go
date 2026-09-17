package executor

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/33cn/chain33/common/address"
	"github.com/33cn/chain33/common/db"
	"github.com/33cn/chain33/system/dapp"
	"github.com/33cn/chain33/types"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
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
	// confirmUsedKeyPrefix Confirm 结算（mint / transfer 两类 utxo 花费确认）已消费集合前缀（E14）。
	confirmUsedKeyPrefix = KeyPrefixStateDB + "confirm-used-"
	// operationRecordKeyPrefix 全局操作台账的单条**不可变**记录前缀（S2）：
	// key = 前缀 + 32 字节 operationId，value = types.Encode(operationRecord)。
	operationRecordKeyPrefix = KeyPrefixStateDB + "oprec-"
	// operationSupplyKeyPrefix 每 symbol 的铸造/销毁累计计数前缀（S2，单调累加，可回滚不可改写历史）。
	operationSupplyKeyPrefix = KeyPrefixStateDB + "opsupply-"
	// operationIndexKeyPrefix 每 symbol 每类操作的下标前缀（S2）：
	// key = 前缀 + symbol + "-" + kindTag + "-" + 20 位十进制下标，value = operationId。
	// 为什么需要它：执行期 stateDB 只有 Get/Set（没有 List），枚举某个 symbol 的操作必须靠
	// "按已知下标逐个 Get"，所以下标要显式落盘（数量记在供应量记录里）。
	operationIndexKeyPrefix = KeyPrefixStateDB + "opidx-"
)

// 操作台账下标里的类别标记（'m' = 铸造 / 'b' = 销毁）。用字符而不是二进制是为了
// 运维直接用键前缀肉眼分辨（键本身不参与哈希，无编码歧义问题）。
const (
	operationIndexTagMint = 'm'
	operationIndexTagBurn = 'b'
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

// formatConfirmUsedKey 按**被确认交易**（mint / transfer 的那笔 chain33 交易）哈希索引已结算的
// Confirm（E14）。与提现侧 formatWithdrawUsedKey、充值侧 formatDepositUsedTxIDKey 对称：
// 三条资金流各自的"已消费"集合都落在共识状态（stateDB），而不是节点私有的 LocalDB。
//
// 为什么键取 confirm.GetTxHash()（被确认交易）而不是 SpendingTx 的 btc txid：
//   - 去重单位就是"待确认的那笔 chain33 交易"：它既是 localdb pendingTx 记录的主体、也是
//     checkConfirm 用 (TxBlockHeight, TxIndex) 定位并与 TxHash 逐字比对的对象，加载 payload
//     用的还是同一个 key（formatPayloadKey(confirm.GetTxHash())）。stateDB 键取同一身份 ⇒
//     与它替换掉的那条非共识守卫（ExecLocal_Confirm 写的 localdb pendingTx.Confirmed）粒度完全一致，
//     只是从"各节点私有"换成"全网共识"。
//   - 反过来，**一笔 BTC 花费可以合法地同时结算多笔 chain33 交易**：一个 BTC 交易允许多个输入
//     各花费一个 rgbx utxo，并带多个 OP_RETURN 承诺（每个指向不同的 chain33 交易），checkConfirm
//     是按 (TxBlockHeight, TxIndex) + 输入下标逐笔校验的，Exec_Confirm 的 mint 与 transfer 分支
//     也共用同一份 UtxoProof/SpendingTx（见 exec.go）。若按 btc txid 建键，这类合法的批量结算
//     （一笔 BTC 交易里同时确认一笔 mint 与一笔 transfer，或多笔 transfer）会被第二笔起的守卫误拒，
//     且守卫粒度比它要替代的那条更粗 —— 故不取 btc txid（A2 用 btc txid 是另一回事：那是归属
//     utxo id 的口径，不是去重键的口径）。
//   - 口径安全性：confirm.GetTxHash() 是链上直接给出的 chain33 交易哈希字节，不经过"从可解析字节
//     重新推导身份"这一步，故不存在 E1/A2 那类"同一身份的另一份编码"问题（chain33 交易哈希没有
//     第二种编码）。定长化与 formatWithdrawUsedKey 一致：先 sha256 再拼接。
func formatConfirmUsedKey(confirmTxHash []byte) []byte {
	hash := sha256.Sum256(confirmTxHash)
	return append([]byte(confirmUsedKeyPrefix), hash[:]...)
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

// formatOperationRecordKey 按 32 字节全局 operationId 索引单条台账记录（S2）。
// 记录不可变：键由 operationId 派生、值是操作本身的事实，**没有任何更新路径**
// （唯一的写入口 newOperationRecordKV 会拒绝已存在的键，见 executor/operation.go）。
func formatOperationRecordKey(operationID []byte) []byte {
	return append([]byte(operationRecordKeyPrefix), operationID...)
}

// formatOperationSupplyKey 按 symbol 索引铸造/销毁累计计数（S2）。
//
// 调用方必须传**归一化后的跨链账户符号**（executor 里的 ensureCrossChainSymbol 结果，如 "XBTC"）：
// 写入侧用账户符号（台账与账户口径一致），查询侧也必须先做同一归一化，否则会读到空计数。
// 本函数只做机械大小写归一（不做前缀补全），避免键的形态依赖节点配置。
func formatOperationSupplyKey(symbol string) []byte {
	return []byte(operationSupplyKeyPrefix + formatSymbol(symbol))
}

// formatOperationIndexKey 按 (symbol, kind, index) 索引 operationId（S2），
// 供"枚举某 symbol 的某类操作"用（执行期 stateDB 无 List，只能按下标逐个 Get）。
// 下标补零到定宽十进制：即使将来换成前缀扫描，字典序也等于数值序。
func formatOperationIndexKey(symbol string, kind int32, index int64) []byte {
	return []byte(fmt.Sprintf("%s%s-%c-%020d", operationIndexKeyPrefix, formatSymbol(symbol),
		operationIndexTag(kind), index))
}

// operationIndexTag 操作类别 → 下标键里的标记字符。
func operationIndexTag(kind int32) byte {
	if kind == rtypes.OperationKindBurn {
		return operationIndexTagBurn
	}
	return operationIndexTagMint
}

// operationNetwork 由 symbol 确定性导出"网络/资产域"标识（不读节点本地配置：
// 网络名在 E5 里正是"本地配置影响共识判定"的反例，台账记录必须是各节点一致的结果）。
// 口径 = 去掉跨链前缀后的基础资产符号（"XBTC"→"BTC"、"XRGB20_USDT"→"RGB20_USDT"）。
func operationNetwork(symbol string) string {
	normalized := formatSymbol(symbol)
	network := strings.TrimPrefix(normalized, rgbxCfg.CrossChainAssetPrefix)
	if network == "" {
		return normalized
	}
	return network
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
