package executor

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/33cn/chain33/common/merkle"
	"github.com/33cn/chain33/types"
	ltypes "github.com/33cn/plugin/plugin/dapp/lightclient/lighttypes"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

const withdrawCommitmentPrefix = "rgbx:withdraw:"
const depositCommitmentPrefix = "rgbx:deposit:"

func btcProof2String(txProof *rtypes.BtcTxProof) string {
	return fmt.Sprintf("btcBlockHeight: %d, btcBlockHash: %s, btcTxIndex: %d, btcTxData: %s",
		txProof.GetBlockHeight(), txProof.GetBlockHash(),
		txProof.GetTxIndex(), hex.EncodeToString(txProof.GetTxData()))
}

func merkelProof2String(merkleProof [][]byte) string {
	result := ""
	for _, proof := range merkleProof {
		result += hex.EncodeToString(proof) + "|"
	}
	return result
}

func (r *rgbx) checkWithdrawConfirm(txHash, confirmHash string, confirm *rtypes.ConfirmTx, pendingTx *rtypes.PendingTx) error {
	// S3：同一笔提现 burn 只允许结算（放款）一次。已消费则直接拒绝。
	// 该守卫落在合约级 statedb（共识状态）上，而不是 LocalDB 的 pending.Confirmed 标记：
	// dapp CheckTx 在出块执行阶段会被每个节点重跑（chain33 executor/execenv.go Exec → CheckTx），
	// 读节点私有数据会让"区块是否合法"依赖各节点本地库，且本地库丢失/落后时同一 burn 会被二次结算
	// （第二次再从共享锁仓地址销毁一遍，侵蚀其他 pending 提现的锁定额度）。与充值侧 formatDepositUsedTxIDKey 对称。
	_, err := r.GetStateDB().Get(formatWithdrawUsedKey(confirm.GetTxHash()))
	if !errors.Is(err, types.ErrNotFound) {
		elog.Error("checkWithdrawConfirm burn already used", "txHash", txHash, "confirmHash", confirmHash,
			"burnTxHash", hex.EncodeToString(confirm.GetTxHash()), "err", err)
		return ErrWithdrawAlreadyConfirmed
	}
	btcTx, err := r.validateBtcTxProof(txHash, confirm.GetBtcTxProof())
	if err != nil {
		elog.Error("checkWithdrawConfirm validate btc tx proof", "txHash", txHash, "confirmHash", confirmHash,
			"btcProof", btcProof2String(confirm.GetBtcTxProof()), "err", err)
		return err
	}
	// RGB20 分支：跳过链上 OP_RETURN 承诺与链上金额校验（H4：提现 correlation 走 txid↔pending 映射）。
	if rtypes.IsRgb20Symbol(pendingTx.GetAssetSymbol()) {
		return nil
	}
	if !hasWithdrawCommitment(btcTx, confirm.GetTxHash()) {
		elog.Error("checkWithdrawConfirm commitment mismatch", "txHash", txHash, "confirmHash", confirmHash,
			"btcProof", btcProof2String(confirm.GetBtcTxProof()))
		return ErrInvalidBtcProofCommitment
	}
	if err = r.validateWithdrawTxContent(txHash, pendingTx, btcTx); err != nil {
		return err
	}
	return nil
}

func (r *rgbx) validateDepositTxContent(txHash string, deposit *rtypes.DepositAsset, btcTx *wire.MsgTx) error {
	// RGB20 分支：全跳过链上金额（RGB 金额在 consignment 内）。
	if rtypes.IsRgb20Symbol(deposit.GetAssetSymbol()) {
		return nil
	}
	info, err := r.getCrossChainInfo(deposit.GetAssetSymbol())
	if err != nil {
		elog.Error("validateDepositTxContent getCrossChainInfo", "txHash", txHash, "symbol", deposit.GetAssetSymbol(), "err", err)
		return ErrGetCrossChainInfo
	}
	var amount int64
	for _, out := range btcTx.TxOut {
		if bytes.Equal(out.PkScript, info.GetPkScript()) {
			amount += out.Value
		}
	}
	if amount != deposit.GetAmount() {
		elog.Error("validateDepositTxContent amount mismatch", "txHash", txHash,
			"expect", deposit.GetAmount(), "actual", amount)
		return ErrInvalidDepositAmount
	}
	return nil
}

