// Copyright Fuzamei Corp. 2018 All Rights Reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package executor

// 记录token 的更改记录，
// 包含创建完成， 铸币， 以后可能包含燃烧等

import (
	dbm "github.com/33cn/chain33/common/db"
	"github.com/33cn/chain33/common/db/table"
	"github.com/33cn/chain33/types"
	evmxgotypes "github.com/33cn/plugin/plugin/dapp/evmxgo/types"
)

var opt_logs_table = &table.Option{
	Prefix:  "LODB-evmxgo",
	Name:    "logs",
	Primary: "txIndex",

	Index: []string{
		"symbol",
	},
}

// LogsRow row
type LogsRow struct {
	*evmxgotypes.LocalEvmxgoLogs
}

// NewOrderRow create row
func NewOrderRow() *LogsRow {
	return &LogsRow{LocalEvmxgoLogs: nil}
}

// CreateRow create row
func (r *LogsRow) CreateRow() *table.Row {
	return &table.Row{Data: &evmxgotypes.LocalEvmxgoLogs{}}
}

// SetPayload set payload
func (r *LogsRow) SetPayload(data types.Message) error {
	if d, ok := data.(*evmxgotypes.LocalEvmxgoLogs); ok {
		r.LocalEvmxgoLogs = d
		return nil
	}
	return types.ErrTypeAsset
}

// Get get index key
func (r *LogsRow) Get(key string) ([]byte, error) {
	switch key {
	case "txIndex":
		return []byte(r.TxIndex), nil
	case "symbol":
		return []byte(r.Symbol), nil
	default:
		return nil, types.ErrNotFound
	}
}

// NewLogsTable create table
func NewLogsTable(kvdb dbm.KV) *table.Table {
	rowMeta := NewOrderRow()
	err := rowMeta.SetPayload(&evmxgotypes.LocalEvmxgoLogs{})
	if err != nil {
		panic(err)
	}
	t, err := table.NewTable(rowMeta, kvdb, opt_logs_table)
	if err != nil {
		panic(err)
	}
	return t
}

func list(db dbm.KVDB, indexName string, data *evmxgotypes.LocalEvmxgoLogs, count, direction int32) ([]*table.Row, error) {
	query := NewLogsTable(db).GetQuery(db)
	var primary []byte
	if len(data.TxIndex) > 0 {
		primary = []byte(data.TxIndex)
	}

	cur := &LogsRow{LocalEvmxgoLogs: data}
	index, err := cur.Get(indexName)
	if err != nil {
		elog.Error("query List failed", "key", string(primary), "param", data, "err", err)
		return nil, err
	}
	elog.Debug("query List dbg", "indexName", indexName, "index", string(index), "primary", primary, "count", count, "direction", direction)
	rows, err := query.ListIndex(indexName, index, primary, count, direction)
	if err != nil {
		elog.Error("query List failed", "key", string(primary), "param", data, "err", err)
		return nil, err
	}
	if len(rows) == 0 {
		return nil, types.ErrNotFound
	}
	return rows, nil
}