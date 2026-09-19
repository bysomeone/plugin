package commands

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"

	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/spf13/cobra"
)

func btcAddrScriptCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "addrScript",
		Short: "convert btc address to pkScript hex",
		Run:   btcAddrScript,
	}
	cmd.Flags().StringP("address", "a", "", "bitcoin address")
	cmd.Flags().String("net", "mainnet", "bitcoin network: mainnet|testnet|regtest|simnet")
	markRequired(cmd, "address")
	return cmd
}

func btcAddrScript(cmd *cobra.Command, _ []string) {
	address, _ := cmd.Flags().GetString("address")
	netName, _ := cmd.Flags().GetString("net")

	params, err := parseNetParams(netName)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "invalid net: %s, err: %v\n", netName, err)
		return
	}
	addr, err := btcutil.DecodeAddress(address, params)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "invalid address: %s, decode err: %v\n", address, err)
		return
	}
	script, err := txscript.PayToAddrScript(addr)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "convert address to script failed: %v\n", err)
		return
	}
	fmt.Println(hex.EncodeToString(script))
}

func btcDepositTxCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "btcDepositTx",
		Short: "build, sign and broadcast btc deposit tx (pays the per-user P2WSH deposit address)",
		Run:   btcDepositTx,
		Example: "btcDepositTx --net regtest --rpcHost 127.0.0.1:18443 " +
			"--wif <wif> --utxo <txid:vout:amountSats:pkScriptHex> --depositAddress <chain33Addr> " +
			"--tssPubkey <tssPubkeyHex> --amount 100000 --fee 300",
	}
	cmd.Flags().String("net", "regtest", "bitcoin network: mainnet|testnet|regtest|simnet")
	cmd.Flags().String("rpcHost", "127.0.0.1:18443", "bitcoin rpc host")
	cmd.Flags().String("rpcUser", "", "bitcoin rpc user (optional)")
	cmd.Flags().String("rpcPass", "", "bitcoin rpc password (optional)")
	cmd.Flags().Bool("disableTLS", true, "disable rpc tls")
	cmd.Flags().String("rpcCertFile", "", "bitcoin rpc cert file path (optional, required when TLS enabled)")
	cmd.Flags().String("wif", "", "sender private key in WIF format")
	cmd.Flags().String("utxo", "", "single input utxo, format: txid:vout:amountSats:pkScriptHex (hex fields accept an optional 0x prefix)")
	cmd.Flags().String("depositAddress", "", "chain33 deposit address (= userID of the p2wsh derivation)")
	cmd.Flags().String("tssPubkey", "", "tss group pubkey, 33-byte compressed hex (from rgbx getCrossChainInfo); an optional 0x prefix is accepted")
	cmd.Flags().Int64("amount", 0, "deposit amount in satoshis")
	cmd.Flags().Int64("fee", 0, "tx fee in satoshis")
	cmd.Flags().String("changeAddress", "", "optional change address, default from private key")
	markRequired(cmd, "wif", "utxo", "depositAddress", "tssPubkey", "amount", "fee")
	return cmd
}

// btcDepositAddressCMD 本地推导某用户的 P2WSH 充值地址（纯函数，不依赖桥）。
//
// 用途：对账/排障/离线核对（以及与桥发放的地址交叉验证）。
// **注意**：本命令不会让桥 watch 该脚本 —— 桥只 watch 它自己发放过的地址（用户要地址时按需 import，
// 见 neutrino 的充值地址发放 HTTP）。所以真正给用户发地址要走桥的接口；这里算出来的地址如果桥没
// watch，用户打进去的 BTC 桥是看不见的（钱不会丢，但不会被自动认领）。
func btcDepositAddressCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "btcDepositAddress",
		Short:   "derive the per-user p2wsh btc deposit address (offline, does not register it at the bridge)",
		Run:     btcDepositAddress,
		Example: "btcDepositAddress --net regtest -d <chain33Addr> -k <tssPubkeyHex>",
	}
	cmd.Flags().String("net", "regtest", "bitcoin network: mainnet|testnet|regtest|simnet")
	cmd.Flags().StringP("depositAddress", "d", "", "chain33 deposit address (= userID of the derivation)")
	cmd.Flags().StringP("tssPubkey", "k", "", "tss group pubkey, 33-byte compressed hex (from rgbx getCrossChainInfo); an optional 0x prefix is accepted")
	markRequired(cmd, "depositAddress", "tssPubkey")
	return cmd
}

