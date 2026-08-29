package crypto

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
)

// Sign produces a base64 ed25519 signature over msg using the device's signing
// key. Used for admin approve/deny actions.
func Sign(priv ed25519.PrivateKey, msg []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg))
}

// Verify checks a base64 ed25519 signature produced by [Sign].
func Verify(pub ed25519.PublicKey, msg []byte, sigB64 string) bool {
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return false
	}
	return ed25519.Verify(pub, msg, sig)
}

// VerifyFromB64Pub is [Verify] taking a base64-encoded public key, as stored in
// the server directory.
func VerifyFromB64Pub(pubB64 string, msg []byte, sigB64 string) (bool, error) {
	pub, err := DecodeSignPub(pubB64)
	if err != nil {
		return false, fmt.Errorf("crypto: decode signer key: %w", err)
	}
	return Verify(pub, msg, sigB64), nil
}
