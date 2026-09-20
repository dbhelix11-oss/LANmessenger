package tunnel

import (
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

// preambleTimeout bounds how long Accept waits for a data stream's
// matching StreamPreamble to arrive on the control stream before giving up
// on it, mirroring the relay's own handshake-timeout convention. A var, not
// a const, so tests can shrink it.
var preambleTimeout = 5 * time.Second

// SessionListener adapts a multiplexed tunnel session into a net.Listener,
// so tunnel-arriving connections can be served by the exact same
// HTTP/WebSocket stack as any other listener (servercore's serveHTTP).
//
// Design note: an earlier version read a StreamPreamble frame directly off
// the front of each data stream before handing it to net/http. That
// produced a real, reproducible bug: net/http's TLS-handshake path gets
// confused when it's the second thing (not the first) to ever call Read on
// a yamux stream — reliably reproduced with a minimal standalone repro
// isolating exactly that interaction, independent of any deadline this
// package itself sets. The fix: the preamble now travels over one
// dedicated control stream instead, correlated to its data stream by yamux
// stream ID. Every data stream net/http ever sees here is one it is
// genuinely the first and only reader of, exactly like a plain TCP
// listener's connections.
type SessionListener struct {
	sess session
	log  *slog.Logger

	ctrlOnce sync.Once
	ctrl     *yamux.Stream
	ctrlErr  error

	mu      sync.Mutex
	pending map[uint32]net.Addr
}

// NewSessionListener wraps sess. log may be nil.
func NewSessionListener(sess session, log *slog.Logger) *SessionListener {
	if log == nil {
		log = slog.Default()
	}
	return &SessionListener{sess: sess, log: log, pending: make(map[uint32]net.Addr)}
}

func (l *SessionListener) Close() error   { return l.sess.Close() }
func (l *SessionListener) Addr() net.Addr { return l.sess.Addr() }

// ensureControlStream accepts the one dedicated control stream — always
// the first stream the cloud tunnel's Hub opens on a session, immediately
// after backend authentication — and starts reading StreamPreamble
// messages from it. Lazy and idempotent: runs once, from the first Accept.
func (l *SessionListener) ensureControlStream() error {
	l.ctrlOnce.Do(func() {
		ctrl, err := l.sess.AcceptStream()
		if err != nil {
			l.ctrlErr = fmt.Errorf("tunnel: accept control stream: %w", err)
			return
		}
		l.ctrl = ctrl
		go l.readControl()
	})
	return l.ctrlErr
}

func (l *SessionListener) readControl() {
	for {
		var p StreamPreamble
		if err := ReadFrame(l.ctrl, &p); err != nil {
			l.log.Debug("tunnel control stream closed", "err", err)
			return
		}
		addr, err := parseRemoteAddr(p.RemoteAddr)
		if err != nil {
			l.log.Debug("tunnel control message had a bad remote addr", "err", err)
			continue
		}
		l.mu.Lock()
		l.pending[p.StreamID] = addr
		l.mu.Unlock()
	}
}

// Accept blocks until a new data stream arrives, waits for its matching
// StreamPreamble to arrive on the control stream, and returns a net.Conn
// reporting the preamble's RemoteAddr. A stream whose preamble doesn't
// show up within preambleTimeout is closed and Accept tries the next one
// rather than returning an error — one bad stream shouldn't take down the
// whole listener.
func (l *SessionListener) Accept() (net.Conn, error) {
	if err := l.ensureControlStream(); err != nil {
		return nil, err
	}
	for {
		stream, err := l.sess.AcceptStream()
		if err != nil {
			return nil, err
		}
		addr, err := l.waitForAddr(stream.StreamID())
		if err != nil {
			l.log.Debug("tunnel data stream never matched a control-stream preamble", "err", err)
			_ = stream.Close()
			continue
		}
		// Wrapped in bufferedConn: see its doc comment for why a data
		// stream net/http will run TLS over can never be handed to it
		// "bare".
		return &preambleConn{Conn: newBufferedConn(stream), remoteAddr: addr}, nil
	}
}

func (l *SessionListener) waitForAddr(id uint32) (net.Addr, error) {
	const pollInterval = 5 * time.Millisecond
	deadline := time.Now().Add(preambleTimeout)
	for {
		l.mu.Lock()
		addr, ok := l.pending[id]
		if ok {
			delete(l.pending, id)
		}
		l.mu.Unlock()
		if ok {
			return addr, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("tunnel: no control-stream preamble for stream %d within %s", id, preambleTimeout)
		}
		time.Sleep(pollInterval)
	}
}

// preambleConn overrides RemoteAddr with the address carried in a
// StreamPreamble; every other method is the wrapped stream's own.
type preambleConn struct {
	net.Conn
	remoteAddr net.Addr
}

func (c *preambleConn) RemoteAddr() net.Addr { return c.remoteAddr }

// parseRemoteAddr parses a "host:port" string into a net.Addr. It never
// resolves via DNS — the value always originates as a literal IP:port read
// from a real net.Conn.RemoteAddr() on the cloud tunnel side.
func parseRemoteAddr(s string) (net.Addr, error) {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("not a literal IP: %q", host)
	}
	var p int
	if _, err := fmt.Sscanf(port, "%d", &p); err != nil {
		return nil, fmt.Errorf("bad port %q: %w", port, err)
	}
	return &net.TCPAddr{IP: ip, Port: p}, nil
}
