// Copyright Fuzamei Corp. 2018 All Rights Reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package neutrino

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/33cn/chain33/types"
	ltypes "github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
	"github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino/rgb20"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/walletdb"
)

/*
 * submitBitcoinHeaders 把 btcd/neutrino 的 canonical 头链提交到 chain33。
 *
 * 关键改动（B3 的中继侧配套）：**每个 tick 先与链上 canonical 头链对账，再决定从哪个高度提交**，
 * 而不是拿一个内存变量单调推进。BTC 重组后中继能自己回退到一致高度重发；对不上的错位（回退深度
 * 超限、起点/锚点配置错位）显式报错并停在那个状态，不再无限重试同一个批。算法与错误语义见
 * btc_header_sync.go。
 */
func (n *neutrinoClient) submitBitcoinHeaders() {

	reconciler := &btcHeaderReconciler{
		chain:       &chain33BtcHeaderView{n: n},
		local:       &neutrinoBtcHeaderView{n: n},
		startHeight: n.cfg.BtcHeaderStartHeight,
		maxDepth:    maxBtcHeaderReorgDepth,
	}
	// 启动期等主链可查询（与改造前一致）：查不通时不提交，交给后续 tick 继续重试。
	n.waitUntilDone("wait getChain33BtcLastHeader", func() bool {
		_, err := reconciler.chain.chainTipHeader()
		return err == nil
	}, 0)

	log.Info("submitBitcoinHeaders start", "startHeight", n.cfg.BtcHeaderStartHeight,
		"maxReorgDepth", maxBtcHeaderReorgDepth, "batchSize", btcHeaderBatchSize)

	interval := n.cfg.BtcBlockInterval/3 + 1
	ticker := time.NewTicker(time.Second * time.Duration(interval))
	defer ticker.Stop()

	state := &btcHeaderSyncState{}
	for {
		select {
		case <-n.ctx.Done():
			return

		case <-ticker.C:

			best := n.getBestBlock()
			if best == nil || best.Height <= 0 {
				continue
			}
			n.submitBtcHeadersOnce(reconciler, state, uint64(best.Height))

		}
	}
}

// btcConfirmedHeight 本地视图里"已经足够确认、可以提交"的最高高度。
// 本地 tip 还不到配置的确认数时返回 0（改造前这里会 uint64 下溢成极大的高度，进而按高度查一堆
// 根本还不存在的头）。
func (n *neutrinoClient) btcConfirmedHeight(localTip uint64) uint64 {
	confirmations := uint64(n.cfg.BlockConfirmations)
	if localTip <= confirmations {
		return 0
	}
	return localTip - confirmations
}

// buildBtcHeaderBatch 组装 [from, to] 的逐高度头。中途取不到头就截断（保持连续），
// 第一个头就取不到时返回空批。
func (n *neutrinoClient) buildBtcHeaderBatch(from, to, bestHeight uint64) []*ltypes.BtcHeader {
	headers := make([]*ltypes.BtcHeader, 0, to-from+1)
	for height := from; height <= to; height++ {
		header, err := n.neutrinoCS.BlockHeaders.FetchHeaderByHeight(uint32(height))
		if err != nil {
			log.Error("buildBtcHeaderBatch FetchHeaderByHeight", "height", height, "err", err)
			break
		}
		headers = append(headers, &ltypes.BtcHeader{
			Hash:          header.BlockHash().String(),
			Confirmations: bestHeight - height + 1,
			Height:        height,
			Version:       uint32(header.Version),
			MerkleRoot:    header.MerkleRoot.String(),
			Time:          header.Timestamp.Unix(),
			Nonce:         uint64(header.Nonce),
			Bits:          int64(header.Bits),
			PreviousHash:  header.PrevBlock.String(),
		})
	}
	return headers
}

