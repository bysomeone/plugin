package types

import (
	"reflect"

	"github.com/33cn/chain33/types"
	//"reflect"
)

/*
 * 交易相关类型定义
 * 交易action通常有对应的log结构，用于交易回执日志记录
 * 每一种action和log需要用id数值和name名称加以区分
 */

// action类型id和name，这些常量可以自定义修改
const (
	TyUnknownAction = iota + 100
	TyMintAction
	TyTransferAction
	TyConfirmAction
	TyCommitDKGAction
	TyDepositAsset
	TyWithdrawAsset
	// TyCrossChainMetaAction 跨链资产元数据回填（精度），见 proto 的 crossChainAssetMeta。
	TyCrossChainMetaAction
	// TyUnlockAsset 放弃一笔尚未结算的提现，把锁定金额退回发起人（见 proto 的 unlockAsset）。
	// 必须追加在末尾：action id 是共识判据，插入中间会改变既有 action 的编号。
	TyUnlockAsset
	// TyBridgeConfigApply / TyBridgeConfigApprove 桥配置的申请与生效（主链 autonomy 治理）。
	TyBridgeConfigApply
	TyBridgeConfigApprove
	// TyAbandonWithdraw 业务侧放弃一笔提现（链上只存证，节点用本地白名单受理）。
	TyAbandonWithdraw
	// TyRegisterDepositScript 绑定用户专属充值地址（派生 + 登记 + 回执）。
	TyRegisterDepositScript
	// TyBumpWithdrawFee 提现发起人请求提高一笔尚未结算提现的矿工费率（见 proto 的 bumpWithdrawFee）。
	// 必须追加在末尾：action id 是共识判据，插入中间会改变既有 action 的编号。
	TyBumpWithdrawFee

	NameMintAction      = "Mint"
	NameTransferAction  = "Transfer"
	NameConfirmAction   = "Confirm"
	NameCommitDKGAction = "CommitDKG"
	// 须与 RgbxAction protobuf oneof 字段名一致（chain33 ExecTypeBase.SetChild 用 strings.ToLower 映射），
	// 例如 oneof deposit -> 此处为 "Deposit"，不能为 "DepositAsset"。
	NameDepositAssetAction  = "Deposit"
	NameWithdrawAssetAction = "Withdraw"
	// NameCrossChainMetaAction 对应 oneof 字段 crossChainMeta。
	NameCrossChainMetaAction = "CrossChainMeta"
	// NameUnlockAssetAction 对应 oneof 字段 unlock（不是 UnlockAsset）。
	NameUnlockAssetAction = "Unlock"
	// NameBridgeConfigApplyAction / NameBridgeConfigApproveAction 对应 oneof 字段
	// bridgeConfigApply / bridgeConfigApprove。
	NameBridgeConfigApplyAction   = "BridgeConfigApply"
	NameBridgeConfigApproveAction = "BridgeConfigApprove"
	// NameAbandonWithdrawAction 对应 oneof 字段 abandon（不是 AbandonWithdraw）。
	NameAbandonWithdrawAction = "Abandon"
	// NameRegisterDepositScriptAction 对应 oneof 字段 registerDepositScript。
	NameRegisterDepositScriptAction = "RegisterDepositScript"
	// NameBumpWithdrawFeeAction 对应 oneof 字段 bumpWithdrawFee。
	NameBumpWithdrawFeeAction = "BumpWithdrawFee"
)

