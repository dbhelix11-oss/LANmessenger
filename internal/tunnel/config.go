package tunnel

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"

	"lanmessenger/internal/crypto"
)

// Config is cmd/lanmsg-tunnel's on-disk configuration (TOML). Unlike the
// relay's config, it has no data directory, no database, and no message
// state — the cloud tunnel holds nothing at rest.
type Config struct {
	// PublicListenAddr is where remote clients dial in (loopback-only in the
	// deployed setup — a local Tor hidden service maps a public .onion
	// virtual port down to it).
	PublicListenAddr string `toml:"public_listen_addr"`

	// BackendListenAddr is where the home relay dials in (also
	// loopback-only, reached via its own .onion virtual port).
	BackendListenAddr string `toml:"backend_listen_addr"`

	// BackendSecret is the verifier for the shared secret that authenticates
	// a connecting backend. The secret itself is never stored — only its
	// Argon2id salt and derived key, the same shape as the relay's own
	// household-passphrase verifier, but a wholly separate secret.
	BackendSecret crypto.PassphraseVerifier `toml:"backend_secret"`

	RateLimit RateLimitConfig `toml:"rate_limit"`

	path string
}

// RateLimitConfig caps how many new public connections are allowed per
// source IP per window.
type RateLimitConfig struct {
	MaxConnectsPerWindow int `toml:"max_connects_per_window"`
	ConnectWindowSeconds int `toml:"connect_window_seconds"`
}

// Defaults for fields left unset in the TOML file.
const (
	defaultPublicListenAddr  = "127.0.0.1:8443"
	defaultBackendListenAddr = "127.0.0.1:9443"

	defaultMaxConnectsPerWindow = 20
	defaultConnectWindowSeconds = 60

	// minSecretLen guards against an accidentally weak shared secret. The
	// secret is meant to be machine-generated (see `lanmsg-tunnel setup`),
	// not human-memorable, so this floor is far higher than the household
	// passphrase's.
	minSecretLen = 16
)

func (c *Config) applyDefaults() {
	if c.PublicListenAddr == "" {
		c.PublicListenAddr = defaultPublicListenAddr
	}
	if c.BackendListenAddr == "" {
		c.BackendListenAddr = defaultBackendListenAddr
	}
	if c.RateLimit.MaxConnectsPerWindow == 0 {
		c.RateLimit.MaxConnectsPerWindow = defaultMaxConnectsPerWindow
	}
	if c.RateLimit.ConnectWindowSeconds == 0 {
		c.RateLimit.ConnectWindowSeconds = defaultConnectWindowSeconds
	}
}

// LoadConfig reads and validates a TOML config file.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("tunnel: read config: %w", err)
	}
	var c Config
	if err := toml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("tunnel: parse config: %w", err)
	}
	c.path = path
	c.applyDefaults()
	if c.BackendSecret.Salt == "" || c.BackendSecret.DerivedB64 == "" {
		return nil, fmt.Errorf("tunnel: config has no backend secret; run `lanmsg-tunnel setup` first")
	}
	return &c, nil
}

// Save writes the config back to its file as TOML with 0600 permissions.
func (c *Config) Save() error {
	if c.path == "" {
		return fmt.Errorf("tunnel: config has no path")
	}
	f, err := os.OpenFile(c.path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("tunnel: open config for write: %w", err)
	}
	defer f.Close()
	if err := toml.NewEncoder(f).Encode(c); err != nil {
		return fmt.Errorf("tunnel: encode config: %w", err)
	}
	return nil
}

// SetPath records where this config should be saved (used by [Setup]).
func (c *Config) SetPath(path string) { c.path = path }

// SetupOptions configures a fresh cloud tunnel.
type SetupOptions struct {
	ConfigPath        string
	Secret            string
	PublicListenAddr  string // optional; default used when empty
	BackendListenAddr string // optional; default used when empty
}

// Setup writes a new tunnel config with a verifier for the given shared
// secret. It refuses to overwrite an existing config file.
func Setup(opts SetupOptions) (*Config, error) {
	if opts.ConfigPath == "" {
		return nil, fmt.Errorf("tunnel: setup needs a config path")
	}
	if len(opts.Secret) < minSecretLen {
		return nil, fmt.Errorf("tunnel: secret must be at least %d characters", minSecretLen)
	}
	if _, statErr := os.Stat(opts.ConfigPath); statErr == nil {
		return nil, fmt.Errorf("tunnel: %s already exists; delete it to re-run setup", opts.ConfigPath)
	}

	verifier, err := crypto.NewPassphraseVerifier(opts.Secret)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		PublicListenAddr:  opts.PublicListenAddr,
		BackendListenAddr: opts.BackendListenAddr,
		BackendSecret:     verifier,
	}
	cfg.SetPath(opts.ConfigPath)
	cfg.applyDefaults()

	if err := cfg.Save(); err != nil {
		return nil, err
	}
	return cfg, nil
}