func btcDepositAddress(cmd *cobra.Command, _ []string) {
	netName, _ := cmd.Flags().GetString("net")
	chain33Addr, _ := cmd.Flags().GetString("depositAddress")
	pubkeyHex, _ := cmd.Flags().GetString("tssPubkey")

	params, err := parseNetParams(netName)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "invalid net: %s, err: %v\n", netName, err)
		return
	}
	pubkey, err := decodeHexAuto(pubkeyHex)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "invalid tssPubkey: %s, decode err: %v\n", pubkeyHex, err)
		return
	}
	userID := strings.TrimSpace(chain33Addr)
	addr, err := rtypes.DeriveDepositAddress(userID, pubkey, params)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "derive deposit address failed: %v\n", err)
		return
	}
	pkScript, err := rtypes.DeriveDepositPkScript(userID, pubkey)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "derive deposit pkScript failed: %v\n", err)
		return
	}
	witnessScript, err := rtypes.DeriveDepositWitnessScript(userID, pubkey)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "derive deposit witnessScript failed: %v\n", err)
		return
	}
	_, _ = fmt.Fprintf(os.Stderr, "note: this address is derived locally; the bridge only sees deposits to "+
		"scripts it has imported (request the address from the bridge to register it)\n")
	fmt.Printf("{\"address\":\"%s\",\"pkScript\":\"%s\",\"witnessScript\":\"%s\",\"spec\":\"%s\"}\n",
		addr, hex.EncodeToString(pkScript), hex.EncodeToString(witnessScript), rtypes.P2WSHDepositSpecV1)
}

type depositUTXO struct {
	hash     *chainhash.Hash
	vout     uint32
	amount   int64
	pkScript []byte
}

func parseDepositUTXO(raw string) (*depositUTXO, error) {
	parts := strings.Split(raw, ":")
	if len(parts) != 4 {
		return nil, fmt.Errorf("invalid utxo format: %s", raw)
	}
	// txid 同样容忍 0x 前缀（chainhash.NewHashFromStr 内部不做这个处理）。
	hash, err := chainhash.NewHashFromStr(stripHexPrefix(parts[0]))
	if err != nil {
		return nil, fmt.Errorf("invalid utxo txid: %w", err)
	}
	vout64, err := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid utxo vout: %w", err)
	}
	amount, err := strconv.ParseInt(strings.TrimSpace(parts[2]), 10, 64)
	if err != nil || amount <= 0 {
		return nil, fmt.Errorf("invalid utxo amount: %s", parts[2])
	}
	pkScript, err := decodeHexAuto(parts[3])
	if err != nil {
		return nil, fmt.Errorf("invalid utxo pkScript: %w", err)
	}
	return &depositUTXO{
		hash:     hash,
		vout:     uint32(vout64),
		amount:   amount,
		pkScript: pkScript,
	}, nil
}

