package clientcore

import (
	"database/sql"
	"errors"
	"fmt"

	"lanmessenger/internal/proto"
	"lanmessenger/internal/store"
)

// Direction marks whether a stored message was sent or received.
type Direction string

const (
	DirIn  Direction = "in"
	DirOut Direction = "out"
)

// MessageState tracks delivery for outbound messages (and is "received" for
// inbound).
type MessageState string

const (
	StateReceived  MessageState = "received"
	StateSent      MessageState = "sent"      // handed to the relay
	StateDelivered MessageState = "delivered" // peer acked
	StateQueued    MessageState = "queued"    // composed offline, in the outbox
	StateFailed    MessageState = "failed"
)

// Message is one row of conversation history.
type Message struct {
	MsgID     string
	PeerID    string
	Direction Direction
	Kind      proto.InnerKind
	Body      string // text for InnerText; JSON for file transfers
	TS        int64  // sender clock, unix milliseconds
	State     MessageState
}

// Peer is a cached directory entry plus local trust state.
type Peer struct {
	DeviceID    string
	DisplayName string
	SignPub     string
	BoxPub      string
	State       proto.EnrollState
	Admin       bool
	Verified    bool // user confirmed the fingerprint out of band
	KeyChanged  bool // keys changed since first seen; needs re-verification
}

type clientStore struct {
	db *sql.DB
}

var clientMigrations = []store.Migration{
	{
		Name: "0001_init",
		SQL: `
CREATE TABLE peers (
	device_id    TEXT PRIMARY KEY,
	display_name TEXT NOT NULL,
	sign_pub     TEXT NOT NULL,
	box_pub      TEXT NOT NULL,
	state        TEXT NOT NULL,
	admin        INTEGER NOT NULL DEFAULT 0,
	verified     INTEGER NOT NULL DEFAULT 0,
	key_changed  INTEGER NOT NULL DEFAULT 0,
	first_seen   INTEGER NOT NULL
);

CREATE TABLE messages (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	msg_id     TEXT NOT NULL,
	peer_id    TEXT NOT NULL,
	direction  TEXT NOT NULL,          -- in | out
	kind       TEXT NOT NULL,          -- text | file_offer | ...
	body       TEXT NOT NULL,
	ts         INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	state      TEXT NOT NULL,
	UNIQUE(msg_id, peer_id, direction)
);
CREATE INDEX messages_peer_idx ON messages(peer_id, id);
`,
	},
	{
		Name: "0002_outbox",
		SQL: `
CREATE TABLE outbox (
	msg_id     TEXT PRIMARY KEY,
	peer_id    TEXT NOT NULL,
	nonce      TEXT NOT NULL,
	ciphertext TEXT NOT NULL,
	ts         INTEGER NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX outbox_order_idx ON outbox(created_at);
`,
	},
}

func openClientStore(path string) (*clientStore, error) {
	db, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	if err := store.Migrate(db, clientMigrations); err != nil {
		db.Close()
		return nil, err
	}
	return &clientStore{db: db}, nil
}

func (s *clientStore) Close() error { return s.db.Close() }

// --- peers -------------------------------------------------------------

// upsertPeer inserts or updates a peer from a directory entry. If the peer
// already exists with different keys, key_changed is set and verified is
// cleared so the UI can prompt for re-verification.
func (s *clientStore) upsertPeer(e proto.DirectoryEntry) (keyChanged bool, err error) {
	prev, err := s.getPeer(e.DeviceID)
	switch {
	case errors.Is(err, errNoPeer):
		_, err = s.db.Exec(`
INSERT INTO peers (device_id, display_name, sign_pub, box_pub, state, admin, first_seen)
VALUES (?, ?, ?, ?, ?, ?, unixepoch())`,
			e.DeviceID, e.DisplayName, e.SignPub, e.BoxPub, string(e.State), boolToInt(e.Admin))
		return false, err
	case err != nil:
		return false, err
	}

	keyChanged = prev.SignPub != e.SignPub || prev.BoxPub != e.BoxPub
	kc := prev.KeyChanged || keyChanged
	verified := prev.Verified && !keyChanged
	_, err = s.db.Exec(`
UPDATE peers SET display_name = ?, sign_pub = ?, box_pub = ?, state = ?, admin = ?,
	verified = ?, key_changed = ?
WHERE device_id = ?`,
		e.DisplayName, e.SignPub, e.BoxPub, string(e.State), boolToInt(e.Admin),
		boolToInt(verified), boolToInt(kc), e.DeviceID)
	return keyChanged, err
}

var errNoPeer = errors.New("clientcore: peer not found")

