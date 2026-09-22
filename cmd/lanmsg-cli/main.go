// Command lanmsg-cli is a terminal client for lanmessenger. It shares the same
// engine (internal/clientcore) as the desktop app, so it is also handy for
// scripting and for testing a relay.
//
// Usage:
//
//	lanmsg-cli [-config dir] enroll -server host:port -name "My box" [-fingerprint fp] [-passphrase p]
//	lanmsg-cli [-config dir] roster
//	lanmsg-cli [-config dir] send -to <name-or-id> -text "hello"
//	lanmsg-cli [-config dir] status <available|away|busy|dnd|invisible>
//	lanmsg-cli [-config dir] watch          # stay connected, print events
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"lanmessenger/internal/clientcore"
	"lanmessenger/internal/proto"
)

func main() {
	cfgDir := flag.String("config", "", "config directory (default: OS config dir + /lanmessenger)")
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	dir := *cfgDir
	if dir == "" {
		d, err := clientcore.DefaultDir()
		must(err)
		dir = d
	}

	var err error
	switch args[0] {
	case "enroll":
		err = cmdEnroll(dir, args[1:])
	case "roster":
		err = cmdRoster(dir)
	case "send":
		err = cmdSend(dir, args[1:])
	case "status":
		err = cmdStatus(dir, args[1:])
	case "watch":
		err = cmdWatch(dir)
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
	fmt.Fprint(os.Stderr, `lanmsg-cli — terminal client

  lanmsg-cli [-config dir] enroll -server host:port -name NAME [-fingerprint FP] [-passphrase P]
  lanmsg-cli [-config dir] roster
  lanmsg-cli [-config dir] send -to NAME_OR_ID -text "message"
  lanmsg-cli [-config dir] status available|away|busy|dnd|invisible
  lanmsg-cli [-config dir] watch
`)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// openClient loads config + client for an already-enrolled install.
func openClient(dir string) (*clientcore.Client, *clientcore.Config, error) {
	cfg, err := clientcore.LoadConfig(dir)
	if err != nil {
		return nil, nil, err
	}
	if !cfg.Configured() || !cfg.Enrolled() {
		return nil, nil, errors.New("this config is not enrolled yet; run `enroll` first")
	}
	cl, err := clientcore.New(cfg, nil)
	if err != nil {
		return nil, nil, err
	}
	if !cl.HasPassphrase() {
		cl.Close()
		return nil, nil, errors.New("no stored passphrase; re-run `enroll` with -passphrase")
	}
	return cl, cfg, nil
}

func cmdEnroll(dir string, args []string) error {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	server := fs.String("server", "", "relay address host:port")
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

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	fingerprint := *fp
	if fingerprint == "" {
		got, err := clientcore.FingerprintOfPresentedCert(ctx, *server)
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
		checkForUpdateOneShot(cl, dir)
	}
	return nil
}

func cmdRoster(dir string) error {
	cl, _, err := openClient(dir)
	if err != nil {
		return err
	}
	defer cl.Close()

	ctx, cancel := signalContext()
	defer cancel()
	if err := cl.Start(ctx); err != nil {
		return err
	}
	waitReady(ctx, cl, 10*time.Second)

	entries, err := cl.Roster()
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Println("(no other devices)")
		return nil
	}
	for _, e := range entries {
		mark := " "
		if e.Online {
			mark = "*"
		}
		verified := ""
		if !e.Verified {
			verified = "  [unverified]"
		}
		fmt.Printf("%s %-20s %-10s %s%s\n", mark, e.DisplayName, e.Status, e.DeviceID[:12], verified)
	}
	checkForUpdateOneShot(cl, dir)
	return nil
}

func cmdSend(dir string, args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	to := fs.String("to", "", "recipient display name or device id")
	text := fs.String("text", "", "message text")
	_ = fs.Parse(args)
	if *to == "" || *text == "" {
		return errors.New("send needs -to and -text")
	}

	cl, _, err := openClient(dir)
	if err != nil {
		return err
	}
	defer cl.Close()

	ctx, cancel := signalContext()
	defer cancel()
	if err := cl.Start(ctx); err != nil {
		return err
	}
	waitReady(ctx, cl, 10*time.Second)

	peerID, err := waitForPeer(ctx, cl, *to, 5*time.Second)
	if err != nil {
		return err
	}
	if _, err := cl.SendText(ctx, peerID, *text); err != nil {
		return err
	}

	// Give the relay a moment to accept it (and to deliver if the peer is online).
	select {
	case <-ctx.Done():
	case <-time.After(1500 * time.Millisecond):
	}
	fmt.Println("sent")
	checkForUpdateOneShot(cl, dir)
	return nil
}

func cmdStatus(dir string, args []string) error {
	if len(args) != 1 {
		return errors.New("status needs one of: available away busy dnd invisible")
	}
	s := proto.Status(strings.ToLower(args[0]))
	if !s.Valid() {
		return fmt.Errorf("unknown status %q", args[0])
	}
	cl, _, err := openClient(dir)
	if err != nil {
		return err
	}
	defer cl.Close()

	ctx, cancel := signalContext()
	defer cancel()
	if err := cl.Start(ctx); err != nil {
		return err
	}
	waitReady(ctx, cl, 10*time.Second)
	if err := cl.SetStatus(ctx, s, ""); err != nil {
		return err
	}
	time.Sleep(500 * time.Millisecond)
	fmt.Printf("status set to %s\n", s)
	checkForUpdateOneShot(cl, dir)
	return nil
}

func cmdWatch(dir string) error {
	cl, _, err := openClient(dir)
	if err != nil {
		return err
	}
	defer cl.Close()

	ctx, cancel := signalContext()
	defer cancel()
	if err := cl.Start(ctx); err != nil {
		return err
	}
	fmt.Println("watching (Ctrl-C to stop)…")

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-cl.Events():
			switch ev.Kind {
			case clientcore.EventConnState:
				fmt.Printf("[conn] %s\n", ev.State)
				if ev.State == clientcore.StateReady {
					go checkForUpdateReexec(cl, dir)
				}
			case clientcore.EventMessage:
				if ev.Message != nil && ev.Message.Direction == clientcore.DirIn {
					fmt.Printf("[msg] %s: %s\n", nameFor(cl, ev.PeerID), ev.Message.Body)
				}
			case clientcore.EventPresence:
				if ev.Presence != nil {
					fmt.Printf("[presence] %s -> %s (online=%v)\n",
						nameFor(cl, ev.PeerID), ev.Presence.Status, ev.Presence.Online)
				}
			case clientcore.EventFileProgress:
				if ev.Progress != nil && ev.Progress.Complete {
					fmt.Printf("[file] %s (%s)\n", ev.Progress.Name, ev.Progress.Path)
				}
			case clientcore.EventError:
				fmt.Printf("[error] %v\n", ev.Err)
			case clientcore.EventUpdateAvailable:
				fmt.Fprintf(os.Stderr, "[update] relay is on v%s; this build is older\n", ev.ServerVersion)
			case clientcore.EventUpdateRequired:
				fmt.Fprintln(os.Stderr, "[update] this build is too old for the relay; update and re-run")
				os.Exit(1)
			}
		}
	}
}

// --- helpers -------------------------------------------------------

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}

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

// waitForPeer retries resolvePeer for up to timeout. The relay sends the
// directory snapshot (the roster's actual contents) as a separate frame
// right after `ready`; a fresh connection can briefly report StateReady
// before that snapshot has been processed, so a single immediate
// resolvePeer call can spuriously fail against a still-empty roster.
func waitForPeer(ctx context.Context, cl *clientcore.Client, needle string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		id, err := resolvePeer(cl, needle)
		if err == nil {
			return id, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return "", lastErr
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(100 * time.Millisecond):
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

func nameFor(cl *clientcore.Client, id string) string {
	entries, _ := cl.Roster()
	for _, e := range entries {
		if e.DeviceID == id {
			return e.DisplayName
		}
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
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
