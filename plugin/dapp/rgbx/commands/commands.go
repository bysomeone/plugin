/*Package commands implement dapp client commands*/
package commands

import (
	"encoding/json"
	"strings"

	jsonrpc "github.com/33cn/chain33/rpc/jsonclient"
	rpctypes "github.com/33cn/chain33/rpc/types"
	"github.com/33cn/chain33/types"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/spf13/cobra"
)

/*
 * 实现合约对应客户端
 */

// Cmd rgbx client command
func Cmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rgbx",
		Short: "rgbx command",
		Args:  cobra.MinimumNArgs(1),
	}
	cmd.AddCommand(
		mintAssetCMD(),
		transferAssetCMD(),
		commitDKGCMD(),
		btcAddrScriptCMD(),
		btcDepositTxCMD(),
		btcDepositAddressCMD(),
		btcMintSpendCMD(),
		btcKeyInfoCMD(),
		depositAssetCMD(),
		withdrawAssetCMD(),
		confirmTxCMD(),
		getAssetCMD(),
		getCrossChainInfoCMD(),
		getPendingTxCMD(),
		listPendingTxCMD(),
		listPendingTxByFromCMD(),
		getConfirmedHeightCMD(),
		getOperationCMD(),
		getOperationSupplyCMD(),
		listOperationsCMD(),
		bridgeConfigApplyCMD(),
		bridgeConfigApproveCMD(),
		abandonWithdrawCMD(),
		bumpWithdrawFeeCMD(),
		registerDepositScriptCMD(),
		getBridgeConfigCMD(),
		listAbandonRequestsCMD(),
		listBumpWithdrawFeeRequestsCMD(),
		getDepositAddressCMD(),
		listDepositRegistrationsCMD(),
		getWithdrawConsignmentCMD(),
	)
	return cmd
}

func sendQueryRPC(cmd *cobra.Command, funcName string, req, reply types.Message, runResult bool) {
	rpcAddr, _ := cmd.Flags().GetString("rpc_laddr")
	paraName, _ := cmd.Flags().GetString("paraName")
	payLoad := types.MustPBToJSON(req)
	query := &rpctypes.Query4Jrpc{
		Execer:   types.GetExecName(rtypes.RgbxX, paraName),
		FuncName: funcName,
		Payload:  payLoad,
	}

	ctx := jsonrpc.NewRPCCtx(rpcAddr, "Chain33.Query", query, reply)
	if runResult {
		_, _ = ctx.RunResult()
		return
	}
	ctx.SetResultCb(func(res interface{}) (interface{}, error) {
		msg, ok := res.(types.Message)
		if !ok {
			return res, nil
		}
		resData, err := types.PBToJSONUTF8(msg)
		if err != nil {
			return res, nil
		}
		return json.RawMessage(resData), nil
	})
	ctx.Run()
}

// sendQueryRPCSilent 查询链上数据但**不打印**结果，只把结果解码进 reply。
//
// 为什么需要它：命令的 stdout 是"结果"本身（例如 withdraw 打印的就是 tx 哈希，脚本按长度/内容
// 解析它）。内部辅助查询（例如 withdraw 为定 `-a` 的小数位而读资产记录）绝不能混进 stdout ——
// 一旦混入，调用方拿到的就是"一段 JSON + 哈希"，看起来像失败但其实交易已经发出去了。
// jsonclient 的 RPCCtx.Run/RunResult 都会打印，所以这里直接走 JSONClient.Call。
func sendQueryRPCSilent(cmd *cobra.Command, funcName string, req, reply types.Message) error {
	rpcAddr, _ := cmd.Flags().GetString("rpc_laddr")
	paraName, _ := cmd.Flags().GetString("paraName")
	query := &rpctypes.Query4Jrpc{
		Execer:   types.GetExecName(rtypes.RgbxX, paraName),
		FuncName: funcName,
		Payload:  types.MustPBToJSON(req),
	}
	client, err := jsonrpc.NewJSONClient(rpcAddr)
	if err != nil {
		return err
	}
	return client.Call("Chain33.Query", query, reply)
}

// isRecordNotFound 判断查询错误是不是"链上没有这条记录"（而不是查询本身失败）。
//
// 执行器对不存在的 symbol 回 ErrNotFound。两者对调用方的含义完全不同：前者是**业务结论**
// （交给精度解析给出可操作的报错："这个资产链上查不到"），后者是**故障**（要立刻停下来说清楚
// 是查询不通，而不是让人以为资产不存在）。错误经 JSON-RPC 往返后只剩文案（不是包装的哨兵），
// 所以只能按文案判定。
func isRecordNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), types.ErrNotFound.Error())
}

func markRequired(cmd *cobra.Command, params ...string) {
	for _, param := range params {
		_ = cmd.MarkFlagRequired(param)
	}
}

func sendCreateTxRPC(cmd *cobra.Command, actionName string, req types.Message) {
	rpcAddr, _ := cmd.Flags().GetString("rpc_laddr")
	paraName, _ := cmd.Flags().GetString("paraName")
	payLoad := types.MustPBToJSON(req)
	pm := &rpctypes.CreateTxIn{
		Execer:     types.GetExecName(rtypes.RgbxX, paraName),
		ActionName: actionName,
		Payload:    payLoad,
	}

	var res string
	ctx := jsonrpc.NewRPCCtx(rpcAddr, "Chain33.CreateTransaction", pm, &res)
	ctx.RunWithoutMarshal()
}
