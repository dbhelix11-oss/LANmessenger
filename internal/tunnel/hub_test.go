package tunnel

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"lanmessenger/internal/crypto"
	"lanmessenger/internal/ratelimit"
)

// startHub wires a real Hub to two real loopback TCP listeners and runs it
// for the duration of the test.
func startHub(t *testing.T, secret string, limiter *ratelimit.Limiter) (h *Hub, backendAddr, publicAddr string) {
	t.Helper()
	verifier, err := crypto.NewPassphraseVerifier(secret)
	if err != nil {
		t.Fatalf("build verifier: %v", err)
	}
	h = NewHub(verifier, limiter, slog.Default())

	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen backend: %v", err)
	}
	publicLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen public: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go h.AcceptBackends(ctx, backendLn)
	go h.AcceptPublic(ctx, publicLn)
	t.Cleanup(cancel)

	return h, backendLn.Addr().String(), publicLn.Addr().String()
}

// waitForBackend blocks until h has an authenticated backend session. A
// client-side AuthenticateAsBackend call returning successfully does NOT
// by itself guarantee the Hub has finished its own post-handshake work
// (opening the control stream, storing the link) — that happens in a
// separate goroutine on the Hub side. Dialing the public listener before
// this completes is a genuine race, not a hypothetical one: it reproduced
// reliably under `go test -count=10`.
func waitForBackend(t *testing.T, h *Hub) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.HasBackend() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for the hub to register the backend")
}

// dialFakeBackend authenticates as the backend and echoes every byte it
// receives on every data stream back onto that same stream — standing in
// for servercore handing an accepted connection to its own HTTP/WebSocket
// handler, without needing a real *servercore.Server in this package. The
// Hub always opens a dedicated control stream first (see listener.go); this
// fake backend drains it without acting on it, since this test only needs
// to prove bytes flow, not that a real SessionListener is on the other end.
func dialFakeBackend(t *testing.T, backendAddr, secret string) *tunnelSessionCloser {
	t.Helper()
	conn, err := net.Dial("tcp", backendAddr)
	if err != nil {
		t.Fatalf("dial backend: %v", err)
	}
	sess, err := AuthenticateAsBackend(context.Background(), conn, secret)
	if err != nil {
		t.Fatalf("AuthenticateAsBackend: %v", err)
	}
	go func() {
		first := true
		for {
			stream, err := sess.Accept()
			if err != nil {
				return
			}
			if first {
				first = false
				go drainForever(stream)
				continue
			}
			go func() {
				defer stream.Close()
				_, _ = io.Copy(stream, stream)
			}()
		}
	}()
	return &tunnelSessionCloser{conn: conn, sess: sess}
}

// drainForever reads from r until it errors, discarding everything — used
// to keep the fake backend's control-stream reads out of the way of a
// yamux session's own bookkeeping without acting on the messages.
func drainForever(r io.Reader) {
	buf := make([]byte, 4096)
	for {
		if _, err := r.Read(buf); err != nil {
			return
		}
	}
}

type tunnelSessionCloser struct {
	conn net.Conn
	sess session
}

func (c *tunnelSessionCloser) Close() {
	_ = c.sess.Close()
	_ = c.conn.Close()
}

func TestHub_EndToEndEchoThroughBackend(t *testing.T) {
	h, backendAddr, publicAddr := startHub(t, "secret", nil)

	backend := dialFakeBackend(t, backendAddr, "secret")
	defer backend.Close()
	waitForBackend(t, h)

	client, err := net.Dial("tcp", publicAddr)
	if err != nil {
		t.Fatalf("dial public: %v", err)
	}
	defer client.Close()

	const msg = "hello from a remote client"
	if _, err := client.Write([]byte(msg)); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != msg {
		t.Fatalf("got %q, want %q", buf, msg)
	}
}

func TestHub_RefusesPublicConnectionWithNoBackend(t *testing.T) {
	_, _, publicAddr := startHub(t, "secret", nil)

	client, err := net.Dial("tcp", publicAddr)
	if err != nil {
		t.Fatalf("dial public: %v", err)
	}
	defer client.Close()

	_ = client.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 1)
	if _, err := client.Read(buf); err == nil {
		t.Fatal("expected the connection to be closed with no backend authenticated")
	}
}

func TestHub_RateLimitsPublicConnections(t *testing.T) {
	limiter := ratelimit.New(1, time.Minute)
	h, backendAddr, publicAddr := startHub(t, "secret", limiter)

	backend := dialFakeBackend(t, backendAddr, "secret")
	defer backend.Close()
	waitForBackend(t, h)

	// Both dials come from 127.0.0.1 (loopback), so with max=1 the second
	// one must be rejected even though its ephemeral source port differs.
	first, err := net.Dial("tcp", publicAddr)
	if err != nil {
		t.Fatalf("dial public (first): %v", err)
	}
	defer first.Close()

	second, err := net.Dial("tcp", publicAddr)
	if err != nil {
		t.Fatalf("dial public (second): %v", err)
	}
	defer second.Close()

	_ = second.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 1)
	if _, err := second.Read(buf); err == nil {
		t.Fatal("expected the rate-limited connection to be closed")
	}
}
