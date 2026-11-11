// Package store is the control plane's SQLite persistence layer.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

const schema = `
CREATE TABLE IF NOT EXISTS users (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, ssh_keys TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS join_tokens (
  token TEXT PRIMARY KEY, created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, used_by INTEGER);
CREATE TABLE IF NOT EXISTS nodes (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, secret_hash TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',          -- pending|ready|disabled
  trusted INTEGER NOT NULL DEFAULT 0,              -- our client cert is in the worker's Incus trust store
  incus_url TEXT NOT NULL DEFAULT '', incus_fingerprint TEXT NOT NULL DEFAULT '',
  ssh_host TEXT NOT NULL DEFAULT '', boot_id TEXT NOT NULL DEFAULT '',
  driver_version TEXT NOT NULL DEFAULT '', cuda_version TEXT NOT NULL DEFAULT '',
  cpu_count INTEGER NOT NULL DEFAULT 0, memory_mb INTEGER NOT NULL DEFAULT 0,
  inventory_error TEXT NOT NULL DEFAULT '', last_seen INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS gpus (
  id INTEGER PRIMARY KEY, node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  uuid TEXT NOT NULL UNIQUE, idx INTEGER NOT NULL, pci TEXT NOT NULL, model TEXT NOT NULL,
  memory_mb INTEGER NOT NULL, mig_capable INTEGER NOT NULL, mig_enabled INTEGER NOT NULL, mig_pending INTEGER NOT NULL DEFAULT 0,
  mode TEXT NOT NULL DEFAULT 'exclusive',          -- desired: exclusive|mig
  layout TEXT NOT NULL DEFAULT '[]',               -- desired []proto.LayoutEntry
  profiles TEXT NOT NULL DEFAULT '[]',             -- []proto.MigProfile
  status TEXT NOT NULL DEFAULT 'ready',            -- ready|applying|error|missing
  error TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS slices (
  id INTEGER PRIMARY KEY, gpu_id INTEGER NOT NULL REFERENCES gpus(id) ON DELETE CASCADE,
  kind TEXT NOT NULL,                              -- full|mig
  profile TEXT NOT NULL DEFAULT '', placement_start INTEGER NOT NULL DEFAULT 0,
  gi_id INTEGER NOT NULL DEFAULT -1, ci_id INTEGER NOT NULL DEFAULT -1, mig_uuid TEXT NOT NULL DEFAULT '',
  memory_mb INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL DEFAULT 'ready',             -- ready|missing
  UNIQUE(gpu_id, kind, profile, placement_start));
CREATE TABLE IF NOT EXISTS containers (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, user_id INTEGER NOT NULL REFERENCES users(id),
  node_id INTEGER NOT NULL REFERENCES nodes(id), image TEXT NOT NULL,
  cpu_limit INTEGER NOT NULL, memory_mb INTEGER NOT NULL, ssh_port INTEGER NOT NULL,
  desired_state TEXT NOT NULL DEFAULT 'running',   -- running|stopped|deleted
  state TEXT NOT NULL DEFAULT 'creating',          -- creating|running|stopped|error|deleted
  error TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL,
  UNIQUE(node_id, ssh_port));
CREATE TABLE IF NOT EXISTS assignments (
  slice_id INTEGER NOT NULL UNIQUE REFERENCES slices(id),
  container_id INTEGER NOT NULL REFERENCES containers(id) ON DELETE CASCADE,
  device_name TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS agent_tasks (
  id INTEGER PRIMARY KEY, node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  type TEXT NOT NULL, payload TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'queued',           -- queued|sent|done|failed
  output TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS operations (
  id INTEGER PRIMARY KEY, type TEXT NOT NULL, target_id INTEGER NOT NULL, node_id INTEGER NOT NULL,
  payload TEXT NOT NULL DEFAULT '{}',
  status TEXT NOT NULL DEFAULT 'queued',           -- queued|running|done|failed
  error TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS ops_status ON operations(status);
`

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: SQLite serialises writers anyway, and this rules out
	// SQLITE_BUSY between our own goroutines. Callers must not hold a Rows
	// open while issuing another query.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func now() int64 { return time.Now().Unix() }

// Tx runs fn in a transaction.
func (s *Store) Tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

type querier interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

func (s *Store) DB() querier { return s.db }

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
