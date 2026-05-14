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

	return &DB{conn: conn}, nil
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
