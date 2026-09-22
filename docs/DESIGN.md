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

### Window, tray, and notifications

- **Close and minimize both hide to the tray.** Fyne only exposes
  `SetCloseIntercept`, so the close button is caught directly; minimize has no
  Fyne hook, so on X11 a second, read-only X connection
  (`cmd/lanmsg/traywatch_linux.go`) watches this window's `WM_STATE` /
  `_NET_WM_STATE` and calls `win.Hide()` when the WM iconifies it. It is
  best-effort — no X display, no tray, or window-not-found ⇒ minimize keeps its
  default behaviour. macOS/Windows keep native minimize (the dock/taskbar still
  holds the app); a per-OS hook is future work. The tray's "Show lanmessenger"
  item is the way back; `trayHidden` (atomic) keeps the two paths in sync.
- **Notification lifetime.** Fyne 2.8's Linux `SendNotification` calls the
  freedesktop `Notify` method with `expire_timeout = 0`, which the spec defines
  as *never expire* — a minimal X11 notifier then leaves the popup on screen with
  no dismiss button. `cmd/lanmsg/notify_linux.go` issues the same D-Bus call with
  a real timeout (6 s) and falls back to Fyne if the session bus is unreachable.
  macOS/Windows use Fyne's native path unchanged.

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
| A fully compromised cloud tunnel box (§13) | It never terminates the app's TLS, never holds a private key, database, or the household passphrase — a compromise leaks connection metadata (timing, byte counts) only, exactly like a relay compromise, one layer further removed. |

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

---

## 12. v2: paging and client updates

Two features the architecture was designed for ahead of time. §12.2 (client
updates) is now built; §12.1 (paging) still isn't — this section originally
fixed both their shapes before there was code to argue with, and §12.2 now
also records what actually shipped against that original shape.

### 12.1 Paging — a high-priority "get back here" alert

**What it is.** From a roster entry (right-click, or a bell button in the
conversation header) you send a *page*: a short message that, on the recipient's
machine, opens a window above everything else, plays a sound, and does not
dismiss until they click `OK` (or `Yes` / `No`). It deliberately punches through
`away` / `busy` / `do-not-disturb`. The use case is "I have been typing to you
for five minutes — look at your screen."

**It needs no new relay behaviour.** A page is just another kind of
end-to-end-sealed `Inner` payload (§3.7), so the relay forwards it exactly like a
text message and still cannot read it. Two new `InnerKind`s:

```
page      -> PageBody{ Text string; Mode string; SentAt int64 }   // Mode: "ok" | "yesno"
page_ack  -> PageAckBody{ PageID string; Response string }         // "ok" | "yes" | "no"
```

The sending client seals a `page` the way `SendText` seals a `text` (§6). The
receiving client, in its dispatch switch (`internal/clientcore/dispatch.go`),
routes `page` to a new handler that emits a dedicated event; the GUI turns that
event into the alert window and, when the user answers, seals a `page_ack` back.
The original sender then shows "✓ acknowledged" or "Dad: No" in the transcript.
Pages are stored in history like any message (`kind = page`).

**The alert window.** Fyne can create a borderless, always-on-top window and ask
the window manager for focus. That is enough on an ordinary desktop. What Fyne
cannot portably do is force itself above a *fullscreen* application (a game, a
screen-share, a slideshow). That last mile is per-OS:

| OS | Mechanism |
|---|---|
| macOS | raise the `NSWindow` level to `kCGScreenSaverWindowLevel` or higher (Objective-C via cgo) |
| Windows | `SetWindowPos(HWND_TOPMOST)` + `SetForegroundWindow`, working around the foreground-lock |
| Linux | request it from the WM via EWMH (`_NET_WM_STATE_ABOVE`); exact behaviour is WM-dependent |

Build order: ship the Fyne-only version first (fine most of the time), then add a
small `internal/alert` package with one function, `RaiseAbove(win)`, and a
platform file per OS behind it.