func (r *rgbx) validateWithdrawTxContent(txHash string, pendingTx *rtypes.PendingTx, btcTx *wire.MsgTx) error {
	if pendingTx == nil {
		return ErrPendingTxNotExist
	}
	// RGB20 分支：全跳过链上金额。
	if rtypes.IsRgb20Symbol(pendingTx.GetAssetSymbol()) {
		return nil
	}
	info, err := r.getCrossChainInfo(pendingTx.GetAssetSymbol())
	if err != nil {
		elog.Error("validateWithdrawTxContent getCrossChainInfo", "txHash", txHash, "symbol", pendingTx.GetAssetSymbol(), "err", err)
		return ErrInvalidCrossChainInfo
	}
	destScript, err := r.decodeBtcAddressScript(pendingTx.GetTargetAddress())
	if err != nil {
		elog.Error("validateWithdrawTxContent decode target address", "txHash", txHash, "address", pendingTx.GetTargetAddress(), "err", err)
		return ErrInvalidWithdrawDestination
	}
	var destAmount int64
	for _, out := range btcTx.TxOut {
		if len(out.PkScript) > 0 && out.PkScript[0] == txscript.OP_RETURN {
			continue
		}
		if bytes.Equal(out.PkScript, destScript) {
			destAmount += out.Value
			continue
		}
		if !bytes.Equal(out.PkScript, info.GetPkScript()) {
			elog.Error("validateWithdrawTxContent unexpected output script", "txHash", txHash, "script", hex.EncodeToString(out.PkScript))
			return ErrInvalidWithdrawDestinationScript
		}
	}
	if destAmount <= 0 || destAmount > pendingTx.GetAmount() {
		elog.Error("validateWithdrawTxContent dest amount invalid", "txHash", txHash,
			"destAmount", destAmount, "expectAmount", pendingTx.GetAmount())
		return ErrInvalidWithdrawAmount
	}

	return nil
}

func (r *rgbx) getCrossChainInfo(symbol string) (*rtypes.CrossChainInfo, error) {
	if symbol == "" {
		symbol = rtypes.BTCSymbol
	}
	info := &rtypes.CrossChainInfo{}
	err := readDB(r.GetStateDB(), formatCrossChainInfoKey(symbol), info)
	return info, err
}

func (r *rgbx) decodeBtcAddressScript(addr string) ([]byte, error) {
	if addr == "" {
		return nil, types.ErrInvalidAddress
	}
	netName, err := r.getBtcNetName()
	if err != nil {
		return nil, err
	}
	params := ltypes.GetBtcChainParams(netName)
	decoded, err := btcutil.DecodeAddress(addr, params)
	if err != nil {
		return nil, err
	}
	return txscript.PayToAddrScript(decoded)
}

// parseBtcTxIDStrict 严格解析 BtcTxProof.TxData 并返回解析后交易的 txid（规范身份）。
// 严格 = 解析后 reader 必须被完整消费（r.Len() == 0），任何尾部多余字节都直接拒绝。
//
// E1 修复（A）：btcwire 的 DeserializeNoWitness 只按需读取、不校验 reader 是否耗尽，
// 给一笔已上链交易追加任意尾部字节后解析结果完全相同（txid、输出、OP_RETURN 承诺、金额都不变），
// 只有原始字节变了 —— 这让所有"按原始字节取哈希"的身份判断都可被绕过（充值防双铸首当其冲）。
func parseBtcTxIDStrict(txHash string, txData []byte) ([]byte, error) {
	if len(txData) == 0 {
		elog.Error("parseBtcTxIDStrict empty btc tx data", "txHash", txHash)
		return nil, ErrInvalidBtcTxProof
	}
	reader := bytes.NewReader(txData)
	var btcTx wire.MsgTx
	if err := btcTx.DeserializeNoWitness(reader); err != nil {
		elog.Error("parseBtcTxIDStrict decode btc tx", "txHash", txHash, "err", err)
		return nil, ErrInvalidBtcTxProof
	}
	if reader.Len() != 0 {
		elog.Error("parseBtcTxIDStrict trailing bytes after btc tx", "txHash", txHash,
			"txDataLen", len(txData), "trailingLen", reader.Len())
		return nil, ErrInvalidBtcTxProof
	}
	txID := btcTx.TxHash()
	return txID.CloneBytes(), nil
}