func btcDepositTx(cmd *cobra.Command, _ []string) {
	netName, _ := cmd.Flags().GetString("net")
	rpcHost, _ := cmd.Flags().GetString("rpcHost")
	rpcUser, _ := cmd.Flags().GetString("rpcUser")
	rpcPass, _ := cmd.Flags().GetString("rpcPass")
	disableTLS, _ := cmd.Flags().GetBool("disableTLS")
	rpcCertFile, _ := cmd.Flags().GetString("rpcCertFile")
	wifStr, _ := cmd.Flags().GetString("wif")
	utxoRaw, _ := cmd.Flags().GetString("utxo")
	tssPubkeyHex, _ := cmd.Flags().GetString("tssPubkey")
	chain33Addr, _ := cmd.Flags().GetString("depositAddress")
	amount, _ := cmd.Flags().GetInt64("amount")
	fee, _ := cmd.Flags().GetInt64("fee")
	changeAddrStr, _ := cmd.Flags().GetString("changeAddress")

	params, err := parseNetParams(netName)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "invalid net: %s, err: %v\n", netName, err)
		return
	}
	if amount <= 0 || fee < 0 {
		_, _ = fmt.Fprintf(os.Stderr, "invalid amount(%d) or fee(%d)\n", amount, fee)
		return
	}
	utxo, err := parseDepositUTXO(utxoRaw)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "invalid utxo: %v\n", err)
		return
	}
	if utxo.amount < amount+fee {
		_, _ = fmt.Fprintf(os.Stderr, "insufficient utxo amount, have=%d need=%d\n", utxo.amount, amount+fee)
		return
	}

	wif, err := btcutil.DecodeWIF(strings.TrimSpace(wifStr))
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "invalid wif: %v\n", err)
		return
	}
	// 充值目标 = 按 (userID, tssPub) 派生的 P2WSH 地址（冻结规格见 rgbx/types/p2wsh_deposit.go）。
	// 硬切后**不再构造 OP_RETURN 充值承诺**：归属由"付给了谁的派生脚本"认定，链上会按同一份派生
	// 重建 program 并核金额；带 OP_RETURN 的旧形态现在必被 checkDeposit 拒（它只看派生脚本）。
	// tssPubkey 只能来自该 symbol 的 CrossChainInfo（rgbx getCrossChainInfo -s BTC），
	// 用错世代（re-DKG 后）的钥会让钱落到桥认不出的脚本里。
	tssPub, err := decodeHexAuto(tssPubkeyHex)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "invalid tssPubkey: %v\n", err)
		return
	}
	userID := strings.TrimSpace(chain33Addr)
	depositAddrStr, err := rtypes.DeriveDepositAddress(userID, tssPub, params)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "derive deposit address failed: %v\n", err)
		return
	}
	depositAddr, err := btcutil.DecodeAddress(depositAddrStr, params)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "decode derived deposit address failed: %v\n", err)
		return
	}
	_, _ = fmt.Fprintf(os.Stderr, "deposit address (p2wsh, derived from chain33 %s): %s\n", userID, depositAddrStr)

	var changeAddr btcutil.Address
	if strings.TrimSpace(changeAddrStr) == "" {
		changeAddr, err = btcutil.NewAddressPubKeyHash(
			btcutil.Hash160(wif.PrivKey.PubKey().SerializeCompressed()), params,
		)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "create default change address failed: %v\n", err)
			return
		}
	} else {
		changeAddr, err = btcutil.DecodeAddress(strings.TrimSpace(changeAddrStr), params)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "invalid changeAddress: %v\n", err)
			return
		}
	}

	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(wire.NewTxIn(wire.NewOutPoint(utxo.hash, utxo.vout), nil, nil))

	depositScript, err := txscript.PayToAddrScript(depositAddr)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "build deposit script failed: %v\n", err)
		return
	}
	tx.AddTxOut(wire.NewTxOut(amount, depositScript))

	change := utxo.amount - amount - fee
	if change > 546 {
		changeScript, err := txscript.PayToAddrScript(changeAddr)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "build change script failed: %v\n", err)
			return
		}
		tx.AddTxOut(wire.NewTxOut(change, changeScript))
	}

	class := txscript.GetScriptClass(utxo.pkScript)
	switch class {
	case txscript.PubKeyHashTy:
		sigScript, err := txscript.SignatureScript(tx, 0, utxo.pkScript, txscript.SigHashAll, wif.PrivKey, true)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "sign p2pkh failed: %v\n", err)
			return
		}
		tx.TxIn[0].SignatureScript = sigScript
	default:
		_, _ = fmt.Fprintf(os.Stderr, "unsupported utxo script class: %s, only p2pkh supported now\n", class.String())
		return
	}

	connCfg := &rpcclient.ConnConfig{
		Host:         rpcHost,
		User:         rpcUser,
		Pass:         rpcPass,
		HTTPPostMode: true,
		DisableTLS:   disableTLS,
	}
	if !disableTLS && strings.TrimSpace(rpcCertFile) != "" {
		certs, err := os.ReadFile(strings.TrimSpace(rpcCertFile))
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "read rpc cert file failed: %v\n", err)
			return
		}
		connCfg.Certificates = certs
	}
	rpcCli, err := rpcclient.New(connCfg, nil)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "create rpc client failed: %v\n", err)
		return
	}
	defer rpcCli.Shutdown()

	txHash, err := rpcCli.SendRawTransaction(tx, false)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "broadcast tx failed: %v\n", err)
		return
	}
	fmt.Println(txHash.String())
}

func btcMintSpendCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "btcMintSpend",
		Short: "build, sign and broadcast btc mint spending tx with OP_RETURN",
		Run:   btcMintSpend,
		Example: "btcMintSpend --net regtest --rpcHost 127.0.0.1:18443 " +
			"--wif <wif> --utxo <txid:vout:amountSats:pkScriptHex> " +
			"--destAddress <btcAddr> --opReturnData <mintTxHashHex> --fee 1000",
	}
	cmd.Flags().String("net", "regtest", "bitcoin network: mainnet|testnet|regtest|simnet")
	cmd.Flags().String("rpcHost", "127.0.0.1:18443", "bitcoin rpc host")
	cmd.Flags().String("rpcUser", "", "bitcoin rpc user (optional)")
	cmd.Flags().String("rpcPass", "", "bitcoin rpc password (optional)")
	cmd.Flags().Bool("disableTLS", true, "disable rpc tls")
	cmd.Flags().String("rpcCertFile", "", "bitcoin rpc cert file path (optional, required when TLS enabled)")
	cmd.Flags().String("wif", "", "sender private key in WIF format")
	cmd.Flags().String("utxo", "", "single input utxo, format: txid:vout:amountSats:pkScriptHex")
	cmd.Flags().String("destAddress", "", "btc destination address for change output")
	cmd.Flags().String("opReturnData", "", "hex data for OP_RETURN output (chain33 mint tx hash)")
	cmd.Flags().Int64("fee", 0, "tx fee in satoshis")
	markRequired(cmd, "wif", "utxo", "destAddress", "opReturnData", "fee")
	return cmd
}

func btcMintSpend(cmd *cobra.Command, _ []string) {
	netName, _ := cmd.Flags().GetString("net")
	rpcHost, _ := cmd.Flags().GetString("rpcHost")
	rpcUser, _ := cmd.Flags().GetString("rpcUser")
	rpcPass, _ := cmd.Flags().GetString("rpcPass")
	disableTLS, _ := cmd.Flags().GetBool("disableTLS")
	rpcCertFile, _ := cmd.Flags().GetString("rpcCertFile")
	wifStr, _ := cmd.Flags().GetString("wif")
	utxoRaw, _ := cmd.Flags().GetString("utxo")
	destAddrStr, _ := cmd.Flags().GetString("destAddress")
	opReturnDataHex, _ := cmd.Flags().GetString("opReturnData")
	fee, _ := cmd.Flags().GetInt64("fee")

	params, err := parseNetParams(netName)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "invalid net: %s, err: %v\n", netName, err)
		return
	}
	if fee < 0 {
		_, _ = fmt.Fprintf(os.Stderr, "invalid fee(%d)\n", fee)
		return
	}
	utxo, err := parseDepositUTXO(utxoRaw)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "invalid utxo: %v\n", err)
		return
	}
	if utxo.amount <= fee {
		_, _ = fmt.Fprintf(os.Stderr, "insufficient utxo amount, have=%d need>%d\n", utxo.amount, fee)
		return
	}

	wif, err := btcutil.DecodeWIF(strings.TrimSpace(wifStr))
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "invalid wif: %v\n", err)
		return
	}
	destAddr, err := btcutil.DecodeAddress(strings.TrimSpace(destAddrStr), params)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "invalid destAddress: %v\n", err)
		return
	}

	opReturnData, err := hex.DecodeString(strings.TrimSpace(opReturnDataHex))
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "invalid opReturnData hex: %v\n", err)
		return
	}

	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(wire.NewTxIn(wire.NewOutPoint(utxo.hash, utxo.vout), nil, nil))

	// Add OP_RETURN output (must be first output, index 0 for neutrino to detect)
	opScript, err := txscript.NullDataScript(opReturnData)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "build op_return failed: %v\n", err)
		return
	}
	tx.AddTxOut(wire.NewTxOut(0, opScript))

	// Add destination output (change to mining address)
	destScript, err := txscript.PayToAddrScript(destAddr)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "build dest script failed: %v\n", err)
		return
	}
	change := utxo.amount - fee
	tx.AddTxOut(wire.NewTxOut(change, destScript))

	// Sign the P2PKH input
	sigScript, err := txscript.SignatureScript(tx, 0, utxo.pkScript, txscript.SigHashAll, wif.PrivKey, true)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "sign p2pkh failed: %v\n", err)
		return
	}
	tx.TxIn[0].SignatureScript = sigScript

	// Connect to btcd RPC and broadcast
	connCfg := &rpcclient.ConnConfig{
		Host:         rpcHost,
		User:         rpcUser,
		Pass:         rpcPass,
		HTTPPostMode: true,
		DisableTLS:   disableTLS,
	}
	if !disableTLS && strings.TrimSpace(rpcCertFile) != "" {
		certs, err := os.ReadFile(strings.TrimSpace(rpcCertFile))
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "read rpc cert file failed: %v\n", err)
			return
		}
		connCfg.Certificates = certs
	}
	rpcCli, err := rpcclient.New(connCfg, nil)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "create rpc client failed: %v\n", err)
		return
	}
	defer rpcCli.Shutdown()

	txHash, err := rpcCli.SendRawTransaction(tx, false)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "broadcast tx failed: %v\n", err)
		return
	}
	fmt.Println(txHash.String())
}

func btcKeyInfoCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "btcKeyInfo",
		Short:   "derive btc key info from private key hex",
		Run:     btcKeyInfo,
		Example: "btcKeyInfo --net regtest --privHex 0000000000000000000000000000000000000000000000000000000000000001",
	}
	cmd.Flags().String("net", "regtest", "bitcoin network: mainnet|testnet|regtest|simnet")
	cmd.Flags().String("privHex", "", "private key hex (32 bytes)")
	markRequired(cmd, "privHex")
	return cmd
}

func btcKeyInfo(cmd *cobra.Command, _ []string) {
	netName, _ := cmd.Flags().GetString("net")
	privHex, _ := cmd.Flags().GetString("privHex")

	params, err := parseNetParams(netName)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "invalid net: %s, err: %v\n", netName, err)
		return
	}
	keyBytes, err := decodeHexAuto(privHex)
	if err != nil || len(keyBytes) != 32 {
		_, _ = fmt.Fprintf(os.Stderr, "invalid privHex, require 32-byte hex\n")
		return
	}
	privKey, _ := btcec.PrivKeyFromBytes(keyBytes)
	wif, err := btcutil.NewWIF(privKey, params, true)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "create wif failed: %v\n", err)
		return
	}
	addr, err := btcutil.NewAddressPubKeyHash(
		btcutil.Hash160(privKey.PubKey().SerializeCompressed()), params,
	)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "derive address failed: %v\n", err)
		return
	}
	script, err := txscript.PayToAddrScript(addr)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "derive pkScript failed: %v\n", err)
		return
	}

	fmt.Printf("{\"wif\":\"%s\",\"address\":\"%s\",\"pkScript\":\"%s\"}\n",
		wif.String(), addr.String(), hex.EncodeToString(script))
}

func parseNetParams(net string) (*chaincfg.Params, error) {
	switch strings.ToLower(strings.TrimSpace(net)) {
	case "mainnet", "main":
		return &chaincfg.MainNetParams, nil
	case "testnet", "testnet3", "test":
		return &chaincfg.TestNet3Params, nil
	case "regtest":
		return &chaincfg.RegressionNetParams, nil
	case "simnet":
		return &chaincfg.SimNetParams, nil
	default:
		return nil, fmt.Errorf("unsupported net %q", net)
	}
}

func commitDKGCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "commitDKG",
		Aliases: []string{"cdkg"},
		Short:   "commit dkg address for cross-chain asset",
		Run:     commitDKG,
		Example: "commitDKG -s BTC -d <dkgAddress> -p <pkScriptHex> -k <tssPubkeyHex>",
	}
	commitDKGFlags(cmd)
	return cmd
}

func commitDKGFlags(cmd *cobra.Command) {
	cmd.Flags().StringP("assetSymbol", "s", rtypes.BTCSymbol, "cross-chain asset symbol")
	cmd.Flags().StringP("dkgAddress", "d", "", "dkg/tss bitcoin address")
	cmd.Flags().StringP("pkScript", "p", "", "dkg address pkScript hex (from rgbx getCrossChainInfo); an optional 0x prefix is accepted")
	// pubkey 必填：checkCommitDKG 对所有 symbol（含 BTC/XBTC）都要求 33 字节压缩 TSS 群公钥，
	// 并校验 hash160(pubkey)==pkScript[2:]。缺了它链上必以 ErrInvalidDkgAddress 拒收
	// （P2WSH 充值地址 = f(userID, pubkey)，执行器要靠它重建充值脚本），所以这里直接
	// 在 CLI 侧挡住，别让用户提交一份注定被拒的交易。
	cmd.Flags().StringP("pubkey", "k", "", "tss group pubkey, 33-byte compressed hex (from rgbx getCrossChainInfo); an optional 0x prefix is accepted")
	markRequired(cmd, "dkgAddress", "pkScript", "pubkey")
}