**Abuse control.** A page is louder than a message, so:

- The relay gains a generic per-sender frame rate limit (e.g. *N* `msg` frames
  per 10 s, set in `server.toml`). It cannot tell a page from a text — it does
  not need to — but the limit caps page spam as a side effect.
- The client enforces a per-contact cooldown (one page per minute from a given
  person) and offers "mute pages from X" per roster entry.
- Offline recipients: a page queues on the relay like any message (§8) and fires
  on reconnect. A page that is already stale on arrival (older than ~10 minutes)
  is shown as a normal message, not an alert.

**Mobile.** The same `page` payload; the shell renders it as an Android
full-screen-intent notification or an iOS *critical alert* (the latter needs a
special Apple entitlement).

### 12.2 Client updates — who builds, who distributes, who trusts

**Built** (2026-09-20/21; full narrative in `DEVLOG.md`'s "Client auto-update"
entries). Two independent mechanisms, deliberately kept separate throughout
the implementation:

- A coarse **protocol compatibility gate** — "can this build even talk to the
  relay" — carried on the `ready` frame itself, changes rarely, applies to
  every client uniformly. See "Protocol version negotiation" below.
- A per-artifact **convenience self-update** — "is there a newer build of
  exactly this binary" — manifest-driven (`internal/update`), independent of
  the protocol gate, so a release touching only `lanmsg-cli` never tells a
  GUI client anything changed for it.

The original design below (build/distribute/trust) survives mostly intact;
what changed under implementation is *who builds* — a hybrid the user chose
deliberately over the CI-only version originally sketched here, to avoid
depending on GitHub for routine releases — and the manifest shape, which
ended up per-artifact rather than one blanket version. The mistake the
original design was already right to avoid: making the server *build* the
clients. **Building and distributing are separate problems, and only
distribution involves the relay.**

#### Why not build on the server

