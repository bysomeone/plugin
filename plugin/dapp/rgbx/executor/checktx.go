package executor

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/33cn/chain33/common/address"
	"github.com/33cn/chain33/types"
	rtypes "github.com/33cn/plugin/plugin/dapp/rgbx/types"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
)

var (
	ErrInvalidSymbolLength              = errors.New("invalid asset symbol length")
	ErrInvalidAssetAmount               = errors.New("invalid asset amount")
	ErrInvalidMetaHashLength            = errors.New("invalid meta hash length")
	ErrNilGenesisOut                    = errors.New("nil genesis output")
	ErrDuplicateAssetSymbol             = errors.New("duplicate asset symbol")
	ErrAssetNotExist                    = errors.New("asset not exist")
	ErrDecodeBtcTx                      = errors.New("decode btc tx error")
	ErrPendingTxNotExist                = errors.New("pending tx not exist")
	ErrConfirmPayloadNotExist           = errors.New("confirm payload not exist")
	ErrTxAlreadyConfirmed               = errors.New("tx already confirmed")
	ErrConfirmedHashNotEqual            = errors.New("confirmed hash not equal")
	ErrSpendingInputNotEqual            = errors.New("spending input not equal")
	ErrOpRetOutputPkScriptNotEqual      = errors.New("op return output pkScript not equal")
	ErrInvalidCommitAddress             = errors.New("invalid commit address")
	ErrFromUtxoPkScriptNotSet           = errors.New("from utxo pkScript not set")
	ErrInvalidAssetPrecision            = errors.New("invalid asset precision")
	ErrInvalidAssetSender               = errors.New("invalid asset sender")
	ErrInvalidFromUtxo                  = errors.New("invalid from utxo")
	ErrInvalidSpendingTxIn              = errors.New("invalid spending tx input")
	ErrInvalidWithdrawAmount            = errors.New("invalid withdraw amount")
	ErrInvalidWithdrawDestination       = errors.New("invalid withdraw destination")
	ErrInvalidWithdrawDestinationScript = errors.New("invalid withdraw destination script")
	// ErrWithdrawToDepositScript 提现目标**就是提现发起人自己的充值脚本**
	// （program == P2WSH(tx.From(), 本 symbol 的 tssPub)）。
	//
	// E15-a 卫生项（纵深防御）：提现是 receiverPaysFee=true（btcwallet.go buildTransaction），
	// 收款输出只有 amount−fee；而去掉充值 OP_RETURN 后，链上充值判定取的是"实际付到派生
	// P2WSH 的输出值"（validateDepositTxContent 累加实际输出，**不是**名义金额）。
	// 两条前提合起来 ⇒ 把提现目标填成自己的充值地址再拿那笔付款当充值证明，这一圈
	// **是自我限制的**：用户每轮净亏一次手续费，桥的总资产不变（BTC 落在桥自己控制的
	// P2WSH 里，用户拿不走），真实后果只是主池流动性被反复抽干（其他用户提现可能因
	// 主池不足而失败）。所以这是低成本卫生项，不是"资金被偷"级紧急项。
	// **这两个前提不要被改掉**（提现保持 receiverPaysFee=true；充值金额保持"实际输出值"）。
	//
	// 只拒"我们自己的充值脚本"，**不拒一般的 P2WSH**：交易所/多签钱包/闪电通道大量使用
	// P2WSH 与 P2TR 地址，一刀切会把合法提现也拒了。
	ErrWithdrawToDepositScript = errors.New("withdraw destination is a bridge deposit script")
	ErrInvalidDepositAmount    = errors.New("invalid deposit amount")
	ErrInvalidDepositAddress   = errors.New("invalid deposit address")
	// ErrInvalidDepositScript 充值交易里没有任何输出付给该用户的 P2WSH 派生脚本
	// （充值地址错 / tssPub 换代 / 用户打到了别的地址）。与 ErrInvalidDepositAmount 分开，
	// 便于桥侧把"地址不对"与"金额不对"区分开。旧 OP_RETURN 承诺口径（ErrInvalidDepositCommitment）
	// 已随硬切删除：v1 的绑定完全由派生承担（见 validate_proof.go 的"充值绑定"注释）。
	ErrInvalidDepositScript             = errors.New("invalid deposit p2wsh script")
	ErrInvalidWithdrawFeeRate           = errors.New("invalid withdraw fee rate")
	ErrInvalidAssetSymbol               = errors.New("invalid asset symbol")
	ErrInvalidBtcTxProof                = errors.New("invalid btc tx proof")
	ErrWithdrawConfirmTimeoutNotAllowed = errors.New("withdraw confirm timeout not allowed")
	ErrInvalidBtcProofIndex             = errors.New("invalid btc proof tx index")
	ErrInvalidBtcBlockHash              = errors.New("invalid btc block hash")
	ErrGetBtcHeader                     = errors.New("get btc header error")
	ErrInvalidBtcProofBlock             = errors.New("invalid btc proof block info")
	ErrInvalidBtcProofCommitment        = errors.New("invalid btc withdraw commitment")
	ErrInvalidBtcProofMerkle            = errors.New("invalid btc merkle proof")
	ErrCalcBtcMerkleRoot                = errors.New("calc btc merkle root error")
	ErrInvalidCrossChainInfo            = errors.New("invalid cross chain info")
	ErrNewAccountDB                     = errors.New("new account db error")
	ErrGetCrossChainInfo                = errors.New("get cross chain info error")
	// ErrDuplicateDepositProof 充值证明的 btc-txid 已被用于铸造（E1 去重）。
	// 文案保留 "duplicate deposit proof" 子串：桥侧按它把重发视为"链上已认过这笔付款交易"，
	// 进而把该笔充值按已铸造处理（neutrino rgb20/deposit.go 的 depositAlreadyConsumedMarker）。
	ErrDuplicateDepositProof    = errors.New("duplicate deposit proof")
	ErrInvalidGuardianCommitter = errors.New("invalid guardian committer")
	ErrDuplicateDKGCommit       = errors.New("duplicate dkg commit")
	ErrGetGuardianNodeAddress   = errors.New("get guardian node address error")
	ErrGetDkgConfirmations      = errors.New("get dkg confirmations error")
	ErrInvalidDkgAddress        = errors.New("invalid dkg address")
	// ErrWithdrawAlreadyConfirmed 同一笔提现 burn 已被结算（放款）过（S3）。
	// 文案保留 "already confirmed" 子串：桥侧重试路径按该子串把重复提交视为幂等成功
	// （neutrino commitWithdrawConfirm 对含 "already confirmed" 的错误不再重试）。
	ErrWithdrawAlreadyConfirmed = errors.New("withdraw already confirmed")
	// ErrMintAlreadyConfirmed 同一笔 mint 确认已在共识状态里结算过（E14）。
	// 与提现侧 S3 对称：真正的判据是 stateDB 的 formatConfirmUsedKey，不是 localdb 的
	// pendingTx.Confirmed（那条只是辅助，见 checkConfirm）。
	// 文案同样保留 "already confirmed" 子串：桥侧 commitPendingTx 按该子串把重复提交视为幂等成功。
	ErrMintAlreadyConfirmed = errors.New("mint already confirmed")
	// ErrTransferAlreadyConfirmed 同一笔 transfer 确认已在共识状态里结算过（E14，与 mint 侧对称）。
	// mint 与 transfer 是 Exec_Confirm 里仅有的两条走 UtxoProof/SpendingTx 的结算分支，
	// 二者的去重键与理由完全相同（见 formatConfirmUsedKey）。
	ErrTransferAlreadyConfirmed = errors.New("transfer already confirmed")
	// ErrNonCanonicalSpendingTx SpendingTx 尾部带多余字节（非规范编码）——同一笔花费的另一份编码，
	// 必须拒绝：否则归属 utxo id 会随编码变化（E1 家族 A2，口径同充值侧 parseBtcTxIDStrict）。
	ErrNonCanonicalSpendingTx = errors.New("non-canonical spending tx encoding")
	// ErrInsufficientBtcConfirmations B8：证明所在 BTC 区块在 canonical 头链里的确认数不足
	// （tip.Height < proof.BlockHeight + minBtcConfirmations - 1），或无法确定 tip
	// （查询失败 / 头链为空）时一律拒绝。错误信息带 tip 高度 / 证明高度 / 要求的 N。
	ErrInsufficientBtcConfirmations = errors.New("insufficient btc confirmations")
	// ErrInvalidOperationID operationId 派生输入非法（提现侧 symbol/burn 哈希异常）。
	// 与 ErrOperationNotExist 分开：这是"算不出 id"，不是"没记录过"。
	ErrInvalidOperationID = errors.New("invalid operation id")
	// ErrOperationNotExist 提现结算时，共识状态里没有对应的提现销毁台账记录（S2）。
	// 含义：这笔 burn 没有经过 Exec_Withdraw 的锁定登记（或状态被回滚/损坏），不允许放款。
	ErrOperationNotExist = errors.New("operation not recorded")
	// ErrOperationMismatch 台账记录与本次结算的**身份**不一致（S2）：kind/symbol/chain33 交易哈希对不上。
	ErrOperationMismatch = errors.New("operation record mismatch")
	// ErrOperationAmountMismatch 台账记录的金额与 payload 里的提现金额不一致（S2）。
	// 含义：本次要销毁的额度不是锁定时登记的那笔额度，拒绝。
	ErrOperationAmountMismatch = errors.New("operation amount mismatch")
	// ErrDuplicateOperation 同一 operationId 已被登记（S2）：台账只可新增、不可改/不可重复入账。
	ErrDuplicateOperation = errors.New("duplicate operation")
	// ErrInsufficientMintedSupply 待销毁额度超过台账里的"已铸造 − 已销毁"（S2）。
	// 含义：这笔提现没有对应的充值铸造记录（proof-of-mint 不成立），拒绝放款。
	ErrInsufficientMintedSupply = errors.New("insufficient minted supply")
)

