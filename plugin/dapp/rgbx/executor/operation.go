package executor

import (
	"bytes"
	"encoding/hex"
	"errors"

	"github.com/33cn/chain33/types"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
)

/*
 * 全局操作台账（S2）：operationId → (kind, amount, symbol/network, owner, 相关身份)
 *
 * 三条不变性 —— 读/改这个文件前先确认它们在：
 *
 *  1. **记录不可变**：单条记录只写一次，永不覆盖。键是 operationId（由操作自身的身份确定性派生，
 *     见 types/operation.go），值是操作的事实本身；同 id 的第二次写入（哪怕内容完全相同）会被
 *     newOperationRecordKV 拒绝（ErrDuplicateOperation）。**没有更新路径**：想让记录里的金额变，
 *     只能换一个 operationId，而换 id 就等于换一笔操作（金额是派生原像的一部分）。
 *
 *  2. **只能随回执 KV 写入**。所有台账 KV 都由 Exec_* 追加进 receipt.KV，由框架随 stateDB 一起
 *     提交/回滚 —— 与 E1（充值防双铸）、S3（提现防重复放款）、E14（Confirm 去重）同一原则：
 *     重组回滚时台账跟着回滚，因此"回滚后重放同一笔操作"不会被误判为重复。
 *
 *  3. **共识状态**。台账读写全部走 GetStateDB()，不读 LocalDB / 节点本地配置：提现结算的判据
 *     必须是全网一致结论，否则会让"区块是否合法"依赖各节点本地库（S3 立的原则）。
 *
 * 供应量计数（operationSupply）是这套台账里**唯一可变**的东西，但它是**单调累加器**：
 *   只增不减、可由记录集合重新算出来（记录才是真相，计数只是 O(1) 的汇总视图）。
 *   minted 在充值铸造成功时 +amount，burned 在提现销毁成功时 +amount；
 *   minted - burned = "桥仍然欠出去的额度"，提现结算时要求它不小于本次销毁额（见 checkWithdrawOperationLedger）。
 */

// loadOperationSupply 读某 symbol 的供应量计数；不存在时返回零值记录（首次操作即从 0 开始）。
func (r *rgbx) loadOperationSupply(symbol string) (*rtypes.OperationSupply, error) {
	supply := &rtypes.OperationSupply{
		Symbol:  formatSymbol(symbol),
		Network: operationNetwork(symbol),
	}
	err := readDB(r.GetStateDB(), formatOperationSupplyKey(symbol), supply)
	if errors.Is(err, types.ErrNotFound) {
		return supply, nil
	}
	if err != nil {
		elog.Error("loadOperationSupply read supply", "symbol", symbol, "err", err)
		return nil, err
	}
	return supply, nil
}

// newOperationRecordKV 生成一条台账记录的 KV，并做**写一次**校验。
//
// 读失败的处理是 fail-closed 的：只有 ErrNotFound（确实没有）才允许写；
// 其它任何错误（含读到了已存在的键）都拒绝 —— 不把"读不到"当成"没写过"。
func (r *rgbx) newOperationRecordKV(rec *rtypes.OperationRecord) (*types.KeyValue, error) {
	key := formatOperationRecordKey(rec.GetOperationId())
	if _, err := r.GetStateDB().Get(key); !errors.Is(err, types.ErrNotFound) {
		elog.Error("newOperationRecordKV operation already recorded", "kind", rec.GetKind(),
			"symbol", rec.GetSymbol(), "opID", hex.EncodeToString(rec.GetOperationId()),
			"amount", rec.GetAmount(), "err", err)
		return nil, ErrDuplicateOperation
	}
	return &types.KeyValue{Key: key, Value: types.Encode(rec)}, nil
}

