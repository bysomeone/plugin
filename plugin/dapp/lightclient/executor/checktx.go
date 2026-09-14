package executor

import (
	"encoding/hex"
	"errors"

	"github.com/33cn/chain33/types"
	ltypes "github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
	"github.com/btcsuite/btcd/blockchain"
)

// CheckTx 实现自定义检验交易接口，供框架调用
func (l *lightclient) CheckTx(tx *types.Transaction, index int) error {

	action := &ltypes.LightClientAction{}

	err := types.Decode(tx.GetPayload(), action)
	if err != nil {
		elog.Error("CheckTx", "txHash", hex.EncodeToString(tx.Hash()), "Decode payload error", err)
		return ErrDecodeAction
	}

	if action.Ty == ltypes.TyBtcHeadersAction {

		err = l.checkBtcHeaders(tx, action.GetBtcHeaders())
	} else {
		err = types.ErrActionNotSupport
	}
	if err != nil {
		elog.Error("CheckTx", "txHash", hex.EncodeToString(tx.Hash()), "actionName", tx.ActionName(), "err", err)
	}
	return err
}

// maxBtcHeadersPerTx 单笔交易允许提交的最大 BTC 头数量。
// 中继（neutrino submitBitcoinHeaders）自身 batchSize=16，这里留 4 倍余量；
// 上限的作用是把"一笔交易把区块/mempool 灌满"的成本固定下来——原先只校验 >= 1，没有上界。
const maxBtcHeadersPerTx = 64

