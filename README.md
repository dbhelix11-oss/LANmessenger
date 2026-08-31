# lanmessenger

A private messenger for a home network. Family members chat computer-to-computer
— across a wired LAN and Wi-Fi, and across more than one router — instead of
texting through phones or shouting down the hall.

## Goals

- **End-to-end encrypted** 1:1 messages. The relay server sees ciphertext only.
- **Presence**: available, away, busy, do-not-disturb, invisible; auto-away on idle.
- **Works across subnets** (e.g. an ISP router and a downstream OpenWRT router)
  by having every client dial out to one small relay server.
- **Offline delivery**: messages queue on the server for a machine that's asleep
  and flush when it reconnects.
- **File transfer** between clients.
- Desktop client for macOS, Linux, and Windows (Fyne). A local web client and
  mobile apps are planned as additional front-ends over the same client core.

## Layout

| Path | What |
|---|---|
| `cmd/lanmsg-server` | The relay: directory, presence, message relay, offline queue. |
| `cmd/lanmsg` | The Fyne desktop client (GUI, system tray, notifications). |
| `cmd/lanmsg-cli` | A terminal client over the same engine — handy for scripting and testing. |
| `internal/proto` | Wire protocol: JSON frames over a TLS WebSocket. |
| `internal/crypto` | Device identity keys, message sealing, fingerprints, passphrase auth. |
| `internal/servercore` | Relay logic, split out for testing. |
| `internal/clientcore` | Headless client engine: connection, crypto, local store, events. Every front-end uses this. |
| `internal/store` | SQLite helpers shared by relay and client. |
| `docs/` | [DESIGN.md](docs/DESIGN.md) (how & why, with Go + crypto primers), [SETUP.md](docs/SETUP.md), [NETWORK.md](docs/NETWORK.md). |
| `deploy/` | systemd unit, Dockerfile, docker-compose for the relay. |

## Status

Feature-complete for v1. `go test -race ./...` passes; the desktop GUI has been
run end to end (wizard → enrol → chat → file transfer) against a live relay.

- **Relay** (`cmd/lanmsg-server`): enrollment, device directory, presence
  fan-out, encrypted message relay, offline queue with retention/caps, admin
  approve/deny. `setup` / `run` / `fingerprint` subcommands.
- **Client engine** (`internal/clientcore`): auto-reconnecting connection,
  enrollment (immediate and pending-approval), end-to-end encryption, local
  SQLite history + roster, text messaging, an outbox that flushes messages
  composed offline, chunked file transfer with SHA-256 verification, presence,
  an event stream, admin actions, TLS certificate pinning.
- **Desktop client** (`cmd/lanmsg`): first-run wizard with fingerprint
  confirmation, roster with presence, 1:1 chat, drag-in file send, system-tray
  status switcher, desktop notifications (muted under DND), fingerprint
  verification, admin panel.
- **Terminal client** (`cmd/lanmsg-cli`): `enroll` / `roster` / `send` /
  `status` / `watch`.
- **Deferred to v2:** group rooms, per-person (multi-device) identity, forward
  secrecy (double ratchet), a local web front-end, mobile apps, auto-away on OS
  idle, [paging](docs/DESIGN.md#121-paging--a-high-priority-get-back-here-alert)
  (high-priority attention alerts) and a
  [signed client-update system](docs/DESIGN.md#122-client-updates--who-builds-who-distributes-who-trusts).

## Building

Requires Go 1.27+.

```sh
# Relay + engine + terminal client — no C toolchain needed:
CGO_ENABLED=0 go build ./cmd/lanmsg-server ./cmd/lanmsg-cli

# Desktop GUI — needs a C compiler and OpenGL headers (see docs/SETUP.md):
go build ./cmd/lanmsg

go test ./...
```

Then follow [docs/SETUP.md](docs/SETUP.md).

## Security model (short version)

- Each device generates an ed25519 signing key and an X25519 box key on first
  run. Public halves are published to the server directory; private halves stay
  on the device (stored `0600`).
- Client↔server traffic is TLS. The server self-signs on first run and prints
  its certificate fingerprint; clients pin it on first connect and warn if it
  changes.
- Messages are sealed with NaCl `box` (X25519 + XSalsa20-Poly1305) between the
  two devices. The server relays `{to, from, nonce, ciphertext}` and can't read
  the contents.
- New devices authenticate with a shared household passphrase using a
  challenge/response (Argon2id + HMAC); the passphrase is never sent. An
  optional admin-approval mode holds new devices in a pending state until an
  existing trusted device approves them.
- Peer key fingerprints are shown in the client for out-of-band verification.
