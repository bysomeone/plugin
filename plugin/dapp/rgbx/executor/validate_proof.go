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

/*
 * 充值绑定：P2WSH 派生脚本（硬切，v1）
 *
 * 非 RGB20（BTC/XBTC）的充值绑定 = "这笔 BTC 交易付给了 (depositAddress, tssPub) 派生出的
 * P2WSH program，且金额相符"。派生由 rtypes.DeriveDepositPkScript 给出，逐字节遵循冻结规格
 * （plugin/dapp/rgbx/types/p2wsh_deposit.go，测试向量 testdata/p2wsh_deposit_vectors.json）。
 *
 * 为什么"去掉 OP_RETURN 承诺"仍然安全（后来者别以为 OP_RETURN 是可有可无的冗余）：
 * 派生是确定性的、且 userID（= chain33 充值地址串）是原像的一部分 ——
 *   - 想领别人那笔付款：得让 program 命中别人的 userID，做不可行（要 sha256 原像）；
 *   - 想给自己记别人的账：得让对方的地址派生出的 program 命中自己那把钥，同样不可行。
 * 于是"付给谁的 P2WSH"就等于"记谁的账"，不再需要额外在 tx 里写承诺。
 * 代价是下面这条不变式必须由桥遵守（链上只能部分兜底）：
 *
 *   【不变式·冻结】用户 P2WSH 脚本只允许由**外部充值方**付款；
 *   桥的任何自有付款（提现放款、扫集找零、手续费补贴、批量归集）都不得落到用户 P2WSH 上。
 *
 * 违反它的后果：桥自己付出去的那笔交易，收款用户可以拿着它回头当**充值证明**再铸一次
 * ——因为链上只能看到"这笔 tx 付了 P2WSH(该用户)"，看不出付款方是谁（链上没有任何标记能
 * 区分"外部充值"与"桥的付款"，见 TECHNICAL.md 的安全模型）。逐轮记账的净额是：
 * 供给 ±0（烧多少铸多少）、主池 −X、桥控脚本 +X —— 桥的总资产不变、用户也没多拿，
 * 所以它不是"资金被偷"，但会让提现额度与主池流动性被反复抽干（主池被抽干后，
 * 其他用户的提现会因主池不足而失败）。因此活干在两头：
 *   - 提现侧：checkWithdraw 直接拒绝 native P2WSH 目标地址（链上可判定，见 E15-a）；
 *   - 扫集/找零/补贴侧：一律回主池 TSS P2WPKH，不许回用户 P2WSH（C4 落地，约束现在立此存照）。
 *
 * 其它已冻结的边界：
 *   - 主托管（TSS 主池）脚本形态不变，仍是 P2WPKH：P2WSH 只作用于"BTC 充值收款"。
 *   - RGB20 分支不走这条路（金额在 consignment 内，链上只验 TSS 阈值签名，H4）。
 *   - `withdrawCommitmentPrefix` 保留：提现确认仍要 OP_RETURN 承诺（只有 RGB20 跳过）。
 */

func btcProof2String(txProof *rtypes.BtcTxProof) string {
	return fmt.Sprintf("btcBlockHeight: %d, btcBlockHash: %s, btcTxIndex: %d, btcTxData: %s",
		txProof.GetBlockHeight(), txProof.GetBlockHash(),
		txProof.GetTxIndex(), hex.EncodeToString(txProof.GetTxData()))
}

