# Changelog

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