// bridgeConfigApply 的单项更新掩码里使用的字段名。取值与 proto 字段名逐字一致：
// 掩码是链上治理的输入，改这里等于改 wire 契约。Apply 校验与 Exec 合并共用同一份
// 名称集合，避免两处字符串漂移。
//
// assetSymbol 与 fields 本身不在其中：前者标识记录、后者是掩码，都不允许被掩码修改。
const (
	BridgeConfigFieldLimitsEnabled                  = "limitsEnabled"
	BridgeConfigFieldMaxPerTx                       = "maxPerTx"
	BridgeConfigFieldMaxPerWindow1H                 = "maxPerWindow1h"
	BridgeConfigFieldMaxPerWindow24H                = "maxPerWindow24h"
	BridgeConfigFieldMaxMintPerWindow               = "maxMintPerWindow"
	BridgeConfigFieldLargeDepositThreshold          = "largeDepositThreshold"
	BridgeConfigFieldLargeDepositExtraConfirmations = "largeDepositExtraConfirmations"
	BridgeConfigFieldPauseDeposit                   = "pauseDeposit"
	BridgeConfigFieldPauseWithdraw                  = "pauseWithdraw"
	// BridgeConfigFieldWithdrawMaxFeeRateSatPerVbyte BTC 计价提现的费率上限（sat/vB）。
	BridgeConfigFieldWithdrawMaxFeeRateSatPerVbyte = "withdrawMaxFeeRateSatPerVbyte"
	// BridgeConfigFieldRgb20WithdrawFeeLockVbytes RGB20 BTC 通道的保守交易体积（vB）。
	BridgeConfigFieldRgb20WithdrawFeeLockVbytes = "rgb20WithdrawFeeLockVbytes"
	// BridgeConfigFieldRgb20WithdrawFee RGB20 资产固定手续费列表（掩码命中时整体替换）。
	BridgeConfigFieldRgb20WithdrawFee = "rgb20WithdrawFee"
)

// log类型id值
const (
	TyUnknownLog = iota + 100
	TyAssetLog
	TyPendingTxLog
	TyCommitDKGLog
	TyDepositAssetLog
	TyWithdrawAssetLog
	// TyCrossChainMetaLog 跨链资产元数据回填（精度）的回执日志。
	TyCrossChainMetaLog
	// TyUnlockAssetLog 放弃提现（退回锁定金额）的回执日志。
	TyUnlockAssetLog
	// TyAbandonWithdrawLog 放弃提现提案（链上只存证）的回执日志。必须追加在末尾：
	// log id 会写进回执，插在中间会改变既有 log 的编号。
	TyAbandonWithdrawLog
	// TyDepositScriptLog 用户充值地址绑定成功的回执日志（带完整地址信息）。
	TyDepositScriptLog
	// TyBumpWithdrawFeeLog 提现加价请求（链上只存证 + 发事件）的回执日志。必须追加在末尾：
	// log id 会写进回执，插在中间会改变既有 log 的编号。
	TyBumpWithdrawFeeLog

	NameAssetLog         = "AssetLog"
	NamePendingTxLog     = "PendingTxLog"
	NameCommitDKGLog     = "CommitDKGLog"
	NameDepositAssetLog  = "DepositAssetLog"
	NameWithdrawAssetLog = "WithdrawAssetLog"
	// NameCrossChainMetaLog 回执里记录"哪个符号的精度被回填成了什么"，便于按区块核对链上值。
	NameCrossChainMetaLog = "CrossChainMetaLog"
	// NameUnlockAssetLog 回执里记录"哪一笔提现被放弃、退给了谁、退了多少"。
	NameUnlockAssetLog = "UnlockAssetLog"
	// NameAbandonWithdrawLog 回执里记录一条已被链上存证的放弃提现提案。
	NameAbandonWithdrawLog = "AbandonWithdrawLog"
	// NameDepositScriptLog 回执里记录用户绑定的充值地址、脚本与规格。
	NameDepositScriptLog = "DepositScriptLog"
	// NameBumpWithdrawFeeLog 回执里记录一条已被链上存证的提现加价请求。
	NameBumpWithdrawFeeLog = "BumpWithdrawFeeLog"
)

