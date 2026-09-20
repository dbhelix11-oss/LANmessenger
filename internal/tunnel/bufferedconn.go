package tunnel

import (
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// bufferedConn shields a yamux stream from a genuine, if obscure, hazard
// found while building this feature: net/http's own "detect an early
// client disconnect while the handler runs" optimization (net/http calls
// this startBackgroundRead/abortPendingRead internally) issues a 1-byte
// probe read on the connection and aborts it via SetReadDeadline the
// moment the handler hijacks the connection for WebSocket. Over a real OS
// TCP socket that abort is atomic at the syscall level — either you get
// real bytes or a clean zero-byte timeout. Over a yamux stream (a
// pure-Go, channel-based implementation), the same abort can land in the
// middle of crypto/tls assembling one TLS record across more than one
// underlying Read call, and crypto/tls treats that as an unrecoverable
// connection error — poisoning every subsequent Read with the same
// spurious timeout, even after net/http correctly clears the deadline.
// Confirmed with a minimal, repeated, standalone repro isolating exactly
// this interaction (real TCP for every other hop, TLS is what triggers
// it, a direct in-process handoff never triggers it because the whole
// record tends to arrive as a single Read).
//
// The fix: never let anyone else's SetReadDeadline touch the raw stream.
// A background goroutine drains it continuously, with no deadline ever
// applied, and hands off already-fully-read, atomic chunks over a
// channel. The exposed Read/SetReadDeadline are implemented purely in
// terms of that channel and a timer, so a timeout can only ever occur
// before any bytes are handed to the caller — never mid-chunk — which
// means crypto/tls can never observe a partially-delivered record.
type bufferedConn struct {
	net.Conn // the raw yamux stream; Write/Close/LocalAddr etc. pass through

	chunkCh chan []byte
	doneCh  chan struct{}
	fillErr error

	mu       sync.Mutex
	leftover []byte

	rdlMu    sync.Mutex
	deadline time.Time
}

func newBufferedConn(underlying net.Conn) *bufferedConn {
	c := &bufferedConn{
		Conn:    underlying,
		chunkCh: make(chan []byte, 64),
		doneCh:  make(chan struct{}),
	}
	go c.fill()
	return c
}

// fill continuously reads the raw stream with no deadline ever applied to
// it, so it can never be interrupted mid-read by anything external.
func (c *bufferedConn) fill() {
	defer close(c.doneCh)
	buf := make([]byte, 32*1024)
	for {
		n, err := c.Conn.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			c.chunkCh <- chunk
		}
		if err != nil {
			c.fillErr = err
			return
		}
	}
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	if len(c.leftover) > 0 {
		n := copy(p, c.leftover)
		c.leftover = c.leftover[n:]
		c.mu.Unlock()
		return n, nil
	}
	c.mu.Unlock()

	c.rdlMu.Lock()
	dl := c.deadline
	c.rdlMu.Unlock()

	var timeout <-chan time.Time
	if !dl.IsZero() {
		d := time.Until(dl)
		if d <= 0 {
			return 0, os.ErrDeadlineExceeded
		}
		t := time.NewTimer(d)
		defer t.Stop()
		timeout = t.C
	}

	select {
	case chunk, ok := <-c.chunkCh:
		if !ok {
			return 0, io.EOF
		}
		return c.deliver(p, chunk), nil
	case <-c.doneCh:
		select {
		case chunk, ok := <-c.chunkCh:
			if ok {
				return c.deliver(p, chunk), nil
			}
		default:
		}
		if c.fillErr != nil {
			return 0, c.fillErr
		}
		return 0, io.EOF
	case <-timeout:
		return 0, os.ErrDeadlineExceeded
	}
}

// deliver copies chunk into p, stashing any remainder as leftover for the
// next Read call.
func (c *bufferedConn) deliver(p, chunk []byte) int {
	n := copy(p, chunk)
	if n < len(chunk) {
		c.mu.Lock()
		c.leftover = chunk[n:]
		c.mu.Unlock()
	}
	return n
}

func (c *bufferedConn) SetReadDeadline(t time.Time) error {
	c.rdlMu.Lock()
	c.deadline = t
	c.rdlMu.Unlock()
	return nil
}

func (c *bufferedConn) SetDeadline(t time.Time) error {
	_ = c.Conn.SetWriteDeadline(t)
	return c.SetReadDeadline(t)
}
