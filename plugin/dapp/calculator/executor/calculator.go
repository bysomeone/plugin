package executor

import (
	"fmt"

	log "github.com/33cn/chain33/common/log/log15"
	drivers "github.com/33cn/chain33/system/dapp"
	"github.com/33cn/chain33/types"
	ptypes "github.com/33cn/plugin/plugin/dapp/calculator/types/calculator"
)

/*
 * 执行器相关定义
 * 重载基类相关接口
 */

var (
	//日志
	elog = log.New("module", "calculator.executor")
)

var driverName = ptypes.CalculatorX

func init() {
	ety := types.LoadExecutorType(driverName)
	ety.InitFuncList(types.ListMethod(&calculator{}))
}

// Init register dapp
func Init(name string, sub []byte) {
	drivers.Register(GetName(), newCalculator, types.GetDappFork(driverName, "Enable"))
}

type calculator struct {
	drivers.DriverBase
}

func newCalculator() drivers.Driver {
	t := &calculator{}
	t.SetChild(t)
	t.SetExecutorType(types.LoadExecutorType(driverName))
	return t
}

// GetName get driver name
func GetName() string {
	return newCalculator().GetName()
}

func (*calculator) GetDriverName() string {
	return driverName
}

// CheckTx 实现自定义检验交易接口，供框架调用
func (*calculator) CheckTx(tx *types.Transaction, index int) error {

	action := &ptypes.CalculatorAction{}
	err := types.Decode(tx.GetPayload(), action)
	if err != nil {
		elog.Error("CheckTx", "DecodeActionErr", err)
		return types.ErrDecode
	}
	//这里只做除法除数零值检查
	if action.Ty == ptypes.TyDivAction {
		div, ok := action.Value.(*ptypes.CalculatorAction_Div)
		if !ok {
			return types.ErrTypeAsset
		}
		if div.Div.Divisor == 0 { //除数不能为零
			elog.Error("CheckTx", "Err", "ZeroDivisor")
			return types.ErrInvalidParam
		}
	}
	return nil
}

func (c *calculator) Query_CalcCount(in *ptypes.ReqQueryCalcCount) (types.Message, error) {

	var countInfo ptypes.ReplyQueryCalcCount
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
