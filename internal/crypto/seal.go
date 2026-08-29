package crypto

import (
	"crypto/rand"
	"fmt"
	"io"

	"golang.org/x/crypto/nacl/box"
)

// NonceSize is the length of a NaCl box nonce.
const NonceSize = 24

// Seal encrypts and authenticates plaintext from the sender to the recipient
// using NaCl box (X25519 + XSalsa20-Poly1305). It returns a fresh random nonce
// and the ciphertext. The recipient recovers the plaintext with [Open] using
// the sender's box public key.
func Seal(plaintext []byte, senderBoxPriv, recipientBoxPub *[32]byte) (nonce [NonceSize]byte, ciphertext []byte, err error) {
	if _, err = io.ReadFull(rand.Reader, nonce[:]); err != nil {
		return nonce, nil, fmt.Errorf("crypto: read nonce: %w", err)
	}
	ciphertext = box.Seal(nil, plaintext, &nonce, recipientBoxPub, senderBoxPriv)
	return nonce, ciphertext, nil
}

// Open reverses [Seal]. It returns an error if authentication fails, which
// happens on a tampered ciphertext, a wrong nonce, or the wrong key pair.
func Open(nonce [NonceSize]byte, ciphertext []byte, senderBoxPub, recipientBoxPriv *[32]byte) ([]byte, error) {
	plaintext, ok := box.Open(nil, ciphertext, &nonce, senderBoxPub, recipientBoxPriv)
	if !ok {
		return nil, fmt.Errorf("crypto: message failed authentication (tampered, wrong key, or wrong nonce)")
	}
	return plaintext, nil
}

// NonceFromSlice copies exactly NonceSize bytes into a nonce array.
func NonceFromSlice(b []byte) ([NonceSize]byte, error) {
	var n [NonceSize]byte
	if len(b) != NonceSize {
		return n, fmt.Errorf("crypto: nonce is %d bytes, want %d", len(b), NonceSize)
	}
	copy(n[:], b)
	return n, nil
}
