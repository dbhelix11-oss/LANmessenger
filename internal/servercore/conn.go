package servercore

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"lanmessenger/internal/crypto"
	"lanmessenger/internal/proto"
	"lanmessenger/internal/version"
)

const (
	pingTimeout      = 10 * time.Second
	writeTimeout     = 10 * time.Second
	handshakeTimeout = 30 * time.Second
)

// conn is one client's connection to the relay, from TLS upgrade through the
// auth handshake and the message read loop.
type conn struct {
	srv *Server
	ws  *websocket.Conn

	deviceID string
	entry    proto.DirectoryEntry
	admin    bool

	ready atomic.Bool

	// presence, guarded by pmu.
	pmu       sync.Mutex
	status    proto.Status
	statusMsg string

	writeMu sync.Mutex

	ctx    context.Context
	cancel context.CancelFunc
}

func (c *conn) isReady() bool           { return c.ready.Load() }
func (c *conn) getStatus() proto.Status { c.pmu.Lock(); defer c.pmu.Unlock(); return c.status }
func (c *conn) getStatusMsg() string    { c.pmu.Lock(); defer c.pmu.Unlock(); return c.statusMsg }

func (c *conn) setStatus(s proto.Status, msg string) {
	c.pmu.Lock()
	c.status, c.statusMsg = s, msg
	c.pmu.Unlock()
}

// handleWS upgrades an HTTP request to a WebSocket and runs the connection.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if !s.connLimiter.Allow(hostOnly(r.RemoteAddr)) {
		s.log.Debug("connection attempt rate-limited", "remote", r.RemoteAddr)
		http.Error(w, "too many connection attempts", http.StatusTooManyRequests)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"*"}, // clients are native apps on the LAN, not browsers with a meaningful Origin
	})
	if err != nil {
		s.log.Debug("ws accept failed", "err", err)
		return
	}
	ws.SetReadLimit(int64(s.cfg.MaxFrameBytes))

	ctx, cancel := context.WithCancel(r.Context())
	c := &conn{
		srv:    s,
		ws:     ws,
		status: proto.StatusAvailable,
		ctx:    ctx,
		cancel: cancel,
	}
	defer cancel()

	hctx, hcancel := context.WithTimeout(ctx, handshakeTimeout)
	err = s.handshake(hctx, c)
	hcancel()
	if err != nil {
		s.log.Debug("handshake failed", "remote", r.RemoteAddr, "err", err)
		_ = ws.Close(websocket.StatusPolicyViolation, truncateReason(err.Error()))
		return
	}

	s.addConn(c)
	defer func() {
		s.removeConn(c)
		if c.isReady() {
			c.setStatus(c.getStatus(), c.getStatusMsg())
			s.broadcastPresence(c, false)
			_ = s.store.setPresence(c.deviceID, c.getStatus(), c.getStatusMsg())
		}
	}()

	if c.isReady() {
		s.afterReady(c)
	}

	go c.heartbeat()
	c.readLoop()
}

// readLoop reads frames until the connection closes or errors.
func (c *conn) readLoop() {
	for {
		typ, data, err := c.ws.Read(c.ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		env, err := proto.Parse(data)
		if err != nil {
			c.sendError("", proto.ErrBadRequest, err.Error())
			continue
		}
		c.srv.dispatch(c, env)
	}
}

func (c *conn) heartbeat() {
	iv := time.Duration(c.srv.cfg.HeartbeatSeconds) * time.Second
	if iv <= 0 {
		iv = 30 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(c.ctx, pingTimeout)
			err := c.ws.Ping(pctx)
			cancel()
			if err != nil {
				c.close(proto.ErrInternal, "heartbeat timeout")
				return
			}
		}
	}
}

// send marshals an envelope and writes it to the socket.
func (c *conn) send(t proto.Type, id string, payload any) error {
	env, err := proto.NewEnvelope(t, id, time.Now().UnixMilli(), payload)
	if err != nil {
		return err
	}
	raw, err := proto.Marshal(env)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	wctx, cancel := context.WithTimeout(c.ctx, writeTimeout)
	defer cancel()
	return c.ws.Write(wctx, websocket.MessageText, raw)
}

// trySend is send that logs and drops on error instead of returning it.
func (c *conn) trySend(t proto.Type, id string, payload any) {
	if err := c.send(t, id, payload); err != nil && !errors.Is(err, context.Canceled) {
		c.srv.log.Debug("send failed", "device", c.deviceID, "type", t, "err", err)
	}
}

func (c *conn) sendError(id, code, msg string) {
	c.trySend(proto.TypeError, id, proto.ErrorBody{Code: code, Message: msg})
}

func (c *conn) close(code, reason string) {
	_ = c.ws.Close(websocket.StatusCode(4000), truncateReason(reason))
	c.cancel()
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

func truncateReason(s string) string {
	// WebSocket close reasons are limited to 123 bytes.
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

// --- handshake ---------------------------------------------------------

func (s *Server) handshake(ctx context.Context, c *conn) error {
	// 1. hello
	env, err := readFrame(ctx, c.ws)
	if err != nil {
		return err
	}
	if env.Type != proto.TypeHello {
		return errors.New("expected hello")
	}
	var hello proto.Hello
	if err := env.Unmarshal(&hello); err != nil {
		return err
	}
	if s.cfg.MinClientVersion != "" && hello.ClientVersion != "" &&
		version.Compare(hello.ClientVersion, s.cfg.MinClientVersion) < 0 {
		c.sendError("", proto.ErrClientTooOld, "this client build is too old; update to continue")
		return errors.New("client too old")
	}

	// 2. auth_challenge
	nonce, err := crypto.NewChallengeNonce()
	if err != nil {
		return err
	}
	nonceB64 := base64.StdEncoding.EncodeToString(nonce)
	if err := c.send(proto.TypeAuthChallenge, "", proto.AuthChallenge{
		Nonce: nonceB64,
		Salt:  s.cfg.Passphrase.Salt,
	}); err != nil {
		return err
	}

	// 3. auth_response
	env, err = readFrame(ctx, c.ws)
	if err != nil {
		return err
	}
	if env.Type != proto.TypeAuthResponse {
		return errors.New("expected auth_response")
	}
	var ar proto.AuthResponse
	if err := env.Unmarshal(&ar); err != nil {
		return err
	}
	ok, err := s.cfg.Passphrase.CheckResponse(nonceB64, ar.Proof)
	if err != nil {
		return err
	}
	if !ok {
		c.sendError("", proto.ErrAuthFailed, "passphrase incorrect")
		return errors.New("bad passphrase")
	}

	// 4. existing device vs new enrollment
	if hello.DeviceID != "" {
		return s.resumeDevice(c, hello.DeviceID)
	}
	return s.enrollDevice(ctx, c)
}

func readFrame(ctx context.Context, ws *websocket.Conn) (*proto.Envelope, error) {
	typ, data, err := ws.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageText {
		return nil, errors.New("expected a text frame")
	}
	return proto.Parse(data)
}
