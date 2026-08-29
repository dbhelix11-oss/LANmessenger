package clientcore

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"lanmessenger/internal/crypto"
	"lanmessenger/internal/proto"
)

// Common errors.
var (
	ErrNotConfigured = errors.New("clientcore: client is not configured (server address, fingerprint, display name)")
	ErrNotEnrolled   = errors.New("clientcore: device is not enrolled")
	ErrNotReady      = errors.New("clientcore: not connected to the relay")
	ErrNoPassphrase  = errors.New("clientcore: household passphrase is not set")
	ErrUnknownPeer   = errors.New("clientcore: unknown peer")
)

// Client is a headless lanmessenger client. Construct with [New], drive the
// connection with [Client.Start], and render [Client.Events].
type Client struct {
	cfg        *Config
	id         *crypto.Identity
	store      *clientStore
	log        *slog.Logger
	passphrase string
	tlsConfig  *tls.Config

	events chan Event

	mu           sync.RWMutex
	state        ConnState
	admin        bool
	conn         *wsConn
	desired      proto.PresenceSet
	peerPresence map[string]proto.PresenceUpdate

	pendingMu sync.Mutex
	pending   map[string]chan *proto.Envelope

	transfersMu sync.Mutex
	transfers   map[string]*inboundTransfer

	runCancel context.CancelFunc
	wg        sync.WaitGroup
}

// New loads (or creates) the device identity and local database for the client
// configured in cfg.
func New(cfg *Config, logger *slog.Logger) (*Client, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.Dir() == "" {
		return nil, errors.New("clientcore: config has no directory")
	}
	id, _, err := crypto.LoadOrCreateIdentity(cfg.IdentityPath())
	if err != nil {
		return nil, err
	}
	st, err := openClientStore(cfg.DBPath())
	if err != nil {
		return nil, err
	}
	secret, err := loadSecret(cfg.Dir())
	if err != nil {
		st.Close()
		return nil, err
	}
	return &Client{
		cfg:          cfg,
		id:           id,
		store:        st,
		log:          logger,
		passphrase:   secret,
		tlsConfig:    pinnedTLSConfig(cfg.CertFingerprint),
		events:       make(chan Event, 256),
		state:        StateDisconnected,
		desired:      proto.PresenceSet{Status: proto.StatusAvailable},
		peerPresence: map[string]proto.PresenceUpdate{},
		pending:      map[string]chan *proto.Envelope{},
		transfers:    map[string]*inboundTransfer{},
	}, nil
}

// Close releases the local database. Call after [Client.Stop].
func (c *Client) Close() error { return c.store.Close() }

// Events is the notification stream for the UI. It is buffered; slow consumers
// miss transient events but can always re-read state from the accessors.
func (c *Client) Events() <-chan Event { return c.events }

// DeviceID returns this device's relay-assigned ID (empty until enrolled).
func (c *Client) DeviceID() string { return c.cfg.DeviceID }

// State returns the current connection state.
func (c *Client) State() ConnState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state
}

// IsAdmin reports whether this device may approve/deny pending devices.
func (c *Client) IsAdmin() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.admin
}

// SetPassphrase sets (and persists) the household passphrase used for the relay
// handshake.
func (c *Client) SetPassphrase(p string) error {
	c.mu.Lock()
	c.passphrase = p
	c.mu.Unlock()
	return saveSecret(c.cfg.Dir(), p)
}

// HasPassphrase reports whether a passphrase is available for connecting.
func (c *Client) HasPassphrase() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.passphrase != ""
}

// --- enrollment ------------------------------------------------------

