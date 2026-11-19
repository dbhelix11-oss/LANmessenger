package servercore

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"lanmessenger/internal/proto"
	"lanmessenger/internal/store"
)

// serverStore is the relay server's persistent state: the device directory,
// last-known presence, and the offline message queue.
type serverStore struct {
	db *sql.DB
}

var serverMigrations = []store.Migration{
	{
		Name: "0001_init",
		SQL: `
CREATE TABLE devices (
	device_id    TEXT PRIMARY KEY,
	display_name TEXT NOT NULL,
	sign_pub     TEXT NOT NULL,
	box_pub      TEXT NOT NULL,
	state        TEXT NOT NULL,             -- active | pending | denied
	admin        INTEGER NOT NULL DEFAULT 0,
	created_at   INTEGER NOT NULL,
	approved_at  INTEGER
);

CREATE TABLE presence (
	device_id  TEXT PRIMARY KEY REFERENCES devices(device_id) ON DELETE CASCADE,
	status     TEXT NOT NULL,
	message    TEXT NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL
);

CREATE TABLE queue (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	recipient   TEXT NOT NULL REFERENCES devices(device_id) ON DELETE CASCADE,
	sender      TEXT NOT NULL,
	msg_id      TEXT NOT NULL,
	nonce       TEXT NOT NULL,
	ciphertext  TEXT NOT NULL,
	ts          INTEGER NOT NULL,           -- sender clock, unix millis
	enqueued_at INTEGER NOT NULL,           -- server clock, unix seconds
	UNIQUE(recipient, sender, msg_id)
);
CREATE INDEX queue_recipient_idx ON queue(recipient, id);
`,
	},
	{
		Name: "0002_device_records",
		SQL: `
-- One row per connection; no foreign key so history survives device removal.
CREATE TABLE conn_log (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	device_id       TEXT NOT NULL,
	connected_at    INTEGER NOT NULL,
	disconnected_at INTEGER,
	path            TEXT NOT NULL DEFAULT '',
	remote_host     TEXT NOT NULL DEFAULT '',
	tls_hash        TEXT NOT NULL DEFAULT '',
	client_version  TEXT NOT NULL DEFAULT '',
	rtt_avg_ms      INTEGER NOT NULL DEFAULT 0,
	rtt_min_ms      INTEGER NOT NULL DEFAULT 0,
	rtt_max_ms      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX conn_log_device_idx ON conn_log(device_id, id);

CREATE TABLE device_facts (
	device_id  TEXT PRIMARY KEY,
	facts_json TEXT NOT NULL,
	verified   INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL
);

CREATE TABLE fact_changes (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	device_id TEXT NOT NULL,
	ts        INTEGER NOT NULL,
	field     TEXT NOT NULL,
	old       TEXT NOT NULL,
	new       TEXT NOT NULL
);
CREATE INDEX fact_changes_device_idx ON fact_changes(device_id, id);
`,
	},
	{
		Name: "0003_conn_client_kind",
		SQL: `
ALTER TABLE conn_log ADD COLUMN client_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE conn_log ADD COLUMN client_os   TEXT NOT NULL DEFAULT '';
ALTER TABLE conn_log ADD COLUMN client_arch TEXT NOT NULL DEFAULT '';
`,
	},
	{
		Name: "0004_revocation",
		SQL: `
ALTER TABLE devices ADD COLUMN revoked_at INTEGER;
-- Attempts by a revoked registration to return; kept after the device is deleted.
CREATE TABLE revoked_attempts (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	device_id   TEXT NOT NULL,
	ts          INTEGER NOT NULL,
	match       TEXT NOT NULL,
	path        TEXT NOT NULL DEFAULT '',
	remote_host TEXT NOT NULL DEFAULT '',
	tls_hash    TEXT NOT NULL DEFAULT '',
	client_kind TEXT NOT NULL DEFAULT '',
	client_os   TEXT NOT NULL DEFAULT '',
	signals     TEXT NOT NULL DEFAULT '',
	new_name    TEXT NOT NULL DEFAULT '',
	new_id      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX revoked_attempts_device_idx ON revoked_attempts(device_id, id);
`,
	},
}

