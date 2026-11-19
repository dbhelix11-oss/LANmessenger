package servercore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"lanmessenger/internal/proto"
)

// maxConnHistory is how many recent connections a device report carries.
const maxConnHistory = 20

// maxConnLogRows caps stored connection rows per device so the table cannot
// grow without bound on a small router disk.
const maxConnLogRows = 500

// openConnRecord inserts a connection row and returns its id.
func (s *serverStore) openConnRecord(deviceID string, r proto.ConnRecord) (int64, error) {
	res, err := s.db.Exec(`
INSERT INTO conn_log (device_id, connected_at, path, remote_host, tls_hash, client_version,
                      client_kind, client_os, client_arch)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, deviceID, r.ConnectedAt, r.Path, r.RemoteHost, r.TLSHash,
		r.ClientVersion, r.ClientKind, r.ClientOS, r.ClientArch)
	if err != nil {
		return 0, fmt.Errorf("servercore: open conn record: %w", err)
	}
	id, _ := res.LastInsertId()
	_, _ = s.db.Exec(`
DELETE FROM conn_log WHERE device_id = ? AND id NOT IN
	(SELECT id FROM conn_log WHERE device_id = ? ORDER BY id DESC LIMIT ?)`,
		deviceID, deviceID, maxConnLogRows)
	return id, nil
}

// closeConnRecord stamps the end time and round-trip stats on a row.
func (s *serverStore) closeConnRecord(id int64, at int64, avg, min, max int) error {
	_, err := s.db.Exec(`
UPDATE conn_log SET disconnected_at = ?, rtt_avg_ms = ?, rtt_min_ms = ?, rtt_max_ms = ? WHERE id = ?`,
		at, avg, min, max, id)
	return err
}

// recordFacts stores a device's declared facts, logging any field that changed
// since the previous report.
func (s *serverStore) recordFacts(deviceID string, f proto.ClientFacts, verified bool, now int64) error {
	f.Sig = ""
	var prevJSON string
	err := s.db.QueryRow(`SELECT facts_json FROM device_facts WHERE device_id = ?`, deviceID).Scan(&prevJSON)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		var prev proto.ClientFacts
		if json.Unmarshal([]byte(prevJSON), &prev) == nil {
			for _, ch := range diffFacts(prev, f) {
				if _, e := s.db.Exec(`INSERT INTO fact_changes (device_id, ts, field, old, new) VALUES (?, ?, ?, ?, ?)`,
					deviceID, now, ch[0], ch[1], ch[2]); e != nil {
					return e
				}
			}
		}
	}
	raw, _ := json.Marshal(f)
	_, err = s.db.Exec(`
INSERT INTO device_facts (device_id, facts_json, verified, updated_at) VALUES (?, ?, ?, ?)
ON CONFLICT(device_id) DO UPDATE SET facts_json = excluded.facts_json,
	verified = excluded.verified, updated_at = excluded.updated_at`,
		deviceID, string(raw), boolToInt(verified), now)
	return err
}

// diffFacts returns [field, old, new] triples for each changed field.
func diffFacts(a, b proto.ClientFacts) [][3]string {
	var out [][3]string
	add := func(name, x, y string) {
		if x != y {
			out = append(out, [3]string{name, x, y})
		}
	}
	add("version", a.Version, b.Version)
	add("os", a.OS, b.OS)
	add("arch", a.Arch, b.Arch)
	add("build_hash", a.BuildHash, b.BuildHash)
	add("model", a.Model, b.Model)
	add("tz", a.TZ, b.TZ)
	return out
}

// deviceRecords fills the history/facts part of a DeviceReport.
func (s *serverStore) deviceRecords(deviceID string, rep *proto.DeviceReport) error {
	var factsJSON string
	var verified int
	err := s.db.QueryRow(`SELECT facts_json, verified, updated_at FROM device_facts WHERE device_id = ?`, deviceID).
		Scan(&factsJSON, &verified, &rep.FactsAt)
	if err == nil {
		var f proto.ClientFacts
		if json.Unmarshal([]byte(factsJSON), &f) == nil {
			rep.Facts = &f
			rep.FactsVerified = verified != 0
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	rows, err := s.db.Query(`SELECT ts, field, old, new FROM fact_changes WHERE device_id = ? ORDER BY id DESC LIMIT 20`, deviceID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var c proto.FactChange
		if err := rows.Scan(&c.TS, &c.Field, &c.Old, &c.New); err != nil {
			rows.Close()
			return err
		}
		rep.FactChanges = append(rep.FactChanges, c)
	}
	rows.Close()

	if err := s.db.QueryRow(`SELECT COUNT(*) FROM conn_log WHERE device_id = ?`, deviceID).Scan(&rep.ConnCount); err != nil {
		return err
	}
	crow, err := s.db.Query(`
SELECT connected_at, COALESCE(disconnected_at, 0), path, remote_host, tls_hash, client_version,
       client_kind, client_os, client_arch, rtt_avg_ms, rtt_min_ms, rtt_max_ms
FROM conn_log WHERE device_id = ? ORDER BY id DESC LIMIT ?`, deviceID, maxConnHistory)
	if err != nil {
		return err
	}
	defer crow.Close()
	for crow.Next() {
		var c proto.ConnRecord
		if err := crow.Scan(&c.ConnectedAt, &c.DisconnectedAt, &c.Path, &c.RemoteHost, &c.TLSHash,
			&c.ClientVersion, &c.ClientKind, &c.ClientOS, &c.ClientArch, &c.RTTAvgMs, &c.RTTMinMs, &c.RTTMaxMs); err != nil {
			return err
		}
		rep.Conns = append(rep.Conns, c)
	}
	if err := crow.Err(); err != nil {
		return err
	}

	// Distinct hashes and paths across the whole retained history.
	for _, q := range []struct {
		col string
		dst *[]string
	}{{"tls_hash", &rep.TLSHashes}, {"path", &rep.Paths}} {
		drows, err := s.db.Query(`SELECT DISTINCT `+q.col+` FROM conn_log WHERE device_id = ? AND `+q.col+` != ''`, deviceID)
		if err != nil {
			return err
		}
		for drows.Next() {
			var v string
			if err := drows.Scan(&v); err != nil {
				drows.Close()
				return err
			}
			*q.dst = append(*q.dst, v)
		}
		drows.Close()
		sort.Strings(*q.dst)
	}
	return nil
}

// deviceTimes returns created_at and approved_at (0 if never approved).
func (s *serverStore) deviceTimes(id string) (created, approved int64) {
	var ap sql.NullInt64
	_ = s.db.QueryRow(`SELECT created_at, approved_at FROM devices WHERE device_id = ?`, id).Scan(&created, &ap)
	if ap.Valid {
		approved = ap.Int64
	}
	return
}

// removeDevice deletes the device row (presence and queue cascade).
func (s *serverStore) removeDevice(id string) error {
	res, err := s.db.Exec(`DELETE FROM devices WHERE device_id = ?`, id)
	if err != nil {
		return fmt.Errorf("servercore: remove device: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNoDevice
	}
	return nil
}

// recordRevokedAttempt stores one attempt resembling a revoked registration and
// reports how many have been made against it in all.
func (s *serverStore) recordRevokedAttempt(deviceID, match, signals, newName, newID string, c *conn, now int64) (int, error) {
	if _, err := s.db.Exec(`
INSERT INTO revoked_attempts (device_id, ts, match, path, remote_host, tls_hash, client_kind, client_os, signals, new_name, new_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, deviceID, now, match, c.path, c.remoteHost, c.tlsHash, c.clientKind, c.clientOS, signals, newName, newID); err != nil {
		return 0, fmt.Errorf("servercore: record revoked attempt: %w", err)
	}
	_, _ = s.db.Exec(`
DELETE FROM revoked_attempts WHERE device_id = ? AND id NOT IN
	(SELECT id FROM revoked_attempts WHERE device_id = ? ORDER BY id DESC LIMIT 200)`, deviceID, deviceID)
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM revoked_attempts WHERE device_id = ?`, deviceID).Scan(&n)
	return n, err
}

// revocationInfo fills the revocation fields of a report.
func (s *serverStore) revocationInfo(deviceID string, rep *proto.DeviceReport) {
	var at sql.NullInt64
	_ = s.db.QueryRow(`SELECT revoked_at FROM devices WHERE device_id = ?`, deviceID).Scan(&at)
	if at.Valid {
		rep.RevokedAt = at.Int64
	}
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM revoked_attempts WHERE device_id = ?`, deviceID).Scan(&rep.RevokeAttempts)
	_ = s.db.QueryRow(`SELECT ts, match, signals, new_name FROM revoked_attempts WHERE device_id = ? ORDER BY id DESC LIMIT 1`, deviceID).
		Scan(&rep.LastRevokeAttempt, &rep.LastRevokeMatch, &rep.LastRevokeSignals, &rep.LastRevokeName)
}

