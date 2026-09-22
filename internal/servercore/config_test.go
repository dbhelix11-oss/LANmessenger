package servercore

import (
	"os"
	"path/filepath"
	"testing"

	"lanmessenger/internal/crypto"
)

func TestConfigSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.toml")

	v, err := crypto.NewPassphraseVerifier("open sesame")
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	c := &Config{
		ListenAddr:           "127.0.0.1:9443",
		Passphrase:           v,
		RequireAdminApproval: true,
		AdminDevices:         []string{"devA"},
	}
	c.SetPath(path)
	c.applyDefaults()
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got.ListenAddr != "127.0.0.1:9443" || !got.RequireAdminApproval {
		t.Fatalf("scalars not preserved: %+v", got)
	}
	if !got.IsAdmin("devA") || got.IsAdmin("devB") {
		t.Fatalf("admin list not preserved: %+v", got.AdminDevices)
	}
	if got.Passphrase.Salt != v.Salt || got.Passphrase.DerivedB64 != v.DerivedB64 {
		t.Fatalf("passphrase verifier not preserved")
	}
	// Defaults applied for unset fields.
	if got.MaxFrameBytes == 0 || got.HeartbeatSeconds == 0 || got.QueueRetentionHours == 0 {
		t.Fatalf("defaults not applied: %+v", got)
	}
}

func TestConfigMinClientVersionRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.toml")

	v, err := crypto.NewPassphraseVerifier("open sesame")
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	c := &Config{Passphrase: v, MinClientVersion: "0.5.0"}
	c.SetPath(path)
	c.applyDefaults()
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got.MinClientVersion != "0.5.0" {
		t.Fatalf("MinClientVersion = %q, want 0.5.0", got.MinClientVersion)
	}
}

func TestConfigMinClientVersionDefaultsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.toml")

	v, err := crypto.NewPassphraseVerifier("open sesame")
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	c := &Config{Passphrase: v}
	c.SetPath(path)
	c.applyDefaults()
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got.MinClientVersion != "" {
		t.Fatalf("MinClientVersion = %q, want empty (no floor)", got.MinClientVersion)
	}
}

func TestLoadConfigRejectsMissingPassphrase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.toml")
	if err := os.WriteFile(path, []byte(`listen_addr = "127.0.0.1:1"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected error for config with no passphrase")
	}
}

func TestAddAdmin(t *testing.T) {
	c := &Config{}
	if !c.AddAdmin("d1") {
		t.Fatal("first AddAdmin should report added")
	}
	if c.AddAdmin("d1") {
		t.Fatal("duplicate AddAdmin should report not added")
	}
	if !c.IsAdmin("d1") {
		t.Fatal("IsAdmin false after AddAdmin")
	}
}

func TestConfigPathResolution(t *testing.T) {
	c := &Config{DataDir: "data"}
	c.SetPath("/etc/lanmsg/server.toml")
	if got := c.DBPath(); got != "/etc/lanmsg/data/server.db" {
		t.Fatalf("DBPath = %q", got)
	}
	if got := c.CertPath(); got != "/etc/lanmsg/data/server.crt" {
		t.Fatalf("CertPath = %q", got)
	}

	abs := &Config{DataDir: "/var/lib/lanmsg"}
	abs.SetPath("/etc/lanmsg/server.toml")
	if got := abs.DBPath(); got != "/var/lib/lanmsg/server.db" {
		t.Fatalf("absolute DataDir DBPath = %q", got)
	}
}
