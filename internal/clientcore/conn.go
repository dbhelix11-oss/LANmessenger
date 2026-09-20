package clientcore

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/net/proxy"

	"lanmessenger/internal/crypto"
	"lanmessenger/internal/proto"
)

const clientVersion = "0.1.0"

// wsConn wraps a websocket connection with a serialized writer.
type wsConn struct {
	ws      *websocket.Conn
	writeMu sync.Mutex
}

func (w *wsConn) send(ctx context.Context, t proto.Type, id string, payload any) error {
	env, err := proto.NewEnvelope(t, id, time.Now().UnixMilli(), payload)
	if err != nil {
		return err
	}
	raw, err := proto.Marshal(env)
	if err != nil {
		return err
	}
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return w.ws.Write(wctx, websocket.MessageText, raw)
}

func (w *wsConn) read(ctx context.Context) (*proto.Envelope, error) {
	typ, data, err := w.ws.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageText {
		return nil, errors.New("clientcore: non-text frame from relay")
	}
	return proto.Parse(data)
}

func (w *wsConn) close(reason string) {
	_ = w.ws.Close(websocket.StatusNormalClosure, reason)
}

// pinnedTLSConfig returns a tls.Config that accepts exactly the certificate
// whose SHA-256 matches fingerprint (trust on first use). An empty fingerprint
// means "accept anything" and is only valid during first-run enrollment when
// the user is about to confirm the fingerprint.
func pinnedTLSConfig(fingerprint string) *tls.Config {
	want := normalizeFingerprint(fingerprint)
	return &tls.Config{
		InsecureSkipVerify: true, // we do our own check below
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("clientcore: relay presented no certificate")
			}
			if want == "" {
				return nil
			}
			sum := sha256.Sum256(rawCerts[0])
			if hex.EncodeToString(sum[:]) != want {
				return fmt.Errorf("clientcore: relay certificate fingerprint mismatch (possible interception)")
			}
			return nil
		},
	}
}

// FingerprintOfPresentedCert dials the relay without pinning and returns the
// fingerprint of the certificate it presents, for the first-run confirmation
// screen.
func FingerprintOfPresentedCert(ctx context.Context, serverAddr string) (string, error) {
	d := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}}
	conn, err := d.DialContext(ctx, "tcp", serverAddr)
	if err != nil {
		return "", fmt.Errorf("clientcore: probe relay: %w", err)
	}
	defer conn.Close()
	state := conn.(*tls.Conn).ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return "", errors.New("clientcore: relay presented no certificate")
	}
	sum := sha256.Sum256(state.PeerCertificates[0].Raw)
	return colonHex(sum[:]), nil
}

// FingerprintOfPresentedCertVia is [FingerprintOfPresentedCert], but dials
// serverAddr through a SOCKS5 proxy (a local Tor daemon in practice)
// instead of directly. Needed to probe a relay reached through the cloud
// tunnel's .onion address, which a plain tls.Dialer can never resolve — no
// DNS server knows what a .onion address is; only Tor does. An empty
// socksProxy is equivalent to calling FingerprintOfPresentedCert.
func FingerprintOfPresentedCertVia(ctx context.Context, serverAddr, socksProxy string) (string, error) {
	if socksProxy == "" {
		return FingerprintOfPresentedCert(ctx, serverAddr)
	}
	raw, err := dialSOCKS5(ctx, socksProxy, "tcp", serverAddr)
	if err != nil {
		return "", fmt.Errorf("clientcore: probe relay via %s: %w", socksProxy, err)
	}
	defer raw.Close()

	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tlsConn := tls.Client(raw, &tls.Config{InsecureSkipVerify: true})
	if err := tlsConn.HandshakeContext(hctx); err != nil {
		return "", fmt.Errorf("clientcore: tls handshake: %w", err)
	}
	state := tlsConn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return "", errors.New("clientcore: relay presented no certificate")
	}
	sum := sha256.Sum256(state.PeerCertificates[0].Raw)
	return colonHex(sum[:]), nil
}

func normalizeFingerprint(fp string) string {
	fp = strings.ToLower(fp)
	fp = strings.NewReplacer(":", "", " ", "", "-", "").Replace(fp)
	return fp
}

func colonHex(b []byte) string {
	parts := make([]string, len(b))
	for i, x := range b {
		parts[i] = hex.EncodeToString([]byte{x})
	}
	return strings.Join(parts, ":")
}

