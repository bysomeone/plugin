package calculator

import (
	"encoding/json"
	"math/rand"
	"reflect"
	"time"

	"github.com/33cn/chain33/common/address"
	log "github.com/33cn/chain33/common/log/log15"
	"github.com/33cn/chain33/types"
)

/*
 * 交易相关类型定义
 * 交易action通常有对应的log结构，用于交易回执日志记录
 * 每一种action和log需要用id数值和name名称加以区分
 */

// action类型id和name，可以自定义修改
const (
	TyAddAction = iota + 100
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
	//定义action的name和id
	actionMap = map[string]int32{
		NameAddAction: TyAddAction,
		NameSubAction: TySubAction,
		NameMulAction: TyMulAction,
		NameDivAction: TyDivAction,
	}
	//定义log的id和具体log类型及名称，填入具体自定义log类型
	logMap = map[int64]*types.LogInfo{
		TyAddLog: {Ty: reflect.TypeOf(AddLog{}), Name: "AddLog"},
		TySubLog: {Ty: reflect.TypeOf(SubLog{}), Name: "SubLog"},
		TyMulLog: {Ty: reflect.TypeOf(MultiplyLog{}), Name: "MultiplyLog"},
		TyDivLog: {Ty: reflect.TypeOf(DivideLog{}), Name: "DivideLog"},
	}
	tlog = log.New("module", "calculator.types")
)

func init() {
	types.AllowUserExec = append(types.AllowUserExec, []byte(CalculatorX))
	types.RegistorExecutor(CalculatorX, newType())
	types.RegisterDappFork(CalculatorX, "Enable", 0)
}

type calculatorType struct {
	types.ExecTypeBase
}

func newType() *calculatorType {
	c := &calculatorType{}
	c.SetChild(c)
	return c
}

// GetPayload 获取合约action结构
func (t *calculatorType) GetPayload() types.Message {
	return &CalculatorAction{}
}

// GeTypeMap 获取合约action的id和name信息
func (t *calculatorType) GetTypeMap() map[string]int32 {
	return actionMap
}

// GetLogMap 获取合约log相关信息
func (t *calculatorType) GetLogMap() map[int64]*types.LogInfo {
	return logMap
}

// CreateTx 重载基类接口，实现本合约交易创建，供框架调用
func (t *calculatorType) CreateTx(action string, message json.RawMessage) (*types.Transaction, error) {
	var tx *types.Transaction

	if action == NameAddAction {
		param := &Add{}
		err := json.Unmarshal(message, param)
		if err != nil {
			tlog.Error("CreateTx", "UnmarshalErr", err)
			return nil, types.ErrUnmarshal
		}
		tx = &types.Transaction{
			Execer:  []byte(types.ExecName(CalculatorX)),
			Payload: types.Encode(&CalculatorAction{Ty: TyAddAction, Value: &CalculatorAction_Add{Add: param}}),
			Nonce:   rand.New(rand.NewSource(time.Now().UnixNano())).Int63(),
			To:      address.ExecAddress(types.ExecName(CalculatorX)),
		}
		return tx, nil
	} else if action == NameSubAction {
	} else if action == NameMulAction {
	} else if action == NameDivAction {

		param := &Divide{}
		err := json.Unmarshal(message, param)
		if err != nil {
			tlog.Error("CreateTx", "UnmarshalErr", err)
			return nil, err
		}
		tx = &types.Transaction{
			Execer:  []byte(types.ExecName(CalculatorX)),
			Payload: types.Encode(&CalculatorAction{Ty: TyDivAction, Value: &CalculatorAction_Div{Div: param}}),
			Nonce:   rand.New(rand.NewSource(time.Now().UnixNano())).Int63(),
			To:      address.ExecAddress(types.ExecName(CalculatorX)),
		}
		return tx, nil
	}

	return tx, types.ErrNotSupport
}