func merkleProof2String(merkleProof [][]byte) string {
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
	if !rtypes.IsRgb20Symbol(pendingTx.GetAssetSymbol()) {
		if !hasWithdrawCommitment(btcTx, confirm.GetTxHash()) {
			elog.Error("checkWithdrawConfirm commitment mismatch", "txHash", txHash, "confirmHash", confirmHash,
				"btcProof", btcProof2String(confirm.GetBtcTxProof()))
			return ErrInvalidBtcProofCommitment
		}
		if err = r.validateWithdrawTxContent(txHash, pendingTx, btcTx); err != nil {
			return err
		}
	}
	// S2：放款前必须对得上全局操作台账 —— 这笔 burn 在**锁定时**就登记过、金额与 payload 一致、
	// 且待销毁额度不超过台账里"已铸造 − 已销毁"。判据全部落在共识状态（记录 + 供应量 + payload），
	// 不读 LocalDB；判定式与边界见 checkWithdrawOperationLedger。
	//
	// 排在这里而不是最前面：前面几项（S3 去重 / SPV 证明 / OP_RETURN 承诺 / 金额脚本）判的是
	// "这份证明本身对不对"，是最有诊断价值的拒绝（桥发来的东西有问题）；台账判的是"链上允不允许
	// 放这笔款"。两者都是**永久性**判据，故都排在最后的暂时性判据（确认深度）之前。
	withdraw := &rtypes.WithdrawAsset{}
	if err = readDB(r.GetStateDB(), formatPayloadKey(confirm.GetTxHash()), withdraw); err != nil {
		elog.Error("checkWithdrawConfirm read payload", "txHash", txHash, "confirmHash", confirmHash,
			"burnTxHash", hex.EncodeToString(confirm.GetTxHash()), "err", err)
		return ErrConfirmPayloadNotExist
	}
	if err = r.checkWithdrawOperationLedger(confirm.GetTxHash(), withdraw); err != nil {
		return err
	}
	// B8：链上最小确认数（放最后，理由见 checkBtcConfirmations 的注释）。
	return r.checkBtcConfirmations("withdrawConfirm", txHash, confirm.GetBtcTxProof())
}

// deriveDepositPkScript 按冻结规格确定性重建"该用户在本 symbol 下的 P2WSH program"。
//
// 执行器侧不需要 bech32、不需要网络参数：只比 34 字节的 pkScript（少一层编解码歧义，
// 也避免"地址串与 program 谁才是真身份"的二次约定）。逐字节规格见
// plugin/dapp/rgbx/types/p2wsh_deposit.go，测试向量为三方共用。
//
// tssPub 取自链上 CrossChainInfo.Pubkey（CommitDKG 提交，checkCommitDKG 已强制带且
// 与 DKG 地址的 P2WPKH 绑定）。取不到 ⇒ ErrInvalidCrossChainInfo（**不留旧状态兼容分支**：
// 本链尚未上线，没有"没有 pubkey 的历史 CrossChainInfo"需要照顾）。
func (r *rgbx) deriveDepositPkScript(txHash, symbol, depositAddr string) ([]byte, error) {
	info, err := r.getCrossChainInfo(symbol)
	if err != nil {
		elog.Error("deriveDepositPkScript getCrossChainInfo", "txHash", txHash, "symbol", symbol, "err", err)
		return nil, ErrGetCrossChainInfo
	}
	pkScript, err := rtypes.DeriveDepositPkScript(depositAddr, info.GetPubkey())
	if err != nil {
		elog.Error("deriveDepositPkScript derive p2wsh", "txHash", txHash, "symbol", symbol,
			"depositAddr", depositAddr, "tssPubLen", len(info.GetPubkey()), "err", err)
		return nil, ErrInvalidCrossChainInfo
	}
	return pkScript, nil
}