func openServerStore(path string) (*serverStore, error) {
	db, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	if err := store.Migrate(db, serverMigrations); err != nil {
		db.Close()
		return nil, err
	}
	return &serverStore{db: db}, nil
}

func (s *serverStore) Close() error { return s.db.Close() }

// --- devices -------------------------------------------------------------

// upsertDevice inserts a new device or updates the keys/display name of an
// existing one. It never downgrades state.
func (s *serverStore) upsertDevice(e proto.DirectoryEntry) error {
	_, err := s.db.Exec(`
INSERT INTO devices (device_id, display_name, sign_pub, box_pub, state, admin, created_at)
VALUES (?, ?, ?, ?, ?, ?, unixepoch())
ON CONFLICT(device_id) DO UPDATE SET
	display_name = excluded.display_name,
	sign_pub     = excluded.sign_pub,
	box_pub      = excluded.box_pub`,
		e.DeviceID, e.DisplayName, e.SignPub, e.BoxPub, string(e.State), boolToInt(e.Admin))
	if err != nil {
		return fmt.Errorf("servercore: upsert device: %w", err)
	}
	return nil
}

var errNoDevice = errors.New("servercore: device not found")

func (s *serverStore) getDevice(id string) (proto.DirectoryEntry, error) {
	var e proto.DirectoryEntry
	var state string
	var admin int
	err := s.db.QueryRow(`
SELECT device_id, display_name, sign_pub, box_pub, state, admin
FROM devices WHERE device_id = ?`, id).
		Scan(&e.DeviceID, &e.DisplayName, &e.SignPub, &e.BoxPub, &state, &admin)
	if errors.Is(err, sql.ErrNoRows) {
		return e, errNoDevice
	}
	if err != nil {
		return e, fmt.Errorf("servercore: get device: %w", err)
	}
	e.State = proto.EnrollState(state)
	e.Admin = admin != 0
	return e, nil
}

func (s *serverStore) listDevicesByState(states ...proto.EnrollState) ([]proto.DirectoryEntry, error) {
	q := `SELECT device_id, display_name, sign_pub, box_pub, state, admin FROM devices`
	var args []any
	if len(states) > 0 {
		q += ` WHERE state IN (` + placeholders(len(states)) + `)`
		for _, st := range states {
			args = append(args, string(st))
		}
	}
	q += ` ORDER BY display_name COLLATE NOCASE`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("servercore: list devices: %w", err)
	}
	defer rows.Close()

	var out []proto.DirectoryEntry
	for rows.Next() {
		var e proto.DirectoryEntry
		var state string
		var admin int
		if err := rows.Scan(&e.DeviceID, &e.DisplayName, &e.SignPub, &e.BoxPub, &state, &admin); err != nil {
			return nil, fmt.Errorf("servercore: scan device: %w", err)
		}
		e.State = proto.EnrollState(state)
		e.Admin = admin != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *serverStore) countDevices() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM devices`).Scan(&n)
	return n, err
}

func (s *serverStore) setDeviceState(id string, state proto.EnrollState) error {
	var approvedAt any
	if state == proto.StateActive {
		approvedAt = time.Now().Unix()
	}
	res, err := s.db.Exec(`UPDATE devices SET state = ?, approved_at = ?, revoked_at = CASE WHEN ? = 'revoked' THEN unixepoch() ELSE NULL END WHERE device_id = ?`,
		string(state), approvedAt, string(state), id)
	if err != nil {
		return fmt.Errorf("servercore: set device state: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNoDevice
	}
	return nil
}

func (s *serverStore) setDisplayName(id, name string) error {
	res, err := s.db.Exec(`UPDATE devices SET display_name = ? WHERE device_id = ?`, name, id)
	if err != nil {
		return fmt.Errorf("servercore: set display name: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNoDevice
	}
	return nil
}

func (s *serverStore) setDeviceAdmin(id string, admin bool) error {
	_, err := s.db.Exec(`UPDATE devices SET admin = ? WHERE device_id = ?`, boolToInt(admin), id)
	return err
}

// --- presence ----------------------------------------------------------

func (s *serverStore) setPresence(id string, status proto.Status, message string) error {
	_, err := s.db.Exec(`
