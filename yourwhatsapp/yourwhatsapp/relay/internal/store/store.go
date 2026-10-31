// Package store owns the SQLite database shared by whatsmeow and the relay.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"go.mau.fi/whatsmeow/store/sqlstore"
	_ "modernc.org/sqlite"
)

type Store struct {
	DB        *sql.DB
	Container *sqlstore.Container
}

const schema = `
CREATE TABLE IF NOT EXISTS accounts (
	session_id TEXT PRIMARY KEY,
	jid TEXT,
	proxy TEXT,
	paused INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE TABLE IF NOT EXISTS sends (
	local_message_id TEXT PRIMARY KEY,
	session_id TEXT NOT NULL,
	state TEXT NOT NULL CHECK (state IN ('queued','sent','failed')),
	message_id TEXT,
	error_code TEXT,
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
	updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE TABLE IF NOT EXISTS outbox (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	method TEXT NOT NULL,
	payload TEXT NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	next_attempt_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
	last_error TEXT,
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE TABLE IF NOT EXISTS outbox_dead (
	id INTEGER PRIMARY KEY,
	method TEXT NOT NULL,
	payload TEXT NOT NULL,
	attempts INTEGER NOT NULL,
	next_attempt_at TEXT NOT NULL,
	last_error TEXT,
	created_at TEXT NOT NULL,
	dead_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE TABLE IF NOT EXISTS relay_config (
	id INTEGER PRIMARY KEY CHECK(id=1),
	api_key TEXT NOT NULL,
	callback_url TEXT NOT NULL,
	site_name TEXT NOT NULL,
	claimed_at TEXT NOT NULL
);
`

// Open creates dataDir, opens relay.db, applies whatsmeow's migrations, then our tables.
func Open(ctx context.Context, dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	dsn := "file:" + filepath.Join(dataDir, "relay.db") +
		"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)

	container := sqlstore.NewWithDB(db, "sqlite3", nil)
	if err := container.Upgrade(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("upgrade whatsmeow store: %w", err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create relay tables: %w", err)
	}
	return &Store{DB: db, Container: container}, nil
}

func (s *Store) Close() error {
	return s.DB.Close()
}