const (
	maxBtcFeeRate        = int64(1000)
	minBtcWithdrawAmount = int64(546)
	// minRgb20WithdrawAmount RGB20 提现最小金额（token 最小单位口径）。
	minRgb20WithdrawAmount = int64(1)
)

// CheckTx 实现自定义检验交易接口，供框架调用
func (r *rgbx) CheckTx(tx *types.Transaction, index int) error {

	txHash := hex.EncodeToString(tx.Hash())
	action := &rtypes.RgbxAction{}
	err := types.Decode(tx.GetPayload(), action)
	if err != nil {
		elog.Error("CheckTx", "txHash", txHash, "Decode payload error", err)
		return types.ErrActionNotSupport
	}

	switch action.Ty {
	case rtypes.TyMintAction:
		err = r.checkMint(txHash, action.GetMint())
	case rtypes.TyTransferAction:
		err = r.checkTransfer(tx, txHash, action.GetTransfer())
	case rtypes.TyCommitDKGAction:
		err = r.checkCommitDKG(txHash, tx.From(), action.GetCommitDKG())
	case rtypes.TyDepositAsset:
		err = r.checkDeposit(txHash, action.GetDeposit())
	case rtypes.TyWithdrawAsset:
		err = r.checkWithdraw(tx.From(), txHash, action.GetWithdraw())
	case rtypes.TyConfirmAction:
		err = r.checkConfirm(tx.From(), txHash, action.GetConfirm())
	default:
		err = types.ErrActionNotSupport

	}
	if err != nil {
		elog.Error("rgbx CheckTx", "txHash", txHash, "actionName", tx.ActionName(),
			"err", err, "action", string(types.MustPBToJSON(action)))
	}
	return err
}

