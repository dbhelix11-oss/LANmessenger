# Changelog

## 2026-09-22 — desktop client: fix blank window on taskbar restore; visible build version

### Added

- The desktop client's window title and main toolbar now show a build
  identifier (release version plus a 7-char git commit hash, e.g.
  `v0.1.0 (77d0884)`, `-dirty` suffixed if built from an uncommitted tree) —
  for confirming at a glance that a running instance is actually the build
  you think it is. Needs no build-script changes: Go embeds this
  automatically for any build made from a git checkout.

### Fixed

- Restoring the desktop client from the taskbar (a single click, as opposed
  to using the tray menu's "Show" item) left the window showing only its
  frame with no rendered content — transparent, showing whatever was behind
  it. Root cause: minimizing calls Fyne's `Window.Hide()`, which marks
  Fyne's *own* internal visibility state hidden, not just the OS window; a
  taskbar click restores the window by sending `_NET_ACTIVE_WINDOW` straight
  to the window manager, remapping the X11 window directly with no call into
  Fyne's API at all — so the frame becomes visible but Fyne's render loop,
  still believing itself hidden, never resumes drawing into it. Two earlier
  attempts at a fix (forcing `Content().Refresh()`, then a window-resize
  nudge) both missed this, since the problem was never "needs a repaint."
  Fixed by explicitly calling `Show()` (matching the tray menu's own working
  path) whenever the window is detected coming back while still marked
  hidden. Reproduced and verified with a scripted `xdotool`/`wmctrl`
  minimize-then-taskbar-restore loop against a real enrolled client, not
  just by inspection — full details, including two dead-end fix attempts,
  in `DEVLOG.md`.

## 2026-09-21 — client auto-update system; desktop tray/notification polish

### Added

- **Client auto-update.** Two independent mechanisms:
  - A protocol compatibility gate on the `ready` frame
    (`ServerVersion`/`MinClientVersion`): a client below the relay's
    configured minimum (`min_client_version` in `server.toml`, no floor by
    default) is rejected at `Hello` time and stops reconnecting; anything
    else gets a soft "update available" notice (GUI status line, CLI
    stderr).
  - A signed, per-artifact self-update manifest. New `internal/update`
    package (manifest fetch, Ed25519 signature verification, `seq`
    anti-rollback check, SHA-256-verified download, atomic swap, and
    re-exec for long-lived processes) that `lanmsg-cli` and
    `lanmsg-remote-cli` check on every connect and update themselves from —
    the desktop GUI only ever shows the notice, it never self-swaps. New
    admin-only `cmd/lanmsg-signrelease` tool (never built by CI or shipped
    anywhere) generates the release keypair and signs manifests, always
    re-deriving artifact hashes from the actual bytes on disk. The relay
    (`internal/servercore/updates.go`) only ever distributes whatever's
    placed in its `updates/` directory — it never builds or signs anything.
  - New `internal/version` package as the single source of truth for the
    build version, replacing a write-only constant.
  - New `scripts/pi-release.sh` cross-compiles `lanmsg-cli`,
    `lanmsg-remote-cli`, and `lanmsg-server` for every supported
    platform locally (no CI needed for routine releases); a new `gui-build`
    job in `.github/ci.yml.example` (tag-triggered) covers the one binary
    that can't be cross-compiled from the Pi. Signing always happens on a
    third machine, separate from both — see `docs/SETUP.md`'s new
    "Updates" section and `docs/DESIGN.md` §12.2 for the full design and
    release workflow.
- **Desktop client: unread tray badge.** The tray icon gets a red-dot badge
  (computed at runtime from the existing app icon) when a message arrives
  while the window is hidden to the tray; clears when the window is shown
  again.
- **Desktop client (Linux): notifications persist until clicked** when the
  session's notification server supports it (detected via
  `GetCapabilities`), instead of a fixed 6-second expiry — clicking one
  brings the window to front and clears the unread badge. Falls back to the
  old fixed timeout on notification servers that can't tell us about a
  click.
- `docs/DEPLOY-TO-PI.html` — a step-by-step tutorial for building a new
  relay version, installing it on the Pi in place, and independently
  verifying its TLS certificate fingerprint from a third computer before
  trusting it.
