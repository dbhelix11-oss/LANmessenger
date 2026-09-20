package tunnel

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"lanmessenger/internal/crypto"
	"lanmessenger/internal/ratelimit"
)

// handshakeTimeout bounds how long a candidate backend connection has to
// complete the auth handshake, mirroring the relay's own convention.
const handshakeTimeout = 30 * time.Second

// Hub is the cloud tunnel's entire runtime: it authenticates exactly one
// backend (the home relay) at a time, accepts public client connections,
// and pumps bytes between them over multiplexed streams. It holds no
// application data — no roster, no message queue, no E2E keys, nothing at
// rest — only the narrow secret proving a connection may call itself the
// backend.
type Hub struct {
	verifier crypto.PassphraseVerifier
	limiter  *ratelimit.Limiter
	log      *slog.Logger

	mu   sync.Mutex
	link *backendLink
}

// backendLink is one authenticated backend session plus its dedicated
// control stream, over which every data stream's StreamPreamble travels
// (see listener.go for why it's a separate stream, not written onto each
// data stream). ctrlMu serializes writes since multiple public connections
// can arrive concurrently and all need to announce themselves on the same
// control stream.
type backendLink struct {
	sess   session
	ctrl   net.Conn
	ctrlMu sync.Mutex
}

// NewHub creates a Hub that authenticates backends against verifier and
// rate-limits new public connections per source address via limiter.
// limiter and log may both be nil (nil limiter disables rate limiting).
func NewHub(verifier crypto.PassphraseVerifier, limiter *ratelimit.Limiter, log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	if limiter == nil {
		limiter = ratelimit.New(0, time.Minute) // max<=0 disables limiting
	}
	return &Hub{verifier: verifier, limiter: limiter, log: log}
}

// HasBackend reports whether an authenticated backend session is currently
// live. Mainly useful for tests synchronizing on "the backend has connected"
// without a fixed sleep.
func (h *Hub) HasBackend() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.link != nil
}

// AcceptBackends accepts connections on ln, treating each as a candidate
// backend. Only one authenticated backend session is live at a time; a
// newly authenticated backend replaces (and closes) any previous one. Runs
// until ctx is cancelled or ln.Accept fails.
func (h *Hub) AcceptBackends(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("tunnel: accept backend: %w", err)
		}
		go h.handleBackend(ctx, conn)
	}
}

func (h *Hub) handleBackend(ctx context.Context, conn net.Conn) {
	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()

	sess, err := AuthenticateBackend(hctx, conn, h.verifier)
	if err != nil {
		h.log.Warn("backend authentication failed", "remote", conn.RemoteAddr(), "err", err)
		_ = conn.Close()
		return
	}

	// Open the dedicated control stream immediately — always the very
	// first stream on a fresh session, so the Pi's SessionListener knows
	// exactly which stream to treat as the control channel.
	ctrl, err := sess.OpenStream()
	if err != nil {
		h.log.Warn("failed to open tunnel control stream", "remote", conn.RemoteAddr(), "err", err)
		_ = sess.Close()
		return
	}
	h.log.Info("backend authenticated", "remote", conn.RemoteAddr())

	link := &backendLink{sess: sess, ctrl: ctrl}
	h.mu.Lock()
	old := h.link
	h.link = link
	h.mu.Unlock()
	if old != nil {
		_ = old.sess.Close()
	}
}

// AcceptPublic accepts connections on ln from remote clients, opens one
// multiplexed stream per connection over the current backend session, and
// pumps bytes both ways until either side closes. A connection is refused
// immediately if no backend session is currently authenticated — the cloud
// box holds nothing at rest, so there is nothing to buffer while a backend
// is unavailable; the client's own reconnect-with-backoff handles the rest.
// Runs until ctx is cancelled or ln.Accept fails.
func (h *Hub) AcceptPublic(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("tunnel: accept public: %w", err)
		}
		go h.handlePublic(conn)
	}
}

func (h *Hub) handlePublic(conn net.Conn) {
	defer conn.Close()

	addr := conn.RemoteAddr().String()
	if !h.limiter.Allow(hostOnly(addr)) {
		h.log.Debug("public connection rate-limited", "remote", addr)
		return
	}

	h.mu.Lock()
	link := h.link
	h.mu.Unlock()
	if link == nil {
		h.log.Debug("no backend session; refusing public connection", "remote", addr)
		return
	}

	stream, err := link.sess.OpenStream()
	if err != nil {
		h.log.Debug("opening tunnel stream failed", "remote", addr, "err", err)
		return
	}
	defer stream.Close()

	// The preamble travels on the control stream, never on this data
	// stream — see listener.go for why.
	preamble := StreamPreamble{StreamID: stream.StreamID(), RemoteAddr: addr, OpenedAtUnixMs: time.Now().UnixMilli()}
	link.ctrlMu.Lock()
	err = WriteFrame(link.ctrl, preamble)
	link.ctrlMu.Unlock()
	if err != nil {
		h.log.Debug("writing stream preamble failed", "remote", addr, "err", err)
		return
	}

	pump(conn, stream)
}

// pump copies bytes both ways between a and b until either side is done,
// blind to whatever protocol they're carrying.
func pump(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
}

// hostOnly strips the port from a "host:port" address for rate-limiter
// keys, since a fresh ephemeral port on every connection would otherwise
// defeat per-source limiting entirely. Falls back to the input unchanged if
// it isn't a valid "host:port" (defensive; shouldn't happen for a real
// net.Conn.RemoteAddr()).
func hostOnly(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}
