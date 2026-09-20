// Package servercore implements the lanmessenger relay server: device
// enrollment and directory, presence fan-out, end-to-end-encrypted message
// relay, and an offline queue for devices that are asleep.
//
// The server never sees message plaintext. It relays sealed {to, from, nonce,
// ciphertext} frames and, when a recipient is offline, stores them until the
// recipient reconnects.
package servercore

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"lanmessenger/internal/proto"
	"lanmessenger/internal/ratelimit"
)

// Server is a running relay. Construct it with [New] and drive it with [Run].
type Server struct {
	cfg     *Config
	store   *serverStore
	log     *slog.Logger
	tlsCert tls.Certificate

	// connLimiter bounds new-connection attempts per remote address;
	// frameLimiter bounds inbound frames per device ID, once authenticated.
	// Both are in-memory only and reset on restart.
	connLimiter  *ratelimit.Limiter
	frameLimiter *ratelimit.Limiter

	mu    sync.RWMutex
	conns map[string]*conn // device_id -> connection (ready or pending)

	// cfgMu guards writes to cfg.AdminDevices + Config.Save from concurrent
	// enrollments / approvals.
	cfgMu sync.Mutex
}

// New creates a server from cfg. It ensures the data directory exists, opens the
// database (running migrations), and loads or generates the TLS certificate.
func New(cfg *Config, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	cfg.applyDefaults() // idempotent; covers callers that build Config directly
	if err := cfg.EnsureDataDir(); err != nil {
		return nil, fmt.Errorf("servercore: data dir: %w", err)
	}
	st, err := openServerStore(cfg.DBPath())
	if err != nil {
		return nil, err
	}
	cert, err := ensureTLSCert(cfg.CertPath(), cfg.KeyPath())
	if err != nil {
		st.Close()
		return nil, err
	}
	return &Server{
		cfg:     cfg,
		store:   st,
		log:     logger,
		tlsCert: cert,
		connLimiter: ratelimit.New(cfg.RateLimit.MaxConnectsPerWindow,
			time.Duration(cfg.RateLimit.ConnectWindowSeconds)*time.Second),
		frameLimiter: ratelimit.New(cfg.RateLimit.MaxFramesPerWindow,
			time.Duration(cfg.RateLimit.FrameWindowSeconds)*time.Second),
		conns: make(map[string]*conn),
	}, nil
}

// CertFingerprint returns the SHA-256 fingerprint of the server's TLS
// certificate, which clients pin on first connect.
func (s *Server) CertFingerprint() (string, error) {
	return CertFingerprintFromFile(s.cfg.CertPath())
}

// Close releases the server's database handle. It is only needed when a Server
// is created but [Run] is never called (e.g. during `setup`); Run closes the
// store itself.
func (s *Server) Close() error { return s.store.Close() }

// Run serves until ctx is cancelled, then shuts down gracefully. It returns nil
// on a clean shutdown.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("servercore: listen %s: %w", s.cfg.ListenAddr, err)
	}
	return s.serve(ctx, ln)
}

// RunListener serves on an already-bound listener until ctx is cancelled. Useful
// for tests (bind 127.0.0.1:0 and read ln.Addr) and for embedding the relay in
// another process.
func (s *Server) RunListener(ctx context.Context, ln net.Listener) error {
	return s.serve(ctx, ln)
}

// serve owns the store lifetime and the background loops, then runs the LAN
// listener. It closes the store on return.
func (s *Server) serve(ctx context.Context, ln net.Listener) error {
	defer s.store.Close()
	go s.purgeLoop(ctx)
	go s.rateLimitGCLoop(ctx)
	if s.cfg.Tunnel != nil {
		go s.runTunnel(ctx)
	}
	return s.serveHTTP(ctx, ln)
}

// serveHTTP runs the HTTPS/WebSocket server on ln until ctx is cancelled. It
// does not touch the store or the background loops started by serve, so it's
// safe to call concurrently against more than one listener sharing this
// *Server — e.g. the LAN listener plus a tunnel-fed one.
func (s *Server) serveHTTP(ctx context.Context, ln net.Listener) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	httpSrv := &http.Server{
		Handler:           mux,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{s.tlsCert}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		s.log.Info("relay listening", "addr", ln.Addr().String())
		errc <- httpSrv.ServeTLS(ln, "", "")
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
		s.closeAllConns()
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("servercore: serve: %w", err)
	}
}

func (s *Server) purgeLoop(ctx context.Context) {
	retention := time.Duration(s.cfg.QueueRetentionHours) * time.Hour
	if retention <= 0 {
		return
	}
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := s.store.purgeExpired(retention); err != nil {
				s.log.Warn("queue purge failed", "err", err)
			} else if n > 0 {
				s.log.Info("purged expired queued messages", "count", n)
			}
		}
	}
}

// rateLimitGCLoop periodically drops rate-limiter bookkeeping for sources
// that have gone quiet, so memory doesn't grow unbounded over a long
// uptime. Decoupled from purgeLoop, which can be disabled entirely by
// QueueRetentionHours == 0.
func (s *Server) rateLimitGCLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.connLimiter.GC(now)
			s.frameLimiter.GC(now)
		}
	}
}

// --- connection registry --------------------------------------------------

func (s *Server) addConn(c *conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old := s.conns[c.deviceID]; old != nil {
		// A second login for the same device replaces the first.
		old.close(proto.ErrForbidden, "replaced by a newer connection")
	}
	s.conns[c.deviceID] = c
}

func (s *Server) removeConn(c *conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conns[c.deviceID] == c {
		delete(s.conns, c.deviceID)
	}
}

func (s *Server) lookupConn(deviceID string) (*conn, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.conns[deviceID]
	return c, ok
}

// readyConns returns a snapshot of all connections that have completed the
// handshake, excluding the one passed (may be nil).
func (s *Server) readyConns(except *conn) []*conn {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*conn, 0, len(s.conns))
	for _, c := range s.conns {
		if c == except || !c.isReady() {
			continue
		}
		out = append(out, c)
	}
	return out
}

func (s *Server) closeAllConns() {
	s.mu.Lock()
	conns := make([]*conn, 0, len(s.conns))
	for _, c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		c.close(proto.ErrInternal, "server shutting down")
	}
}

// broadcastDirectoryUpdate tells every other ready connection about a directory
// change. The subject device does not need to hear about itself.
func (s *Server) broadcastDirectoryUpdate(entry proto.DirectoryEntry, removed bool) {
	for _, c := range s.readyConns(nil) {
		if c.deviceID == entry.DeviceID {
			continue
		}
		c.trySend(proto.TypeDirectoryUpdate, "", proto.DirectoryUpdate{Entry: entry, Removed: removed})
	}
}

// broadcastPresence tells every other ready connection about a presence change.
func (s *Server) broadcastPresence(from *conn, online bool) {
	pu := proto.PresenceUpdate{
		DeviceID: from.deviceID,
		Status:   from.getStatus(),
		Message:  from.getStatusMsg(),
		Online:   online && from.getStatus() != proto.StatusInvisible,
		TS:       time.Now().UnixMilli(),
	}
	for _, c := range s.readyConns(from) {
		c.trySend(proto.TypePresenceUpdate, "", pu)
	}
}
