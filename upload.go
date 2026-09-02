package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type Upload struct {
	ID          int64
	Token       string
	DeleteToken string
	ObjectKey   string
	ContentType string
	ContentHash string
	ByteSize    int64
	Filename    string
	IP          string
	APITokenID  sql.NullInt64
	TokenLabel  string // display only, filled by the admin listing
	ExpiresAt   time.Time
	CreatedAt   time.Time
}

func (u Upload) Video() bool { return strings.HasPrefix(u.ContentType, "video/") }

// Markdown is the string people paste. GitHub renders a <video> tag in a PR body but shows
// nothing for ![](clip.mp4), so the two types need different markup.
func (u Upload) Markdown(url string) string {
	if u.Video() {
		return fmt.Sprintf(`<video src=%q controls></video>`, url)
	}
	return "![](" + url + ")"
}

var retentionPattern = regexp.MustCompile(`^(\d+)([smhd]?)$`)

// RetentionFrom parses "30m", "6h", "7d" or a plain number of seconds. Anything longer than
// the default is capped rather than refused.
func RetentionFrom(value string) (time.Duration, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return DefaultRetention, nil
	}
	m := retentionPattern.FindStringSubmatch(value)
	if m == nil {
		return 0, ErrRejected{"expires_in should look like 30m, 6h or 7d"}
	}
	amount, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, ErrRejected{"expires_in is out of range"}
	}
	unit := map[string]time.Duration{
		"": time.Second, "s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour,
	}[m[2]]

	d := time.Duration(amount) * unit
	return min(max(d, MinRetention), DefaultRetention), nil
}

// CreateUpload validates, stores and records. Every ErrRejected it returns is meant to be
// shown to the uploader as-is.
func (a *App) CreateUpload(ctx context.Context, body []byte, filename, ip string, tokenID sql.NullInt64, expiresIn string) (Upload, error) {
	retention, err := RetentionFrom(expiresIn)
	if err != nil {
		return Upload{}, err
	}
	if len(body) == 0 {
		return Upload{}, ErrRejected{"The file is empty"}
	}
	// Second line of defence: the request body has already been read by the time we are here.
	// The real gate is the body-size limit on the proxy in front (see README).
	if len(body) > MaxBytes {
		return Upload{}, ErrRejected{fmt.Sprintf("The file is larger than %d MB", MaxBytes>>20)}
	}

	stored, kind, err := Sanitize(body)
	if err != nil {
		return Upload{}, err
	}

	sum := sha256.Sum256(stored)
	hash := hex.EncodeToString(sum[:])

	var blocked int
	if err := a.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM blocked_hashes WHERE content_hash = ?`, hash).Scan(&blocked); err != nil {
		return Upload{}, err
	}
	if blocked > 0 {
		a.RecordEvent(ctx, "rejected", hash, ip, tokenID, kind.ContentType, int64(len(stored)))
		return Upload{}, ErrRejected{"This file has been blocked"}
	}

	u := Upload{
		Token:       randToken(12),
		DeleteToken: randToken(24),
		ContentType: kind.ContentType,
		ContentHash: hash,
		ByteSize:    int64(len(stored)),
		Filename:    displayFilename(filename),
		IP:          ip,
		APITokenID:  tokenID,
		ExpiresAt:   time.Now().Add(retention).UTC(),
		CreatedAt:   time.Now().UTC(),
	}
	u.ObjectKey = u.Token + "." + kind.Ext

	if err := a.Store.Put(u.ObjectKey, stored); err != nil {
		return Upload{}, fmt.Errorf("storage: %w", err)
	}

	res, err := a.DB.ExecContext(ctx,
		`INSERT INTO uploads (token, delete_token, object_key, content_type, content_hash,
		                      byte_size, filename, ip, api_token_id, expires_at, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		u.Token, u.DeleteToken, u.ObjectKey, u.ContentType, u.ContentHash, u.ByteSize,
		u.Filename, u.IP, u.APITokenID, unix(u.ExpiresAt), unix(u.CreatedAt))
	if err != nil {
		// The file is already on disk; leave no orphan behind.
		_ = a.Store.Delete(u.ObjectKey)
		return Upload{}, err
	}
	u.ID, _ = res.LastInsertId()

	a.RecordEvent(ctx, "created", u.ContentHash, u.IP, u.APITokenID, u.ContentType, u.ByteSize)
	return u, nil
}

// Purge removes the object, records the action and drops the row. With block set, the hash is
// refused for any later upload.
func (a *App) Purge(ctx context.Context, u Upload, action string, block bool) error {
	if block {
		if _, err := a.DB.ExecContext(ctx,
			`INSERT OR IGNORE INTO blocked_hashes (content_hash, reason, created_at) VALUES (?,?,?)`,
			u.ContentHash, action, time.Now().Unix()); err != nil {
			return err
		}
	}
	if err := a.Store.Delete(u.ObjectKey); err != nil {
		// Leave the row alone so the next purge retries it.
		return err
	}
	a.RecordEvent(ctx, action, u.ContentHash, u.IP, u.APITokenID, u.ContentType, u.ByteSize)
	_, err := a.DB.ExecContext(ctx, `DELETE FROM uploads WHERE id = ?`, u.ID)
	return err
}

func (a *App) RecordEvent(ctx context.Context, action, hash, ip string, tokenID sql.NullInt64, contentType string, size int64) {
	if _, err := a.DB.ExecContext(ctx,
		`INSERT INTO upload_events (action, content_hash, ip, api_token_id, content_type, byte_size, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		action, hash, ip, tokenID, contentType, size, time.Now().Unix()); err != nil {
		a.Log.Printf("could not record %s event: %v", action, err)
	}
}

func (a *App) FindUploadBy(ctx context.Context, column, value string) (Upload, error) {
	var u Upload
	var filename, ip sql.NullString
	var expires, created int64
	err := a.DB.QueryRowContext(ctx,
		`SELECT id, token, delete_token, object_key, content_type, content_hash, byte_size,
		        filename, ip, api_token_id, expires_at, created_at
		 FROM uploads WHERE `+column+` = ?`, value).
		Scan(&u.ID, &u.Token, &u.DeleteToken, &u.ObjectKey, &u.ContentType, &u.ContentHash,
			&u.ByteSize, &filename, &ip, &u.APITokenID, &expires, &created)
	if err != nil {
		return Upload{}, err
	}
	u.Filename, u.IP = filename.String, ip.String
	u.ExpiresAt = time.Unix(expires, 0).UTC()
	u.CreatedAt = time.Unix(created, 0).UTC()
	return u, nil
}

// displayFilename keeps the client's name for display only. It never reaches the object key.
func displayFilename(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, name)
	name = truncate(name, 120)
	if name == "." || name == "/" {
		return ""
	}
	return name
}

// truncate cuts to at most limit bytes without splitting a rune in half, which would leave
// invalid UTF-8 in the database and replacement characters in the dashboard.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit]
}

func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error()) // unusable process; do not paper over it
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (a *App) FindUploadByID(ctx context.Context, id int64) (Upload, error) {
	return a.FindUploadBy(ctx, "id", strconv.FormatInt(id, 10))
}
