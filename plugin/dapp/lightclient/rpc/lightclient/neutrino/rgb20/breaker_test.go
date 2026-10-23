package rgb20

import "testing"

func TestWithdrawBreakerCountsDistinctBurns(t *testing.T) {
	b := newWithdrawBreaker(3)
	// Retrying the same withdrawal must not trip the breaker.
	for i := 0; i < 10; i++ {
		if b.recordMismatch("burn-a") {
			t.Fatal("breaker tripped on repeated refusals of the same burn")
		}
	}
	if b.isTripped() {
		t.Fatal("breaker is open with one distinct burn")
	}
	if b.recordMismatch("burn-b") {
		t.Fatal("breaker tripped before the threshold")
	}
	if !b.recordMismatch("burn-c") {
		t.Fatal("breaker did not trip at the threshold")
	}
	if !b.isTripped() || b.tripReason() == "" {
		t.Fatal("breaker state or reason missing after tripping")
	}
}

func TestWithdrawBreakerDefaultsAndDisable(t *testing.T) {
	if got := newWithdrawBreaker(0).threshold; got != 3 {
		t.Fatalf("default threshold = %d, want 3", got)
	}
	b := newWithdrawBreaker(-1)
	for _, key := range []string{"a", "b", "c", "d", "e"} {
		if b.recordMismatch(key) {
			t.Fatal("a disabled breaker must never trip")
		}
	}
	if b.isTripped() {
		t.Fatal("disabled breaker reports tripped")
	}
	var nilBreaker *withdrawBreaker
	if nilBreaker.isTripped() || nilBreaker.recordMismatch("x") {
		t.Fatal("nil breaker must be inert")
	}
}

func TestAdapterWithdrawalsBlockedBySafetyConfig(t *testing.T) {
	a := &Adapter{cfg: Config{Safety: SafetyConfig{DisableWithdrawals: true}},
		breaker: newWithdrawBreaker(0)}
	if reason := a.WithdrawalsBlocked(); reason == "" {
		t.Fatal("local kill switch is not reported")
	}
	var nilAdapter *Adapter
	if reason := nilAdapter.WithdrawalsBlocked(); reason != "" {
		t.Fatalf("nil adapter reported a block: %s", reason)
	}
	if nilAdapter.DepositsDisabled() {
		t.Fatal("nil adapter reports deposits disabled")
	}
}