func (r *rgbx) checkMint(txHash string, mint *rtypes.MintAsset) error {

	if len(mint.GetSymbol()) < 1 || len(mint.GetSymbol()) > rtypes.MaxAssetSymbolLength {
		elog.Error("checkMint", "txHash", txHash,
			"symbol", mint.Symbol, "symbolLen", len(mint.GetSymbol()))
		return ErrInvalidSymbolLength
	}

	// A6：symbol 进入白名单字符集校验（formatSymbol 的 ToUpper 只在 ASCII 字母/数字/下划线上单射），
	// 否则 "xſ" 这类输入会与 "xs" 归一化为同一个 asset key，可抢注/别名化已有 symbol。
	if !isValidSymbolCharset(mint.GetSymbol()) {
		elog.Error("checkMint invalid symbol charset", "txHash", txHash, "symbol", mint.Symbol)
		return ErrInvalidAssetSymbol
	}

	if isCrossChainSymbol(mint.GetSymbol()) {
		return ErrInvalidAssetSymbol
	}

	ty := rtypes.AssetType(mint.GetType())
	if mint.GetTotalAmount() <= 0 || mint.GetTotalAmount() > rtypes.MaxAssetAmount ||
		(ty == rtypes.Collectible && mint.GetTotalAmount() != 1) {
		elog.Error("checkMint", "txHash", txHash, "symbol", mint.Symbol,
			"amount", mint.GetTotalAmount(), "type", ty.String())
		return ErrInvalidAssetAmount
	}
	if ty != rtypes.Collectible && mint.GetPrecision() > rtypes.MaxPrecision {
		elog.Error("checkMint", "txHash", txHash, "symbol", mint.Symbol,
			"precision", mint.GetPrecision(), "maxPrecision", rtypes.MaxPrecision)
		return ErrInvalidAssetPrecision
	}

	if len(mint.GetMetaHash()) > rtypes.MetaHashLen {
		elog.Error("checkMint", "txHash", txHash, "symbol", mint.Symbol,
			"metaHashLen", len(mint.GetMetaHash()))
		return ErrInvalidMetaHashLength
	}

	_, err := r.GetStateDB().Get(formatAssetKey(mint.GetSymbol()))
	if !errors.Is(err, types.ErrNotFound) {
		elog.Error("checkMint duplicate asset", "txHash", txHash, "symbol", mint.Symbol)
		return ErrDuplicateAssetSymbol
	}

	if mint.GetGenesisOut().GetHash() == "" || mint.GetGenesisOut().GetPkScript() == nil {
		elog.Error("checkMint invalid genesis out", "txHash", txHash, "symbol", mint.Symbol)
		return ErrNilGenesisOut
	}

	return nil
}

