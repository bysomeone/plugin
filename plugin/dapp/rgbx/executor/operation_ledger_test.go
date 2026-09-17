package executor

import (
	"bytes"
	"testing"

	"github.com/33cn/chain33/client/mocks"
	"github.com/33cn/chain33/common/db"
	"github.com/33cn/chain33/types"
	"github.com/33cn/chain33/util"
	ltypes "github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

/*
 * 本文件覆盖 S2（全局 operationId + proof-of-mint 台账）。四组断言：
 *
 *  ① 不可变/不可重复入账：同 operationId 二次写入被拒、已写入的值不被改写；
 *     金额进原像 ⇒ "改金额"等于换一笔操作（换 id），不存在"改记录"的入口。
 *  ② 提现判据：没登记过 / 金额不一致 / 没铸造过（供应量不足）⇒ 一律拒绝；
 *  ③ 正常路径：真充值（Exec_Deposit）→ 真提现锁定（Exec_Withdraw）→ 结算 → 放行，
 *     且 minted/burned 两个计数按同一笔金额对称变动；
 *  ④ 查询/审计：GetOperation（含 settled 补算）、GetOperationSupply、ListOperations（分页）。
 */

// operationLedgerFixture 台账测试的最小环境：statedb + 执行器。
type operationLedgerFixture struct {
	r     *rgbx
	state db.DB
}

func newOperationLedgerFixture(t *testing.T) *operationLedgerFixture {
	t.Helper()
	f := &operationLedgerFixture{}
	dir, state, _ := util.CreateTestDB()
	t.Cleanup(func() { util.CloseTestDB(dir, state) })
	f.state = state
	api := &mocks.QueueProtocolAPI{}
	api.On("GetConfig").Return(types.NewChain33Config(types.GetDefaultCfgstring()))
	f.r = newRgbx().(*rgbx)
	f.r.SetAPI(api)
	f.r.SetStateDB(state)
	return f
}

// canonicalBtcTxData 构造一份**规范编码**的 BTC 交易字节（Exec_Deposit 的严格解析要求）。
// Exec 层不校验 merkle/头链/脚本（那是 CheckTx 的事），所以这里只要编码合法即可。
func canonicalBtcTxData(t *testing.T, seed string, value int64) []byte {
	t.Helper()
	tx := &wire.MsgTx{Version: 2}
	tx.TxIn = append(tx.TxIn,
		wire.NewTxIn(&wire.OutPoint{Hash: chainhash.DoubleHashH([]byte(seed)), Index: 0}, nil, nil))
	tx.TxOut = append(tx.TxOut, wire.NewTxOut(value, []byte{0x51}))
	buf := bytes.NewBuffer(make([]byte, 0, tx.SerializeSizeStripped()))
	require.NoError(t, tx.SerializeNoWitness(buf))
	return buf.Bytes()
}

// mintForTest 走**生产写入口**（recordMintOperation）登记一笔铸造并提交到 statedb，返回记录。
// 记录与供应量成对写入（生产路径也是如此），所以用它铺前置状态的用例不会造出"有额度没记录"的假状态。
func mintForTest(t *testing.T, r *rgbx, state db.DB, symbol, owner string, amount int64, seed string) *rtypes.OperationRecord {
	t.Helper()
	symbol = ensureCrossChainSymbol(symbol)
	txID := chainhash.DoubleHashH([]byte("btc-tx-" + seed))
	chain33TxHash := chainhash.DoubleHashH([]byte("chain33-tx-" + seed))
	opID, err := rtypes.MintOperationID(symbol, txID.CloneBytes(), owner, amount)
	require.NoError(t, err)
	kvs, err := r.recordMintOperation(opID, txID.CloneBytes(), symbol, owner, amount, chain33TxHash.CloneBytes())
	require.NoError(t, err)
	applyStateKV(t, state, kvs)
	rec := &rtypes.OperationRecord{}
	require.NoError(t, readDB(state, formatOperationRecordKey(opID), rec))
	return rec
}

// mintForTestF 同 mintForTest，只是接 fixture。
func mintForTestF(t *testing.T, f *operationLedgerFixture, symbol, owner string, amount int64, seed string) *rtypes.OperationRecord {
	t.Helper()
	return mintForTest(t, f.r, f.state, symbol, owner, amount, seed)
}

// recordBurnForTest 走**生产写入口**（recordBurnOperation）登记一笔提现锁定并提交到 statedb，
// 返回 burn 的 operationId。链上唯一的生产调用点是 Exec_Withdraw；直接调 exec/CheckTx 函数的
// 用例用它补上前置状态（S2 起"没有 burn 记录就不许结算"）。
func recordBurnForTest(t *testing.T, r *rgbx, state db.DB, symbol string, amount int64, burnTxHash []byte) []byte {
	t.Helper()
	symbol = ensureCrossChainSymbol(symbol)
	opID, err := rtypes.BurnOperationID(symbol, burnTxHash)
	require.NoError(t, err)
	kvs, err := r.recordBurnOperation(opID, symbol, "burn-owner", amount, burnTxHash)
	require.NoError(t, err)
	applyStateKV(t, state, kvs)
	return opID
}

// burnForTest 同 recordBurnForTest，只是接 fixture。
func burnForTest(t *testing.T, f *operationLedgerFixture, symbol, owner string, amount int64, burnTxHash []byte) []byte {
	t.Helper()
	symbol = ensureCrossChainSymbol(symbol)
	opID, err := rtypes.BurnOperationID(symbol, burnTxHash)
	require.NoError(t, err)
	kvs, err := f.r.recordBurnOperation(opID, symbol, owner, amount, burnTxHash)
	require.NoError(t, err)
	applyStateKV(t, f.state, kvs)
	return opID
}

func supplyForTest(t *testing.T, f *operationLedgerFixture, symbol string) *rtypes.OperationSupply {
	t.Helper()
	supply, err := f.r.loadOperationSupply(ensureCrossChainSymbol(symbol))
	require.NoError(t, err)
	return supply
}

// ---------------------------------------------------------------------------
// ① 不可变 / 不可重复入账
// ---------------------------------------------------------------------------

// Test_OperationLedger_mintRecordIsWriteOnce 同一 operationId 不能重复入账、更不能被改写。
// 反向验证：去掉 newOperationRecordKV 里的写一次校验（直接返回 KV）→ 本用例变红。
func Test_OperationLedger_mintRecordIsWriteOnce(t *testing.T) {
	f := newOperationLedgerFixture(t)
	rec := mintForTestF(t, f, "btc", "user-addr", 1000, "once")

	// 用**同一 operationId**再写一次（模拟"重复入账"），内容刻意改成另一个金额：
	// 生产路径上 opID 是派生出来的，构造不出"同 id 不同金额"的记录 —— 这里直接调底层写入口，
	// 考的就是那道校验本身。
	tampered := &rtypes.OperationRecord{
		OperationId:   rec.GetOperationId(),
		Kind:          rtypes.OperationKindMint,
		Symbol:        rec.GetSymbol(),
		Network:       rec.GetNetwork(),
		Amount:        rec.GetAmount() + 1,
		Owner:         rec.GetOwner(),
		BtcTxID:       rec.GetBtcTxID(),
		Chain33TxHash: rec.GetChain33TxHash(),
	}
	_, err := f.r.newOperationRecordKV(tampered)
	require.Equal(t, ErrDuplicateOperation, err, "同 operationId 的第二次写入必须被拒")

	// 已写入的记录逐字节未变（金额仍是 1000，不是 1001）
	stored := &rtypes.OperationRecord{}
	require.NoError(t, readDB(f.state, formatOperationRecordKey(rec.GetOperationId()), stored))
	require.Equal(t, int64(1000), stored.GetAmount())
	require.True(t, bytes.Equal(types.Encode(rec), types.Encode(stored)), "记录必须与首次写入完全一致")

	// 连"内容完全相同"的重复写入也拒绝（写一次，不是"写相同的值算幂等"）
	_, err = f.r.newOperationRecordKV(rec)
	require.Equal(t, ErrDuplicateOperation, err)
}

// Test_OperationLedger_amountBindsOpID 金额是派生原像的一部分 ⇒ "改金额"在链上不存在入口。
func Test_OperationLedger_amountBindsOpID(t *testing.T) {
	f := newOperationLedgerFixture(t)
	rec := mintForTestF(t, f, "btc", "user-addr", 1000, "amount-bind")

	otherID, err := rtypes.MintOperationID(rec.GetSymbol(), rec.GetBtcTxID(), rec.GetOwner(), 2000)
	require.NoError(t, err)
	require.False(t, bytes.Equal(rec.GetOperationId(), otherID), "金额不同 ⇒ operationId 必须不同")

	// 旧记录的金额依然固定在链上；换 id 只是另开一条记录，不存在"改写原记录"的入口
	stored := &rtypes.OperationRecord{}
	require.NoError(t, readDB(f.state, formatOperationRecordKey(rec.GetOperationId()), stored))
	require.Equal(t, int64(1000), stored.GetAmount())
	require.ErrorIs(t, readDB(f.state, formatOperationRecordKey(otherID), &rtypes.OperationRecord{}), types.ErrNotFound)
}

// Test_OperationLedger_supplyAccumulates 供应量单调累加（minted 只增，burned 只在结算时增）。
func Test_OperationLedger_supplyAccumulates(t *testing.T) {
	f := newOperationLedgerFixture(t)
	mintForTestF(t, f, "btc", "user-a", 1000, "s1")
	mintForTestF(t, f, "btc", "user-b", 500, "s2")

	supply := supplyForTest(t, f, "btc")
	require.Equal(t, int64(1500), supply.GetMinted())
	require.Equal(t, int64(0), supply.GetBurned())
	require.Equal(t, int64(2), supply.GetNextMintIndex())
	require.Equal(t, "BTC", supply.GetNetwork())

	// 锁定一笔提现：burned 不动（销毁尚未发生），但 burn 下标前进
	burnForTest(t, f, "btc", "user-a", 600, []byte("burn-1"))
	supply = supplyForTest(t, f, "btc")
	require.Equal(t, int64(1500), supply.GetMinted())
	require.Equal(t, int64(0), supply.GetBurned(), "锁定不等于销毁：burned 只在结算成功时累加")
	require.Equal(t, int64(1), supply.GetNextBurnIndex())

	// 结算：burned += 600
	kv, err := f.r.bumpBurnedOperationSupply(ensureCrossChainSymbol("btc"), 600)
	require.NoError(t, err)
	applyStateKV(t, f.state, []*types.KeyValue{kv})
	supply = supplyForTest(t, f, "btc")
	require.Equal(t, int64(600), supply.GetBurned())
	require.Equal(t, int64(900), supply.GetMinted()-supply.GetBurned())
}

// ---------------------------------------------------------------------------
// ② 提现判据（对台账，不对地址余额求和）
// ---------------------------------------------------------------------------

// Test_CheckWithdrawOperationLedger_rejectsUnrecorded 没登记过的 burn ⇒ 拒绝（ErrOperationNotExist）。
func Test_CheckWithdrawOperationLedger_rejectsUnrecorded(t *testing.T) {
	f := newOperationLedgerFixture(t)
	mintForTestF(t, f, "btc", "user-a", 1000, "u1")

	withdraw := &rtypes.WithdrawAsset{AssetSymbol: "btc", Amount: 600}
	err := f.r.checkWithdrawOperationLedger([]byte("never-recorded-burn"), withdraw)
	require.Equal(t, ErrOperationNotExist, err)
}

// Test_CheckWithdrawOperationLedger_rejectsAmountMismatch 记录在、但金额与 payload 不一致 ⇒ 拒绝。
// 反向验证：把 checkWithdrawOperationLedger 里的金额比对删掉 → 本用例变红。
func Test_CheckWithdrawOperationLedger_rejectsAmountMismatch(t *testing.T) {
	f := newOperationLedgerFixture(t)
	mintForTestF(t, f, "btc", "user-a", 1000, "u2")
	burnTxHash := []byte("burn-amount-mismatch")
	burnForTest(t, f, "btc", "user-a", 600, burnTxHash)

	// 记录的锁定额是 600，payload 却说 601（"金额不一致"的构造）
	err := f.r.checkWithdrawOperationLedger(burnTxHash, &rtypes.WithdrawAsset{AssetSymbol: "btc", Amount: 601})
	require.Equal(t, ErrOperationAmountMismatch, err)

	// 金额一致时才放行
	require.NoError(t, f.r.checkWithdrawOperationLedger(burnTxHash,
		&rtypes.WithdrawAsset{AssetSymbol: "btc", Amount: 600}))
}

// Test_CheckWithdrawOperationLedger_rejectsUnmintedSupply 余额存在但**没有对应的铸造记录** ⇒ 拒绝。
// 这是"确实铸造过"这一条的关键用例：锁仓地址上凭空多出来的额度，链上不允许它离开桥。
// 反向验证：去掉供应量闸门 → 本用例变红。
func Test_CheckWithdrawOperationLedger_rejectsUnmintedSupply(t *testing.T) {
	f := newOperationLedgerFixture(t)
	burnTxHash := []byte("burn-no-mint")
	burnForTest(t, f, "btc", "user-a", 600, burnTxHash)

	// 台账里没有任何充值铸造（供应量记录都不存在 ⇒ minted=0）
	supply := supplyForTest(t, f, "btc")
	require.Equal(t, int64(0), supply.GetMinted())

	err := f.r.checkWithdrawOperationLedger(burnTxHash, &rtypes.WithdrawAsset{AssetSymbol: "btc", Amount: 600})
	require.Equal(t, ErrInsufficientMintedSupply, err)

	// 补上一笔足额铸造后放行（证明拒绝的原因确实是"没铸造过"，不是别的）
	mintForTestF(t, f, "btc", "user-a", 600, "late-mint")
	require.NoError(t, f.r.checkWithdrawOperationLedger(burnTxHash,
		&rtypes.WithdrawAsset{AssetSymbol: "btc", Amount: 600}))
}

// Test_CheckWithdrawOperationLedger_supplyCoversOnlyMinted 待销毁额可以超过单笔充值，但不得超过
// "已铸造 − 已销毁"；已结算（burned）的部分不能重复使用。
func Test_CheckWithdrawOperationLedger_supplyCoversOnlyMinted(t *testing.T) {
	f := newOperationLedgerFixture(t)
	mintForTestF(t, f, "btc", "user-a", 1000, "m-cov")
	firstBurn := []byte("burn-cov-1")
	burnForTest(t, f, "btc", "user-a", 900, firstBurn)

	// 只铸造过 1000：第一笔 900 通过
	require.NoError(t, f.r.checkWithdrawOperationLedger(firstBurn,
		&rtypes.WithdrawAsset{AssetSymbol: "btc", Amount: 900}))

	// 第二笔 900 对应的记录也在，但结算第一笔后"未销毁额度"只剩 100 ⇒ 拒绝
	secondBurn := []byte("burn-cov-2")
	burnForTest(t, f, "btc", "user-a", 900, secondBurn)
	require.NoError(t, f.r.checkWithdrawOperationLedger(secondBurn,
		&rtypes.WithdrawAsset{AssetSymbol: "btc", Amount: 900}), "第一笔尚未结算：额度仍算未使用")

	usedKV, err := f.r.bumpBurnedOperationSupply(ensureCrossChainSymbol("btc"), 900)
	require.NoError(t, err)
	applyStateKV(t, f.state, []*types.KeyValue{usedKV})
	require.Equal(t, ErrInsufficientMintedSupply,
		f.r.checkWithdrawOperationLedger(secondBurn, &rtypes.WithdrawAsset{AssetSymbol: "btc", Amount: 900}))

	// 另一 symbol 的额度不能混用（供应量按 symbol 隔离）
	burnForTest(t, f, "RGB20_USDT", "user-a", 900, secondBurn)
	require.Equal(t, ErrInsufficientMintedSupply,
		f.r.checkWithdrawOperationLedger(secondBurn, &rtypes.WithdrawAsset{AssetSymbol: "RGB20_USDT", Amount: 900}))
}

// ---------------------------------------------------------------------------
// ③ 正常路径（真 Exec_Deposit + 真 Exec_Withdraw + 结算）
// ---------------------------------------------------------------------------

// Test_OperationLedger_depositThenWithdrawEndToEnd 端到端：充值铸造 → 提现锁定 → 结算放行，
// 台账三项（记录/下标/供应量）随交易一起落库，minted 与 burned 对称变动。
func Test_OperationLedger_depositThenWithdrawEndToEnd(t *testing.T) {
	f := newOperationLedgerFixture(t)
	userAddr, userPriv := util.Genaddress()

	// —— 充值：真 Exec_Deposit（含严格解析 txid → 派生 opId → 写台账 → 铸造）——
	deposit := &rtypes.DepositAsset{
		Amount:         1000,
		DepositAddress: userAddr,
		AssetSymbol:    "btc",
		TxProof:        &rtypes.BtcTxProof{TxData: canonicalBtcTxData(t, "e2e-deposit", 1000)},
	}
	depositTx, err := f.r.GetExecutorType().CreateTransaction(rtypes.NameDepositAssetAction, deposit)
	require.NoError(t, err)
	depositTx.Sign(types.SECP256K1, userPriv)
	depositReceipt, err := f.r.Exec(depositTx, 0)
	require.NoError(t, err)
	applyStateKV(t, f.state, depositReceipt.KV)

	supply := supplyForTest(t, f, "btc")
	require.Equal(t, int64(1000), supply.GetMinted())

	// 铸造记录的身份口径：btc txid（解析后交易的规范身份）+ 充值地址 + 金额
	txID := parseBtcTxIDFromRaw(t, deposit.TxProof.GetTxData())
	mintOpID, err := rtypes.MintOperationID(ensureCrossChainSymbol("btc"), txID, userAddr, 1000)
	require.NoError(t, err)
	mintRec := &rtypes.OperationRecord{}
	require.NoError(t, readDB(f.state, formatOperationRecordKey(mintOpID), mintRec))
	require.Equal(t, rtypes.OperationKindMint, mintRec.GetKind())
	require.Equal(t, int64(1000), mintRec.GetAmount())
	require.Equal(t, userAddr, mintRec.GetOwner())
	require.True(t, bytes.Equal(txID, mintRec.GetBtcTxID()))
	require.True(t, bytes.Equal(depositTx.Hash(), mintRec.GetChain33TxHash()))
	require.True(t, f.r.isOperationSettled(mintRec), "铸造记录在铸造成功那刻即已结算")

	// —— 提现：真 Exec_Withdraw（含派生 opId → 写 burn 台账 + 锁定）——
	withdraw := &rtypes.WithdrawAsset{
		Amount:          600,
		FeeRate:         10,
		DestinationAddr: "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kygt080",
		AssetSymbol:     "btc",
	}
	withdrawTx, err := f.r.GetExecutorType().CreateTransaction(rtypes.NameWithdrawAssetAction, withdraw)
	require.NoError(t, err)
	withdrawTx.Sign(types.SECP256K1, userPriv)
	withdrawReceipt, err := f.r.Exec(withdrawTx, 1)
	require.NoError(t, err)
	applyStateKV(t, f.state, withdrawReceipt.KV)

	burnOpID, err := rtypes.BurnOperationID(ensureCrossChainSymbol("btc"), withdrawTx.Hash())
	require.NoError(t, err)
	burnRec := &rtypes.OperationRecord{}
	require.NoError(t, readDB(f.state, formatOperationRecordKey(burnOpID), burnRec))
	require.Equal(t, rtypes.OperationKindBurn, burnRec.GetKind())
	require.Equal(t, int64(600), burnRec.GetAmount())
	require.Equal(t, userAddr, burnRec.GetOwner())
	require.False(t, f.r.isOperationSettled(burnRec), "尚未结算放款")

	// 锁定阶段 burned 不动
	supply = supplyForTest(t, f, "btc")
	require.Equal(t, int64(0), supply.GetBurned())

	// —— 结算前的判据（CheckTx 调的就是这个）——
	require.NoError(t, f.r.checkWithdrawOperationLedger(withdrawTx.Hash(), withdraw))

	// —— 结算：销毁 + burned += 600 ——
	settleReceipt, err := f.r.confirmWithdrawSettlement(&rtypes.ConfirmTx{TxHash: withdrawTx.Hash()}, "txH", "cH")
	require.NoError(t, err)
	applyStateKV(t, f.state, settleReceipt.KV)

	supply = supplyForTest(t, f, "btc")
	require.Equal(t, int64(600), supply.GetBurned())
	require.Equal(t, int64(400), supply.GetMinted()-supply.GetBurned())

	// 结算后查询面：burn 记录 settled = true（补算，不写回）
	view, err := f.r.Query_GetOperation(&rtypes.ReqGetOperation{OperationId: burnOpID})
	require.NoError(t, err)
	require.True(t, view.(*rtypes.OperationRecord).GetSettled())

	// 同一笔 burn 不能二次结算（S3 的已消费键仍然在位）
	_, err = f.r.confirmWithdrawSettlement(&rtypes.ConfirmTx{TxHash: withdrawTx.Hash()}, "txH", "cH")
	require.Error(t, err)
}

// Test_ConfirmWithdrawSettlement_requiresRecord 没有台账记录就不许销毁（Exec 侧纵深防御）：
// 否则供应量会漏记一笔 burn，"已铸造 − 已销毁"与实际不符。
func Test_ConfirmWithdrawSettlement_requiresRecord(t *testing.T) {
	f := newOperationLedgerFixture(t)
	burnTxHash := []byte("no-record-settlement")
	require.NoError(t, f.state.Set(formatPayloadKey(burnTxHash), types.Encode(
		&rtypes.WithdrawAsset{AssetSymbol: "btc", Amount: 600})))

	_, err := f.r.confirmWithdrawSettlement(&rtypes.ConfirmTx{TxHash: burnTxHash}, "txH", "cH")
	require.Equal(t, ErrOperationNotExist, err)

	// 记录金额与 payload 不符时同样拒绝
	burnForTest(t, f, "btc", "user-a", 500, burnTxHash)
	_, err = f.r.confirmWithdrawSettlement(&rtypes.ConfirmTx{TxHash: burnTxHash}, "txH", "cH")
	require.Equal(t, ErrOperationAmountMismatch, err)
}

// Test_Exec_Deposit_rejectsInvalidProofFailClosed 非规范（解析不出 txid）的证明：
// 整笔充值失败，而不是"铸了但不记账" —— 后者会让供应量少算，把合法的后续提现挡在闸门外。
func Test_Exec_Deposit_rejectsInvalidProofFailClosed(t *testing.T) {
	f := newOperationLedgerFixture(t)
	deposit := &rtypes.DepositAsset{
		Amount:         100,
		DepositAddress: "1AmRYcURfDGxBhiaJAvEGdRkvkoM7ztn1u",
		AssetSymbol:    "btc",
		TxProof:        &rtypes.BtcTxProof{}, // 空证明
	}
	tx, err := f.r.GetExecutorType().CreateTransaction(rtypes.NameDepositAssetAction, deposit)
	require.NoError(t, err)
	tx.Sign(types.SECP256K1, testPriv)
	_, err = f.r.Exec(tx, 0)
	require.Equal(t, ErrInvalidBtcTxProof, err)

	// 没有铸出任何资产，也没有台账记录
	acc, err := f.r.newAccount("xBTC")
	require.NoError(t, err)
	require.Equal(t, int64(0), acc.LoadAccount(deposit.GetDepositAddress()).GetBalance())
	require.Equal(t, int64(0), supplyForTest(t, f, "btc").GetMinted())
}

// ---------------------------------------------------------------------------
// 判据的**接线**：走真实 CheckTx（共识强制点）
// ---------------------------------------------------------------------------

// Test_CheckTx_withdrawConfirmRequiresLedger 提现确认走 CheckTx 全链路时，台账判据必须生效：
// 没登记过 ⇒ ErrOperationNotExist；登记了但没铸造过 ⇒ ErrInsufficientMintedSupply；
// 两者都齐 ⇒ 放行。反向验证：删掉 validate_proof.go 里对 checkWithdrawOperationLedger 的调用
// → 本用例的前两条断言变红（判据还在，但没人调它）。
func Test_CheckTx_withdrawConfirmRequiresLedger(t *testing.T) {
	r := newRgbx()
	tx := &types.Transaction{}
	tx.Sign(types.SECP256K1, testPriv) // from == rgbxCfg.CommitAddress

	// 单交易区块的有效 SPV：header.merkleRoot == btc txid
	btcTx := wire.NewMsgTx(wire.TxVersion)
	btcTx.AddTxOut(wire.NewTxOut(1000, []byte{txscript.OP_0, 0x14}))
	var buf bytes.Buffer
	require.NoError(t, btcTx.SerializeNoWitness(&buf))
	txid := btcTx.TxHash()

	api := mockGuardianAPI(t, testCommitAddr)
	api.On("Query", ltypes.LightclientX, "GetBtcHeader", mock.Anything).Return(
		&ltypes.BtcHeader{Hash: "hash1", Height: 100, MerkleRoot: txid.String()}, nil)
	dir, state, local := util.CreateTestDB()
	defer util.CloseTestDB(dir, state)
	r.SetAPI(api)
	r.SetStateDB(state)
	r.SetLocalDB(local)

	burnTxHash := []byte("ledger-gate-burn")
	withdraw := &rtypes.WithdrawAsset{AssetSymbol: rtypes.RGB20USDTSymbol, Amount: 100}
	// RGB20 分支：链上跳过 OP_RETURN 承诺与金额校验，判据只剩台账这一层，正好用来测接线。
	require.NoError(t, state.Set(formatPayloadKey(burnTxHash), types.Encode(withdraw)))
	require.NoError(t, local.Set(formatPendingTxKey(3, 0), types.Encode(&rtypes.PendingTx{
		ActionType:    rtypes.TyWithdrawAsset,
		TxBlockHeight: 3,
		TxIndex:       0,
		TxHash:        burnTxHash,
		AssetSymbol:   rtypes.RGB20USDTSymbol,
	})))

	action := &rtypes.RgbxAction{Ty: rtypes.TyConfirmAction, Value: &rtypes.RgbxAction_Confirm{}}
	confirm := &rtypes.ConfirmTx{
		ActionType:    rtypes.TyWithdrawAsset,
		TxBlockHeight: 3,
		TxIndex:       0,
		TxHash:        burnTxHash,
		BtcTxProof: &rtypes.BtcTxProof{
			TxData:      buf.Bytes(),
			BlockHeight: 100,
			BlockHash:   "hash1",
			TxIndex:     0,
		},
	}
	action.Value.(*rtypes.RgbxAction_Confirm).Confirm = confirm
	tx.Payload = types.Encode(action)

	// 1) 没有 burn 台账记录 ⇒ 拒绝（这一笔 burn 不是从 Exec_Withdraw 来的）
	require.Equal(t, ErrOperationNotExist, r.CheckTx(tx, 0))

	// 2) 有记录、但台账里没有任何铸造（"没铸造过"）⇒ 拒绝
	recordBurnForTest(t, r.(*rgbx), state, rtypes.RGB20USDTSymbol, withdraw.GetAmount(), burnTxHash)
	require.Equal(t, ErrInsufficientMintedSupply, r.CheckTx(tx, 0))

	// 3) 补上足额铸造 ⇒ 放行
	mintForTest(t, r.(*rgbx), state, rtypes.RGB20USDTSymbol, "depositor", withdraw.GetAmount(), "gate")
	require.NoError(t, r.CheckTx(tx, 0))
}

// ---------------------------------------------------------------------------
// ④ 查询 / 审计
// ---------------------------------------------------------------------------

// Test_Query_OperationLedger 审计三件套：按 operationId 查记录、按 symbol 查供应量、按下标列举。
func Test_Query_OperationLedger(t *testing.T) {
	f := newOperationLedgerFixture(t)

	var mintIDs [][]byte
	for i, seed := range []string{"q1", "q2", "q3"} {
		rec := mintForTestF(t, f, "btc", "user-a", int64(100*(i+1)), seed)
		mintIDs = append(mintIDs, rec.GetOperationId())
	}
	burnOpID := burnForTest(t, f, "btc", "user-a", 100, []byte("q-burn"))

	// 1) GetOperation：按 id 查记录（含 btc txid 等完整身份）
	msg, err := f.r.Query_GetOperation(&rtypes.ReqGetOperation{OperationId: mintIDs[0]})
	require.NoError(t, err)
	rec := msg.(*rtypes.OperationRecord)
	require.Equal(t, int64(100), rec.GetAmount())
	require.Equal(t, "XBTC", rec.GetSymbol())
	require.Equal(t, "BTC", rec.GetNetwork())
	require.Len(t, rec.GetBtcTxID(), rtypes.OperationIDLen)
	require.True(t, rec.GetSettled())

	// 未登记的 id：返回 ErrNotFound（"没有这条记录"必须响亮，不能返回空记录）
	_, err = f.r.Query_GetOperation(&rtypes.ReqGetOperation{OperationId: bytes.Repeat([]byte{0xaa}, 32)})
	require.Error(t, err)
	// 长度不对：参数错误
	_, err = f.r.Query_GetOperation(&rtypes.ReqGetOperation{OperationId: []byte("short")})
	require.Equal(t, types.ErrInvalidParam, err)

	// 2) GetOperationSupply：minted/burned 汇总（未操作过的 symbol 返回零值，不报错）
	msg, err = f.r.Query_GetOperationSupply(&types.ReqString{Data: "btc"})
	require.NoError(t, err)
	supply := msg.(*rtypes.OperationSupply)
	require.Equal(t, int64(600), supply.GetMinted())
	require.Equal(t, int64(0), supply.GetBurned())
	require.Equal(t, int64(3), supply.GetNextMintIndex())

	msg, err = f.r.Query_GetOperationSupply(&types.ReqString{Data: "rgb20_usdt"})
	require.NoError(t, err)
	require.Equal(t, int64(0), msg.(*rtypes.OperationSupply).GetMinted())
	require.Equal(t, "XRGB20_USDT", msg.(*rtypes.OperationSupply).GetSymbol())

	// 3) ListOperations：铸造三条（按下标升序 = 写入顺序），提现一条
	msg, err = f.r.Query_ListOperations(&rtypes.ReqListOperations{
		AssetSymbol: "btc", Kind: rtypes.OperationKindMint, Start: 0, Count: 10})
	require.NoError(t, err)
	records := msg.(*rtypes.OperationRecords)
	require.Len(t, records.GetRecords(), 3)
	require.Equal(t, int64(0), records.GetNextIndex(), "未取满一页 ⇒ 没有下一页")
	for i, rec := range records.GetRecords() {
		require.True(t, bytes.Equal(mintIDs[i], rec.GetOperationId()), "第 %d 条按写入顺序返回", i)
	}

	msg, err = f.r.Query_ListOperations(&rtypes.ReqListOperations{
		AssetSymbol: "btc", Kind: rtypes.OperationKindBurn, Start: 0, Count: 10})
	require.NoError(t, err)
	records = msg.(*rtypes.OperationRecords)
	require.Len(t, records.GetRecords(), 1)
	require.True(t, bytes.Equal(burnOpID, records.GetRecords()[0].GetOperationId()))
	require.False(t, records.GetRecords()[0].GetSettled())

	// 分页：一页两条
	msg, err = f.r.Query_ListOperations(&rtypes.ReqListOperations{
		AssetSymbol: "btc", Kind: rtypes.OperationKindMint, Start: 0, Count: 2})
	require.NoError(t, err)
	records = msg.(*rtypes.OperationRecords)
	require.Len(t, records.GetRecords(), 2)
	require.Equal(t, int64(2), records.GetNextIndex())
	msg, err = f.r.Query_ListOperations(&rtypes.ReqListOperations{
		AssetSymbol: "btc", Kind: rtypes.OperationKindMint, Start: records.GetNextIndex(), Count: 2})
	require.NoError(t, err)
	require.Len(t, msg.(*rtypes.OperationRecords).GetRecords(), 1)

	// 参数校验：非法 kind / count / symbol
	_, err = f.r.Query_ListOperations(&rtypes.ReqListOperations{AssetSymbol: "btc", Kind: 99, Count: 1})
	require.Equal(t, types.ErrInvalidParam, err)
	_, err = f.r.Query_ListOperations(&rtypes.ReqListOperations{AssetSymbol: "btc", Kind: 1, Count: 0})
	require.Equal(t, types.ErrInvalidParam, err)
	_, err = f.r.Query_ListOperations(&rtypes.ReqListOperations{Kind: 1, Count: 1})
	require.Equal(t, types.ErrInvalidParam, err)
}

// Test_OperationLedger_networkIsConfigDerived network 由 symbol 确定性导出（不读节点本地配置）。
func Test_OperationLedger_networkIsConfigDerived(t *testing.T) {
	origin := rgbxCfg.CrossChainAssetPrefix
	defer func() { rgbxCfg.CrossChainAssetPrefix = origin }()

	rgbxCfg.CrossChainAssetPrefix = "X"
	require.Equal(t, "BTC", operationNetwork("XBTC"))
	require.Equal(t, "RGB20_USDT", operationNetwork("XRGB20_USDT"))
	// 前缀本身就是符号时不会退化成空串
	require.Equal(t, "X", operationNetwork("X"))
	// 键与下标键的构造口径（前缀 + 32 字节 id / symbol + 类别 + 定宽下标）
	require.Equal(t, string(formatOperationRecordKey([]byte{1, 2})),
		KeyPrefixStateDB+"oprec-"+string([]byte{1, 2}))
	// 下标键用调用方传入的 symbol 形态（写入侧传的是跨链账户符号 "XBTC"）
	require.Equal(t, string(formatOperationIndexKey("XBTC", rtypes.OperationKindBurn, 7)),
		KeyPrefixStateDB+"opidx-XBTC-b-00000000000000000007")
	require.Equal(t, string(formatOperationIndexKey("XBTC", rtypes.OperationKindMint, 0)),
		KeyPrefixStateDB+"opidx-XBTC-m-00000000000000000000")
	// 供应量键只做大小写归一（调用方负责传跨链账户符号）
	require.Equal(t, string(formatOperationSupplyKey("xbtc")),
		KeyPrefixStateDB+"opsupply-XBTC")
}

// parseBtcTxIDFromRaw 测试辅助：拿到规范 txid（与生产口径同一个函数）。
func parseBtcTxIDFromRaw(t *testing.T, raw []byte) []byte {
	t.Helper()
	txID, err := parseBtcTxIDStrict("test", raw)
	require.NoError(t, err)
	return txID
}
