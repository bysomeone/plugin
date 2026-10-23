package neutrino

import (
	"strings"
	"testing"
	"time"
)

func TestCheckWalletBirthday(t *testing.T) {
	birthday := time.Unix(1_700_000_000, 0)

	// A fresh wallet created on a non-empty chain misses everything before it existed.
	got := checkWalletBirthday(true, birthday, 100)
	if got == "" {
		t.Fatal("expected a warning for a fresh wallet on a non-empty chain")
	}
	if !strings.Contains(got, "100") || !strings.Contains(got, birthday.UTC().Format(time.RFC3339)) {
		t.Fatalf("warning omits the height or birthday: %s", got)
	}

	// A restored wallet (opened, not created) is the normal recovery path: no warning.
	if got := checkWalletBirthday(false, birthday, 100); got != "" {
		t.Fatalf("unexpected warning for a restored wallet: %s", got)
	}
	// A fresh wallet on an empty/fresh chain has nothing to miss.
	if got := checkWalletBirthday(true, birthday, 0); got != "" {
		t.Fatalf("unexpected warning at genesis: %s", got)
	}
}
