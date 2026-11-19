package clientcore_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"lanmessenger/internal/clientcore"
	"lanmessenger/internal/crypto"
)

func startReady(t *testing.T, addr, fp, name string) *clientcore.Client {
	t.Helper()
	c := newClient(t, addr, fp)
	if _, err := c.Enroll(context.Background(), name, relayPassphrase); err != nil {
		t.Fatalf("%s enroll: %v", name, err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("%s start: %v", name, err)
	}
	return c
}

func TestAdminKeyConsole(t *testing.T) {
	addr, fp, cfg := startRelayCfg(t, true)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	cfg.AdminKeyPub = crypto.EncodeSignPub(pub)

	dad := startReady(t, addr, fp, "Dad")
	waitState(t, dad, clientcore.StateReady, 5*time.Second)
	kid := startReady(t, addr, fp, "Kid")
	waitState(t, kid, clientcore.StatePendingApproval, 5*time.Second)
	ctx := context.Background()

	// Locked: no admin power, not even for the first (flagged) device.
	if _, err := dad.AdminDevices(ctx); !errors.Is(err, clientcore.ErrNoAdminKey) {
		t.Fatalf("locked AdminDevices err = %v, want ErrNoAdminKey", err)
	}
	if _, err := dad.ListPending(ctx); err == nil {
		t.Fatal("ListPending should fail without the admin key once the relay has one")
	}

	// A wrong key is refused by the relay.
	_, wrong, _ := ed25519.GenerateKey(rand.Reader)
	dad.SetAdminKey(wrong)
	if _, err := dad.AdminDevices(ctx); err == nil {
		t.Fatal("wrong admin key was accepted")
	}

	// The right key works, even carried by a device that is not flagged admin.
	dad.SetAdminKey(priv)
	if p, err := dad.ListPending(ctx); err != nil || len(p) != 1 {
		t.Fatalf("ListPending with key: %v %v", p, err)
	}
	if err := dad.Approve(ctx, kid.DeviceID()); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	waitState(t, kid, clientcore.StateReady, 5*time.Second)

	var kidRep *clientcore.AdminDeviceView
	waitFor(t, "facts and connection record", 5*time.Second, func() bool {
		devs, err := dad.AdminDevices(ctx)
		if err != nil || len(devs) != 2 {
			return false
		}
		for i := range devs {
			if devs[i].Entry.DeviceID == kid.DeviceID() {
				kidRep = &devs[i]
			}
		}
		return kidRep != nil && kidRep.Facts != nil && kidRep.Online
	})
	if kidRep.Fingerprint == "" || !kidRep.FactsVerified {
		t.Fatalf("fingerprint %q verified=%v", kidRep.Fingerprint, kidRep.FactsVerified)
	}
	if len(kidRep.Conns) == 0 || kidRep.Conns[0].Path != "lan" || kidRep.Conns[0].TLSHash == "" || kidRep.Conns[0].ClientOS == "" || kidRep.Conns[0].ClientKind == "" {
		t.Fatalf("connection record not populated: %+v", kidRep.Conns)
	}

	// Force re-auth: kid drops back to pending, then is approved again.
	if err := dad.ReauthDevice(ctx, kid.DeviceID()); err != nil {
		t.Fatalf("ReauthDevice: %v", err)
	}
	waitState(t, kid, clientcore.StatePendingApproval, 8*time.Second)
	if err := dad.Approve(ctx, kid.DeviceID()); err != nil {
		t.Fatalf("re-Approve: %v", err)
	}
	waitState(t, kid, clientcore.StateReady, 8*time.Second)

	// Remove: the device disappears from the relay's list.
	if err := dad.RemoveDevice(ctx, kid.DeviceID()); err != nil {
		t.Fatalf("RemoveDevice: %v", err)
	}
	waitFor(t, "kid removed", 5*time.Second, func() bool {
		devs, err := dad.AdminDevices(ctx)
		return err == nil && len(devs) == 1
	})
}

func TestRevokeKeepsRecordAndReportsReturn(t *testing.T) {
	addr, fp, cfg := startRelayCfg(t, true)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	cfg.AdminKeyPub = crypto.EncodeSignPub(pub)

	dad := startReady(t, addr, fp, "Dad")
	waitState(t, dad, clientcore.StateReady, 5*time.Second)
	kid := startReady(t, addr, fp, "Kid")
	waitState(t, kid, clientcore.StatePendingApproval, 5*time.Second)
	ctx := context.Background()
	dad.SetAdminKey(priv)
	if err := dad.Approve(ctx, kid.DeviceID()); err != nil {
		t.Fatal(err)
	}
	waitState(t, kid, clientcore.StateReady, 5*time.Second)

	if err := dad.RevokeDevice(ctx, kid.DeviceID()); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	// The record stays, marked revoked, and the kid is dropped.
	var rep clientcore.AdminDeviceView
	waitFor(t, "revoked record with a return attempt", 25*time.Second, func() bool {
		time.Sleep(400 * time.Millisecond) // polling faster would use up the device's frame budget
		devs, err := dad.AdminDevices(ctx)
		if err != nil {
			return false
		}
		for _, d := range devs {
			if d.Entry.DeviceID == kid.DeviceID() {
				rep = d
			}
		}
		return rep.Entry.State == "revoked" && rep.RevokedAt > 0 && rep.RevokeAttempts >= 1
	})
	if rep.LastRevokeMatch != "id" || rep.Online {
		t.Fatalf("revoked report wrong: match=%q online=%v", rep.LastRevokeMatch, rep.Online)
	}

	// The admin device was told about the attempt as it happened.
	var alert *clientcore.Event
	deadline := time.After(5 * time.Second)
drain:
	for {
		select {
		case ev := <-dad.Events():
			if ev.Kind == clientcore.EventAdminAlert {
				e := ev
				alert = &e
				break drain
			}
		case <-deadline:
			break drain
		}
	}
	if alert == nil || alert.Alert.DeviceID != kid.DeviceID() || alert.Alert.Kind != "revoked_attempt" {
		t.Fatalf("no admin alert for the revoked device's attempt: %+v", alert)
	}

	// A NEW key from the same machine (same TLS shape, client, LAN address) is not
	// refused, but the admin is told it resembles the revoked device.
	imposter := startReady(t, addr, fp, "Kid again")
	waitState(t, imposter, clientcore.StatePendingApproval, 5*time.Second)
	var fpAlert *clientcore.Event
	deadline = time.After(5 * time.Second)
drain2:
	for {
		select {
		case ev := <-dad.Events():
			if ev.Kind == clientcore.EventAdminAlert && ev.Alert.Match == "fingerprint" {
				e := ev
				fpAlert = &e
				break drain2
			}
		case <-deadline:
			break drain2
		}
	}
	if fpAlert == nil || fpAlert.Alert.NewName != "Kid again" || fpAlert.Alert.DeviceID != kid.DeviceID() ||
		!strings.Contains(fpAlert.Alert.Signals, "tls") || !strings.Contains(fpAlert.Alert.Signals, "client") {
		t.Fatalf("no fingerprint alert for a lookalike registration: %+v", fpAlert)
	}

	// Restoring goes back through approval; deleting removes the record.
	if err := dad.ReauthDevice(ctx, kid.DeviceID()); err != nil {
		t.Fatalf("ReauthDevice on revoked: %v", err)
	}
	if err := dad.Approve(ctx, kid.DeviceID()); err != nil {
		t.Fatalf("Approve after restore: %v", err)
	}
	// A client that was refused as "forbidden" stops retrying (so it does not hammer
	// the relay); once restored it is started again, as a restarted app would be.
	if err := kid.Start(ctx); err != nil {
		t.Fatalf("restart kid: %v", err)
	}
	waitState(t, kid, clientcore.StateReady, 20*time.Second)
	if err := dad.RevokeDevice(ctx, dad.DeviceID()); err == nil {
		t.Fatal("revoking the device in use should be refused")
	}
}

func TestAdminTogglesRequireApproval(t *testing.T) {
	addr, fp, cfg := startRelayCfg(t, false)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	cfg.AdminKeyPub = crypto.EncodeSignPub(pub)
	ctx := context.Background()

	dad := startReady(t, addr, fp, "Dad")
	waitState(t, dad, clientcore.StateReady, 5*time.Second)
	if err := dad.SetRequireApproval(ctx, true); !errors.Is(err, clientcore.ErrNoAdminKey) {
		t.Fatalf("locked SetRequireApproval err = %v, want ErrNoAdminKey", err)
	}
	dad.SetAdminKey(priv)
	ov, err := dad.AdminOverview(ctx)
	if err != nil || ov.RequireApproval {
		t.Fatalf("overview: approval=%v err=%v", ov.RequireApproval, err)
	}

	// Off: a newcomer is active at once.
	open1 := startReady(t, addr, fp, "Open")
	waitState(t, open1, clientcore.StateReady, 5*time.Second)

	// On: the next newcomer is held for approval, and the setting survives in the config.
	if err := dad.SetRequireApproval(ctx, true); err != nil {
		t.Fatalf("SetRequireApproval(true): %v", err)
	}
	ov, _ = dad.AdminOverview(ctx)
	if !ov.RequireApproval {
		t.Fatal("overview does not show approval required")
	}
	held := startReady(t, addr, fp, "Held")
	waitState(t, held, clientcore.StatePendingApproval, 5*time.Second)
	data, err := os.ReadFile(cfg.DataDir + "/server.toml")
	if err != nil || !strings.Contains(string(data), "require_admin_approval = true") {
		t.Fatalf("config on disk not updated: %v\n%s", err, data)
	}

	if err := dad.SetRequireApproval(ctx, false); err != nil {
		t.Fatalf("SetRequireApproval(false): %v", err)
	}
	free := startReady(t, addr, fp, "Free")
	waitState(t, free, clientcore.StateReady, 5*time.Second)
}

func TestAdminRenameReachesEveryone(t *testing.T) {
	addr, fp, cfg := startRelayCfg(t, false)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	cfg.AdminKeyPub = crypto.EncodeSignPub(pub)
	ctx := context.Background()

	dad := startReady(t, addr, fp, "Dad")
	waitState(t, dad, clientcore.StateReady, 5*time.Second)
	kid := startReady(t, addr, fp, "Kid")
	waitState(t, kid, clientcore.StateReady, 5*time.Second)
	sis := startReady(t, addr, fp, "Sis")
	waitState(t, sis, clientcore.StateReady, 5*time.Second)
	dad.SetAdminKey(priv)

	nameOf := func(c *clientcore.Client, id string) string {
		r, _ := c.Roster()
		for _, e := range r {
			if e.DeviceID == id {
				return e.DisplayName
			}
		}
		return ""
	}
	waitFor(t, "sis sees kid", 5*time.Second, func() bool { return nameOf(sis, kid.DeviceID()) == "Kid" })

	for _, bad := range []string{"", "   ", "x\ny", strings.Repeat("a", 65)} {
		if err := dad.RenameDevice(ctx, kid.DeviceID(), bad); err == nil {
			t.Fatalf("name %q was accepted", bad)
		}
	}
	if err := dad.RenameDevice(ctx, kid.DeviceID(), "  Junior  "); err != nil {
		t.Fatalf("RenameDevice: %v", err)
	}
	// Everyone else's roster shows it, the old name is gone, and the renamed device adopts it.
	waitFor(t, "sis sees the new name", 5*time.Second, func() bool { return nameOf(sis, kid.DeviceID()) == "Junior" })
	waitFor(t, "kid adopts its new name", 5*time.Second, func() bool { return kid.DisplayName() == "Junior" })
	devs, _ := dad.AdminDevices(ctx)
	for _, d := range devs {
		if d.Entry.DeviceID == kid.DeviceID() && d.Entry.DisplayName != "Junior" {
			t.Fatalf("relay still holds %q", d.Entry.DisplayName)
		}
	}

	// The admin can rename their own device too; others and the device itself follow.
	if err := dad.RenameDevice(ctx, dad.DeviceID(), "Papa"); err != nil {
		t.Fatalf("self rename: %v", err)
	}
	waitFor(t, "sis sees Papa", 5*time.Second, func() bool { return nameOf(sis, dad.DeviceID()) == "Papa" })
	waitFor(t, "dad adopts Papa", 5*time.Second, func() bool { return dad.DisplayName() == "Papa" })
}