// submitBtcHeadersOnce 对账一次并提交一批头。
//
// 所有错误都只影响本轮：可自愈的问题（查询失败、主链未就绪、本地还没追上）等下一轮；不可自愈的问题
// （链上明确拒收、回退深度超限、起点与锚点不配套）显式报错并停在那个状态，同一个状态只报一次。
// 同一批在途时不会每个 tick 都重发（见 btcHeaderSyncState）。
func (n *neutrinoClient) submitBtcHeadersOnce(r *btcHeaderReconciler, st *btcHeaderSyncState, localTip uint64) {

	plan, err := r.plan(localTip)
	if err != nil {
		switch {
		case errors.Is(err, errBtcReconcileLocalBehind):
			// btcd 还在这条链上往前同步（重启/重建头库）：等它追上，不是错误。
			// 状态是"本地落后"，与每一轮的具体高度无关，故用固定 key（否则每轮都会刷一条）。
			if st.report.allow("local-behind") {
				log.Info("submitBitcoinHeaders waiting for the local btc header view to catch up", "err", err)
			}
		default:
			// 不可自愈：允许窗口内找不到任何一致高度（超深重组 / 换网 / 起点与锚点不配套）。
			// 显式报错，同一状态只报一次，停在这里等运维处理（重新 bootstrap）。
			if st.report.allow("unreconcilable") {
				log.Error("submitBitcoinHeaders cannot reconcile the local btc header view with the on-chain chain, "+
					"header submission is stalled until this changes (re-bootstrap or check btcHeaderStartHeight against the anchor)",
					"err", err, "localTip", localTip)
			}
		}
		return
	}
	st.report.reset()

	// L2：只在 bootstrap（链上 tip==0，此时才用得上 cfg.BtcHeaderStartHeight）断言起点与链上锚点配套。
	// 不配套就是"每个 batch 都被执行器以 ErrBtcHeaderNoAnchor 确定性拒收"，这里直接不发交易（fail-closed），
	// 并把期望值打进日志；链上不支持该查询（旧执行器）或本网络没有锚点时降级为照常提交。
	if plan.chainTipHeight == 0 && !r.anchor.check(r.chain, r.startHeight) {
		return
	}

	now := time.Now()
	from, to, reason := st.beginSubmit(plan, n.btcConfirmedHeight(localTip), now)
	switch reason {
	case btcHeaderSubmitPlanHalted:
		// 同一个计划刚被链上明确拒收过：在它变化之前不再重复提交（不刷屏），等对账给出新计划。
		log.Debug("submitBtcHeadersOnce plan halted after an unrecoverable rejection", "plan", plan.key())
		return
	case btcHeaderSubmitNothingConfirmed:
		return
	case btcHeaderSubmitAlreadyInFlight:
		log.Debug("submitBtcHeadersOnce batch already in flight", "next", plan.nextSubmitHeight,
			"pendingTop", st.pendingTop)
		return
	}

	headers := n.buildBtcHeaderBatch(from, to, localTip)
	if len(headers) == 0 {
		return
	}
	top := headers[len(headers)-1].GetHeight()
	payload := &ltypes.BtcHeaders{Headers: headers}
	txHash, submitErr := n.submitMainchainTx(ltypes.LightclientX, ltypes.NameBtcHeadersAction, payload)
	result := classifyBtcHeaderSubmitErr(submitErr)

	switch result {
	case btcHeaderSubmitAccepted, btcHeaderSubmitDuplicateAccepted:
		duplicate := result == btcHeaderSubmitDuplicateAccepted
		st.finishSubmit(plan, from, top, now, result)
		// 提交被受理 = 提交侧的问题（临时失败/拒收/空转）已经过去，允许下次重新报告。
		st.submitReport.reset()
		if plan.rolledBack {
			log.Info("submitBtcHeadersOnce resubmitted after rollback", "from", from, "to", top,
				"matchedHeight", plan.matchedHeight, "chainTip", plan.chainTipHeight,
				"localTip", plan.localTipHeight, "probes", plan.probes, "txHash", txHash, "duplicate", duplicate)
		} else {
			log.Debug("submitBtcHeadersOnce submitted", "from", from, "to", top,
				"chainTip", plan.chainTipHeight, "txHash", txHash, "duplicate", duplicate)
		}
		// 反复把同一批当成新交易提交、链上 canonical 链却毫无推进：报一次（不停心跳，也不刷屏）。
		if st.futileSubmits >= maxBtcHeaderFutileSubmits && st.submitReport.allow("futile:"+plan.key()) {
			log.Error("submitBtcHeadersOnce the same batch keeps being resubmitted without advancing the "+
				"on-chain chain (the local chain may be lighter than the on-chain canonical chain, or chain33 is not producing blocks)",
				"from", from, "to", top, "chainTip", plan.chainTipHeight, "count", st.futileSubmits)
		}
	case btcHeaderSubmitRejected:
		// 确定性拒收：重试同一批永远不会成功，记下这个计划并显式报错（只报一次）。
		st.finishSubmit(plan, from, top, now, result)
		if st.submitReport.allow("rejected:" + submitErr.Error()) {
			log.Error("submitBtcHeadersOnce btc header batch rejected by chain33, stop resubmitting this batch",
				"from", from, "to", top, "plan", plan.key(), "err", submitErr)
		}
	default:
		// 可自愈：网络/主链未就绪/费用等，下一轮重试（同一个错只报一次，避免每轮刷屏）。
		if st.submitReport.allow("transient:" + submitErr.Error()) {
			log.Warn("submitBtcHeadersOnce submit btc headers failed, retry next round",
				"from", from, "to", top, "err", submitErr)
		}
	}
}