func (r *rgbx) checkTransfer(tx *types.Transaction, txHash string, transfer *rtypes.TransferAsset) error {

	if transfer.GetAmount() <= 0 {
		elog.Error("checkTransfer amount", "txHash", txHash, "symbol", transfer.GetSymbol(), "amount", transfer.GetAmount())
		return ErrInvalidAssetAmount
	}
	fromAddr := tx.From()
	if isCrossChainSymbol(transfer.GetSymbol()) {
		return r.checkCrossChainTransfer(txHash, fromAddr, transfer)
	}
	fromUtxo := transfer.GetFromUtxo()
	if fromUtxo != "" {
		if !rtypes.IsUtxoAddress(fromUtxo) || len(transfer.GetFromUtxoPkScript()) == 0 {
			elog.Error("checkTransfer invalid fromUtxo", "txHash", txHash, "symbol", transfer.GetSymbol(), "fromUtxo", fromUtxo)
			return ErrInvalidFromUtxo
		}
		fromAddr = fromUtxo
	}
	if address.CheckAddress(transfer.GetTo(), -1) != nil ||
		(transfer.GetChangeAddr() != "" && address.CheckAddress(transfer.GetChangeAddr(), -1) != nil) {
		elog.Error("checkTransfer address", "txHash", txHash, "symbol", transfer.GetSymbol(),
			"from", fromAddr, "to", transfer.GetTo(), "changeAddr", transfer.GetChangeAddr())
		return types.ErrInvalidAddress
	}

	asset := &rtypes.RgbxAsset{}
	err := readDB(r.GetStateDB(), formatAssetKey(transfer.GetSymbol()), asset)
	if err != nil {
		elog.Error("checkTransfer get asset", "txHash", txHash, "symbol", transfer.GetSymbol(),
			"err", err)
		return ErrAssetNotExist
	}

	assetTy := rtypes.AssetType(asset.GetType())
	if assetTy == rtypes.Normal {
		accDb, err := r.newAccount(transfer.GetSymbol())
		if err != nil {
			elog.Error("checkTransfer newAccount", "txHash", txHash, "symbol", transfer.GetSymbol(), "err", err)
			return ErrNewAccountDB
		}
		balance := accDb.LoadAccount(fromAddr).GetBalance()
		if balance < transfer.GetAmount() {
			elog.Error("checkTransfer insufficient balance", "txHash", txHash, "from", fromAddr,
				"symbol", transfer.GetSymbol(), "need", transfer.GetAmount(), "balance", balance)
			return types.ErrInsufficientBalance
		}
	} else if fromAddr != asset.Owner {
		elog.Error("checkTransfer invalid owner", "txHash", txHash, "symbol", transfer.GetSymbol(),
			"from", fromAddr, "assetOwner", asset.Owner)
		return ErrInvalidAssetSender
	}
	return nil
}

func (r *rgbx) checkCrossChainTransfer(txHash, fromAddr string, transfer *rtypes.TransferAsset) error {

	accDB, err := r.newAccount(transfer.GetSymbol())
	if err != nil {
		elog.Error("checkCrossChainTransfer newCrossChainAccount", "txHash", txHash, "symbol", transfer.GetSymbol(), "err", err)
		return ErrNewAccountDB
	}
	if accDB.LoadAccount(fromAddr).GetBalance() < transfer.GetAmount() {
		elog.Error("checkCrossChainTransfer insufficient balance", "txHash", txHash, "from", fromAddr,
			"symbol", transfer.GetSymbol(), "need", transfer.GetAmount())
		return types.ErrInsufficientBalance
	}
	return nil
}

func (r *rgbx) checkCommitDKG(txHash, fromAddr string, commitDKG *rtypes.CommitDKG) error {

	symbol := commitDKG.GetAssetSymbol()
	// A6：同 checkMint —— CrossChainInfo 以 formatSymbol(symbol) 为 key，别名化 symbol 会写到
	// 另一个资产的 key 上（例如占用/抢先注册 RGB20_USDT 的 CrossChainInfo，进而影响充值验签）。
	if !isValidSymbolCharset(symbol) {
		elog.Error("checkCommitDKG invalid symbol charset", "txHash", txHash, "symbol", symbol)
		return ErrInvalidAssetSymbol
	}
	pkScript, err := r.decodeBtcAddressScript(commitDKG.GetDkgAddress())
	if err != nil || !bytes.Equal(pkScript, commitDKG.GetPkScript()) {
		elog.Error("checkCommitDKG decode btc address script", "txHash", txHash,
			"symbol", symbol, "dkgAddress", commitDKG.GetDkgAddress(),
			"pkScript", hex.EncodeToString(commitDKG.GetPkScript()), "expectPkScript", hex.EncodeToString(pkScript), "err", err)
		return ErrInvalidDkgAddress
	}
	// 所有 symbol（含 BTC/XBTC）都必须提交 TSS 组公钥，并校验 hash160(pubkey) == pkScript[2:]，
	// 确保群公钥与 DKG 地址一致（原 RGB20 分支 BL-1 提升为全局）。
	//
	// 为什么 BTC 符号也必须带：P2WSH 充值地址 = f(userID, tssPub)，执行器/桥都靠这把钥重建脚本
	// （validate_proof.go 的 deriveDepositPkScript）。缺了它充值根本无法判定；带了别的钥，
	// 用户打进的 BTC 会落到桥认不出的脚本里。所以"取不到即报错"，**不留无 pubkey 的兼容分支**
	// （本链未上线，不存在需要照顾的历史 CrossChainInfo）。
	// 只接受 33 字节压缩公钥（ParseDepositTssPubKey）：非压缩的同一把钥会派生出另一个地址。
	pub, err := rtypes.ParseDepositTssPubKey(commitDKG.GetPubkey())
	if err != nil {
		elog.Error("checkCommitDKG parse tss pubkey", "txHash", txHash, "symbol", symbol,
			"pubkeyLen", len(commitDKG.GetPubkey()), "err", err)
		return ErrInvalidDkgAddress
	}
	pubHash := btcutil.Hash160(pub)
	if len(pkScript) != 22 || pkScript[0] != txscript.OP_0 || pkScript[1] != 0x14 ||
		!bytes.Equal(pubHash, pkScript[2:]) {
		elog.Error("checkCommitDKG pubkey mismatch", "txHash", txHash, "symbol", symbol,
			"pubHash", hex.EncodeToString(pubHash), "pkScript", hex.EncodeToString(pkScript))
		return ErrInvalidDkgAddress
	}
	guardianAddrs, err := r.getGuardianNodeAddress(rgbxCfg.GuardianParachainTitle)
	if err != nil {
		elog.Error("checkCommitDKG getGuardianNodeAddress", "txHash", txHash, "symbol", symbol, "err", err)
		return ErrGetGuardianNodeAddress
	}
	if !strings.Contains(guardianAddrs, fromAddr) {
		elog.Error("checkCommitDKG invalid committer", "txHash", txHash, "symbol", symbol, "fromAddr", fromAddr)
		return ErrInvalidGuardianCommitter
	}

	_, err = r.GetStateDB().Get(formatCrossChainInfoKey(symbol))
	if err == nil {
		elog.Error("checkCommitDKG duplicate cross chain info", "txHash", txHash, "symbol", symbol)
		return ErrDuplicateDKGCommit
	}
	return nil
}

