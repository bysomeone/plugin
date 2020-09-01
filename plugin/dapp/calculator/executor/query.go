package executor

import (
	"fmt"

	"github.com/33cn/chain33/types"
	calculatortypes "github.com/33cn/plugin/plugin/dapp/calculator/types"
)

func (c *calculator) Query_CalcCount(in *calculatortypes.ReqQueryCalcCount) (types.Message, error) {

	var countInfo calculatortypes.ReplyQueryCalcCount
	localKey := []byte(fmt.Sprintf("%s-CalcCount-%s", KeyPrefixLocalDB, in.Action))
	oldVal, err := c.GetLocalDB().Get(localKey)
	if err != nil && err != types.ErrNotFound {
		return nil, err
	}
	err = types.Decode(oldVal, &countInfo)
	if err != nil {
		elog.Error("execLocalAdd", "DecodeErr", err)
		return nil, err
	}
	return &countInfo, nil
}
