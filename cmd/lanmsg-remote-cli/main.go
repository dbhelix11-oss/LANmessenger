// Command lanmsg-remote-cli is a minimal, one-shot terminal client for
// reaching the LANmessenger relay from outside the home LAN, through the
// cloud tunnel over Tor. It reuses the exact same engine
// (internal/clientcore) as the desktop app and lanmsg-cli — enrollment,
// crypto, SendText — with one difference: it always routes its connection
// through a local Tor SOCKS5 proxy, since ServerAddr is the cloud tunnel's
// .onion address rather than a LAN address.
//
// There is no roster browsing, no presence, no daemon: point it at a
// config directory and a message, it sends and exits. Enrollment is a
// one-time step; every later invocation just sends.
//
// It cross-compiles cleanly for Android/Termux
// (CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build), since nothing in its
// dependency chain (this package, internal/clientcore, internal/store's
// pure-Go modernc.org/sqlite driver) requires cgo.
//
// Usage:
//
//	lanmsg-remote-cli [-config dir] [-socks host:port] enroll -server onion:port -name "My phone" [-fingerprint fp] [-passphrase p]
//	lanmsg-remote-cli [-config dir] [-socks host:port] send -to <name-or-id> -text "message"
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"lanmessenger/internal/clientcore"
)

const defaultSOCKSProxy = "127.0.0.1:9050"

func main() {
	cfgDir := flag.String("config", "", "config directory (default: OS config dir + /lanmessenger-remote)")
	socks := flag.String("socks", defaultSOCKSProxy, "local Tor SOCKS5 proxy address")
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	dir := *cfgDir
	if dir == "" {
		d, err := defaultRemoteDir()
		must(err)
		dir = d
	}

	var err error
	switch args[0] {
	case "enroll":
		err = cmdEnroll(dir, *socks, args[1:])
	case "send":
		err = cmdSend(dir, *socks, args[1:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// defaultRemoteDir is deliberately NOT clientcore.DefaultDir() — that's the
// same directory the GUI (cmd/lanmsg) and cmd/lanmsg-cli default to. Sharing
// it caused a real bug: running `enroll` here with no -config silently
// overwrote a LAN client's config.json with SOCKSProxy set, breaking its
// LAN connection (Tor refuses to proxy to private-use addresses). This
// tool gets its own sibling directory so it can never collide.
func defaultRemoteDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate config dir: %w", err)
	}
	return filepath.Join(base, "lanmessenger-remote"), nil
}

func usage() {
	fmt.Fprint(os.Stderr, `lanmsg-remote-cli — one-shot remote client (via the cloud tunnel over Tor)

  lanmsg-remote-cli [-config dir] [-socks host:port] enroll -server onion:port -name NAME [-fingerprint FP] [-passphrase P]
  lanmsg-remote-cli [-config dir] [-socks host:port] send -to NAME_OR_ID -text "message"

-socks defaults to 127.0.0.1:9050 (a local Tor daemon's default SOCKS5 port).
`)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func cmdEnroll(dir, socks string, args []string) error {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	server := fs.String("server", "", "cloud tunnel's public .onion address, host:port")
	name := fs.String("name", "", "display name for this device")
	fp := fs.String("fingerprint", "", "relay TLS fingerprint (fetched and shown if omitted)")
	pass := fs.String("passphrase", "", "household passphrase (prompted if omitted)")
	_ = fs.Parse(args)

	if *server == "" || *name == "" {
		return errors.New("enroll needs -server and -name")
	}

	cfg, err := clientcore.LoadConfig(dir)
	if err != nil {
		return err
	}
	cfg.ServerAddr = *server
	cfg.SOCKSProxy = socks

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	fingerprint := *fp
	if fingerprint == "" {
		got, err := clientcore.FingerprintOfPresentedCertVia(ctx, *server, socks)
		if err != nil {
			return err
		}
		fmt.Printf("Relay certificate fingerprint:\n\n  %s\n\nConfirm this matches what `lanmsg-server setup` printed.\n", got)
		if !confirm("Continue?") {
			return errors.New("aborted")
		}
		fingerprint = got
	}
	cfg.CertFingerprint = fingerprint
	if err := cfg.Save(); err != nil {
		return err
	}

	passphrase := *pass
	if passphrase == "" {
		passphrase = prompt("Household passphrase: ")
	}

	cl, err := clientcore.New(cfg, nil)
	if err != nil {
		return err
	}
	defer cl.Close()

	state, err := cl.Enroll(ctx, *name, passphrase)
	if err != nil {
		return err
	}
	switch state {
	case clientcore.StatePendingApproval:
		fmt.Println("Enrolled. Waiting for an admin to approve this device.")
	default:
		fmt.Println("Enrolled and active.")
	}
	return nil
}

func cmdSend(dir, socks string, args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	to := fs.String("to", "", "recipient display name or device id")
	text := fs.String("text", "", "message text")
	_ = fs.Parse(args)
	if *to == "" || *text == "" {
		return errors.New("send needs -to and -text")
	}

	cfg, err := clientcore.LoadConfig(dir)
	if err != nil {
		return err
	}
	if !cfg.Configured() || !cfg.Enrolled() {
		return errors.New("this config is not enrolled yet; run `enroll` first")
	}
	cfg.SOCKSProxy = socks

	cl, err := clientcore.New(cfg, nil)
	if err != nil {
		return err
	}
	defer cl.Close()
	if !cl.HasPassphrase() {
		return errors.New("no stored passphrase; re-run `enroll` with -passphrase")
	}

	// A bounded context, not signal-driven: this is a one-shot tool, not a
	// daemon, so there is nothing to wait around for after the send.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := cl.Start(ctx); err != nil {
		return err
	}
	waitReady(ctx, cl, 15*time.Second)

	peerID, err := resolvePeer(cl, *to)
	if err != nil {
		return err
	}
	if _, err := cl.SendText(ctx, peerID, *text); err != nil {
		return err
	}

	// Give the relay a moment to accept it (and to deliver if the peer is
	// online) before this one-shot process exits.
	select {
	case <-ctx.Done():
	case <-time.After(1500 * time.Millisecond):
	}
	fmt.Println("sent")
	return nil
}

// --- helpers, shared in spirit with lanmsg-cli but kept local since this
// tool intentionally has a much smaller surface (no roster/watch/status). ---

func waitReady(ctx context.Context, cl *clientcore.Client, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cl.State() == clientcore.StateReady {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func resolvePeer(cl *clientcore.Client, needle string) (string, error) {
	entries, err := cl.Roster()
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.DeviceID == needle || strings.EqualFold(e.DisplayName, needle) {
			return e.DeviceID, nil
		}
	}
	for _, e := range entries {
		if strings.HasPrefix(e.DeviceID, needle) {
			return e.DeviceID, nil
		}
	}
	return "", fmt.Errorf("no peer matching %q", needle)
}

func prompt(label string) string {
	fmt.Fprint(os.Stderr, label)
	r := bufio.NewReader(os.Stdin)
	line, _ := r.ReadString('\n')
	return strings.TrimSpace(line)
}

func confirm(label string) bool {
	ans := strings.ToLower(prompt(label + " [y/N] "))
	return ans == "y" || ans == "yes"
}