func (r *rgbx) checkWithdraw(fromAddr, txHash string, withdraw *rtypes.WithdrawAsset) error {

	symbol := ensureCrossChainSymbol(withdraw.GetAssetSymbol())

	// RGB20 分支：destinationAddr 是用户 RGB 钱包 invoice（非 BTC 地址），跳过地址解码；
	// 金额下限按 token 最小单位口径，不套用 BTC dust（546 sat）。
	if rtypes.IsRgb20Symbol(withdraw.GetAssetSymbol()) {
		if withdraw.GetAmount() < minRgb20WithdrawAmount {
			elog.Error("checkWithdraw rgb20 amount", "txHash", txHash, "amount", withdraw.GetAmount())
			return ErrInvalidWithdrawAmount
		}
		if withdraw.GetDestinationAddr() == "" {
			elog.Error("checkWithdraw rgb20 empty invoice", "txHash", txHash)
			return ErrInvalidWithdrawDestination
		}
		if withdraw.GetFeeRate() < 1 || withdraw.GetFeeRate() > maxBtcFeeRate {
			elog.Error("checkWithdraw rgb20 feeRate", "txHash", txHash, "feeRate", withdraw.GetFeeRate())
			return ErrInvalidWithdrawFeeRate
		}
		accDB, err := r.newAccount(symbol)
		if err != nil {
			elog.Error("checkWithdraw rgb20 newAccount", "txHash", txHash, "symbol", withdraw.GetAssetSymbol(), "err", err)
			return err
		}
		balance := accDB.LoadAccount(fromAddr).GetBalance()
		if balance < withdraw.GetAmount() {
			elog.Error("checkWithdraw rgb20 insufficient balance", "txHash", txHash, "from", fromAddr,
				"symbol", symbol, "need", withdraw.GetAmount(), "balance", balance)
			return types.ErrInsufficientBalance
		}
		return nil
	}

	if withdraw.GetAmount() < minBtcWithdrawAmount {
		elog.Error("checkWithdraw amount", "txHash", txHash, "amount", withdraw.GetAmount())
		return ErrInvalidWithdrawAmount
	}

	destScript, err := r.decodeBtcAddressScript(withdraw.GetDestinationAddr())
	if err != nil {
		elog.Error("checkWithdraw invalid btc destination", "txHash", txHash, "address", withdraw.GetDestinationAddr(), "err", err)
		return ErrInvalidWithdrawDestination
	}
	if withdraw.GetFeeRate() < 1 || withdraw.GetFeeRate() > maxBtcFeeRate {
		elog.Error("checkWithdraw feeRate", "txHash", txHash, "feeRate", withdraw.GetFeeRate())
		return ErrInvalidWithdrawFeeRate
	}
	// E15-a：拒"提现目标 = 发起人自己的充值脚本"。理由与两条前提见 ErrWithdrawToDepositScript。
	//
	// tssPub 取自链上 CrossChainInfo（fail-closed：读不到 / 没带 pubkey 都拒 —— 同一份信息
	// 在提现确认（validateWithdrawTxContent）那里也是必需的，早拒比事后卡住好）。
	// 注意用**原始** symbol 查：CrossChainInfo 的 key 是 formatSymbol(raw)（"btc" → "BTC"），
	// 而上面的 symbol 已经过 ensureCrossChainSymbol（"btc" → "XBTC"）—— 用后者查不到。
	info, err := r.getCrossChainInfo(withdraw.GetAssetSymbol())
	if err != nil {
		elog.Error("checkWithdraw getCrossChainInfo", "txHash", txHash, "symbol", withdraw.GetAssetSymbol(), "err", err)
		return ErrGetCrossChainInfo
	}
	tssPub, err := rtypes.ParseDepositTssPubKey(info.GetPubkey())
	if err != nil {
		elog.Error("checkWithdraw invalid tss pubkey", "txHash", txHash, "symbol", symbol,
			"pubkeyLen", len(info.GetPubkey()), "err", err)
		return ErrInvalidCrossChainInfo
	}
	// 判定为什么是这个形态（而不是"目标是不是 native P2WSH"）：链上拿到的是目标地址的输出
	// 脚本，对 native P2WSH 只有 34 字节的 `OP_0 <sha256(witnessScript)>` —— witnessScript
	// 本身在花费前不可见，所以"看目标脚本是不是 <data> OP_DROP <tssPub> OP_CHECKSIG 形态"
	// 在链上**不可判定**。可判定的等价形式是"program 是否等于按 (发起人, tssPub) 能确定性
	// 重建出来的那个充值 program"，而发起人自己的充值地址正是这条自循环唯一能得手的形态
	// （充值金额必须落在按 depositAddress 派生的脚本上，depositAddress 就是发起人的 chain33
	// 地址）。一般的 P2WSH（交易所/多签/闪电通道）与 P2TR 目标一律放行。
	//
	// 覆盖面边界（如实记）：若用**另一个**自己控制的 chain33 地址 B 的充值地址当提现目标
	// （再以 B 认领充值），链上无法枚举"已发放地址" ⇒ 挡不住；那一段只能由桥侧用 registry
	// 兜底（属 C2/C4）。链上这条是无白名单、零成本的兜底。
	if rtypes.IsDepositPkScript(destScript, fromAddr, tssPub) {
		elog.Error("checkWithdraw destination is own deposit script", "txHash", txHash,
			"fromAddr", fromAddr, "symbol", symbol, "address", withdraw.GetDestinationAddr(),
			"destScript", hex.EncodeToString(destScript))
		return ErrWithdrawToDepositScript
	}
	accDB, err := r.newAccount(symbol)
	if err != nil {
		elog.Error("checkWithdraw newAccount", "txHash", txHash, "symbol", withdraw.GetAssetSymbol(), "err", err)
		return err
	}
	balance := accDB.LoadAccount(fromAddr).GetBalance()
	if balance < withdraw.GetAmount() {
		elog.Error("checkWithdraw insufficient balance", "txHash", txHash, "from", fromAddr,
			"symbol", symbol, "need", withdraw.GetAmount(), "balance", balance)
		return types.ErrInsufficientBalance
	}
	return nil
}

