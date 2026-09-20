package servercore

import (
	"context"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"lanmessenger/internal/clientcore"
	"lanmessenger/internal/crypto"
	"lanmessenger/internal/ratelimit"
	"lanmessenger/internal/tunnel"
)

const testTunnelSecret = "test tunnel shared secret, not the household passphrase"

// newTunneledTestServer starts a real *Server configured with a [tunnel]
// block pointing at a real tunnel.Hub bound to loopback listeners — a
// stand-in for a cloud box reached over Tor. Tor itself needs no exercising
// here: SOCKS5/onion dialing is isolated to the single dialTunnelConn seam,
// swapped out below for a plain direct dial, exactly as the design intends.
// Returns the LAN listener's address, the fake cloud's public listener
// address, and the relay's certificate fingerprint.
func newTunneledTestServer(t *testing.T, limiter *ratelimit.Limiter) (lanAddr, publicAddr, fingerprint string) {
	t.Helper()

	origDial := dialTunnelConn
	dialTunnelConn = func(ctx context.Context, _, target string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", target)
	}
	t.Cleanup(func() { dialTunnelConn = origDial })

	hub, backendLn, publicLn := newTestHub(t, limiter)
	hubCtx, hubCancel := context.WithCancel(context.Background())
	go hub.AcceptBackends(hubCtx, backendLn)
	go hub.AcceptPublic(hubCtx, publicLn)
	t.Cleanup(hubCancel)

	dir := t.TempDir()
	passVerifier, err := crypto.NewPassphraseVerifier(testPassphrase)
	if err != nil {
		t.Fatalf("passphrase verifier: %v", err)
	}
	cfg := &Config{
		DataDir:          dir,
		Passphrase:       passVerifier,
		HeartbeatSeconds: 3600, // don't ping during short tests
		Tunnel: &TunnelConfig{
			CloudOnionAddr: backendLn.Addr().String(),
			Secret:         testTunnelSecret,
		},
	}
	cfg.SetPath(dir + "/server.toml")
	cfg.applyDefaults()

	srv, err := New(cfg, discardLog())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.serve(ctx, ln); close(done) }()
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

	waitForCondition(t, "relay to authenticate with the cloud tunnel", 5*time.Second, hub.HasBackend)

	return ln.Addr().String(), publicLn.Addr().String(), fp
}

// newTestHub builds a real tunnel.Hub and its two loopback listeners,
// without starting its accept loops (callers start those, so a caller that
// only wants the backend listener, e.g. the wrong-secret test, can skip
// starting the public one).
func newTestHub(t *testing.T, limiter *ratelimit.Limiter) (hub *tunnel.Hub, backendLn, publicLn net.Listener) {
	t.Helper()
	verifier, err := crypto.NewPassphraseVerifier(testTunnelSecret)
	if err != nil {
		t.Fatalf("tunnel verifier: %v", err)
	}
	hub = tunnel.NewHub(verifier, limiter, discardLog())

	backendLn, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen backend: %v", err)
	}
	publicLn, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen public: %v", err)
	}
	return hub, backendLn, publicLn
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func waitForCondition(t *testing.T, what string, timeout time.Duration, fn func() bool) {
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

// newTunnelTestClient builds a real, completely ordinary clientcore.Client —
// the same constructor is used for both the "LAN" and "remote" roles below,
// deliberately: the whole point of this feature (plan decision 6) is that a
// tunnel-arriving client needs no different code, only a different address.
func newTunnelTestClient(t *testing.T, addr, fingerprint string) *clientcore.Client {
	t.Helper()
	cfg := &clientcore.Config{
		ServerAddr:      addr,
		CertFingerprint: fingerprint,
	}
	cfg.SetDir(t.TempDir())
	cl, err := clientcore.New(cfg, discardLog())
	if err != nil {
		t.Fatalf("clientcore.New: %v", err)
	}
	t.Cleanup(func() { cl.Stop(); cl.Close() })
	return cl
}

// drainUntilIncomingMessage waits for an inbound (not the sender's own
// "sent" echo) message event. Filtering by direction matters here
// specifically because this test exercises both directions on the same
// two clients: a naive "first EventMessage" wait can pick up a sender's
// own leftover Direction-out echo from its earlier SendText call instead
// of the reply it's actually waiting for.
func drainUntilIncomingMessage(t *testing.T, cl *clientcore.Client, timeout time.Duration) clientcore.Event {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case ev := <-cl.Events():
			if ev.Kind == clientcore.EventMessage && ev.Message != nil && ev.Message.Direction == clientcore.DirIn {
				return ev
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for an incoming message")
		}
	}
}

