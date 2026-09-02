package main

import (
	"context"
	"database/sql"
	"time"
)

// bump adds delta to a counter that resets after window, and returns the new value.
//
// The claim happens before the check so two uploads racing on the same token cannot both read
// the same figure and both pass. A rejected upload hands its claim straight back via a
// negative delta.
// peek reads a counter without writing anything. Missing or expired reads as zero.
func peek(ctx context.Context, db *sql.DB, key string) (int64, error) {
	var value int64
	err := db.QueryRowContext(ctx,
		`SELECT value FROM counters WHERE key = ? AND expires_at > ?`, key, time.Now().Unix()).Scan(&value)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return value, err
}

// extend pushes a counter's expiry out to now+window, leaving its value alone.
func extend(ctx context.Context, db *sql.DB, key string, window time.Duration) error {
	_, err := db.ExecContext(ctx, `UPDATE counters SET expires_at = ? WHERE key = ?`,
		time.Now().Add(window).Unix(), key)
	return err
}

func clear(ctx context.Context, db *sql.DB, key string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM counters WHERE key = ?`, key)
	return err
}

func bump(ctx context.Context, db *sql.DB, key string, delta int64, window time.Duration) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	var value, expires int64
	err = tx.QueryRowContext(ctx, `SELECT value, expires_at FROM counters WHERE key = ?`, key).
		Scan(&value, &expires)
	switch {
	case err == sql.ErrNoRows || (err == nil && expires <= now):
		value, expires = 0, now+int64(window.Seconds())
	case err != nil:
		return 0, err
	}

	value += delta
	if value < 0 {
		value = 0
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO counters(key, value, expires_at) VALUES(?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, expires_at = excluded.expires_at`,
		key, value, expires); err != nil {
		return 0, err
	}
	return value, tx.Commit()
}