// checkDepositDuplicate 充值防双铸检查（E1 修复 B）。
// 旧口径（TxData 原始字节哈希）可被"同一笔交易的另一份编码"绕过：追加尾部字节后 key 变化、
// 重复检查不触发，而 merkle/金额/OP_RETURN 校验沿用同一份解析结果 → 同一笔真实充值可重复铸币。
// 故唯一标识改用交易的规范化身份：先对 TxData 做严格解析（尾部多余字节直接拒绝），
// 再以解析后交易的 btc txid 查重。txid 对同一笔交易恒定、不随 TxData 的编码变化，
// 因此"同一笔真实充值的另一份编码"必然命中同一 key。
func (r *rgbx) checkDepositDuplicate(txHash string, deposit *rtypes.DepositAsset) error {
	txID, err := parseBtcTxIDStrict(txHash, deposit.GetTxProof().GetTxData())
	if err != nil {
		return err
	}
	if _, err = r.GetStateDB().Get(formatDepositUsedTxIDKey(txID)); !errors.Is(err, types.ErrNotFound) {
		elog.Error("checkDeposit duplicate proof", "txHash", txHash, "symbol", deposit.GetAssetSymbol(),
			"proofID", "btc-txid", "btcTxID", hex.EncodeToString(txID), "err", err)
		return ErrDuplicateDepositProof
	}
	return nil
}

