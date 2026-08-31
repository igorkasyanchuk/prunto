package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// TokenPrefix makes a leaked token recognisable in a log or a paste.
const TokenPrefix = "priito_"

type APIToken struct {
	ID         int64
	Label      string
	RevokedAt  *time.Time
	LastUsedAt *time.Time
	CreatedAt  time.Time
}

// MintToken returns the raw token exactly once. Only its digest is stored, so a lost token
// cannot be recovered - mint another.
func MintToken(ctx context.Context, db *sql.DB, label string) (string, error) {
	raw := TokenPrefix + randToken(24)
	_, err := db.ExecContext(ctx,
		`INSERT INTO api_tokens (label, token_digest, created_at) VALUES (?,?,?)`,
		label, digest(raw), time.Now().Unix())
	if err != nil {
		return "", err
	}
	return raw, nil
}

// Authenticate resolves a raw token to its row. Every upload records the token that made it,
// so abuse always has an owner to cut off.
//
// The lookup is by digest rather than by a constant-time comparison over every row: the token
// is 24 bytes of crypto/rand, so there is no low-entropy secret here for a timing signal to
// walk byte by byte, and an indexed lookup is what keeps this from scanning the table.
func (a *App) Authenticate(ctx context.Context, raw string) (sql.NullInt64, bool) {
	if raw == "" {
		return sql.NullInt64{}, false
	}
	var id int64
	err := a.DB.QueryRowContext(ctx,
		`SELECT id FROM api_tokens WHERE token_digest = ? AND revoked_at IS NULL`, digest(raw)).Scan(&id)
	if err != nil {
		return sql.NullInt64{}, false
	}
	if _, err := a.DB.ExecContext(ctx,
		`UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, time.Now().Unix(), id); err != nil {
		a.Log.Printf("could not stamp last_used_at on token %d: %v", id, err)
	}
	return sql.NullInt64{Int64: id, Valid: true}, true
}

// BearerToken reads the Authorization header and nothing else.
//
// A token in the query string leaks into access logs, browser history and Referer headers, so
// one that arrives there is not merely ignored - the request is refused, because a caller
// sending it that way has already leaked it and needs to be told.
var ErrTokenInQuery = errors.New("token in query string")

func BearerToken(header, query string) (string, error) {
	if query != "" {
		return "", ErrTokenInQuery
	}
	after, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return "", nil
	}
	return strings.TrimSpace(after), nil
}

func digest(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
