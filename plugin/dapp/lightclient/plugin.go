package types

import (
	"github.com/33cn/chain33/pluginmgr"
	"github.com/33cn/plugin/plugin/dapp/lightclient/commands"
	"github.com/33cn/plugin/plugin/dapp/lightclient/executor"
	"github.com/33cn/plugin/plugin/dapp/lightclient/rpc"
	lightclienttypes "github.com/33cn/plugin/plugin/dapp/lightclient/types"
)

/*
 * 初始化dapp相关的组件
 */

func init() {
	pluginmgr.Register(&pluginmgr.PluginBase{
		Name:     lightclienttypes.LightclientX,
		ExecName: executor.GetName(),
		Exec:     executor.Init,
		Cmd:      commands.Cmd,
		RPC:      rpc.Init,
	})
}
