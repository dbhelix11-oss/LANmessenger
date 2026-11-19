package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"lanmessenger/internal/clientcore"
	"lanmessenger/internal/crypto"
	"lanmessenger/internal/proto"
)

// cmdAdmin is the terminal admin console:
//
//	admin devices            one line per device
//	admin stats              household summary
//	admin show NAME|ID       full Properties report
//	admin remove NAME|ID     delete the device (it must enroll again)
//	admin reauth NAME|ID     send it back to pending (must be approved again)
//	admin revoke NAME|ID     withdraw the registration but keep the record
//	admin rename NAME|ID -as "New Name"   change the name everyone sees
//	admin approval on|off|show   whether new devices must be approved
//	admin pending            devices waiting for approval
//	admin approve NAME|ID    approve a pending device
//	admin deny NAME|ID       deny a pending device
//
// Every action is signed by the admin key (asked for once, by passphrase, on
// the terminal) and the relay refuses it unless this machine is on the home
// network.
func cmdAdmin(dir string, args []string) error {
	fs := flag.NewFlagSet("admin", flag.ExitOnError)
	keyPath := fs.String("key", "", "admin key file (default ~/.lanmsg-keys/admin.key)")
	yes := fs.Bool("yes", false, "do not ask for confirmation before remove / reauth / deny")
	newName := fs.String("as", "", "the new display name, for: admin rename NAME -as \"New Name\"")
	// Allow flags before or after the subcommand words.
	var words []string
	rest := args
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		words = append(words, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if len(words) == 0 {
		return errors.New("admin needs one of: devices stats approval show rename revoke remove reauth pending approve deny")
	}
	sub, target := words[0], strings.Join(words[1:], " ")
	switch sub {
	case "devices", "stats", "pending", "approval":
	case "show", "remove", "reauth", "revoke", "rename", "approve", "deny":
		if target == "" {
			return fmt.Errorf("admin %s needs a device name or id", sub)
		}
	default:
		return fmt.Errorf("unknown admin command %q", sub)
	}

	path := *keyPath
	if path == "" {
		p, err := crypto.DefaultAdminKeyPath()
		if err != nil {
			return err
		}
		path = p
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("a terminal is needed to type the admin key passphrase")
	}
	fmt.Fprint(os.Stderr, "Admin key passphrase: ")
	pb, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return err
	}
	key, err := crypto.LoadAdminKey(path, string(pb))
	if err != nil {
		return err
	}

	cl, _, err := openClient(dir)
	if err != nil {
		return err
	}
	defer cl.Close()
	cl.SetAdminKey(key)

	ctx, cancel := signalContext()
	defer cancel()
	if err := cl.Start(ctx); err != nil {
		return err
	}
	waitReady(ctx, cl, 10*time.Second)

	call, cancelCall := context.WithTimeout(ctx, 15*time.Second)
	defer cancelCall()

	if sub == "pending" {
		list, err := cl.ListPending(call)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Println("(nobody is waiting)")
		}
		for _, e := range list {
			fmt.Printf("%-20s %s\n", e.DisplayName, e.DeviceID)
		}
		return nil
	}

	if sub == "approval" {
		switch strings.ToLower(target) {
		case "on", "off":
			if err := cl.SetRequireApproval(call, strings.ToLower(target) == "on"); err != nil {
				return err
			}
			fmt.Println("require_admin_approval is now", strings.ToLower(target))
		case "", "show":
			ov, err := cl.AdminOverview(call)
			if err != nil {
				return err
			}
			fmt.Println("require_admin_approval:", map[bool]string{true: "on", false: "OFF"}[ov.RequireApproval])
		default:
			return errors.New("admin approval takes on, off or show")
		}
		return nil
	}

	reps, err := cl.AdminDevices(call)
	if err != nil {
		return err
	}
	switch sub {
	case "devices":
		fmt.Print(clientcore.DeviceTable(reps))
		return nil
	case "stats":
		fmt.Print(clientcore.DeviceStats(reps))
		return nil
	}

	hit, err := oneDevice(reps, target)
	if err != nil {
		return err
	}
	switch sub {
	case "show":
		fmt.Print(clientcore.FormatDeviceReport(hit))
		return nil
	case "rename":
		if strings.TrimSpace(*newName) == "" {
			return errors.New(`rename needs -as "New Name"`)
		}
		if err := cl.RenameDevice(call, hit.Entry.DeviceID, *newName); err != nil {
			return err
		}
		fmt.Printf("%s is now %s\n", hit.Entry.DisplayName, strings.TrimSpace(*newName))
		return nil
	case "approve":
		if err := cl.Approve(call, hit.Entry.DeviceID); err != nil {
			return err
		}
		fmt.Printf("approved %s\n", hit.Entry.DisplayName)
		return nil
	}

	// remove / reauth / deny change who can use the household: confirm with the
	// device's fingerprint on screen, so the right person is being acted on.
	if !*yes {
		fmt.Printf("%s %s\n  device id   %s\n  fingerprint %s\n  state       %s\n",
			strings.ToUpper(sub), hit.Entry.DisplayName, hit.Entry.DeviceID, hit.Fingerprint, hit.Entry.State)
		fmt.Printf("Type the device name (%s) to confirm: ", hit.Entry.DisplayName)
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.TrimSpace(line) != hit.Entry.DisplayName {
			return errors.New("not confirmed; nothing changed")
		}
	}
	switch sub {
	case "remove":
		err = cl.RemoveDevice(call, hit.Entry.DeviceID)
	case "reauth":
		err = cl.ReauthDevice(call, hit.Entry.DeviceID)
	case "revoke":
		err = cl.RevokeDevice(call, hit.Entry.DeviceID)
	case "deny":
		err = cl.Deny(call, hit.Entry.DeviceID)
	}
	if err != nil {
		return err
	}
	fmt.Printf("%s: done (%s)\n", sub, hit.Entry.DisplayName)
	return nil
}

// oneDevice resolves a name or device-id prefix to exactly one device.
func oneDevice(reps []proto.DeviceReport, q string) (proto.DeviceReport, error) {
	m := clientcore.DeviceMatches(reps, q)
	switch len(m) {
	case 0:
		return proto.DeviceReport{}, fmt.Errorf("no device named or starting with %q (try: admin devices)", q)
	case 1:
		return m[0], nil
	}
	var names []string
	for _, r := range m {
		names = append(names, r.Entry.DisplayName+" ("+r.Entry.DeviceID[:12]+"…)")
	}
	return proto.DeviceReport{}, fmt.Errorf("%q matches %d devices: %s; use a longer device id", q, len(m), strings.Join(names, ", "))
}
