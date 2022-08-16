// Copyright Fuzamei Corp. 2018 All Rights Reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package common

import (
	"fmt"
	"github.com/33cn/chain33/rpc/jsonclient"
	rpcTypes "github.com/33cn/chain33/rpc/types"
	commandtypes "github.com/33cn/chain33/system/dapp/commands/types"
	"github.com/33cn/chain33/types"
	"github.com/spf13/cobra"
	"os"
	"strings"
)

func getRealExecName(paraName string, name string) string {
	if strings.HasPrefix(name, "user.p.") {
		return name
	}
	return paraName + name
}

// CreateTransfer2Exec 通用的创建 send_exec 交易， 额外指定资产合约
func CreateTransfer2Exec(cmd *cobra.Command, args []string, fromExec string) {
	paraName, _ := cmd.Flags().GetString("paraName")
	exec, _ := cmd.Flags().GetString("exec")
	realExec := getRealExecName(paraName, exec)

	rpcLaddr, _ := cmd.Flags().GetString("rpc_laddr")
	cfg, err := commandtypes.GetChainConfig(rpcLaddr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}

	execAddr, err := commandtypes.GetExecAddr(realExec, cfg.DefaultAddressID)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}

	amount, _ := cmd.Flags().GetFloat64("amount")
	note, _ := cmd.Flags().GetString("note")
	symbol, _ := cmd.Flags().GetString("symbol")

	amountInt64, err := types.FormatFloatDisplay2Value(amount, cfg.CoinPrecision)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}

	payload := &types.AssetsTransferToExec{
		To:        execAddr,
		Amount:    amountInt64,
		Note:      []byte(note),
		Cointoken: symbol,
		ExecName:  exec,
	}

	params := &rpcTypes.CreateTxIn{
		Execer:     types.GetExecName(fromExec, paraName),
		ActionName: "TransferToExec",
		Payload:    types.MustPBToJSON(payload),
	}

	ctx := jsonclient.NewRPCCtx(rpcLaddr, "Chain33.CreateTransaction", params, nil)
	ctx.RunWithoutMarshal()
}

// CreateAssetWithdraw 通用的创建 withdraw 交易， 额外指定资产合约
func CreateAssetWithdraw(cmd *cobra.Command, args []string, fromExec string) {
	exec, _ := cmd.Flags().GetString("exec")
	paraName, _ := cmd.Flags().GetString("paraName")
	exec = getRealExecName(paraName, exec)
	amount, _ := cmd.Flags().GetFloat64("amount")
	note, _ := cmd.Flags().GetString("note")
	symbol, _ := cmd.Flags().GetString("symbol")

	exec = getRealExecName(paraName, exec)

	rpcLaddr, _ := cmd.Flags().GetString("rpc_laddr")
	cfg, err := commandtypes.GetChainConfig(rpcLaddr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	execAddr, err := commandtypes.GetExecAddr(exec, cfg.DefaultAddressID)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}

	amountInt64, err := types.FormatFloatDisplay2Value(amount, cfg.CoinPrecision)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	payload := &types.AssetsWithdraw{
		To:        execAddr,
		Amount:    amountInt64,
		Note:      []byte(note),
		Cointoken: symbol,
		ExecName:  exec,
	}
	params := &rpcTypes.CreateTxIn{
		Execer:     types.GetExecName(fromExec, paraName),
		ActionName: "Withdraw",
		Payload:    types.MustPBToJSON(payload),
	}

	ctx := jsonclient.NewRPCCtx(rpcLaddr, "Chain33.CreateTransaction", params, nil)
	ctx.RunWithoutMarshal()
}

// CreateAssetTransfer 通用的创建 transfer 交易， 额外指定资产合约
func CreateAssetTransfer(cmd *cobra.Command, args []string, fromExec string) {
	toAddr, _ := cmd.Flags().GetString("to")
	amount, _ := cmd.Flags().GetFloat64("amount")
	note, _ := cmd.Flags().GetString("note")
	symbol, _ := cmd.Flags().GetString("symbol")
	paraName, _ := cmd.Flags().GetString("paraName")

	rpcLaddr, _ := cmd.Flags().GetString("rpc_laddr")
	cfg, err := commandtypes.GetChainConfig(rpcLaddr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	amountInt64, err := types.FormatFloatDisplay2Value(amount, cfg.CoinPrecision)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}

	payload := &types.AssetsTransfer{
		To:        toAddr,
		Amount:    amountInt64,
		Note:      []byte(note),
		Cointoken: symbol,
	}
	params := &rpcTypes.CreateTxIn{
		Execer:     types.GetExecName(fromExec, paraName),
		ActionName: "Transfer",
		Payload:    types.MustPBToJSON(payload),
	}

	ctx := jsonclient.NewRPCCtx(rpcLaddr, "Chain33.CreateTransaction", params, nil)
	ctx.RunWithoutMarshal()
}