func (l *lightclient) checkBtcHeaders(tx *types.Transaction, headers *ltypes.BtcHeaders) error {

	if lightCfg.CommitAddress != "" && tx.From() != lightCfg.CommitAddress {

		elog.Error("checkBtcHeaders", "from", tx.From(), "configAddress", lightCfg.CommitAddress)
		return ErrIllegalCommitAddress
	}

	prevHeader, err := getBtcLastHeader(l.GetStateDB())
	if err != nil {
		elog.Error("checkBtcHeaders", "getBtcLastHeader err", err)
		return ErrBtcGetLastHeader
	}

	list := headers.GetHeaders()
	if len(list) < 1 {
		elog.Error("checkBtcHeaders", "err", "commit empty headers")
		return types.ErrInvalidParam
	}
	if len(list) > maxBtcHeadersPerTx {
		elog.Error("checkBtcHeaders", "err", "too many headers in one tx", "count", len(list), "max", maxBtcHeadersPerTx)
		return ErrBtcHeadersTooMany
	}
	// 批内高度必须"逐个 +1"：同一高度重复提交（重放/灌水）与跳高都在这里挡住。
	// 首个头的高度不在此约束（bootstrap 允许从任意高度起，其合法性由 checkBootstrapAnchor 负责）。
	for i, h := range list {
		if h.GetHash() == "" {
			elog.Error("checkBtcHeaders nil header", "index", i)
			return types.ErrInvalidParam
		}
		if i > 0 && h.GetHeight() != list[i-1].GetHeight()+1 {
			elog.Error("checkBtcHeaders", "err", "header heights are not strictly increasing by 1",
				"prevHeight", list[i-1].GetHeight(), "height", h.GetHeight(), "index", i)
			return ErrBtcHeaderDuplicateHeight
		}
	}

	params := ltypes.GetBtcChainParams(lightCfg.BtcNetName)
	chainCtx := newBtcChainContext(params)
	timeSource := blockchain.NewMedianTime()
	isBootstrap := prevHeader == nil || prevHeader.GetHash() == ""
	var prevCtx blockchain.HeaderCtx
	if !isBootstrap {
		prevCtx = newBtcHeaderContext(prevHeader, nil, l.GetLocalDB())
	} else if err = checkBootstrapAnchor(list[0], params, l.GetLocalDB()); err != nil {
		// 链上还没有任何 BTC 头：首个头必须锚定到真实链（创世/已知锚点/localDB 可回溯），
		// 否则任何人都能自造一条短链作为头链起点（见 checkBootstrapAnchor）。
		elog.Error("checkBtcHeaders bootstrap anchor reject", "height", list[0].GetHeight(),
			"hash", list[0].GetHash(), "prevHash", list[0].GetPreviousHash(), "err", err)
		return err
	}

	for _, h := range list {

		// 首次提交也要保证本批 headers 内部严格连续；仅首个header允许无前置锚点。
		if prevHeader.GetHash() != "" && (prevHeader.Height+1 != h.GetHeight() || prevHeader.Hash != h.PreviousHash) {
			elog.Error("checkBtcHeaders", "prevHeight", prevHeader.Height, "prevHash", prevHeader.Hash,
				"commitHeight", h.GetHeight(), "commitPrevHash", h.GetPreviousHash())
			return ErrBtcHeaderDisorder
		}

		btcHeader, err := toWireHeader(h)
		if err != nil {
			elog.Error("checkBtcHeaders", "height", h.GetHeight(), "hash", h.GetHash(), "toWireHeader err", err)
			return ErrToBtcWireHeader
		}
		hash := btcHeader.BlockHash()
		if hash.String() != h.Hash {
			elog.Error("checkBtcHeaders", "expectHash", hash, "height", h.GetHeight(), "hash", h.GetHash(), "err", err)
			return ErrInvalidBtcBlockHash
		}

		if err = blockchain.CheckBlockHeaderSanity(btcHeader, params.PowLimit, timeSource, blockchain.BFNone); err != nil {
			elog.Error("checkBtcHeaders CheckBlockHeaderSanity", "height", h.GetHeight(), "hash", h.GetHash(), "err", err)
			return mapBtcHeaderVerifyErr(err)
		}
		// 首次提交时(prevCtx=nil)无法获取前置上下文，仅首个header跳过context校验。
		if prevCtx != nil {
			// 首次导入且刚好在难度调整点，如果缺少历史祖先，则仅校验sanity与顺序。
			if isBootstrap && !canValidateHeaderContext(prevCtx, chainCtx) {
				prevHeader = h
				prevCtx = newBtcHeaderContext(h, prevCtx, l.GetLocalDB())
				continue
			}
			// skipCheckpoint=false：启用锚点表校验（见 btcChainContext.VerifyCheckpoint）。
			chainCtx.setCheckHeight(int32(h.GetHeight()))
			if err = blockchain.CheckBlockHeaderContext(btcHeader, prevCtx, blockchain.BFNone, chainCtx, false); err != nil {
				// regtest 批量导入场景下，连续快速挖块可能出现同秒时间戳。
				err = mapBtcHeaderVerifyErr(err)
				if !(lightCfg.AllowRegtestTimeWarp && lightCfg.BtcNetName == "regtest" && err == ErrBtcHeaderTimeTooOld) {
					elog.Error("checkBtcHeaders CheckBlockHeaderContext", "height", h.GetHeight(), "hash", h.GetHash(), "err", err)
					return err
				}
			}
		}
		prevHeader = h
		prevCtx = newBtcHeaderContext(h, prevCtx, l.GetLocalDB())

	}

	return nil

}

func mapBtcHeaderVerifyErr(err error) error {
	var ruleErr blockchain.RuleError
	if errors.As(err, &ruleErr) {
		switch ruleErr.ErrorCode {
		case blockchain.ErrUnexpectedDifficulty:
			return ErrBtcTargetBits
		case blockchain.ErrTimeTooOld:
			return ErrBtcHeaderTimeTooOld
		case blockchain.ErrTimeTooNew:
			return ErrBtcHeaderTimeTooNew
		case blockchain.ErrInvalidTime:
			return ErrBtcHeaderInvalidTime
		}
	}
	return ErrBtcHeaderVerify
}

func canValidateHeaderContext(prevCtx blockchain.HeaderCtx, chainCtx *btcChainContext) bool {
	if prevCtx == nil {
		return false
	}
	if (prevCtx.Height()+1)%chainCtx.BlocksPerRetarget() != 0 {
		return true
	}
	return prevCtx.RelativeAncestorCtx(chainCtx.BlocksPerRetarget()-1) != nil
}