func (r *rgbx) validateBtcTxProof(txHash string, proof *rtypes.BtcTxProof) (*wire.MsgTx, error) {
	if proof == nil || len(proof.GetTxData()) == 0 {
		elog.Error("validateBtcTxProof empty btc proof", "txHash", txHash)
		return nil, ErrInvalidBtcTxProof
	}
	var btcTx wire.MsgTx
	if err := btcTx.DeserializeNoWitness(bytes.NewReader(proof.GetTxData())); err != nil {
		elog.Error("validateBtcTxProof decode btc tx", "txHash", txHash, "err", err)
		return nil, ErrInvalidBtcTxProof
	}

	blockHashStr := proof.GetBlockHash()
	header, err := r.getBtcHeader(proof.GetBlockHeight())
	if err != nil {
		elog.Error("validateBtcTxProof get btc header", "txHash", txHash,
			"blockHash", blockHashStr, "height", proof.GetBlockHeight(), "err", err)
		return nil, ErrGetBtcHeader
	}
	if header.GetHash() != blockHashStr || header.GetHeight() != proof.GetBlockHeight() {
		elog.Error("validateBtcTxProof header mismatch", "txHash", txHash,
			"expectHash", blockHashStr, "actualHash", header.GetHash(),
			"expectHeight", proof.GetBlockHeight(), "actualHeight", header.GetHeight())
		return nil, ErrInvalidBtcProofBlock
	}

	txID := btcTx.TxHash()
	merkleRoot := merkle.GetMerkleRootFromBranch(proof.GetMerkleProof(), txID.CloneBytes(), proof.GetTxIndex())
	headerMerkleRoot, err := chainhash.NewHashFromStr(header.GetMerkleRoot())
	if err != nil {
		elog.Error("validateBtcTxProof invalid header merkleRoot", "txHash", txHash,
			"merkleRoot", header.GetMerkleRoot(), "err", err)
		return nil, ErrInvalidBtcProofMerkle
	}
	if !bytes.Equal(merkleRoot, headerMerkleRoot.CloneBytes()) {
		elog.Error("validateBtcTxProof merkle root not match", "txHash", txHash,
			"expectMerkleRoot", header.GetMerkleRoot(), "actualMerkleRoot", hex.EncodeToString(merkleRoot),
			"merkleProof", merkelProof2String(proof.GetMerkleProof()))
		return nil, ErrInvalidBtcProofMerkle
	}
	return &btcTx, nil
}

func (r *rgbx) getBtcNetName() (string, error) {
	msg, err := r.GetAPI().Query(ltypes.LightclientX, "GetBtcNetName", &types.ReqNil{})
	if err != nil {
		elog.Error("getBtcNetName query", "err", err)
		return "", err
	}
	return msg.(*types.ReplyString).Data, nil
}

func (r *rgbx) getBtcHeader(height uint64) (*ltypes.BtcHeader, error) {

	msg, err := r.GetAPI().Query(ltypes.LightclientX, "GetBtcHeader", &ltypes.ReqGetBtcHeader{Height: height})
	if err != nil {
		elog.Error("getBtcHeader query", "height", height, "err", err)
		return nil, err
	}
	header, ok := msg.(*ltypes.BtcHeader)
	if !ok || header == nil {
		elog.Error("getBtcHeader invalid header", "height", height)
		return nil, types.ErrInvalidParam
	}
	return header, nil
}

func hasWithdrawCommitment(tx *wire.MsgTx, chain33TxHash []byte) bool {
	expectData := append([]byte(withdrawCommitmentPrefix), chain33TxHash...)
	return hasExpectedOpReturnData(tx, expectData)
}

func hasDepositCommitment(tx *wire.MsgTx, depositAddress string) bool {
	if rtypes.IsUtxoAddress(depositAddress) {
		if len(tx.TxIn) > 0 {
			firstInputUtxo := tx.TxIn[0].PreviousOutPoint
			return depositAddress == rtypes.FormatUtxo(firstInputUtxo.Hash.String(), firstInputUtxo.Index)
		}
		return false
	}
	expectData := append([]byte(depositCommitmentPrefix), []byte(depositAddress)...)
	return hasExpectedOpReturnData(tx, expectData)
}

func hasExpectedOpReturnData(tx *wire.MsgTx, expectData []byte) bool {
	expectScript, err := txscript.NullDataScript(expectData)
	if err != nil {
		elog.Error("hasExpectedOpReturnData null data script", "err", err)
		return false
	}
	for _, out := range tx.TxOut {
		if len(out.PkScript) == 0 || out.PkScript[0] != txscript.OP_RETURN {
			continue
		}
		if bytes.Equal(out.PkScript, expectScript) {
			return true
		}
	}
	return false
}
