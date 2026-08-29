// Command lanmsg-server runs the lanmessenger relay: device directory, presence
// fan-out, end-to-end-encrypted message relay, and an offline queue.
//
// Usage:
//
//	lanmsg-server setup [-config path] [-listen host:port] [-approval]
//	lanmsg-server run   [-config path]
//	lanmsg-server fingerprint [-config path]
//
// `setup` prompts for the household passphrase, writes the config, and prints
// the TLS certificate fingerprint each client must confirm on first connect.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/term"

	"lanmessenger/internal/servercore"
)

const defaultConfigPath = "lanmsg-server.toml"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	var err error
	switch os.Args[1] {
	case "setup":
		err = cmdSetup(os.Args[2:], logger)
	case "run":
		err = cmdRun(os.Args[2:], logger)
	case "fingerprint":
		err = cmdFingerprint(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `lanmsg-server — lanmessenger relay

  lanmsg-server setup [-config path] [-listen host:port] [-data dir] [-approval]
      Create a new config, generate the TLS certificate, and print its fingerprint.

  lanmsg-server run [-config path]
      Run the relay.

  lanmsg-server fingerprint [-config path]
      Print the TLS certificate fingerprint clients must confirm.
`)
}

func cmdSetup(args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, "path to write the config file")
	listen := fs.String("listen", "", "listen address, host:port (default 0.0.0.0:8443)")
	dataDir := fs.String("data", "", "data directory (default: 'data' next to the config)")
	approval := fs.Bool("approval", false, "require admin approval for every new device")
	passArg := fs.String("passphrase", "", "household passphrase (omit to be prompted)")
	_ = fs.Parse(args)

	passphrase := *passArg
	if passphrase == "" {
		var err error
		passphrase, err = promptPassphrase()
		if err != nil {
			return err
		}
	}

	cfg, fp, err := servercore.Setup(servercore.SetupOptions{
		ConfigPath:           *cfgPath,
		Passphrase:           passphrase,
		ListenAddr:           *listen,
		DataDir:              *dataDir,
		RequireAdminApproval: *approval,
	}, logger)
	if err != nil {
		return err
	}

	fmt.Printf(`
Relay configured.

  config file : %s
  listen addr : %s
  admin approval: %v

TLS certificate fingerprint (SHA-256) — every client confirms this on first connect:

  %s

Start the relay with:  lanmsg-server run -config %s
`, *cfgPath, cfg.ListenAddr, cfg.RequireAdminApproval, fp, *cfgPath)
	return nil
}

func cmdRun(args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, "path to the config file")
	_ = fs.Parse(args)

	cfg, err := servercore.LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	srv, err := servercore.New(cfg, logger)
	if err != nil {
		return err
	}
	if fp, err := srv.CertFingerprint(); err == nil {
		logger.Info("tls certificate fingerprint", "sha256", fp)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := srv.Run(ctx); err != nil {
		return err
	}
	logger.Info("relay stopped")
	return nil
}

func cmdFingerprint(args []string) error {
	fs := flag.NewFlagSet("fingerprint", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, "path to the config file")
	_ = fs.Parse(args)

	cfg, err := servercore.LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	fp, err := servercore.CertFingerprintFromFile(cfg.CertPath())
	if err != nil {
		return err
	}
	fmt.Println(fp)
	return nil
}

func promptPassphrase() (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", errors.New("no terminal for passphrase prompt; pass -passphrase")
	}
	fmt.Fprint(os.Stderr, "Household passphrase: ")
	b1, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	fmt.Fprint(os.Stderr, "Confirm passphrase:   ")
	b2, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	p1, p2 := strings.TrimSpace(string(b1)), strings.TrimSpace(string(b2))
	if p1 != p2 {
		return "", errors.New("passphrases did not match")
	}
	return p1, nil
}
