package rgb20

import (
	"fmt"
	"sync"
)

// withdrawBreaker is the node-local automatic circuit breaker for withdrawals.
//
// It trips when the bridge refuses several *distinct* burns for the same systematic reason:
// the transfer pays the user an amount that does not match "chain33 pending − configured fee".
// One such refusal can be a bad invoice; a handful in a row means the bridge's accounting or
// configuration is wrong and every further signature risks paying the wrong amount. Once
// tripped, this node stops signing new withdrawals until it is restarted (the trip is
// intentionally not self-clearing: an operator must look at why it happened).
type withdrawBreaker struct {
	mu        sync.Mutex
	threshold int // negative disables the breaker
	seen      map[string]struct{}
	tripped   bool
}

func newWithdrawBreaker(threshold int) *withdrawBreaker {
	if threshold == 0 {
		threshold = 3
	}
	return &withdrawBreaker{threshold: threshold, seen: make(map[string]struct{})}
}

// isTripped reports whether the breaker is open.
func (b *withdrawBreaker) isTripped() bool {
	if b == nil || b.threshold < 0 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tripped
}

// recordMismatch notes one burn refused for a payout mismatch and returns true when this
// refusal opens the breaker. Counting distinct burns keeps a retrying single withdrawal (the
// scheduler retries the same pending) from tripping it.
func (b *withdrawBreaker) recordMismatch(burnHash string) (trippedNow bool) {
	if b == nil || b.threshold < 0 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tripped {
		return false
	}
	b.seen[burnHash] = struct{}{}
	if len(b.seen) < b.threshold {
		return false
	}
	b.tripped = true
	return true
}

// tripReason returns the operator-facing cause, or "" when the breaker is closed.
func (b *withdrawBreaker) tripReason() string {
	if !b.isTripped() {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return fmt.Sprintf("%d distinct withdrawals were refused for a payout mismatch", len(b.seen))
}
