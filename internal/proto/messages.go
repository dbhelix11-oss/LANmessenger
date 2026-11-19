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

	// Admin-key console: device report, removal, forced re-authorization.
	TypeAdminDevices     Type = "admin_devices"      // client -> server
	TypeAdminDeviceList  Type = "admin_device_list"  // server -> client
	TypeAdminRemove      Type = "admin_remove"       // client -> server
	TypeAdminReauth      Type = "admin_reauth"       // client -> server
	TypeAdminRename      Type = "admin_rename"       // client -> server
	TypeProfileUpdate    Type = "profile_update"     // server -> client: the admin changed YOUR display name
	TypeAdminRevoke      Type = "admin_revoke"       // client -> server
	TypeAdminSetApproval Type = "admin_set_approval" // client -> server
	TypeAdminOK          Type = "admin_ok"           // server -> client
	TypeAdminAlert       Type = "admin_alert"        // server -> admin device (LAN): something needs the admin's attention

	// A device's signed statement about itself (version, OS, build hash...).
	TypeClientFacts Type = "client_facts" // client -> server

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
	// ClientKind ("gui", "cli", "remote-cli", "android"), ClientOS and
	// ClientArch are self-declared and unsigned (a hello comes before any
	// authentication), so they describe the program for the admin's Properties
	// view and for spotting change; they prove nothing. Sent on enrolment too.
	ClientKind string `json:"client_kind,omitempty"`
	ClientOS   string `json:"client_os,omitempty"`
	ClientArch string `json:"client_arch,omitempty"`
	DeviceID   string `json:"device_id,omitempty"`
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
	// StateRevoked: an admin withdrew this registration. The record is kept (greyed out
	// for the admin); every later attempt to sign in or enroll with the same credentials
	// is refused, recorded and reported to the admin.
	StateRevoked EnrollState = "revoked"
)

// EnrollResult tells the client its assigned DeviceID and whether it is active
// immediately or waiting for admin approval.
type EnrollResult struct {
	DeviceID string      `json:"device_id"`
	State    EnrollState `json:"state"`
}

