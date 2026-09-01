package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	t.Setenv("PRUNTO_BASE_URL", "http://prunto.test")
	t.Setenv("ADMIN_USER", "admin")
	t.Setenv("ADMIN_PASSWORD", "hunter2")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	db, err := OpenDB(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	store, err := NewStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &App{Config: cfg, DB: db, Store: store, Log: log.New(io.Discard, "", 0)}
}

func uploadRequest(t *testing.T, body []byte, filename, token string, fields map[string]string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	part.Write(body)
	mw.Close()

	r := httptest.NewRequest(http.MethodPost, "http://prunto.test/api/v1/uploads", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestUploadRoundTrip(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()
	token, err := CreateToken(context.Background(), app.DB, "test")
	if err != nil {
		t.Fatal(err)
	}

	// The filename claims PNG; the bytes are a GIF. The bytes decide.
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, uploadRequest(t, sampleGIF(t, 2), "screenshot.png", token, nil))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}

	var payload map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	url, _ := payload["url"].(string)
	if !strings.HasSuffix(url, ".gif") {
		t.Errorf("url = %q, want a .gif extension taken from the bytes", url)
	}
	if md, _ := payload["markdown"].(string); md != "!["+"]("+url+")" {
		t.Errorf("markdown = %q", md)
	}

	deleteURL, _ := payload["delete_url"].(string)
	if deleteURL == "" {
		t.Fatal("no delete_url returned")
	}
	r := httptest.NewRequest(http.MethodDelete, deleteURL, nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body = %s", w.Code, w.Body)
	}
}

func TestUploadRequiresAToken(t *testing.T) {
	app := newTestApp(t)
	w := httptest.NewRecorder()
	app.Routes().ServeHTTP(w, uploadRequest(t, samplePNG(t, 4, 4), "a.png", "", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

// A token in the query string has already leaked into access logs, browser history and
// Referer headers, so it is refused rather than quietly ignored.
func TestTokenInQueryStringIsRefused(t *testing.T) {
	app := newTestApp(t)
	token, _ := CreateToken(context.Background(), app.DB, "test")

	r := uploadRequest(t, samplePNG(t, 4, 4), "a.png", token, nil)
	r.URL.RawQuery = "token=" + token
	w := httptest.NewRecorder()
	app.Routes().ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Authorization header") {
		t.Errorf("the error does not explain why: %s", w.Body)
	}
}

func TestRevokedTokenIsRefused(t *testing.T) {
	app := newTestApp(t)
	token, _ := CreateToken(context.Background(), app.DB, "test")
	if _, err := app.DB.Exec(`UPDATE api_tokens SET revoked_at = 1`); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	app.Routes().ServeHTTP(w, uploadRequest(t, samplePNG(t, 4, 4), "a.png", token, nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestUploadsPerHourAreCapped(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()
	token, _ := CreateToken(context.Background(), app.DB, "test")

	for i := range UploadsPerHour {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, uploadRequest(t, samplePNG(t, 4, 4), "a.png", token, nil))
		if w.Code != http.StatusCreated {
			t.Fatalf("upload %d: status = %d, body = %s", i, w.Code, w.Body)
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, uploadRequest(t, samplePNG(t, 4, 4), "a.png", token, nil))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 on upload %d", w.Code, UploadsPerHour+1)
	}
}

func TestBlockedHashCannotBeReUploaded(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()
	token, _ := CreateToken(context.Background(), app.DB, "test")
	body := samplePNG(t, 6, 6)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, uploadRequest(t, body, "a.png", token, nil))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}

	upload, err := app.FindUploadBy(context.Background(), "id", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Purge(context.Background(), upload, "blocked", true); err != nil {
		t.Fatal(err)
	}

	// The same image again, this time with different metadata: the hash is taken after
	// stripping, so an EXIF tweak does not defeat the blocklist.
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, uploadRequest(t, body, "a.png", token, nil))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}
	if !strings.Contains(w.Body.String(), "blocked") {
		t.Errorf("body = %s", w.Body)
	}
}

func TestAdminIsClosedWhenUnconfigured(t *testing.T) {
	app := newTestApp(t)
	app.Config.AdminUser, app.Config.AdminPassword = "", ""

	w := httptest.NewRecorder()
	app.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://prunto.test/admin", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 - an unconfigured admin is closed, not open", w.Code)
	}
}

