package tunnel

import (
	"net"

	"github.com/hashicorp/yamux"
)

// session is the subset of *yamux.Session this package needs, declared as
// an interface so tests can substitute a fake multiplexer instead of
// running real yamux sessions end to end.
//
// It exposes the Stream-returning Open/AcceptStream variants (not the
// net.Conn-returning Open/Accept) specifically so callers can read a
// stream's ID without a type assertion — needed to correlate a data stream
// with its StreamPreamble on the dedicated control stream (see listener.go).
type session interface {
	AcceptStream() (*yamux.Stream, error)
	OpenStream() (*yamux.Stream, error)
	Close() error
	Addr() net.Addr
}
