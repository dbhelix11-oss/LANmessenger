package servercore

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/hashicorp/yamux"
	"golang.org/x/net/proxy"

	"lanmessenger/internal/tunnel"
)

// runTunnel dials the cloud tunnel and serves the relay's own HTTP/WebSocket
// stack over the resulting multiplexed session, retrying with backoff on any
// failure — the same reconnect shape clientcore already uses to reach this
// very relay. It never returns until ctx is cancelled. Only called when
// cfg.Tunnel is non-nil.
func (s *Server) runTunnel(ctx context.Context) {
	const maxBackoff = 30 * time.Second
	backoff := time.Second

	for {
		if ctx.Err() != nil {
			return
		}
		sess, err := s.dialTunnel(ctx)
		if err == nil {
			backoff = time.Second
			ln := tunnel.NewSessionListener(sess, s.log)
			err = s.serveHTTP(ctx, ln)
			_ = sess.Close()
			if ctx.Err() != nil {
				return
			}
		}
		s.log.Warn("tunnel session ended, retrying", "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
		}
	}
}

// dialTunnelConn reaches the cloud tunnel's backend address. It's a package
// variable, not a direct call to dialViaSOCKS5, so tests can substitute a
// plain direct dial — the only Tor-specific behavior in this whole feature
// is "how do we reach CloudOnionAddr," isolated to exactly this one seam.
var dialTunnelConn = dialViaSOCKS5

// dialTunnel makes one attempt to reach the cloud tunnel's backend .onion
// address through the relay's local Tor SOCKS proxy and authenticate as its
// backend.
func (s *Server) dialTunnel(ctx context.Context) (*yamux.Session, error) {
	raw, err := dialTunnelConn(ctx, s.cfg.Tunnel.SOCKSProxy, s.cfg.Tunnel.CloudOnionAddr)
	if err != nil {
		return nil, fmt.Errorf("servercore: dial cloud tunnel via %s: %w", s.cfg.Tunnel.SOCKSProxy, err)
	}
	sess, err := tunnel.AuthenticateAsBackend(ctx, raw, s.cfg.Tunnel.Secret)
	if err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("servercore: authenticate to cloud tunnel: %w", err)
	}
	return sess, nil
}

// dialViaSOCKS5 dials target through the SOCKS5 proxy at proxyAddr (a local
// Tor daemon in practice), honoring ctx's deadline/cancellation when the
// underlying dialer supports it.
func dialViaSOCKS5(ctx context.Context, proxyAddr, target string) (net.Conn, error) {
	d, err := proxy.SOCKS5("tcp", proxyAddr, nil, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("build SOCKS5 dialer: %w", err)
	}
	if cd, ok := d.(proxy.ContextDialer); ok {
		return cd.DialContext(ctx, "tcp", target)
	}
	return d.Dial("tcp", target)
}