func commitDKG(cmd *cobra.Command, _ []string) {
	symbol, _ := cmd.Flags().GetString("assetSymbol")
	dkgAddress, _ := cmd.Flags().GetString("dkgAddress")
	pkScriptHex, _ := cmd.Flags().GetString("pkScript")
	pubkeyHex, _ := cmd.Flags().GetString("pubkey")

	pkScript, err := decodeHexAuto(pkScriptHex)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "invalid pkScript: %s, decode err: %v\n", pkScriptHex, err)
		return
	}
	pubkey, err := decodeHexAuto(pubkeyHex)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "invalid pubkey: %s, decode err: %v\n", pubkeyHex, err)
		return
	}
	// 与链上 checkCommitDKG 同一把尺子（只接受 33 字节压缩格式，非压缩的同一把钥会派生出
	// 另一个充值地址），并核对 hash160(pubkey) == pkScript[2:] —— 两者不配套说明地址与公钥
	// 不是同一个 DKG 结果，链上会拒。
	normalized, err := rtypes.ParseDepositTssPubKey(pubkey)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "invalid pubkey, require 33-byte compressed secp256k1 hex: %v\n", err)
		return
	}
	if len(pkScript) != 22 || pkScript[0] != txscript.OP_0 || pkScript[1] != 0x14 ||
		!bytes.Equal(btcutil.Hash160(normalized), pkScript[2:]) {
		_, _ = fmt.Fprintf(os.Stderr, "pubkey does not match pkScript: hash160(pubkey)=%s, pkScript=%s\n",
			hex.EncodeToString(btcutil.Hash160(normalized)), pkScriptHex)
		return
	}

	sendCreateTxRPC(cmd, rtypes.NameCommitDKGAction, &rtypes.CommitDKG{
		AssetSymbol: symbol,
		DkgAddress:  dkgAddress,
		PkScript:    pkScript,
		Pubkey:      normalized,
	})
}

func depositAssetCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "deposit",
		Aliases: []string{"dep"},
		Short:   "submit btc deposit proof",
		Run:     depositAsset,
		Example: "deposit -a 1000 -d <chain33Addr> --txData <btcTxHex> --blockHeight 1 --txIndex 0 --blockHash <hash>",
	}
	depositAssetFlags(cmd)
	return cmd
}

func depositAssetFlags(cmd *cobra.Command) {
	cmd.Flags().Int64P("amount", "a", 0, "deposit amount")
	cmd.Flags().StringP("depositAddress", "d", "", "target chain33 address")
	cmd.Flags().StringP("assetSymbol", "s", rtypes.BTCSymbol, "cross-chain asset symbol")
	cmd.Flags().Uint64("blockHeight", 0, "btc proof block height")
	cmd.Flags().Uint32("txIndex", 0, "btc tx index in block")
	cmd.Flags().String("blockHash", "", "btc proof block hash")
	cmd.Flags().String("txData", "", "btc raw transaction hex")
	cmd.Flags().String("merkleProof", "", "comma-separated merkle proof hex list")
	markRequired(cmd, "amount", "depositAddress", "blockHeight", "txIndex", "blockHash", "txData")
}

