/*Package commands implement dapp client commands*/
package commands

import (
	"github.com/33cn/chain33/rpc/jsonclient"
	"github.com/33cn/chain33/types"
	"github.com/spf13/cobra"

	rpctypes "github.com/33cn/chain33/rpc/types"
	ptypes "github.com/33cn/plugin/plugin/dapp/calculator/types/calculator"
)

/*
 * 实现合约对应客户端
 */

// Cmd calculator client command
func Cmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "calculator",
		Short: "calculator command",
		Args:  cobra.MinimumNArgs(1),
	}
	cmd.AddCommand(
		//add sub command
		createAddCmd(),
		queryCalcCountCmd(),
	)
	return cmd
}

func createAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add",
		Short: "create add calc tx",
		Run:   createAdd,
	}
	cmd.Flags().Int32P("summand", "s", 0, "summand integer number")
	cmd.Flags().Int32P("addend", "a", 0, "addend integer number")
	cmd.MarkFlagRequired("summand")
	cmd.MarkFlagRequired("addend")
	return cmd
}

func createAdd(cmd *cobra.Command, args []string) {
	rpcLaddr, _ := cmd.Flags().GetString("rpc_laddr")
	summand, _ := cmd.Flags().GetInt32("summand")
	addend, _ := cmd.Flags().GetInt32("addend")

	req := ptypes.Add{
		Summand: summand,
		Addend:  addend,
	}
	chain33Req := rpctypes.CreateTxIn{
		Execer:     ptypes.CalculatorX,
		ActionName: ptypes.NameAddAction,
		Payload:    types.MustPBToJSON(&req),
	}
	var res string
	ctx := jsonclient.NewRPCCtx(rpcLaddr, "Chain33.CreateTransaction", chain33Req, &res)
	//ctx := jsonclient.NewRPCCtx(rpcLaddr, "calculator.CreateRawAddTx", req, &res)
	ctx.RunWithoutMarshal()
}

func queryCalcCountCmd() *cobra.Command {

	cmd := &cobra.Command{
		Use:   "query_count",
		Short: "query calculator count",
		Run:   queryCalcCount,
	}
	cmd.Flags().StringP("action", "a", "", "calc action name[Add | Sub | Mul | Div]")
	cmd.MarkFlagRequired("action")

	return cmd
}

func queryCalcCount(cmd *cobra.Command, args []string) {

	rpcLaddr, _ := cmd.Flags().GetString("rpc_laddr")
	action, _ := cmd.Flags().GetString("action")
	req := ptypes.ReqQueryCalcCount{
		Action: action,
	}
	chain33Req := &rpctypes.Query4Jrpc{
		Execer:   ptypes.CalculatorX,
		FuncName: "CalcCount",
		Payload:  types.MustPBToJSON(&req),
	}
	var res interface{}
	res = &ptypes.ReplyQueryCalcCount{}
	ctx := jsonclient.NewRPCCtx(rpcLaddr, "Chain33.Query", chain33Req, &res)
	//ctx := jsonclient.NewRPCCtx(rpcLaddr, "calculator.QueryCalcCount", req, &res)
	ctx.Run()
}
