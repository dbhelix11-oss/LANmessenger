package update

import (
	"context"
	"fmt"
	"net"

	"golang.org/x/net/proxy"
)

// socksDialContext returns an http.Transport.DialContext that dials through
// the SOCKS5 proxy at proxyAddr — the same mechanism internal/clientcore
// uses for a remote client's WebSocket connection over Tor, duplicated here
// (rather than exported from clientcore) to keep internal/update from
// depending on it: neither package should import the other.
func socksDialContext(proxyAddr string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		d, err := proxy.SOCKS5(network, proxyAddr, nil, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("update: build SOCKS5 dialer: %w", err)
		}
		if cd, ok := d.(proxy.ContextDialer); ok {
			return cd.DialContext(ctx, network, addr)
		}
		return d.Dial(network, addr)
	}
}
