package tunnel

// ProtocolVersion is the tunnel control-protocol version, bumped on any
// breaking change to the handshake or preamble frame shapes.
const ProtocolVersion = 1

// RoleBackend is the only value [Hello.Role] may hold today; the field
// exists so the protocol can grow other roles later without changing the
// frame shape.
const RoleBackend = "backend"

// Hello is the first frame the backend (the home relay) sends when it dials
// the cloud tunnel.
type Hello struct {
	Version int    `json:"v"`
	Role    string `json:"role"`
}

// Challenge is the cloud tunnel's reply to a valid Hello: a fresh nonce and
// the Argon2id salt for the shared secret it has on file, both base64.
type Challenge struct {
	Nonce string `json:"nonce"`
	Salt  string `json:"salt"`
}

// Response answers a Challenge: HMAC-SHA256(Argon2id(secret, salt), nonce),
// base64 encoded — the same construction as the household-passphrase
// challenge/response (see internal/crypto), over a separate secret scoped
// only to this link.
type Response struct {
	Proof string `json:"proof"`
}

// Result ends the handshake. OK == false means the connection is about to
// be closed by the cloud tunnel.
type Result struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

// StreamPreamble describes one data stream the cloud tunnel opened toward
// the backend for an accepted public client connection. It travels over a
// dedicated control stream (see listener.go), correlated to its data
// stream by StreamID — never written onto the data stream itself, so
// net/http never sees a data stream that anything has already read from.
// It's trustworthy specifically because the control stream can only ever
// exist on an already-authenticated backend session — an unauthenticated
// internet host can never inject one.
type StreamPreamble struct {
	StreamID       uint32 `json:"stream_id"`
	RemoteAddr     string `json:"remote_addr"`
	OpenedAtUnixMs int64  `json:"opened_at_unix_ms"`
}
