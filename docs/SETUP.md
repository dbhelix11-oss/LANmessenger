# Setup

Two parts: run the **relay** once on an always-on machine, then install the
**client** on each computer.

## 1. The relay

### Option A — from source

```sh
git clone <your repo> lanmessenger && cd lanmessenger
go build -o lanmsg-server ./cmd/lanmsg-server

# Interactive: prompts for the household passphrase.
./lanmsg-server setup -config ./server.toml -listen 0.0.0.0:8443

# Non-interactive:
./lanmsg-server setup -config ./server.toml -listen 0.0.0.0:8443 \
    -passphrase 'pick something the family shares'
```

`setup` writes `server.toml`, generates a self-signed TLS certificate under
`./data/`, and prints its **fingerprint**:

```
TLS certificate fingerprint (SHA-256) — every client confirms this on first connect:

  57:be:45:64:34:c8:f2:c2:4f:6f:7f:16:aa:7f:8f:f5:43:14:3c:0a:12:c3:c0:6b:63:ea:31:b6:46:c9:3b:46
```

Write that down — each client is asked to confirm it once.

Run it:

```sh
./lanmsg-server run -config ./server.toml
```

For a permanent install use the systemd unit or Docker files in
[`deploy/`](../deploy).

### Option B — Docker

```sh
cd deploy
docker compose run --rm relay setup -config /data/server.toml -data /data \
    -listen 0.0.0.0:8443 -passphrase 'family passphrase'
docker compose up -d
docker compose run --rm relay fingerprint -config /data/server.toml   # print it again
```

### Turning on admin approval

By default anyone with the passphrase who can reach the relay is admitted
automatically. To require that an existing trusted device approve each new one,
set this in `server.toml` and restart:

```toml
require_admin_approval = true
```

The **first** device to enroll is always admitted and becomes an admin, even
with this on. After that, new devices sit in a "pending" state until an admin
approves them from the client's **Pending devices** panel.

## 2. The client

### Desktop (Fyne GUI)

Build needs a C toolchain and a few system libraries (Fyne uses OpenGL):

| OS | Prerequisites |
|---|---|
| Debian/Ubuntu | `sudo apt install golang gcc pkg-config libgl1-mesa-dev xorg-dev libxkbcommon-dev` |
| Fedora | `sudo dnf install golang gcc pkgconf-pkg-config mesa-libGL-devel libX11-devel libXcursor-devel libXrandr-devel libXinerama-devel libXi-devel libxkbcommon-devel` |
| macOS | Xcode command-line tools (`xcode-select --install`) |
| Windows | a MinGW-w64 gcc (e.g. via MSYS2), or build with `fyne-cross` |

```sh
go build -o lanmsg ./cmd/lanmsg      # or: go run ./cmd/lanmsg
```

For distributable bundles (`.app`, `.exe`, Linux tarball) use
[`fyne`](https://docs.fyne.io/started/packaging):

```sh
go install fyne.io/tools/cmd/fyne@latest
fyne package -os darwin  -icon icon.png -name lanmessenger -sourceDir ./cmd/lanmsg
fyne package -os windows -icon icon.png -name lanmessenger -sourceDir ./cmd/lanmsg
fyne package -os linux   -icon icon.png -name lanmessenger -sourceDir ./cmd/lanmsg
```

### First run

1. Enter the relay address (`relay.lan:8443`).
2. The client shows the certificate fingerprint it received. Check it matches
   what `setup` printed, then continue.
3. Enter a display name and the household passphrase.
4. If the relay uses admin approval and this isn't the first device, you'll see
   "waiting for approval" until someone accepts it.

Config, keys and history are stored under your OS config directory
(`~/.config/lanmessenger` on Linux, `~/Library/Application Support/lanmessenger`
on macOS, `%AppData%\lanmessenger` on Windows). Override with `-config <dir>`.

### Verifying a contact

Each device has a short fingerprint (Settings → *This device's fingerprint*).
When a new person appears in your roster they show as *unverified*. Ask them to
read their fingerprint aloud, compare, and click **verify**. If someone's
fingerprint ever changes unexpectedly, the client warns you.
