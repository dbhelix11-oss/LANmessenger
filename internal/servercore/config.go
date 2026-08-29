package servercore

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"lanmessenger/internal/crypto"
)

// Config is the relay server's on-disk configuration (TOML). Create it with
// `lanmsg-server setup`, which fills in Passphrase and generates the TLS cert.
type Config struct {
	// ListenAddr is the host:port the TLS WebSocket listener binds to.
	ListenAddr string `toml:"listen_addr"`

	// DataDir holds the SQLite database and the TLS certificate/key. Relative
	// paths are resolved against the config file's directory.
	DataDir string `toml:"data_dir"`

	// Passphrase is the household passphrase verifier (Argon2id salt + derived
	// key). The passphrase itself is never stored.
	Passphrase crypto.PassphraseVerifier `toml:"passphrase"`

	// RequireAdminApproval, when true, holds every newly enrolled device in the
	// "pending" state until an admin device approves it. New devices can
	// authenticate but cannot send, receive, or see the directory until then.
	RequireAdminApproval bool `toml:"require_admin_approval"`

	// AdminDevices lists the device IDs allowed to approve/deny pending devices.
	// The first device to enroll on a fresh server is added here automatically.
	AdminDevices []string `toml:"admin_devices"`

	// QueueRetentionHours is how long an undelivered queued message lives before
	// it is dropped. 0 means keep until delivered.
	QueueRetentionHours int `toml:"queue_retention_hours"`

	// MaxQueuePerDevice caps how many undelivered messages are held for one
	// recipient. Oldest are dropped past the cap. 0 means unlimited.
	MaxQueuePerDevice int `toml:"max_queue_per_device"`

	// MaxFrameBytes rejects any single wire frame larger than this. This bounds
	// per-chunk file payloads too.
	MaxFrameBytes int `toml:"max_frame_bytes"`

	// HeartbeatSeconds is the server->client ping interval. A client that misses
	// two consecutive pongs is considered offline and disconnected.
	HeartbeatSeconds int `toml:"heartbeat_seconds"`

	// path is the location this config was loaded from; used to resolve DataDir.
	path string
}

// Defaults for fields left unset in the TOML file.
const (
	defaultListenAddr       = "0.0.0.0:8443"
	defaultDataDir          = "data"
	defaultQueueRetentionH  = 168 // 7 days
	defaultMaxQueuePerDev   = 500
	defaultMaxFrameBytes    = 2 << 20 // 2 MiB; a sealed+base64 512 KiB file chunk is ~0.95 MiB
	defaultHeartbeatSeconds = 30
)

func (c *Config) applyDefaults() {
	if c.ListenAddr == "" {
		c.ListenAddr = defaultListenAddr
	}
	if c.DataDir == "" {
		c.DataDir = defaultDataDir
	}
	if c.QueueRetentionHours == 0 {
		c.QueueRetentionHours = defaultQueueRetentionH
	}
	if c.MaxQueuePerDevice == 0 {
		c.MaxQueuePerDevice = defaultMaxQueuePerDev
	}
	if c.MaxFrameBytes == 0 {
		c.MaxFrameBytes = defaultMaxFrameBytes
	}
	if c.HeartbeatSeconds == 0 {
		c.HeartbeatSeconds = defaultHeartbeatSeconds
	}
}

// LoadConfig reads and validates a TOML config file.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("servercore: read config: %w", err)
	}
	var c Config
	if err := toml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("servercore: parse config: %w", err)
	}
	c.path = path
	c.applyDefaults()
	if c.Passphrase.Salt == "" || c.Passphrase.DerivedB64 == "" {
		return nil, fmt.Errorf("servercore: config has no passphrase; run `lanmsg-server setup` first")
	}
	return &c, nil
}

// Save writes the config back to its file as TOML with 0600 permissions.
func (c *Config) Save() error {
	if c.path == "" {
		return fmt.Errorf("servercore: config has no path")
	}
	f, err := os.OpenFile(c.path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("servercore: open config for write: %w", err)
	}
	defer f.Close()
	if err := toml.NewEncoder(f).Encode(c); err != nil {
		return fmt.Errorf("servercore: encode config: %w", err)
	}
	return nil
}

// SetPath records where this config should be saved (used by `setup`).
func (c *Config) SetPath(path string) { c.path = path }

// resolve turns a possibly-relative path into one anchored at the config's
// directory.
func (c *Config) resolve(p string) string {
	if filepath.IsAbs(p) || c.path == "" {
		return p
	}
	return filepath.Join(filepath.Dir(c.path), p)
}

// DBPath is the SQLite database location.
func (c *Config) DBPath() string { return filepath.Join(c.resolve(c.DataDir), "server.db") }

// CertPath is the TLS certificate location.
func (c *Config) CertPath() string { return filepath.Join(c.resolve(c.DataDir), "server.crt") }

// KeyPath is the TLS private key location.
func (c *Config) KeyPath() string { return filepath.Join(c.resolve(c.DataDir), "server.key") }

// EnsureDataDir creates the data directory if it does not exist.
func (c *Config) EnsureDataDir() error {
	return os.MkdirAll(c.resolve(c.DataDir), 0o700)
}

// IsAdmin reports whether deviceID is in the AdminDevices list.
func (c *Config) IsAdmin(deviceID string) bool {
	for _, d := range c.AdminDevices {
		if d == deviceID {
			return true
		}
	}
	return false
}

// AddAdmin appends deviceID to AdminDevices if not already present, returning
// whether it was added.
func (c *Config) AddAdmin(deviceID string) bool {
	if c.IsAdmin(deviceID) {
		return false
	}
	c.AdminDevices = append(c.AdminDevices, deviceID)
	return true
}
