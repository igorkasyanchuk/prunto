package main

import (
	"database/sql"
	"time"

	_ "modernc.org/sqlite"
)

// ponytail: SQLite, one file, no migration framework. The schema is small enough to be one
// idempotent statement list, and every query below is parameterised - string interpolation
// into SQL is the one shortcut that is never worth taking.
const schema = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;

CREATE TABLE IF NOT EXISTS api_tokens (
  id            INTEGER PRIMARY KEY,
  label         TEXT NOT NULL,
  token_digest  TEXT NOT NULL UNIQUE,
  revoked_at    INTEGER,
  last_used_at  INTEGER,
  created_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS uploads (
  id            INTEGER PRIMARY KEY,
  token         TEXT NOT NULL UNIQUE,
  delete_token  TEXT NOT NULL UNIQUE,
  object_key    TEXT NOT NULL,
  content_type  TEXT NOT NULL,
  content_hash  TEXT NOT NULL,
  byte_size     INTEGER NOT NULL,
  filename      TEXT,
  ip            TEXT,
  api_token_id  INTEGER REFERENCES api_tokens(id) ON DELETE SET NULL,
  expires_at    INTEGER NOT NULL, -- unix seconds, 0 = never
  created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS uploads_expires_at ON uploads(expires_at);
-- The admin dashboard sums bytes per token; without this the join scans uploads per token.
CREATE INDEX IF NOT EXISTS uploads_api_token_id ON uploads(api_token_id);

-- Append-only, and deliberately outlives the file it describes: an abuse report almost always
-- arrives after the content is gone, and without this there is nothing left to answer it with.
CREATE TABLE IF NOT EXISTS upload_events (
  id            INTEGER PRIMARY KEY,
  action        TEXT NOT NULL,
  content_hash  TEXT,
  ip            TEXT,
  api_token_id  INTEGER,
  content_type  TEXT,
  byte_size     INTEGER,
  created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS upload_events_created_at ON upload_events(created_at);

CREATE TABLE IF NOT EXISTS blocked_hashes (
  content_hash  TEXT PRIMARY KEY,
  reason        TEXT,
  created_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS abuse_reports (
  id            INTEGER PRIMARY KEY,
  url           TEXT NOT NULL,
  upload_token  TEXT,
  reason        TEXT NOT NULL,
  handled_at    INTEGER,
  created_at    INTEGER NOT NULL
);

-- Rate-limit and quota counters. Replaces Redis; SQLite serialises the writes, so the
-- read-modify-write below only has to be wrapped in a transaction to be atomic.
CREATE TABLE IF NOT EXISTS counters (
  key         TEXT PRIMARY KEY,
  value       INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL
);
`

func OpenDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// One writer. SQLite serialises writes anyway, and a single connection removes any chance
	// of a transaction landing on a different one mid-flight.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	return db, nil
}

func unix(t time.Time) int64 { return t.Unix() }

func nullTime(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.Unix(v.Int64, 0).UTC()
	return &t
}
