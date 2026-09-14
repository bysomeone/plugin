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

	ErrBtcTargetBits        = errors.New("ErrBtcTargetBits")
	ErrBtcHeaderTimeTooOld  = errors.New("ErrBtcHeaderTimeTooOld")
	ErrBtcHeaderTimeTooNew  = errors.New("ErrBtcHeaderTimeTooNew")
	ErrBtcHeaderInvalidTime = errors.New("ErrBtcHeaderInvalidTime")
	ErrToBtcWireHeader      = errors.New("ErrToBtcWireHeader")
	ErrInvalidBtcBlockHash  = errors.New("ErrInvalidBtcBlockHash")
	ErrBtcHeaderVerify      = errors.New("ErrBtcHeaderVerify")
)
