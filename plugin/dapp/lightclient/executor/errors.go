package executor

import "errors"

var (
	ErrDecodeAction         = errors.New("ErrDecodeAction")
	ErrNilBtcHeader         = errors.New("ErrNilBtcHeader")
	ErrBtcGetLastHeader     = errors.New("ErrBtcGetLastHeader")
	ErrIllegalCommitAddress = errors.New("ErrIllegalCommitAddress")
	ErrBtcHeaderDisorder    = errors.New("ErrBtcHeaderDisorder")
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