// depositWatcher 监听比特币充值交易, 并向chain33主链提交rgbx deposit交易
func (n *neutrinoClient) depositWatcher() {

	depositChan := n.bw.GetDepositChannel()
	retryTicker := time.NewTicker(time.Second * 30)
	var retryList []*btcPendingTx
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-retryTicker.C:
			temp := retryList
			retryList = retryList[:0]
			for _, pendingTx := range temp {
				if err := n.commitDepositTx(pendingTx); err != nil {
					log.Error("depositWatcher commitDepositTx retry", "txHash", pendingTx.txHash.String(), "err", err)
					retryList = append(retryList, pendingTx)
				}
			}
		case pendingTx := <-depositChan:

			// 分类排除防御（BL-5）：已知 RGB 交易的充值走 RGB20 路径，绝不经 BTC 路径双铸。
			if n.rgb20 != nil && n.rgb20.IsKnownRgbTxid(pendingTx.txHash.String()) {
				log.Debug("depositWatcher skip known rgb tx", "txHash", pendingTx.txHash.String())
				continue
			}
			// 如果chain33DepositAddress为空，则使用第一个输入的utxo地址
			if pendingTx.chain33DepositAddress == "" {
				firstInputUtxo := pendingTx.tx.TxIn[0].PreviousOutPoint
				pendingTx.chain33DepositAddress = rtypes.FormatUtxo(firstInputUtxo.Hash.String(), firstInputUtxo.Index)
			}
			if pendingTx.depositAmount <= 0 {
				log.Error("depositWatcher invalid deposit amount", "txHash", pendingTx.txHash.String(),
					"amount", pendingTx.depositAmount)
				continue
			}

			if err := n.commitDepositTx(pendingTx); err != nil {
				log.Error("depositWatcher commitDepositTx", "txHash", pendingTx.txHash.String(), "err", err)
				retryList = append(retryList, pendingTx)
			}
		}
	}
}
func (n *neutrinoClient) commitDepositTx(pendingTx *btcPendingTx) error {
	if state := n.getDepositState(pendingTx.txHash[:]); bytes.Equal(state, depositStatusProcessed) {
		log.Debug("commitDepositTx already processed", "txHash", pendingTx.txHash.String())
		n.bw.removePendingTx(pendingTx.txHash)
		return nil
	}
	// 分类排除防御（BL-5）：已知 RGB 交易由 RGB20 路径铸造，跳过 BTC 铸造。
	if n.rgb20 != nil && n.rgb20.IsKnownRgbTxid(pendingTx.txHash.String()) {
		log.Debug("commitDepositTx skip known rgb tx", "txHash", pendingTx.txHash.String())
		return nil
	}
	spv, err := n.bw.buildTxExistenceProof(pendingTx)
	if err != nil {
		log.Error("commitDepositTx buildTxExistenceProof", "txHash", pendingTx.txHash.String(), "err", err)
		return err
	}
	buf := bytes.NewBuffer(make([]byte, 0, pendingTx.tx.SerializeSizeStripped()))
	if err = pendingTx.tx.SerializeNoWitness(buf); err != nil {
		log.Error("commitDepositTx SerializeNoWitness", "txHash", pendingTx.txHash.String(), "err", err)
		return err
	}
	deposit := &rtypes.DepositAsset{
		Amount:         int64(pendingTx.depositAmount),
		DepositAddress: pendingTx.chain33DepositAddress,
		AssetSymbol:    rtypes.BTCSymbol,
		TxProof: &rtypes.BtcTxProof{
			TxData:      buf.Bytes(),
			BlockHash:   pendingTx.blockHash.String(),
			BlockHeight: uint64(pendingTx.blockHeight),
			TxIndex:     spv.GetTxIndex(),
			MerkleProof: spv.GetBranchProof(),
		},
	}
	n.submitMainchainTxUntilSuccess(rtypes.RgbxX, rtypes.NameDepositAssetAction, deposit)
	if err = n.setDepositState(pendingTx.txHash[:], depositStatusProcessed); err != nil {
		log.Error("commitDepositTx setDepositState processed", "txHash", pendingTx.txHash.String(), "err", err)
	}
	n.bw.removePendingTx(pendingTx.txHash)
	log.Debug("commitDepositTx submit deposit success", "btxHash", pendingTx.txHash.String(),
		"depositAddr", deposit.GetDepositAddress(), "amount", deposit.GetAmount())
	return nil
}

