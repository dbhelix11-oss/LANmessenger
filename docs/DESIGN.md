# lanmessenger — design & implementation, explained

This document explains **how lanmessenger is built and why**, written for someone
comfortable with C++ and a bit of Python but new to Go, and new to cryptography
beyond "I've generated an SSH key and used GPG once or twice."

It's long. Read it in order the first time; after that it's a reference. The
sections:

1. [The 10,000-foot view](#1-the-10000-foot-view)
2. [A Go primer for C++/Python programmers](#2-a-go-primer-for-cpython-programmers)
3. [A cryptography primer](#3-a-cryptography-primer)
4. [How the pieces fit: package by package](#4-how-the-pieces-fit-package-by-package)
5. [The wire protocol](#5-the-wire-protocol)
6. [A message's whole journey](#6-a-messages-whole-journey)
7. [Concurrency model](#7-concurrency-model)
8. [Data storage](#8-data-storage)
9. [Security model: what it does and does not protect](#9-security-model-what-it-does-and-does-not-protect)
10. [Design decisions and the roads not taken](#10-design-decisions-and-the-roads-not-taken)
11. [How to extend it](#11-how-to-extend-it)

---

## 1. The 10,000-foot view

The goal: family members on a home network message each other computer-to-computer,
with real encryption, working even when they're on different routers/subnets, with
presence ("available", "do not disturb", …) and offline delivery.

There are **three programs**, all compiled from one Go codebase:

| Program | What it is |
|---|---|
| `lanmsg-server` | The **relay**. One instance runs on an always-on machine. It shuttles encrypted blobs between clients and holds messages for clients that are asleep. It cannot read message contents. |
| `lanmsg` | The **desktop client** (a GUI). One per computer. |
| *(future)* web / mobile front-ends | Thin UIs over the same client engine. |

And **five library packages** they share (all under `internal/`, Go's convention
for "not importable from outside this module"):

```
internal/proto/        the wire format: what bytes go over the network
internal/crypto/       keys, encryption, signatures, password hashing
internal/store/        a thin SQLite helper used by both sides
internal/servercore/   all the relay's logic (so it can be unit-tested)
internal/clientcore/   all the client's logic, UI-agnostic (so a GUI, a web
                       app, or a test can drive it identically)
```

The single most important architectural idea: **the client's brain
(`clientcore`) is completely separate from the client's face (the Fyne GUI).**
`clientcore` exposes plain Go method calls (`SendText`, `SetStatus`,
`Roster`, …) and a stream of events (`EventMessage`, `EventPresence`, …). The GUI
is ~900 lines that call those methods and render those events. A web UI or a
phone app would reuse `clientcore` unchanged.

### The shape of the network

```
   Dad's PC ─────┐
                 ├──TLS──►  lanmsg-server  ◄──TLS──── Mom's iMac
   Kid's laptop ─┘          (the relay)
```

Every client makes **one outbound TLS connection** to the relay and keeps it
open. That's the whole trick for "works across routers": no client ever has to
*accept* a connection or be *found* by a broadcast; it just dials one address.
[NETWORK.md](NETWORK.md) covers where to place the relay so every device can
reach it.

### The two layers of encryption

1. **Client ↔ relay: TLS.** Standard transport encryption, the same kind your
   browser uses, so nobody on the Wi-Fi can read the connection.
2. **Client ↔ client: end-to-end (E2E).** The message text is *separately*
   encrypted with keys only the two chatting devices hold. The relay forwards a
   sealed blob it cannot open. TLS protects the pipe; E2E protects the payload
   even from whoever runs the pipe.

---

## 2. A Go primer for C++/Python programmers

Enough Go to read this codebase. Go is small on purpose — you can hold the whole
language in your head.

### 2.1 Packages, modules, imports

- A **package** is a directory of `.go` files sharing `package foo` at the top.
  It's the unit of compilation and of privacy.
- A **module** is a tree of packages with a `go.mod` file at the root naming it.
  Ours is `module lanmessenger`, so the package in `internal/crypto/` is
  imported as `"lanmessenger/internal/crypto"`.
- `go.mod` also pins dependency versions (like `package.json` + lockfile, or
  `Cargo.toml`). `go.sum` records their cryptographic hashes so a tampered
  download is detected.
- **Privacy is by capitalization, not keywords.** An identifier starting with an
  uppercase letter (`Seal`, `Client`, `DeviceID`) is exported — visible to other
  packages. Lowercase (`seal`, `conn`, `deviceID`) is package-private. There is
  no `public`/`private`/`friend`. There *is* `internal/`: any package under a
  directory named `internal/` can only be imported by code rooted at
  `internal/`'s parent. That's why our shared code lives there — third parties
  can't build against it, so we're free to change it.

### 2.2 No classes. Structs + methods + interfaces.

```go
type Client struct {          // like a C++ struct/class with only data members
    cfg   *Config
    store *clientStore
    mu    sync.RWMutex         // a mutex is just a field
}

// A "method" is a free function with a receiver in parentheses before the name.
func (c *Client) DeviceID() string {
    return c.cfg.DeviceID
}
```

- `(c *Client)` is the receiver — the equivalent of `this`, but explicit and
  named by you. Pointer receiver `*Client` = can mutate the struct (and avoids
  copying it). Value receiver `Client` = operates on a copy.
- **No inheritance.** Code reuse is by *embedding* (composition) and *interfaces*.
- **Interfaces are structural / duck-typed, but checked at compile time.** You
  never write "implements". If a type has the right methods, it satisfies the
  interface:

  ```go
  type Reader interface { Read(p []byte) (int, error) }
  // Any type with that exact method IS a Reader. No declaration needed.
  ```

  We use this for `io.Reader`/`io.Writer` (hashing a file streams through them)
  and for the standard library's `net.Listener`.

### 2.3 Errors are values, not exceptions

There is no `try/catch`. Functions that can fail return an `error` as their last
result. `nil` means success.

```go
f, err := os.Open(path)
if err != nil {
    return fmt.Errorf("clientcore: open for hashing: %w", err)  // wrap and bubble up
}
defer f.Close()
```

- The `if err != nil { return ... }` dance is everywhere. It's verbose but makes
  every failure path visible — no invisible stack unwinding.
- `fmt.Errorf("...: %w", err)` **wraps** an error, preserving a chain. Callers
  can test the chain with `errors.Is(err, ErrNotReady)` (sentinel match) or
  `errors.As(err, &relayErr)` (type match). You'll see both.
- `panic` exists but is reserved for "this should be impossible" bugs, not
  ordinary errors.

### 2.4 `defer`

`defer f()` schedules `f()` to run when the *current function* returns, no matter
how. It's Go's RAII substitute — the cleanup sits right next to the acquisition:

```go
mu.Lock()
defer mu.Unlock()      // released on every return path below
// ... do work under the lock ...
```

Multiple `defer`s run last-in-first-out.

### 2.5 Goroutines and channels

- `go f(x)` starts `f(x)` running concurrently. A **goroutine** is like a thread
  but cheap (a few KB of stack, scheduled by the Go runtime onto real OS
  threads). Spawning thousands is normal. The relay runs one goroutine per
  connected client plus one heartbeat goroutine each; that's fine.
- A **channel** `chan T` is a typed, thread-safe queue for passing values
  between goroutines. `ch <- v` sends, `v := <-ch` receives, both block until the
  other side is ready (unless the channel is buffered).

  ```go
  events chan Event          // created as make(chan Event, 256): a 256-slot buffer

  select {                   // wait on several channel operations at once
  case <-ctx.Done():         // context was cancelled -> time to stop
      return
  case ev := <-c.client.Events():
      handle(ev)
  }
  ```

- Go's motto: **"share memory by communicating"** — prefer handing data to
  another goroutine over a channel rather than sharing a variable behind a lock.
  We use both: channels for the event stream and connection lifecycle, a
  `sync.RWMutex` for the few maps that genuinely need shared random access
  (the connection registry, the roster-presence cache).

### 2.6 `context.Context`

Almost every function that does I/O takes `ctx context.Context` as its first
argument. A context carries:

- **cancellation**: when `ctx` is cancelled (parent said stop, or a timeout
  fired), blocked operations return promptly with `ctx.Err()`.
- **deadlines**: `context.WithTimeout(parent, 10*time.Second)`.

It's how you get "stop everything cleanly on Ctrl-C" without global flags. In
`main` we make a context that's cancelled on SIGINT; it flows down into the
server loop, every connection, every read.

### 2.7 Slices, maps, zero values, `struct{}` tags

- `[]T` is a slice: a view (pointer, length, capacity) onto a backing array,
  like `std::span` that can also grow via `append`.
- `map[K]V` is a hash map. Reading a missing key returns the **zero value**
  (`0`, `""`, `nil`, or an all-zero struct), plus an optional `, ok` boolean:
  `v, ok := m[k]`.
- Every type has a usable **zero value**. `var c Config` is a ready-to-use empty
  `Config`; `sync.Mutex{}` is an unlocked mutex. We lean on this — e.g. a
  freshly loaded config with no server address is just "not configured yet",
  not an error.
- Struct field **tags** are metadata strings read by libraries via reflection:

  ```go
  type Msg struct {
      To         string `json:"to"`
      Ciphertext string `json:"ciphertext"`
      TS         int64  `json:"ts"`
  }
  ```

  The `json:"to"` tells `encoding/json` what key to use. We also use
  `toml:"..."` for the server config file.

### 2.8 The toolchain

- `go build ./...` — compile everything (`./...` = "this dir and all
  subdirs"). Output is a single static binary; no runtime to install on the
  target.
- `go test ./...` — run every `*_test.go`. Tests are just functions
  `func TestX(t *testing.T)` in the same package (white-box) or `package foo_test`
  (black-box, only the public API). We have both.
- `go test -race ./...` — instrument the binary to **detect data races** at
  runtime. We keep this green; it's the main reason to trust the concurrency.
- `go vet ./...` — static checks for likely mistakes (bad `Printf` verbs,
  unreachable code, lock copying).
- `gofmt` — canonical formatter. There is one true style; nobody argues about
  it. CI rejects unformatted code.
- Cross-compiling is a one-liner: `GOOS=darwin GOARCH=arm64 go build ...`
  produces a Mac binary from Linux, as long as the code (or its deps) doesn't
  need C. Ours doesn't, for the server and client engine — see 2.9.

### 2.9 cgo, and why the SQLite choice matters

Go can call C ("cgo"), but doing so forfeits easy cross-compilation and the
pure-static binary. We deliberately avoid it in the engine:

- SQLite is normally a C library. We use `modernc.org/sqlite`, which is SQLite
  **transpiled to pure Go**. Slower than the C one, irrelevant at family scale,
  and it cross-compiles cleanly.
- The **GUI** (`cmd/lanmsg`) is the one exception: the Fyne toolkit uses OpenGL
  via cgo, so building *it* needs a C compiler and some system `-dev` packages
  (listed in [SETUP.md](SETUP.md)). The server and `clientcore` never do.

---

## 3. A cryptography primer

Built from the pieces you already know. Each subsection ends with **"where this
shows up"** pointing at the code.

### 3.1 Hash functions

A hash function (we use **SHA-256**) eats any amount of data and produces a
fixed 256-bit (32-byte) digest. Properties that matter:

- **Deterministic**: same input → same digest, always.
- **One-way**: given a digest you can't feasibly find an input that produces it.
- **Collision-resistant**: you can't feasibly find two inputs with the same
  digest.
- **Avalanche**: flip one input bit and ~half the output bits flip. Digests of
  similar inputs look unrelated.

A hash is *not* encryption — there's no key and nothing to "undo". It's a
fingerprint of data.

**Where this shows up:**
- **Device IDs.** `DeviceID = hex(SHA-256(signing-public-key)[:16])`. A stable,
  collision-resistant name for a device, derived from its key. You can't pick
  your own ID or steal someone's without their key.
- **Key fingerprints.** `Fingerprint = first 8 bytes of SHA-256(signPub ‖ boxPub)`,
  shown as `1a2b-3c4d-5e6f-7a8b`. Short enough to read aloud, long enough that
  forging a key with the same fingerprint is infeasible. This is exactly what
  GPG "key fingerprints" and SSH's `SHA256:…` host-key display are.
- **File integrity.** The sender hashes the whole file; the receiver hashes what
  it reassembled and checks they match before accepting it.
- Inside TLS, HMAC, signatures, Argon2 — hashing is a building block everywhere.

### 3.2 HMAC — a keyed hash (proof you know a secret)

An **HMAC** combines a hash with a secret key: `HMAC(key, message)`. Anyone with
the key can compute it; nobody without the key can. It's used to prove "I know
the secret" or "this message wasn't altered by someone lacking the key".

**Where this shows up: the household passphrase.** We never send the passphrase
to the relay, not even encrypted. Instead:

1. The relay stores, once, a *verifier*: a random `salt` plus
   `K = Argon2id(passphrase, salt)` (see 3.3). Not the passphrase itself.
2. On each connection the relay sends a fresh random `nonce`.
3. The client computes `K` from the passphrase it was given, then sends
   `proof = HMAC(K, nonce)`.
4. The relay computes the same HMAC and compares (in constant time, so timing
   doesn't leak). Match ⇒ the client knows the passphrase.

Because the `nonce` is fresh every time, a recorded `proof` can't be replayed.
This is a **challenge–response** scheme, the same family as SSH key auth (where
the "secret" is your private key and the response is a signature).

### 3.3 Argon2id — turning a human password into a key

Passphrases are low-entropy; an attacker who steals the verifier could try
millions of guesses per second against a plain hash. **Argon2id** is a
*deliberately slow, memory-hungry* key-derivation function. We configure it to
use 64 MiB of RAM and a pass of mixing per attempt, so a brute-forcer needs 64
MiB and meaningful time *per guess*. The random per-install `salt` ensures two
households with the same passphrase get different verifiers and that precomputed
"rainbow tables" are useless.

**Where this shows up:** `internal/crypto/passphrase.go`, `DeriveKey`.

### 3.4 Symmetric vs. asymmetric encryption

- **Symmetric**: one shared secret key encrypts and decrypts. Fast. Problem: how
  do two parties get the same key without meeting?
- **Asymmetric (public-key)**: a *keypair* — a public key you hand out, a private
  key you guard. This is the GPG / SSH model you know. Two big uses:
  - **Encrypt to someone**: anything encrypted with their *public* key can only
    be decrypted with their *private* key.
  - **Sign something**: a signature made with your *private* key can be verified
    by anyone with your *public* key, proving you (and only you) produced it and
    the content is unaltered.

Real systems combine them: use asymmetric crypto once to agree on a symmetric
key, then use fast symmetric crypto for the actual data. That combined step for
"encrypt a message to a specific person" is what we use for E2E.

### 3.5 The two keypairs each device has

When a client first runs, `internal/crypto/identity.go` generates **two**
keypairs and stores them in `identity.json` (file permissions `0600`, i.e.
owner-only):

| Keypair | Algorithm | Purpose |
|---|---|---|
| **signing** | Ed25519 | signatures — proves "this device said X". Used for admin approve/deny, and it's what the device ID is derived from. |
| **box** | X25519 | encryption — used to seal/open E2E messages. |

Why two? Signing and encryption are different jobs with different key types
(Ed25519 for signatures, X25519 for Diffie–Hellman key agreement). Keeping them
separate is standard practice and means rotating one doesn't force the other.

The **public** halves of both are published to the relay's directory so other
devices can look them up. The **private** halves never leave the machine.

### 3.6 Sealing a message: NaCl `box`

To send Mom a message, Dad's client calls `crypto.Seal(plaintext,
DadBoxPrivate, MomBoxPublic)`. Under the hood this is **NaCl `box`**
(`golang.org/x/crypto/nacl/box`), which does three things in one step:

1. **X25519 Diffie–Hellman**: combine Dad's box *private* key with Mom's box
   *public* key to derive a shared secret. (Mom, using her private key and Dad's
   public key, derives the identical secret — that's the DH magic.)
2. **XSalsa20**: a fast stream cipher, keyed by that shared secret, encrypts the
   plaintext. A fresh random 24-byte **nonce** ("number used once") makes each
   encryption unique even for identical plaintexts.
3. **Poly1305**: computes an authentication tag over the ciphertext.

The result Dad sends is `(nonce, ciphertext+tag)`. Mom's `crypto.Open` reverses
it and **fails loudly** if even one bit was flipped, or if it wasn't really from
Dad's key. This property — you can't decrypt without also verifying authenticity
— is called **AEAD** (authenticated encryption). It's why a corrupted or forged
message is rejected rather than silently mis-decrypted.

Note what the relay sees: `{from: Dad, to: Mom, nonce: …, ciphertext: …}`. The
`nonce` and `ciphertext` are opaque. The relay has no private key for anyone and
cannot run step 1.

**Where this shows up:** `internal/crypto/seal.go`, and
`internal/clientcore/crypto.go` which wraps a JSON payload (`Inner`) and seals
it.

### 3.7 What travels *inside* the sealed blob

The plaintext we seal is itself a small JSON object, an `Inner`:

```json
{ "kind": "text", "data": { "text": "dinner's ready" } }
```

`kind` can also be `file_offer` (a file's name/size/hash), `file_chunk` (512 KiB
of a file, base64'd), or `receipt`. So **file transfers reuse the exact same
sealed-message path as chat** — the relay can't even tell a file chunk from a
text message except by size.

### 3.8 Signatures for admin actions

Approving a pending device is an authority decision, so it's signed. The admin's
client computes `Ed25519-Sign(adminSigningPrivate, "approve:" + deviceID)` and
sends that signature. The relay verifies it against the admin's signing *public*
key (which it has in its directory) and against its list of admin device IDs.
Even though the admin is already authenticated by the passphrase, the signature
means the relay's logs contain cryptographic proof of *which* device authorized
each approval.

**Where this shows up:** `internal/crypto/sign.go`,
`servercore/handlers.go:handleAdminAction`.

### 3.9 TLS and certificate pinning

TLS gives the client↔relay pipe confidentiality and integrity. Normally TLS
trust comes from Certificate Authorities (the ~150 organizations your OS trusts
to vouch for `example.com`). On a home LAN there's no CA and no public domain
name, so the relay makes a **self-signed certificate** on first run — it vouches
for itself.

A self-signed cert alone proves nothing (anyone can make one). So we use
**trust on first use (TOFU)**, the same model as SSH's `known_hosts`:

1. `setup` prints the certificate's SHA-256 **fingerprint**.
2. On first connect the client shows the fingerprint it received and asks the
   user to confirm it matches.
3. The confirmed fingerprint is **pinned** — saved in the client config. On
   every later connection the client checks the relay's certificate hashes to
   exactly that value (`clientcore/conn.go:pinnedTLSConfig`, via TLS's
   `VerifyPeerCertificate` hook). If it ever differs — someone interposing a
   fake relay — the connection is refused.

The one-time human comparison is the trust anchor. If the user rubber-stamps it
without checking, that anchor is weak; hence the UI nudges them to compare it to
what `setup` printed.

### 3.10 Putting the crypto together

```
Dad types "hi"
  │
  ├─ Inner{kind:text, data:{text:"hi"}}                 (plain JSON)
  ├─ crypto.Seal(that, DadBoxPriv, MomBoxPub)           → nonce, ciphertext   [E2E]
  ├─ Msg{to:Mom, from:"", nonce, ciphertext} as JSON
  ├─ sent over the WebSocket, which is inside TLS       → wire bytes          [transport]
  ▼
Relay: sees Msg{from:Dad(stamped), to:Mom, nonce, ciphertext}. Routes or queues it.
  ▼
Mom: TLS decrypts the pipe → Msg → crypto.Open(nonce, ciphertext, DadBoxPub, MomBoxPriv)
     → Inner → "hi". Any tampering anywhere fails the Open.
```

---

## 4. How the pieces fit: package by package

### `internal/proto` — the vocabulary

Defines every message that can go over the wire and nothing else (no logic).
Everything is a JSON **`Envelope`**:

```go
type Envelope struct {
    V    int             `json:"v"`     // protocol version, currently 1
    Type Type            `json:"type"`  // "hello", "msg", "presence_update", …
    ID   string          `json:"id"`    // correlation id for request/reply pairs
    TS   int64           `json:"ts"`    // sender's clock, unix milliseconds
    Data json.RawMessage `json:"data"`  // the type-specific payload, still-raw JSON
}
```

`json.RawMessage` is "JSON bytes I'll parse later" — the reader first looks at
`Type`, then unmarshals `Data` into the matching struct (`Msg`, `PresenceSet`,
…). This keeps the envelope one type instead of a tagged union.

Why JSON and not Protocol Buffers / MessagePack? At family message volumes the
size and speed difference is irrelevant, and JSON is trivially inspectable when
debugging ("just print the frame"). The `V` field leaves the door open to switch
later.

### `internal/crypto` — all the math, wrapped in a friendly API

Covered in §3. The public surface is deliberately tiny:
`GenerateIdentity`/`LoadOrCreateIdentity`, `Seal`/`Open`, `Fingerprint`,
`DeriveKey`/`Proof`/`VerifyProof`, `Sign`/`Verify`. Callers never touch the
underlying primitives directly, so if we ever change algorithm the blast radius
is one package.

### `internal/store` — SQLite with sane defaults

~90 lines. `Open(path)` opens a database with the pragmas we always want
(write-ahead logging for concurrent readers, foreign keys enforced, a busy
timeout so brief lock contention retries instead of erroring). `Migrate(db,
migrations)` runs a list of named SQL steps once each, recording which have run
in a `schema_migrations` table — a minimal forward-only migration system so the
schema can evolve without a library.

### `internal/servercore` — the relay

| File | Responsibility |
|---|---|
| `config.go` | Load/save `server.toml`; defaults; the admin device list. |
| `tls.go` | Generate/load the self-signed cert; compute its fingerprint. |
| `store.go` | SQLite schema + queries: `devices`, `presence`, `queue`. |
| `server.go` | The `Server` type, the HTTPS listener, the connection registry (a `map[deviceID]*conn` behind an `RWMutex`), broadcast helpers, the hourly queue-purge loop. |
| `conn.go` | Per-connection lifecycle: WebSocket upgrade, the auth handshake state machine, the read loop, the heartbeat. |
| `enroll.go` | New-device enrollment and returning-device resume; "first device becomes admin"; promoting a pending device when an admin approves. |
| `handlers.go` | One function per inbound frame type: presence, message relay + offline queue, acks, admin list/approve/deny. |
| `setup.go` | The `setup` subcommand's guts. |

The relay is intentionally dumb: it authenticates you, tells you who else exists,
carries opaque blobs, and remembers blobs for absent recipients. It has no
notion of "conversations" or message content.

### `internal/clientcore` — the client engine

| File | Responsibility |
|---|---|
| `config.go` | `config.json`: relay address, pinned fingerprint, display name, this device's assigned ID, downloads folder. |
| `secret.go` | Stores the household passphrase locally (`secret.json`, `0600`) so the app reconnects after a restart without re-prompting — the same trade-off as your Wi-Fi password being saved. |
| `store.go` | SQLite schema + queries: `peers` (roster + per-peer trust state), `messages` (history), `outbox` (messages composed while offline). |
| `conn.go` | Dialing with certificate pinning; the client side of the handshake; `FingerprintOfPresentedCert` for the first-run confirmation screen. |
| `client.go` | The `Client` type; `Enroll`; `Start`/`Stop`; the reconnect loop with exponential backoff; the read loop. |
| `dispatch.go` | Handling each inbound frame: decrypt messages, apply directory/presence updates, correlate admin replies. |
| `crypto.go` | Seal/open an `Inner` for a given peer; compute fingerprints. |
| `files.go` | Send a file (hash, offer, chunk, seal each chunk); receive one (reassemble to `<name>.part`, verify hash, rename). |
| `api.go` | The methods a UI calls: `Roster`, `History`, `SendText`, `SetStatus`, `MarkVerified`, `ListPending`, `Approve`/`Deny`, plus `flushOutbox`. |
| `events.go` | The `Event` type and the buffered channel the UI consumes. |

Key idea repeated: **no UI code here, and no blocking calls that assume a UI.**
The engine runs its own goroutines, does its own reconnection, and just emits
events. `integration_test.go` drives it exactly as the GUI would, which is why
the tests are meaningful.

### `cmd/lanmsg-server` and `cmd/lanmsg`

Thin `main` packages. The server's is argument parsing + `setup`/`run`/
`fingerprint` subcommands. The client's builds the Fyne window, wires widgets to
`clientcore` calls, and runs a goroutine that pumps `clientcore` events onto the
UI thread (`fyne.Do`). See §7 for why that last part matters.

---

## 5. The wire protocol

All frames are `Envelope` JSON objects sent as WebSocket text messages over TLS.

### Connderehandshake (client → relay unless noted)

```
→ hello            { client_version, device_id? }      device_id omitted on first ever run
← auth_challenge   { nonce, salt }                     relay, fresh nonce each time
→ auth_response    { device_id?, proof }               proof = HMAC(Argon2id(passphrase,salt), nonce)
→ enroll           { display_name, sign_pub, box_pub } first run only
← enroll_result    { device_id, state }                state = "active" | "pending"
← ready            { device_id, admin }                sent once the device is active
← directory_snapshot { entries:[…] }                   everyone else who's active
← presence_update  { device_id, status, online, … }    one per currently-connected peer
← msg              { … }                               any messages queued while offline
```

If `require_admin_approval` is on and this isn't the first device, the relay
sends `enroll_result{state:"pending"}` and then *nothing more* until an admin
approves — at which point it sends `ready` + the snapshot on the same
connection.

### Steady state

| Frame | Direction | Meaning |
|---|---|---|
| `presence_set` | client → relay | "my status is now Away" |
| `presence_update` | relay → clients | someone's status/online changed |
| `directory_update` | relay → clients | a device was added/changed/removed |
| `msg` | both | one sealed end-to-end message (text, or a file offer/chunk) |
| `msg_ack` | both | recipient got `msg N`; relay drops it from the queue and forwards the ack to the sender |
| `admin_list_pending` / `admin_pending_list` | client ⇄ relay | request/reply, correlated by `Envelope.ID` |
| `admin_approve` / `admin_deny` | client → relay | signed decision about a pending device |
| `ping` / `pong` | both | app-level liveness (plus WebSocket's own ping/pong for the heartbeat) |
| `error` | relay → client | `{ code, message }` |

### The `msg` frame and what's inside it

```json
{ "v":1, "type":"msg", "data": {
    "from": "",                       // relay fills this in; clients never set it
    "to": "9f8e…",
    "msg_id": "b1946ac9…",            // client-generated, unique per sender
    "nonce": "base64-24-bytes",
    "ciphertext": "base64…",          // seal(Inner)
    "ts": 1724900000000
}}
```

Decrypting `ciphertext` yields an `Inner`:

```json
{ "kind": "text",       "data": { "text": "…" } }
{ "kind": "file_offer", "data": { "transfer_id":"…","name":"cat.jpg","size":91234,
                                  "sha256":"…","chunk_size":524288,"chunks":1 } }
{ "kind": "file_chunk", "data": { "transfer_id":"…","index":0,"bytes":"base64…" } }
{ "kind": "receipt",    "data": { "msg_id":"…","state":"delivered" } }
```

The relay authenticates the sender and rewrites `from` to the connection's
device ID, so you can't spoof messages as someone else.

---

## 6. A message's whole journey

Dad, on the OpenWRT subnet, sends Mom "on my way" while Mom's iMac is asleep.

1. **Compose.** GUI calls `client.SendText(ctx, momID, "on my way")`.
2. **Look up Mom.** `clientcore` reads Mom's `box_pub` from its local `peers`
   table (populated from the relay's directory).
3. **Seal.** `Inner{kind:text,…}` → JSON → `crypto.Seal(_, dadBoxPriv,
   momBoxPub)` → `(nonce, ciphertext)`.
4. **Record + try to send.** A row goes into the local `messages` table with
   state `queued`. If the relay connection is up, a `msg` frame is written and
   the state flips to `sent`; if not, the sealed bytes also go into the local
   `outbox` table for a later retry. The GUI shows the message immediately with
   a "·" (sent) or "…" (queued) mark, because `SendText` emits `EventMessage`
   for your own message too.
5. **Relay receives it.** Over Dad's TLS WebSocket. The relay stamps
   `from = Dad`, sees Mom has no live connection, and inserts a row into its
   `queue` table (respecting a per-recipient cap and a retention window).
6. **Mom wakes up.** Her client reconnects (the reconnect loop has been retrying
   with backoff), does the `hello`/`auth` handshake with her stored `device_id`,
   gets `ready` + the directory snapshot, and then the relay **drains the
   queue**: it sends the stored `msg` frame and deletes it after Mom's client
   acks.
7. **Mom's client opens it.** `crypto.Open(nonce, ciphertext, dadBoxPub,
   momBoxPriv)` → `Inner` → text. A `messages` row is stored (`received`), and
   `EventMessage` fires. If Mom's window isn't focused and she's not on DND, a
   desktop notification pops.
8. **Ack round-trip.** Mom's client sends `msg_ack{msg_id, peer:Dad}`. The relay
   deletes the queue row (already gone here) and forwards the ack to Dad, whose
   client flips the stored message to `delivered` and the GUI mark to "✓".

If Dad had been fully offline when he composed (step 4, no connection at all),
step 5 happens later: on Dad's next successful connect, `flushOutbox` walks the
`outbox` table oldest-first and sends each frame.

---

## 7. Concurrency model

### Relay

- **One goroutine per connection**, running `conn.readLoop` — a plain
  `for { read frame; dispatch }`. Blocking reads are fine because each
  connection has its own goroutine.
- **One heartbeat goroutine per connection**: every 30 s it sends a WebSocket
  ping with a 10 s deadline; a miss cancels that connection's context, which
  unblocks its read loop and tears it down.
- **Shared state** is the connection registry (`map[deviceID]*conn`) and each
  `conn`'s presence fields. Guarded by a `sync.RWMutex`. Writes to a client
  socket are serialized by a per-connection mutex so two goroutines
  (e.g. a relayed message and a presence broadcast) can't interleave bytes.
- **Cancellation** flows from `main`'s signal-aware context → the HTTP server →
  each request's context → each connection.

### Client engine

- **The run loop** (`Client.runLoop`) owns connection lifecycle: dial, handshake,
  serve, and on any error, back off (1 s, 2 s, 4 s … capped at 30 s) and
  redial — unless the failure was fatal (bad passphrase, denied), in which case
  it stops and emits an error.
- **The read loop** dispatches inbound frames. Decrypting, DB writes, and event
  ema all happen here, on this one goroutine, so `clientcore`'s own state needs
  little locking; the `RWMutex` mainly guards the presence cache and the
  "current connection" pointer that API calls read.
- **Request/reply** (only `admin_list_pending` today) is correlated by
  `Envelope.ID`: the caller registers a channel under a random ID, sends the
  request, and `select`s on that channel with a timeout.
- **The event channel** is buffered (256) and **lossy on overflow** — if a UI
  stalls, transient events (a presence blip) may be dropped, but the UI can
  always re-read authoritative state via `Roster()` / `History()`. Message
  storage is in SQLite before the event fires, so a dropped `EventMessage`
  never means a lost message.

### GUI thread rule

Fyne, like most GUI toolkits, requires all widget mutation on the main
goroutine. `clientcore` events arrive on a background goroutine, so the pump
does:

```go
for ev := range client.Events() {
    ev := ev
    fyne.Do(func() { g.handleEvent(ev) })   // marshals onto the UI goroutine
}
```

`fyne.Do` is the equivalent of Qt's `QMetaObject::invokeMethod(..., QueuedConnection)`
or JavaScript's `queueMicrotask`.

---

## 8. Data storage

Everything is SQLite files; no server database process.

### Relay: `<data>/server.db`

| Table | Rows |
|---|---|
| `devices` | one per enrolled device: display name, both public keys, `state` (active/pending/denied), `admin` flag. |
| `presence` | last-known status per device (so a reconnecting peer's status isn't "unknown"). |
| `queue` | messages awaiting an offline recipient: `recipient, sender, msg_id, nonce, ciphertext, ts`. Deleted on ack; a background loop also purges anything older than the retention window; a per-recipient cap drops the oldest when exceeded. |

Plus `<data>/server.crt` and `server.key` (the pinned TLS identity), and
`server.toml`.

### Client: `<config dir>/lanmessenger/`

| File | Contents |
|---|---|
| `config.json` | relay address, pinned cert fingerprint, display name, this device's ID, downloads dir. |
| `identity.json` (`0600`) | this device's Ed25519 + X25519 private keys. |
| `secret.json` (`0600`) | the household passphrase. |
| `client.db` | `peers` (cached directory + `verified`/`key_changed` per peer), `messages` (full history; file transfers stored as a JSON metadata row), `outbox` (sealed messages composed offline). |

Received files land in the downloads dir (`~/Downloads/lanmessenger` by
default), written first as `name.part` and renamed only after the SHA-256 check
passes.

---

## 9. Security model: what it does and does not protect

### Threats it addresses

| Threat | Mitigation |
|---|---|
| Someone sniffing Wi-Fi / the wire | TLS on every client↔relay link. |
| The relay operator (or a relay compromise) reading messages | End-to-end `nacl/box`; the relay has no one's private key. Message text, file contents and file names are all inside the sealed payload. |
| A stranger on the LAN joining | Household passphrase, proven by challenge–response; never sent in the clear. |
| A guessing attack on a stolen verifier | Argon2id (64 MiB, salted) makes each guess expensive. |
| Impersonating another device | Device IDs are derived from the signing key; the relay stamps `from` from the authenticated connection; you can't send as someone else. |
| A malicious "relay" in the middle on first connect | TOFU fingerprint confirmation, then strict cert pinning. |
| Unwanted people who *do* learn the passphrase later | Optional `require_admin_approval`: new devices are quarantined until an existing trusted device signs an approval. |
| Tampered / corrupted messages or files | AEAD (Poly1305) rejects any altered ciphertext; whole-file SHA-256 is checked before a download is accepted. |
| A denied/removed device | Server refuses it at handshake; if connected, it's disconnected. |

### Limits — be honest about these

- **Metadata is visible to the relay.** It knows who is online, who exchanges
  messages with whom, when, and roughly how big each message/file is. It just
  can't read contents. If you don't trust the machine running the relay at all,
  this design isn't enough.
- **No forward secrecy (yet).** Each device has *one long-lived* box key. If an
  attacker records ciphertext today and later steals a device's `identity.json`,
  they can decrypt those past messages. A future upgrade to a Signal-style
  double ratchet (ephemeral keys that rotate per message) would fix this; it was
  deliberately deferred as overkill for v1. The mitigation today is that
  `identity.json` is `0600` and never leaves the machine.
- **Endpoint trust.** All of this assumes the computers themselves aren't
  compromised. Malware on Dad's PC with read access to his home directory has
  his keys and his history. No messenger can fix a compromised endpoint.
- **TOFU is only as strong as the human.** If nobody actually compares the
  fingerprint on first connect, a first-connection impersonator could slip in.
  Every later connection is safe once the right fingerprint is pinned.
- **The passphrase is stored on each client** (`secret.json`, `0600`). That's a
  convenience/security trade-off matching "your laptop remembers the Wi-Fi
  password." Someone with read access to that file can authenticate to the relay
  (but still not read past messages — that needs the box key — nor send as you
  without your signing key).
- **The relay can drop or delay messages**, or lie about presence. It can't
  forge message *content* (that would fail the recipient's `Open`) or forge
  admin approvals (those are signed), but availability depends on it behaving.
- **Compromised admin device** ⇒ attacker can approve rogue devices. Keep the
  admin device list short.

---

## 10. Design decisions and the roads not taken

- **Central relay vs. pure peer-to-peer.** P2P would avoid the metadata
  exposure, but LAN P2P needs each client to be reachable (listening sockets,
  NAT traversal between subnets, discovery that crosses routers) — exactly the
  fragile parts we're trying to avoid. The relay turns "be reachable" into "make
  one outbound connection," which is what makes the cross-router requirement
  tractable. E2E encryption claws back most of what P2P would have given us.
- **One long-lived key vs. a ratchet.** A double ratchet (per-message forward
  secrecy) is the gold standard but a lot of moving parts (session state,
  out-of-order handling, key rotation). For a home tool, `nacl/box` with a
  static key is simple, well-understood, and easy to audit. Left as a clean
  future upgrade behind the `Seal`/`Open` API.
- **Per-device identity vs. per-person.** Each device is its own roster entry
  ("Dad's laptop", "Dad's desktop"). Per-person identity spanning devices means
  multi-device key management and message fan-out — deferred.
- **JSON vs. a binary protocol.** Chosen for debuggability; the `V` field allows
  a later switch. Volumes don't justify the complexity now.
- **`modernc.org/sqlite` (pure Go) vs. the C library.** Slower, but keeps the
  server and engine cgo-free: trivial cross-compilation, static binaries, no
  build prerequisites. The GUI needs cgo anyway (OpenGL), so that trade only
  costs us on the desktop client, where startup speed is irrelevant.
- **Fyne vs. a web UI (Wails) vs. terminal.** Fyne is one Go codebase for
  macOS/Linux/Windows with a real mobile path later, and a native system tray.
  The engine/GUI split means a web or TUI front-end can be added without
  touching `clientcore`.
- **Passphrase auth vs. per-device invite tokens vs. certificates.** A shared
  household secret is the least friction for the actual users (a family), and
  the optional admin-approval gate covers "the secret leaked."

---

## 11. How to extend it

The seams are designed for these:

- **A new message type** (reactions, edits, voice notes): add an `InnerKind` and
  payload struct in `proto`, seal/handle it in `clientcore` (`SendText` is the
  template; `files.go` shows a multi-frame flow). The relay needs *no* changes —
  it already forwards any `msg`.
- **A web client**: build a small server that embeds `clientcore` and exposes
  its methods/events over a local WebSocket to a browser UI. Nothing in
  `clientcore` assumes Fyne.
- **Group chat**: the bigger job. Either fan-out at the sender (seal once per
  member) or introduce group keys. New `proto` types, new `clientcore` state,
  relay stays a dumb forwarder or gains a "room membership" concept.
- **Forward secrecy**: replace the body of `crypto.Seal`/`Open` and add per-peer
  session state in `clientcore`'s `peers` table. The call sites don't move.
- **Push/mobile**: `clientcore` already survives disconnection and reconnects;
  a mobile shell would drive it the same way the GUI does, plus platform
  background-execution glue.
- **Auto-away on idle**: watch OS idle time in the GUI and call
  `client.SetStatus(Away)` / restore on activity. Kept out of `clientcore`
  because "idle" is a UI-platform concept.

Run the tests (`go test ./...`) after any change to the engine or relay — the
integration tests in `internal/clientcore/integration_test.go` exercise a real
relay end to end (enroll, message, offline queue, file transfer, admin
approval, cert pinning) and will catch protocol breakage fast.
