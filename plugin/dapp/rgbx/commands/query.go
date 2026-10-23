package commands

import (
	"fmt"
	"math"
	"os"

	"github.com/33cn/chain33/types"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/spf13/cobra"
)

func listPendingTxCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "listPendingTx",
		Aliases: []string{"lp"},
		Short:   "list pending tx",
		Run:     listPending,
		Example: "listPendingTx -s startHeight -i startIndex -c count",
	}
	listPendingFlags(cmd)
	return cmd
}

func listPendingFlags(cmd *cobra.Command) {

	cmd.Flags().Uint64P("startHeight", "s", 0, "list start Height")
	cmd.Flags().Uint64P("startIndex", "i", 0, "list start tx index")
	cmd.Flags().Uint32P("count", "c", 1, "list tx count")
}

func listPending(cmd *cobra.Command, _ []string) {

	startHeight, _ := cmd.Flags().GetUint64("startHeight")
	startIndex, _ := cmd.Flags().GetUint64("startIndex")
	count, _ := cmd.Flags().GetUint32("count")

	req := &rtypes.ReqListPendingTx{
		StartHeight: int64(startHeight),
		StartIndex:  int64(startIndex),
		Count:       int32(count),
	}
	reply := &rtypes.PendingTxs{}
	sendQueryRPC(cmd, "ListPendingTx", req, reply, false)
}

func getPendingTxCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "getPendingTx",
		Aliases: []string{"gp"},
		Short:   "get pending tx by height",
		Run:     getPending,
		Example: "getPendingTx -h height -i index",
	}
	getPendingFlags(cmd)
	return cmd
}

func getPendingFlags(cmd *cobra.Command) {

	cmd.Flags().Uint64P("height", "h", 0, "block height")
	cmd.Flags().Uint64P("index", "i", 0, "tx index")
}

func getPending(cmd *cobra.Command, _ []string) {

	height, _ := cmd.Flags().GetUint64("height")
	index, _ := cmd.Flags().GetUint64("index")

	req := &rtypes.ReqGetPendingTx{
		Height: int64(height),
		Index:  int64(index),
	}
	reply := &rtypes.PendingTx{}
	sendQueryRPC(cmd, "GetPendingTx", req, reply, false)
}

func getAssetCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "getAsset",
		Aliases: []string{"ga"},
		Short:   "get rgbx asset",
		Run:     getAsset,
		Example: "getAsset -s symbol",
	}
	getAssetFlags(cmd)
	return cmd
}

func getAssetFlags(cmd *cobra.Command) {

	cmd.Flags().StringP("symbol", "s", "", "asset symbol")
	markRequired(cmd, "symbol")
}

func getAsset(cmd *cobra.Command, _ []string) {

	symbol, _ := cmd.Flags().GetString("symbol")
	if symbol == "" || len(symbol) > rtypes.MaxAssetSymbolLength {
		_, _ = fmt.Fprintf(os.Stderr, "invalid asset symbol: %s, "+
			"length must less than %d\n", symbol, rtypes.MaxAssetSymbolLength)
		return
	}

	req := &types.ReqString{
		Data: symbol,
	}
	reply := &rtypes.RgbxAsset{}
	sendQueryRPC(cmd, "GetAsset", req, reply, false)
}

func getConfirmedHeightCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "getConfirmedHeight",
		Aliases: []string{"gch"},
		Short:   "get rgbx confirmed height",
		Run:     getConfirmedHeight,
		Example: "getConfirmedHeight",
	}
	return cmd
}

func getConfirmedHeight(cmd *cobra.Command, _ []string) {

	reply := &types.Int64{}
	sendQueryRPC(cmd, "GetConfirmedHeight", &types.ReqNil{}, reply, false)
}

func getCrossChainInfoCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "getCrossChainInfo",
		Aliases: []string{"gc"},
		Short:   "get cross-chain info",
		Run:     getCrossChainInfo,
		Example: "getCrossChainInfo -s BTC",
	}
	getCrossChainInfoFlags(cmd)
	return cmd
}

func getCrossChainInfoFlags(cmd *cobra.Command) {
	cmd.Flags().StringP("symbol", "s", "BTC", "cross-chain asset symbol")
	markRequired(cmd, "symbol")
}

