package clientcore

import "lanmessenger/internal/proto"

// ConnState is the client's connection lifecycle state.
type ConnState int

const (
	StateDisconnected ConnState = iota
	StateConnecting
	StatePendingApproval // enrolled but the relay is holding us for admin approval
	StateReady
)

func (s ConnState) String() string {
	switch s {
	case StateConnecting:
		return "connecting"
	case StatePendingApproval:
		return "pending_approval"
	case StateReady:
		return "ready"
	default:
		return "disconnected"
	}
}

// EventKind tags an [Event].
type EventKind string

const (
	EventConnState      EventKind = "conn_state"      // connection state changed
	EventRosterChanged  EventKind = "roster_changed"  // a peer was added/removed/updated
	EventPresence       EventKind = "presence"        // a peer's presence changed
	EventMessage        EventKind = "message"         // a message was received and stored
	EventMessageState   EventKind = "message_state"   // an outbound message's state changed (e.g. delivered)
	EventKeyChanged     EventKind = "key_changed"     // a peer's keys changed; re-verification needed
	EventFileProgress   EventKind = "file_progress"   // a file transfer advanced
	EventError          EventKind = "error"           // a non-fatal error worth surfacing
	EventPendingChanged EventKind = "pending_changed" // the admin pending-device list may have changed

	// EventUpdateAvailable: the relay is running a newer version than this
	// client build. Soft notice only — the connection is unaffected.
	EventUpdateAvailable EventKind = "update_available"
	// EventUpdateRequired: the relay rejected this build as too old
	// (ErrClientTooOld). runLoop has already stopped retrying — the
	// connection will not come back until the binary is updated.
	EventUpdateRequired EventKind = "update_required"
)

// Event is a single notification for the UI. Only the fields relevant to Kind
// are populated.
type Event struct {
	Kind EventKind

	State         ConnState             // EventConnState
	PeerID        string                // EventPresence, EventKeyChanged, EventMessageState, EventFileProgress
	Presence      *proto.PresenceUpdate // EventPresence
	Message       *Message              // EventMessage
	MsgID         string                // EventMessageState
	MsgState      MessageState          // EventMessageState
	Progress      *FileProgress         // EventFileProgress
	Err           error                 // EventError
	ServerVersion string                // EventUpdateAvailable
}

// FileProgress reports how far a file transfer has got.
type FileProgress struct {
	TransferID string
	Name       string
	Direction  Direction
	Done       int64
	Total      int64
	Complete   bool
	Path       string // set on a completed inbound transfer
}

// emit sends an event, dropping it if no one is listening fast enough (the UI
// can always re-read state from the store).
func (c *Client) emit(ev Event) {
	select {
	case c.events <- ev:
	default:
	}
}

func (c *Client) emitConnState(s ConnState) {
	c.mu.Lock()
	c.state = s
	c.mu.Unlock()
	c.emit(Event{Kind: EventConnState, State: s})
}

func (c *Client) emitError(err error) {
	if err != nil {
		c.emit(Event{Kind: EventError, Err: err})
	}
}