INSERT INTO presence (device_id, status, message, updated_at)
VALUES (?, ?, ?, unixepoch())
ON CONFLICT(device_id) DO UPDATE SET
	status = excluded.status, message = excluded.message, updated_at = excluded.updated_at`,
		id, string(status), message)
	if err != nil {
		return fmt.Errorf("servercore: set presence: %w", err)
	}
	return nil
}

type presenceRow struct {
	Status  proto.Status
	Message string
}

// getPresence returns the last-known presence for id. Missing rows default to
// available with no message.
func (s *serverStore) getPresence(id string) (presenceRow, error) {
	var r presenceRow
	var status string
	err := s.db.QueryRow(`SELECT status, message FROM presence WHERE device_id = ?`, id).
		Scan(&status, &r.Message)
	if errors.Is(err, sql.ErrNoRows) {
		return presenceRow{Status: proto.StatusAvailable}, nil
	}
	if err != nil {
		return r, fmt.Errorf("servercore: get presence: %w", err)
	}
	r.Status = proto.Status(status)
	return r, nil
}

// --- offline queue ---------------------------------------------------

type queuedMsg struct {
	Sender     string
	MsgID      string
	Nonce      string
	Ciphertext string
	TS         int64
}

// enqueue stores a message for a recipient that is offline. If maxPerDevice > 0
// and the queue is at the cap, the oldest entries are dropped to make room.
// Duplicate (recipient, sender, msg_id) rows are ignored.
func (s *serverStore) enqueue(recipient string, m queuedMsg, maxPerDevice int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("servercore: begin enqueue: %w", err)
	}
	defer tx.Rollback()

	if maxPerDevice > 0 {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM queue WHERE recipient = ?`, recipient).Scan(&n); err != nil {
			return fmt.Errorf("servercore: count queue: %w", err)
		}
		if n >= maxPerDevice {
			drop := n - maxPerDevice + 1
			if _, err := tx.Exec(`
DELETE FROM queue WHERE id IN (
	SELECT id FROM queue WHERE recipient = ? ORDER BY id LIMIT ?
)`, recipient, drop); err != nil {
				return fmt.Errorf("servercore: trim queue: %w", err)
			}
		}
	}

	if _, err := tx.Exec(`
INSERT OR IGNORE INTO queue (recipient, sender, msg_id, nonce, ciphertext, ts, enqueued_at)
VALUES (?, ?, ?, ?, ?, ?, unixepoch())`,
		recipient, m.Sender, m.MsgID, m.Nonce, m.Ciphertext, m.TS); err != nil {
		return fmt.Errorf("servercore: insert queue: %w", err)
	}
	return tx.Commit()
}

// drain returns every queued message for a recipient, oldest first.
func (s *serverStore) drain(recipient string) ([]queuedMsg, error) {
	rows, err := s.db.Query(`
SELECT sender, msg_id, nonce, ciphertext, ts FROM queue
WHERE recipient = ? ORDER BY id`, recipient)
	if err != nil {
		return nil, fmt.Errorf("servercore: drain queue: %w", err)
	}
	defer rows.Close()

	var out []queuedMsg
	for rows.Next() {
		var m queuedMsg
		if err := rows.Scan(&m.Sender, &m.MsgID, &m.Nonce, &m.Ciphertext, &m.TS); err != nil {
			return nil, fmt.Errorf("servercore: scan queued: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ackQueued removes one delivered message from a recipient's queue.
func (s *serverStore) ackQueued(recipient, sender, msgID string) error {
	_, err := s.db.Exec(`DELETE FROM queue WHERE recipient = ? AND sender = ? AND msg_id = ?`,
		recipient, sender, msgID)
	return err
}

// purgeExpired drops queued messages enqueued more than retention ago. A
// retention of 0 disables expiry.
func (s *serverStore) purgeExpired(retention time.Duration) (int64, error) {
	if retention <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-retention).Unix()
	res, err := s.db.Exec(`DELETE FROM queue WHERE enqueued_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("servercore: purge queue: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// --- helpers ---------------------------------------------------------

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, 0, 2*n)
	for i := 0; i < n; i++ {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '?')
	}
	return string(b)
}
