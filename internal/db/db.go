package db

import (
	"database/sql"
	"embed"
	_ "github.com/mattn/go-sqlite3"
)

//go:embed schema.sql
var schemaFS embed.FS

type DB struct {
	conn *sql.DB
}

func Open(path string) (*DB, error) {
	conn, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		return nil, err
	}

	schema, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return nil, err
	}

	if _, err := conn.Exec(string(schema)); err != nil {
		return nil, err
	}

	if err := migrate(conn); err != nil {
		return nil, err
	}

	return &DB{conn: conn}, nil
}

// migrate applies schema changes that CREATE TABLE IF NOT EXISTS can't make to
// an already-existing database.
func migrate(conn *sql.DB) error {
	added, err := addColumnIfMissing(conn, "files", "updated_at", "TEXT")
	if err != nil {
		return err
	}
	if added {
		// ALTER ... ADD COLUMN cannot take a non-constant (strftime) default, so
		// backfill existing rows with their best-known timestamp.
		_, err = conn.Exec(`UPDATE files SET updated_at =
			COALESCE(uploaded_at, queued_at, copied_at, discovered_at,
			         strftime('%Y-%m-%d %H:%M:%f','now'))
			WHERE updated_at IS NULL`)
		if err != nil {
			return err
		}
	}

	// Transfer timing columns (issue #1).
	if _, err := addColumnIfMissing(conn, "files", "ingest_ms", "INTEGER"); err != nil {
		return err
	}
	if _, err := addColumnIfMissing(conn, "files", "upload_ms", "INTEGER"); err != nil {
		return err
	}
	return nil
}

// addColumnIfMissing adds a column only if it isn't already present. table/col
// are internal constants (never user input), so string interpolation is safe.
func addColumnIfMissing(conn *sql.DB, table, col, decl string) (bool, error) {
	rows, err := conn.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == col {
			return false, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if _, err := conn.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + col + ` ` + decl); err != nil {
		return false, err
	}
	return true, nil
}

func (d *DB) Close() error {
	return d.conn.Close()
}

// Config
func (d *DB) SetConfig(key, value string) error {
	_, err := d.conn.Exec(
		`INSERT INTO config(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=CURRENT_TIMESTAMP`,
		key, value,
	)
	return err
}

func (d *DB) GetConfig(key string) (string, error) {
	var value string
	err := d.conn.QueryRow(`SELECT value FROM config WHERE key=?`, key).Scan(&value)
	return value, err
}

// Logs
func (d *DB) Log(level, component, message string) error {
	_, err := d.conn.Exec(
		`INSERT INTO logs(level, component, message) VALUES(?, ?, ?)`,
		level, component, message,
	)
	return err
}

func (d *DB) RecentLogs(limit int) ([]LogEntry, error) {
	rows, err := d.conn.Query(
		`SELECT id, ts, level, component, message FROM logs ORDER BY id DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []LogEntry
	for rows.Next() {
		var e LogEntry
		rows.Scan(&e.ID, &e.Ts, &e.Level, &e.Component, &e.Message)
		entries = append(entries, e)
	}
	return entries, nil
}

type LogEntry struct {
	ID        int64
	Ts        string
	Level     string
	Component string
	Message   string
}