func (r *rgbx) checkDeposit(txHash string, deposit *rtypes.DepositAsset) error {
	if deposit.GetAmount() <= 0 {
		elog.Error("checkDeposit amount", "txHash", txHash, "amount", deposit.GetAmount())
		return ErrInvalidDepositAmount
	}
	// 充值地址必须是 chain33 地址串：它同时是 P2WSH 派生里的 userID（原样字节，见
	// types/p2wsh_deposit.go）。UTXO 形态（<btc txid>:<idx>，旧 fromUtxo 承诺路径的遗留）
	// 不再是合法充值地址 —— 硬切后没有 OP_RETURN / 首输入承诺可依赖，拒绝而非兼容。
	// IsUtxoAddress 必须显式排除：address.CheckAddress(addr, -1) 对 "<64hex>:<idx>" 这样的串
	// 也会返回 nil（任一 driver 通过即算合法），只靠它挡不住 UTXO 形态。
	addr := deposit.GetDepositAddress()
	if addr == "" || rtypes.IsUtxoAddress(addr) || address.CheckAddress(addr, -1) != nil {
		elog.Error("checkDeposit address invalid", "txHash", txHash, "address", addr,
			"utxoForm", rtypes.IsUtxoAddress(addr))
		return ErrInvalidDepositAddress
	}
	if err := r.checkDepositDuplicate(txHash, deposit); err != nil {
		return err
	}
	btcTx, err := r.validateBtcTxProof(txHash, deposit.GetTxProof())
	if err != nil {
		elog.Error("checkDeposit validate btc tx proof", "txHash", txHash, "btcProof", btcProof2String(deposit.GetTxProof()), "err", err)
		return err
	}
	// RGB20 分支：跳过链上 OP_RETURN 承诺与链上金额校验（RGB 金额在 consignment 内，由侧车验证），
	// 改验 TSS 阈值签名 thresholdSig（btcec 直验 C=sha256(Encode(DepositAsset{thresholdSig:nil}))）。
	if rtypes.IsRgb20Symbol(deposit.GetAssetSymbol()) {
		info, err := r.getCrossChainInfo(deposit.GetAssetSymbol())
		if err != nil {
			elog.Error("checkDeposit rgb20 getCrossChainInfo", "txHash", txHash, "symbol", deposit.GetAssetSymbol(), "err", err)
			return ErrGetCrossChainInfo
		}
		if err := verifyThresholdSig(info.GetPubkey(), deposit); err != nil {
			elog.Error("checkDeposit rgb20 verifyThresholdSig", "txHash", txHash, "symbol", deposit.GetAssetSymbol(), "err", err)
			return err
		}
	} else {
		// 硬切：充值只认 P2WSH 派生脚本（金额 + 归属一次判定），不再有 OP_RETURN 承诺。
		if err = r.validateDepositTxContent(txHash, deposit, btcTx); err != nil {
			return err
		}
	}
	// B8：链上最小确认数（放最后，理由见 checkBtcConfirmations 的注释）。
	return r.checkBtcConfirmations("deposit", txHash, deposit.GetTxProof())
}

