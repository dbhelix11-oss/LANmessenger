// Package crypto holds the cryptographic primitives for lanmessenger: per-device
// identity keys, end-to-end message sealing, human-readable key fingerprints,
// and the household-passphrase challenge/response used at enrollment.
//
// Each device owns two long-lived key pairs:
//
//   - an ed25519 pair for signatures (admin actions, future key attestation)
//   - an X25519 pair for NaCl box authenticated encryption of messages
//
// The public halves are published to the relay server's directory. The private
// halves never leave the device and are stored 0600 on disk.
package crypto

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/nacl/box"
)

// Identity is a device's full key material, private parts included.
type Identity struct {
	SignPub  ed25519.PublicKey
	SignPriv ed25519.PrivateKey
	BoxPub   *[32]byte
	BoxPriv  *[32]byte
}

// PublicIdentity is the shareable half of an [Identity].
type PublicIdentity struct {
	SignPub ed25519.PublicKey
	BoxPub  *[32]byte
}

// GenerateIdentity creates a fresh device identity from crypto/rand.
func GenerateIdentity() (*Identity, error) {
	signPub, signPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("crypto: generate signing key: %w", err)
	}
	boxPub, boxPriv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("crypto: generate box key: %w", err)
	}
	return &Identity{SignPub: signPub, SignPriv: signPriv, BoxPub: boxPub, BoxPriv: boxPriv}, nil
}

// Public returns the shareable half of the identity.
func (id *Identity) Public() PublicIdentity {
	return PublicIdentity{SignPub: id.SignPub, BoxPub: id.BoxPub}
}

// DeviceID is a stable identifier derived from the signing public key: the first
// 16 bytes of its SHA-256, hex encoded. It changes only if the device's signing
// key changes (which peers surface as a key-change warning).
func (p PublicIdentity) DeviceID() string {
	sum := sha256.Sum256(p.SignPub)
	return hex.EncodeToString(sum[:16])
}

// DeviceID is shorthand for id.Public().DeviceID().
func (id *Identity) DeviceID() string { return id.Public().DeviceID() }

// --- on-disk persistence ---------------------------------------------------

type identityFile struct {
	Version  int    `json:"version"`
	SignPub  string `json:"sign_pub"`
	SignPriv string `json:"sign_priv"`
	BoxPub   string `json:"box_pub"`
	BoxPriv  string `json:"box_priv"`
}

const identityFileVersion = 1

// Save writes the identity to path as JSON with 0600 permissions, creating
// parent directories as needed.
func (id *Identity) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("crypto: create identity dir: %w", err)
	}
	f := identityFile{
		Version:  identityFileVersion,
		SignPub:  base64.StdEncoding.EncodeToString(id.SignPub),
		SignPriv: base64.StdEncoding.EncodeToString(id.SignPriv),
		BoxPub:   base64.StdEncoding.EncodeToString(id.BoxPub[:]),
		BoxPriv:  base64.StdEncoding.EncodeToString(id.BoxPriv[:]),
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("crypto: marshal identity: %w", err)
	}
	// Write atomically so a crash mid-write can't corrupt an existing identity.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("crypto: write identity: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("crypto: install identity: %w", err)
	}
	return nil
}

// LoadIdentity reads an identity previously written by [Identity.Save].
func LoadIdentity(path string) (*Identity, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err // callers check os.IsNotExist
	}
	var f identityFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("crypto: parse identity file: %w", err)
	}
	if f.Version != identityFileVersion {
		return nil, fmt.Errorf("crypto: unsupported identity file version %d", f.Version)
	}
	signPub, err := base64.StdEncoding.DecodeString(f.SignPub)
	if err != nil {
		return nil, fmt.Errorf("crypto: decode sign_pub: %w", err)
	}
	signPriv, err := base64.StdEncoding.DecodeString(f.SignPriv)
	if err != nil {
		return nil, fmt.Errorf("crypto: decode sign_priv: %w", err)
	}
	boxPub, err := decodeKey32(f.BoxPub)
	if err != nil {
		return nil, fmt.Errorf("crypto: decode box_pub: %w", err)
	}
	boxPriv, err := decodeKey32(f.BoxPriv)
	if err != nil {
		return nil, fmt.Errorf("crypto: decode box_priv: %w", err)
	}
	if len(signPub) != ed25519.PublicKeySize || len(signPriv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("crypto: identity file has malformed signing key")
	}
	return &Identity{
		SignPub:  ed25519.PublicKey(signPub),
		SignPriv: ed25519.PrivateKey(signPriv),
		BoxPub:   boxPub,
		BoxPriv:  boxPriv,
	}, nil
}

// LoadOrCreateIdentity returns the identity at path, generating and saving a new
// one if the file does not exist. The bool return is true when a new identity
// was created.
func LoadOrCreateIdentity(path string) (id *Identity, created bool, err error) {
	id, err = LoadIdentity(path)
	if err == nil {
		return id, false, nil
	}
	if !os.IsNotExist(err) {
		return nil, false, err
	}
	id, err = GenerateIdentity()
	if err != nil {
		return nil, false, err
	}
	if err := id.Save(path); err != nil {
		return nil, false, err
	}
	return id, true, nil
}

// --- encoding helpers ----------------------------------------------------

// EncodeKey base64-encodes a 32-byte key for the wire or the directory.
func EncodeKey(k *[32]byte) string { return base64.StdEncoding.EncodeToString(k[:]) }

// EncodeSignPub base64-encodes an ed25519 public key.
func EncodeSignPub(k ed25519.PublicKey) string { return base64.StdEncoding.EncodeToString(k) }

// DecodeKey32 parses a base64 32-byte key (box public/private).
func DecodeKey32(s string) (*[32]byte, error) { return decodeKey32(s) }

func decodeKey32(s string) (*[32]byte, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("crypto: key is %d bytes, want 32", len(b))
	}
	var k [32]byte
	copy(k[:], b)
	return &k, nil
}

// DecodeSignPub parses a base64 ed25519 public key.
func DecodeSignPub(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("crypto: ed25519 public key is %d bytes, want %d", len(b), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}
