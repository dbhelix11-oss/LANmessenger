package crypto

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters for stretching the household passphrase. These are modest
// on purpose: enrollment happens rarely and on hardware as small as a Raspberry
// Pi. 64 MiB / 1 pass / 4 lanes.
const (
	argonTime    = 1
	argonMemory  = 64 * 1024
	argonThreads = 4
	argonKeyLen  = 32

	// SaltSize is the length of the Argon2id salt stored in the server config.
	SaltSize = 16
	// ChallengeNonceSize is the length of the per-connection auth nonce.
	ChallengeNonceSize = 32
)

// DeriveKey stretches passphrase with Argon2id and the given salt into a
// 32-byte key. The server stores this key (not the passphrase) as its verifier;
// clients recompute it to answer the auth challenge.
func DeriveKey(passphrase string, salt []byte) []byte {
	return argon2.IDKey([]byte(passphrase), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
}

// NewSalt returns a fresh random Argon2id salt.
func NewSalt() ([]byte, error) {
	return randBytes(SaltSize)
}

// NewChallengeNonce returns a fresh random nonce for one auth challenge.
func NewChallengeNonce() ([]byte, error) {
	return randBytes(ChallengeNonceSize)
}

// Proof computes the value a client sends in response to an auth challenge:
// HMAC-SHA256(derivedKey, nonce).
func Proof(derivedKey, nonce []byte) []byte {
	mac := hmac.New(sha256.New, derivedKey)
	mac.Write(nonce)
	return mac.Sum(nil)
}

// VerifyProof reports whether proof matches the expected HMAC for derivedKey and
// nonce, using a constant-time comparison.
func VerifyProof(derivedKey, nonce, proof []byte) bool {
	want := Proof(derivedKey, nonce)
	return subtle.ConstantTimeCompare(want, proof) == 1
}

// PassphraseVerifier is what the server persists to authenticate clients: the
// Argon2id salt and the derived key, both base64. It never contains the
// passphrase itself.
type PassphraseVerifier struct {
	Salt       string `json:"salt" toml:"salt"`
	DerivedB64 string `json:"key" toml:"key"`
}

// NewPassphraseVerifier builds a verifier for a chosen passphrase, generating a
// fresh salt.
func NewPassphraseVerifier(passphrase string) (PassphraseVerifier, error) {
	salt, err := NewSalt()
	if err != nil {
		return PassphraseVerifier{}, err
	}
	key := DeriveKey(passphrase, salt)
	return PassphraseVerifier{
		Salt:       base64.StdEncoding.EncodeToString(salt),
		DerivedB64: base64.StdEncoding.EncodeToString(key),
	}, nil
}

// SaltBytes decodes the verifier's salt.
func (v PassphraseVerifier) SaltBytes() ([]byte, error) {
	return base64.StdEncoding.DecodeString(v.Salt)
}

// DerivedKey decodes the verifier's stored key.
func (v PassphraseVerifier) DerivedKey() ([]byte, error) {
	return base64.StdEncoding.DecodeString(v.DerivedB64)
}

// CheckResponse verifies a client's base64 proof for the given base64 nonce
// against this verifier.
func (v PassphraseVerifier) CheckResponse(nonceB64, proofB64 string) (bool, error) {
	key, err := v.DerivedKey()
	if err != nil {
		return false, fmt.Errorf("crypto: decode verifier key: %w", err)
	}
	nonce, err := base64.StdEncoding.DecodeString(nonceB64)
	if err != nil {
		return false, fmt.Errorf("crypto: decode nonce: %w", err)
	}
	proof, err := base64.StdEncoding.DecodeString(proofB64)
	if err != nil {
		return false, fmt.Errorf("crypto: decode proof: %w", err)
	}
	return VerifyProof(key, nonce, proof), nil
}

func randBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return nil, fmt.Errorf("crypto: read random: %w", err)
	}
	return b, nil
}