- `docs/MACOS-NOTIFICATIONS.md` — troubleshooting checklist for the known
  macOS signed-`.app` notification-permission requirement.

### Fixed

- `internal/clientcore`: the client handshake only expected an
  `auth_challenge` frame immediately after `hello`; an `error` frame
  arriving there instead (as the new `client_too_old` rejection does) was
  silently mangled into a generic error instead of a `*RelayError`, so the
  hard-stop path could never actually be detected. Found by the new
  version-gate integration test.
- `internal/update`: `DownloadAndVerify`'s cleanup of a failed download used
  a named return value from inside a `defer`, which had already been
  overwritten to `""` by the failing `return` statement — the temp file was
  never actually deleted. Found by its own test.
- Desktop client (Linux): restoring the main window through a path other
  than the tray's own "Show" item (e.g. a native window-manager restore)
  could leave just the window frame on screen with no repainted content,
  since the X11 property watcher only ever reacted to the window becoming
  minimized, never to it becoming un-minimized. Now forces a repaint on
  either transition.

## 2026-09-20 — desktop client: start-minimized flag, tray-watcher shutdown fix

### Added

- `lanmsg -start-minimized` starts the desktop client hidden in the system
  tray instead of opening the main window — for launching on login without
  the roster popping up every time. No-op (window shows as normal) if the
  desktop has no system tray.

### Fixed

- `cmd/lanmsg/traywatch_linux.go`: the X11 tray-watcher connection could be
  closed twice on shutdown (once by its own deferred close, once by the
  goroutine watching for app-context cancellation), and the underlying
  `xgb` library panics on a second `Close()`. Found while setting up
  autostart, which made a clean exit-on-logout a routine path instead of a
  rare one. Now guarded with `sync.Once`.

### Docs

- `docs/SETUP.md`: a new "Starting automatically" section under the client
  setup steps — installing the desktop client per-user under `~/.local`
  (no `sudo`), adding it to the application menu and/or Desktop, and
  autostarting it via the XDG autostart spec (`~/.config/autostart/`) with
  `-start-minimized`.

## 2026-09-20 — docs: README build commands for lanmsg-remote-cli on Android

### Changed

