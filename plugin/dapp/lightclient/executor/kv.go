package executor

import "fmt"

/*
 * 用户合约存取kv数据时，key值前缀需要满足一定规范
 * 即key = keyPrefix + userKey
 * 需要字段前缀查询时，使用’-‘作为分割符号
 */

var (
	//KeyPrefixStateDB state db key必须前缀
	KeyPrefixStateDB = "mavl-lightclient-"
	//KeyPrefixLocalDB local db的key必须前缀
	KeyPrefixLocalDB = "LODB-lightclient-"
)

// statedb

func btcLastHeaderKey() []byte {
	return []byte(KeyPrefixStateDB + "btc-lastheader")
}

// btcChainStateKey B3/B4：canonical 头链最近若干节点的索引窗口（含每个节点的累积工作量）
func btcChainStateKey() []byte {
	return []byte(KeyPrefixStateDB + "btc-chainstate")
}

// localdb

func btcHeaderKey(height uint64) []byte {
	return []byte(KeyPrefixLocalDB + fmt.Sprintf("btc-header-%010d", height))
}

func btcHeaderHashHeightKey(hash string) []byte {
	return []byte(KeyPrefixLocalDB + "btc-hash2height-" + hash)
}
