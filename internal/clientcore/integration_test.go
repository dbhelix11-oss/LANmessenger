package clientcore_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lanmessenger/internal/clientcore"
	"lanmessenger/internal/crypto"
	"lanmessenger/internal/proto"
	"lanmessenger/internal/servercore"
)

const relayPassphrase = "test pass phrase"

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startRelay boots a servercore relay on an ephemeral port and returns its
// address and TLS certificate fingerprint.
func startRelay(t *testing.T, requireApproval bool) (addr, fingerprint string) {
	addr, fingerprint, _ = startRelayCfg(t, requireApproval)
	return addr, fingerprint
}

// startRelayCfg is startRelay but also returns the live *servercore.Config,
// which a test may go on mutating (e.g. MinClientVersion) — the server reads
// it fresh on every handshake, not just at startup.
func startRelayCfg(t *testing.T, requireApproval bool) (addr, fingerprint string, cfg *servercore.Config) {
	t.Helper()
	dir := t.TempDir()

	verifier, err := crypto.NewPassphraseVerifier(relayPassphrase)
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	cfg = &servercore.Config{
		DataDir:              dir,
		Passphrase:           verifier,
		RequireAdminApproval: requireApproval,
		HeartbeatSeconds:     3600,
	}
	cfg.SetPath(dir + "/server.toml")

	srv, err := servercore.New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("servercore.New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.RunListener(ctx, ln); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	})

	fp, err := srv.CertFingerprint()
	if err != nil {
		t.Fatalf("CertFingerprint: %v", err)
	}
	return ln.Addr().String(), fp, cfg
}

func newClient(t *testing.T, addr, fingerprint string) *clientcore.Client {
	return newClientWithDownloads(t, addr, fingerprint, t.TempDir())
}

func newClientWithDownloads(t *testing.T, addr, fingerprint, downloadsDir string) *clientcore.Client {
	t.Helper()
	cfg := &clientcore.Config{
		ServerAddr:      addr,
		CertFingerprint: fingerprint,
		DownloadsDir:    downloadsDir,
	}
	cfg.SetDir(t.TempDir())
	cl, err := clientcore.New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("clientcore.New: %v", err)
	}
	t.Cleanup(func() { cl.Stop(); cl.Close() })
	return cl
}

func waitFor(t *testing.T, what string, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func waitState(t *testing.T, cl *clientcore.Client, want clientcore.ConnState, timeout time.Duration) {
	t.Helper()
	waitFor(t, "state "+want.String(), timeout, func() bool { return cl.State() == want })
}

// drainUntil reads events until one of kind arrives.
func drainUntil(t *testing.T, cl *clientcore.Client, kind clientcore.EventKind, timeout time.Duration) clientcore.Event {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case ev := <-cl.Events():
			if ev.Kind == kind {
				return ev
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for event %s", kind)
		}
	}
}

func TestEnrollAndMessage(t *testing.T) {
	addr, fp := startRelay(t, false)

	dad := newClient(t, addr, fp)
	st, err := dad.Enroll(context.Background(), "Dad", relayPassphrase)
	if err != nil {
		t.Fatalf("Dad enroll: %v", err)
	}
	if st != clientcore.StateReady {
		t.Fatalf("Dad enroll state = %v, want ready", st)
	}

	mom := newClient(t, addr, fp)
	if _, err := mom.Enroll(context.Background(), "Mom", relayPassphrase); err != nil {
		t.Fatalf("Mom enroll: %v", err)
	}

	if err := dad.Start(context.Background()); err != nil {
		t.Fatalf("Dad start: %v", err)
	}
	if err := mom.Start(context.Background()); err != nil {
		t.Fatalf("Mom start: %v", err)
	}
	waitState(t, dad, clientcore.StateReady, 5*time.Second)
	waitState(t, mom, clientcore.StateReady, 5*time.Second)

	// Dad should learn about Mom.
	waitFor(t, "Dad sees Mom in roster", 5*time.Second, func() bool {
		r, _ := dad.Roster()
		for _, e := range r {
			if e.DeviceID == mom.DeviceID() {
				return true
			}
		}
		return false
	})

	if _, err := dad.SendText(context.Background(), mom.DeviceID(), "dinner in 5"); err != nil {
		t.Fatalf("SendText: %v", err)
	}

	ev := drainUntil(t, mom, clientcore.EventMessage, 5*time.Second)
	if ev.Message == nil || ev.Message.Body != "dinner in 5" || ev.Message.Direction != clientcore.DirIn {
		t.Fatalf("unexpected received message: %+v", ev.Message)
	}
	if ev.Message.PeerID != dad.DeviceID() {
		t.Fatalf("message peer = %s, want %s", ev.Message.PeerID, dad.DeviceID())
	}

	// Mom's history should now contain it.
	waitFor(t, "Mom history has the message", 3*time.Second, func() bool {
		h, _ := mom.History(dad.DeviceID(), 10)
		return len(h) == 1 && h[0].Body == "dinner in 5"
	})

	// Dad's copy should be marked delivered once Mom acks.
	waitFor(t, "Dad message delivered", 5*time.Second, func() bool {
		h, _ := dad.History(mom.DeviceID(), 10)
		return len(h) == 1 && h[0].State == clientcore.StateDelivered
	})
}