// recordMintOperation 生成"充值铸造"操作的台账 KV：记录 + 下标 + 供应量计数（三件套）。
//
// 调用点是 Exec_Deposit 的**铸造成功之后**（与 E1 的已消费 txid 键同一位置）：铸造失败不产生台账，
// 台账与资产一起生效、一起回滚。
func (r *rgbx) recordMintOperation(opID, btcTxID []byte, symbol, owner string, amount int64,
	chain33TxHash []byte) ([]*types.KeyValue, error) {

	supply, err := r.loadOperationSupply(symbol)
	if err != nil {
		return nil, err
	}
	seq := supply.GetNextMintIndex()
	rec := &rtypes.OperationRecord{
		OperationId:   opID,
		Kind:          rtypes.OperationKindMint,
		Symbol:        formatSymbol(symbol),
		Network:       operationNetwork(symbol),
		Amount:        amount,
		Owner:         owner,
		BtcTxID:       btcTxID,
		Chain33TxHash: chain33TxHash,
		Height:        r.GetHeight(),
		Timestamp:     r.GetBlockTime(),
	}
	recordKV, err := r.newOperationRecordKV(rec)
	if err != nil {
		return nil, err
	}
	supply.Minted += amount
	supply.NextMintIndex = seq + 1
	return []*types.KeyValue{
		recordKV,
		{Key: formatOperationIndexKey(symbol, rtypes.OperationKindMint, seq), Value: opID},
		{Key: formatOperationSupplyKey(symbol), Value: types.Encode(supply)},
	}, nil
}

// recordBurnOperation 生成"提现销毁"操作的台账 KV（记录 + 下标 + 供应量计数）。
//
// 调用点是 Exec_Withdraw 的**锁定成功之后**：从这一刻起这笔 burn 的金额就固定写进了共识状态
// （不可变），后面结算时的校验（checkWithdrawOperationLedger）拿它当判据 —— 也就是说
// "放款金额"由锁定时写下的记录决定，不是由结算时可变的输入决定。
//
// 注意这里**不动 burned**：销毁还没发生（放款尚未结算），burned 只在结算成功时才累加。
// 因此 minted - burned 始终包含"已锁定未结算"的额度（对结算检查是保守方向：不会因为
// 一笔在途提现而把另一笔合法提现判为超额）。
func (r *rgbx) recordBurnOperation(opID []byte, symbol, owner string, amount int64, chain33TxHash []byte) ([]*types.KeyValue, error) {

	supply, err := r.loadOperationSupply(symbol)
	if err != nil {
		return nil, err
	}
	seq := supply.GetNextBurnIndex()
	rec := &rtypes.OperationRecord{
		OperationId:   opID,
		Kind:          rtypes.OperationKindBurn,
		Symbol:        formatSymbol(symbol),
		Network:       operationNetwork(symbol),
		Amount:        amount,
		Owner:         owner,
		Chain33TxHash: chain33TxHash,
		Height:        r.GetHeight(),
		Timestamp:     r.GetBlockTime(),
	}
	recordKV, err := r.newOperationRecordKV(rec)
	if err != nil {
		return nil, err
	}
	supply.NextBurnIndex = seq + 1
	return []*types.KeyValue{
		recordKV,
		{Key: formatOperationIndexKey(symbol, rtypes.OperationKindBurn, seq), Value: opID},
		{Key: formatOperationSupplyKey(symbol), Value: types.Encode(supply)},
	}, nil
}

// bumpBurnedOperationSupply 提现结算成功时的供应量累加（burned += amount）。
func (r *rgbx) bumpBurnedOperationSupply(symbol string, amount int64) (*types.KeyValue, error) {
	supply, err := r.loadOperationSupply(symbol)
	if err != nil {
		return nil, err
	}
	supply.Burned += amount
	return &types.KeyValue{Key: formatOperationSupplyKey(symbol), Value: types.Encode(supply)}, nil
}

