// rgb-deposit-sign produces a signed deposit request for the E2E harness.
//
// It exists so the shell harness can play the user: the request body posted to the gateway must
// be signed with the user's chain33 key, exactly as the gateway and the chain verify it. It is a
// test tool; production key management belongs to the wallet/product backend.
package main

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/33cn/chain33/common/address"
	_ "github.com/33cn/chain33/system/address/btc"
	"github.com/33cn/plugin/plugin/dapp/rgbx/depositrequest"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
)

type output struct {
	RequestID   string `json:"request_id"`
	Chain33Addr string `json:"chain33_addr"`
	AssetSymbol string `json:"asset_symbol"`
	Amount      int64  `json:"amount"`
	Timestamp   int64  `json:"timestamp"`
	PublicKey   string `json:"pubkey"`
	Signature   string `json:"signature"`
}

func main() {
	privHex := flag.String("priv", "", "chain33 private key (hex, 0x tolerated)")
	chainTitle := flag.String("chain-title", "", "chain title the request is signed for")
	requestID := flag.String("request-id", "", "request id (unique per request)")
	symbol := flag.String("symbol", "", "asset symbol, e.g. RGB20_USDT")
	amount := flag.Int64("amount", 0, "amount in asset min units")
	timestamp := flag.Int64("timestamp", 0, "unix timestamp (0 = now)")
	flag.Parse()

	privBytes, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(*privHex), "0x"))
	if err != nil || len(privBytes) != 32 {
		fail("-priv must be a 32-byte hex private key")
	}
	if *chainTitle == "" || *requestID == "" || *symbol == "" || *amount <= 0 {
		fail("-chain-title, -request-id, -symbol and -amount are required")
	}
	ts := *timestamp
	if ts == 0 {
		ts = time.Now().Unix()
	}
	priv, _ := btcec.PrivKeyFromBytes(privBytes)
	pub := priv.PubKey().SerializeCompressed()
	req := depositrequest.Request{
		RequestID:   *requestID,
		Chain33Addr: address.PubKeyToAddr(-1, pub),
		AssetSymbol: *symbol,
		Amount:      *amount,
		Timestamp:   ts,
	}
	if err := req.Validate(); err != nil {
		fail(err.Error())
	}
	sig := ecdsa.Sign(priv, depositrequest.MessageHash(*chainTitle, req)).Serialize()
	enc := json.NewEncoder(os.Stdout)
	if err := enc.Encode(output{
		RequestID:   req.RequestID,
		Chain33Addr: req.Chain33Addr,
		AssetSymbol: req.AssetSymbol,
		Amount:      req.Amount,
		Timestamp:   req.Timestamp,
		PublicKey:   hex.EncodeToString(pub),
		Signature:   hex.EncodeToString(sig),
	}); err != nil {
		fail(err.Error())
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "rgb-deposit-sign:", msg)
	os.Exit(2)
}
