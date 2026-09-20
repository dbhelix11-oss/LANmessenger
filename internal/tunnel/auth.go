package tunnel

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/hashicorp/yamux"

	"lanmessenger/internal/crypto"
)

// yamuxConfig is the shared multiplexer configuration for both ends of the
// tunnel. Defaults include keepalive (~30s interval), matching the relay's
// own heartbeat convention.
func yamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	return cfg
}

// withDeadline applies ctx's deadline (if any) to conn for the duration of a
// blocking handshake, clearing it before returning.
func withDeadline(ctx context.Context, conn net.Conn) func() {
	dl, ok := ctx.Deadline()
	if !ok {
		return func() {}
	}
	_ = conn.SetDeadline(dl)
	return func() { _ = conn.SetDeadline(time.Time{}) }
}

// AuthenticateAsBackend runs the backend's (the home relay's) side of the
// handshake over conn, which must already be connected to the cloud tunnel.
// On success it wraps conn as a yamux client session. On error the caller
// still owns conn and should close it.
func AuthenticateAsBackend(ctx context.Context, conn net.Conn, secret string) (*yamux.Session, error) {
	defer withDeadline(ctx, conn)()

	if err := WriteFrame(conn, Hello{Version: ProtocolVersion, Role: RoleBackend}); err != nil {
		return nil, err
	}

	var ch Challenge
	if err := ReadFrame(conn, &ch); err != nil {
		return nil, fmt.Errorf("tunnel: read challenge: %w", err)
	}
	salt, err := base64.StdEncoding.DecodeString(ch.Salt)
	if err != nil {
		return nil, fmt.Errorf("tunnel: decode challenge salt: %w", err)
	}
	nonce, err := base64.StdEncoding.DecodeString(ch.Nonce)
	if err != nil {
		return nil, fmt.Errorf("tunnel: decode challenge nonce: %w", err)
	}
	proof := crypto.Proof(crypto.DeriveKey(secret, salt), nonce)
	if err := WriteFrame(conn, Response{Proof: base64.StdEncoding.EncodeToString(proof)}); err != nil {
		return nil, err
	}

	var res Result
	if err := ReadFrame(conn, &res); err != nil {
		return nil, fmt.Errorf("tunnel: read result: %w", err)
	}
	if !res.OK {
		return nil, fmt.Errorf("tunnel: backend auth rejected: %s", res.Message)
	}

	sess, err := yamux.Client(conn, yamuxConfig())
	if err != nil {
		return nil, fmt.Errorf("tunnel: start yamux client: %w", err)
	}
	return sess, nil
}

// AuthenticateBackend runs the cloud tunnel's side of the handshake over
// conn, a freshly accepted connection. On success it wraps conn as a yamux
// server session. On any failure it makes a best-effort attempt to write a
// Result{OK:false} before returning an error; the caller should close conn
// either way.
func AuthenticateBackend(ctx context.Context, conn net.Conn, verifier crypto.PassphraseVerifier) (*yamux.Session, error) {
	defer withDeadline(ctx, conn)()

	var hello Hello
	if err := ReadFrame(conn, &hello); err != nil {
		return nil, fmt.Errorf("tunnel: read hello: %w", err)
	}
	if hello.Role != RoleBackend {
		_ = WriteFrame(conn, Result{OK: false, Message: "unsupported role"})
		return nil, fmt.Errorf("tunnel: unsupported role %q", hello.Role)
	}

	nonce, err := crypto.NewChallengeNonce()
	if err != nil {
		return nil, err
	}
	nonceB64 := base64.StdEncoding.EncodeToString(nonce)
	if err := WriteFrame(conn, Challenge{Nonce: nonceB64, Salt: verifier.Salt}); err != nil {
		return nil, err
	}

	var resp Response
	if err := ReadFrame(conn, &resp); err != nil {
		return nil, fmt.Errorf("tunnel: read response: %w", err)
	}
	ok, err := verifier.CheckResponse(nonceB64, resp.Proof)
	if err != nil {
		_ = WriteFrame(conn, Result{OK: false, Message: "malformed proof"})
		return nil, fmt.Errorf("tunnel: check response: %w", err)
	}
	if !ok {
		_ = WriteFrame(conn, Result{OK: false, Message: "authentication failed"})
		return nil, errors.New("tunnel: backend authentication failed")
	}
	if err := WriteFrame(conn, Result{OK: true}); err != nil {
		return nil, err
	}

	sess, err := yamux.Server(conn, yamuxConfig())
	if err != nil {
		return nil, fmt.Errorf("tunnel: start yamux server: %w", err)
	}
	return sess, nil
}