func (s *clientStore) getPeer(id string) (Peer, error) {
	var p Peer
	var admin, verified, keyChanged int
	var state string
	err := s.db.QueryRow(`
SELECT device_id, display_name, sign_pub, box_pub, state, admin, verified, key_changed
FROM peers WHERE device_id = ?`, id).
		Scan(&p.DeviceID, &p.DisplayName, &p.SignPub, &p.BoxPub, &state, &admin, &verified, &keyChanged)
	if errors.Is(err, sql.ErrNoRows) {
		return p, errNoPeer
	}
	if err != nil {
		return p, fmt.Errorf("clientcore: get peer: %w", err)
	}
	p.State = proto.EnrollState(state)
	p.Admin = admin != 0
	p.Verified = verified != 0
	p.KeyChanged = keyChanged != 0
	return p, nil
}

func (s *clientStore) listPeers() ([]Peer, error) {
	rows, err := s.db.Query(`
SELECT device_id, display_name, sign_pub, box_pub, state, admin, verified, key_changed
FROM peers ORDER BY display_name COLLATE NOCASE`)
	if err != nil {
		return nil, fmt.Errorf("clientcore: list peers: %w", err)
	}
	defer rows.Close()

	var out []Peer
	for rows.Next() {
		var p Peer
		var admin, verified, keyChanged int
		var state string
		if err := rows.Scan(&p.DeviceID, &p.DisplayName, &p.SignPub, &p.BoxPub, &state, &admin, &verified, &keyChanged); err != nil {
			return nil, fmt.Errorf("clientcore: scan peer: %w", err)
		}
		p.State = proto.EnrollState(state)
		p.Admin = admin != 0
		p.Verified = verified != 0
		p.KeyChanged = keyChanged != 0
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *clientStore) removePeer(id string) error {
	_, err := s.db.Exec(`DELETE FROM peers WHERE device_id = ?`, id)
	return err
}

func (s *clientStore) setPeerVerified(id string, v bool) error {
	res, err := s.db.Exec(`UPDATE peers SET verified = ?, key_changed = 0 WHERE device_id = ?`, boolToInt(v), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNoPeer
	}
	return nil
}

// --- messages --------------------------------------------------------

// insertMessage stores a message, ignoring exact duplicates (same msg_id +
// peer + direction). It reports whether a new row was inserted.
func (s *clientStore) insertMessage(m Message) (inserted bool, err error) {
	res, err := s.db.Exec(`
INSERT OR IGNORE INTO messages (msg_id, peer_id, direction, kind, body, ts, created_at, state)
VALUES (?, ?, ?, ?, ?, ?, unixepoch(), ?)`,
		m.MsgID, m.PeerID, string(m.Direction), string(m.Kind), m.Body, m.TS, string(m.State))
	if err != nil {
		return false, fmt.Errorf("clientcore: insert message: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *clientStore) setMessageState(msgID, peerID string, dir Direction, st MessageState) error {
	_, err := s.db.Exec(`
UPDATE messages SET state = ? WHERE msg_id = ? AND peer_id = ? AND direction = ?`,
		string(st), msgID, peerID, string(dir))
	return err
}

// listMessages returns up to limit most-recent messages with a peer, oldest
// first.
func (s *clientStore) listMessages(peerID string, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Query(`
SELECT msg_id, peer_id, direction, kind, body, ts, state FROM (
	SELECT * FROM messages WHERE peer_id = ? ORDER BY id DESC LIMIT ?
) ORDER BY id ASC`, peerID, limit)
	if err != nil {
		return nil, fmt.Errorf("clientcore: list messages: %w", err)
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		var m Message
		var dir, kind, st string
		if err := rows.Scan(&m.MsgID, &m.PeerID, &dir, &kind, &m.Body, &m.TS, &st); err != nil {
			return nil, fmt.Errorf("clientcore: scan message: %w", err)
		}
		m.Direction = Direction(dir)
		m.Kind = proto.InnerKind(kind)
		m.State = MessageState(st)
		out = append(out, m)
	}
	return out, rows.Err()
}

// --- outbox (messages composed while offline) -----------------------

// outboxRow is one sealed message waiting for the next connection.
type outboxRow struct {
	MsgID      string
	PeerID     string
	Nonce      string
	Ciphertext string
	TS         int64
}

func (s *clientStore) enqueueOutbox(r outboxRow) error {
	_, err := s.db.Exec(`
INSERT OR REPLACE INTO outbox (msg_id, peer_id, nonce, ciphertext, ts, created_at)
VALUES (?, ?, ?, ?, ?, unixepoch())`,
		r.MsgID, r.PeerID, r.Nonce, r.Ciphertext, r.TS)
	return err
}

func (s *clientStore) listOutbox() ([]outboxRow, error) {
	rows, err := s.db.Query(`
SELECT msg_id, peer_id, nonce, ciphertext, ts FROM outbox ORDER BY created_at, rowid`)
	if err != nil {
		return nil, fmt.Errorf("clientcore: list outbox: %w", err)
	}
	defer rows.Close()

	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		if err := rows.Scan(&r.MsgID, &r.PeerID, &r.Nonce, &r.Ciphertext, &r.TS); err != nil {
			return nil, fmt.Errorf("clientcore: scan outbox: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *clientStore) deleteOutbox(msgID string) error {
	_, err := s.db.Exec(`DELETE FROM outbox WHERE msg_id = ?`, msgID)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