- `README.md`'s Building section now includes the `GOOS=android
  GOARCH=arm64` cross-compile command for `lanmsg-remote-cli`, plus the
  on-device alternative (`pkg install golang` under Termux, no GOOS/GOARCH
  needed). Previously this was documented only in `docs/SETUP.md`.

## 2026-09-20 — lanmsg-remote-cli: add a `watch` command

### Added

- `lanmsg-remote-cli watch` — stays connected and prints incoming messages,
  presence, and connection-state changes as they arrive, same idea as
  `lanmsg-cli watch`. Previously the remote CLI was send-only: replies still
  arrived and were safely acknowledged and stored locally, but nothing ever
  displayed them.

## 2026-09-20 — fix: sending right after connecting could miss the roster

### Fixed

- `lanmsg-cli send` and `lanmsg-remote-cli send` could fail with "no peer
  matching ..." for a genuinely valid recipient. The relay sends the
  directory snapshot (the roster's actual contents) as a separate frame
  right after `ready`; a fresh connection could report ready a moment
  before that snapshot was processed, so an immediate roster lookup could
  race against a still-empty roster. Far more likely over Tor, whose
  latency is higher and more variable than a LAN connection. Both commands
  now retry the lookup for a few seconds instead of checking once. Found
  during the first live deployment.

## 2026-09-20 — fix: lanmsg-remote-cli's default config dir collided with the GUI's

### Fixed

- `lanmsg-remote-cli` defaulted to the same config directory as the GUI
  (`lanmsg`) and `lanmsg-cli`. Running `enroll` here with no `-config` flag
  silently overwrote a LAN client's `config.json` with `socks_proxy` set,
  breaking its LAN connection (Tor refuses to proxy to private-use
  addresses — surfaced as "general SOCKS server failure"). Found during the
  first live deployment. Now defaults to its own sibling directory
  (`lanmessenger-remote`) so it can never collide.

## 2026-09-20 — reach the relay from outside the LAN (Tor-tunneled cloud relay)

### Added

- **Cloud tunnel** (`cmd/lanmsg-tunnel`, `internal/tunnel`): a small, stateless
  byte-forwarder deployed on a cloud VM, reachable only via a Tor hidden
  service — no inbound port on the home network or the cloud box. The relay
  dials out through its own local Tor SOCKS proxy, authenticates with a
  shared secret (Argon2id + HMAC, the same construction as the household
  passphrase), and the resulting connection is multiplexed
  (`github.com/hashicorp/yamux`) so many remote clients can be relayed
  concurrently. TLS for the app protocol still terminates only at the relay.
- **Rate limiting** (`internal/ratelimit`): a sliding-window limiter bounding
  connection attempts (per source IP) and inbound frames (per device), wired
  into the relay's existing LAN listener and the new cloud tunnel.
- `cmd/lanmsg-remote-cli`: a minimal, one-shot terminal client for sending a
  message through the cloud tunnel — no roster, no presence, no daemon.
  Reuses `internal/clientcore` end to end, with one added optional setting
  (a local SOCKS5 proxy) to route through Tor. Cross-compiles for
  `android/arm64` and runs under Termux.
- `internal/clientcore`: optional `SOCKSProxy` config field and a
  SOCKS5-aware certificate-fingerprint probe
  (`FingerprintOfPresentedCertVia`), used only by remote/Tor clients —
  existing LAN clients are unaffected.
- `internal/servercore`: optional `[tunnel]` config block; when present the
  relay also dials out to the cloud tunnel alongside its normal LAN
  listener.
- `deploy/lanmsg-tunnel.service` — systemd unit for the cloud tunnel,
  sandboxed tighter than the relay's own unit since it holds no database or
  queue.
- `docs/DESIGN.md` §13, `docs/NETWORK.md` ("Case D"), `docs/SETUP.md` §5 —
  design, topology, and a full deployment walkthrough (Tor setup on both
  ends, the cloud box, and Android/Termux).
- `DEVLOG.md` — a new narrative dev notebook alongside this changelog.

### Fixed

- A rate limiter keyed by the full `ip:port` remote address defeats itself,
  since every new connection gets a fresh ephemeral port; now keyed by IP
  only.

## 2026-08-31 — desktop client: minimize-to-tray, notification fix

### Fixed

- **Minimize now hides to the system tray** like the close button already did.
  Fyne has no minimize hook, so on X11 (`cmd/lanmsg/traywatch_linux.go`) a
  second read-only X connection watches this window's `WM_STATE` /
  `_NET_WM_STATE` and calls `Hide()` when the WM iconifies it. Best-effort: no X
  display / no tray / window-not-found leaves minimize unchanged. macOS/Windows
  keep native minimize for now. New dep: `github.com/BurntSushi/xgb` (pure Go).
- **Linux desktop notifications now expire.** Fyne 2.8 sends the freedesktop
  `Notify` call with `expire_timeout = 0` ("never expire"); minimal X11
  notifiers then leave popups stuck on screen with no close button.
  `cmd/lanmsg/notify_linux.go` issues the D-Bus call directly with a 6 s
  timeout and falls back to Fyne if the session bus is unreachable.
  `github.com/godbus/dbus/v5` promoted from indirect to direct.

### Changed

- `docs/SETUP.md` — macOS packaging section now covers signing the `.app` and
  the notification-permission requirement (a bare `go build` binary shows no
  notifications on macOS).
- `docs/DESIGN.md` §7 — new "Window, tray, and notifications" note; §12.2
  reworked so binary-delta ("bit comparison") updates are a designed-in but
  separate, opt-in module (`internal/updatedelta` behind a `Patcher` interface)
  that never changes the signed-manifest trust model.

## 2026-08-31 — docs, packaging, v2 design

No code changes; documentation, packaging assets, and the deployment unit.

### Added

- `docs/INSTALL.html` — a standalone, offline installation guide covering
  installing Go on macOS / Linux / Raspberry Pi OS / Windows, cross-compiling the
  relay for a Pi, the full systemd walkthrough, building and packaging the
  desktop client, first-run enrollment, contact verification, and a
  troubleshooting section (`203/EXEC`, `go`/`fyne` not found, fingerprint
  mismatch, cross-subnet reachability).
- `cmd/lanmsg/icon.png` — 512×512 application icon (metadata-stripped).
- `cmd/lanmsg/FyneApp.toml` — app metadata so `fyne package` needs no flags.
- `docs/DESIGN.md` §12 — design for two v2 features: **paging** (high-priority
  attention alerts as new end-to-end `Inner` payloads; per-OS always-on-top
  shim; abuse controls) and a **signed client-update system** (CI builds, relay
  distributes, client verifies an offline-signed manifest; protocol
  version-negotiation gate first; no binary diffing; mobile updates via the app
  stores).

### Changed

- `docs/SETUP.md` — restructured into a linear relay walkthrough (get the binary
  → `setup` → systemd) with a symptom/cause/fix troubleshooting table; the
  packaging section now states that each OS bundle is built on that OS and shows
  the `fyne-cross` path for Windows-from-Linux.
- `deploy/lanmsg-server.service` — header comment points at `docs/SETUP.md`;
  `ReadWritePaths` now includes `/etc/lanmsg` so the first-device-becomes-admin
  config write succeeds under `ProtectSystem=strict`.
- `README.md` — v2 list now mentions paging and the client-update system.
- `.gitignore` — ignore `fyne package` output (`*.tar.xz`, `*.app`).

## 2026-08-29 — initial v1

First complete version of lanmessenger: an end-to-end-encrypted messenger for a
home network.

### Added

- **Wire protocol** (`internal/proto`): versioned JSON envelopes over a TLS
  WebSocket; frames for enrollment, directory, presence, message relay, acks,
  and admin actions; end-to-end `Inner` payloads (text, file offer/chunk,
  receipts).
- **Crypto** (`internal/crypto`): per-device Ed25519 (signing) + X25519
  (`nacl/box`) identities stored `0600`; `Seal`/`Open` authenticated encryption;
  human-readable key fingerprints; household-passphrase challenge/response
  (Argon2id + HMAC, constant-time verify); Ed25519 signatures for admin actions.
- **Relay server** (`cmd/lanmsg-server`, `internal/servercore`): self-signed TLS
  with a printed fingerprint; enrollment with optional admin approval (first
  device becomes admin); device directory; presence fan-out; end-to-end-blind
  message relay; SQLite offline queue with retention and per-recipient caps;
  `setup` / `run` / `fingerprint` subcommands; systemd unit, Dockerfile and
  docker-compose in `deploy/`.
- **Client engine** (`internal/clientcore`): auto-reconnecting connection with
  exponential backoff; enrollment (immediate and pending-approval); TLS
  certificate pinning (trust on first use); local SQLite history, roster and
  outbox; text messaging; outbox flush of messages composed offline; chunked
  file transfer with whole-file SHA-256 verification; presence including
  do-not-disturb; a UI-agnostic event stream; admin approve/deny.
- **Desktop client** (`cmd/lanmsg`, Fyne): first-run wizard with certificate
  fingerprint confirmation; roster with presence; 1:1 chat; file send via file
  dialog; system-tray status switcher; desktop notifications (muted under DND);
  peer fingerprint verification; admin panel for pending devices.
- **Terminal client** (`cmd/lanmsg-cli`): `enroll`, `roster`, `send`, `status`,
  `watch`.
- **Docs**: `docs/DESIGN.md` (architecture and rationale, with primers on Go and
  on cryptography), `docs/SETUP.md`, `docs/NETWORK.md`.
- CI workflow: gofmt check, `go vet`, `go test -race`, and a cross-compile matrix
  for the relay and engine.

### Known limitations (targeted for v2)

- No forward secrecy (each device has one long-lived box key).
- Identity is per-device, not per-person-across-devices.
- No group rooms.
- No local web front-end or mobile apps yet.
- The relay can see metadata (who talks to whom, when, message sizes) though not
  contents.
