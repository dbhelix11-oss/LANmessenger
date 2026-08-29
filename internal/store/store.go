// Package store provides a thin helper for opening the SQLite databases used by
// both the relay server and the client core, with consistent pragmas and a
// minimal forward-only migration runner.
package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // pure-Go driver, registered as "sqlite"
)

// Open opens (creating if needed) the SQLite database at path and applies the
// pragmas we rely on everywhere: WAL journaling for concurrent readers, enforced
// foreign keys, and a busy timeout so brief lock contention retries instead of
// failing.
func Open(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// modernc's driver serializes writes internally; a single connection avoids
	// "database is locked" churn under WAL for our modest workloads.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}
	return db, nil
}

// Migration is one forward-only schema step. Name is recorded once applied so it
// never runs twice.
type Migration struct {
	Name string
	SQL  string
}

// Migrate applies any migrations not yet recorded in the schema_migrations
// table, in slice order, each in its own transaction.
func Migrate(db *sql.DB, migrations []Migration) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		name       TEXT PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}

	applied := map[string]bool{}
	rows, err := db.Query(`SELECT name FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("store: read schema_migrations: %w", err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return fmt.Errorf("store: scan migration name: %w", err)
		}
		applied[n] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: iterate migrations: %w", err)
	}
	rows.Close()

	for _, m := range migrations {
		if applied[m.Name] {
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("store: begin %q: %w", m.Name, err)
		}
		if _, err := tx.Exec(m.SQL); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: apply %q: %w", m.Name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (name, applied_at) VALUES (?, unixepoch())`, m.Name); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: record %q: %w", m.Name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: commit %q: %w", m.Name, err)
		}
	}
	return nil
}
