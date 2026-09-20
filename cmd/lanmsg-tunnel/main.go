// Command lanmsg-tunnel runs the cloud side of the LANmessenger cloud
// tunnel: a small, stateless byte-forwarder that lets remote clients reach
// the home relay without any inbound port on the home network. It holds no
// application data — no roster, no database, no message queue, no E2E
// keys — only a shared secret proving a connecting backend is the real home
// relay. See docs/DESIGN.md's cloud-tunnel section for the full design.
//
// Usage:
//
//	lanmsg-tunnel setup [-config path] [-secret secret] [-public-listen host:port] [-backend-listen host:port]
//	lanmsg-tunnel run   [-config path]
//
// `setup` generates a random shared secret if none is given, writes the
// config, and prints the secret to copy into the home relay's own
// server.toml [tunnel] block.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"lanmessenger/internal/ratelimit"
	"lanmessenger/internal/tunnel"
)

const defaultConfigPath = "lanmsg-tunnel.toml"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	var err error
	switch os.Args[1] {
	case "setup":
		err = cmdSetup(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:], logger)
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
	fmt.Fprint(os.Stderr, `lanmsg-tunnel — LANmessenger cloud tunnel (stateless byte forwarder)

  lanmsg-tunnel setup [-config path] [-secret secret] [-public-listen host:port] [-backend-listen host:port]
      Create a new config. Generates a random shared secret if -secret is
      omitted; copy the printed secret into the home relay's own
      server.toml [tunnel] block.

  lanmsg-tunnel run [-config path]
      Run the tunnel.
`)
}

func cmdSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, "path to write the config file")
	secretArg := fs.String("secret", "", "shared secret authenticating the home relay (default: randomly generated)")
	publicListen := fs.String("public-listen", "", "public listener address, host:port (default 127.0.0.1:8443)")
	backendListen := fs.String("backend-listen", "", "backend listener address, host:port (default 127.0.0.1:9443)")
	_ = fs.Parse(args)

	secret := *secretArg
	generated := false
	if secret == "" {
		var err error
		secret, err = randomSecret()
		if err != nil {
			return err
		}
		generated = true
	}

	cfg, err := tunnel.Setup(tunnel.SetupOptions{
		ConfigPath:        *cfgPath,
		Secret:            secret,
		PublicListenAddr:  *publicListen,
		BackendListenAddr: *backendListen,
	})
	if err != nil {
		return err
	}

	fmt.Printf(`
Tunnel configured.

  config file          : %s
  public listen addr   : %s (remote clients dial in here, via Tor)
  backend listen addr  : %s (the home relay dials in here, via Tor)
`, *cfgPath, cfg.PublicListenAddr, cfg.BackendListenAddr)

	if generated {
		fmt.Printf(`
Shared secret (copy this into the home relay's server.toml [tunnel] block,
"secret" field — it is not stored anywhere in plaintext, and this is the
only time it is shown):

  %s
`, secret)
	}
	fmt.Printf("\nStart the tunnel with:  lanmsg-tunnel run -config %s\n", *cfgPath)
	return nil
}

func cmdRun(args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, "path to the config file")
	_ = fs.Parse(args)

	cfg, err := tunnel.LoadConfig(*cfgPath)
	if err != nil {
		return err
	}

	limiter := ratelimit.New(cfg.RateLimit.MaxConnectsPerWindow, time.Duration(cfg.RateLimit.ConnectWindowSeconds)*time.Second)
	hub := tunnel.NewHub(cfg.BackendSecret, limiter, logger)

	backendLn, err := net.Listen("tcp", cfg.BackendListenAddr)
	if err != nil {
		return fmt.Errorf("listen backend %s: %w", cfg.BackendListenAddr, err)
	}
	publicLn, err := net.Listen("tcp", cfg.PublicListenAddr)
	if err != nil {
		return fmt.Errorf("listen public %s: %w", cfg.PublicListenAddr, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("tunnel listening", "public", cfg.PublicListenAddr, "backend", cfg.BackendListenAddr)

	errc := make(chan error, 2)
	go func() { errc <- hub.AcceptBackends(ctx, backendLn) }()
	go func() { errc <- hub.AcceptPublic(ctx, publicLn) }()

	// Both accept loops return nil on a clean ctx cancellation and a
	// non-nil error if their listener fails unexpectedly. Surface the first
	// real error, if any, while still waiting for both to finish; stop()
	// makes sure one loop failing brings the other down too.
	var firstErr error
	for i := 0; i < 2; i++ {
		if err := <-errc; err != nil && firstErr == nil {
			firstErr = err
			stop()
		}
	}
	logger.Info("tunnel stopped")
	return firstErr
}

func randomSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
