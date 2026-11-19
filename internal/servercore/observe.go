package servercore

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"lanmessenger/internal/proto"
)

// listenerKindKey carries which listener accepted a connection ("tunnel" for
// the Tor tunnel session, "" for the direct socket).
type listenerKindKey struct{}

// pathLabel says how a connection reached the relay. Tunnelled clients all
// appear to come from loopback, so the listener — not the address — is what
// identifies them. The relay cannot tell an SSH forward from a Tor circuit when
// both enter through the same tunnel; round-trip time is the only hint.
func pathLabel(r *http.Request) string {
	if k, _ := r.Context().Value(listenerKindKey{}).(string); k != "" {
		return k
	}
	ip := net.ParseIP(hostOnly(r.RemoteAddr))
	if ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()) {
		return "lan"
	}
	return "direct"
}

// clientHelloHash is a JA3-style fingerprint: a short hash of what the client
// offered in its TLS handshake (versions, cipher suites, curves, point formats,
// signature schemes, ALPN). Go does not expose the raw extension list, so this
// is "JA3-lite": stable per TLS stack and build, not comparable to public
// JA3 databases. GREASE values (random placeholders) are skipped.
func clientHelloHash(h *tls.ClientHelloInfo) string {
	var b strings.Builder
	list := func(vals []uint16) {
		out := make([]string, 0, len(vals))
		for _, v := range vals {
			if isGrease(v) {
				continue
			}
			out = append(out, fmt.Sprint(v))
		}
		b.WriteString(strings.Join(out, "-"))
		b.WriteByte(',')
	}
	list(h.SupportedVersions)
	list(h.CipherSuites)
	curves := make([]uint16, len(h.SupportedCurves))
	for i, c := range h.SupportedCurves {
		curves[i] = uint16(c)
	}
	list(curves)
	pts := make([]string, len(h.SupportedPoints))
	for i, p := range h.SupportedPoints {
		pts[i] = fmt.Sprint(p)
	}
	b.WriteString(strings.Join(pts, "-") + ",")
	sch := make([]uint16, len(h.SignatureSchemes))
	for i, s := range h.SignatureSchemes {
		sch[i] = uint16(s)
	}
	list(sch)
	b.WriteString(strings.Join(h.SupportedProtos, "-"))
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

func isGrease(v uint16) bool { return v&0x0f0f == 0x0a0a && v>>8 == v&0xff }

// tlsHashTable hands the ClientHello hash from the TLS layer (which sees the
// handshake) to the WebSocket handler (which sees the HTTP request), keyed by
// the connection's remote address.
type tlsHashTable struct {
	mu sync.Mutex
	m  map[string]tlsHashEntry
}

type tlsHashEntry struct {
	hash string
	at   time.Time
}

func (t *tlsHashTable) put(addr, hash string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = make(map[string]tlsHashEntry)
	}
	if len(t.m) > 1024 { // connections that never became a WebSocket (healthz, updates)
		cut := time.Now().Add(-time.Minute)
		for k, e := range t.m {
			if e.at.Before(cut) {
				delete(t.m, k)
			}
		}
	}
	t.m[addr] = tlsHashEntry{hash, time.Now()}
}

func (t *tlsHashTable) take(addr string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.m[addr]
	delete(t.m, addr)
	return e.hash
}

// rttStats accumulates heartbeat round-trip times.
type rttStats struct {
	mu            sync.Mutex
	n             int
	sum, min, max time.Duration
}

func (r *rttStats) add(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n == 0 || d < r.min {
		r.min = d
	}
	if d > r.max {
		r.max = d
	}
	r.n++
	r.sum += d
}

// snapshot returns avg/min/max in whole milliseconds (zeros without samples).
func (r *rttStats) snapshot() (avg, min, max int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n == 0 {
		return 0, 0, 0
	}
	return int((r.sum / time.Duration(r.n)).Milliseconds()),
		int(r.min.Milliseconds()), int(r.max.Milliseconds())
}

// openConnLog records the start of a connection for an identified device.
func (s *Server) openConnLog(c *conn) {
	if c.deviceID == "" {
		return
	}
	id, err := s.store.openConnRecord(c.deviceID, proto.ConnRecord{
		ConnectedAt:   time.Now().Unix(),
		Path:          c.path,
		RemoteHost:    c.remoteHost,
		TLSHash:       c.tlsHash,
		ClientVersion: c.clientVersion,
		ClientKind:    c.clientKind,
		ClientOS:      c.clientOS,
		ClientArch:    c.clientArch,
	})
	if err != nil {
		s.log.Warn("connection log failed", "device", c.deviceID, "err", err)
		return
	}
	c.connRecID = id
}

func (s *Server) closeConnLog(c *conn) {
	if c.connRecID == 0 {
		return
	}
	avg, min, max := c.rtt.snapshot()
	if err := s.store.closeConnRecord(c.connRecID, time.Now().Unix(), avg, min, max); err != nil {
		s.log.Warn("connection log close failed", "device", c.deviceID, "err", err)
	}
}

// adminReplayCache remembers recently used admin nonces so a captured admin
// frame cannot be replayed inside the freshness window.
type adminReplayCache struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// firstUse reports whether nonce is new, and remembers it.
func (a *adminReplayCache) firstUse(nonce string, now time.Time, window time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.seen == nil {
		a.seen = make(map[string]time.Time)
	}
	for k, t := range a.seen {
		if now.Sub(t) > 2*window {
			delete(a.seen, k)
		}
	}
	if _, dup := a.seen[nonce]; dup {
		return false
	}
	a.seen[nonce] = now
	return true
}