func getCrossChainInfo(cmd *cobra.Command, _ []string) {
	symbol, _ := cmd.Flags().GetString("symbol")
	if symbol == "" || len(symbol) > rtypes.MaxAssetSymbolLength {
		_, _ = fmt.Fprintf(os.Stderr, "invalid asset symbol: %s, length must less than %d\n", symbol, rtypes.MaxAssetSymbolLength)
		return
	}
	reply := &rtypes.CrossChainInfo{}
	sendQueryRPC(cmd, "GetCrossChainInfo", &types.ReqString{Data: symbol}, reply, false)
}

// getOperationCMD 按全局 operationId 查台账记录（S2）。operationId 为 64 位十六进制（32 字节）。
func getOperationCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "getOperation",
		Aliases: []string{"go"},
		Short:   "get operation ledger record by global operationId",
		Run:     getOperation,
		Example: "getOperation --operationId 6f...（64 位 hex）",
	}
	cmd.Flags().String("operationId", "", "32-byte operation id in hex")
	markRequired(cmd, "operationId")
	return cmd
}

// parseOperationIDHex 解析 operationId 十六进制串：容忍 "0x" 前缀与空白（复用 decodeHexAuto，
// 与 getCrossChainInfo 输出的 pubkey/pkScript 同一口径），但长度必须是 32 字节 —— 长度不对就不是
// 一个 id，宁可报错也不要拿半截 id 去查（那只会查到"没有这条记录"，误导排查方向）。
func parseOperationIDHex(s string) ([]byte, error) {
	opID, err := decodeHexAuto(s)
	if err != nil {
		return nil, err
	}
	if len(opID) != rtypes.OperationIDLen {
		return nil, fmt.Errorf("invalid operationId length: got %d bytes, expect %d",
			len(opID), rtypes.OperationIDLen)
	}
	return opID, nil
}

// validateListOperationsArgs 参数校验（抽出来便于单测：CLI 的错都必须在发 RPC 之前拦下）。
func validateListOperationsArgs(symbol string, kind int32, start int64, count int32) error {
	if symbol == "" {
		return fmt.Errorf("symbol is required")
	}
	if kind != rtypes.OperationKindMint && kind != rtypes.OperationKindBurn {
		return fmt.Errorf("invalid kind %d: 1=mint 2=burn", kind)
	}
	if start < 0 {
		return fmt.Errorf("invalid start %d: must not be negative", start)
	}
	if count <= 0 {
		return fmt.Errorf("invalid count %d: must be positive", count)
	}
	return nil
}

func getOperation(cmd *cobra.Command, _ []string) {

	opIDHex, _ := cmd.Flags().GetString("operationId")
	opID, err := parseOperationIDHex(opIDHex)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "invalid operationId %q: %s\n", opIDHex, err)
		return
	}
	reply := &rtypes.OperationRecord{}
	sendQueryRPC(cmd, "GetOperation", &rtypes.ReqGetOperation{OperationId: opID}, reply, false)
}

// getOperationSupplyCMD 查某 symbol 的累计铸造/销毁（S2）：minted − burned = 桥仍欠的额度。
func getOperationSupplyCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "getOperationSupply",
		Aliases: []string{"gos"},
		Short:   "get minted/burned totals of an asset symbol",
		Run:     getOperationSupply,
		Example: "getOperationSupply -s XBTC",
	}
	cmd.Flags().StringP("symbol", "s", "", "cross-chain asset symbol")
	markRequired(cmd, "symbol")
	return cmd
}

func getOperationSupply(cmd *cobra.Command, _ []string) {

	symbol, _ := cmd.Flags().GetString("symbol")
	if symbol == "" {
		_, _ = fmt.Fprintln(os.Stderr, "symbol is required")
		return
	}
	reply := &rtypes.OperationSupply{}
	sendQueryRPC(cmd, "GetOperationSupply", &types.ReqString{Data: symbol}, reply, false)
}

// listOperationsCMD 枚举某 symbol 某类操作的台账记录（S2），分页用 --start / 返回的 nextIndex。
func listOperationsCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "listOperations",
		Aliases: []string{"lo"},
		Short:   "list operation ledger records of an asset symbol",
		Run:     listOperations,
		Example: "listOperations -s XBTC -k 1 -i 0 -c 20  (k: 1=mint 2=burn)",
	}
	cmd.Flags().StringP("symbol", "s", "", "cross-chain asset symbol")
	cmd.Flags().Int32P("kind", "k", int32(rtypes.OperationKindMint), "operation kind: 1=mint(deposit) 2=burn(withdraw)")
	cmd.Flags().Int64P("start", "i", 0, "start index (use nextIndex from the previous page)")
	cmd.Flags().Int32P("count", "c", 20, "max records to return")
	markRequired(cmd, "symbol")
	return cmd
}