// checkWithdrawOperationLedger 提现结算前的台账校验（S2 的判据）。
//
// 判定式（四项全部满足才放行；前两项是"确实记录过且金额一致"，第三项是"确实铸造过"）：
//
//  1. 记录存在：ledger[burnOpId(symbol, burnTxHash)] 命中（否则 ErrOperationNotExist）；
//  2. 记录与本次结算逐项一致：kind=burn、symbol 相同、chain33TxHash=本次 confirm 的 burn、
//     **amount 与 payload 里的提现金额相等**（否则 ErrOperationAmountMismatch / ErrOperationMismatch）；
//  3. 供应量足够：minted − burned ≥ 本次销毁额（否则 ErrInsufficientMintedSupply）。
//
// 为什么是"对照台账"而不是"对地址余额求和"（这是本项要解决的问题）：
//   - 余额是**地址级聚合**：它只说"这个地址现在有多少"，说不出"这些额度从哪来"。台账是
//     **操作级**的：每个单位都能追到一条具体的充值铸造记录（含 btc txid、金额、归属地址）。
//   - 第 3 项把"每个离开桥的单位都必须有一条铸造记录"变成**退出闸门**：任何未来新增/被改坏的
//     铸币路径若不写台账，铸出来的额度就出不了桥（一旦 attempted 提现即被拒）。
//     —— 这正是"proof-of-mint"的落地形态：供给侧的证明在**出口**被强制核对。
//   - 只有共识状态参与判定（记录 + 供应量 + statedb 里的 payload），**不读 LocalDB**：
//     同一笔提现的结论必须全网一致（对照 S3 的教训）。
//
// 如实记（边界，别当成它更强）：在"每地址余额 + 守恒记账"的现有模型下，第 3 项对**当前**代码
// 路径是**冗余**的 —— 锁定成功已要求用户余额足够，而余额本身只能来自通过校验的铸造。它的价值在
// 于把这条不变性**显式化、可判定、可审计**，并对未来任何绕过台账的铸币路径兜底；它并不会
// 让今天的合法提现变得更难，也不会挡住今天能挡的攻击之外的东西。
func (r *rgbx) checkWithdrawOperationLedger(confirmTxHash []byte, withdraw *rtypes.WithdrawAsset) error {

	symbol := ensureCrossChainSymbol(withdraw.GetAssetSymbol())
	opID, err := rtypes.BurnOperationID(symbol, confirmTxHash)
	if err != nil {
		elog.Error("checkWithdrawOperationLedger derive burn opID", "symbol", symbol,
			"burnTxHash", hex.EncodeToString(confirmTxHash), "err", err)
		return ErrInvalidOperationID
	}
	rec := &rtypes.OperationRecord{}
	if err = readDB(r.GetStateDB(), formatOperationRecordKey(opID), rec); errors.Is(err, types.ErrNotFound) {
		elog.Error("checkWithdrawOperationLedger burn operation not recorded", "symbol", symbol,
			"burnTxHash", hex.EncodeToString(confirmTxHash), "opID", hex.EncodeToString(opID))
		return ErrOperationNotExist
	} else if err != nil {
		elog.Error("checkWithdrawOperationLedger read operation record", "symbol", symbol,
			"burnTxHash", hex.EncodeToString(confirmTxHash), "opID", hex.EncodeToString(opID), "err", err)
		return err
	}
	if rec.GetKind() != rtypes.OperationKindBurn ||
		!bytes.Equal(rec.GetChain33TxHash(), confirmTxHash) ||
		rec.GetSymbol() != formatSymbol(symbol) {
		elog.Error("checkWithdrawOperationLedger record mismatch", "symbol", symbol,
			"burnTxHash", hex.EncodeToString(confirmTxHash), "opID", hex.EncodeToString(opID),
			"kind", rec.GetKind(), "recSymbol", rec.GetSymbol(), "amount", rec.GetAmount(),
			"recBurnTxHash", hex.EncodeToString(rec.GetChain33TxHash()))
		return ErrOperationMismatch
	}
	if rec.GetAmount() != withdraw.GetAmount() {
		elog.Error("checkWithdrawOperationLedger amount mismatch", "symbol", symbol,
			"burnTxHash", hex.EncodeToString(confirmTxHash), "opID", hex.EncodeToString(opID),
			"recordedAmount", rec.GetAmount(), "payloadAmount", withdraw.GetAmount())
		return ErrOperationAmountMismatch
	}
	supply, err := r.loadOperationSupply(symbol)
	if err != nil {
		return err
	}
	if outstanding := supply.GetMinted() - supply.GetBurned(); outstanding < withdraw.GetAmount() {
		elog.Error("checkWithdrawOperationLedger insufficient minted supply", "symbol", symbol,
			"burnTxHash", hex.EncodeToString(confirmTxHash), "opID", hex.EncodeToString(opID),
			"minted", supply.GetMinted(), "burned", supply.GetBurned(),
			"outstanding", outstanding, "need", withdraw.GetAmount())
		return ErrInsufficientMintedSupply
	}
	return nil
}
