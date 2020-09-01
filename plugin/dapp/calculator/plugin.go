package types

import (
	"github.com/33cn/chain33/pluginmgr"
	"github.com/33cn/plugin/plugin/dapp/calculator/commands"
	"github.com/33cn/plugin/plugin/dapp/calculator/executor"
	"github.com/33cn/plugin/plugin/dapp/calculator/rpc"
	calculatortypes "github.com/33cn/plugin/plugin/dapp/calculator/types"
)

/*
 * 初始化dapp相关的组件
 */

func init() {
	pluginmgr.Register(&pluginmgr.PluginBase{
		Name:     calculatortypes.CalculatorX,
		ExecName: executor.GetName(),
		Exec:     executor.Init,
		Cmd:      commands.Cmd,
		RPC:      rpc.Init,
	})
}