func TestAdminAuthAndOrigin(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()

	do := func(method, origin string, auth bool) int {
		var r *http.Request
		if method == http.MethodPost {
			r = httptest.NewRequest(method, "http://prunto.test/admin/actions",
				strings.NewReader("do=handle&id=1"))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		} else {
			r = httptest.NewRequest(method, "http://prunto.test/admin", nil)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if auth {
			r.SetBasicAuth("admin", "hunter2")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}

	if got := do(http.MethodGet, "", false); got != http.StatusUnauthorized {
		t.Errorf("no credentials = %d, want 401", got)
	}
	if got := do(http.MethodGet, "", true); got != http.StatusOK {
		t.Errorf("good credentials = %d, want 200", got)
	}
	// Basic auth is replayed by the browser on cross-site POSTs; without this check any page
	// on the internet could drive the dashboard.
	if got := do(http.MethodPost, "http://evil.test", true); got != http.StatusForbidden {
		t.Errorf("cross-origin POST = %d, want 403", got)
	}
	if got := do(http.MethodPost, "http://prunto.test", true); got != http.StatusSeeOther {
		t.Errorf("same-origin POST = %d, want 303", got)
	}
}

// The pages link the icon by path, and browsers ask for /favicon.ico regardless; both have to
// resolve or every page load logs a 404.
func TestFaviconIsServed(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()

	for _, path := range []string{"/static/favicon.svg", "/static/favicon-32.png", "/static/apple-touch-icon.png"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://prunto.test"+path, nil))
		if w.Code != http.StatusOK {
			t.Errorf("%s = %d, want 200", path, w.Code)
		}
	}

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://prunto.test/favicon.ico", nil))
	if w.Code != http.StatusMovedPermanently {
		t.Errorf("/favicon.ico = %d, want 301", w.Code)
	}

	// The drop page has to actually reference it, or the route is decoration.
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://prunto.test/", nil))
	if !strings.Contains(w.Body.String(), `rel="icon"`) {
		t.Error("the drop page does not link a favicon")
	}
}

// WebKit omits Origin on same-origin form submissions, so the dashboard has to recognise its
// own pages by Sec-Fetch-Site or Referer too - and still refuse everything cross-site.
func TestAdminOriginFallbacks(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()

	post := func(headers map[string]string) int {
		r := httptest.NewRequest(http.MethodPost, "http://prunto.test/admin/actions",
			strings.NewReader("do=handle&id=1"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		r.SetBasicAuth("admin", "hunter2")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}

	allowed := []struct {
		name    string
		headers map[string]string
	}{
		{"sec-fetch-site same-origin", map[string]string{"Sec-Fetch-Site": "same-origin"}},
		{"sec-fetch-site none", map[string]string{"Sec-Fetch-Site": "none"}},
		{"referer on this origin", map[string]string{"Referer": "http://prunto.test/admin"}},
		{"referer is the origin itself", map[string]string{"Referer": "http://prunto.test"}},
	}
	for _, c := range allowed {
		if got := post(c.headers); got != http.StatusSeeOther {
			t.Errorf("%s = %d, want 303", c.name, got)
		}
	}

	refused := []struct {
		name    string
		headers map[string]string
	}{
		{"no signal at all", nil},
		{"cross-site fetch metadata", map[string]string{"Sec-Fetch-Site": "cross-site"}},
		{"same-site is still another origin", map[string]string{"Sec-Fetch-Site": "same-site"}},
		{"foreign referer", map[string]string{"Referer": "http://evil.test/x"}},
		{"referer only prefix-matches the host", map[string]string{"Referer": "http://prunto.test.evil.test/x"}},
		{"origin wins over a friendly referer", map[string]string{
			"Origin": "http://evil.test", "Referer": "http://prunto.test/admin"}},
		{"fetch metadata wins over a friendly referer", map[string]string{
			"Sec-Fetch-Site": "cross-site", "Referer": "http://prunto.test/admin"}},
	}
	for _, c := range refused {
		if got := post(c.headers); got != http.StatusForbidden {
			t.Errorf("%s = %d, want 403", c.name, got)
		}
	}
}

// Behind Cloudflare, CF-Connecting-IP is the only address a client cannot forge. Falling back
// to X-Forwarded-For would turn every rate limit here into decoration.
func TestRefusesForgedForwardedFor(t *testing.T) {
	app := newTestApp(t)
	app.Config.TrustProxy = "cloudflare"
	token, _ := CreateToken(context.Background(), app.DB, "test")

	r := uploadRequest(t, samplePNG(t, 4, 4), "a.png", token, nil)
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	w := httptest.NewRecorder()
	app.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 when CF-Connecting-IP is absent", w.Code)
	}

	r = uploadRequest(t, samplePNG(t, 4, 4), "a.png", token, nil)
	r.Header.Set("CF-Connecting-IP", "1.2.3.4")
	w = httptest.NewRecorder()
	app.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d with CF-Connecting-IP set, body = %s", w.Code, w.Body)
	}
}

func TestResponsesCarryTheSecurityHeaders(t *testing.T) {
	app := newTestApp(t)
	w := httptest.NewRecorder()
	app.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://prunto.test/", nil))

	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
	if got := w.Header().Get("X-Robots-Tag"); !strings.Contains(got, "noindex") {
		t.Errorf("X-Robots-Tag = %q", got)
	}
}

func TestSkillIsRenderedWithThisInstancesHost(t *testing.T) {
	app := newTestApp(t)
	w := httptest.NewRecorder()
	app.Routes().ServeHTTP(w,
		httptest.NewRequest(http.MethodGet, "http://prunto.test/prunto-screenshot/SKILL.md", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "http://prunto.test/api/v1/uploads") {
		t.Error("the skill does not point at this instance")
	}
	// text/template, not html/template: HTML-escaping would mangle every code fence.
	if strings.Contains(body, "&amp;") || strings.Contains(body, "&#34;") {
		t.Error("the skill was HTML-escaped")
	}

	// Previously installed skills refresh themselves against the pre-rename path.
	w = httptest.NewRecorder()
	app.Routes().ServeHTTP(w,
		httptest.NewRequest(http.MethodGet, "http://prunto.test/priito-screenshot/SKILL.md", nil))
	if w.Code != http.StatusOK {
		t.Errorf("legacy skill path status = %d, want 200", w.Code)
	}
}

// The rename must not strand pre-rename deployments: the old env var still configures the
// base URL, and an existing priito.db is adopted rather than abandoned for an empty DB.
func TestRenameCompatShims(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	t.Setenv("PRUNTO_BASE_URL", "")
	t.Setenv("PRIITO_BASE_URL", "https://old.example.com")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "https://old.example.com" {
		t.Fatalf("BaseURL = %q, want the PRIITO_BASE_URL fallback", cfg.BaseURL)
	}

	old := filepath.Join(cfg.DataDir, "priito.db")
	if err := os.WriteFile(old, []byte("not empty"), 0o600); err != nil {
		t.Fatal(err)
	}
	adoptRenamedDB(cfg, log.New(io.Discard, "", 0))
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("priito.db was not renamed")
	}
	if b, err := os.ReadFile(cfg.DBPath()); err != nil || string(b) != "not empty" {
		t.Errorf("prunto.db = %q, %v; want the adopted file", b, err)
	}

	// A schema-only prunto.db — what the shimless rename release created on boot — must be
	// moved aside and adopted over, not treated as the database.
	if err := os.Remove(cfg.DBPath()); err != nil {
		t.Fatal(err)
	}
	db, err := OpenDB(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := os.WriteFile(old, []byte("old data"), 0o600); err != nil {
		t.Fatal(err)
	}
	adoptRenamedDB(cfg, log.New(io.Discard, "", 0))
	if b, _ := os.ReadFile(cfg.DBPath()); string(b) != "old data" {
		t.Error("an empty prunto.db was not adopted over")
	}
	if _, err := os.Stat(cfg.DBPath() + ".empty"); err != nil {
		t.Error("the empty prunto.db was not preserved aside")
	}

	// A prunto.db with data wins; priito.db is left in place for the operator.
	if err := os.Remove(cfg.DBPath()); err != nil {
		t.Fatal(err)
	}
	db, err = OpenDB(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CreateToken(context.Background(), db, "keep"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := os.WriteFile(old, []byte("older"), 0o600); err != nil {
		t.Fatal(err)
	}
	adoptRenamedDB(cfg, log.New(io.Discard, "", 0))
	if _, err := os.Stat(old); err != nil {
		t.Error("a populated prunto.db must leave priito.db in place")
	}
	if !dbHasData(cfg.DBPath()) {
		t.Error("the populated prunto.db was replaced")
	}
}

func TestPurgeRemovesExpiredUploads(t *testing.T) {
	app := newTestApp(t)
	token, _ := CreateToken(context.Background(), app.DB, "test")

	w := httptest.NewRecorder()
	app.Routes().ServeHTTP(w, uploadRequest(t, samplePNG(t, 4, 4), "a.png", token,
		map[string]string{"expires_in": "1m"}))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	if _, err := app.DB.Exec(`UPDATE uploads SET expires_at = 1`); err != nil {
		t.Fatal(err)
	}

	app.PurgeExpired(context.Background())

	var remaining int
	app.DB.QueryRow(`SELECT COUNT(*) FROM uploads`).Scan(&remaining)
	if remaining != 0 {
		t.Fatalf("%d uploads survived the purge", remaining)
	}
	// The audit trail deliberately outlives the file it describes.
	var events int
	app.DB.QueryRow(`SELECT COUNT(*) FROM upload_events WHERE action = 'purged'`).Scan(&events)
	if events != 1 {
		t.Fatalf("purge events = %d, want 1", events)
	}
}

// The pool is capped at one connection, so resolving each report's upload while the report
// cursor is still open waited forever for the connection that cursor held. It only showed up
// with a report whose upload still exists, which is the case an operator actually sees.
func TestAdminLoadsWithAnOpenReport(t *testing.T) {
	app := newTestApp(t)
	token, _ := CreateToken(context.Background(), app.DB, "test")

	w := httptest.NewRecorder()
	app.Routes().ServeHTTP(w, uploadRequest(t, samplePNG(t, 4, 4), "a.png", token, nil))
	if w.Code != http.StatusCreated {
		t.Fatalf("upload status = %d", w.Code)
	}
	upload, err := app.FindUploadBy(context.Background(), "id", "1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.DB.Exec(
		`INSERT INTO abuse_reports (url, upload_token, reason, created_at) VALUES (?,?,?,?)`,
		"http://cdn.test/"+upload.ObjectKey, upload.Token, "bad", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	done := make(chan int, 1)
	go func() {
		r := httptest.NewRequest(http.MethodGet, "http://prunto.test/admin", nil)
		r.SetBasicAuth("admin", "hunter2")
		rec := httptest.NewRecorder()
		app.Routes().ServeHTTP(rec, r)
		done <- rec.Code
	}()

	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("/admin hung: a nested query starved on the single connection")
	}
}

// A browser seeking within a <video> asks for a byte range; answering 200 with the whole body
// leaves the scrubber dead.
func TestLocalBlobServesRanges(t *testing.T) {
	app := newTestApp(t)
	token, _ := CreateToken(context.Background(), app.DB, "test")

	w := httptest.NewRecorder()
	app.Routes().ServeHTTP(w, uploadRequest(t, samplePNG(t, 4, 4), "a.png", token, nil))
	if w.Code != http.StatusCreated {
		t.Fatalf("upload status = %d", w.Code)
	}
	var payload map[string]any
	json.Unmarshal(w.Body.Bytes(), &payload)
	url, _ := payload["url"].(string)

	r := httptest.NewRequest(http.MethodGet, url, nil)
	r.Header.Set("Range", "bytes=0-7")
	w = httptest.NewRecorder()
	app.Routes().ServeHTTP(w, r)

	if w.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", w.Code)
	}
	if got := w.Body.Len(); got != 8 {
		t.Fatalf("body = %d bytes, want 8", got)
	}
}

// The drop page previews the upload straight off the CDN, which is a different origin in every
// real deployment. A CSP that only names 'self' leaves that preview blocked in production and
// working locally, where the CDN is this host.
func TestDropPageCSPNamesTheCDNOrigin(t *testing.T) {
	app := newTestApp(t)
	app.Config.CDNBaseURL = "https://cdn.example.test/blobs"

	w := httptest.NewRecorder()
	app.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://prunto.test/", nil))

	csp := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{
		"img-src 'self' blob: data: https://cdn.example.test;",
		"media-src 'self' blob: https://cdn.example.test;",
	} {
		if !strings.Contains(csp, want) {
			t.Fatalf("CSP %q does not contain %q", csp, want)
		}
	}
	if strings.Contains(csp, "/blobs") {
		t.Fatalf("CSP carries a path, which only matches that exact path: %q", csp)
	}
}

// A token in a URL lands in browser history and in every access log in front of the origin,
// which is what the API refuses a request over. Creating one must not put it there.
func TestNewTokenIsNeverInTheURL(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()

	form := strings.NewReader("do=create&label=laptop")
	r := httptest.NewRequest(http.MethodPost, "http://prunto.test/admin/actions", form)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", app.Config.BaseURL)
	r.SetBasicAuth("admin", "hunter2")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("create returned %d, want %d", w.Code, http.StatusSeeOther)
	}
	if loc := w.Header().Get("Location"); loc != "/admin" {
		t.Fatalf("the redirect carries the token: %q", loc)
	}

	var created *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == newTokenCookieName {
			created = c
		}
	}
	if created == nil || !strings.HasPrefix(created.Value, TokenPrefix) {
		t.Fatalf("no created token handed back in a cookie: %v", created)
	}
	if !created.HttpOnly || created.SameSite != http.SameSiteStrictMode {
		t.Fatalf("the created-token cookie is not locked down: %+v", created)
	}

	// Shown once: the dashboard renders it and clears the cookie in the same response.
	r = httptest.NewRequest(http.MethodGet, "http://prunto.test/admin", nil)
	r.AddCookie(created)
	r.SetBasicAuth("admin", "hunter2")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if !strings.Contains(w.Body.String(), created.Value) {
		t.Fatal("the dashboard did not show the created token")
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == newTokenCookieName && c.MaxAge >= 0 {
			t.Fatalf("the created-token cookie was not cleared: %+v", c)
		}
	}
}

// PRUNTO_BASE_URL is compared against an Origin header and used to derive the CSP's CDN source,
// so a value that is not a bare origin has to stop the boot rather than silently disable both.
func TestBaseURLMustBeABareOrigin(t *testing.T) {
	notOrigins := []string{
		"prunto.test", "https://prunto.test/sub", "ftp://prunto.test", "https://",
		"https://prunto.test?v=2", "https://prunto.test#x", "https://user@prunto.test",
	}
	for _, bad := range notOrigins {
		t.Run(bad, func(t *testing.T) {
			t.Setenv("DATA_DIR", t.TempDir())
			t.Setenv("PRUNTO_BASE_URL", bad)
			if _, err := LoadConfig(); err == nil {
				t.Fatalf("LoadConfig accepted %q", bad)
			}
		})
	}
	t.Setenv("DATA_DIR", t.TempDir())
	t.Setenv("PRUNTO_BASE_URL", "https://prunto.test/")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CDNOrigin() != "https://prunto.test" {
		t.Fatalf("CDNOrigin = %q", cfg.CDNOrigin())
	}
}
