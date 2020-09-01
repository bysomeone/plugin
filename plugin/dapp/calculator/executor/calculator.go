package executor

import (
	log "github.com/33cn/chain33/common/log/log15"
	drivers "github.com/33cn/chain33/system/dapp"
	"github.com/33cn/chain33/types"
	calculatortypes "github.com/33cn/plugin/plugin/dapp/calculator/types"
)

/*
 * 执行器相关定义
 * 重载基类相关接口
 */

var (
	//日志
	elog = log.New("module", "calculator.executor")
)

var driverName = calculatortypes.CalculatorX

// Init register dapp
func Init(name string, cfg *types.Chain33Config, sub []byte) {
	drivers.Register(cfg, GetName(), newCalculator, cfg.GetDappFork(driverName, "Enable"))
	InitExecType()
}

// InitExecType Init Exec Type
func InitExecType() {
	ety := types.LoadExecutorType(driverName)
	ety.InitFuncList(types.ListMethod(&calculator{}))
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

func (c *calculator) GetDriverName() string {
	return driverName
}

// CheckTx 实现自定义检验交易接口，供框架调用
func (c *calculator) CheckTx(tx *types.Transaction, index int) error {
	// implement code
	action := &calculatortypes.CalculatorAction{}
	err := types.Decode(tx.GetPayload(), action)
	if err != nil {
		elog.Error("CheckTx", "DecodeActionErr", err)
		return types.ErrDecode
	}
	//这里只做除法除数零值检查
	if action.Ty == calculatortypes.TyDivAction {
		div, ok := action.Value.(*calculatortypes.CalculatorAction_Div)
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
