package executor

import (
	"errors"
	"fmt"

	"github.com/33cn/chain33/system/dapp"
	"github.com/33cn/chain33/types"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
)

/*
 * Height-indexed stateDB lists.
 *
 * Execution-time stateDB has no List, so append-only request records (abandon proposals, fee
 * bumps) are stored as one key per (height, per-height sequence) plus a per-height counter.
 * Both lists share this layout and the same paging rules; only the key prefixes and the item
 * type differ.
 */

// maxHeightScan bounds how many heights one listing call walks. Callers page by advancing
// StartHeight.
const maxHeightScan = int64(maxListCount)

// heightIndexedItem is the shape every stored list item shares: it knows its own height and
// block transaction index, which is what the paging code filters and orders by. Generated
// protobuf item pointers satisfy it through their GetHeight / GetIndex getters.
type heightIndexedItem interface {
	types.Message
	GetHeight() int64
	GetIndex() int32
}

// formatHeightIndexKey indexes one item by (height, per-height sequence). The sequence keeps
// keys contiguous inside a block, which is what the query loop walks.
func formatHeightIndexKey(prefix string, height, sequence int64) []byte {
	return []byte(prefix + dapp.HeightIndexStr(height, sequence))
}

// formatHeightCountKey stores how many items exist at a height.
func formatHeightCountKey(prefix string, height int64) []byte {
	return []byte(fmt.Sprintf("%s%020d", prefix, height))
}

// heightIndexCount reads the number of items at a height (missing = 0).
func (r *rgbx) heightIndexCount(prefix string, height int64) (int64, error) {
	if r.GetStateDB() == nil {
		return 0, nil
	}
	value, err := r.GetStateDB().Get(formatHeightCountKey(prefix, height))
	if errors.Is(err, types.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	count := &types.Int64{}
	if err = types.Decode(value, count); err != nil {
		elog.Error("heightIndexCount decode", "prefix", prefix, "height", height, "err", err)
		return 0, err
	}
	return count.GetData(), nil
}

// heightIndexKVs returns the KVs that append one item to a height-indexed list: the item at
// (height, sequence) and the incremented per-height counter.
func (r *rgbx) heightIndexKVs(requestPrefix, countPrefix string, height, sequence int64,
	item types.Message) []*types.KeyValue {

	return []*types.KeyValue{
		{Key: formatHeightIndexKey(requestPrefix, height, sequence), Value: types.Encode(item)},
		{Key: formatHeightCountKey(countPrefix, height), Value: types.Encode(&types.Int64{Data: sequence + 1})},
	}
}

// listHeightIndexed walks a height range and returns at most req.Count items. StartIndex skips
// items whose block transaction index is lower at StartHeight. EndHeight = 0 means "up to the
// current height"; at most maxHeightScan heights are walked per call.
func listHeightIndexed[T heightIndexedItem](r *rgbx, req *rtypes.ReqListPendingTx,
	requestPrefix, countPrefix string, newItem func() T) ([]T, error) {

	if req.GetCount() <= 0 || req.GetCount() > maxListCount {
		return nil, types.ErrInvalidParam
	}
	start := req.GetStartHeight()
	if start < 0 {
		return nil, types.ErrInvalidParam
	}
	end := req.GetEndHeight()
	if end > 0 && end < start {
		return nil, types.ErrInvalidParam
	}
	if end <= 0 {
		end = r.GetHeight()
		if end < start {
			end = start + maxHeightScan - 1
		}
	}
	if end-start+1 > maxHeightScan {
		end = start + maxHeightScan - 1
	}
	var items []T
	for height := start; height <= end; height++ {
		count, err := r.heightIndexCount(countPrefix, height)
		if err != nil {
			return nil, err
		}
		for sequence := int64(0); sequence < count; sequence++ {
			item := newItem()
			if err = readDB(r.GetStateDB(), formatHeightIndexKey(requestPrefix, height, sequence), item); err != nil {
				elog.Error("listHeightIndexed read item", "prefix", requestPrefix, "height", height,
					"sequence", sequence, "err", err)
				return nil, err
			}
			if height == start && int64(item.GetIndex()) < req.GetStartIndex() {
				continue
			}
			items = append(items, item)
			if int32(len(items)) >= req.GetCount() {
				return items, nil
			}
		}
	}
	return items, nil
}