The relay runs on a Raspberry Pi. Cross-compiling `lanmsg-cli`,
`lanmsg-remote-cli`, and `lanmsg-server` is easy — they are cgo-free (§2.9) —
and `scripts/pi-release.sh` does exactly that, on the Pi itself, for every
supported OS/arch. The GUI is the one binary this doesn't extend to: Fyne
needs cgo and a native per-OS toolchain, and a macOS build additionally needs
Apple's SDK plus a cross-linker (osxcross) that has no reasonable place on a
Pi. Even Linux GUI builds are host-*architecture*-bound — `fyne package -os
linux` run on the Pi's arm64 produces an arm64 tarball, not amd64, since it
packages for the target OS using the *host's* toolchain rather than actually
cross-compiling (see `docs/SETUP.md`'s packaging section). The relay stays a
message router; a GUI-affecting release goes through CI instead (below).

#### Who builds: a hybrid, chosen to avoid a GitHub dependency for routine releases

**Headless binaries (`lanmsg-cli`, `lanmsg-remote-cli`, and `lanmsg-server`
for an admin's own manual relay upgrade) build locally, no CI:**
`scripts/pi-release.sh`, run on the Pi (or any machine with Go 1.27+, as a
plain shell session — *not* inside `lanmsg-server.service`, whose
`ProtectSystem=strict` blocks a writable source tree). Cross-compiles all
six OS/arch combinations for each CLI plus `android/arm64` for
`lanmsg-remote-cli` (Termux), with `CGO_ENABLED=0`.

**GUI-affecting releases use GitHub Actions** (`.github/ci.yml.example`'s
`gui-build` job, gated to version-tag pushes) — native runners
(`ubuntu-latest`/`macos-latest`/`windows-latest`) each run `fyne package` for
their own OS, since that's the one build CI genuinely does something the Pi
categorically cannot. **CI never signs anything** — see "who trusts" below —
it only uploads unsigned workflow artifacts for the admin to download.

Either path produces artifacts that then get **hashed and signed on a third
machine** (`cmd/lanmsg-signrelease`, run on a machine kept separate from both
the Pi and CI) — not on whichever machine happened to build them. This is the
one deliberate departure from "signing lives in CI secrets," which the
original design (just below, historically) had allowed as an option: the
user chose to keep signing off of every automated system, full stop, so a
compromised Pi *or* a compromised CI run can still never push a trusted
backdoored client.

#### Who distributes: the relay

`internal/servercore/updates.go` serves whatever a release workflow placed
under `cfg.UpdatesDir()` (`<data_dir>/updates/`) — `manifest.json`,
`manifest.json.sig`, and `artifacts/*` — over three new routes on the
existing HTTPS listener, unauthenticated beyond TLS (see "who trusts,"
these files are public release info by design). A relay with nothing ever
published there simply 404s, a normal and valid state. The originally-sketched
`lanmsg-server fetch-update` subcommand (pulling artifacts from GitHub
automatically) was **not built** — every release currently reaches the relay
by the admin copying files there directly; automating that fetch remains
future work if it turns out to matter.

#### Who trusts what: the client

Each client ships with the release key's **public** half compiled in
(`internal/update.UpdatePubKey`, empty in this repo until a real release key
exists — see `cmd/lanmsg-signrelease`). `lanmsg-cli`/`lanmsg-remote-cli`
(the GUI never does this — see below) check after connecting:

1. fetch `manifest.json` + `manifest.json.sig`, verify the signature against
   the built-in public key — **fail ⇒ stop, the update is ignored entirely**;
2. verify the manifest's `seq` is strictly newer than the highest this client
   has ever seen (persisted locally) — **not newer ⇒ reject as a possible
   rollback**, closing a replay gap a stale or compromised relay could
   otherwise exploit by re-serving an old, still-validly-signed manifest;
3. look up the entry matching this binary's own `(target, os, arch)` — the
   manifest is a flat list of per-artifact entries, not one blanket release
   version (see the worked example below); no entry, or no newer `version`
   for it, and the check simply ends here, quietly;
4. download that artifact to a temp file in the executable's own directory,
   check its SHA-256 against the manifest;
5. atomically replace the binary — Unix: `rename(2)` over it (same
   filesystem, so any process still holding the old file open, including
   this one mid-syscall, keeps working against the old inode until it
   exits); Windows: rename the running `.exe` aside first (Windows won't
   let you overwrite a mapped-in-use executable directly), then move the new
   one in;
6. **only for a long-lived invocation** (`watch`) — re-exec into the new
   binary immediately (`syscall.Exec` on Unix, spawn-detached-and-exit on
   Windows). One-shot commands (`enroll`/`send`/`status`/`roster`) just swap
   the file and let the *next* invocation naturally run the new binary —
   there's no live session worth restarting into.

No third-party self-update library — `internal/update` is hand-rolled
(a few hundred lines: manifest fetch/verify, download/hash-check, and the
platform-split swap/re-exec), since the actual mechanics here (rollback-safe
manifest fetch, per-artifact matching, one-shot-vs-long-lived swap
semantics) didn't map cleanly onto an off-the-shelf GitHub-Releases-shaped
updater.

**Worked example** — a manifest after one CLI-only release followed by a
release that also touched the GUI:

```json
{
  "seq": 2,
  "generated_at": "2026-09-22T03:42:59Z",
  "artifacts": [
    { "target": "lanmsg-cli", "os": "linux", "arch": "arm64", "version": "0.2.0",
      "url": "/updates/artifacts/lanmsg-cli-linux-arm64-0.2.0", "sha256": "…", "size": 11460768 },
    { "target": "lanmsg-remote-cli", "os": "windows", "arch": "amd64", "version": "0.3.0",
      "url": "/updates/artifacts/lanmsg-remote-cli-windows-amd64-0.3.0.exe", "sha256": "…", "size": 11814400 },
    { "target": "lanmsg", "os": "darwin", "arch": "arm64", "version": "0.2.0",
      "url": "/updates/artifacts/lanmsg-darwin-arm64-0.2.0.tar.xz", "sha256": "…", "size": 19661784 }
  ]
}
```

`lanmsg-cli`'s entry is untouched from the previous release (`seq=1`) —
`lanmsg-signrelease sign -prev ...` carries forward any artifact not named in
the new release's spec, byte-for-byte. `lanmsg` entries exist for the
banner to link to (a person can download and install it themselves) but
`internal/update`'s auto-swap is never invoked for `target == "lanmsg"` —
structurally impossible, not just policy, since the GUI (which shares
`internal/clientcore`) never imports `internal/update` at all.
`lanmsg-server`/`lanmsg-tunnel` never appear in a manifest — updating the
relay itself stays the manual process in `docs/DEPLOY-TO-PI.html`.

#### Why manifest signing is non-negotiable

An auto-update path is a remote-code-execution path by definition. If updates
were trusted merely for arriving over the relay's TLS connection, anyone who
compromised the Pi could push a backdoored client to every machine in the house.
With an offline signing key — kept off the Pi *and* off CI (above) — a
compromised relay can at worst serve a stale or corrupt file, which step 1, 2,
or 4 above rejects. **The relay is distribution, not authority.**

#### Optional module: binary-delta ("bit comparison") updates

**Not built** — everything below is still the original design, unaffected by
the hybrid build/sign workflow above. `internal/update` has no `Patcher`
interface or delta path today; every client does a whole-file download.
Revisit if bandwidth actually becomes a constraint (see "it stops being a
guess" below).

On three machines on gigabit Ethernet, a whole 10–20 MB signed binary downloads
in a fraction of a second, so a delta saves nothing *today*. The reasons to
build it anyway, as a **separate, opt-in module**:

- it stops being a guess the moment there are many clients, frequent releases,
  Wi-Fi-only laptops, or a relay reached over the internet or a slow VPN;
- designed in now it is ~200 lines behind an interface; retrofitted into a
  shipped updater it is a manifest/format change affecting every client.

**The trust model does not change.** A delta is a bandwidth optimisation that
sits *behind* the same two gates as a full download: the offline Ed25519
signature on the manifest, and the manifest's whole-file SHA-256 of the
*result*. The manifest gains an optional per-artifact `patches` array:

```json
{ "os": "linux", "arch": "amd64", "url": "…", "sha256": "<sha256 of whole new binary>",
  "patches": [
    { "from_version": "1.3.0", "from_sha256": "<sha256 of whole old binary>",
      "algo": "bsdiff", "url": "…", "sha256": "<sha256 of the patch file>" }
  ] }
```

Client decision tree:

1. verify manifest signature (unchanged); newer version available?
2. is my on-disk binary's SHA-256 equal to some `patches[].from_sha256`, and do I
   have a patcher for its `algo`? If **no** → whole-file download path (unchanged).
3. if **yes** → download the patch, check the patch file's own `sha256`, apply it
   to my current binary → candidate file;
4. check the candidate's SHA-256 against the artifact's whole-file `sha256`.
   **Mismatch ⇒ discard, fall back to the whole-file download.**
5. atomic swap + re-exec, exactly as the whole-file path.

A corrupt or hostile patch cannot produce a binary that matches the signed
whole-file hash, so step 4 makes the delta path no more trusted than the plain
one. If the on-disk binary isn't a pristine release (locally built, already
patched by a half-finished run), step 2 fails closed and the client just fetches
the whole file.

**Modularity boundary.** A core package `internal/update` owns the manifest,
signature check, download, hash check and atomic swap — this part is now
real code, not just a design (see "who trusts what: the client" above) — and
would depend only on:

```go
// internal/updatedelta
type Patcher interface {
    Algo() string
    Apply(old io.ReaderAt, oldSize int64, patch io.Reader, out io.Writer) error
}
```

plus a registry. The delta implementation — a pure-Go bsdiff/bspatch port
(`github.com/gabstv/go-bsdiff` or similar) or a zstd `--patch-from` backend —
lives entirely in `internal/updatedelta` and registers itself from an optional
import (`_ "lanmessenger/internal/updatedelta/bsdiff"`) or a build tag. With
nothing registered, `internal/update` compiles and runs with no delta code at
all and every client does whole-file downloads. Nothing on the core path imports
the delta package.

**Who generates patches: CI, not the relay.** After building the new artifacts,
a CI step pulls the previous *N* still-supported releases' artifacts from GitHub
Releases and runs the diff generator (`new ← old`) for each `(os, arch)` and each
`from_version`, hashes each patch, appends the `patches[]` entries, and uploads
the patch files next to the artifacts. Patch *generation* is the memory-hungry
side (bsdiff is O(n log n) with a large working set) and it runs on a CI runner.
The relay's `updates/` dir and `fetch-update` just mirror the extra files; the
relay never diffs or patches anything. Clients no more than *N* releases behind
get a delta; everyone else gets the whole file.

**Per-OS opt-in.** Enable deltas for `linux` and `windows` first (patch the lone
executable). macOS ships a signed `.app` bundle; patching the inner Mach-O
client-side would break the code signature, so **macOS stays whole-file** (whole
-bundle) until there's a signing story for in-place updates. Mobile is
store-only regardless (previous section). Delta application peak memory is
roughly `oldsize + newsize` transient — nothing on a desktop, but another reason
mobile stays out.

**Bottom line:** design it in, keep it in its own package behind the `Patcher`
interface, ship it disabled, and turn it on per-OS when a real bandwidth reason
shows up.

#### Mobile updates are not part of this

iOS App Store rule 2.5.2 forbids an app downloading and running executable code —
iOS updates come from the App Store or TestFlight, full stop. Android technically
lets an app install a signed APK if it was sideloaded, but the flow is clunky
(system installer UI, an "install unknown apps" permission) and Play-distributed
apps must update through Play. Plan on **mobile updating through the stores.**

#### The common denominator: protocol version negotiation (built)

Every client — desktop now, mobile later — shares one small mechanism, worth
having even before any self-update code exists. The `Envelope` was already
versioned (§5); the `ready` frame (`internal/proto/messages.go`) now carries:

```go
type Ready struct {
    DeviceID         string
    Admin            bool
    ServerVersion    string `json:"server_version,omitempty"`
    MinClientVersion string `json:"min_client_version,omitempty"`
}
```

- Client version `< MinClientVersion` (an admin-configured floor,
  `min_client_version` in `server.toml`, empty by default — no floor,
  every existing deployment keeps working unchanged) ⇒ **hard stop**: the
  relay rejects at `Hello` time with a new `client_too_old` error code,
  before any challenge/response — cheap, and before any admin-approval logic
  even runs. The client's `runLoop` recognizes this and stops
  reconnecting (an `EventUpdateRequired` event; retrying would just get
  rejected again forever).
- `MinClientVersion ≤` client version `< ServerVersion` ⇒ soft "update
  available" (`EventUpdateAvailable`), surfaced as a status-line banner in
  the GUI and a stderr line in both CLIs — connection unaffected.

Desktop self-update (`internal/update`, above) is a separate, optional
convenience layered on top of this gate — deliberately not carried in the
`Ready` frame itself, so the coarse protocol check never needs to know about
manifests, and `internal/clientcore` (which the GUI shares) never needs to
import `internal/update` at all. Mobile, when it exists, would just show the
banner with a link to its store.

**What shipped vs. what's still ahead:** the protocol gate, the signed
per-artifact manifest, `internal/update`, the relay's `/updates/` routes,
and both release workflows (Pi-local for headless binaries, GitHub Actions
for the GUI) are all built and live-tested end to end (`DEVLOG.md`). The
`fetch-update` convenience subcommand and the `internal/updatedelta` binary-
delta module above remain future work — neither blocks anything that's
already shipped.

---

## 13. Reaching the relay from outside the LAN: the Tor-tunneled cloud relay

The relay (§1) assumes every client can route to it. For family members
away from the house, that assumption doesn't hold, and the home router has
no public IP and no port-forwarding. The fix keeps the Pi's role
unchanged — it still only ever *dials out* — and adds one new, deliberately
minimal component: `lanmsg-tunnel` (`cmd/lanmsg-tunnel`, runtime in
`internal/tunnel`), a small always-on process on a cheap cloud VM that does
pure byte-level TCP forwarding, reachable only via a Tor hidden service
(a `.onion` address) rather than any open inbound port.

**What it is not.** It is not a second relay. It has no database, no
roster, no message queue, no E2E keys, and no household passphrase. It
cannot approve a device, read a message, or impersonate the relay to a
client. Its public-facing listener is deliberately never wrapped in the
app's TLS, because that TLS must terminate only at the Pi (§1: "TLS
protects the pipe; E2E protects the payload even from whoever runs the
pipe"). Its only privilege is deciding whether one specific connection is
allowed to call itself "the backend" — narrower and less trusted than
anything the Pi itself holds (§12.2: "the relay is distribution, not
authority").

**How it works.** The Pi dials the cloud box's `.onion` address (through
its own local Tor client, over its local SOCKS5 proxy — `internal/servercore/tunnel.go`,
`dialTunnel`) and proves it holds a shared secret (Argon2id + HMAC
challenge–response — the same construction as the household passphrase,
§3.2/§3.3, but a separate secret scoped only to this link — see
`internal/tunnel/auth.go`). No additional TLS wraps this leg: a `.onion`
address is itself a cryptographic proof of identity, so a second layer
would be redundant. The resulting connection is multiplexed
(`github.com/hashicorp/yamux`) into one logical stream per remote client
the cloud box accepts (`internal/tunnel/hub.go`, `Hub`). Each stream
carries that client's raw TLS bytes untouched straight to the Pi's
existing WebSocket server (`internal/tunnel/listener.go`,
`SessionListener`), which handles it exactly like a LAN connection — same
certificate, same fingerprint pin, same passphrase auth, same rate limits
(`internal/ratelimit`).

A remote client's address is correlated to its data stream over a
*separate, dedicated control stream* rather than a preamble frame written
onto the data stream itself — a data stream reaching `net/http` is one
that genuinely nothing has ever read from beforehand, which turns out to
matter: see `internal/tunnel/bufferedconn.go`'s doc comment and the
2026-09-19/20 `DEVLOG.md` entry for a real, subtle bug (TLS state
corruption from an interrupted read over a multiplexed stream) this
avoids.

**What the cloud box can see if fully compromised:** connection metadata
only — timing and byte counts of an already-anonymized Tor circuit; not
even the remote client's real IP is guaranteed visible past the `.onion`
layer. It can never see message content, the household passphrase, or any
device's keys, and can at worst deny service, which clients already
handle as an ordinary "relay unreachable" reconnect (`internal/clientcore`'s
existing reconnect-with-backoff, unchanged).

Remote clients need no changes to the existing GUI or CLI — a new, minimal
`lanmsg-remote-cli` (`cmd/lanmsg-remote-cli`) reuses the same
`internal/clientcore` client library with one added optional setting
(`Config.SOCKSProxy`) to route through Tor. It cross-compiles for
`android/arm64` and runs under Termux (see `docs/SETUP.md`), since nothing
in its dependency chain requires cgo.

See `docs/NETWORK.md` ("Case D") for the deployment topology and
`docs/SETUP.md` for the full Tor setup walkthrough.
