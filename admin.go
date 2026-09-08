package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"net/http"
	"strconv"
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
		// Authenticate first. The CSRF check below calls ParseForm, and reading an
		// attacker-supplied body is work worth doing only for a caller who has already
		// proved who they are.
		ip, ipOK := a.requireIP(w, r, false)
		if !ipOK {
			return
		}
		// AdminLoginAttempts wrong passwords from one address and it is refused for
		// AdminLockout, right answer or not. The read is a plain SELECT: this runs on every
		// dashboard request and must not cost a write. A limiter that cannot be read fails
		// closed - the dashboard needs the same database a moment later anyway.
		failKey := "admin-fail:" + ip
		failures, err := peek(r.Context(), a.DB, failKey)
		if err != nil {
			a.Log.Printf("admin login limit: %v", err)
			http.Error(w, "Try again later", http.StatusServiceUnavailable)
			return
		}
		if failures >= AdminLoginAttempts {
			http.Error(w, "Too many failed logins from this address, try again later",
				http.StatusTooManyRequests)
			return
		}
		user, pass, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(a.Config.AdminUser)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(a.Config.AdminPassword)) == 1
		if !ok || !userOK || !passOK {
			// A browser's first, credential-less request is not an attempt.
			if ok {
				a.recordAdminFailure(r.Context(), failKey)
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="prunto"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		// A successful login forgives earlier typos; otherwise nine of them linger for the
		// rest of the window and the tenth locks the admin out.
		if failures > 0 {
			if err := clear(r.Context(), a.DB, failKey); err != nil {
				a.Log.Printf("admin login limit: %v", err)
			}
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
		// The dashboard renders a freshly created token in plaintext and carries the CSRF
		// token in every form. Neither belongs in a disk cache or a shared proxy.
		w.Header().Set("Cache-Control", "no-store")
		next(w, r)
	}
}

// recordAdminFailure counts a wrong password. bump's window is anchored at the first failure,
// so on its own the tenth failure at 14m59s would lock the address for one second; when the
// threshold is reached the expiry is pushed out so the lockout lasts the full AdminLockout.
func (a *App) recordAdminFailure(ctx context.Context, key string) {
	n, err := bump(ctx, a.DB, key, 1, AdminLockout)
	if err == nil && n >= AdminLoginAttempts {
		err = extend(ctx, a.DB, key, AdminLockout)
	}
	if err != nil {
		a.Log.Printf("admin login limit: %v", err)
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
		Secure:   a.Config.HTTPS(),
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
	Uploads    int   // files stored right now, not lifetime
	Bytes      int64 // same: what is on the volume for this token
}

// adminStats is what is on the volume right now: live rows only, so it tracks the disk.
type adminStats struct {
	Uploads int
	Bytes   int64
	Tokens  int
	Blocked int
}

// Counted here rather than with len() over the listings, which stop at 100 rows. Only rows
// that would still be served count: a file past expires_at that the sweep has not reached is
// already refused by handleBlob, so it should not be on the dashboard's disk figure either.
//
// ponytail: one full scan of uploads per dashboard load for the totals; activeTokens' per-token
// sums ride the uploads_api_token_id index. Fine at self-hosted scale.
func (a *App) stats(ctx context.Context) (adminStats, error) {
	var s adminStats
	err := a.DB.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(byte_size), 0),
		        (SELECT COUNT(*) FROM api_tokens WHERE revoked_at IS NULL),
		        (SELECT COUNT(*) FROM blocked_hashes)
		 FROM uploads WHERE expires_at = 0 OR expires_at > ?`, time.Now().Unix()).
		Scan(&s.Uploads, &s.Bytes, &s.Tokens, &s.Blocked)
	return s, err
}

// AdminPageSize is the row count per page for the uploads and audit tables.
const AdminPageSize = 50

// pager is one paged table's position: which page, and whether there is a page either side.
// Offset paging on a single-writer SQLite file: no counts, the query fetches one row past the
// page to learn whether "Older" exists.
type pager struct {
	Page       int
	Prev, Next int // 0 when there is no such page
}

// pageParam reads ?name=N, treating anything unparseable or below 1 as page 1.
func pageParam(r *http.Request, name string) int {
	n, _ := strconv.Atoi(r.URL.Query().Get(name))
	return max(n, 1)
}

// paged trims a page-plus-one fetch to the page and reports what lies either side.
func paged[T any](rows []T, page int) ([]T, pager) {
	p := pager{Page: page, Prev: page - 1}
	if len(rows) > AdminPageSize {
		rows, p.Next = rows[:AdminPageSize], page+1
	}
	return rows, p
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
	uploads, err := a.recentUploads(ctx, pageParam(r, "uploads"))
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
	events, err := a.recentEvents(ctx, pageParam(r, "events"))
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

	stats, err := a.stats(ctx)
	if err != nil {
		a.Log.Printf("admin: stats: %v", err)
		http.Error(w, "Could not load the dashboard", http.StatusInternalServerError)
		return
	}

	view["Stats"] = stats
	view["Reports"] = reports
	view["Uploads"], view["UploadsPager"] = paged(uploads, pageParam(r, "uploads"))
	view["Tokens"] = tokens
	view["Events"], view["EventsPager"] = paged(events, pageParam(r, "events"))
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
		Secure:   a.Config.HTTPS(),
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

// recentUploads returns up to AdminPageSize+1 rows for the page; the extra row, if present,
// tells paged() there is an older page.
func (a *App) recentUploads(ctx context.Context, page int) ([]Upload, error) {
	rows, err := a.DB.QueryContext(ctx,
		`SELECT u.id, u.token, u.delete_token, u.object_key, u.content_type, u.content_hash,
		        u.byte_size, COALESCE(u.filename, ''), COALESCE(u.ip, ''), u.api_token_id,
		        COALESCE(t.label, ''), u.expires_at, u.created_at
		 FROM uploads u LEFT JOIN api_tokens t ON t.id = u.api_token_id
		 ORDER BY u.created_at DESC, u.id DESC LIMIT ? OFFSET ?`,
		AdminPageSize+1, (page-1)*AdminPageSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Upload
	for rows.Next() {
		var u Upload
		var expires, created int64
		if err := rows.Scan(&u.ID, &u.Token, &u.DeleteToken, &u.ObjectKey, &u.ContentType,
			&u.ContentHash, &u.ByteSize, &u.Filename, &u.IP, &u.APITokenID, &u.TokenLabel,
			&expires, &created); err != nil {
			return nil, err
		}
		u.ExpiresAt = expiresTime(expires)
		u.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, u)
	}
	return out, rows.Err()
}

func (a *App) activeTokens(ctx context.Context) ([]adminToken, error) {
	rows, err := a.DB.QueryContext(ctx,
		`SELECT t.id, t.label, t.last_used_at, t.created_at, COUNT(u.id), COALESCE(SUM(u.byte_size), 0)
		 FROM api_tokens t LEFT JOIN uploads u ON u.api_token_id = t.id AND (u.expires_at = 0 OR u.expires_at > ?)
		 WHERE t.revoked_at IS NULL
		 GROUP BY t.id ORDER BY t.created_at DESC LIMIT 100`, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []adminToken
	for rows.Next() {
		var t adminToken
		var lastUsed sql.NullInt64
		var created int64
		if err := rows.Scan(&t.ID, &t.Label, &lastUsed, &created, &t.Uploads, &t.Bytes); err != nil {
			return nil, err
		}
		t.LastUsedAt = nullTime(lastUsed)
		t.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, t)
	}
	return out, rows.Err()
}

func (a *App) recentEvents(ctx context.Context, page int) ([]adminEvent, error) {
	rows, err := a.DB.QueryContext(ctx,
		`SELECT action, COALESCE(content_hash, ''), COALESCE(ip, ''), COALESCE(content_type, ''),
		        COALESCE(byte_size, 0), created_at
		 FROM upload_events ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`,
		AdminPageSize+1, (page-1)*AdminPageSize)
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
