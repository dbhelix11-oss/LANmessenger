// Package proto defines the wire protocol spoken between lanmsg clients and the
// relay server. Frames are JSON objects sent over a TLS WebSocket connection.
//
// Every frame is an [Envelope]. The concrete payload for a given [Type] lives in
// the envelope's Data field as raw JSON and is decoded with [Envelope.Unmarshal].
//
// The server only ever sees the outer envelope and, for [TypeMsg], an opaque
// sealed ciphertext. Message contents (text, file transfers, receipts) are
// end-to-end encrypted and travel as an [Inner] payload inside that ciphertext.
package proto

import (
	"encoding/json"
	"fmt"
)

// Version is the protocol version carried in every [Envelope]. Bump it on any
// breaking change to frame shapes.
const Version = 1

// Type identifies which payload an [Envelope] carries.
type Type string

const (
	// Connection setup.
	TypeHello         Type = "hello"          // client -> server, first frame
	TypeAuthChallenge Type = "auth_challenge" // server -> client
	TypeAuthResponse  Type = "auth_response"  // client -> server
	TypeEnroll        Type = "enroll"         // client -> server, first time only
	TypeEnrollResult  Type = "enroll_result"  // server -> client
	TypeReady         Type = "ready"          // server -> client, auth complete

	// Directory of known devices.
	TypeDirectorySnapshot Type = "directory_snapshot" // server -> client
	TypeDirectoryUpdate   Type = "directory_update"   // server -> client

	// Presence.
	TypePresenceSet    Type = "presence_set"    // client -> server
	TypePresenceUpdate Type = "presence_update" // server -> client

	// End-to-end encrypted message relay. File transfers and receipts also ride
	// inside TypeMsg as an encrypted Inner payload.
	TypeMsg    Type = "msg"     // both directions
	TypeMsgAck Type = "msg_ack" // both directions

	// Admin approval of pending devices.
	TypeAdminListPending Type = "admin_list_pending" // client -> server
	TypeAdminPendingList Type = "admin_pending_list" // server -> client
	TypeAdminApprove     Type = "admin_approve"      // client -> server
	TypeAdminDeny        Type = "admin_deny"         // client -> server

	// Liveness and errors.
	TypePing  Type = "ping"
	TypePong  Type = "pong"
	TypeError Type = "error"
)

// Envelope is the outer frame for every message on the wire.
type Envelope struct {
	V    int             `json:"v"`
	Type Type            `json:"type"`
	ID   string          `json:"id,omitempty"` // correlation id, echoed in replies
	TS   int64           `json:"ts,omitempty"` // unix milliseconds, sender clock
	Data json.RawMessage `json:"data,omitempty"`
}

// NewEnvelope builds an envelope of type t wrapping payload, which is marshalled
// to JSON. A nil payload produces an envelope with no Data.
func NewEnvelope(t Type, id string, ts int64, payload any) (*Envelope, error) {
	e := &Envelope{V: Version, Type: t, ID: id, TS: ts}
	if payload == nil {
		return e, nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("proto: marshal %s payload: %w", t, err)
	}
	e.Data = raw
	return e, nil
}

// Unmarshal decodes the envelope's Data into v.
func (e *Envelope) Unmarshal(v any) error {
	if len(e.Data) == 0 {
		return fmt.Errorf("proto: %s envelope has no data", e.Type)
	}
	if err := json.Unmarshal(e.Data, v); err != nil {
		return fmt.Errorf("proto: unmarshal %s payload: %w", e.Type, err)
	}
	return nil
}

// Parse decodes a wire frame into an Envelope and validates its version.
func Parse(b []byte) (*Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, fmt.Errorf("proto: parse envelope: %w", err)
	}
	if e.V != Version {
		return nil, fmt.Errorf("proto: unsupported protocol version %d (want %d)", e.V, Version)
	}
	if e.Type == "" {
		return nil, fmt.Errorf("proto: envelope missing type")
	}
	return &e, nil
}

// Marshal encodes an Envelope to a wire frame.
func Marshal(e *Envelope) ([]byte, error) {
	return json.Marshal(e)
}

// ---------------------------------------------------------------------------
// Connection setup payloads
// ---------------------------------------------------------------------------

// Hello is the first frame a client sends. DeviceID is empty for a device that
// has not enrolled yet.
type Hello struct {
	ClientVersion string `json:"client_version"`
	DeviceID      string `json:"device_id,omitempty"`
}

