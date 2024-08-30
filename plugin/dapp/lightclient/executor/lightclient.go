package executor

import (
	log "github.com/33cn/chain33/common/log/log15"
	drivers "github.com/33cn/chain33/system/dapp"
	"github.com/33cn/chain33/types"
	lightclienttypes "github.com/33cn/plugin/plugin/dapp/lightclient/types"
)

/*
 * 执行器相关定义
 * 重载基类相关接口
 */

var (
	//日志
	elog = log.New("module", "lightclient.executor")
)

var driverName = lightclienttypes.LightclientX

// Init register dapp
func Init(name string, cfg *types.Chain33Config, sub []byte) {
	drivers.Register(cfg, GetName(), newLightclient, cfg.GetDappFork(driverName, "Enable"))
	InitExecType()
}

// InitExecType Init Exec Type
func InitExecType() {
	ety := types.LoadExecutorType(driverName)
	ety.InitFuncList(types.ListMethod(&lightclient{}))
}

type lightclient struct {
	drivers.DriverBase
}

func newLightclient() drivers.Driver {
	t := &lightclient{}
	t.SetChild(t)
	t.SetExecutorType(types.LoadExecutorType(driverName))
	return t
}

// GetName get driver name
func GetName() string {
	return newLightclient().GetName()
}

func (l *lightclient) GetDriverName() string {
	return driverName
}

// CheckTx 实现自定义检验交易接口，供框架调用
func (l *lightclient) CheckTx(tx *types.Transaction, index int) error {
	// implement code
	return nil
}
