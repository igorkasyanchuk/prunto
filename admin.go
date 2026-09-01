package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// guard wraps an admin handler with HTTP basic auth and a same-origin check.
//
// With either credential unset the dashboard returns 403: an unconfigured admin is closed,
// not open.
func (a *App) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.Config.AdminEnabled() {
			http.Error(w, "ADMIN_USER and ADMIN_PASSWORD are not set, so /admin is closed",
				http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost {
			if !a.notCrossSite(r) {
				http.Error(w, "Bad origin", http.StatusForbidden)
				return
			}
			if !a.csrfTokenValid(r) {
				http.Error(w, "That form is stale. Reload /admin and try again.",
					http.StatusForbidden)
				return
			}
		}
		user, pass, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(a.Config.AdminUser)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(a.Config.AdminPassword)) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="prunto"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// Basic auth is replayed by the browser on cross-site POSTs, so it is a CSRF carrier on its
// own and the dashboard needs its own check. Request headers turned out to be the wrong place
// to look for one: WebKit omits Origin on same-origin form submissions, Sec-Fetch-Site is
// absent on older browsers, and Referer cannot be the fallback because withSecurityHeaders
// sends `Referrer-Policy: no-referrer` on the very page holding the form. A browser with none
// of the three then looks identical to an attacker. So the token below is the actual check,
// and the header tests are kept only to reject what they can prove is cross-site.

// notCrossSite rejects a request whose own headers say it came from somewhere else. It never
// accepts on its own - a request with no headers at all still has to carry the CSRF token.
func (a *App) notCrossSite(r *http.Request) bool {
	// "null" is a withheld origin, not a foreign one. withSecurityHeaders sends
	// Referrer-Policy: no-referrer, and on a form navigation that makes the browser serialise
	// Origin as null - so the dashboard's own forms arrive this way and rejecting it locks the
	// admin out of every button. It proves nothing in either direction, since an attacker's
	// page can set the same policy and produce the same value, so treat it as absent and let
	// the CSRF token below be the thing that decides.
	if o := r.Header.Get("Origin"); o != "" && o != "null" && o != a.Config.BaseURL {
		return false
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
		return true
	default:
		// cross-site, or same-site from another host on the registrable domain.
		return false
	}
}

// csrfCookie holds the double-submit token. It is not a session: it only has to be a value an
// attacker's page cannot read (it is HttpOnly and same-origin) and therefore cannot echo back
// in a form field.
const csrfCookie = "prunto_csrf"

// issueCSRFToken returns the token for this browser, minting and setting one if needed. The
// dashboard calls it on every render so a form is never served without a matching cookie.
func (a *App) issueCSRFToken(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(csrfCookie); err == nil && c.Value != "" {
		return c.Value
	}
	token := randToken(32)
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookie,
		Value:    token,
		Path:     "/admin",
		MaxAge:   12 * 60 * 60,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   strings.HasPrefix(a.Config.BaseURL, "https://"),
	})
	return token
}

func (a *App) csrfTokenValid(r *http.Request) bool {
	c, err := r.Cookie(csrfCookie)
	if err != nil || c.Value == "" {
		return false
	}
	// ParseForm here rather than in the handler: the guard has to see the field before the
	// action runs, and ParseForm is idempotent.
	if err := r.ParseForm(); err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(r.PostFormValue("csrf"))) == 1
}

type adminReport struct {
	ID          int64
	URL         string
	UploadToken string
	Reason      string
	CreatedAt   time.Time
	Upload      *Upload
}

type adminToken struct {
	ID         int64
	Label      string
	LastUsedAt *time.Time
	CreatedAt  time.Time
	Uploads    int
}

type adminEvent struct {
	Action      string
	ContentHash string
	IP          string
	ContentType string
	ByteSize    int64
	CreatedAt   time.Time
}

func (a *App) handleAdmin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	view := a.view()

	reports, err := a.openReports(ctx)
	if err != nil {
		a.Log.Printf("admin: reports: %v", err)
		http.Error(w, "Could not load the dashboard", http.StatusInternalServerError)
		return
	}
	uploads, err := a.recentUploads(ctx)
	if err != nil {
		a.Log.Printf("admin: uploads: %v", err)
		http.Error(w, "Could not load the dashboard", http.StatusInternalServerError)
		return
	}
	tokens, err := a.activeTokens(ctx)
	if err != nil {
		a.Log.Printf("admin: tokens: %v", err)
		http.Error(w, "Could not load the dashboard", http.StatusInternalServerError)
		return
	}
	events, err := a.recentEvents(ctx)
	if err != nil {
		a.Log.Printf("admin: events: %v", err)
		http.Error(w, "Could not load the dashboard", http.StatusInternalServerError)
		return
	}
	blocked, err := a.blockedHashes(ctx)
	if err != nil {
		a.Log.Printf("admin: blocked hashes: %v", err)
		http.Error(w, "Could not load the dashboard", http.StatusInternalServerError)
		return
	}

	view["Reports"] = reports
	view["Uploads"] = uploads
	view["Tokens"] = tokens
	view["Events"] = events
	view["Blocked"] = blocked
	view["NewToken"] = a.takeNewToken(w, r)
	view["CSRF"] = a.issueCSRFToken(w, r)
	a.render(w, "admin.html", view)
}