func listOperations(cmd *cobra.Command, _ []string) {

	symbol, _ := cmd.Flags().GetString("symbol")
	kind, _ := cmd.Flags().GetInt32("kind")
	start, _ := cmd.Flags().GetInt64("start")
	count, _ := cmd.Flags().GetInt32("count")
	if err := validateListOperationsArgs(symbol, kind, start, count); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "invalid args: %s\n", err)
		return
	}
	reply := &rtypes.OperationRecords{}
	sendQueryRPC(cmd, "ListOperations", &rtypes.ReqListOperations{
		AssetSymbol: symbol,
		Kind:        kind,
		Start:       start,
		Count:       count,
	}, reply, false)
}

// getWithdrawConsignmentCMD returns the consignment hash registered by an RGB20 withdrawal's
// Confirm. The burn hash is the chain33 transaction hash of the withdrawal (0x prefix allowed).
func getWithdrawConsignmentCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "get-withdraw-consignment",
		Short:   "get the consignment sha256 registered for an RGB20 withdrawal burn",
		Run:     getWithdrawConsignment,
		Example: "get-withdraw-consignment --tx-hash 0x...",
	}
	cmd.Flags().String("tx-hash", "", "chain33 withdrawal (burn) transaction hash in hex")
	markRequired(cmd, "tx-hash")
	return cmd
}

func getWithdrawConsignment(cmd *cobra.Command, _ []string) {
	txHash, _ := cmd.Flags().GetString("tx-hash")
	if txHash == "" {
		_, _ = fmt.Fprintln(os.Stderr, "tx hash is required")
		return
	}
	reply := &types.ReplyString{}
	sendQueryRPC(cmd, "GetWithdrawConsignmentHash", &types.ReqString{Data: txHash}, reply, false)
}

func listPendingTxByFromCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "listPendingTxByFrom",
		Aliases: []string{"lpf"},
		Short:   "list pending tx by from address",
		Run:     listPendingByFrom,
		Example: "listPendingTxByFrom -f 1xxxxxxxxxxxxxxxx",
	}
	listPendingTxByFromFlags(cmd)
	return cmd
}

func listPendingTxByFromFlags(cmd *cobra.Command) {
	cmd.Flags().StringP("from", "f", "", "from address")
	markRequired(cmd, "from")
}

func listPendingByFrom(cmd *cobra.Command, _ []string) {
	from, _ := cmd.Flags().GetString("from")
	if from == "" {
		_, _ = fmt.Fprintln(os.Stderr, "from address is required")
		return
	}
	reply := &rtypes.PendingTxs{}
	sendQueryRPC(cmd, "ListPendingTxByFrom", &types.ReqString{Data: from}, reply, false)
}

// maxListCount is the executor's upper bound for one height-indexed listing call
// (executor.maxListCount). Checking it here turns an invalid request into a local error
// instead of a round trip.
const maxListCount = 1000

// listHeightRangeFlags registers the paging flags shared by the height-indexed request
// lists (ReqListPendingTx). startHeight/startIndex/count keep the names and shorthands of
// listPendingTx; endHeight = 0 means "up to the current height".
func listHeightRangeFlags(cmd *cobra.Command) {
	cmd.Flags().Uint64P("startHeight", "s", 0, "list start height")
	cmd.Flags().Uint64P("startIndex", "i", 0, "skip records with a lower block tx index at startHeight")
	cmd.Flags().Uint64P("endHeight", "e", 0, "list end height (0 = up to the current height)")
	cmd.Flags().Uint32P("count", "c", 20, "max records to return (1..1000)")
}

func buildListPendingReq(cmd *cobra.Command) (*rtypes.ReqListPendingTx, error) {
	startHeight, _ := cmd.Flags().GetUint64("startHeight")
	startIndex, _ := cmd.Flags().GetUint64("startIndex")
	endHeight, _ := cmd.Flags().GetUint64("endHeight")
	count, _ := cmd.Flags().GetUint32("count")
	return validateListPendingReq(startHeight, startIndex, endHeight, count)
}

