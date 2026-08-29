package crypto

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Fingerprint returns a short, human-verifiable digest of a peer's public keys,
// formatted as groups of four hex characters, e.g. "1a2b-3c4d-5e6f-7a8b". Two
// family members can read these aloud across the house to confirm they are
// talking to the right device (trust on first use, verified out of band).
//
// It hashes both the signing and box public keys so that swapping either one
// changes the fingerprint.
func Fingerprint(signPub ed25519.PublicKey, boxPub *[32]byte) string {
	h := sha256.New()
	h.Write(signPub)
	h.Write(boxPub[:])
	sum := h.Sum(nil)

	const groups = 4 // 4 groups * 2 bytes = 8 bytes of digest shown
	enc := hex.EncodeToString(sum[:groups*2])

	var b strings.Builder
	for i := 0; i < len(enc); i += 4 {
		if i > 0 {
			b.WriteByte('-')
		}
		b.WriteString(enc[i : i+4])
	}
	return b.String()
}

// FingerprintPublic is [Fingerprint] for a [PublicIdentity].
func FingerprintPublic(p PublicIdentity) string {
	return Fingerprint(p.SignPub, p.BoxPub)
}