// Ready signals that authentication is complete and the client is now active.
// It is not sent while a device is Pending.
//
// ServerVersion and MinClientVersion are the relay's coarse protocol
// compatibility gate: a client older than MinClientVersion is rejected before
// it ever gets here (see ErrClientTooOld), so their presence in Ready only
// ever means "you're allowed to connect, but a newer build exists." This is
// deliberately separate from per-binary self-update version checks, which are
// manifest-driven (internal/update) rather than carried on this frame.
type Ready struct {
	DeviceID         string `json:"device_id"`
	Admin            bool   `json:"admin"`
	ServerVersion    string `json:"server_version,omitempty"`
	MinClientVersion string `json:"min_client_version,omitempty"`
	// DisplayName is the name the relay holds for this device. A client that sees a
	// different one than its own adopts it (an admin may have renamed it).
	DisplayName string `json:"display_name,omitempty"`
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
	// InnerClearHistory asks the peer to delete its copy of this conversation
	// ("delete for both"). Sealed like any message; the relay cannot tell it
	// from text.
	InnerClearHistory InnerKind = "clear_history"
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
type AdminListPending struct {
	Auth *AdminAuth `json:"auth,omitempty"`
}

// AdminPendingList is the server's reply to [AdminListPending].
type AdminPendingList struct {
	Entries []DirectoryEntry `json:"entries"`
}

// AdminApprove asks the server to activate a pending device. Signature is an
// ed25519 signature by the admin's signing key over AdminActionMessage("approve",
// DeviceID).
type AdminApprove struct {
	DeviceID  string     `json:"device_id"`
	Signature string     `json:"signature"` // base64
	Auth      *AdminAuth `json:"auth,omitempty"`
}

// AdminDeny asks the server to reject a pending device. Signature covers
// AdminActionMessage("deny", DeviceID).
type AdminDeny struct {
	DeviceID  string     `json:"device_id"`
	Signature string     `json:"signature"` // base64
	Auth      *AdminAuth `json:"auth,omitempty"`
}

// AdminActionMessage returns the exact bytes an admin signs (and the server
// verifies) for an approve/deny action.
func AdminActionMessage(action, deviceID string) []byte {
	return []byte(action + ":" + deviceID)
}

// AdminAuth proves an admin frame was authorised by the holder of the admin
// key (not merely by some connected device). Sig is an ed25519 signature over
// AdminAuthMessage(frameType, target, TS, Nonce). The relay rejects stale
// timestamps and nonces it has already seen.
type AdminAuth struct {
	TS    int64  `json:"ts"` // unix milliseconds, admin machine clock
	Nonce string `json:"nonce"`
	Sig   string `json:"sig"` // base64
}

// AdminAuthMessage returns the exact bytes signed for an admin frame.
// target is the device ID acted on, or "" for list-style requests.
func AdminAuthMessage(t Type, target string, ts int64, nonce string) []byte {
	return []byte(fmt.Sprintf("lanmsg-admin-v1|%s|%s|%d|%s", t, target, ts, nonce))
}

// AdminDevices requests the full device report (every device, any state).
type AdminDevices struct {
	Auth *AdminAuth `json:"auth,omitempty"`
}

// AdminRemove deletes a device; it must enroll again to return.
type AdminRemove struct {
	DeviceID string     `json:"device_id"`
	Auth     *AdminAuth `json:"auth,omitempty"`
}

// AdminReauth sends a device back to pending and drops its session.
type AdminReauth struct {
	DeviceID string     `json:"device_id"`
	Auth     *AdminAuth `json:"auth,omitempty"`
}

// AdminRename changes the display name the relay holds for a device. The signature
// binds both the device and the new name (target "id|name").
type AdminRename struct {
	DeviceID string     `json:"device_id"`
	NewName  string     `json:"new_name"`
	Auth     *AdminAuth `json:"auth,omitempty"`
}

// ProfileUpdate tells a device its display name was changed by the admin.
type ProfileUpdate struct {
	DisplayName string `json:"display_name"`
}

// AdminRevoke withdraws a device's registration without deleting it.
type AdminRevoke struct {
	DeviceID string     `json:"device_id"`
	Auth     *AdminAuth `json:"auth,omitempty"`
}

// AdminAlert is pushed to connected admin devices on the LAN. Kind "revoked_attempt":
// something that looks like a revoked registration tried to come back. Match is
// "key" (the enrolment carried the very same signing key), "id" (only the device ID
// was presented; a resume proves the household passphrase, not the device key) or
// "fingerprint" (a NEW key, but the connection resembles the revoked device's: see
// Signals, a comma list of "tls" = same TLS handshake shape, "client" = same
// program/OS/CPU, "address" = same LAN address). Fingerprint matches are hints
// that it may be the same machine, never proof.
type AdminAlert struct {
	Kind     string `json:"kind"`
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
	Match    string `json:"match"`
	Path     string `json:"path"`
	Count    int    `json:"count"`
	TS       int64  `json:"ts"`
	Signals  string `json:"signals,omitempty"`
	NewName  string `json:"new_name,omitempty"` // display name the newcomer registered with
	NewID    string `json:"new_device_id,omitempty"`
}

// ClientFacts is what a device says about itself. Sig is the device's own
// signing key over FactsMessage(); the relay marks the facts "verified" when it
// matches the key on file. Facts are self-reported: useful for spotting change,
// not proof of anything.
type ClientFacts struct {
	Version   string `json:"version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	BuildHash string `json:"build_hash,omitempty"` // sha256 of the running binary
	Model     string `json:"model,omitempty"`
	TZ        string `json:"tz,omitempty"`
	Sig       string `json:"sig,omitempty"`
}

// FactsMessage is the canonical byte string a device signs.
func (f ClientFacts) FactsMessage(deviceID string) []byte {
	return []byte(fmt.Sprintf("lanmsg-facts-v1|%s|%s|%s|%s|%s|%s|%s",
		deviceID, f.Version, f.OS, f.Arch, f.BuildHash, f.Model, f.TZ))
}

// ConnRecord is one past or current connection of a device.
type ConnRecord struct {
	ConnectedAt    int64  `json:"connected_at"`    // unix seconds
	DisconnectedAt int64  `json:"disconnected_at"` // 0 while still connected
	Path           string `json:"path"`            // "lan" | "tunnel" | "direct"
	RemoteHost     string `json:"remote_host"`     // as the relay sees it
	TLSHash        string `json:"tls_hash"`        // JA3-style ClientHello hash
	ClientVersion  string `json:"client_version"`
	ClientKind     string `json:"client_kind,omitempty"`
	ClientOS       string `json:"client_os,omitempty"`
	ClientArch     string `json:"client_arch,omitempty"`
	RTTAvgMs       int    `json:"rtt_avg_ms"`
	RTTMinMs       int    `json:"rtt_min_ms"`
	RTTMaxMs       int    `json:"rtt_max_ms"`
}

// FactChange records a field of a device's declared facts changing.
type FactChange struct {
	TS    int64  `json:"ts"`
	Field string `json:"field"`
	Old   string `json:"old"`
	New   string `json:"new"`
}

// DeviceReport is everything the relay knows about one device.
type DeviceReport struct {
	Entry         DirectoryEntry `json:"entry"`
	Fingerprint   string         `json:"fingerprint"`
	Online        bool           `json:"online"`
	CreatedAt     int64          `json:"created_at"`
	ApprovedAt    int64          `json:"approved_at"`
	Facts         *ClientFacts   `json:"facts,omitempty"`
	FactsVerified bool           `json:"facts_verified"`
	FactsAt       int64          `json:"facts_at"`
	FactChanges   []FactChange   `json:"fact_changes,omitempty"`
	Conns         []ConnRecord   `json:"conns,omitempty"` // newest first, capped
	ConnCount     int            `json:"conn_count"`
	TLSHashes     []string       `json:"tls_hashes,omitempty"` // distinct hashes ever seen
	Paths         []string       `json:"paths,omitempty"`      // distinct paths ever seen
	// Revocation (state "revoked"): when, and how often it has tried to come back.
	RevokedAt         int64  `json:"revoked_at,omitempty"`
	RevokeAttempts    int    `json:"revoke_attempts,omitempty"`
	LastRevokeAttempt int64  `json:"last_revoke_attempt,omitempty"`
	LastRevokeMatch   string `json:"last_revoke_match,omitempty"`
	LastRevokeSignals string `json:"last_revoke_signals,omitempty"`
	LastRevokeName    string `json:"last_revoke_name,omitempty"`
}

// AdminDeviceList is the reply to AdminDevices.
type AdminDeviceList struct {
	Devices []DeviceReport `json:"devices"`
	// RequireApproval is the relay's current "hold every new device for admin approval" setting.
	RequireApproval bool `json:"require_approval"`
}

// AdminSetApproval turns "require admin approval for new devices" on or off. The
// signature binds the value (target "on" or "off"), so a captured frame cannot be
// replayed to flip it back.
type AdminSetApproval struct {
	Value bool       `json:"value"`
	Auth  *AdminAuth `json:"auth,omitempty"`
}

// ---------------------------------------------------------------------------
// Error payload
// ---------------------------------------------------------------------------

// Error codes used in [ErrorBody].
const (
	ErrBadRequest   = "bad_request"
	ErrAuthFailed   = "auth_failed"
	ErrPending      = "pending_approval"
	ErrNotFound     = "not_found"
	ErrForbidden    = "forbidden"
	ErrTooLarge     = "too_large"
	ErrRateLimited  = "rate_limited"
	ErrInternal     = "internal"
	ErrClientTooOld = "client_too_old" // Hello.ClientVersion is below the relay's configured minimum
)

// ErrorBody is the payload for [TypeError].
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ClearHistoryBody is the payload for [InnerClearHistory]. The receiver deletes
// its stored messages with this sender whose timestamp is <= UpTo (Unix ms,
// sender's clock), so a message sent after the request is never removed.
type ClearHistoryBody struct {
	UpTo int64 `json:"up_to"`
}