// validateListPendingReq mirrors the executor's paging validation, so an impossible range
// is reported before any RPC.
func validateListPendingReq(startHeight, startIndex, endHeight uint64,
	count uint32) (*rtypes.ReqListPendingTx, error) {

	if count == 0 || count > maxListCount {
		return nil, fmt.Errorf("invalid count %d: must be in [1, %d]", count, maxListCount)
	}
	if startHeight > math.MaxInt64 || startIndex > math.MaxInt64 || endHeight > math.MaxInt64 {
		return nil, fmt.Errorf("invalid height/index: value overflows int64")
	}
	if endHeight != 0 && endHeight < startHeight {
		return nil, fmt.Errorf("invalid height range: endHeight %d is below startHeight %d", endHeight, startHeight)
	}
	return &rtypes.ReqListPendingTx{
		StartHeight: int64(startHeight),
		StartIndex:  int64(startIndex),
		EndHeight:   int64(endHeight),
		Count:       int32(count),
	}, nil
}

// getBridgeConfigCMD returns the effective bridge config; an empty symbol addresses the
// global record (same key the apply command uses).
func getBridgeConfigCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "get-bridge-config",
		Short:   "get the effective bridge configuration",
		Run:     getBridgeConfig,
		Example: "get-bridge-config\n  get-bridge-config --asset-symbol XBTC",
	}
	cmd.Flags().String("asset-symbol", "", "asset symbol; empty returns the global config")
	return cmd
}

func getBridgeConfig(cmd *cobra.Command, _ []string) {
	symbol, _ := cmd.Flags().GetString("asset-symbol")
	reply := &rtypes.BridgeConfigApply{}
	sendQueryRPC(cmd, "GetBridgeConfig", &types.ReqString{Data: symbol}, reply, false)
}

func listAbandonRequestsCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list-abandon-requests",
		Short:   "list abandon-withdraw proposals by height range",
		Run:     listAbandonRequests,
		Example: "list-abandon-requests -s 100 -e 200 -c 20",
	}
	listHeightRangeFlags(cmd)
	return cmd
}

func listAbandonRequests(cmd *cobra.Command, _ []string) {
	req, err := buildListPendingReq(cmd)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "invalid args: %s\n", err)
		return
	}
	reply := &rtypes.AbandonWithdrawList{}
	sendQueryRPC(cmd, "ListAbandonRequests", req, reply, false)
}

func listBumpWithdrawFeeRequestsCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list-bump-withdraw-fee-requests",
		Short:   "list withdrawal fee-bump requests by height range",
		Run:     listBumpWithdrawFeeRequests,
		Example: "list-bump-withdraw-fee-requests -s 100 -e 200 -c 20",
	}
	listHeightRangeFlags(cmd)
	return cmd
}

func listBumpWithdrawFeeRequests(cmd *cobra.Command, _ []string) {
	req, err := buildListPendingReq(cmd)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "invalid args: %s\n", err)
		return
	}
	reply := &rtypes.BumpWithdrawFeeList{}
	sendQueryRPC(cmd, "ListBumpWithdrawFeeRequests", req, reply, false)
}

func getDepositAddressCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "get-deposit-address",
		Short:   "get the registered BTC deposit address of a user id",
		Run:     getDepositAddress,
		Example: "get-deposit-address --user-id <chain33Address>",
	}
	cmd.Flags().String("user-id", "", "user id (the chain33 address that registered the script)")
	markRequired(cmd, "user-id")
	return cmd
}

func getDepositAddress(cmd *cobra.Command, _ []string) {
	userID, _ := cmd.Flags().GetString("user-id")
	if userID == "" {
		_, _ = fmt.Fprintln(os.Stderr, "user id is required")
		return
	}
	reply := &rtypes.DepositScriptItem{}
	sendQueryRPC(cmd, "GetDepositAddress", &types.ReqString{Data: userID}, reply, false)
}

func listDepositRegistrationsCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list-deposit-registrations",
		Short:   "list registered BTC deposit scripts by height range",
		Run:     listDepositRegistrations,
		Example: "list-deposit-registrations -s 100 -e 200 -c 20",
	}
	listHeightRangeFlags(cmd)
	return cmd
}

func listDepositRegistrations(cmd *cobra.Command, _ []string) {
	req, err := buildListPendingReq(cmd)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "invalid args: %s\n", err)
		return
	}
	reply := &rtypes.DepositScriptList{}
	sendQueryRPC(cmd, "ListDepositRegistrations", req, reply, false)
}