func pending2WithdrawRequest(chain33Pending *rtypes.PendingTx) *withdrawRequest {
	return &withdrawRequest{
		chain33WithDrawHash: chain33Pending.GetTxHash(),
		amount:              btcutil.Amount(chain33Pending.GetAmount()),
		feeRate:             btcutil.Amount(chain33Pending.GetFeeRate()),
		toAddress:           chain33Pending.GetTargetAddress(),
	}
}

func (n *neutrinoClient) processWithdrawRequest(req *withdrawRequest) (err error) {

	txHash := hex.EncodeToString(req.chain33WithDrawHash)
	tx, inputAmounts, lockedUTXOs, err := n.bw.buildWithdrawTx(req)
	if err != nil {
		log.Error("processWithdrawRequest buildWithdrawTx", "txHash", txHash, "err", err)
		return err
	}
	defer func() {
		if r := recover(); r != nil {
			log.Error("processWithdrawRequest panic", "err", err, "panic err", r)
			err = fmt.Errorf("processWithdrawRequest panic: %v", r)
		}
		if err != nil {
			n.bw.releaseUTXOsExcept(lockedUTXOs, req.stickyUTXO)
		}
	}()

	if len(lockedUTXOs) == 0 {
		return fmt.Errorf("no locked UTXOs")
	}
	lastUTXO := lockedUTXOs[len(lockedUTXOs)-1]
	expectedHash := n.getExpectedWithdrawHash(lastUTXO.OutPoint.String())
	if len(expectedHash) > 0 && !bytes.Equal(expectedHash, req.chain33WithDrawHash) {
		log.Error("processWithdrawRequest sticky input mismatch", "expected", hex.EncodeToString(expectedHash),
			"actual", txHash, "stickyOutPoint", lastUTXO.OutPoint.String())
		return fmt.Errorf("invalid sticky input")
	}

	// 提现交易构建后，则和最后一个utxo强绑定，后续不能更改
	if req.stickyUTXO == nil {
		if err = n.setWithdrawStickyUTXO(req.chain33WithDrawHash, lastUTXO); err != nil {
			log.Error("processWithdrawRequest setWithdrawStickyUTXO", "txHash", txHash, "stickyUTXO", lastUTXO.OutPoint.String(), "err", err)
			return err
		}
		req.stickyUTXO = lastUTXO
	}
	// 主节点也进行验证，保证各节点执行相同的逻辑
	err = n.tss.validateWithdrawTx(tx, inputAmounts, req)
	if err != nil {
		log.Error("processSignBtcTx validateWithdrawTx", "txHash", txHash, "err", err)
		return err
	}
	btcTxHash := tx.TxHash().String()
	if err = n.tss.processSignBtcTx(tx, transactionTypeWithdraw, inputAmounts, req.chain33WithDrawHash); err != nil {
		log.Error("processWithdrawRequest processSignBtcTx", "txHash", txHash, "btcTxHash", btcTxHash, "err", err)
		return err
	}

	if err = n.bw.broadcastTransaction(tx, btcTxHash); err != nil {
		log.Error("processWithdrawRequest broadcastTransaction", "txHash", txHash, "btcTxHash", btcTxHash, "err", err)
		return err
	}
	n.bw.addPendingTx(&btcPendingTx{
		tx:                    tx,
		submitTime:            types.Now(),
		confirmations:         0,
		blockHeight:           -1,
		txHash:                tx.TxHash(),
		txType:                transactionTypeWithdraw,
		withdrawAddress:       req.toAddress,
		chain33WithdrawTxHash: req.chain33WithDrawHash,
	})

	if setStateErr := n.setWithdrawState(req.chain33WithDrawHash, withdrawStatusSent); setStateErr != nil {
		log.Error("processWithdrawRequest setWithdrawState", "txHash", txHash, "btcTxHash", btcTxHash, "err", setStateErr)
		return setStateErr
	}
	log.Debug("processWithdrawRequest success", "withdrawTxHash", txHash, "btcTxHash", btcTxHash)
	return nil
}

type confirmWithdraw struct {
	btcPending          *btcPendingTx
	pendingTxBlockIndex *rtypes.TxBlockIndex
	confirmTx           *rtypes.ConfirmTx
}

var (
	withdrawStateBucket     = []byte("rgbx-withdraw-state")
	withdrawStatusSent      = []byte("broadcasted")
	withdrawStatusConfirmed = []byte("confirmed")
	// withdrawStatusUnrecoverable 该提现重试永远不可能成功（如侧车已无 pending 指向的资产），
	// 桥已停止重试并把它落盘：链上 pending 会保留（不自动退款），该状态即"停在这里"的凭据。
	withdrawStatusUnrecoverable = []byte("unrecoverable")
	withdrawStickyUTXOBucket    = []byte("rgbx-withdraw-sticky-utxo")

	depositStateBucket     = []byte("rgbx-deposit-state")
	depositStatusProcessed = []byte("processed")
)