func (a *App) handleAdminAction(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "That form could not be read", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	id := r.PostFormValue("id")

	var err error
	switch r.PostFormValue("do") {
	case "remove", "remove_block":
		block := r.PostFormValue("do") == "remove_block"
		action := "deleted"
		if block {
			action = "blocked"
		}
		var upload Upload
		if upload, err = a.FindUploadBy(ctx, "id", id); err == nil {
			err = a.Purge(ctx, upload, action, block)
		}
	case "revoke":
		_, err = a.DB.ExecContext(ctx,
			`UPDATE api_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
			time.Now().Unix(), id)
	case "handle":
		_, err = a.DB.ExecContext(ctx,
			`UPDATE abuse_reports SET handled_at = ? WHERE id = ?`, time.Now().Unix(), id)
	case "unblock":
		_, err = a.DB.ExecContext(ctx, `DELETE FROM blocked_hashes WHERE content_hash = ?`, id)
	case "create":
		label := r.PostFormValue("label")
		if label == "" {
			label = "unnamed"
		}
		var raw string
		if raw, err = CreateToken(ctx, a.DB, label); err == nil {
			http.SetCookie(w, a.newTokenCookie(raw, 60))
		}
	default:
		http.Error(w, "Unknown action", http.StatusBadRequest)
		return
	}

	if err == sql.ErrNoRows {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	if err != nil {
		a.Log.Printf("admin action %q: %v", r.PostFormValue("do"), err)
		http.Error(w, "That action failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// newTokenCookieName carries a freshly created token across the redirect to /admin.
//
// Not the query string: a token in a URL lands in browser history and in the access log of
// every proxy in front of this origin, which is precisely what the API refuses a request over
// (see ErrTokenInQuery). The cookie is read once and cleared on the render that shows it.
const newTokenCookieName = "prunto_new_token"

func (a *App) takeNewToken(w http.ResponseWriter, r *http.Request) string {
	c, err := r.Cookie(newTokenCookieName)
	if err != nil {
		return ""
	}
	http.SetCookie(w, a.newTokenCookie("", -1))
	return c.Value
}

func (a *App) newTokenCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     newTokenCookieName,
		Value:    value,
		Path:     "/admin",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   strings.HasPrefix(a.Config.BaseURL, "https://"),
	}
}

func (a *App) openReports(ctx context.Context) ([]adminReport, error) {
	rows, err := a.DB.QueryContext(ctx,
		`SELECT id, url, COALESCE(upload_token, ''), reason, created_at
		 FROM abuse_reports WHERE handled_at IS NULL ORDER BY created_at DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []adminReport
	for rows.Next() {
		var r adminReport
		var created int64
		if err := rows.Scan(&r.ID, &r.URL, &r.UploadToken, &r.Reason, &created); err != nil {
			return nil, err
		}
		r.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Resolving the uploads happens only once the cursor above is closed. The pool is capped at
	// a single connection, so querying while these rows are still open would wait forever for
	// the connection this very cursor holds.
	rows.Close()
	for i := range out {
		// The upload is usually already gone; that is expected, not an error.
		if out[i].UploadToken == "" {
			continue
		}
		if u, err := a.FindUploadBy(ctx, "token", out[i].UploadToken); err == nil {
			out[i].Upload = &u
		}
	}
	return out, nil
}

func (a *App) recentUploads(ctx context.Context) ([]Upload, error) {
	rows, err := a.DB.QueryContext(ctx,
		`SELECT id, token, delete_token, object_key, content_type, content_hash, byte_size,
		        COALESCE(filename, ''), COALESCE(ip, ''), api_token_id, expires_at, created_at
		 FROM uploads ORDER BY created_at DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Upload
	for rows.Next() {
		var u Upload
		var expires, created int64
		if err := rows.Scan(&u.ID, &u.Token, &u.DeleteToken, &u.ObjectKey, &u.ContentType,
			&u.ContentHash, &u.ByteSize, &u.Filename, &u.IP, &u.APITokenID, &expires, &created); err != nil {
			return nil, err
		}
		u.ExpiresAt = time.Unix(expires, 0).UTC()
		u.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, u)
	}
	return out, rows.Err()
}

func (a *App) activeTokens(ctx context.Context) ([]adminToken, error) {
	rows, err := a.DB.QueryContext(ctx,
		`SELECT t.id, t.label, t.last_used_at, t.created_at, COUNT(u.id)
		 FROM api_tokens t LEFT JOIN uploads u ON u.api_token_id = t.id
		 WHERE t.revoked_at IS NULL
		 GROUP BY t.id ORDER BY t.created_at DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []adminToken
	for rows.Next() {
		var t adminToken
		var lastUsed sql.NullInt64
		var created int64
		if err := rows.Scan(&t.ID, &t.Label, &lastUsed, &created, &t.Uploads); err != nil {
			return nil, err
		}
		t.LastUsedAt = nullTime(lastUsed)
		t.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, t)
	}
	return out, rows.Err()
}

func (a *App) recentEvents(ctx context.Context) ([]adminEvent, error) {
	rows, err := a.DB.QueryContext(ctx,
		`SELECT action, COALESCE(content_hash, ''), COALESCE(ip, ''), COALESCE(content_type, ''),
		        COALESCE(byte_size, 0), created_at
		 FROM upload_events ORDER BY created_at DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []adminEvent
	for rows.Next() {
		var e adminEvent
		var created int64
		if err := rows.Scan(&e.Action, &e.ContentHash, &e.IP, &e.ContentType, &e.ByteSize, &created); err != nil {
			return nil, err
		}
		e.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

func (a *App) blockedHashes(ctx context.Context) ([]adminEvent, error) {
	rows, err := a.DB.QueryContext(ctx,
		`SELECT content_hash, COALESCE(reason, ''), created_at
		 FROM blocked_hashes ORDER BY created_at DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []adminEvent
	for rows.Next() {
		var e adminEvent
		var created int64
		if err := rows.Scan(&e.ContentHash, &e.Action, &created); err != nil {
			return nil, err
		}
		e.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
	case n >= 1<<10:
		return strconv.FormatInt(n/(1<<10), 10) + " KB"
	default:
		return strconv.FormatInt(n, 10) + " B"
	}
}
