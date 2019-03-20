package calculator

import (
	"github.com/33cn/chain33/pluginmgr"
	"github.com/33cn/plugin/plugin/dapp/calculator/commands"
	"github.com/33cn/plugin/plugin/dapp/calculator/executor"
	"github.com/33cn/plugin/plugin/dapp/calculator/rpc"
	ptypes "github.com/33cn/plugin/plugin/dapp/calculator/types/calculator"
)

/*
 * 初始化dapp相关的组件
 */

func init() {
	pluginmgr.Register(&pluginmgr.PluginBase{
		Name:     ptypes.CalculatorX,
		ExecName: executor.GetName(),
		Exec:     executor.Init,
		Cmd:      commands.Cmd,
		RPC:      rpc.Init,
	})
}
