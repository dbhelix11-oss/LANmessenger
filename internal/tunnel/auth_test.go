package tunnel

import (
	"context"
	"io"
	"net"
	"testing"

	"github.com/hashicorp/yamux"

	"lanmessenger/internal/crypto"
)

func TestAuthenticate_HappyPathProducesWorkingSessions(t *testing.T) {
	backendConn, cloudConn := net.Pipe()

	verifier, err := crypto.NewPassphraseVerifier("correct secret")
	if err != nil {
		t.Fatalf("build verifier: %v", err)
	}

	var backendSess, cloudSess *yamux.Session
	backendErr := make(chan error, 1)
	cloudErr := make(chan error, 1)

	go func() {
		sess, err := AuthenticateAsBackend(context.Background(), backendConn, "correct secret")
		backendSess = sess
		backendErr <- err
	}()
	go func() {
		sess, err := AuthenticateBackend(context.Background(), cloudConn, verifier)
		cloudSess = sess
		cloudErr <- err
	}()

	if err := <-backendErr; err != nil {
		t.Fatalf("AuthenticateAsBackend: %v", err)
	}
	if err := <-cloudErr; err != nil {
		t.Fatalf("AuthenticateBackend: %v", err)
	}
	defer backendSess.Close()
	defer cloudSess.Close()

	// Prove the resulting sessions are a working multiplexed transport: the
	// cloud side opens a stream, the backend side accepts it, bytes flow.
	const msg = "hello through the tunnel"
	streamErr := make(chan error, 1)
	go func() {
		stream, err := cloudSess.Open()
		if err != nil {
			streamErr <- err
			return
		}
		defer stream.Close()
		_, err = stream.Write([]byte(msg))
		streamErr <- err
	}()

	accepted, err := backendSess.Accept()
	if err != nil {
		t.Fatalf("backend Accept: %v", err)
	}
	defer accepted.Close()

	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(accepted, buf); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if string(buf) != msg {
		t.Fatalf("got %q, want %q", buf, msg)
	}
	if err := <-streamErr; err != nil {
		t.Fatalf("open/write stream: %v", err)
	}
}

func TestAuthenticateBackend_RejectsWrongSecret(t *testing.T) {
	backendConn, cloudConn := net.Pipe()
	defer backendConn.Close()
	defer cloudConn.Close()

	verifier, err := crypto.NewPassphraseVerifier("correct secret")
	if err != nil {
		t.Fatalf("build verifier: %v", err)
	}

	backendErr := make(chan error, 1)
	go func() {
		_, err := AuthenticateAsBackend(context.Background(), backendConn, "wrong secret")
		backendErr <- err
	}()

	if _, err := AuthenticateBackend(context.Background(), cloudConn, verifier); err == nil {
		t.Fatal("expected AuthenticateBackend to reject a wrong secret")
	}
	if err := <-backendErr; err == nil {
		t.Fatal("expected AuthenticateAsBackend to see the rejection")
	}
}

func TestAuthenticateBackend_RejectsWrongRole(t *testing.T) {
	backendConn, cloudConn := net.Pipe()
	defer backendConn.Close()
	defer cloudConn.Close()

	verifier, err := crypto.NewPassphraseVerifier("secret")
	if err != nil {
		t.Fatalf("build verifier: %v", err)
	}

	// AuthenticateBackend makes a best-effort attempt to write back a
	// Result{OK:false} before returning its error, so this fake backend
	// must read it — net.Pipe's Write blocks until something reads it, and
	// leaving it unread here would deadlock the test.
	done := make(chan error, 1)
	go func() {
		if err := WriteFrame(backendConn, Hello{Version: ProtocolVersion, Role: "not-backend"}); err != nil {
			done <- err
			return
		}
		var res Result
		done <- ReadFrame(backendConn, &res)
	}()

	if _, err := AuthenticateBackend(context.Background(), cloudConn, verifier); err == nil {
		t.Fatal("expected AuthenticateBackend to reject a non-backend role")
	}
	if err := <-done; err != nil {
		t.Fatalf("write hello / read result: %v", err)
	}
}