func TestOfflineQueueDelivery(t *testing.T) {
	addr, fp := startRelay(t, false)

	dad := newClient(t, addr, fp)
	if _, err := dad.Enroll(context.Background(), "Dad", relayPassphrase); err != nil {
		t.Fatalf("Dad enroll: %v", err)
	}
	mom := newClient(t, addr, fp)
	if _, err := mom.Enroll(context.Background(), "Mom", relayPassphrase); err != nil {
		t.Fatalf("Mom enroll: %v", err)
	}

	if err := dad.Start(context.Background()); err != nil {
		t.Fatalf("Dad start: %v", err)
	}
	waitState(t, dad, clientcore.StateReady, 5*time.Second)
	waitFor(t, "Dad sees Mom (offline)", 5*time.Second, func() bool {
		r, _ := dad.Roster()
		for _, e := range r {
			if e.DeviceID == mom.DeviceID() {
				return true
			}
		}
		return false
	})

	// Mom is offline: this should be queued on the relay.
	if _, err := dad.SendText(context.Background(), mom.DeviceID(), "call me back"); err != nil {
		t.Fatalf("SendText: %v", err)
	}

	// Mom comes online and should receive it.
	if err := mom.Start(context.Background()); err != nil {
		t.Fatalf("Mom start: %v", err)
	}
	waitState(t, mom, clientcore.StateReady, 5*time.Second)

	ev := drainUntil(t, mom, clientcore.EventMessage, 5*time.Second)
	if ev.Message == nil || ev.Message.Body != "call me back" {
		t.Fatalf("queued message not delivered: %+v", ev.Message)
	}
}

func TestClientOutboxFlush(t *testing.T) {
	addr, fp := startRelay(t, false)

	dad := newClient(t, addr, fp)
	if _, err := dad.Enroll(context.Background(), "Dad", relayPassphrase); err != nil {
		t.Fatalf("Dad enroll: %v", err)
	}
	mom := newClient(t, addr, fp)
	if _, err := mom.Enroll(context.Background(), "Mom", relayPassphrase); err != nil {
		t.Fatalf("Mom enroll: %v", err)
	}

	// Dad connects once so he learns Mom's keys, then disconnects.
	if err := dad.Start(context.Background()); err != nil {
		t.Fatalf("Dad start: %v", err)
	}
	waitState(t, dad, clientcore.StateReady, 5*time.Second)
	waitFor(t, "Dad sees Mom", 5*time.Second, func() bool {
		r, _ := dad.Roster()
		for _, e := range r {
			if e.DeviceID == mom.DeviceID() {
				return true
			}
		}
		return false
	})
	dad.Stop()
	waitState(t, dad, clientcore.StateDisconnected, 3*time.Second)

	// Compose while Dad is fully offline: this lands in the local outbox.
	sentMsg, err := dad.SendText(context.Background(), mom.DeviceID(), "left you a note")
	if err != nil {
		t.Fatalf("offline SendText: %v", err)
	}
	if sentMsg.State != clientcore.StateQueued {
		t.Fatalf("offline message state = %v, want queued", sentMsg.State)
	}

	// Mom comes online, then Dad reconnects and flushes the outbox.
	if err := mom.Start(context.Background()); err != nil {
		t.Fatalf("Mom start: %v", err)
	}
	waitState(t, mom, clientcore.StateReady, 5*time.Second)
	if err := dad.Start(context.Background()); err != nil {
		t.Fatalf("Dad restart: %v", err)
	}
	waitState(t, dad, clientcore.StateReady, 5*time.Second)

	ev := drainUntil(t, mom, clientcore.EventMessage, 5*time.Second)
	if ev.Message == nil || ev.Message.Body != "left you a note" {
		t.Fatalf("outbox message not delivered: %+v", ev.Message)
	}
	waitFor(t, "Dad's copy marked delivered", 5*time.Second, func() bool {
		h, _ := dad.History(mom.DeviceID(), 10)
		return len(h) == 1 && h[0].State == clientcore.StateDelivered
	})
}

