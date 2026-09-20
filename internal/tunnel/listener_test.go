package tunnel

import (
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

func newSessionPair(t *testing.T) (client, server *yamux.Session) {
	t.Helper()
	c, s := net.Pipe()
	clientSess, err := yamux.Client(c, nil)
	if err != nil {
		t.Fatalf("yamux.Client: %v", err)
	}
	serverSess, err := yamux.Server(s, nil)
	if err != nil {
		t.Fatalf("yamux.Server: %v", err)
	}
	t.Cleanup(func() {
		_ = clientSess.Close()
		_ = serverSess.Close()
	})
	return clientSess, serverSess
}

func TestSessionListener_StripsPreambleAndReportsRemoteAddr(t *testing.T) {
	client, server := newSessionPair(t)
	ln := NewSessionListener(server, slog.Default())

	ctrl, err := client.OpenStream()
	if err != nil {
		t.Fatalf("open control stream: %v", err)
	}

	const wantAddr = "203.0.113.7:54321"
	const msg = "payload after preamble"

	openErr := make(chan error, 1)
	go func() {
		stream, err := client.OpenStream()
		if err != nil {
			openErr <- err
			return
		}
		defer stream.Close()
		preamble := StreamPreamble{StreamID: stream.StreamID(), RemoteAddr: wantAddr, OpenedAtUnixMs: time.Now().UnixMilli()}
		if err := WriteFrame(ctrl, preamble); err != nil {
			openErr <- err
			return
		}
		_, err = stream.Write([]byte(msg))
		openErr <- err
	}()

	conn, err := ln.Accept()
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	defer conn.Close()

	if got := conn.RemoteAddr().String(); got != wantAddr {
		t.Fatalf("RemoteAddr = %q, want %q", got, wantAddr)
	}

	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if string(buf) != msg {
		t.Fatalf("got %q, want %q", buf, msg)
	}
	if err := <-openErr; err != nil {
		t.Fatalf("open/write: %v", err)
	}
}

func TestSessionListener_SkipsStreamWithNoMatchingPreamble(t *testing.T) {
	orig := preambleTimeout
	preambleTimeout = 50 * time.Millisecond
	defer func() { preambleTimeout = orig }()

	client, server := newSessionPair(t)
	ln := NewSessionListener(server, slog.Default())

	ctrl, err := client.OpenStream()
	if err != nil {
		t.Fatalf("open control stream: %v", err)
	}

	// A data stream opened but never announced on the control stream:
	// Accept must give up on it after preambleTimeout and move on to the
	// next stream rather than hanging forever.
	bad, err := client.OpenStream()
	if err != nil {
		t.Fatalf("open bad stream: %v", err)
	}
	defer bad.Close()

	const wantAddr = "198.51.100.9:1111"
	goodErr := make(chan error, 1)
	go func() {
		stream, err := client.OpenStream()
		if err != nil {
			goodErr <- err
			return
		}
		defer stream.Close()
		preamble := StreamPreamble{StreamID: stream.StreamID(), RemoteAddr: wantAddr, OpenedAtUnixMs: time.Now().UnixMilli()}
		goodErr <- WriteFrame(ctrl, preamble)
	}()

	conn, err := ln.Accept()
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	defer conn.Close()
	if got := conn.RemoteAddr().String(); got != wantAddr {
		t.Fatalf("RemoteAddr = %q, want %q", got, wantAddr)
	}
	if err := <-goodErr; err != nil {
		t.Fatalf("write good preamble: %v", err)
	}
}
