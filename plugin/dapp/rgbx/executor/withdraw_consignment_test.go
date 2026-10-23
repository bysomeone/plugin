package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/33cn/chain33/client/mocks"
	"github.com/33cn/chain33/types"
	"github.com/33cn/chain33/util"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/stretchr/testify/require"
)

// Test_checkWithdrawConfirm_consignmentHashValidation covers the two structural rules the chain
// enforces on ConfirmTx.consignmentHash: it is either empty or a 32-byte sha256, and it is only
// meaningful for RGB20 assets.
func Test_checkWithdrawConfirm_consignmentHashValidation(t *testing.T) {
	f := newB8Fixture(t)

	// 长度既不是 0 也不是 32：直接拒，不需要任何证明/台账（判据在函数最前面）。
	badLen := &rtypes.ConfirmTx{TxHash: []byte("burn"), ConsignmentHash: make([]byte, 31)}
	require.Equal(t, ErrInvalidConsignmentHash,
		f.r.checkWithdrawConfirm("tx", "confirm", badLen, &rtypes.PendingTx{AssetSymbol: "btc"}))

	// BTC 类提现带 32 字节哈希：即使证明有效也必须拒（BTC 放款没有 consignment）。
	confirm := &rtypes.ConfirmTx{
		TxHash:          []byte("burn-btc"),
		BtcTxProof:      f.deposit(f.raw).TxProof,
		ConsignmentHash: make([]byte, rtypes.ConsignmentHashLen),
	}
	pending := &rtypes.PendingTx{AssetSymbol: "btc", Amount: f.amount, TargetAddress: f.depositAddr}
	require.Equal(t, ErrConsignmentHashNotAllowed,
		f.r.checkWithdrawConfirm("tx", "confirm", confirm, pending))
}

// Test_confirmWithdrawSettlement_consignmentHash covers the RGB20 branch end to end at the
// settlement layer: the hash is stored under the burn identity, the receipt carries the log,
// the query returns it, and a second Confirm is rejected by the existing terminal-state key.
func Test_confirmWithdrawSettlement_consignmentHash(t *testing.T) {
	r := newRgbx()
	burn := []byte("burn-rgb20-consignment")

	dir, state, _ := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	api := &mocks.QueueProtocolAPI{}
	r.SetAPI(api)
	api.On("GetConfig").Return(types.NewChain33Config(types.GetDefaultCfgstring()))
	r.SetStateDB(state)

	// 链上登记为 RGB20：Exec 侧据此允许登记 consignment 哈希。
	setCrossChainInfoForTest(t, state, rtypes.RGB20USDTSymbol, rtypes.CrossChainAssetRgb20,
		rtypes.MaxPrecision, []byte{0x02})
	require.NoError(t, state.Set(formatPayloadKey(burn),
		types.Encode(&rtypes.WithdrawAsset{AssetSymbol: rtypes.RGB20USDTSymbol, Amount: 100})))
	recordBurnForTest(t, r.(*rgbx), state, rtypes.RGB20USDTSymbol, 100, burn)
	mintForTest(t, r.(*rgbx), state, rtypes.RGB20USDTSymbol, "depositor", 100, "consignment")
	acc, err := r.(*rgbx).newAccount(ensureCrossChainSymbol(rtypes.RGB20USDTSymbol))
	require.NoError(t, err)
	lockAddr := r.(*rgbx).crossChainLockAddress(acc)
	_, err = acc.Mint(lockAddr, 100)
	require.NoError(t, err)

	consignment := []byte("raw-consignment-bytes")
	hash := sha256.Sum256(consignment)
	recp, err := r.(*rgbx).confirmWithdrawSettlement(&rtypes.ConfirmTx{
		TxHash:          burn,
		ConsignmentHash: hash[:],
	}, "tx", "confirm")
	require.NoError(t, err)
	require.NotNil(t, recp)
	applyStateKV(t, state, recp.KV)

	// 1) 共识状态里按 burn 身份登记了这 32 字节。
	stored, err := state.Get(formatWithdrawConsignmentKey(burn))
	require.NoError(t, err)
	require.Equal(t, hash[:], stored)

	// 2) 回执带一条新日志（追加在末尾的 TyWithdrawConsignmentLog），内容是 burn ↔ hash。
	var found bool
	for _, log := range recp.Logs {
		if log.Ty != rtypes.TyWithdrawConsignmentLog {
			continue
		}
		found = true
		msg := &rtypes.WithdrawConsignment{}
		require.NoError(t, types.Decode(log.Log, msg))
		require.Equal(t, burn, msg.GetBurnTxHash())
		require.Equal(t, hash[:], msg.GetConsignmentHash())
	}
	require.True(t, found, "receipt must carry a TyWithdrawConsignmentLog")

	// 3) 查询按 hex 返回哈希；未登记的 burn 返回 ErrNotFound。
	reply, err := r.(*rgbx).Query_GetWithdrawConsignmentHash(&types.ReqString{Data: hex.EncodeToString(burn)})
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(hash[:]), reply.(*types.ReplyString).GetData())
	_, err = r.(*rgbx).Query_GetWithdrawConsignmentHash(&types.ReqString{Data: "0x" + hex.EncodeToString(burn)})
	require.NoError(t, err, "0x prefix is tolerated")
	_, err = r.(*rgbx).Query_GetWithdrawConsignmentHash(&types.ReqString{Data: "deadbeef"})
	require.ErrorIs(t, err, types.ErrNotFound)

	// 4) 重复确认：终态键（S3）先挡下，哈希字段不会让同一笔 burn 二次结算。
	err = r.(*rgbx).checkWithdrawConfirm("tx2", "confirm2", &rtypes.ConfirmTx{
		TxHash:          burn,
		ConsignmentHash: hash[:],
	}, &rtypes.PendingTx{AssetSymbol: rtypes.RGB20USDTSymbol})
	require.Equal(t, ErrWithdrawAlreadyConfirmed, err)

	// 5) Exec 侧的纵深防御：BTC 类资产即使带了哈希也不落库。
	btcBurn := []byte("burn-btc-consignment")
	setCrossChainInfoForTest(t, state, "BTC", rtypes.CrossChainAssetBtc,
		rtypes.BTCChainPrecision, []byte{0x02})
	require.NoError(t, state.Set(formatPayloadKey(btcBurn),
		types.Encode(&rtypes.WithdrawAsset{AssetSymbol: "BTC", Amount: 100})))
	recordBurnForTest(t, r.(*rgbx), state, "BTC", 100, btcBurn)
	mintForTest(t, r.(*rgbx), state, "BTC", "depositor", 100, "btc-consignment")
	btcAcc, err := r.(*rgbx).newAccount(ensureCrossChainSymbol("BTC"))
	require.NoError(t, err)
	btcLock := r.(*rgbx).crossChainLockAddress(btcAcc)
	_, err = btcAcc.Mint(btcLock, 100)
	require.NoError(t, err)
	_, err = r.(*rgbx).confirmWithdrawSettlement(&rtypes.ConfirmTx{
		TxHash:          btcBurn,
		ConsignmentHash: hash[:],
	}, "tx3", "confirm3")
	require.Equal(t, ErrConsignmentHashNotAllowed, err)
}
