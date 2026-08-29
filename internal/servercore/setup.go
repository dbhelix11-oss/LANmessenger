package servercore

import (
	"fmt"
	"log/slog"
	"os"

	"lanmessenger/internal/crypto"
)

// SetupOptions configures a fresh relay server.
type SetupOptions struct {
	ConfigPath           string
	Passphrase           string
	ListenAddr           string // optional; default used when empty
	DataDir              string // optional; default used when empty
	RequireAdminApproval bool
}

// Setup writes a new server config (with a passphrase verifier), creates the
// data directory, and generates the self-signed TLS certificate. It returns the
// certificate fingerprint that clients will pin.
//
// It refuses to overwrite an existing config file.
func Setup(opts SetupOptions, logger *slog.Logger) (cfg *Config, fingerprint string, err error) {
	if opts.ConfigPath == "" {
		return nil, "", fmt.Errorf("servercore: setup needs a config path")
	}
	if len(opts.Passphrase) < 6 {
		return nil, "", fmt.Errorf("servercore: passphrase must be at least 6 characters")
	}
	if _, statErr := os.Stat(opts.ConfigPath); statErr == nil {
		return nil, "", fmt.Errorf("servercore: %s already exists; delete it to re-run setup", opts.ConfigPath)
	}

	verifier, err := crypto.NewPassphraseVerifier(opts.Passphrase)
	if err != nil {
		return nil, "", err
	}

	cfg = &Config{
		ListenAddr:           opts.ListenAddr,
		DataDir:              opts.DataDir,
		Passphrase:           verifier,
		RequireAdminApproval: opts.RequireAdminApproval,
	}
	cfg.SetPath(opts.ConfigPath)
	cfg.applyDefaults()

	if err := cfg.Save(); err != nil {
		return nil, "", err
	}

	// Constructing a Server generates the DB + TLS cert as a side effect.
	srv, err := New(cfg, logger)
	if err != nil {
		return nil, "", err
	}
	defer srv.Close()

	fingerprint, err = srv.CertFingerprint()
	if err != nil {
		return nil, "", err
	}
	return cfg, fingerprint, nil
}