func (n *neutrinoClient) getWithdrawState(chain33TxHash []byte) []byte {
	var data []byte
	err := walletdb.View(n.neutrinoCfg.Database, func(tx walletdb.ReadTx) error {
		bucket := tx.ReadBucket(withdrawStateBucket)
		if bucket == nil {
			return walletdb.ErrBucketNotFound
		}
		data = bucket.Get(chain33TxHash)
		return nil
	})
	if err != nil && !errors.Is(err, walletdb.ErrBucketNotFound) {
		log.Error("getWithdrawState", "txHash", hex.EncodeToString(chain33TxHash), "err", err)
	}
	return data
}

func (n *neutrinoClient) setWithdrawState(txHash []byte, status []byte) error {

	return walletdb.Update(n.neutrinoCfg.Database, func(tx walletdb.ReadWriteTx) error {
		bucket, err := tx.CreateTopLevelBucket(withdrawStateBucket)
		if err != nil {
			return err
		}
		return bucket.Put(txHash, status)
	})
}

func encodeOutPoint(op *wire.OutPoint) []byte {
	return []byte(op.String())
}

func decodeOutPoint(data []byte) (*wire.OutPoint, error) {
	return wire.NewOutPointFromString(string(data))
}

func (n *neutrinoClient) getWithdrawStickyUTXO(chain33TxHash []byte) *UTXO {
	var data []byte
	err := walletdb.View(n.neutrinoCfg.Database, func(tx walletdb.ReadTx) error {
		bucket := tx.ReadBucket(withdrawStickyUTXOBucket)
		if bucket == nil {
			return walletdb.ErrBucketNotFound
		}
		data = bucket.Get(chain33TxHash)
		return nil
	})
	if err != nil && !errors.Is(err, walletdb.ErrBucketNotFound) {
		log.Error("getWithdrawStickyUTXO", "txHash", hex.EncodeToString(chain33TxHash), "err", err)
		return nil
	}
	if len(data) == 0 {
		return nil
	}
	u := &rtypes.Utxo{}
	err = types.Decode(data, u)
	if err != nil {
		log.Error("getWithdrawStickyUTXO decode", "txHash", hex.EncodeToString(chain33TxHash), "err", err)
		return nil
	}
	outPoint, err := wire.NewOutPointFromString(u.OutPoint)
	if err != nil {
		log.Error("getWithdrawStickyUTXO NewOutPointFromString", "txHash", hex.EncodeToString(chain33TxHash), "err", err)
		return nil
	}
	return &UTXO{
		OutPoint: *outPoint,
		Amount:   btcutil.Amount(u.Amount),
		PkScript: u.PkScript,
	}
}

func (n *neutrinoClient) getExpectedWithdrawHash(outPoint string) []byte {
	var data []byte
	err := walletdb.View(n.neutrinoCfg.Database, func(tx walletdb.ReadTx) error {
		bucket := tx.ReadBucket(withdrawStickyUTXOBucket)
		if bucket == nil {
			return walletdb.ErrBucketNotFound
		}
		data = bucket.Get([]byte(outPoint))
		return nil
	})
	if err != nil && !errors.Is(err, walletdb.ErrBucketNotFound) {
		log.Error("getExpectedWithdrawHash", "outPoint", outPoint, "err", err)
		return nil
	}
	return data
}

func (n *neutrinoClient) setWithdrawStickyUTXO(chain33TxHash []byte, utxo *UTXO) error {
	u := &rtypes.Utxo{
		OutPoint: utxo.OutPoint.String(),
		Amount:   int64(utxo.Amount),
		PkScript: utxo.PkScript,
	}
	return walletdb.Update(n.neutrinoCfg.Database, func(tx walletdb.ReadWriteTx) error {
		bucket, err := tx.CreateTopLevelBucket(withdrawStickyUTXOBucket)
		if err != nil {
			return err
		}
		err = bucket.Put([]byte(utxo.OutPoint.String()), chain33TxHash)
		if err != nil {
			return err
		}
		return bucket.Put(chain33TxHash, types.Encode(u))
	})
}

func (n *neutrinoClient) clearWithdrawStickyUTXO(chain33TxHash []byte) error {
	return walletdb.Update(n.neutrinoCfg.Database, func(tx walletdb.ReadWriteTx) error {
		bucket := tx.ReadWriteBucket(withdrawStickyUTXOBucket)
		if bucket == nil {
			return nil
		}
		return bucket.Delete(chain33TxHash)
	})
}