func (r *rgbx) checkConfirm(fromAddr, txHash string, confirm *rtypes.ConfirmTx) error {

	confirmTxHash := hex.EncodeToString(confirm.TxHash)
	action := rtypes.GetActionName(confirm.GetActionType())

	if rgbxCfg.CommitAddress != "" && fromAddr != rgbxCfg.CommitAddress {
		elog.Error("checkConfirm fromAddr", "action", action,
			"txHash", txHash, "confirmTxHash", confirmTxHash,
			"fromAddr", fromAddr, "commitAddr", rgbxCfg.CommitAddress)
		return ErrInvalidCommitAddress
	}

	pendingTx := &rtypes.PendingTx{}
	err := readDB(r.GetLocalDB(), formatPendingTxKey(confirm.TxBlockHeight, confirm.TxIndex), pendingTx)
	if err != nil {
		elog.Error("checkConfirm read pending tx", "action", action,
			"txHash", txHash, "confirmTxHash", confirmTxHash,
			"height", confirm.TxBlockHeight, "index", confirm.TxIndex, "err", err)
		return ErrPendingTxNotExist
	}

	// 注意（E14）：这条 **不是** 去重的权威判据。Confirmed 由非共识路径的 ExecLocal_Confirm
	// 写进节点私有的 LocalDB —— 该写可被 exec.disableExecLocal 整块跳过、
	// localdb 与 stateDB 又是两次独立提交（存在崩溃窗口），因此它可能"陈旧"（该为 true 却为 false）。
	// 真正的共识权威判据是下面 checkConfirmNotUsed 查的 stateDB 键（Exec_Confirm 结算成功时
	// 随回执 KV 写入）。这条保留作纵深防御（早退、少一次 stateDB 读、错误码更贴近旧行为），
	// 但**去重不能依赖它**。
	if pendingTx.Confirmed {
		elog.Error("checkConfirm tx already confirmed", "action", action,
			"txHash", txHash, "confirmTxHash", confirmTxHash)
		return ErrTxAlreadyConfirmed
	}

	_, err = r.GetStateDB().Get(formatPayloadKey(confirm.GetTxHash()))
	if err != nil {
		elog.Error("checkConfirm get payload", "action", action,
			"txHash", txHash, "confirmTxHash", confirmTxHash, "err", err)
		return ErrConfirmPayloadNotExist
	}
	if !bytes.Equal(confirm.GetTxHash(), pendingTx.GetTxHash()) {
		elog.Error("checkConfirm tx hash not equal", "action", action,
			"txHash", txHash, "confirmTxHash", confirmTxHash,
			"expectConfirmHash", hex.EncodeToString(pendingTx.GetTxHash()))
		return ErrConfirmedHashNotEqual
	}

	if confirm.GetActionType() == rtypes.TyWithdrawAsset {
		if confirm.Timeout {
			elog.Error("checkConfirm timeout not supported for withdraw", "action", action,
				"txHash", txHash, "confirmTxHash", confirmTxHash)
			return ErrWithdrawConfirmTimeoutNotAllowed
		}
		return r.checkWithdrawConfirm(txHash, confirmTxHash, confirm, pendingTx)
	}

	if confirm.Timeout {
		elog.Debug("checkConfirm timeout", "action", action,
			"txHash", txHash, "confirmTxHash", confirmTxHash)
		return nil
	}

	// E14：mint / transfer 的确认去重（共识判据）。放在 timeout 早退之后：timeout 确认不结算
	// 任何资产（Exec_Confirm 直接返回空回执），其行为与本次修复前完全一致，不受影响。
	if err = r.checkConfirmNotUsed(confirm, txHash, confirmTxHash); err != nil {
		return err
	}

	// A2：SpendingTx 必须是规范编码（解析后无尾部多余字节）。btcwire 的 DeserializeNoWitness
	// 不做该项校验，给同一笔花费追加尾部字节后输入/OP_RETURN 承诺比对都不变、只有原始字节变，
	// 而归属 utxo id 由该交易导出 → 同一笔花费会被记到另一个 owner id。
	// 归属 id 的口径同时改为解析后的交易身份 TxHash()（见 Exec_Confirm），与充值侧 txid 口径一致。
	spendingTx, err := parseSpendingTxStrict(txHash, confirm.GetUtxoProof().GetSpendingTx())
	if err != nil {
		elog.Error("checkConfirm parse spending tx", "action", action,
			"txHash", txHash, "confirmTxHash", hex.EncodeToString(confirm.GetTxHash()),
			"btcSpendingTxLen", len(confirm.GetUtxoProof().GetSpendingTx()), "err", err)
		return err
	}
	btcSpendHash := spendingTx.TxHash().String()

	spendingInputIdx := int(confirm.GetUtxoProof().GetSpendingInputIdx())
	if spendingInputIdx >= len(spendingTx.TxIn) {
		elog.Error("checkConfirm spending tx input", "action", action,
			"txHash", txHash, "confirmTxHash", hex.EncodeToString(confirm.GetTxHash()),
			"inputIdx", spendingInputIdx, "txInLen", len(spendingTx.TxIn), "btcSpendHash", btcSpendHash)
		return ErrInvalidSpendingTxIn
	}

	// check input
	expectInput := pendingTx.Utxo.ToString()
	actualInput := spendingTx.TxIn[int(confirm.GetUtxoProof().GetSpendingInputIdx())].PreviousOutPoint.String()
	if expectInput != actualInput {
		elog.Error("checkConfirm input utxo not equal", "action", action,
			"txHash", txHash, "confirmTxHash", hex.EncodeToString(confirm.GetTxHash()),
			"expectInput", expectInput, "actualInput", actualInput, "btcSpendHash", btcSpendHash)
		return ErrSpendingInputNotEqual
	}

	opRetOutIdx := int(confirm.GetUtxoProof().GetOpRetOutputIdx())
	// 表示op_return输出不存在，即utxo已经在btc链花费, 但没有构建rgbx所约束的op_return输出
	if opRetOutIdx < 0 || opRetOutIdx >= len(spendingTx.TxOut) {
		elog.Debug("checkConfirm opReturn output not exist",
			"action", action, "txHash", txHash,
			"confirmTxHash", confirmTxHash, "btcSpendHash", btcSpendHash)
		return nil
	}

	// 提供的op_return pkScript参数非法，和btc原始交易中的输出不符
	if !bytes.Equal(confirm.GetUtxoProof().OpRetOutputPkScript,
		spendingTx.TxOut[int(confirm.GetUtxoProof().GetOpRetOutputIdx())].PkScript) {
		elog.Error("checkConfirm opReturn pkScript not equal",
			"action", action, "txHash", txHash,
			"confirmTxHash", confirmTxHash, "btcSpendHash", btcSpendHash)
		return ErrOpRetOutputPkScriptNotEqual
	}

	return nil
}

// checkConfirmNotUsed E14：mint / transfer 的确认去重守卫（共识判据）。
// 与提现侧 S3（checkWithdrawConfirm 里的 formatWithdrawUsedKey）完全对称：
// 键写在合约级 statedb 上，由 Exec_Confirm 在**结算成功**时随回执 KV 写入，
// 因此随 stateDB 一起回滚（重组重放不会误伤），且全网各节点读到的结论一致。
//
// 读的失败语义与 S3 一致（fail-closed）：ErrNotFound ⇒ 未结算过、放行；
// 其余任何错误（含键存在）⇒ 一律拒绝 —— 不把"读不到"当成"没结算过"。
func (r *rgbx) checkConfirmNotUsed(confirm *rtypes.ConfirmTx, txHash, confirmTxHash string) error {

	_, err := r.GetStateDB().Get(formatConfirmUsedKey(confirm.GetTxHash()))
	if errors.Is(err, types.ErrNotFound) {
		return nil
	}
	alreadyConfirmed := ErrTransferAlreadyConfirmed
	if confirm.GetActionType() == rtypes.TyMintAction {
		alreadyConfirmed = ErrMintAlreadyConfirmed
	}
	elog.Error("checkConfirm confirm already used", "action", rtypes.GetActionName(confirm.GetActionType()),
		"txHash", txHash, "confirmTxHash", confirmTxHash,
		"confirmHash", hex.EncodeToString(confirm.GetTxHash()), "err", err)
	return alreadyConfirmed
}