// AuthChallenge carries a fresh random nonce plus the Argon2id salt the server
// has on file for the household passphrase. The client proves knowledge of the
// passphrase without sending it.
type AuthChallenge struct {
	Nonce string `json:"nonce"` // base64, 32 bytes
	Salt  string `json:"salt"`  // base64, Argon2id salt
}

// AuthResponse answers an AuthChallenge. Proof is
// HMAC-SHA256(Argon2id(passphrase, salt), nonce), base64 encoded. DeviceID is
// set when an already-enrolled device is re-authenticating.
type AuthResponse struct {
	DeviceID string `json:"device_id,omitempty"`
	Proof    string `json:"proof"`
}

// Enroll registers a brand new device. It is sent right after a successful
// AuthResponse when the client has no DeviceID yet.
type Enroll struct {
	DisplayName string `json:"display_name"`
	SignPub     string `json:"sign_pub"` // base64, ed25519 public key (32 bytes)
	BoxPub      string `json:"box_pub"`  // base64, X25519 public key (32 bytes)
}

// EnrollState is the lifecycle state of a device in the server directory.
type EnrollState string

const (
	StateActive  EnrollState = "active"  // may send and receive
	StatePending EnrollState = "pending" // awaiting admin approval
	StateDenied  EnrollState = "denied"  // rejected by an admin
)

// EnrollResult tells the client its assigned DeviceID and whether it is active
// immediately or waiting for admin approval.
type EnrollResult struct {
	DeviceID string      `json:"device_id"`
	State    EnrollState `json:"state"`
}

// Ready signals that authentication is complete and the client is now active.
// It is not sent while a device is Pending.
type Ready struct {
	DeviceID string `json:"device_id"`
	Admin    bool   `json:"admin"`
}

// ---------------------------------------------------------------------------
// Directory payloads
// ---------------------------------------------------------------------------

// DirectoryEntry describes one device known to the server.
type DirectoryEntry struct {
	DeviceID    string      `json:"device_id"`
	DisplayName string      `json:"display_name"`
	SignPub     string      `json:"sign_pub"` // base64
	BoxPub      string      `json:"box_pub"`  // base64
	State       EnrollState `json:"state"`
	Admin       bool        `json:"admin"`
}

// DirectorySnapshot is the full directory, sent once after the client becomes
// ready and whenever it reconnects.
type DirectorySnapshot struct {
	Entries []DirectoryEntry `json:"entries"`
}

// DirectoryUpdate is an incremental change to the directory.
type DirectoryUpdate struct {
	Entry   DirectoryEntry `json:"entry"`
	Removed bool           `json:"removed,omitempty"`
}

// ---------------------------------------------------------------------------
// Presence payloads
// ---------------------------------------------------------------------------

// Status is a user-selectable presence state.
type Status string

const (
	StatusAvailable Status = "available"
	StatusAway      Status = "away"
	StatusBusy      Status = "busy"
	StatusDND       Status = "dnd"
	StatusInvisible Status = "invisible"
	StatusOffline   Status = "offline"
)

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	switch s {
	case StatusAvailable, StatusAway, StatusBusy, StatusDND, StatusInvisible, StatusOffline:
		return true
	default:
		return false
	}
}

// PresenceSet is a client publishing its own presence.
type PresenceSet struct {
	Status  Status `json:"status"`
	Message string `json:"message,omitempty"` // optional free-text status
}

// PresenceUpdate is the server broadcasting a device's presence to others.
// A device that is Invisible is reported to others as Online=false.
type PresenceUpdate struct {
	DeviceID string `json:"device_id"`
	Status   Status `json:"status"`
	Message  string `json:"message,omitempty"`
	Online   bool   `json:"online"`
	TS       int64  `json:"ts"`
}

// ---------------------------------------------------------------------------
// Message relay payloads
// ---------------------------------------------------------------------------

// Msg carries one end-to-end encrypted message. From is filled in by the server
// on delivery; clients leave it empty when sending. Ciphertext is a sealed
// [Inner] payload (see internal/crypto.Seal).
type Msg struct {
	From       string `json:"from,omitempty"`
	To         string `json:"to"`
	MsgID      string `json:"msg_id"`     // client-generated, unique per sender
	Nonce      string `json:"nonce"`      // base64, 24-byte nacl/box nonce
	Ciphertext string `json:"ciphertext"` // base64
	TS         int64  `json:"ts"`         // sender clock, unix milliseconds
}