// dial opens a websocket to the relay with certificate pinning. When
// cfg.SOCKSProxy is set, the underlying TCP connection is made through that
// SOCKS5 proxy instead of dialing directly — TLS still happens in this
// process, against the same pinned fingerprint, so a remote client reached
// through the cloud tunnel over Tor is authenticated exactly like a LAN one.
func (c *Client) dial(ctx context.Context) (*wsConn, error) {
	transport := &http.Transport{
		TLSClientConfig:     c.tlsConfig,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	if c.cfg.SOCKSProxy != "" {
		transport.DialContext = socksDialContext(c.cfg.SOCKSProxy)
	}
	httpClient := &http.Client{Transport: transport}
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(dctx, "wss://"+c.cfg.ServerAddr+"/ws", &websocket.DialOptions{
		HTTPClient: httpClient,
	})
	if err != nil {
		return nil, fmt.Errorf("clientcore: connect %s: %w", c.cfg.ServerAddr, err)
	}
	ws.SetReadLimit(4 << 20)
	return &wsConn{ws: ws}, nil
}

// socksDialContext returns an http.Transport.DialContext that dials through
// the SOCKS5 proxy at proxyAddr.
func socksDialContext(proxyAddr string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialSOCKS5(ctx, proxyAddr, network, addr)
	}
}

// dialSOCKS5 dials target through the SOCKS5 proxy at proxyAddr (a local
// Tor daemon in practice), honoring ctx's deadline/cancellation when the
// underlying dialer supports it.
func dialSOCKS5(ctx context.Context, proxyAddr, network, target string) (net.Conn, error) {
	d, err := proxy.SOCKS5(network, proxyAddr, nil, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("clientcore: build SOCKS5 dialer: %w", err)
	}
	if cd, ok := d.(proxy.ContextDialer); ok {
		return cd.DialContext(ctx, network, target)
	}
	return d.Dial(network, target)
}

// handshakeResult reports the outcome of the auth handshake.
type handshakeResult struct {
	state ConnState // StateReady or StatePendingApproval
	admin bool
}

// handshake performs hello + challenge/response on w. If enroll is non-nil it
// registers a new device; otherwise it resumes cfg.DeviceID.
func (c *Client) handshake(ctx context.Context, w *wsConn, enroll *proto.Enroll) (handshakeResult, error) {
	var res handshakeResult

	hello := proto.Hello{ClientVersion: clientVersion}
	if enroll == nil {
		hello.DeviceID = c.cfg.DeviceID
	}
	if err := w.send(ctx, proto.TypeHello, "", hello); err != nil {
		return res, err
	}

	env, err := w.read(ctx)
	if err != nil {
		return res, err
	}
	if env.Type != proto.TypeAuthChallenge {
		return res, unexpected(env, "auth_challenge")
	}
	var chal proto.AuthChallenge
	if err := env.Unmarshal(&chal); err != nil {
		return res, err
	}
	salt, err := base64.StdEncoding.DecodeString(chal.Salt)
	if err != nil {
		return res, fmt.Errorf("clientcore: bad challenge salt: %w", err)
	}
	nonce, err := base64.StdEncoding.DecodeString(chal.Nonce)
	if err != nil {
		return res, fmt.Errorf("clientcore: bad challenge nonce: %w", err)
	}
	proof := crypto.Proof(crypto.DeriveKey(c.passphrase, salt), nonce)
	if err := w.send(ctx, proto.TypeAuthResponse, "", proto.AuthResponse{
		DeviceID: hello.DeviceID,
		Proof:    base64.StdEncoding.EncodeToString(proof),
	}); err != nil {
		return res, err
	}

	if enroll != nil {
		if err := w.send(ctx, proto.TypeEnroll, "", *enroll); err != nil {
			return res, err
		}
	}

	// Read until we reach a terminal handshake frame.
	for {
		env, err := w.read(ctx)
		if err != nil {
			return res, err
		}
		switch env.Type {
		case proto.TypeError:
			var e proto.ErrorBody
			_ = env.Unmarshal(&e)
			return res, &RelayError{Code: e.Code, Message: e.Message}

		case proto.TypeEnrollResult:
			var er proto.EnrollResult
			if err := env.Unmarshal(&er); err != nil {
				return res, err
			}
			c.applyEnrollResult(er)
			if er.State != proto.StateActive {
				res.state = StatePendingApproval
				return res, nil
			}
			// active: wait for the ready frame that follows.

		case proto.TypeReady:
			var rd proto.Ready
			if err := env.Unmarshal(&rd); err != nil {
				return res, err
			}
			res.state = StateReady
			res.admin = rd.Admin
			return res, nil

		default:
			// The relay may interleave directory/presence frames; feed them to
			// the normal handler so nothing is lost.
			c.dispatch(ctx, w, env)
		}
	}
}

func unexpected(env *proto.Envelope, want string) error {
	return fmt.Errorf("clientcore: expected %s from relay, got %s", want, env.Type)
}

// RelayError is an error frame received from the relay.
type RelayError struct {
	Code    string
	Message string
}

func (e *RelayError) Error() string { return fmt.Sprintf("relay error %s: %s", e.Code, e.Message) }
