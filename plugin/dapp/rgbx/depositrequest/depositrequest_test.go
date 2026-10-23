package depositrequest

import (
	"errors"
	"testing"

	"github.com/33cn/chain33/common/address"
	// Registers the default (btc) address driver for address derivation.
	_ "github.com/33cn/chain33/system/address/btc"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
)

const testChainTitle = "user.p.test."

func signedRequest(t *testing.T) (Request, []byte, []byte) {
	t.Helper()
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatalf("new key: %v", err)
	}
	pub := priv.PubKey().SerializeCompressed()
	req := Request{
		RequestID:   "req-1",
		Chain33Addr: address.PubKeyToAddr(-1, pub),
		AssetSymbol: "RGB20_USDT",
		Amount:      100_000,
		Timestamp:   1_700_000_000,
	}
	sig := ecdsa.Sign(priv, MessageHash(testChainTitle, req)).Serialize()
	return req, pub, sig
}

func TestVerifyAcceptsAValidRequest(t *testing.T) {
	req, pub, sig := signedRequest(t)
	if err := Verify(testChainTitle, req, pub, sig); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerifyRejectsTampering(t *testing.T) {
	req, pub, sig := signedRequest(t)

	tamperedAmount := req
	tamperedAmount.Amount++
	if err := Verify(testChainTitle, tamperedAmount, pub, sig); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("amount tampered: err = %v, want ErrInvalidSignature", err)
	}

	tamperedAddr := req
	tamperedAddr.Chain33Addr = address.PubKeyToAddr(-1, pub) // same key: must pass; then a foreign one
	if err := Verify(testChainTitle, tamperedAddr, pub, sig); err != nil {
		t.Fatalf("same address: %v", err)
	}
	other, _, _ := signedRequest(t)
	tamperedAddr.Chain33Addr = other.Chain33Addr
	if err := Verify(testChainTitle, tamperedAddr, pub, sig); err == nil {
		t.Fatal("foreign address accepted")
	}

	if err := Verify("user.p.other.", req, pub, sig); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("chain title tampered: err = %v, want ErrInvalidSignature", err)
	}

	if err := Verify(testChainTitle, req, pub, []byte("not-a-signature")); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("bad signature encoding: err = %v", err)
	}
}

func TestVerifyRejectsAForeignKey(t *testing.T) {
	req, _, sig := signedRequest(t)
	other, otherPub, _ := signedRequest(t)
	_ = other
	if err := Verify(testChainTitle, req, otherPub, sig); err == nil {
		t.Fatal("signature accepted with another key")
	}
}

func TestVerifyRejectsABadShape(t *testing.T) {
	req, pub, sig := signedRequest(t)
	bad := req
	bad.Amount = 0
	if err := Verify(testChainTitle, bad, pub, sig); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("zero amount: err = %v", err)
	}
	bad = req
	bad.RequestID = "has space"
	if err := Verify(testChainTitle, bad, pub, sig); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("request id charset: err = %v", err)
	}
	if err := Verify(testChainTitle, req, pub[:32], sig); !errors.Is(err, ErrInvalidPubKey) {
		t.Fatalf("short pubkey: err = %v", err)
	}
}

func TestCanonicalMessageIsUnambiguous(t *testing.T) {
	a := Request{RequestID: "a", Chain33Addr: "b", AssetSymbol: "c", Amount: 1, Timestamp: 2}
	b := Request{RequestID: "a\nb", Chain33Addr: "c", AssetSymbol: "1", Amount: 2, Timestamp: 2}
	// b is rejected by Validate, and its raw encoding must not collide with a's.
	if string(CanonicalMessage("t", a)) == string(CanonicalMessage("t", b)) {
		t.Fatal("canonical messages collide")
	}
	if err := b.Validate(); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("newline in request id accepted: %v", err)
	}
}