func (n *neutrinoClient) getDepositState(txHash []byte) []byte {
	var data []byte
	err := walletdb.View(n.neutrinoCfg.Database, func(tx walletdb.ReadTx) error {
		bucket := tx.ReadBucket(depositStateBucket)
		if bucket == nil {
			return walletdb.ErrBucketNotFound
		}
		data = bucket.Get(txHash)
		return nil
	})
	if err != nil && !errors.Is(err, walletdb.ErrBucketNotFound) {
		log.Error("getDepositState", "txHash", hex.EncodeToString(txHash), "err", err)
	}
	return data
}

func (n *neutrinoClient) setDepositState(txHash []byte, status []byte) error {
	return walletdb.Update(n.neutrinoCfg.Database, func(tx walletdb.ReadWriteTx) error {
		bucket, err := tx.CreateTopLevelBucket(depositStateBucket)
		if err != nil {
			return err
		}
		return bucket.Put(txHash, status)
	})
}

func (n *neutrinoClient) getPendingTxBlockIndex(txHash []byte) *rtypes.TxBlockIndex {
	hashStr := hex.EncodeToString(txHash)
	pendingTx := n.rgbx.pendingCache.getTx(hashStr)
	if pendingTx != nil {
		return &rtypes.TxBlockIndex{
			BlockHeight: pendingTx.TxBlockHeight,
			TxIndex:     pendingTx.TxIndex,
		}
	}
	txDetail, err := n.getTxDetail(txHash)
	if err != nil {
		log.Error("getPendingTxBlockIndex getTxDetail", "txHash", hashStr, "err", err)
		return nil
	}
	txBlockIndex := &rtypes.TxBlockIndex{BlockHeight: txDetail.GetHeight(), TxIndex: txDetail.GetIndex()}
	return txBlockIndex
}

// isRgb20Asset 判断资产符号是否为已注册的 RGB20 资产。
func (n *neutrinoClient) isRgb20Asset(symbol string) bool {
	return n.rgb20 != nil && n.rgb20.Registry().IsRegistered(symbol)
}

// processRgb20Withdraw 将 RGB20 提现 pending 交易路由到 rgb20 适配器（Phase 4 接线）。
// 提现全程由 Adapter.Withdraw 内部 mutex 串行；失败返回 error 由调用方加入重试列表。
func (n *neutrinoClient) processRgb20Withdraw(pending *rtypes.PendingTx) error {
	req := &rgb20.WithdrawRequest{
		Chain33TxHash:    pending.GetTxHash(),
		Amount:           pending.GetAmount(),
		FeeRate:          pending.GetFeeRate(),
		RecipientInvoice: pending.GetTargetAddress(),
		AssetSymbol:      pending.GetAssetSymbol(),
		TxBlockHeight:    pending.GetTxBlockHeight(),
	}
	_, err := n.rgb20.WithdrawFlow(n.ctx, req)
	return err
}

// retryRgb20Withdraw 处理/重试一笔 RGB20 提现，返回"是否应继续重试"。
//
// 不可恢复的失败（pending 指向的资产侧车已不存在，见 rgb20.IsUnrecoverableWithdraw）在这里
// 终止重试：把 withdrawStatusUnrecoverable 落盘（可查询的终态）、只报一次 ERROR 日志，并让
// 调用方把它移出重试队列。链上 pending 不动 —— 桥不做自动退款，已锁仓资产如何处置属产品决策。
func (n *neutrinoClient) retryRgb20Withdraw(p *rtypes.PendingTx, logMsg string) bool {
	err := n.processRgb20Withdraw(p)
	if err == nil {
		return false
	}
	txHash := hex.EncodeToString(p.GetTxHash())
	if class, unrecoverable := rgb20.IsUnrecoverableWithdraw(err); unrecoverable {
		if setErr := n.setWithdrawState(p.GetTxHash(), withdrawStatusUnrecoverable); setErr != nil {
			log.Error("withdrawalProcessor setWithdrawState unrecoverable", "txHash", txHash, "err", setErr)
		}
		log.Error("withdrawalProcessor rgb20 withdraw UNRECOVERABLE stop retrying",
			"txHash", txHash, "assetSymbol", p.GetAssetSymbol(), "class", class, "err", err)
		return false
	}
	log.Error(logMsg, "txHash", txHash, "err", err)
	return true
}