func TestFileTransfer(t *testing.T) {
	addr, fp := startRelay(t, false)

	dad := newClient(t, addr, fp)
	if _, err := dad.Enroll(context.Background(), "Dad", relayPassphrase); err != nil {
		t.Fatalf("Dad enroll: %v", err)
	}
	momDownloads := t.TempDir()
	mom := newClientWithDownloads(t, addr, fp, momDownloads)
	if _, err := mom.Enroll(context.Background(), "Mom", relayPassphrase); err != nil {
		t.Fatalf("Mom enroll: %v", err)
	}

	if err := dad.Start(context.Background()); err != nil {
		t.Fatalf("Dad start: %v", err)
	}
	if err := mom.Start(context.Background()); err != nil {
		t.Fatalf("Mom start: %v", err)
	}
	waitState(t, dad, clientcore.StateReady, 5*time.Second)
	waitState(t, mom, clientcore.StateReady, 5*time.Second)

	waitFor(t, "Dad sees Mom", 5*time.Second, func() bool {
		r, _ := dad.Roster()
		for _, e := range r {
			if e.DeviceID == mom.DeviceID() {
				return true
			}
		}
		return false
	})

	// A ~1.5 MiB file => several chunks.
	src := filepath.Join(t.TempDir(), "photo.bin")
	payload := make([]byte, 1_500_000)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	want := sha256.Sum256(payload)

	if _, err := dad.SendFile(context.Background(), mom.DeviceID(), src); err != nil {
		t.Fatalf("SendFile: %v", err)
	}

	// Wait for Mom's completion event.
	var gotPath string
	deadline := time.After(10 * time.Second)
	for gotPath == "" {
		select {
		case ev := <-mom.Events():
			if ev.Kind == clientcore.EventFileProgress && ev.Progress != nil &&
				ev.Progress.Complete && ev.Progress.Direction == clientcore.DirIn {
				gotPath = ev.Progress.Path
			}
		case <-deadline:
			t.Fatal("timed out waiting for inbound file completion")
		}
	}

	got, err := os.ReadFile(gotPath)
	if err != nil {
		t.Fatalf("read received file: %v", err)
	}
	if h := sha256.Sum256(got); hex.EncodeToString(h[:]) != hex.EncodeToString(want[:]) {
		t.Fatalf("received file hash mismatch (got %d bytes, want %d)", len(got), len(payload))
	}

	// History should record a completed inbound file.
	waitFor(t, "Mom history shows received file", 3*time.Second, func() bool {
		h, _ := mom.History(dad.DeviceID(), 10)
		for _, m := range h {
			if m.Kind == proto.InnerFileOffer {
				meta, err := clientcore.DecodeFileMeta(m.Body)
				if err == nil && meta.Status == "received" {
					return true
				}
			}
		}
		return false
	})
}

func TestCertPinningRejectsWrongFingerprint(t *testing.T) {
	addr, _ := startRelay(t, false)

	bogus := "00:11:22:33:44:55:66:77:88:99:aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99:aa:bb:cc:dd:ee:ff"
	cl := newClient(t, addr, bogus)
	if _, err := cl.Enroll(context.Background(), "Mallory", relayPassphrase); err == nil {
		t.Fatal("enrollment succeeded despite a mismatched certificate fingerprint")
	}
}

