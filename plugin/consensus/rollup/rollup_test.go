package rollup

import (
	"encoding/hex"
	"testing"

	_ "github.com/33cn/chain33/system/dapp/init"
	"github.com/33cn/chain33/types"
	"github.com/33cn/chain33/util"
	"github.com/33cn/plugin/plugin/crypto/bls"
)

func TestRollup(t *testing.T) {

	cfg := types.NewChain33Config(types.GetDefaultCfgstring())

	blsDrv := &bls.Driver{}

	//
	//for i:=0;i<100;i++{
	priv, _ := blsDrv.GenKey()

	println("blsPriv", hex.EncodeToString(priv.Bytes()))
	println("blsPub", hex.EncodeToString(priv.PubKey().Bytes()))
	addr, priv1 := util.Genaddress()

	println("secpPriv", hex.EncodeToString(priv1.Bytes()), addr)

	//pk := priv.PubKey()
	_, priv = util.Genaddress()
	tx := util.CreateCoinsTx(cfg, priv, addr, 1)

	//}
	sign := tx.GetSignature()
	tx.Signature = nil
	println("txsize", tx.Size(), len(sign.Pubkey), len(sign.GetSignature()))

}