// revokedProfile is what the relay remembers of how a revoked device used to connect.
type revokedProfile struct {
	ID, Name string
	tls      map[string]bool // TLS handshake shapes
	client   map[string]bool // "kind|os|arch"
	hosts    map[string]bool // LAN addresses (meaningless over the tunnel, so only "lan" rows)
}

// revokedProfiles returns the connection history of every revoked device.
func (s *serverStore) revokedProfiles() ([]revokedProfile, error) {
	rows, err := s.db.Query(`
SELECT d.device_id, d.display_name, l.tls_hash, l.client_kind, l.client_os, l.client_arch, l.remote_host, l.path
FROM devices d JOIN conn_log l ON l.device_id = d.device_id WHERE d.state = 'revoked'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := map[string]*revokedProfile{}
	var order []string
	for rows.Next() {
		var id, name, tls, kind, osn, arch, host, path string
		if err := rows.Scan(&id, &name, &tls, &kind, &osn, &arch, &host, &path); err != nil {
			return nil, err
		}
		p := byID[id]
		if p == nil {
			p = &revokedProfile{ID: id, Name: name, tls: map[string]bool{}, client: map[string]bool{}, hosts: map[string]bool{}}
			byID[id] = p
			order = append(order, id)
		}
		if tls != "" {
			p.tls[tls] = true
		}
		if kind != "" || osn != "" {
			p.client[kind+"|"+osn+"|"+arch] = true
		}
		if path == "lan" && host != "" {
			p.hosts[host] = true
		}
	}
	out := make([]revokedProfile, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out, rows.Err()
}

// fingerprintSignals lists which parts of this connection resemble a revoked device's.
func fingerprintSignals(p revokedProfile, c *conn) []string {
	var sig []string
	if c.tlsHash != "" && p.tls[c.tlsHash] {
		sig = append(sig, "tls")
	}
	if (c.clientKind != "" || c.clientOS != "") && p.client[c.clientKind+"|"+c.clientOS+"|"+c.clientArch] {
		sig = append(sig, "client")
	}
	if c.path == "lan" && c.remoteHost != "" && p.hosts[c.remoteHost] {
		sig = append(sig, "address")
	}
	return sig
}
