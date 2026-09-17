package commands

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/33cn/chain33/types"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/spf13/cobra"
)

func mintAssetCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use: "mint",
		//Aliases: []string{"cg"},
		Short:   "mint asset",
		Run:     mintAsset,
		Example: "mint -t type -s symbol -a totalAmount -m metaHashHex -o genesisUtxo(hash:index:pkScript)",
	}
	mintAssetFlags(cmd)
	return cmd
}

func transferAssetCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use: "transfer",
		//Aliases: []string{"cg"},
		Short:   "transfer asset",
		Run:     transferAsset,
		Example: "transfer",
	}
	transferAssetFlags(cmd)
	return cmd
}

func mintAssetFlags(cmd *cobra.Command) {
	cmd.Flags().Uint32P("type", "t", 0, "asset type")
	cmd.Flags().Int64P("totalAmount", "a", 10000, "asset total amount")
	cmd.Flags().StringP("symbol", "s", "", "asset symbol")
	cmd.Flags().StringP("metaHash", "m", "", "asset metaData or human-readable identifier hash")
	cmd.Flags().StringP("genesisOut", "o", "", "genesis utxo for binding, hash:index:scriptPubkey format")
	cmd.Flags().Int32P("precision", "p", -1, "asset precision length")
	markRequired(cmd, "totalAmount", "symbol", "genesisOut")
}

func mintAsset(cmd *cobra.Command, args []string) {

	symbol, _ := cmd.Flags().GetString("symbol")
	metaHashStr, _ := cmd.Flags().GetString("metaHash")
	genesisOutStr, _ := cmd.Flags().GetString("genesisOut")
	totalAmount, _ := cmd.Flags().GetInt64("totalAmount")
	ty, _ := cmd.Flags().GetUint32("type")
	precision, _ := cmd.Flags().GetInt32("precision")

	if symbol == "" || len(symbol) > rtypes.MaxAssetSymbolLength {
		_, _ = fmt.Fprintf(os.Stderr, "invalid asset symbol: %s, "+
			"length must less than %d\n", symbol, rtypes.MaxAssetSymbolLength)
		return
	}

	metaHash, err := decodeHexAuto(metaHashStr)
	if err != nil || len(metaHash) > rtypes.MetaHashLen {
		_, _ = fmt.Fprintf(os.Stderr, "invalid meta hash: %s, decode err:%s", metaHashStr, err)
		return
	}

	if precision < 0 || precision > 8 {
		precision = 8
	}

	totalAmount = totalAmount * pow10(int(precision))

	if totalAmount < 1 ||
		totalAmount > rtypes.MaxAssetAmount {
		_, _ = fmt.Fprintf(os.Stderr, "invalid total amount: %d, overflow", totalAmount)
		return
	}

	strs := strings.Split(genesisOutStr, ":")
	if len(strs) != 3 {
		_, _ = fmt.Fprintf(os.Stderr, "invalid genesis out: %s, must  hash:index:pkScript format", genesisOutStr)
		return
	}

	hash := strs[0]
	pkScript, err1 := decodeHexAuto(strs[2])
	index, err2 := strconv.ParseUint(strs[1], 10, 32)

	if err1 != nil || err2 != nil {
		_, _ = fmt.Fprintf(os.Stderr, "invalid genesis out: %s", genesisOutStr)
		return
	}

	mint := &rtypes.MintAsset{
		Symbol:      symbol,
		TotalAmount: totalAmount,
		Precision:   uint32(precision),
		MetaHash:    metaHash,
		Type:        ty,
		GenesisOut: &rtypes.OutPoint{
			Hash:     hash,
			Index:    uint32(index),
			PkScript: pkScript,
		},
	}

	sendCreateTxRPC(cmd, rtypes.NameMintAction, mint)
}

func transferAssetFlags(cmd *cobra.Command) {

	// amount 用字符串接收并按十进制精确换算（与 withdraw 同理，见 parseDecimalAmount）：
	// float64 相乘再截断会少 1 个最小单位。
	cmd.Flags().StringP("amount", "a", "1", "asset amount")
	cmd.Flags().StringP("symbol", "s", "", "asset symbol")
	cmd.Flags().StringP("from", "f", "", "from address, hash:index format for utxo, use sign address if not set")
	cmd.Flags().StringP("to", "t", "", "to address, hash:index format for utxo")
	cmd.Flags().StringP("pkScript", "p", "", "from pkScript( set only when from is an utxo)")
	cmd.Flags().StringP("change", "c", "", "to address, hash:index format for utxo")
	markRequired(cmd, "amount", "symbol", "to")
}

func transferAsset(cmd *cobra.Command, args []string) {

	symbol, _ := cmd.Flags().GetString("symbol")
	from, _ := cmd.Flags().GetString("from")
	to, _ := cmd.Flags().GetString("to")
	change, _ := cmd.Flags().GetString("change")
	amountStr, _ := cmd.Flags().GetString("amount")
	pkScriptStr, _ := cmd.Flags().GetString("pkScript")

	if symbol == "" || len(symbol) > rtypes.MaxAssetSymbolLength {
		_, _ = fmt.Fprintf(os.Stderr, "invalid asset symbol: %s, "+
			"length must less than %d\n", symbol, rtypes.MaxAssetSymbolLength)
		return
	}

	precision := 8
	req := &types.ReqString{
		Data: symbol,
	}
	reply := &rtypes.RgbxAsset{}
	sendQueryRPC(cmd, "GetAsset", req, reply, true)
	if reply.GetSymbol() != "" {
		precision = int(reply.Precision)
	}

	amount, amountErr := parseDecimalAmount(amountStr, precision)
	if amountErr != nil {
		_, _ = fmt.Fprintf(os.Stderr, "%v\n", amountErr)
		return
	}
	if amount < 1 ||
		amount > rtypes.MaxAssetAmount {
		_, _ = fmt.Fprintf(os.Stderr, "invalid amount: %q, overflow", amountStr)
		return
	}
	pkScript, err := decodeHexAuto(pkScriptStr)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "invalid pkScript: %s, decode err: %s", pkScriptStr, err)
		return
	}

	transfer := &rtypes.TransferAsset{
		Symbol:           symbol,
		Amount:           amount,
		FromUtxo:         from,
		To:               to,
		ChangeAddr:       change,
		FromUtxoPkScript: pkScript,
	}
	sendCreateTxRPC(cmd, rtypes.NameTransferAction, transfer)
}
