# Dev Log

This is a running notebook, not a changelog. `CHANGELOG.md` stays the terse
"what shipped" list; this is where the *why* behind a design lives — the
reasoning, the dead ends, the small pieces of code that are actually worth
looking at rather than just describing. Started 2026-09-19.

## 2026-09-20 (live deployment) — ready-before-roster race in both send commands

Second real bug caught by actually running this over a live Tor link
rather than loopback tests: `lanmsg-remote-cli send -to "Jessie" ...`
failed with `no peer matching "Jessie"` right after a *successful*
enrollment that had just put "Jessie" in the roster moments earlier — the
device unquestionably existed.

Traced it to `internal/clientcore/dispatch.go`: `ready` and
`directory_snapshot` are two separate frames. `handleReady` (dispatch.go:39-51)
flips state to `StateReady` the instant the `ready` frame arrives;
`handleSnapshot` populates the actual roster only once the *next* frame is
processed. Both `cmd/lanmsg-cli` and `cmd/lanmsg-remote-cli`'s `send`
commands did `waitReady(...)` then an immediate, one-shot `resolvePeer(...)`
— correct only if the snapshot always arrives before (or immediately with)
`ready`, which the protocol never actually guarantees. On a LAN this race
is narrow enough to almost never lose; over Tor, where latency is higher
and far more variable, it's easy to lose.

This is the same shape of bug the three-role integration test
(`internal/servercore/tunnel_integration_test.go`) had already caught and
fixed with `waitSeesPeer` — but that fix lived only in the test harness,
never in the actual shipped `cmd/` tools that have the identical pattern.
Worth remembering: a test catching a race in its *own* helper code doesn't
mean the same race isn't still live in the product.

Fixed by giving both `send` commands a `waitForPeer` retry loop (a few
seconds, short poll interval) instead of a single immediate lookup.

## 2026-09-20 (live deployment) — A shared config directory bit us on the first real run

First live deployment of the cloud tunnel (real AWS box, real Pi, real Tor)
went smoothly right up through the Pi authenticating to the cloud tunnel
and the fingerprint matching on a remote enrollment attempt — then the
*desktop GUI client*, on the same home machine used to test
`lanmsg-remote-cli`, started failing to reach the relay over the LAN with:

```
Clientcore: connect 192.168.1.118:8443: failed to WebSocket dial: ...
socks connect tcp 127.0.0.1:9050->192.168.1.118:8443: unknown error general SOCKS server failure
```

Cause, once traced through: `cmd/lanmsg-remote-cli` defaulted to
`clientcore.DefaultDir()` — the *exact same* directory `cmd/lanmsg` (the
GUI) and `cmd/lanmsg-cli` already use. Running `lanmsg-remote-cli enroll`
without an explicit `-config` (exactly what the SETUP.md walkthrough's
example command does) silently loaded and overwrote the GUI's own
`config.json` in place, setting `socks_proxy` on it. The GUI then tried to
reach its perfectly normal LAN address *through Tor*, which correctly
refuses to proxy to a private-use address.

