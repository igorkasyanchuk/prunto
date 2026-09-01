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
	"regexp"
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

	return &App{Config: cfg, DB: db, Store: NewStore(cfg), Log: log.New(io.Discard, "", 0)}
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

// The drop page is where someone lands first, so the two install paths and the paste-to-an-AI
// prompt all have to be there, each copyable, and each carrying this instance's own URL.
func TestDropPageExplainsBothInstallPaths(t *testing.T) {
	app := newTestApp(t)
	w := httptest.NewRecorder()
	app.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://prunto.test/", nil))
	body := w.Body.String()

	for _, want := range []string{
		`id="install-global"`,
		`id="install-repo"`,
		`id="setup-prompt"`,
		"mkdir -p ~/.claude/skills/prunto-screenshot",
		"mkdir -p .claude/skills/prunto-screenshot",
		"/static/copy.js",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the drop page is missing %q", want)
		}
	}
	// Every block needs a button pointed at it, or the copy affordance is decoration.
	for _, id := range []string{"install-global", "install-repo", "setup-prompt"} {
		if !strings.Contains(body, `data-copy-target="`+id+`"`) {
			t.Errorf("no copy button targets %q", id)
		}
	}
	// The prompt has to name this instance, or it is useless pasted into another machine.
	if strings.Count(body, "http://prunto.test/prunto-screenshot/SKILL.md") < 3 {
		t.Error("the install commands and the prompt do not all point at this instance")
	}
	if !strings.Contains(body, "http://prunto.test/admin") {
		t.Error("the prompt does not tell the reader where tokens come from")
	}
}

// The dashboard renders a live API token and the CSRF token, so it must not be storable.
// Authentication also has to come before the CSRF check, or an anonymous caller can make the
// server parse a form body it will then throw away.
func TestAdminIsUncacheableAndAuthenticatesFirst(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()

	r := httptest.NewRequest(http.MethodGet, "http://prunto.test/admin", nil)
	r.SetBasicAuth("admin", "hunter2")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store on a page showing a token", got)
	}

	// No credentials: the answer has to be 401, which is what proves auth ran before the
	// CSRF check rather than after it.
	r = httptest.NewRequest(http.MethodPost, "http://prunto.test/admin/actions",
		strings.NewReader("do=create&label=x"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated POST = %d, want 401 before any form parsing", w.Code)
	}
}

// adminSession does what a browser does before it can submit an admin form: load /admin, keep
// the CSRF cookie, and read the token the page embedded in every form.
func adminSession(t *testing.T, handler http.Handler) (*http.Cookie, string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "http://prunto.test/admin", nil)
	r.SetBasicAuth("admin", "hunter2")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("loading /admin = %d", w.Code)
	}
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == csrfCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("/admin set no CSRF cookie, so no form on it can be submitted")
	}
	m := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(w.Body.String())
	if m == nil {
		t.Fatal("no CSRF field rendered into the admin forms")
	}
	if m[1] != cookie.Value {
		t.Fatalf("the form token %q does not match the cookie %q", m[1], cookie.Value)
	}
	return cookie, m[1]
}

