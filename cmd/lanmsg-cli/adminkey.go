package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"lanmessenger/internal/crypto"
)

// cmdAdminKeygen creates the household admin key in ~/.lanmsg-keys/admin.key
// (or -key / LANMSG_ADMIN_KEY), sealed with a passphrase, and prints the public
// half to install on the relay.
func cmdAdminKeygen(args []string) error {
	fs := flag.NewFlagSet("admin-keygen", flag.ExitOnError)
	keyPath := fs.String("key", "", "where to write the key (default ~/.lanmsg-keys/admin.key)")
	_ = fs.Parse(args)

	path := *keyPath
	if path == "" {
		p, err := crypto.DefaultAdminKeyPath()
		if err != nil {
			return err
		}
		path = p
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("a terminal is needed to type the passphrase")
	}
	read := func(prompt string) (string, error) {
		fmt.Fprint(os.Stderr, prompt)
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return string(b), err
	}
	p1, err := read("New admin key passphrase (8+ characters): ")
	if err != nil {
		return err
	}
	p2, err := read("Confirm passphrase:                       ")
	if err != nil {
		return err
	}
	if p1 != p2 {
		return errors.New("passphrases did not match")
	}
	pub, err := crypto.GenerateAdminKey(path, p1)
	if err != nil {
		return err
	}
	fmt.Printf(`
Admin key written: %s   (mode 0600)

Back this file up somewhere offline. Without it you cannot administer the
relay; without the passphrase it is useless to a thief. It must never be
copied to the relay or committed to a repo.

Install the PUBLIC half on the relay (then restart the relay):

  lanmsg-server admin-key set -config <relay config> %s
`, path, pub)
	return nil
}

// cmdAdminPub prints the public half of the local admin key (no passphrase).
func cmdAdminPub(args []string) error {
	fs := flag.NewFlagSet("admin-pub", flag.ExitOnError)
	keyPath := fs.String("key", "", "key file (default ~/.lanmsg-keys/admin.key)")
	_ = fs.Parse(args)
	path := *keyPath
	if path == "" {
		p, err := crypto.DefaultAdminKeyPath()
		if err != nil {
			return err
		}
		path = p
	}
	pub, err := crypto.ReadAdminKeyPub(path)
	if err != nil {
		return err
	}
	fmt.Println(strings.TrimSpace(pub))
	return nil
}