// withdrawalProcessor 监听chain33主链上比特币提现请求, 构造提现交易到比特币网络，并向chain33主链提交rgbx confirm交易
func (n *neutrinoClient) withdrawalProcessor() {
	withdrawalChan := n.bw.GetWithdrawChannel()
	// 根据配置，同时兼容测试和正式环境
	retryTicker := time.NewTicker(time.Second * time.Duration(n.cfg.BtcBlockInterval/15+1))
	defer retryTicker.Stop()
	withdrawReqChan := n.withdrawReqChan
	withdrawReqList := make([]*withdrawRequest, 0, 16)
	rgb20WithdrawRetry := make([]*rtypes.PendingTx, 0, 16)
	confirmRetryList := make([]*confirmWithdraw, 0, 16)

	for {
		select {
		case <-n.ctx.Done():
			return
		case <-retryTicker.C:
			tempWithdraws := withdrawReqList
			withdrawReqList = withdrawReqList[:0]
			for _, p := range tempWithdraws {
				if err := n.processWithdrawRequest(p); err != nil {
					withdrawReqList = append(withdrawReqList, p)
				}
			}
			tempRgb20 := rgb20WithdrawRetry
			rgb20WithdrawRetry = rgb20WithdrawRetry[:0]
			for _, p := range tempRgb20 {
				if n.retryRgb20Withdraw(p, "withdrawalProcessor rgb20 retry") {
					rgb20WithdrawRetry = append(rgb20WithdrawRetry, p)
				}
			}
			tempConfirms := confirmRetryList
			confirmRetryList = confirmRetryList[:0]
			for _, p := range tempConfirms {
				if !n.processWithdrawConfirm(p) {
					confirmRetryList = append(confirmRetryList, p)
				}
			}
		case pendingBtc := <-withdrawalChan: // 向chain33主链提交确认提现交易
			confirm := &confirmWithdraw{
				btcPending: pendingBtc,
			}
			if !n.processWithdrawConfirm(confirm) {
				confirmRetryList = append(confirmRetryList, confirm)
			}

		case pending := <-withdrawReqChan: // 向btc链提交提现交易
			state := n.getWithdrawState(pending.GetTxHash())
			if len(state) > 0 && bytes.Equal(state, withdrawStatusConfirmed) {
				log.Debug("withdrawalProcessor hasWithdrawState", "txHash", hex.EncodeToString(pending.GetTxHash()), "state", string(state))
				continue
			}
			// 已判定不可恢复的提现（见下方 processRgb20Withdraw 分支）：链上 pending 因为不做
			// 自动退款而仍然存在，pullPendingTx 每次全量重扫（StartHeight 恒为 0）都会重新投递它，
			// 这里按落盘状态短路，避免每轮重扫都重试一次、无限刷屏。
			if bytes.Equal(state, withdrawStatusUnrecoverable) {
				log.Debug("withdrawalProcessor skip unrecoverable withdraw", "txHash", hex.EncodeToString(pending.GetTxHash()))
				continue
			}
			// RGB20 提现：路由到 rgb20 适配器（invoice→BuildWithdrawal→TSS signPsbt→Finalize→广播→txid↔pending）。
			if n.isRgb20Asset(pending.GetAssetSymbol()) {
				log.Debug("withdrawalProcessor rgb20 withdraw", "txHash", hex.EncodeToString(pending.GetTxHash()),
					"assetSymbol", pending.GetAssetSymbol())
				if n.retryRgb20Withdraw(pending, "withdrawalProcessor rgb20 withdraw") {
					rgb20WithdrawRetry = append(rgb20WithdrawRetry, pending)
				}
				continue
			}
			req := pending2WithdrawRequest(pending)
			req.stickyUTXO = n.getWithdrawStickyUTXO(pending.GetTxHash())
			// 状态非空时，存在stickyUTXO才允许重试
			if len(state) > 0 && req.stickyUTXO == nil {
				log.Debug("withdrawalProcessor hasWithdrawState but no stickyUTXO", "txHash", hex.EncodeToString(pending.GetTxHash()), "state", string(state))
				continue
			}
			if err := n.processWithdrawRequest(req); err != nil {
				withdrawReqList = append(withdrawReqList, req)
			}
		}
	}
}

func (n *neutrinoClient) commitWithdrawConfirm(confirm *rtypes.ConfirmTx, confirmHash string) (string, error) {

	txHash, err := n.submitMainchainTx(rtypes.RgbxX, rtypes.NameConfirmAction, confirm)
	if err != nil && !strings.Contains(err.Error(), "already confirmed") {
		return "", err
	}
	n.rgbx.pendingCache.removeTx(confirmHash)
	if err := n.setWithdrawState(confirm.TxHash, withdrawStatusConfirmed); err != nil {
		log.Error("commitWithdrawConfirm setWithdrawState", "txHash", txHash,
			"confirmHash", confirmHash, "err", err)
	}
	return txHash, nil
}

