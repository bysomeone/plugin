// Package depositrequest defines the user-signed deposit request.
//
// The request travels off chain (user -> gateway -> bridge) and is carried on chain later, inside
// the mint transaction. Keeping the canonical encoding and the verification here means the
// gateway, the bridge and the executor all agree on exactly what was signed.
//
// What the signature binds:
//   - the chain title, so a request signed for one chain cannot be replayed on another;
//   - the request id, so the mint can reject a replay of the same request;
//   - the chain33 address that will receive the minted asset;
//   - the asset symbol and amount, so neither the bridge nor the gateway can redirect or resize
//     the mint;
//   - a timestamp, which is recorded for audit (it is not an expiry).
//
// The key management (who holds the private key) is outside this package: callers only produce
// and verify the signature.
package depositrequest

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/33cn/chain33/common/address"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
)

const (
	// Domain separates this message from every other use of the same key (notably transaction
	// signatures). Changing it invalidates every previously signed request.
	Domain = "rgbx-deposit-request:v1"
	// CompressedPubKeyLen is the only accepted public key form (33 bytes).
	CompressedPubKeyLen = 33
	// MaxRequestIDLen bounds the request id carried in the mint payload.
	MaxRequestIDLen = 64
	// MaxSymbolLen mirrors the asset-symbol bound used by the executor.
	MaxSymbolLen = 32
)

var (
	ErrInvalidRequest   = errors.New("invalid deposit request")
	ErrInvalidPubKey    = errors.New("invalid deposit request public key")
	ErrInvalidSignature = errors.New("invalid deposit request signature")
	ErrAddressMismatch  = errors.New("deposit request signature does not match the address")
	ErrUnsupportedAddr  = errors.New("deposit request address is not decodable")
)

// Request is the canonical content of a deposit request. The signature covers every field.
type Request struct {
	RequestID   string
	Chain33Addr string
	AssetSymbol string
	Amount      int64
	Timestamp   int64
}

// Validate checks the shape of the fields. It does not check the signature.
func (r Request) Validate() error {
	if r.RequestID == "" || len(r.RequestID) > MaxRequestIDLen || !isRequestIDCharset(r.RequestID) {
		return fmt.Errorf("%w: request id %q", ErrInvalidRequest, r.RequestID)
	}
	if r.Chain33Addr == "" {
		return fmt.Errorf("%w: empty address", ErrInvalidRequest)
	}
	if r.AssetSymbol == "" || len(r.AssetSymbol) > MaxSymbolLen {
		return fmt.Errorf("%w: asset symbol %q", ErrInvalidRequest, r.AssetSymbol)
	}
	if r.Amount <= 0 {
		return fmt.Errorf("%w: amount %d", ErrInvalidRequest, r.Amount)
	}
	// Timestamp is audit data; 0 is not a valid unix time.
	if r.Timestamp <= 0 {
		return fmt.Errorf("%w: timestamp %d", ErrInvalidRequest, r.Timestamp)
	}
	return nil
}

// CanonicalMessage returns the exact bytes the user signs.
//
// Every field is newline terminated and validated to contain no newline, so no combination of
// field contents can produce the same message as a different request.
func CanonicalMessage(chainTitle string, r Request) []byte {
	return []byte(strings.Join([]string{
		Domain,
		chainTitle,
		r.RequestID,
		r.Chain33Addr,
		r.AssetSymbol,
		strconv.FormatInt(r.Amount, 10),
		strconv.FormatInt(r.Timestamp, 10),
	}, "\n"))
}

// Verify checks that signature over the canonical message was produced by the key behind
// r.Chain33Addr. pubKey is the compressed public key (33 bytes) and signature is the DER
// encoding produced by the chain33 secp256k1 signer.
func Verify(chainTitle string, r Request, pubKey, signature []byte) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if len(pubKey) != CompressedPubKeyLen {
		return fmt.Errorf("%w: length %d", ErrInvalidPubKey, len(pubKey))
	}
	pub, err := btcec.ParsePubKey(pubKey)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidPubKey, err)
	}
	sig, err := ecdsa.ParseDERSignature(signature)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSignature, err)
	}
	if !sig.Verify(MessageHash(chainTitle, r), pub) {
		return ErrInvalidSignature
	}
	addrType, err := address.GetAddressType(r.Chain33Addr)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnsupportedAddr, err)
	}
	if derived := address.PubKeyToAddr(addrType, pubKey); derived != r.Chain33Addr {
		return fmt.Errorf("%w: signed key belongs to %s", ErrAddressMismatch, derived)
	}
	return nil
}

// MessageHash returns sha256(CanonicalMessage). The chain33 secp256k1 signer signs this hash, and
// the executor verifies over the same value.
func MessageHash(chainTitle string, r Request) []byte {
	sum := sha256.Sum256(CanonicalMessage(chainTitle, r))
	return sum[:]
}

func isRequestIDCharset(s string) bool {
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}