var (
	//RgbxX 执行器名称定义
	RgbxX = "rgbx"
	//定义actionMap
	actionMap = map[string]int32{
		NameMintAction:          TyMintAction,
		NameTransferAction:      TyTransferAction,
		NameConfirmAction:       TyConfirmAction,
		NameCommitDKGAction:     TyCommitDKGAction,
		NameDepositAssetAction:  TyDepositAsset,
		NameWithdrawAssetAction: TyWithdrawAsset,

		NameCrossChainMetaAction: TyCrossChainMetaAction,
		NameUnlockAssetAction:    TyUnlockAsset,

		NameBridgeConfigApplyAction:   TyBridgeConfigApply,
		NameBridgeConfigApproveAction: TyBridgeConfigApprove,
		NameAbandonWithdrawAction:     TyAbandonWithdraw,

		NameRegisterDepositScriptAction: TyRegisterDepositScript,

		NameBumpWithdrawFeeAction: TyBumpWithdrawFee,
	}
	//定义log的id和具体log类型及名称，填入具体自定义log类型
	logMap = map[int64]*types.LogInfo{
		TyAssetLog:     {Ty: reflect.TypeOf(RgbxAsset{}), Name: NameAssetLog},
		TyPendingTxLog: {Ty: reflect.TypeOf(PendingTx{}), Name: NamePendingTxLog},
		TyCommitDKGLog: {Ty: reflect.TypeOf(types.ReqAddrs{}), Name: NameCommitDKGLog},
		// 回填日志带的是载荷（symbol + precision），比"只留一条 KV"更适合按区块核对链上精度。
		TyCrossChainMetaLog:  {Ty: reflect.TypeOf(CrossChainAssetMeta{}), Name: NameCrossChainMetaLog},
		TyUnlockAssetLog:     {Ty: reflect.TypeOf(UnlockAsset{}), Name: NameUnlockAssetLog},
		TyAbandonWithdrawLog: {Ty: reflect.TypeOf(AbandonWithdrawItem{}), Name: NameAbandonWithdrawLog},
		TyDepositScriptLog:   {Ty: reflect.TypeOf(DepositScriptItem{}), Name: NameDepositScriptLog},
		TyBumpWithdrawFeeLog: {Ty: reflect.TypeOf(BumpWithdrawFeeItem{}), Name: NameBumpWithdrawFeeLog},
	}
)

// init defines a register function
func init() {
	types.AllowUserExec = append(types.AllowUserExec, []byte(RgbxX))
	//注册合约启用高度
	types.RegFork(RgbxX, InitFork)
	types.RegExec(RgbxX, InitExecutor)
}

// InitFork defines register fork
func InitFork(cfg *types.Chain33Config) {
	cfg.RegisterDappFork(RgbxX, "Enable", 0)
}

// InitExecutor defines register executor
func InitExecutor(cfg *types.Chain33Config) {
	types.RegistorExecutor(RgbxX, NewType(cfg))
}

type rgbxType struct {
	types.ExecTypeBase
}

func NewType(cfg *types.Chain33Config) *rgbxType {
	c := &rgbxType{}
	c.SetChild(c)
	c.SetConfig(cfg)
	return c
}

// GetPayload 获取合约action结构
func (r *rgbxType) GetPayload() types.Message {
	return &RgbxAction{}
}

// GetTypeMap 获取合约action的id和name信息
func (r *rgbxType) GetTypeMap() map[string]int32 {
	return actionMap
}

// GetLogMap 获取合约log相关信息
func (r *rgbxType) GetLogMap() map[int64]*types.LogInfo {
	return logMap
}

// GetActionName get action name by action type
func GetActionName(ty int32) string {
	switch ty {
	case TyMintAction:
		return NameMintAction
	case TyTransferAction:
		return NameTransferAction
	case TyConfirmAction:
		return NameConfirmAction
	case TyCommitDKGAction:
		return NameCommitDKGAction
	case TyDepositAsset:
		return NameDepositAssetAction
	case TyWithdrawAsset:
		return NameWithdrawAssetAction
	case TyCrossChainMetaAction:
		return NameCrossChainMetaAction
	case TyUnlockAsset:
		return NameUnlockAssetAction
	case TyBridgeConfigApply:
		return NameBridgeConfigApplyAction
	case TyBridgeConfigApprove:
		return NameBridgeConfigApproveAction
	case TyAbandonWithdraw:
		return NameAbandonWithdrawAction
	case TyRegisterDepositScript:
		return NameRegisterDepositScriptAction
	case TyBumpWithdrawFee:
		return NameBumpWithdrawFeeAction
	default:
		return "unknownAction"
	}
}