func waitSeesPeer(t *testing.T, cl *clientcore.Client, peerID string, timeout time.Duration) {
	t.Helper()
	waitForCondition(t, "client to see peer in its roster", timeout, func() bool {
		r, _ := cl.Roster()
		for _, e := range r {
			if e.DeviceID == peerID {
				return true
			}
		}
		return false
	})
}

// TestTunnel_MessageCrossesBothWays proves plan decision 6 (zero
// client-side changes) end to end: a real, unmodified clientcore.Client
// dialing the fake cloud's public listener exchanges messages both ways
// with a normal LAN-arriving client, through the real relay, the real Hub,
// and real yamux multiplexing — nothing in this path is a mock.
func TestTunnel_MessageCrossesBothWays(t *testing.T) {
	lanAddr, publicAddr, fp := newTunneledTestServer(t, nil)

	lan := newTunnelTestClient(t, lanAddr, fp)
	if _, err := lan.Enroll(context.Background(), "LanDevice", testPassphrase); err != nil {
		t.Fatalf("lan enroll: %v", err)
	}
	if err := lan.Start(context.Background()); err != nil {
		t.Fatalf("lan start: %v", err)
	}
	waitForCondition(t, "lan client ready", 5*time.Second, func() bool { return lan.State() == clientcore.StateReady })

	remote := newTunnelTestClient(t, publicAddr, fp)
	if _, err := remote.Enroll(context.Background(), "RemoteDevice", testPassphrase); err != nil {
		t.Fatalf("remote enroll: %v", err)
	}
	if err := remote.Start(context.Background()); err != nil {
		t.Fatalf("remote start: %v", err)
	}
	waitForCondition(t, "remote client ready", 5*time.Second, func() bool { return remote.State() == clientcore.StateReady })

	// Wait for BOTH sides' rosters, not just one: each client needs the
	// other's public keys to seal a message, and readiness (checked above)
	// doesn't guarantee the separate directory snapshot has been processed
	// yet — asserting only one direction here was a real source of test
	// flakiness ("clientcore: unknown peer") on the side that raced ahead.
	waitSeesPeer(t, lan, remote.DeviceID(), 5*time.Second)
	waitSeesPeer(t, remote, lan.DeviceID(), 5*time.Second)

	// Remote -> LAN, through the tunnel.
	if _, err := remote.SendText(context.Background(), lan.DeviceID(), "hello from outside the LAN"); err != nil {
		t.Fatalf("remote SendText: %v", err)
	}
	ev := drainUntilIncomingMessage(t, lan, 5*time.Second)
	if ev.Message.Body != "hello from outside the LAN" || ev.Message.PeerID != remote.DeviceID() {
		t.Fatalf("unexpected message on lan side: %+v", ev.Message)
	}

	// LAN -> remote, back through the tunnel.
	if _, err := lan.SendText(context.Background(), remote.DeviceID(), "reply from the LAN"); err != nil {
		t.Fatalf("lan SendText: %v", err)
	}
	ev = drainUntilIncomingMessage(t, remote, 5*time.Second)
	if ev.Message.Body != "reply from the LAN" || ev.Message.PeerID != lan.DeviceID() {
		t.Fatalf("unexpected message on remote side: %+v", ev.Message)
	}
}

// TestTunnel_WrongSecretNeverEstablishesSession is the negative case: a
// relay configured with the wrong shared secret must never obtain a usable
// tunnel session, no matter how many times runTunnel retries.
func TestTunnel_WrongSecretNeverEstablishesSession(t *testing.T) {
	origDial := dialTunnelConn
	dialTunnelConn = func(ctx context.Context, _, target string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", target)
	}
	defer func() { dialTunnelConn = origDial }()

	hub, backendLn, _ := newTestHub(t, nil)
	hubCtx, hubCancel := context.WithCancel(context.Background())
	defer hubCancel()
	go hub.AcceptBackends(hubCtx, backendLn)

	dir := t.TempDir()
	passVerifier, err := crypto.NewPassphraseVerifier(testPassphrase)
	if err != nil {
		t.Fatalf("passphrase verifier: %v", err)
	}
	cfg := &Config{
		DataDir:          dir,
		Passphrase:       passVerifier,
		HeartbeatSeconds: 3600,
		Tunnel: &TunnelConfig{
			CloudOnionAddr: backendLn.Addr().String(),
			Secret:         "definitely the wrong secret",
		},
	}
	cfg.SetPath(dir + "/server.toml")
	cfg.applyDefaults()

	srv, err := New(cfg, discardLog())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.serve(ctx, ln); close(done) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}()

	// Give runTunnel several retries' worth of time; it must never succeed.
	time.Sleep(300 * time.Millisecond)
	if hub.HasBackend() {
		t.Fatal("hub accepted a backend authenticating with the wrong secret")
	}
}
