// Package clientcore is the headless heart of a lanmessenger client:
// connection and reconnection to the relay, the auth/enrollment handshake,
// end-to-end encryption, a local SQLite store for history and the roster, and
// an event stream that a UI (the Fyne desktop app today, a web or mobile
// front-end later) renders.
package clientcore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Config is the client's persisted settings. It lives at
// <user config dir>/lanmessenger/config.json alongside identity.json and the
// local database.
type Config struct {
	// ServerAddr is the relay's host:port.
	ServerAddr string `json:"server_addr"`

	// CertFingerprint is the SHA-256 fingerprint of the relay's TLS
	// certificate, confirmed by the user on first connect and pinned
	// thereafter. Format: colon-separated hex pairs.
	CertFingerprint string `json:"cert_fingerprint"`

	// DisplayName is how this device appears in others' rosters.
	DisplayName string `json:"display_name"`

	// DeviceID is assigned by the relay at enrollment. Empty means "not yet
	// enrolled".
	DeviceID string `json:"device_id"`

	// DownloadsDir is where received files are written. Empty uses the platform
	// default (~/Downloads/lanmessenger).
	DownloadsDir string `json:"downloads_dir,omitempty"`

	// MaxFileBytes is the largest file this client will send or accept. 0 uses
	// the default (100 MiB).
	MaxFileBytes int64 `json:"max_file_bytes,omitempty"`

	dir string // directory this config was loaded from
}

// DefaultMaxFileBytes is the fallback cap for file transfers.
const DefaultMaxFileBytes = 100 << 20

// MaxFile returns the configured file-size cap or the default.
func (c *Config) MaxFile() int64 {
	if c.MaxFileBytes > 0 {
		return c.MaxFileBytes
	}
	return DefaultMaxFileBytes
}

// DefaultDir returns <user config dir>/lanmessenger.
func DefaultDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("clientcore: locate config dir: %w", err)
	}
	return filepath.Join(base, "lanmessenger"), nil
}

// LoadConfig reads config.json from dir. A missing file yields a zero Config
// (not an error) so first-run setup can populate it.
func LoadConfig(dir string) (*Config, error) {
	c := &Config{dir: dir}
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, fmt.Errorf("clientcore: read config: %w", err)
	}
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("clientcore: parse config: %w", err)
	}
	c.dir = dir
	return c, nil
}

// Save writes config.json atomically with 0600 permissions.
func (c *Config) Save() error {
	if c.dir == "" {
		return fmt.Errorf("clientcore: config has no directory")
	}
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return fmt.Errorf("clientcore: create config dir: %w", err)
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("clientcore: marshal config: %w", err)
	}
	path := filepath.Join(c.dir, "config.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("clientcore: write config: %w", err)
	}
	return os.Rename(tmp, path)
}

// Dir returns the configuration directory.
func (c *Config) Dir() string { return c.dir }

// SetDir sets the configuration directory (used before the first Save).
func (c *Config) SetDir(dir string) { c.dir = dir }

// IdentityPath is where the device key pair is stored.
func (c *Config) IdentityPath() string { return filepath.Join(c.dir, "identity.json") }

// DBPath is the local SQLite database location.
func (c *Config) DBPath() string { return filepath.Join(c.dir, "client.db") }

// ResolvedDownloadsDir returns DownloadsDir or the platform default, creating it.
func (c *Config) ResolvedDownloadsDir() (string, error) {
	dir := c.DownloadsDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, "Downloads", "lanmessenger")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("clientcore: create downloads dir: %w", err)
	}
	return dir, nil
}

// Configured reports whether the client has enough to attempt a connection.
func (c *Config) Configured() bool {
	return c.ServerAddr != "" && c.CertFingerprint != "" && c.DisplayName != ""
}

// Enrolled reports whether this device has completed enrollment.
func (c *Config) Enrolled() bool { return c.DeviceID != "" }
