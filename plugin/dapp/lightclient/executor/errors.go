package executor

import "errors"

var (
	ErrDecodeAction         = errors.New("ErrDecodeAction")
	ErrNilBtcHeader         = errors.New("ErrNilBtcHeader")
	ErrBtcGetLastHeader     = errors.New("ErrBtcGetLastHeader")
	ErrIllegalCommitAddress = errors.New("ErrIllegalCommitAddress")
	ErrBtcHeaderDisorder    = errors.New("ErrBtcHeaderDisorder")

	ErrBtcTargetBits       = errors.New("ErrBtcTargetBits")
	ErrToBtcWireHeader     = errors.New("ErrToBtcWireHeader")
	ErrInvalidBtcBlockHash = errors.New("ErrInvalidBtcBlockHash")
)