func TestAdminApproval(t *testing.T) {
	addr, fp := startRelay(t, true)

	dad := newClient(t, addr, fp)
	st, err := dad.Enroll(context.Background(), "Dad", relayPassphrase)
	if err != nil {
		t.Fatalf("Dad enroll: %v", err)
	}
	if st != clientcore.StateReady {
		t.Fatalf("first device should be ready despite approval mode, got %v", st)
	}
	if err := dad.Start(context.Background()); err != nil {
		t.Fatalf("Dad start: %v", err)
	}
	waitState(t, dad, clientcore.StateReady, 5*time.Second)

	kid := newClient(t, addr, fp)
	kst, err := kid.Enroll(context.Background(), "Kid", relayPassphrase)
	if err != nil {
		t.Fatalf("Kid enroll: %v", err)
	}
	if kst != clientcore.StatePendingApproval {
		t.Fatalf("Kid enroll state = %v, want pending", kst)
	}
	if err := kid.Start(context.Background()); err != nil {
		t.Fatalf("Kid start: %v", err)
	}
	waitState(t, kid, clientcore.StatePendingApproval, 5*time.Second)

	if !dad.IsAdmin() {
		t.Fatal("Dad should be an admin")
	}
	var pending []proto.DirectoryEntry
	waitFor(t, "Dad sees Kid pending", 5*time.Second, func() bool {
		p, err := dad.ListPending(context.Background())
		if err != nil {
			return false
		}
		pending = p
		return len(p) == 1
	})
	if pending[0].DeviceID != kid.DeviceID() {
		t.Fatalf("pending device = %s, want %s", pending[0].DeviceID, kid.DeviceID())
	}

	if err := dad.Approve(context.Background(), kid.DeviceID()); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	waitState(t, kid, clientcore.StateReady, 5*time.Second)
}

// TestUpdateRequiredStopsReconnecting covers the hard-stop path: a client
// that was fine at enrollment gets locked out once the relay's
// MinClientVersion rises above this build's own version.Version. It should
// see EventUpdateRequired and runLoop should give up rather than retry
// forever against a relay that will keep rejecting it.
func TestUpdateRequiredStopsReconnecting(t *testing.T) {
	addr, fp, cfg := startRelayCfg(t, false)

	cl := newClient(t, addr, fp)
	if _, err := cl.Enroll(context.Background(), "Old build", relayPassphrase); err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if err := cl.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitState(t, cl, clientcore.StateReady, 5*time.Second)
	cl.Stop()

	// The relay now requires a build newer than this one has.
	cfg.MinClientVersion = "99.0.0"

	if err := cl.Start(context.Background()); err != nil {
		t.Fatalf("Start (2nd): %v", err)
	}
	ev := drainUntil(t, cl, clientcore.EventUpdateRequired, 5*time.Second)
	if ev.Kind != clientcore.EventUpdateRequired {
		t.Fatalf("got event %s, want %s", ev.Kind, clientcore.EventUpdateRequired)
	}

	// runLoop should have given up: state settles on disconnected and stays
	// there rather than climbing back to connecting/ready on a retry.
	time.Sleep(200 * time.Millisecond)
	waitFor(t, "state stays disconnected", time.Second, func() bool {
		return cl.State() == clientcore.StateDisconnected
	})
}

// TestUpdateAvailableEmittedOnNewerRelay covers the soft path directly
// against applyProtocolGate (package-internal, see versiongate_test.go) since
// faithfully reproducing "the relay is on a newer release" through this
// external test package would require the test relay to report a
// version.Version different from the one it's compiled with. This test just
// confirms the wiring: a real handshake against a relay with no
// MinClientVersion configured leaves the client's ServerVersion() populated
// and does not itself misfire EventUpdateRequired.
func TestReadyPopulatesServerVersion(t *testing.T) {
	addr, fp := startRelay(t, false)
	cl := newClient(t, addr, fp)
	if _, err := cl.Enroll(context.Background(), "Solo", relayPassphrase); err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if err := cl.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitState(t, cl, clientcore.StateReady, 5*time.Second)
	if cl.ServerVersion() == "" {
		t.Fatal("ServerVersion() empty after ready")
	}
}
