package crypto

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/nacl/secretbox"
)

// AdminKeyFile is the on-disk form of the household admin key. The private key
// is sealed with a key derived (Argon2id) from a passphrase, so the file alone
// is useless to a thief. Keep it in a dedicated directory (~/.lanmsg-keys/).
type AdminKeyFile struct {
	Pub    string `json:"pub"`    // base64 ed25519 public key (goes in the relay config)
	Salt   string `json:"salt"`   // base64 Argon2id salt
	Nonce  string `json:"nonce"`  // base64 secretbox nonce
	Sealed string `json:"sealed"` // base64 secretbox of the private key
}

// DefaultAdminKeyDir is where admin and other lanmsg keys live.
func DefaultAdminKeyDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".lanmsg-keys"), nil
}

// DefaultAdminKeyPath is DefaultAdminKeyDir()/admin.key. LANMSG_ADMIN_KEY
// overrides it.
func DefaultAdminKeyPath() (string, error) {
	if p := os.Getenv("LANMSG_ADMIN_KEY"); p != "" {
		return p, nil
	}
	d, err := DefaultAdminKeyDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "admin.key"), nil
}

// GenerateAdminKey creates a new admin keypair, seals it with passphrase and
// writes it to path (mode 0600, parent 0700). It refuses to overwrite.
func GenerateAdminKey(path, passphrase string) (pubB64 string, err error) {
	if len(passphrase) < 8 {
		return "", errors.New("crypto: admin key passphrase must be at least 8 characters")
	}
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("crypto: %s already exists; refusing to overwrite an admin key", path)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	salt, err := NewSalt()
	if err != nil {
		return "", err
	}
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	var k [32]byte
	copy(k[:], DeriveKey(passphrase, salt))
	sealed := secretbox.Seal(nil, priv, &nonce, &k)
	f := AdminKeyFile{
		Pub:    EncodeSignPub(pub),
		Salt:   base64.StdEncoding.EncodeToString(salt),
		Nonce:  base64.StdEncoding.EncodeToString(nonce[:]),
		Sealed: base64.StdEncoding.EncodeToString(sealed),
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return f.Pub, nil
}

// ErrAdminKeyPassphrase means the passphrase did not unlock the key.
var ErrAdminKeyPassphrase = errors.New("crypto: wrong admin key passphrase")

// ReadAdminKeyPub returns the public half without needing the passphrase.
func ReadAdminKeyPub(path string) (string, error) {
	f, err := readAdminKeyFile(path)
	if err != nil {
		return "", err
	}
	return f.Pub, nil
}

func readAdminKeyFile(path string) (AdminKeyFile, error) {
	var f AdminKeyFile
	raw, err := os.ReadFile(path)
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return f, fmt.Errorf("crypto: parse admin key: %w", err)
	}
	return f, nil
}

// LoadAdminKey unlocks the admin private key at path.
func LoadAdminKey(path, passphrase string) (ed25519.PrivateKey, error) {
	f, err := readAdminKeyFile(path)
	if err != nil {
		return nil, err
	}
	salt, err1 := base64.StdEncoding.DecodeString(f.Salt)
	nb, err2 := base64.StdEncoding.DecodeString(f.Nonce)
	sealed, err3 := base64.StdEncoding.DecodeString(f.Sealed)
	if err1 != nil || err2 != nil || err3 != nil || len(nb) != 24 {
		return nil, errors.New("crypto: malformed admin key file")
	}
	var nonce [24]byte
	copy(nonce[:], nb)
	var k [32]byte
	copy(k[:], DeriveKey(passphrase, salt))
	priv, ok := secretbox.Open(nil, sealed, &nonce, &k)
	if !ok {
		return nil, ErrAdminKeyPassphrase
	}
	if len(priv) != ed25519.PrivateKeySize {
		return nil, errors.New("crypto: malformed admin private key")
	}
	return ed25519.PrivateKey(priv), nil
}

// NewAdminAuthNonce returns a random nonce for an admin frame.
func NewAdminAuthNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(b), nil
}
