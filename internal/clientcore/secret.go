package clientcore

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// secretFile stores the household passphrase locally so a desktop client can
// reconnect after a restart without prompting every time. It sits in the same
// 0600-protected config directory as the device's private key. It is never sent
// to the relay in the clear — only an Argon2id+HMAC proof derived from it is.
type secretFile struct {
	Passphrase string `json:"passphrase"`
}

func secretPath(dir string) string { return filepath.Join(dir, "secret.json") }

func loadSecret(dir string) (string, error) {
	raw, err := os.ReadFile(secretPath(dir))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var s secretFile
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", err
	}
	return s.Passphrase, nil
}

func saveSecret(dir, passphrase string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(secretFile{Passphrase: passphrase})
	if err != nil {
		return err
	}
	path := secretPath(dir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