// MsgAck acknowledges receipt of a Msg by its MsgID. The server uses acks from
// recipients to drop messages from the offline queue, then relays the ack to the
// original sender.
//
// Peer is the "other party": when a recipient sends the ack it is the original
// sender's device ID; when the server relays the ack onward it is rewritten to
// the acking device's ID so the sender learns who received the message.
type MsgAck struct {
	MsgID string `json:"msg_id"`
	Peer  string `json:"peer"`
}

// ---------------------------------------------------------------------------
// Inner (end-to-end encrypted) payloads
// ---------------------------------------------------------------------------

// InnerKind tags the decrypted contents of a [Msg].
type InnerKind string

const (
	InnerText      InnerKind = "text"
	InnerFileOffer InnerKind = "file_offer"
	InnerFileChunk InnerKind = "file_chunk"
	InnerReceipt   InnerKind = "receipt"
	InnerTyping    InnerKind = "typing"
)

// Inner is the decrypted body of a [Msg]. Data holds the kind-specific payload.
type Inner struct {
	Kind InnerKind       `json:"kind"`
	Data json.RawMessage `json:"data,omitempty"`
}

// NewInner builds an Inner of the given kind wrapping payload.
func NewInner(kind InnerKind, payload any) (*Inner, error) {
	i := &Inner{Kind: kind}
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("proto: marshal inner %s: %w", kind, err)
		}
		i.Data = raw
	}
	return i, nil
}

// Unmarshal decodes the Inner payload into v.
func (i *Inner) Unmarshal(v any) error {
	if len(i.Data) == 0 {
		return fmt.Errorf("proto: inner %s has no data", i.Kind)
	}
	return json.Unmarshal(i.Data, v)
}

// TextBody is the payload for [InnerText].
type TextBody struct {
	Text string `json:"text"`
}

// FileOfferBody is the payload for [InnerFileOffer]: the manifest that precedes
// a sequence of [InnerFileChunk] messages sharing the same TransferID.
type FileOfferBody struct {
	TransferID string `json:"transfer_id"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	MIME       string `json:"mime,omitempty"`
	SHA256     string `json:"sha256"`     // hex, of the whole plaintext file
	ChunkSize  int    `json:"chunk_size"` // plaintext bytes per chunk
	Chunks     int    `json:"chunks"`
}

// FileChunkBody is the payload for [InnerFileChunk]. Bytes is base64 of the
// plaintext chunk (the whole Inner is sealed on the wire).
type FileChunkBody struct {
	TransferID string `json:"transfer_id"`
	Index      int    `json:"index"`
	Bytes      string `json:"bytes"`
}

// ReceiptBody is the payload for [InnerReceipt]: a delivered/read marker for a
// previously sent message.
type ReceiptBody struct {
	MsgID string `json:"msg_id"`
	State string `json:"state"` // "delivered" | "read"
}

// TypingBody is the payload for [InnerTyping].
type TypingBody struct {
	Active bool `json:"active"`
}

// ---------------------------------------------------------------------------
// Admin payloads
// ---------------------------------------------------------------------------

// AdminListPending requests the list of devices awaiting approval.
type AdminListPending struct{}

// AdminPendingList is the server's reply to [AdminListPending].
type AdminPendingList struct {
	Entries []DirectoryEntry `json:"entries"`
}

// AdminApprove asks the server to activate a pending device. Signature is an
// ed25519 signature by the admin's signing key over AdminActionMessage("approve",
// DeviceID).
type AdminApprove struct {
	DeviceID  string `json:"device_id"`
	Signature string `json:"signature"` // base64
}

// AdminDeny asks the server to reject a pending device. Signature covers
// AdminActionMessage("deny", DeviceID).
type AdminDeny struct {
	DeviceID  string `json:"device_id"`
	Signature string `json:"signature"` // base64
}

// AdminActionMessage returns the exact bytes an admin signs (and the server
// verifies) for an approve/deny action.
func AdminActionMessage(action, deviceID string) []byte {
	return []byte(action + ":" + deviceID)
}

// ---------------------------------------------------------------------------
// Error payload
// ---------------------------------------------------------------------------

// Error codes used in [ErrorBody].
const (
	ErrBadRequest  = "bad_request"
	ErrAuthFailed  = "auth_failed"
	ErrPending     = "pending_approval"
	ErrNotFound    = "not_found"
	ErrForbidden   = "forbidden"
	ErrTooLarge    = "too_large"
	ErrRateLimited = "rate_limited"
	ErrInternal    = "internal"
)

// ErrorBody is the payload for [TypeError].
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