func (n *neutrinoClient) buildWithdrawConfirm(btcPending *btcPendingTx, pendingTxBlockIndex *rtypes.TxBlockIndex) *rtypes.ConfirmTx {
	if pendingTxBlockIndex == nil {
		return nil
	}
	spv, err := n.bw.buildTxExistenceProof(btcPending)
	if err != nil {
		log.Error("buildWithdrawConfirmPayload buildTxExistenceProof", "btcTxHash", btcPending.txHash.String(),
			"chain33WithdrawTxHash", hex.EncodeToString(btcPending.chain33WithdrawTxHash), "err", err)
		return nil
	}
	buf := bytes.NewBuffer(make([]byte, 0, btcPending.tx.SerializeSizeStripped()))
	if err = btcPending.tx.SerializeNoWitness(buf); err != nil {
		log.Error("buildWithdrawConfirmPayload SerializeNoWitness", "btcTxHash", btcPending.txHash.String(),
			"chain33WithdrawTxHash", hex.EncodeToString(btcPending.chain33WithdrawTxHash), "err", err)
		return nil
	}
	minPendingHeight := n.rgbx.pendingCache.getMinPendingHeight()
	return &rtypes.ConfirmTx{
		ActionType:           rtypes.TyWithDrawAsset,
		ConfirmedBlockHeight: minPendingHeight - 1,
		TxBlockHeight:        pendingTxBlockIndex.GetBlockHeight(),
		TxIndex:              pendingTxBlockIndex.GetTxIndex(),
		TxHash:               btcPending.chain33WithdrawTxHash,
		BtcTxProof: &rtypes.BtcTxProof{
			TxData:      buf.Bytes(),
			BlockHash:   btcPending.blockHash.String(),
			BlockHeight: uint64(btcPending.blockHeight),
			TxIndex:     spv.GetTxIndex(),
			MerkleProof: spv.GetBranchProof(),
		},
	}
}

func (n *neutrinoClient) processWithdrawConfirm(confirm *confirmWithdraw) bool {

	// 已知 RGB20 提现交易但缺 chain33 关联（txid↔pending 映射缺失）→ 直接丢弃，
	// 避免 getPendingTxBlockIndex("") 对空 hash 反复查询造成死循环（HR-2）。
	if confirm.btcPending != nil && n.rgb20 != nil &&
		n.rgb20.IsKnownRgbTxid(confirm.btcPending.txHash.String()) &&
		len(confirm.btcPending.chain33WithdrawTxHash) == 0 {
		log.Error("processWithdrawConfirm rgb20 withdraw missing chain33 mapping",
			"btcTxid", confirm.btcPending.txHash.String())
		n.bw.removePendingTx(confirm.btcPending.txHash)
		return true
	}

	if confirm.pendingTxBlockIndex == nil {
		confirm.pendingTxBlockIndex = n.getPendingTxBlockIndex(confirm.btcPending.chain33WithdrawTxHash)
	}

	if confirm.confirmTx == nil {
		confirm.confirmTx = n.buildWithdrawConfirm(confirm.btcPending, confirm.pendingTxBlockIndex)
	}
	if confirm.confirmTx == nil {
		return false
	}
	confirmHash := hex.EncodeToString(confirm.confirmTx.TxHash)
	if state := n.getWithdrawState(confirm.confirmTx.GetTxHash()); bytes.Equal(state, withdrawStatusConfirmed) {
		n.rgbx.pendingCache.removeTx(confirmHash)
		n.bw.removePendingTx(confirm.btcPending.txHash)
		if err := n.clearWithdrawStickyUTXO(confirm.confirmTx.GetTxHash()); err != nil {
			log.Error("processWithdrawConfirm clearWithdrawFirstInput", "confirmHash", confirmHash, "err", err)
		}
		log.Debug("processWithdrawConfirm already confirmed local state", "confirmHash", confirmHash)
		return true
	}

	txHash, err := n.commitWithdrawConfirm(confirm.confirmTx, confirmHash)
	if err != nil {
		log.Error("processWithdrawConfirm commitWithdrawConfirm", "txHash", txHash, "confirmHash", confirmHash, "err", err)
		return false
	}
	n.bw.removePendingTx(confirm.btcPending.txHash)
	if err := n.clearWithdrawStickyUTXO(confirm.confirmTx.GetTxHash()); err != nil {
		log.Error("processWithdrawConfirm clearWithdrawFirstInput", "confirmHash", confirmHash, "err", err)
	}
	log.Debug("processWithdrawConfirm success", "txHash", txHash,
		"btcTxHash", confirm.btcPending.txHash.String(), "confirmHash", confirmHash)
	return true

}
