# Setting up lanmessenger

lanmessenger is two programs:

- **The relay** (`lanmsg-server`) — **one** copy, on a machine that stays on (a
  Raspberry Pi, a NAS, a spare Linux box). It forwards encrypted messages between
  clients and holds messages for a client that's asleep. It cannot read anything
  it forwards.
- **The client** — **one per computer people actually use**. Either the desktop
  app (`lanmsg`, with a roster, chat, tray icon and notifications) or the
  terminal client (`lanmsg-cli`).

Every client connects to the relay over TLS. The relay creates its own
certificate the first time it runs and prints a **fingerprint** — a short string
of hex. Each client shows you that fingerprint on its first connection and asks
you to confirm it. **Write the fingerprint down when you set up the relay**; you
need it on every client.

Order of this doc: [1. the relay](#1-the-relay) → [2. a client](#2-the-client) →
[3. verify contacts](#3-verifying-contacts) → [4. troubleshooting](#4-troubleshooting) →
[5. reaching the relay from outside the LAN](#5-reaching-the-relay-from-outside-the-lan-optional)
(optional).

---

## 1. The relay

### Step 1 — get the `lanmsg-server` binary onto the relay machine

Two ways. Building on your desktop and copying the binary over is usually less
hassle than installing Go on a Pi.

#### Option A — build on your desktop, copy it over (recommended)

In a checkout of this repo on your desktop:

```sh
# Raspberry Pi 3 / 4 / 5 on 64-bit Raspberry Pi OS   (on the Pi, `uname -m` says aarch64):
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" \
    -o lanmsg-server-arm64 ./cmd/lanmsg-server

# 32-bit Raspberry Pi OS:
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags="-s -w" \
    -o lanmsg-server-armhf ./cmd/lanmsg-server

# An ordinary x86-64 Linux box:
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o lanmsg-server ./cmd/lanmsg-server
```

Why `CGO_ENABLED=0`: it produces a single self-contained binary with no shared
library dependencies, and it is what lets you build for the Pi *from a different
kind of machine* without installing a cross-compiler. (The relay uses a pure-Go
SQLite, so there is no C code to link.) `-trimpath -ldflags="-s -w"` just keeps
your local paths out of the binary and makes it smaller.

Copy it to the relay and install it (swap in your relay's hostname):

```sh
scp lanmsg-server-arm64 stoic@relay.lan:/tmp/
ssh stoic@relay.lan
sudo install -m 755 /tmp/lanmsg-server-arm64 /usr/local/bin/lanmsg-server
```

#### Option B — build on the relay machine itself

Needs Go 1.27+ on that machine. The `apt` package is too old — install from
<https://go.dev/dl> (pick the `linux-arm64` tarball for a 64-bit Pi). Then:

```sh
git clone <your repo> lanmessenger && cd lanmessenger
CGO_ENABLED=0 go build -o /tmp/lanmsg-server ./cmd/lanmsg-server
sudo install -m 755 /tmp/lanmsg-server /usr/local/bin/lanmsg-server
```

#### Check it either way

```sh
lanmsg-server -h        # should print usage text
```

### Step 2 — create the config (run once)

`setup` writes the config file, generates the TLS certificate, and prints the
fingerprint.

We put the config in `/etc/lanmsg/` and the data (database + certificate) in
`/var/lib/lanmsg/`, both owned by a dedicated `lanmsg` user that the service will
run as:

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin lanmsg
sudo install -d -o lanmsg -g lanmsg /etc/lanmsg /var/lib/lanmsg

sudo -u lanmsg lanmsg-server setup \
    -config /etc/lanmsg/server.toml \
    -data   /var/lib/lanmsg \
    -listen 0.0.0.0:8443
```

- `0.0.0.0:8443` means "listen on port 8443 on every network this machine has."
- It prompts for the **household passphrase** twice. Everyone uses this same
  passphrase to enroll a device. The relay stores only a verifier derived from
  it, never the passphrase itself.
- Run it as `lanmsg` (via `sudo -u lanmsg`) so the files it writes are owned by
  the user the service runs as.

When it finishes it prints:

```
TLS certificate fingerprint (SHA-256) — every client confirms this on first connect:

  57:be:45:64:34:c8:...:3b:46
```

**Copy that fingerprint somewhere safe.** Lost it later?

```sh
sudo -u lanmsg lanmsg-server fingerprint -config /etc/lanmsg/server.toml
```

### Step 3 — run it under systemd

The repo ships a unit at [`deploy/lanmsg-server.service`](../deploy/lanmsg-server.service)
that already matches the paths above.

```sh
# copy the unit into place (scp it over, or run from your repo checkout on the relay)
sudo cp deploy/lanmsg-server.service /etc/systemd/system/

# 1. test by hand first — you should see a log line saying it's listening; Ctrl-C to stop
sudo -u lanmsg lanmsg-server run -config /etc/lanmsg/server.toml

# 2. hand it to systemd
sudo systemctl daemon-reload
sudo systemctl enable --now lanmsg-server

# 3. check
systemctl status lanmsg-server
journalctl -u lanmsg-server -f
```

`enable --now` starts it now *and* makes it start again after a reboot.

If `status` shows `activating (auto-restart)` with an error, jump to
[Troubleshooting](#4-troubleshooting) — the two usual causes are "binary not
where the unit expects it" and "setup was never run."

### Running the relay in Docker instead

```sh
cd deploy
docker compose run --rm relay setup -config /data/server.toml -data /data -listen 0.0.0.0:8443
docker compose up -d
docker compose run --rm relay fingerprint -config /data/server.toml   # print the fingerprint
```

`docker-compose.yml` uses host networking and keeps everything under `./data`.

### Admin approval (optional)

By default, anyone who has the passphrase and can reach the relay is admitted
automatically. To instead require that an existing trusted device approve each
new one:

- pass `-approval` to `setup`, **or**
- set `require_admin_approval = true` in `server.toml` and restart the service.

The **first** device to enroll is always admitted and becomes an admin — even
with approval turned on — so you can't lock yourself out. After that, each new
device sits in a "pending" state until an admin approves it from the desktop
app's **Pending devices** panel.

---

## 2. The client

Pick one:

| | Desktop app (`lanmsg`) | Terminal client (`lanmsg-cli`) |
|---|---|---|
| What it is | Windowed app: roster, chat, tray icon, notifications | `enroll` / `roster` / `send` / `status` / `watch` |
| Build needs | Go **and a C compiler + OpenGL/X11 libs** | Go only (`CGO_ENABLED=0 go build ./cmd/lanmsg-cli`) |
| Good for | Everyday use | Headless machines, scripting, testing the relay |

### Building the desktop app from source

The GUI toolkit (Fyne) needs a C compiler and some system libraries:

| OS | Install first |
|---|---|
| Debian / Ubuntu | `sudo apt install golang gcc pkg-config libgl1-mesa-dev xorg-dev libxkbcommon-dev` |
| Fedora | `sudo dnf install golang gcc pkgconf-pkg-config mesa-libGL-devel libX11-devel libXcursor-devel libXrandr-devel libXinerama-devel libXi-devel libxkbcommon-devel` |
| macOS | `xcode-select --install` |
| Windows | a MinGW-w64 gcc (via MSYS2), or cross-build with `fyne-cross` |

```sh
go build -o lanmsg ./cmd/lanmsg        # or run it directly: go run ./cmd/lanmsg
```

That's all you need to *run* the app on the machine you built it on.

### Making a double-clickable bundle (`.app`, `.exe`, Linux tarball)

Use the `fyne` packaging tool:

```sh
go install fyne.io/tools/cmd/fyne@latest
```

`go install` puts it in `$(go env GOPATH)/bin` (usually `~/go/bin`). If
`fyne: command not found`, that directory isn't on your `PATH` — add it:
`export PATH="$PATH:$(go env GOPATH)/bin"`.

App metadata (name, icon, ID, version) lives in
[`cmd/lanmsg/FyneApp.toml`](../cmd/lanmsg/FyneApp.toml), so packaging is one
command run **from that directory**:

```sh
cd cmd/lanmsg
fyne package -os linux         # -> lanmessenger.tar.xz
```

**You package for an OS *on* that OS** — Fyne needs a C compiler that targets the
destination, and `fyne package` does not ship cross-toolchains:

| Target | Where to run it |
|---|---|
| Linux tarball | on Linux: `cd cmd/lanmsg && fyne package -os linux` |
| macOS `.app` | on a Mac (after `xcode-select --install`): `cd cmd/lanmsg && fyne package -os darwin` |
| Windows `.exe` | on Windows (MSYS2 + MinGW): `fyne package -os windows`, **or** from Linux with [`fyne-cross`](https://github.com/fyne-io/fyne-cross) (Docker-based): `sudo apt install gcc-mingw-w64 && go install github.com/fyne-io/fyne-cross@latest && fyne-cross windows -arch amd64 ./cmd/lanmsg` |

Cross-building a macOS `.app` from Linux is not practical — it needs the Apple
SDK. Build it on a Mac.

**macOS: run the `.app`, and sign it, or notifications stay silent.** Fyne asks
macOS for notification permission only from a *bundled* app; an unsigned bundle
can't get that permission and Fyne falls back to `osascript`, which recent macOS
silently drops. So on macOS:

```sh
cd cmd/lanmsg && fyne package -os darwin          # -> lanmessenger.app (bundle ID from FyneApp.toml)
codesign --force --deep --sign - lanmessenger.app  # ad-hoc; a Developer ID cert is better
open lanmessenger.app                              # launch the .app, not the bare `go build` binary
```

Approve the prompt on the first message. If you denied it once, re-enable it in
System Settings → Notifications → **lanmessenger**. Running a plain
`go build ./cmd/lanmsg` binary will never show notifications on macOS.

### First run

The desktop app opens a short wizard:

1. **Relay address** — e.g. `relay.lan:8443` or `192.168.1.10:8443`.
2. **Confirm the fingerprint** — the app shows the certificate fingerprint it
   received from the relay. Check it against what `setup` printed. Match →
   continue. No match → stop (see [Troubleshooting](#4-troubleshooting)).
3. **Display name and passphrase** — a name for *this device* ("Dad's laptop")
   and the household passphrase.
4. If the relay uses admin approval and this isn't the first device, you land on
   a "waiting for approval" screen until an admin approves you.

The terminal client does the same in one command:

```sh
lanmsg-cli enroll -server relay.lan:8443 -name "Dad's laptop"
# fetches the fingerprint, shows it, asks you to confirm, then prompts for the passphrase
```

Config, keys and history live in your OS config directory:

| OS | Path |
|---|---|
| Linux | `~/.config/lanmessenger` |
| macOS | `~/Library/Application Support/lanmessenger` |
| Windows | `%AppData%\lanmessenger` |

Pass `-config <dir>` to any client to use a different directory (useful for
running two identities on one machine while testing).

---

## 3. Verifying contacts

TLS and the certificate fingerprint protect the hop between a client and the
relay. They do **not** prove that "Mom's laptop" in your roster is really Mom's —
the relay operator, or anyone who learned the passphrase, could enroll a device
under any name.

So each device also has its own short **key fingerprint** (desktop app:
Settings → *This device's fingerprint*). A newly seen person shows in your roster
as *unverified*. To verify: have them read their fingerprint aloud — across the
house, over the phone — compare it to what your client shows for them, and click
**verify**.

If a contact's fingerprint ever changes (reinstall, new machine, or something
wrong), the client flags it and you verify again.

---

## 4. Troubleshooting

### The relay service won't start

Get the exact error first:

```sh
systemctl status lanmsg-server
journalctl -u lanmsg-server -n 40 --no-pager
```

| Symptom | Cause | Fix |
|---|---|---|
| `status=203/EXEC` | the binary isn't at `/usr/local/bin/lanmsg-server`, isn't executable, or is built for the wrong CPU | `ls -l /usr/local/bin/lanmsg-server`, then `file` it — on a 64-bit Pi you want `ELF 64-bit ... ARM aarch64`. Redo Step 1. |
| `config has no passphrase` or `no such file` for `server.toml` | `setup` was never run, or `-config` points at the wrong path | run Step 2; make sure the path matches `ExecStart=` in the unit |
| `permission denied` on the config or data dir | `/etc/lanmsg` or `/var/lib/lanmsg` isn't owned by `lanmsg` | `sudo chown -R lanmsg:lanmsg /etc/lanmsg /var/lib/lanmsg` |
| `bind: address already in use` | port 8443 is taken | `sudo ss -tlnp 'sport = :8443'`; change `listen_addr` in `server.toml` or stop the other program |
| `bind: permission denied` on a port < 1024 | non-root can't bind low ports | use a port ≥ 1024 (8443 is fine), or add `AmbientCapabilities=CAP_NET_BIND_SERVICE` to the unit |

Always reproduce it by hand as the service user before blaming systemd:

```sh
sudo -u lanmsg lanmsg-server run -config /etc/lanmsg/server.toml
```

### A client reports a fingerprint mismatch

The fingerprint the client now sees differs from the one it pinned on first
connect. Usually one of:

- **The relay's certificate was regenerated** — its data dir was wiped, or
  `setup` was re-run. Every client has to re-confirm: accept the new fingerprint
  when prompted, or clear the stored one in the client's config dir and
  re-enroll.
- **You're connecting to the wrong host** — a different machine answered on that
  address.
- **Something is intercepting the connection** — if neither of the above fits,
  stop and investigate.

### A client connects from one part of the house but not another

That's network routing, not lanmessenger — the relay has to be reachable from
both subnets. See [NETWORK.md](NETWORK.md) for where to place the relay and the
static route a two-router setup needs.

### `connection refused` from every client

The relay isn't listening, or a firewall blocks the port. On the relay:
`systemctl status lanmsg-server`, then `ss -tlnp 'sport = :8443'`. From a client:
`nc -vz relay.lan 8443`.

---

## 5. Reaching the relay from outside the LAN (optional)

Everything above assumes every client is on the home LAN. This section adds
one more path in: a small, stateless cloud component (`lanmsg-tunnel`) that
remote family members reach over Tor — no port-forwarding, no open port on
the home router, and (since it's a Tor hidden service) no open port on the
cloud box's firewall either. Design rationale and the exact trust boundary:
[DESIGN.md §13](DESIGN.md#13-reaching-the-relay-from-outside-the-lan-the-tor-tunneled-cloud-relay).
Topology diagram: [NETWORK.md, Case D](NETWORK.md#case-d--reachable-from-outside-the-lan).

### Step 1 — provision the cloud box

Any small, cheap, always-on VM works (e.g. an AWS EC2 `t4g.nano` or
equivalent). **Security group / firewall: no inbound rules at all** — not
even for the tunnel — beyond whatever you need for your own SSH access.
Tor needs no inbound port to publish a hidden service.

### Step 2 — install and configure Tor on the cloud box

```sh
sudo apt install tor
```

Add to `/etc/tor/torrc`:

```
HiddenServiceDir /var/lib/tor/lanmsg_tunnel/
HiddenServicePort 8443 127.0.0.1:8443
HiddenServicePort 9443 127.0.0.1:9443
```

```sh
sudo systemctl restart tor
sudo cat /var/lib/tor/lanmsg_tunnel/hostname   # your .onion address — stable across restarts
```

That `.onion` address is derived from a keypair Tor generates the first
time it starts, stored in `HiddenServiceDir`. It doesn't change on restart
as long as that directory isn't deleted — **back it up**; losing it means
generating a new address and re-pointing everyone at it.

### Step 3 — get `lanmsg-tunnel` onto the cloud box and run it

Same cross-compile pattern as the relay (Step 1 above), just a different
package:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" \
    -o lanmsg-tunnel ./cmd/lanmsg-tunnel   # adjust GOARCH for the cloud box's CPU
```

On the cloud box:

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin lanmsg-tunnel
sudo install -d -o lanmsg-tunnel -g lanmsg-tunnel /etc/lanmsg-tunnel
sudo install -m 0755 lanmsg-tunnel /usr/local/bin/lanmsg-tunnel

sudo -u lanmsg-tunnel lanmsg-tunnel setup -config /etc/lanmsg-tunnel/tunnel.toml
```

`setup` generates a random shared secret and prints it — **copy it down**;
this is the only time it's shown, and you'll need it in Step 4. It also
prints the loopback addresses Tor forwards to (defaults `127.0.0.1:8443`
public, `127.0.0.1:9443` backend, matching the `torrc` above).

```sh
sudo cp deploy/lanmsg-tunnel.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now lanmsg-tunnel
systemctl status lanmsg-tunnel
```

### Step 4 — point the home relay at it

Install Tor on the Pi too (client-only use — no `HiddenServiceDir` needed
there, just its local SOCKS proxy):

```sh
sudo apt install tor
```

Add a `[tunnel]` block to `/etc/lanmsg/server.toml` (the config `setup`
already created in [Step 2 of §1](#step-2--create-the-config-run-once)):

```toml
[tunnel]
cloud_onion_addr = "abcd...xyz.onion:9443"   # from Step 2 above, backend port
socks_proxy      = "127.0.0.1:9050"          # the Pi's local Tor SOCKS proxy (Tor's default)
secret            = "the secret lanmsg-tunnel setup printed in Step 3"
```

```sh
sudo systemctl restart lanmsg-server
journalctl -u lanmsg-server -f   # look for "backend authenticated" — confirms the tunnel is up
```

### Step 5 — set up each remote family member's client

Same enrollment flow as any client ([§2](#2-the-client)), pointed at the
`.onion` address instead of a LAN address, plus a local Tor for the SOCKS
proxy. Desktop/laptop:

```sh
sudo apt install tor   # or install Tor Browser, which also runs a local SOCKS proxy
lanmsg-remote-cli enroll -server abcd...xyz.onion:8443 -name "Dad's phone (remote)"
# fetches the fingerprint over Tor, shows it, asks you to confirm — should match
# the SAME fingerprint every LAN client already has, since TLS still terminates at the Pi
lanmsg-remote-cli send -to "Mom's laptop" -text "hello from the road"
lanmsg-remote-cli watch                                      # stay connected, print replies as they arrive
```

`lanmsg-remote-cli` has no roster and no presence-setting. `send` is a
one-shot fire-and-exit command. `watch` is the one long-running exception —
without it, replies still arrive and get safely stored (any message this
client is ever connected for gets acknowledged and saved locally,
regardless of whether anything is watching), but nothing prints them to the
terminal, so you'd have no way to know a reply came in. `enroll` is a
one-time step; every later invocation just calls `send` or `watch`.

### Step 6 — Android, via Termux

[Termux](https://termux.dev) is a terminal-emulator app that gives Android
a real Linux-like userland with its own package manager, no root needed.

```sh
pkg install tor
tor &                              # or set it up under termux-services for a
                                    # persistent daemon instead of a fresh
                                    # bootstrap every session
lanmsg-remote-cli enroll -server abcd...xyz.onion:8443 -name "Dad's phone (remote)"
lanmsg-remote-cli send -to "Mom's laptop" -text "hello from the road"
lanmsg-remote-cli watch
```

Build `lanmsg-remote-cli` for Android the same way as any other target —
no cgo anywhere in its dependency chain, so no NDK is needed:

```sh
CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -o lanmsg-remote-cli-android ./cmd/lanmsg-remote-cli
```

Copy that binary onto the phone (or run `pkg install golang` in Termux and
build it there directly). One tradeoff worth knowing: Termux only reliably
keeps *foreground* processes alive, so a freshly started `tor` means a
several-second circuit-bootstrap wait before the first send of a session.

### Tunnel-specific troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| Relay log never shows "backend authenticated" | Wrong `.onion` address, wrong `secret`, or Tor not running on the Pi | check `systemctl status tor` on the Pi; double check `cloud_onion_addr`/`secret` against what `lanmsg-tunnel setup` printed |
| `lanmsg-tunnel` log shows repeated backend auth failures | `secret` mismatch between the Pi's `server.toml` and the cloud box's `tunnel.toml` | re-copy the secret from the cloud box's `setup` output; it's never re-shown, so if lost, delete `tunnel.toml` and re-run `setup` (and update the Pi's copy) |
| Remote client's enrollment hangs on the fingerprint probe | Local Tor daemon on the remote machine isn't running, or its SOCKS port isn't `127.0.0.1:9050` | check `systemctl status tor` (or that Tor Browser is open); pass `-socks host:port` if it's different |
| Remote client connects but everything is slow | Expected — Tor circuits add real latency, typically hundreds of milliseconds to a couple of seconds per connection, more on a fresh circuit | not a bug; this path isn't trying to feel like LAN-speed chat |