func depositAsset(cmd *cobra.Command, _ []string) {
	amount, _ := cmd.Flags().GetInt64("amount")
	depositAddress, _ := cmd.Flags().GetString("depositAddress")
	symbol, _ := cmd.Flags().GetString("assetSymbol")
	blockHeight, _ := cmd.Flags().GetUint64("blockHeight")
	txIndex, _ := cmd.Flags().GetUint32("txIndex")
	blockHash, _ := cmd.Flags().GetString("blockHash")
	txDataHex, _ := cmd.Flags().GetString("txData")
	merkleProofStr, _ := cmd.Flags().GetString("merkleProof")

	txData, err := decodeHexAuto(txDataHex)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "invalid txData: %s, decode err: %v\n", txDataHex, err)
		return
	}
	merkleProof, err := decodeHexList(merkleProofStr)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "invalid merkleProof: %s, decode err: %v\n", merkleProofStr, err)
		return
	}

	sendCreateTxRPC(cmd, rtypes.NameDepositAssetAction, &rtypes.DepositAsset{
		Amount:         amount,
		DepositAddress: depositAddress,
		AssetSymbol:    symbol,
		TxProof: &rtypes.BtcTxProof{
			BlockHeight: blockHeight,
			TxIndex:     txIndex,
			BlockHash:   blockHash,
			TxData:      txData,
			MerkleProof: merkleProof,
		},
	})
}

func withdrawAssetCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "withdraw",
		Aliases: []string{"wd"},
		Short:   "withdraw cross-chain asset to btc address",
		Run:     withdrawAsset,
		Example: "withdraw -a 1000 -f 10 -d <btcAddress> -s BTC",
	}
	withdrawAssetFlags(cmd)
	return cmd
}

func withdrawAssetFlags(cmd *cobra.Command) {
	// amount 用字符串接收并按十进制精确换算：float64 无法精确表示 0.0006 这类小数，
	// 相乘再截断会少 1 个最小单位（详见 parseDecimalAmount）。
	cmd.Flags().StringP("amount", "a", "0", "withdraw amount")
	cmd.Flags().Int64P("feeRate", "f", 1, "btc fee rate (sat/vbyte)")
	cmd.Flags().StringP("destinationAddr", "d", "", "btc destination address")
	cmd.Flags().StringP("assetSymbol", "s", rtypes.BTCSymbol, "cross-chain asset symbol")
	markRequired(cmd, "amount", "feeRate", "destinationAddr")
}

func withdrawAsset(cmd *cobra.Command, _ []string) {
	amountStr, _ := cmd.Flags().GetString("amount")
	feeRate, _ := cmd.Flags().GetInt64("feeRate")
	destAddr, _ := cmd.Flags().GetString("destinationAddr")
	symbol, _ := cmd.Flags().GetString("assetSymbol")

	amount, err := parseDecimalAmount(amountStr, withdrawAmountDecimals)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "%v\n", err)
		return
	}
	if amount < 1 || amount > rtypes.MaxAssetAmount {
		_, _ = fmt.Fprintf(os.Stderr, "invalid amount: %q, must be in [1, %d] min units\n",
			amountStr, int64(rtypes.MaxAssetAmount))
		return
	}

	sendCreateTxRPC(cmd, rtypes.NameWithdrawAssetAction, &rtypes.WithdrawAsset{
		Amount:          amount,
		FeeRate:         feeRate,
		DestinationAddr: destAddr,
		AssetSymbol:     symbol,
	})
}

func confirmTxCMD() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "confirm",
		Aliases: []string{"cf"},
		Short:   "confirm pending rgbx tx",
		Run:     confirmTx,
		Example: "confirm --actionType 106 --txBlockHeight 100 --txIndex 1 --txHash <hashHex>",
	}
	confirmTxFlags(cmd)
	return cmd
}

func confirmTxFlags(cmd *cobra.Command) {
	cmd.Flags().Int32("actionType", 0, "pending action type")
	cmd.Flags().Int64("confirmedBlockHeight", 0, "confirmed btc block height")
	cmd.Flags().Int64("txBlockHeight", 0, "pending tx block height")
	cmd.Flags().Int64("txIndex", 0, "pending tx index")
	cmd.Flags().String("txHash", "", "pending tx hash hex")
	cmd.Flags().Bool("timeout", false, "whether confirm by timeout")

	cmd.Flags().Uint32("spendingInputIdx", 0, "btc spending tx input index")
	cmd.Flags().Int32("opRetOutputIdx", -1, "btc op_return output index")

	cmd.Flags().Uint64("btcBlockHeight", 0, "btc proof block height")
	cmd.Flags().Uint32("btcTxIndex", 0, "btc proof tx index")
	cmd.Flags().String("btcBlockHash", "", "btc proof block hash")
	cmd.Flags().String("btcTxData", "", "btc proof tx raw hex")
	cmd.Flags().String("btcMerkleProof", "", "comma-separated merkle proof hex list")
	markRequired(cmd, "actionType", "txBlockHeight", "txIndex", "txHash")
}