// validateDepositTxContent 链上金额校验（非 RGB20）：累加"等于该用户 P2WSH program"的输出，
// 要求总和**恰好等于**申报金额。
//
// 两个错误码分得开，便于运维/桥侧区分"地址不对"与"金额不对"：
//   - ErrInvalidDepositScript：这笔 tx 里**根本没有**付给该用户 P2WSH 的输出（充值地址错、
//     tssPub 换代、或用户打到了别的地址）；
//   - ErrInvalidDepositAmount：有这个输出，但金额与申报不符（多付/少报、想多记别人的付款）。
//
// 允许一笔 tx 同时给多个用户的 P2WSH 付款：各用户各自索赔时只累加自己那份（各自
// deposit.GetAmount() 必须与自己那段相等），互不影响。
func (r *rgbx) validateDepositTxContent(txHash string, deposit *rtypes.DepositAsset, btcTx *wire.MsgTx) error {
	// RGB20 分支：全跳过链上金额（RGB 金额在 consignment 内）。
	if rtypes.IsRgb20Symbol(deposit.GetAssetSymbol()) {
		return nil
	}
	expectPkScript, err := r.deriveDepositPkScript(txHash, deposit.GetAssetSymbol(), deposit.GetDepositAddress())
	if err != nil {
		return err
	}
	var amount int64
	matched := 0
	for _, out := range btcTx.TxOut {
		if bytes.Equal(out.PkScript, expectPkScript) {
			amount += out.Value
			matched++
		}
	}
	if matched == 0 {
		elog.Error("validateDepositTxContent no p2wsh output for user", "txHash", txHash,
			"symbol", deposit.GetAssetSymbol(), "depositAddr", deposit.GetDepositAddress(),
			"expectPkScript", hex.EncodeToString(expectPkScript))
		return ErrInvalidDepositScript
	}
	if amount != deposit.GetAmount() {
		elog.Error("validateDepositTxContent amount mismatch", "txHash", txHash,
			"depositAddr", deposit.GetDepositAddress(),
			"expect", deposit.GetAmount(), "actual", amount, "matchedOutputs", matched)
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

// parseSpendingTxStrict 解析 UtxoSpendingProof.SpendingTx 并要求其为规范编码（E1 家族 A2）。
// 与充值侧 parseBtcTxIDStrict 同一口径：解析后 reader 必须被完整消费，尾部多余字节直接拒绝。
//
// btcwire 的 DeserializeNoWitness 只按需读取、不校验 reader 是否耗尽：给同一笔花费追加尾部字节后，
// 解析结果（输入 outpoint、OP_RETURN 承诺）完全不变，只有原始字节变了。因此
//   - SpendingTx 必须能被严格解析（本函数）；
//   - 归属 utxo id 必须由解析后的交易身份 TxHash() 导出，不能对原始字节取 DoubleHashH，
//     否则同一笔花费可被记到另一个 owner id（见 Exec_Confirm）。
func parseSpendingTxStrict(txHash string, txData []byte) (*wire.MsgTx, error) {
	if len(txData) == 0 {
		elog.Error("parseSpendingTxStrict empty spending tx", "txHash", txHash)
		return nil, ErrDecodeBtcTx
	}
	reader := bytes.NewReader(txData)
	spendingTx := &wire.MsgTx{}
	if err := spendingTx.DeserializeNoWitness(reader); err != nil {
		elog.Error("parseSpendingTxStrict decode spending tx", "txHash", txHash, "err", err)
		return nil, ErrDecodeBtcTx
	}
	if reader.Len() != 0 {
		elog.Error("parseSpendingTxStrict trailing bytes after spending tx", "txHash", txHash,
			"txDataLen", len(txData), "trailingLen", reader.Len())
		return nil, ErrNonCanonicalSpendingTx
	}
	return spendingTx, nil
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
			"merkleProof", merkleProof2String(proof.GetMerkleProof()))
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

// getBtcCanonicalTip 读 lightclient 的 canonical BTC tip（statedb 里的 btc-lastheader）。
//
// 为什么是 GetBtcLastHeader（statedb）而不是 GetBtcHeader（localdb）：
//   - btc-lastheader 写在 **statedb**，由 lightclient 的 Exec_BtcHeaders 在 canonical 链切换时写入
//     （B3/B4 之后重组回退也会改写它），是"全网共识可见"的头链 tip；query 带 stateHash，
//     读到的是**链上状态**、各节点一致 ⇒ 满足"共识判定只依赖共识状态"。
//   - btc-header-<height> 在 **localdb**，是节点私有的逐高度索引（见 lightclient/executor/kv.go）。
//     读它做共识判定会让"区块是否合法"依赖各节点本地库（本地库落后/丢失时结论不同），
//     正是本次要避免的老毛病（对照 checkWithdrawConfirm 里 S3 守卫的同类理由）。
//
// 头链还没写入过任何头时，lightclient 返回零值头（Hash == ""）而不是错误，调用方需按"无 tip"处理。
func (r *rgbx) getBtcCanonicalTip() (*ltypes.BtcHeader, error) {
	msg, err := r.GetAPI().Query(ltypes.LightclientX, "GetBtcLastHeader", &types.ReqNil{})
	if err != nil {
		elog.Error("getBtcCanonicalTip query", "err", err)
		return nil, err
	}
	header, ok := msg.(*ltypes.BtcHeader)
	if !ok || header == nil {
		elog.Error("getBtcCanonicalTip invalid header")
		return nil, types.ErrInvalidParam
	}
	return header, nil
}

// btcConfirmations proofHeight 处的块在 tipHeight 处的确认数（含该块自身）：tip == proofHeight 时为 1。
// tip 还没到该高度（含 canonical 链被重组回退到该块之前）时为 0 —— 这正是 B8 想让"重组"可见的地方。
func btcConfirmations(tipHeight, proofHeight uint64) uint64 {
	if tipHeight < proofHeight {
		return 0
	}
	return tipHeight - proofHeight + 1
}

// checkBtcConfirmations 链上最小确认数（B8）：要求
//
//	canonical tip 高度 >= proof.BlockHeight + N - 1        （N = rgbxCfg.MinBtcConfirmations，默认 6）
//
// 即证明所在 BTC 区块在链上被确认至少 N 个块，否则拒绝。
//
// 两个调用点（checkDeposit / checkWithdrawConfirm）都把它安排在**其它校验全部通过之后**：
// 确认深度是**会随时间自愈**的暂时状态（tip 前进即满足）；而被重组回退时它又会重新不满足 ——
// 后者正是本项要的效果：头链能跟随重组回退（B3/B4）之后，链上"看得见的深度"才有意义。
// 相比 txid 去重 / 严格解析 / OP_RETURN 承诺 / 金额校验这些**永久性**判定，暂时性的拒绝必须排在后面，
// 否则"这份证明永远无效"会被报成"确认不足"，把运维引向等待。
//
// fail-closed：查询不到 canonical tip（lightclient 不可用、头链为空）时同样拒绝 ——
// 确定不了深度就不能默认深度足够。
func (r *rgbx) checkBtcConfirmations(action, txHash string, proof *rtypes.BtcTxProof) error {
	required := rgbxCfg.MinBtcConfirmations
	if required <= 0 {
		// 防御性兜底：initCfg 保证 > 0（<= 0 取默认值）；此处覆盖绕过 Init 直接构造的调用方。
		required = defaultMinBtcConfirmations
	}
	height := proof.GetBlockHeight()

	tip, err := r.getBtcCanonicalTip()
	if err != nil {
		elog.Error("checkBtcConfirmations get canonical tip", "action", action, "txHash", txHash,
			"proofHeight", height, "required", required, "err", err)
		return fmt.Errorf("%w: tipHeight=unknown proofHeight=%d required=%d err=%v",
			ErrInsufficientBtcConfirmations, height, required, err)
	}
	if tip.GetHash() == "" {
		elog.Error("checkBtcConfirmations empty canonical tip", "action", action, "txHash", txHash,
			"proofHeight", height, "required", required)
		return fmt.Errorf("%w: tipHeight=none proofHeight=%d required=%d",
			ErrInsufficientBtcConfirmations, height, required)
	}

	confs := btcConfirmations(tip.GetHeight(), height)
	if confs < uint64(required) {
		elog.Error("checkBtcConfirmations insufficient confirmations", "action", action, "txHash", txHash,
			"tipHeight", tip.GetHeight(), "proofHeight", height, "confirmations", confs, "required", required)
		return fmt.Errorf("%w: tipHeight=%d proofHeight=%d confirmations=%d required=%d",
			ErrInsufficientBtcConfirmations, tip.GetHeight(), height, confs, required)
	}
	return nil
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

// 提现确认仍要求 OP_RETURN 承诺 rgbx:withdraw:<chain33 burn txHash>（只有 RGB20 跳过，H4）。
// 充值侧自 v1 起**没有**这一层：绑定改由 P2WSH 派生承担（见文件头的"充值绑定"注释）。
func hasWithdrawCommitment(tx *wire.MsgTx, chain33TxHash []byte) bool {
	expectData := append([]byte(withdrawCommitmentPrefix), chain33TxHash...)
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