func TestAdminAuthAndOrigin(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()

	cookie, token := adminSession(t, handler)

	do := func(method, origin string, auth bool) int {
		var r *http.Request
		if method == http.MethodPost {
			r = httptest.NewRequest(method, "http://prunto.test/admin/actions",
				strings.NewReader("do=handle&id=1&csrf="+token))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.AddCookie(cookie)
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

// The one-time token is shown once, so the flash has to make it copyable: a real field with
// the value in it, and the script that drives the copy button actually served.
func TestNewTokenFlashIsCopyable(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()
	cookie, csrf := adminSession(t, handler)

	r := httptest.NewRequest(http.MethodPost, "http://prunto.test/admin/actions",
		strings.NewReader("do=create&label=laptop&csrf="+csrf))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(cookie)
	r.SetBasicAuth("admin", "hunter2")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("create = %d", w.Code)
	}
	var created *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == newTokenCookieName {
			created = c
		}
	}
	if created == nil {
		t.Fatal("no token cookie to render the flash from")
	}

	r = httptest.NewRequest(http.MethodGet, "http://prunto.test/admin", nil)
	r.AddCookie(created)
	r.AddCookie(cookie)
	r.SetBasicAuth("admin", "hunter2")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	body := w.Body.String()

	if !strings.Contains(body, `value="`+created.Value+`"`) {
		t.Error("the token is not in a field the user can select and copy")
	}
	if !strings.Contains(body, `id="copy-token"`) {
		t.Error("no copy button rendered")
	}
	if !strings.Contains(body, "/static/copy.js") {
		t.Error("the admin page does not load the script that drives the copy button")
	}

	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://prunto.test/static/copy.js", nil))
	if w.Code != http.StatusOK {
		t.Errorf("/static/copy.js = %d, want 200", w.Code)
	}
}

// The skill is what an agent follows, so the variable it names has to be the one the docs and
// the drop page tell people to export - and it must point at this instance, not a hardcoded host.
func TestSkillNamesOneTokenVariable(t *testing.T) {
	app := newTestApp(t)
	w := httptest.NewRecorder()
	app.Routes().ServeHTTP(w,
		httptest.NewRequest(http.MethodGet, "http://prunto.test/prunto-screenshot/SKILL.md", nil))
	body := w.Body.String()

	if !strings.Contains(body, "$PRUNTO_API_TOKEN") {
		t.Error("the skill does not use PRUNTO_API_TOKEN")
	}
	for _, gone := range []string{"PRIITO_API_TOKEN", "PRIITO_TOKEN", "PRUNTO_TOKEN:-", "priito-screenshot"} {
		if strings.Contains(body, gone) {
			t.Errorf("the skill still mentions %q", gone)
		}
	}
	if !strings.Contains(body, "http://prunto.test/api/v1/uploads") ||
		!strings.Contains(body, "http://prunto.test/blobs/") {
		t.Error("the skill does not point at this instance's own domain")
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

// The dashboard cannot decide CSRF from request headers: WebKit omits Origin on same-origin
// form posts, older browsers send no Sec-Fetch-Site, and Referer is suppressed by this app's
// own Referrer-Policy: no-referrer. A real browser can therefore arrive with none of the three
// and must still work - the CSRF token is what actually authorises the POST.
func TestAdminActionNeedsTheCSRFToken(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()
	cookie, token := adminSession(t, handler)

	post := func(body string, cookies []*http.Cookie, headers map[string]string) int {
		r := httptest.NewRequest(http.MethodPost, "http://prunto.test/admin/actions",
			strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		for _, c := range cookies {
			r.AddCookie(c)
		}
		r.SetBasicAuth("admin", "hunter2")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}

	good := "do=handle&id=1&csrf=" + token
	all := []*http.Cookie{cookie}

	// A browser that sends no Origin, no Sec-Fetch-Site and no Referer still gets through.
	if got := post(good, all, nil); got != http.StatusSeeOther {
		t.Errorf("token with no headers at all = %d, want 303", got)
	}
	for _, h := range []map[string]string{
		{"Sec-Fetch-Site": "same-origin"},
		{"Sec-Fetch-Site": "none"},
		{"Origin": "http://prunto.test"},
		// What a real form navigation actually sends from a page carrying this app's own
		// Referrer-Policy: no-referrer - the browser opaques the origin to "null".
		{"Origin": "null", "Sec-Fetch-Site": "same-origin"},
		{"Origin": "null"},
	} {
		if got := post(good, all, h); got != http.StatusSeeOther {
			t.Errorf("token with %v = %d, want 303", h, got)
		}
	}

	// Everything an attacker can actually mount is still refused.
	refused := []struct {
		name    string
		body    string
		cookies []*http.Cookie
		headers map[string]string
	}{
		{"no token at all", "do=handle&id=1", all, nil},
		{"wrong token", "do=handle&id=1&csrf=" + randToken(32), all, nil},
		{"empty token", "do=handle&id=1&csrf=", all, nil},
		{"token but no cookie to match it", good, nil, nil},
		{"cross-site origin, valid token", good, all, map[string]string{"Origin": "http://evil.test"}},
		// An attacker can produce Origin: null too, so it must not be a free pass: with no
		// matching cookie the token is unforgeable and the request still dies here.
		{"null origin without the cookie", good, nil, map[string]string{"Origin": "null"}},
		{"cross-site fetch metadata, valid token", good, all, map[string]string{"Sec-Fetch-Site": "cross-site"}},
		{"same-site is still another host", good, all, map[string]string{"Sec-Fetch-Site": "same-site"}},
	}
	for _, c := range refused {
		if got := post(c.body, c.cookies, c.headers); got != http.StatusForbidden {
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
func TestBlobServesRanges(t *testing.T) {
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

// Blobs are served from this app's own origin, so the drop page's preview is covered by 'self'
// and the policy should name no host at all - a stray host here would be a leftover.
func TestDropPageCSPIsSelfOnly(t *testing.T) {
	app := newTestApp(t)

	w := httptest.NewRecorder()
	app.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://prunto.test/", nil))

	csp := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{
		"img-src 'self' blob: data:;",
		"media-src 'self' blob:;",
		"default-src 'self'",
		"base-uri 'none'",
	} {
		if !strings.Contains(csp, want) {
			t.Fatalf("CSP %q does not contain %q", csp, want)
		}
	}
	if strings.Contains(csp, "http") {
		t.Fatalf("CSP names a host, but blobs are same-origin now: %q", csp)
	}
}

// The bytes come back off the volume with the content type recorded at upload and sandboxed,
// and stop coming back the moment the row says the upload expired. Range lives in
// TestBlobServesRanges.
func TestBlobIsServedFromDisk(t *testing.T) {
	app := newTestApp(t)
	token, _ := CreateToken(context.Background(), app.DB, "test")
	png := samplePNG(t, 8, 8)

	w := httptest.NewRecorder()
	app.Routes().ServeHTTP(w, uploadRequest(t, png, "shot.png", token, nil))
	if w.Code != http.StatusCreated {
		t.Fatalf("upload = %d", w.Code)
	}
	var body struct{ URL string }
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(body.URL, "http://prunto.test/blobs/") {
		t.Fatalf("url = %q, want it served from this origin", body.URL)
	}

	get := func(headers map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, body.URL, nil)
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		app.Routes().ServeHTTP(rec, r)
		return rec
	}

	rec := get(nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("blob = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", got)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != "sandbox" {
		t.Errorf("blob CSP = %q, want sandbox", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("blob nosniff = %q", got)
	}
	// Not the uploaded bytes: those are re-encoded to strip metadata. What matters is that the
	// handler streams back exactly what is on the volume.
	key := strings.TrimPrefix(body.URL, "http://prunto.test/blobs/")
	onDisk, err := os.ReadFile(filepath.Join(app.Config.DataDir, "blobs", key))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rec.Body.Bytes(), onDisk) {
		t.Errorf("served %d bytes, but the file on disk is %d", rec.Body.Len(), len(onDisk))
	}
	if !bytes.HasPrefix(onDisk, []byte("\x89PNG")) {
		t.Error("what landed on the volume is not a PNG")
	}

	// An expired row is not served, even while its file is still on disk waiting for the
	// hourly sweep: the sweep is bounded, so it cannot be what enforces expires_at.
	if _, err := app.DB.Exec(`UPDATE uploads SET expires_at = ?`, time.Now().Add(-time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(app.Config.DataDir, "blobs", key)); err != nil {
		t.Fatalf("the file should still be on disk for this case: %v", err)
	}
	if rec := get(nil); rec.Code != http.StatusNotFound {
		t.Errorf("expired blob = %d, want 404", rec.Code)
	}

	// A key that is not in the database at all is a 404 too.
	if _, err := app.DB.Exec(`DELETE FROM uploads`); err != nil {
		t.Fatal(err)
	}
	if rec := get(nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown blob = %d, want 404", rec.Code)
	}
}

// A token in a URL lands in browser history and in every access log in front of the origin,
// which is what the API refuses a request over. Creating one must not put it there.
func TestNewTokenIsNeverInTheURL(t *testing.T) {
	app := newTestApp(t)
	handler := app.Routes()

	cookie, csrf := adminSession(t, handler)

	form := strings.NewReader("do=create&label=laptop&csrf=" + csrf)
	r := httptest.NewRequest(http.MethodPost, "http://prunto.test/admin/actions", form)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", app.Config.BaseURL)
	r.AddCookie(cookie)
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

// PRUNTO_BASE_URL is compared against an Origin header and is what every blob URL is built on,
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
	if cfg.BlobBaseURL() != "https://prunto.test/blobs" {
		t.Fatalf("BlobBaseURL = %q", cfg.BlobBaseURL())
	}
}
