package neutrino

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// 签名节点侧的落盘关卡：每个参与付款签名的节点在签名成功之后、报告成功之前写记录；
// 写失败不得把签名结果当成可用；持有记录的节点必须拒绝同一 burn 的 Unlock 组签名。

type memSignedPaymentStore struct {
	signed map[string]bool
	putErr error
	hasErr error
}

func (m *memSignedPaymentStore) Has(burnTxHash []byte) (bool, error) {
	if m.hasErr != nil {
		return false, m.hasErr
	}
	return m.signed[string(burnTxHash)], nil
}

func (m *memSignedPaymentStore) PutOnce(burnTxHash []byte) error {
	if m.putErr != nil {
		return m.putErr
	}
	if m.signed == nil {
		m.signed = make(map[string]bool)
	}
	m.signed[string(burnTxHash)] = true
	return nil
}

// Test_signerRecordsPaymentBeforeReportingSuccess 签名成功 ⇒ 记录已写、handler 才报告成功；
// 该节点随后 hasSignedPayment 为真。
func Test_signerRecordsPaymentBeforeReportingSuccess(t *testing.T) {
	n := &neutrinoClient{signedPayments: &memSignedPaymentStore{}}
	tss := &tssService{client: n}
	burn := []byte("burn-hash")

	signed := false
	released := false
	err := tss.signAndRecordOnParticipant(burn, func() error {
		signed = true
		return nil
	})
	if err == nil {
		released = true
	}
	require.NoError(t, err)
	require.True(t, signed, "the signing round must have run")
	require.True(t, released, "success is reported only after the record is persisted")
	require.True(t, n.hasSignedPayment(burn), "the signer node must hold its own record")
}

// Test_signerRecordFailureDoesNotReleaseSignature 写失败 ⇒ handler 不报告成功（不释放签名结果）。
func Test_signerRecordFailureDoesNotReleaseSignature(t *testing.T) {
	n := &neutrinoClient{signedPayments: &memSignedPaymentStore{putErr: errors.New("disk full")}}
	tss := &tssService{client: n}
	burn := []byte("burn-hash")

	released := false
	err := tss.signAndRecordOnParticipant(burn, func() error { return nil })
	if err == nil {
		released = true
	}
	require.ErrorContains(t, err, "disk full")
	require.False(t, released, "a signature whose record could not be persisted must not be released")
	// Even though the durable write failed, this process must still refuse an unlock for the burn.
	require.True(t, n.hasSignedPayment(burn), "in-memory fail-safe after a failed durable write")
	require.ErrorContains(t, tss.ensureNoPaymentSignature(burn), "already produced a payment signature")
}

// Test_signerSigningFailureDoesNotWriteRecord 签名轮次失败 ⇒ 不写记录（失败签名不得让节点
// 永久拒绝后续合法的 Unlock）。
func Test_signerSigningFailureDoesNotWriteRecord(t *testing.T) {
	n := &neutrinoClient{signedPayments: &memSignedPaymentStore{}}
	tss := &tssService{client: n}
	burn := []byte("burn-hash")

	err := tss.signAndRecordOnParticipant(burn, func() error { return errors.New("signing round failed") })
	require.ErrorContains(t, err, "signing round failed")
	require.False(t, n.hasSignedPayment(burn))
}

// Test_signerRefusesUnlockAfterPaymentSignature 门限交集安全：本节点签过付款 ⇒ 拒绝 Unlock；
// 未签过 ⇒ 允许参与。
func Test_signerRefusesUnlockAfterPaymentSignature(t *testing.T) {
	burn := []byte("burn-hash")

	signed := &neutrinoClient{signedPayments: &memSignedPaymentStore{signed: map[string]bool{string(burn): true}}}
	err := (&tssService{client: signed}).ensureNoPaymentSignature(burn)
	require.ErrorContains(t, err, "already produced a payment signature")

	clean := &neutrinoClient{signedPayments: &memSignedPaymentStore{}}
	require.NoError(t, (&tssService{client: clean}).ensureNoPaymentSignature(burn))
}
