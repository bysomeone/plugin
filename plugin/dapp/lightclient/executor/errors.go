package executor

import "errors"

var (
	ErrDecodeAction         = errors.New("ErrDecodeAction")
	ErrNilBtcHeader         = errors.New("ErrNilBtcHeader")
	ErrBtcGetLastHeader     = errors.New("ErrBtcGetLastHeader")
	ErrIllegalCommitAddress = errors.New("ErrIllegalCommitAddress")
	ErrBtcHeaderDisorder    = errors.New("ErrBtcHeaderDisorder")
	// ErrBtcHeadersTooMany 单笔交易提交的头数超过上限
	ErrBtcHeadersTooMany = errors.New("ErrBtcHeadersTooMany")
	// ErrBtcHeaderDuplicateHeight 同一笔交易内的头高度不是逐个 +1（重复高度或跳高）
	ErrBtcHeaderDuplicateHeight = errors.New("ErrBtcHeaderDuplicateHeight")
	// ErrBtcHeaderNoAnchor bootstrap 时的首个头无法锚定到该网络的真实链
	ErrBtcHeaderNoAnchor = errors.New("ErrBtcHeaderNoAnchor")
	// ErrBtcHeaderUnknownAncestor 首个头的父块不是 canonical 链最近窗口里的已知区块（分叉点无从确认）
	ErrBtcHeaderUnknownAncestor = errors.New("ErrBtcHeaderUnknownAncestor")
	// ErrBtcReorgTooDeep 分叉点比当前 tip 旧超过 maxBtcReorgDepth，超出允许回退的深度
	ErrBtcReorgTooDeep = errors.New("ErrBtcReorgTooDeep")
	// ErrBtcHeaderContextMissing 挂载点在 canonical 链上，但拿不到它的完整头（localdb 缺数据），无法校验
	ErrBtcHeaderContextMissing = errors.New("ErrBtcHeaderContextMissing")
	// ErrBtcHeaderNotCanonical localdb 里该高度的头与 statedb canonical 窗口里的节点不一致
	// （读 localdb 做共识判定的交叉校验失败，见 btc_index_guard.go）
	ErrBtcHeaderNotCanonical = errors.New("ErrBtcHeaderNotCanonical")
	// ErrBtcLocalIndexMismatch localdb 在共识 tip 高度上没有与 btc-lastheader 一致的头
	ErrBtcLocalIndexMismatch = errors.New("ErrBtcLocalIndexMismatch")
	// ErrBtcLocalIndexUnavailable 节点没有可用的 localdb（exec.disableExecLocal / 未绑定）
	ErrBtcLocalIndexUnavailable = errors.New("ErrBtcLocalIndexUnavailable")

	ErrBtcTargetBits        = errors.New("ErrBtcTargetBits")
	ErrBtcHeaderTimeTooOld  = errors.New("ErrBtcHeaderTimeTooOld")
	ErrBtcHeaderTimeTooNew  = errors.New("ErrBtcHeaderTimeTooNew")
	ErrBtcHeaderInvalidTime = errors.New("ErrBtcHeaderInvalidTime")
	ErrToBtcWireHeader      = errors.New("ErrToBtcWireHeader")
	ErrInvalidBtcBlockHash  = errors.New("ErrInvalidBtcBlockHash")
	ErrBtcHeaderVerify      = errors.New("ErrBtcHeaderVerify")
)