func confirmTx(cmd *cobra.Command, _ []string) {
	actionType, _ := cmd.Flags().GetInt32("actionType")
	confirmedHeight, _ := cmd.Flags().GetInt64("confirmedBlockHeight")
	txBlockHeight, _ := cmd.Flags().GetInt64("txBlockHeight")
	txIndex, _ := cmd.Flags().GetInt64("txIndex")
	txHashHex, _ := cmd.Flags().GetString("txHash")
	timeout, _ := cmd.Flags().GetBool("timeout")

	spendingInputIdx, _ := cmd.Flags().GetUint32("spendingInputIdx")
	opRetOutputIdx, _ := cmd.Flags().GetInt32("opRetOutputIdx")

	btcBlockHeight, _ := cmd.Flags().GetUint64("btcBlockHeight")
	btcTxIndex, _ := cmd.Flags().GetUint32("btcTxIndex")
	btcBlockHash, _ := cmd.Flags().GetString("btcBlockHash")
	btcTxDataHex, _ := cmd.Flags().GetString("btcTxData")
	btcMerkleProofStr, _ := cmd.Flags().GetString("btcMerkleProof")

	txHash, err := decodeHexAuto(txHashHex)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "invalid txHash: %s, decode err: %v\n", txHashHex, err)
		return
	}

	btcTxData, err := decodeHexOptional(btcTxDataHex)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "invalid btcTxData: %s, decode err: %v\n", btcTxDataHex, err)
		return
	}
	btcMerkleProof, err := decodeHexList(btcMerkleProofStr)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "invalid btcMerkleProof: %s, decode err: %v\n", btcMerkleProofStr, err)
		return
	}

	sendCreateTxRPC(cmd, rtypes.NameConfirmAction, &rtypes.ConfirmTx{
		ActionType:           actionType,
		ConfirmedBlockHeight: confirmedHeight,
		TxBlockHeight:        txBlockHeight,
		TxIndex:              txIndex,
		TxHash:               txHash,
		Timeout:              timeout,
		UtxoProof: &rtypes.UtxoSpendingProof{
			SpendingInputIdx: spendingInputIdx,
			OpRetOutputIdx:   opRetOutputIdx,
		},
		BtcTxProof: &rtypes.BtcTxProof{
			BlockHeight: btcBlockHeight,
			TxIndex:     btcTxIndex,
			BlockHash:   btcBlockHash,
			TxData:      btcTxData,
			MerkleProof: btcMerkleProof,
		},
	})
}

// stripHexPrefix 去掉首尾空白与可选的 "0x"/"0X" 前缀（大小写不敏感）。
func stripHexPrefix(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		return s[2:]
	}
	return s
}

// decodeHexAuto 解码用户提供的 hex 串：容忍可选的 0x/0X 前缀、首尾空白与大小写。
//
// 为什么需要它：本包多处 flag 的帮助文本就指向 `rgbx getCrossChainInfo` 的输出，而该输出
// 的 pubkey/pkScript 带 0x 前缀（如 "0x03ecaf..."）。旧实现直接把原串丢给 hex.DecodeString，
// 遇到 0x 会以 "encoding/hex: invalid byte: U+0078 'x'" 失败 —— P2WSH 充值场景因此连交易都
// 构造不出来（btcDepositTx --tssPubkey 取的就是这个字段）。容错放在 CLI 侧治本，脚本里 sed
// 掉前缀只是治标。
//
// 奇数长度单独报错：hex 的 "odd length hex string" 不带长度，定位时不够直白。
func decodeHexAuto(raw string) ([]byte, error) {
	s := stripHexPrefix(raw)
	if len(s)%2 != 0 {
		return nil, fmt.Errorf("odd-length hex string: %d chars", len(s))
	}
	return hex.DecodeString(s)
}

func decodeHexOptional(s string) ([]byte, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	return decodeHexAuto(s)
}

func decodeHexList(raw string) ([][]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	result := make([][]byte, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		b, err := decodeHexAuto(part)
		if err != nil {
			return nil, err
		}
		result = append(result, b)
	}
	return result, nil
}