This was a real gap in the plan and the SETUP.md walkthrough alike:
"reuse `internal/clientcore`" (true, and still the right call — no crypto
or protocol code was duplicated) quietly implied "reuse its default
directory too," which was never actually the right call for a *second,
independent* client identity that happens to run on the same machine as
a LAN one. Fixed by giving `lanmsg-remote-cli` its own sibling default
directory (`lanmessenger-remote`) — a one-line-of-intent fix
(`cmd/lanmsg-remote-cli/main.go`'s `defaultRemoteDir`), but the kind of
thing that only shows up by actually running the thing end to end on real
hardware, not in any of the loopback tests.

## 2026-09-19/20 — A real bug: TLS over a yamux stream, poisoned by net/http's own optimization

Found while writing the three-role loopback integration test
(`internal/servercore/tunnel_integration_test.go`) — the one that proves a
message crosses the tunnel end to end using the real relay, the real
`tunnel.Hub`, and a real, unmodified `clientcore.Client`. It failed on the
very first real exercise of the full path (TLS + WebSocket handshake over
a tunnel-arriving connection), with a strange, intermittent error: the
relay's handshake read would fail with `i/o deadline reached` — yamux's
own timeout error — despite no timeout anywhere in the code that should
have fired yet.

**Why it was hard to pin down.** It didn't reproduce in a direct,
in-process hand-off of a yamux stream to `net/http`'s `ServeTLS`, nor
without TLS, nor with a fake backend that just echoes bytes with no HTTP
involved at all — every one of `internal/tunnel`'s own unit tests passed
every time. It only showed up once a *pump* was in the path — the `Hub`
copying bytes between a real external TCP connection and the yamux data
stream, via `io.Copy` — the exact shape of the real production request
path, just never exercised end-to-end until this test.

**Root cause**, isolated with a series of shrinking standalone repros
(and finally a debug-instrumented local copy of `yamux` to watch exactly
which stream got which deadline, when): `net/http` runs an internal
optimization for every request with no body (which a WebSocket upgrade
GET always is) — it starts a background 1-byte "peek" read on the raw
connection immediately after headers are parsed, so it can detect the
client disconnecting early while the handler is still running. The moment
the handler hijacks the connection (which `websocket.Accept` does
internally), `net/http` aborts that peek read by setting its deadline to
a sentinel time in the past, waits for it to unblock, then restores no
deadline. Over a real OS TCP socket this abort is atomic at the syscall
level: the interrupted read either got real bytes or nothing, never
"some." Over a yamux stream — a pure-Go, channel-based implementation —
the same abort can land in the middle of `crypto/tls` assembling one TLS
record across more than one underlying `Read` call, and `crypto/tls`
treats *any* read error mid-record as permanently fatal to the
connection — poisoning every later `Read` with that same spurious
timeout, even though `net/http` correctly cleared its own deadline
immediately after. A direct hand-off (no pump) tends to deliver a whole
record in one `Read` call, which never hits this window; a pumped
connection, with genuine network-style chunking, hits it often enough to
fail reliably.

**Fix**: `internal/tunnel/bufferedconn.go` — a small wrapper
(`bufferedConn`) placed around every data stream before `SessionListener`
hands it to `net/http`. A background goroutine drains the raw yamux
stream continuously with **no deadline ever applied to it** by anyone
else, handing off whatever it reads as atomic, already-fully-retrieved
chunks over a channel. The conn `net/http`/`crypto/tls` actually see
implements `Read`/`SetReadDeadline` purely in terms of that channel and a
timer — a timeout can only ever fire *before* any bytes are handed over,
never mid-chunk, so `crypto/tls` can never observe a partially-delivered
record. Verified with the same standalone repro run 15 times clean
(previously failed reliably), then the real integration test run 20
times under `-race`, and the whole suite run 3 times under `-race` — all
clean.

Two smaller, genuinely separate bugs turned up alongside it, both fixed
in the same pass: rate limiting had been keyed by the full `ip:port`
remote address rather than just the IP, which defeats per-source limiting
entirely since every new connection gets a fresh ephemeral port (fixed
with a `hostOnly` helper in both `servercore` and `tunnel`); and a
pre-existing race in `internal/tunnel`'s own `hub_test.go`, where a test's
fake backend completing its handshake didn't guarantee the `Hub`'s
*own* post-handshake bookkeeping (opening the control stream, storing the
link) had finished before the test dialed the public listener — reliably
reproduced under `go test -count=10`, fixed by waiting on
`Hub.HasBackend()` before proceeding, mirroring what the servercore test
already did correctly.

## 2026-09-19 (later) — Correction found while building: fingerprint probe needs SOCKS5 too

The approved plan claimed the client's first-run certificate-probe helper
(`FingerprintOfPresentedCert`) "works unmodified" through the tunnel. That
was true back when the tunnel design was a plain public IP:port — but the
plan later pivoted to Tor, and nobody went back to re-check that claim
against the new design. It doesn't hold: the probe dials with a raw
`tls.Dialer`, which resolves hostnames via normal DNS, and no DNS server on
earth knows what a `.onion` address is — only Tor does. First-time
enrollment against the cloud tunnel would have failed outright unless the
fingerprint was always supplied manually with `-fingerprint`.

Fixed with a small addition rather than a workaround: `internal/clientcore`
gained `FingerprintOfPresentedCertVia(ctx, serverAddr, socksProxy)`, sharing
a new `dialSOCKS5` helper with the SOCKS5 dial path already added to the
main connection code. Same probe, same fingerprint check, just dialed
through Tor when a SOCKS proxy is configured. `lanmsg-remote-cli`'s
`enroll` command uses it unconditionally.

## 2026-09-19 (later) — Plan amendment: Android via Termux

Added to the cloud-tunnel plan after realizing it's nearly free: the whole
dependency chain the plan already depends on is pure Go —
`internal/store` uses `modernc.org/sqlite`, not a cgo driver, and
`yamux`/`golang.org/x/net/proxy` are pure Go too — so
`lanmsg-remote-cli` cross-compiles for `GOOS=android GOARCH=arm64` with
`CGO_ENABLED=0` and just runs under Termux (the no-root terminal-emulator
app that gives Android a real Linux userland and its own package
manager). Termux's repo already carries `tor`, so there's no Tor-bundling
work either — just a `pkg install tor` line in the docs, identical in
spirit to the `apt install tor` step already planned for the Pi and
desktop remote clients. One real tradeoff: Termux only reliably keeps
foreground processes alive, so a freshly started Tor daemon means a
several-second circuit-bootstrap wait before the first send of a Termux
session.

## 2026-09-19 — Approved plan: reaching the relay from outside the home LAN (Tor-tunneled cloud relay)

Approved through plan mode; archiving its plan text per the standing
practice (see `reference_devlog_convention.md`).

> # Reaching the LANmessenger relay from outside the home LAN (Tor-tunneled cloud relay)
>
> ## Context
>
> LANmessenger's relay (`cmd/lanmsg-server`, logic in `internal/servercore`) only
> works when a client can route to it directly — it was built for a home LAN
> where the Pi has no public IP and no port-forwarding (`docs/NETWORK.md`).
> Family members away from the house currently have no way to reach it.
>
> We (the user and I) designed the fix together in conversation. The
> requirements that came out of that discussion, in the order they were
> locked in:
>
> 1. **No inbound port anywhere** — not on the home router, and (once we got
>    to discussing Tor) not on the cloud box either. The Pi only ever *dials
>    out*, and so does every remote client — nothing accepts unsolicited
>    inbound connections except Tor's own protocol, which needs no firewall
>    hole to work.
> 2. **The cloud box stays "dumb" and stateless.** It never terminates the
>    app's TLS, never decrypts anything, never stores a byte at rest, and
>    holds no application secrets — only a narrow shared secret proving "this
>    connection may call itself the Pi."
> 3. **No presence/status forwarding** (idle/busy/away) for the outside path —
>    left out entirely; not a gap, just an omission, since presence is just
>    another message type a client can choose not to send.
> 4. **The outside participant is a new, minimal, one-shot CLI** — not the
>    full GUI, not even the full existing `lanmsg-cli` UX — point it at a key
>    location and a message, it sends and exits.
> 5. **Reachability via a Tor (.onion) hidden service**, not a plain public
>    IP:port. This was the last decision, made once the user learned that a
>    `.onion` address requires *zero* inbound firewall rules on the cloud box
>    either — strictly stronger than the original "AWS has one open port"
>    design, and consistent with locked decision #1.
>
> Two invariants from `docs/DESIGN.md` must not be violated:
> - §1: *"TLS protects the pipe; E2E protects the payload even from whoever
>   runs the pipe."* The cloud box sits inside the "pipe" layer only, for app
>   traffic — it never sees anything the existing relay-to-client TLS didn't
>   already expose to a LAN-based relay.
> - §12.2: *"The relay is distribution, not authority."* The cloud box has
>   even less authority than the relay: it can't approve devices, read the
>   roster, or read a message. Its only privilege is "is this TCP connection
>   allowed to call itself the backend."
>
> ---
>
> ## Architecture
>
> ```
>  remote family member         AWS box (Tor hidden service)          home Pi
>  ─────────────────────         ─────────────────────────           ────────
>  lanmsg-remote-cli                                                  lanmsg-server
>  (clientcore + SOCKS5)                                              (unmodified relay
>         │                                                            logic; gains an
>         │ dial <onion>.onion:8443                                   optional [tunnel]
>         │ via local Tor SOCKS proxy                                 dial-out block)
>         ▼
>    [ Tor network ]  ──rendezvous, no inbound port on AWS──▶  Tor daemon on AWS
>                                                               (torrc: HiddenServiceDir +
>                                                                two HiddenServicePort lines)
>                                                                      │
>                                                      forwards to loopback only:
>                                                      virtual :8443 → 127.0.0.1:8443 (public)
>                                                      virtual :9443 → 127.0.0.1:9443 (backend)
>                                                                      │
>                                                           ┌──────────┴──────────┐
>                                                           │   cmd/lanmsg-tunnel  │
>                                                           │  (new, stateless,   │
>                                                           │   loopback-only)    │
>                                                           └──────────┬──────────┘
>                                                                      │ yamux stream per
>                                                                      │ remote client,
>                                                                      │ raw byte pump
>                                                                      ▼
>                                                           persistent connection ◀── Pi dials OUT
>                                                           (Argon2id+HMAC auth,      via its own
>                                                            no extra TLS — Tor       local Tor SOCKS
>                                                            already authenticates    proxy, to
>                                                            the .onion endpoint)     <onion>.onion:9443
> ```
>
> Both of `cmd/lanmsg-tunnel`'s listeners are **loopback-only plain TCP**
> (`127.0.0.1:8443`, `127.0.0.1:9443`). Tor forwards two virtual ports on one
> `.onion` identity down to those loopback ports. The app code never binds a
> public interface and never needs to know Tor exists — Tor is a separate
> system daemon in front of it. The AWS security group can stay closed to all
> inbound traffic (aside from SSH for admin, out of scope here).
>
> Because a `.onion` address is a cryptographic proof of identity (derived
> from the service's own keypair), the Pi↔cloud backend leg does **not** get
> its own TLS+cert-pinning layer — that would be redundant. It keeps the
> Argon2id+HMAC challenge/response (the same primitive as the household
> passphrase) to prove "this connection is really the Pi," run directly over
> the Tor-provided connection.
>
> The public leg (remote client → cloud box) is unchanged in spirit from a
> plain-IP design: the cloud box never touches the client's actual app-layer
> TLS (`wss://` to the Pi's own fingerprint-pinned cert) — it's a blind byte
> pump, now reached via Tor instead of a direct socket.
>
> ---
>
> ## Package layout: new and changed files
>
> ### New package: `internal/ratelimit`
> Minimal in-memory sliding-window limiter, no disk, no external deps:
> ```go
> type Limiter struct { max int; window time.Duration; mu sync.Mutex; hits map[string][]time.Time }
> func New(max int, window time.Duration) *Limiter
> func (l *Limiter) Allow(key string) bool
> func (l *Limiter) GC(now time.Time)
> ```
> Used in `servercore` (connection-attempt limiter keyed by remote IP, frame-rate
> limiter keyed by device ID — this is the "generic per-sender frame rate
> limit... N msg frames per 10s, set in server.toml" already sketched as
> backlog in `docs/DESIGN.md:910-912`, built now instead of deferred) and once
> in `lanmsg-tunnel` (connection-attempt limiter at the public listener).
>
> ### New package: `internal/tunnel` (shared by `servercore` and `cmd/lanmsg-tunnel`)
> - **`frame.go`** — tiny length-prefixed JSON framing over a plain `net.Conn`
>   (modeled on the existing frame-reading helper in
>   `internal/servercore/conn.go`), used only for the pre-multiplexing
>   handshake.
> - **`protocol.go`** — handshake message shapes:
>   ```go
>   type Hello struct { Version int; Role string } // Role must be "backend"
>   type Challenge struct { Nonce, Salt string }   // base64
>   type Response struct { Proof string }          // base64 HMAC
>   type Result struct { OK bool; Message string }
>   type StreamPreamble struct { RemoteAddr string; OpenedAtUnixMs int64 }
>   ```
>   `StreamPreamble` is written as the first frame of every yamux stream the
>   cloud box opens toward the Pi (one per accepted remote client), carrying
>   the real external IP — trustworthy specifically because it can only ever
>   arrive inside a stream on an already-authenticated backend session.
> - **`auth.go`** — the handshake, reusing the household-passphrase crypto
>   verbatim (no new crypto primitive anywhere in this feature):
>   - `AuthenticateAsBackend(ctx, conn net.Conn, secret string) (*yamux.Session, error)`
>     — Pi's side: `Hello` → read `Challenge` → `crypto.DeriveKey` +
>     `crypto.Proof` → send `Response` → read `Result` → `yamux.Client(conn, cfg)`.
>   - `AuthenticateBackend(ctx, conn net.Conn, verifier crypto.PassphraseVerifier) (*yamux.Session, error)`
>     — cloud box's side: read `Hello` (reject if `Role != "backend"`) →
>     `crypto.NewChallengeNonce` → send `Challenge` → read `Response` →
>     `verifier.CheckResponse` → send `Result` → `yamux.Server(conn, cfg)`.
>   - Reused functions, confirmed present: `crypto.DeriveKey`,
>     `crypto.NewChallengeNonce`, `crypto.Proof`,
>     `crypto.PassphraseVerifier.CheckResponse`, `crypto.NewPassphraseVerifier`
>     (all in `internal/crypto/passphrase.go`).
> - **`listener.go`** — `SessionListener` implements `net.Listener` over a
>   `*yamux.Session`: `Accept()` calls `sess.Accept()`, reads the
>   `StreamPreamble` off the top (short read deadline), and returns a
>   `net.Conn` wrapper whose `RemoteAddr()` reports the parsed real IP. This is
>   the seam that lets tunnel-arriving connections flow into the **unmodified**
>   `servercore` HTTP/WebSocket stack.
> - **`hub.go`** — the cloud-side runtime, exported and independently testable
>   (mirrors how `cmd/lanmsg-server` is a thin wrapper around
>   `servercore.Server`):
>   ```go
>   type Hub struct { verifier crypto.PassphraseVerifier; limiter *ratelimit.Limiter; log *slog.Logger; mu sync.Mutex; sess *yamux.Session }
>   func (h *Hub) AcceptBackends(ctx context.Context, ln net.Listener)
>   func (h *Hub) AcceptPublic(ctx context.Context, ln net.Listener)
>   ```
>   `AcceptBackends`: accept raw conn → `AuthenticateBackend` → on success,
>   lock, close any previous session, install the new one. `AcceptPublic`:
>   accept raw conn → rate-limit by `RemoteAddr()` → if no current session,
>   close immediately (no buffering, no queue — matches "cloud box stays
>   stateless") → else `sess.Open()`, write `StreamPreamble`, pump bytes both
>   ways until either side closes.
>
> ### Changed: `internal/servercore`
> - **`config.go`** — add `Tunnel *TunnelConfig` (nil by default — fully
>   backward compatible with every existing `server.toml`):
>   ```go
>   type TunnelConfig struct {
>       CloudOnionAddr string `toml:"cloud_onion_addr"` // e.g. "abcd...xyz.onion:9443"
>       SOCKSProxy     string `toml:"socks_proxy"`      // default "127.0.0.1:9050"
>       Secret         string `toml:"secret"`           // shared secret, NOT the household passphrase
>   }
>   ```
>   Plus `RateLimit RateLimitConfig` with defaults applied in `applyDefaults()`.
> - **`server.go`** — the one genuinely tricky structural change. Today
>   `serve()` does three things in one function: `defer s.store.Close()`,
>   `go s.purgeLoop(ctx)`, and the `httpSrv.ServeTLS` loop. Calling `serve()` a
>   second time concurrently for the tunnel listener (as the naive "just reuse
>   RunListener" idea would) **double-closes the shared store** the first time
>   either listener returns — and the tunnel listener *will* return
>   repeatedly, every time the cloud connection drops and retries. Verified
>   directly in `internal/servercore/server.go:97-134`: `defer s.store.Close()`
>   and `go s.purgeLoop(ctx)` both live inside `serve()`. Fix — split into:
>   ```go
>   func (s *Server) serve(ctx context.Context, ln net.Listener) error {
>       defer s.store.Close()
>       go s.purgeLoop(ctx)
>       if s.cfg.Tunnel != nil {
>           go s.runTunnel(ctx)
>       }
>       return s.serveHTTP(ctx, ln)
>   }
>   // serveHTTP is today's serve() body minus the store-close/purgeLoop lines.
>   // Called once for the LAN listener (from serve) and once per tunnel
>   // session (from runTunnel).
>   func (s *Server) serveHTTP(ctx context.Context, ln net.Listener) error { ... }
>   ```
>   `RunListener` is unchanged. Add the dial-out loop, modeled directly on
>   `clientcore`'s existing reconnect loop (`internal/clientcore/client.go:220-254`,
>   1s→30s exponential backoff — same shape, reused pattern not reinvented):
>   ```go
>   func (s *Server) runTunnel(ctx context.Context) {
>       backoff := time.Second
>       for {
>           if ctx.Err() != nil { return }
>           sess, err := s.dialTunnel(ctx)
>           if err == nil {
>               backoff = time.Second
>               ln := tunnel.NewSessionListener(sess, s.log)
>               err = s.serveHTTP(ctx, ln)
>               _ = sess.Close()
>               if ctx.Err() != nil { return }
>           }
>           s.log.Warn("tunnel session ended, retrying", "err", err)
>           select {
>           case <-ctx.Done(): return
>           case <-time.After(backoff):
>           }
>           if backoff < 30*time.Second { backoff *= 2 }
>       }
>   }
>
>   func (s *Server) dialTunnel(ctx context.Context) (*yamux.Session, error) {
>       dialer, err := proxy.SOCKS5("tcp", s.cfg.Tunnel.SOCKSProxy, nil, proxy.Direct)
>       if err != nil { return nil, err }
>       raw, err := dialer.Dial("tcp", s.cfg.Tunnel.CloudOnionAddr)
>       if err != nil { return nil, err }
>       sess, err := tunnel.AuthenticateAsBackend(ctx, raw, s.cfg.Tunnel.Secret)
>       if err != nil { raw.Close(); return nil, err }
>       return sess, nil
>   }
>   ```
>   (`golang.org/x/net/proxy` provides `proxy.SOCKS5`, used to reach the
>   `.onion` address through the Pi's local Tor daemon.)
> - **`ratelimit.go`** (new, thin glue): connection-attempt limiter applied in
>   `handleWS` before the WebSocket upgrade (keyed by `r.RemoteAddr`, which is
>   accurate for both LAN and tunnel-arriving connections since
>   `SessionListener` already corrects it via the preamble); frame-rate
>   limiter applied once per inbound frame in the read loop, keyed by device
>   ID.
>
> ### Changed: `internal/clientcore`
> - **`config.go`** — add one optional field: `SOCKSProxy string` (empty =
>   today's behavior, unchanged). Existing GUI and `lanmsg-cli` never set it —
>   zero behavior change for LAN users.
> - **`conn.go`** (`dial`) — when `cfg.SOCKSProxy != ""`, build the
>   `http.Client.Transport`'s `DialContext` through
>   `proxy.SOCKS5("tcp", cfg.SOCKSProxy, nil, proxy.Direct)` instead of the
>   default dialer; everything else (TLS pinning, handshake, enrollment,
>   reconnect loop) is untouched and shared.
>
> ### New binary: `cmd/lanmsg-tunnel` (the AWS-side component)
> Mirrors `cmd/lanmsg-server/main.go`'s `setup` / `run` / `fingerprint`
> subcommand structure:
> ```
> lanmsg-tunnel setup [-config path] [-secret ...]
> lanmsg-tunnel run   [-config path]
> ```
> No `fingerprint` subcommand needed (no TLS cert on this leg anymore — see
> architecture section). `run` loads config, builds a `tunnel.Hub`, opens two
> **loopback-only** listeners (`net.Listen("tcp", "127.0.0.1:8443")` and
> `"127.0.0.1:9443"`), calls `hub.AcceptPublic` / `hub.AcceptBackends` in
> goroutines, blocks on `signal.NotifyContext`.
>
> Config (`lanmsg-tunnel.toml`, minimal — no `data_dir`, no DB path, the box is
> stateless):
> ```toml
> public_listen_addr  = "127.0.0.1:8443"
> backend_listen_addr = "127.0.0.1:9443"
>
> [backend_secret]      # crypto.PassphraseVerifier — generated by `setup`, never the raw secret
> salt = "..."
> key  = "..."
>
> [rate_limit]
> max_connects_per_window = 20
> connect_window_seconds  = 60
> ```
>
> ### New minimal binary: `cmd/lanmsg-remote-cli`
> The "point it at a key location and a message" tool. Thin wrapper reusing
> `internal/clientcore` end to end (enrollment, `SendText`, crypto — nothing
> new): loads a config/identity directory given as a flag, sets
> `SOCKSProxy` (default `127.0.0.1:9050`), connects, sends one message,
> waits for ack, exits. No roster browsing, no presence, no daemon.
>
> ---
>
> ## Config schema additions
>
> `server.toml`:
> ```toml
> [tunnel]                                  # optional; absent = feature disabled
> cloud_onion_addr = "abcd...xyz.onion:9443"
> socks_proxy      = "127.0.0.1:9050"       # the Pi's local Tor SOCKS proxy
> secret           = "long random shared secret, NOT the household passphrase"
>
> [rate_limit]
> max_frames_per_window   = 20
> frame_window_seconds    = 10
> max_connects_per_window = 10
> connect_window_seconds  = 60
> ```
>
> Remote client's `config.json` (`internal/clientcore`): identical to any LAN
> client's, plus `server_addr` = the cloud box's `.onion` public address and
> `socks_proxy` = wherever Tor's SOCKS proxy runs on that machine. Same
> `cert_fingerprint` every LAN client already has — TLS still terminates only
> at the Pi.
>
> ---
>
> ## Deployment walkthrough (AWS box)
>
> 1. Provision a small always-on VM (e.g. AWS EC2 `t4g.nano` or similar).
>    **Security group: no inbound rules at all** (or only SSH from your own
>    admin IP — outside this feature's scope).
> 2. Install Tor: `apt install tor` (Debian/Ubuntu).
> 3. Edit `/etc/tor/torrc`, add:
>    ```
>    HiddenServiceDir /var/lib/tor/lanmsg_tunnel/
>    HiddenServicePort 8443 127.0.0.1:8443
>    HiddenServicePort 9443 127.0.0.1:9443
>    ```
> 4. `systemctl restart tor`. After it starts, read the generated address:
>    `cat /var/lib/tor/lanmsg_tunnel/hostname` → this is the `.onion` address,
>    stable across restarts as long as `HiddenServiceDir` isn't deleted (the
>    private key lives there — back it up).
> 5. Build/copy `lanmsg-tunnel` to the box (same cross-compile pattern already
>    documented for `lanmsg-server`). Run `lanmsg-tunnel setup -secret ...` to
>    generate `lanmsg-tunnel.toml`. Run it under a new `deploy/lanmsg-tunnel.service`
>    systemd unit (tighter than `lanmsg-server.service` — no database, so
>    `ReadWritePaths` covers only its own config directory).
> 6. On the Pi: `apt install tor` (client-only use, no `HiddenServiceDir`
>    needed there), add the `[tunnel]` block to `server.toml` with the
>    `.onion` address from step 4 and the same secret from step 5, restart
>    `lanmsg-server`.
> 7. For each remote family member: install `tor` locally (or Tor Browser,
>    which also exposes a local SOCKS proxy), give them `lanmsg-remote-cli`
>    plus a config pointing `server_addr` at the `.onion` public address
>    (port 8443) and the Pi's existing certificate fingerprint.
>
> `docs/SETUP.md` gets a new section with these exact steps written out in
> full; `docs/NETWORK.md` gets a new "Case D — reachable from outside the LAN"
> section with the topology diagram above.
>
> ---
>
> ## Testing strategy
>
> - `internal/ratelimit`: table-driven — burst to `max`, next blocked,
>   distinct key unaffected, recovers after the window elapses.
> - `internal/tunnel/frame_test.go`, `auth_test.go`: round-trip over
>   `net.Pipe()`; wrong-secret rejection (mirrors the existing
>   `TestPassphraseChallengeResponse`-style test already in
>   `internal/crypto`); malformed `Hello.Role` rejected.
> - `internal/tunnel/listener_test.go`: preamble stripped and `RemoteAddr()`
>   reported correctly; missing/malformed preamble times out rather than
>   hanging.
> - `internal/servercore/tunnel_integration_test.go` — three-role loopback
>   test using **real** production code on every side (not re-implementations):
>   a real `*Server` with `cfg.Tunnel` pointing at a real `tunnel.Hub` bound to
>   loopback listeners (standing in for Tor, which isn't needed in tests —
>   SOCKS proxying is the only Tor-specific bit, and it's isolated behind the
>   `proxy.SOCKS5` dial call, easily swapped for a direct dial in tests), a
>   "remote" `clientcore.Client` pointed at the fake cloud's loopback address.
>   Assert a message from the tunnel-arriving client reaches a normal
>   LAN-arriving client and vice versa. Negative test: wrong secret is
>   rejected and `runTunnel` never obtains a usable session. Rate-limit test:
>   rapid attempts from one synthetic source IP (via the preamble) are
>   throttled, a second IP is unaffected.
> - Existing `internal/clientcore/integration_test.go` must keep passing
>   unchanged — confirms the `serve()`/`serveHTTP()` split is behavior-preserving.
>
> ---
>
> ## Build order
>
> 0. **Create `DEVLOG.md`** (project root, doesn't exist yet in LANmessenger)
>    and archive this plan's full text into it, dated, before touching any
>    code — per the standing devlog convention already used in HoneyPot and
>    PortKnocks (narrative "why" companion to `CHANGELOG.md`'s terse "what
>    shipped" list; plans approved through plan mode get archived here first
>    since the plan-mode file itself is overwritten by the next planning
>    session).
> 1. `internal/ratelimit` + wire into the existing LAN listener only. Ships
>    independently, reduces exposure risk before anything is internet-reachable.
> 2. Refactor `serve()` → `serve()` + `serveHTTP()`. Zero behavior change,
>    covered by existing tests.
> 3. `internal/tunnel`: `frame.go`, `protocol.go`, `auth.go` — pure library,
>    unit-tested via `net.Pipe()`.
> 4. `internal/tunnel`: `listener.go`, `hub.go` — still library-only, tested
>    with loopback listeners.
> 5. `cmd/lanmsg-tunnel` — inert until a Pi is configured to use it; safe to
>    merge/deploy with no effect on the existing product.
> 6. `internal/servercore`: `TunnelConfig`, `runTunnel`/`dialTunnel`, wired
>    into `serve()`, gated behind `cfg.Tunnel == nil`.
> 7. `internal/clientcore`: `SOCKSProxy` field + dial change (opt-in, LAN
>    clients unaffected).
> 8. `cmd/lanmsg-remote-cli`.
> 9. Three-role loopback integration test — the strongest correctness gate
>    before recommending this to real users.
> 10. Docs: `docs/DESIGN.md` new §13, `docs/NETWORK.md` Case D,
>     `docs/SETUP.md` Tor walkthrough, `deploy/lanmsg-tunnel.service`.
> 11. Live deployment walkthrough on the actual AWS box and Pi, together,
>     following the steps above.
>
> ---
>
> ## New dependencies
>
> - `github.com/hashicorp/yamux` — connection multiplexing (approved).
> - `golang.org/x/net/proxy` — SOCKS5 dialing to reach `.onion` addresses
>   (standard extended-library package maintained by the Go team).
>
> ---
>
> ## Draft language for new DESIGN.md §13
>
> > ## 13. Reaching the relay from outside the LAN: the Tor-tunneled cloud relay
> >
> > The relay (§1) assumes every client can route to it. For family members
> > away from the house, that assumption doesn't hold, and the home router has
> > no public IP and no port-forwarding. The fix keeps the Pi's role
> > unchanged — it still only ever *dials out* — and adds one new, deliberately
> > minimal component: `lanmsg-tunnel`, a small always-on process on a cheap
> > cloud VM that does pure byte-level TCP forwarding, reachable only via a Tor
> > hidden service (`.onion` address) rather than any open inbound port.
> >
> > **What it is not.** It is not a second relay. It has no database, no
> > roster, no message queue, no E2E keys, and no household passphrase. It
> > cannot approve a device, read a message, or impersonate the relay to a
> > client. Its public-facing listener is deliberately never wrapped in the
> > app's TLS, because that TLS must terminate only at the Pi (§1). Its only
> > privilege is deciding whether one specific connection is allowed to call
> > itself "the backend" — narrower and less trusted than anything the Pi
> > itself holds (§12.2: "the relay is distribution, not authority").
> >
> > **How it works.** The Pi dials the cloud box's `.onion` address (through
> > its own local Tor client) and proves it holds a shared secret
> > (Argon2id + HMAC challenge–response — the same primitive as the household
> > passphrase, but a separate secret scoped only to this link). No additional
> > TLS wraps this leg: a `.onion` address is itself a cryptographic proof of
> > identity, so a second layer would be redundant. The resulting connection is
> > multiplexed (`yamux`) into one logical stream per remote client the cloud
> > box accepts. Each stream carries that client's raw TLS bytes untouched
> > straight to the Pi's existing WebSocket server, which handles it exactly
> > like a LAN connection — same certificate, same fingerprint pin, same
> > passphrase auth, same rate limits.
> >
> > **What the cloud box can see if fully compromised:** connection metadata
> > only — timing and byte counts of an already-anonymized Tor circuit; not
> > even the remote client's real IP is guaranteed visible past the `.onion`
> > layer. It can never see message content, the household passphrase, or any
> > device's keys, and can at worst deny service, which clients already
> > handle as an ordinary "relay unreachable" reconnect.
> >
> > Remote clients need no changes to the existing GUI or CLI — a new, minimal
> > `lanmsg-remote-cli` reuses the same client library with one added optional
> > setting (a local SOCKS proxy address) to route through Tor.
>
> ---
>
> ## Verification
>
> - `go build ./... && go vet ./...` after each build-order step.
> - `go test -race ./...` — must stay green throughout, including the new
>   three-role loopback integration test.
> - Manual live test once deployed: enroll `lanmsg-remote-cli` from a machine
>   outside the LAN (e.g. phone hotspot), send a message, confirm it's
>   received by a LAN-connected GUI client and vice versa; kill the cloud
>   box's tunnel process mid-session and confirm the Pi's `runTunnel` retries
>   with backoff and recovers when it's restarted.
