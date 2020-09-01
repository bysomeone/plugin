package types

import (
	log "github.com/33cn/chain33/common/log/log15"
	"github.com/33cn/chain33/types"
	"reflect"
)

/*
 * 交易相关类型定义
 * 交易action通常有对应的log结构，用于交易回执日志记录
 * 每一种action和log需要用id数值和name名称加以区分
 */

// action类型id和name，这些常量可以自定义修改
const (
	TyUnknowAction = iota + 100
	TyAddAction
	TySubAction
	TyMulAction
	TyDivAction

	NameAddAction = "Add"
	NameSubAction = "Sub"
	NameMulAction = "Mul"
	NameDivAction = "Div"
)

// log类型id值
const (
	TyUnknownLog = iota + 100
	TyAddLog
	TySubLog
	TyMulLog
	TyDivLog
)

var (
	//CalculatorX 执行器名称定义
	CalculatorX = "calculator"
	//定义actionMap
	actionMap = map[string]int32{
		NameAddAction: TyAddAction,
		NameSubAction: TySubAction,
		NameMulAction: TyMulAction,
		NameDivAction: TyDivAction,
	}
	//定义log的id和具体log类型及名称，填入具体自定义log类型
	logMap = map[int64]*types.LogInfo{
		TyAddLog: {Ty:reflect.TypeOf(AddLog{}), Name: "AddLog"},
		TySubLog: {Ty:reflect.TypeOf(SubLog{}), Name: "SubLog"},
		TyMulLog: {Ty:reflect.TypeOf(MultiplyLog{}), Name: "MultiplyLog"},
		TyDivLog: {Ty:reflect.TypeOf(DivideLog{}), Name: "DivideLog"},
	}
	tlog = log.New("module", "calculator.types")
)

// init defines a register function
func init() {
	types.AllowUserExec = append(types.AllowUserExec, []byte(CalculatorX))
	//注册合约启用高度
	types.RegFork(CalculatorX, InitFork)
	types.RegExec(CalculatorX, InitExecutor)
}

// InitFork defines register fork
func InitFork(cfg *types.Chain33Config) {
	cfg.RegisterDappFork(CalculatorX, "Enable", 0)
}

// InitExecutor defines register executor
func InitExecutor(cfg *types.Chain33Config) {
	types.RegistorExecutor(CalculatorX, NewType(cfg))
}

type calculatorType struct {
	types.ExecTypeBase
}

func NewType(cfg *types.Chain33Config) *calculatorType {
	c := &calculatorType{}
	c.SetChild(c)
	c.SetConfig(cfg)
	return c
}

// GetPayload 获取合约action结构
func (c *calculatorType) GetPayload() types.Message {
	return &CalculatorAction{}
}

// GeTypeMap 获取合约action的id和name信息
func (c *calculatorType) GetTypeMap() map[string]int32 {
	return actionMap
}

// GetLogMap 获取合约log相关信息
func (c *calculatorType) GetLogMap() map[int64]*types.LogInfo {
	return logMap
}