// Enroll performs first-time registration with the relay: it connects, proves
// the passphrase, registers this device's keys under displayName, persists the
// assigned device ID, and disconnects. The returned state is StateReady when the
// device is active immediately or StatePendingApproval when an admin must
// approve it first. Call [Client.Start] afterwards to run the connection.
func (c *Client) Enroll(ctx context.Context, displayName, passphrase string) (ConnState, error) {
	if c.cfg.ServerAddr == "" || c.cfg.CertFingerprint == "" {
		return StateDisconnected, ErrNotConfigured
	}
	if passphrase == "" {
		return StateDisconnected, ErrNoPassphrase
	}
	c.mu.Lock()
	c.passphrase = passphrase
	c.mu.Unlock()

	w, err := c.dial(ctx)
	if err != nil {
		return StateDisconnected, err
	}
	defer w.close("enrollment complete")

	res, err := c.handshake(ctx, w, &proto.Enroll{
		DisplayName: displayName,
		SignPub:     crypto.EncodeSignPub(c.id.SignPub),
		BoxPub:      crypto.EncodeKey(c.id.BoxPub),
	})
	if err != nil {
		return StateDisconnected, err
	}

	c.cfg.DisplayName = displayName
	if err := c.cfg.Save(); err != nil {
		return res.state, err
	}
	if err := saveSecret(c.cfg.Dir(), passphrase); err != nil {
		return res.state, err
	}
	return res.state, nil
}

// applyEnrollResult persists a freshly assigned device ID.
func (c *Client) applyEnrollResult(er proto.EnrollResult) {
	if er.DeviceID != "" && c.cfg.DeviceID != er.DeviceID {
		c.cfg.DeviceID = er.DeviceID
		_ = c.cfg.Save()
	}
}

// --- run loop ------------------------------------------------------

// Start begins connecting to the relay and reconnecting on failure until
// [Client.Stop] or ctx is cancelled. It returns immediately.
func (c *Client) Start(ctx context.Context) error {
	if !c.cfg.Configured() {
		return ErrNotConfigured
	}
	if !c.cfg.Enrolled() {
		return ErrNotEnrolled
	}
	if !c.HasPassphrase() {
		return ErrNoPassphrase
	}

	runCtx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	c.runCancel = cancel
	c.mu.Unlock()

	c.wg.Add(1)
	go c.runLoop(runCtx)
	return nil
}

// Stop ends the connection loop and waits for it to finish.
func (c *Client) Stop() {
	c.mu.Lock()
	cancel := c.runCancel
	c.runCancel = nil
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	c.wg.Wait()
}

func (c *Client) runLoop(ctx context.Context) {
	defer c.wg.Done()
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		if ctx.Err() != nil {
			return
		}
		err := c.connectAndServe(ctx)
		c.setConn(nil)
		c.emitConnState(StateDisconnected)

		if ctx.Err() != nil {
			return
		}
		if err != nil {
			var re *RelayError
			if errors.As(err, &re) && (re.Code == proto.ErrAuthFailed || re.Code == proto.ErrForbidden) {
				c.emitError(fmt.Errorf("relay rejected this device: %w", err))
				return // no point retrying with the same credentials
			}
			c.log.Debug("relay connection ended", "err", err)
		}

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

// connectAndServe dials, handshakes, and then reads frames until the connection
// drops. It returns the error that ended the connection (nil on a clean stop).
func (c *Client) connectAndServe(ctx context.Context) error {
	c.emitConnState(StateConnecting)

	w, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer w.close("client stopping")

	res, err := c.handshake(ctx, w, nil)
	if err != nil {
		return err
	}

	c.setConn(w)
	c.mu.Lock()
	c.admin = res.admin
	desired := c.desired
	c.mu.Unlock()

	c.emitConnState(res.state)
	if res.state != StateReady {
		// Pending approval: keep the connection open and wait. The relay sends a
		// ready frame if/when an admin approves.
		return c.readLoop(ctx, w)
	}

	// Ready: publish our desired presence, flush anything composed offline, serve.
	if err := w.send(ctx, proto.TypePresenceSet, "", desired); err != nil {
		return err
	}
	c.flushOutbox(ctx, w)
	return c.readLoop(ctx, w)
}

func (c *Client) readLoop(ctx context.Context, w *wsConn) error {
	for {
		env, err := w.read(ctx)
		if err != nil {
			return err
		}
		c.dispatch(ctx, w, env)
	}
}

func (c *Client) setConn(w *wsConn) {
	c.mu.Lock()
	c.conn = w
	c.mu.Unlock()
}

func (c *Client) currentConn() *wsConn {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.conn
}
