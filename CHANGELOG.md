# Changelog

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
