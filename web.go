package main

import (
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	texttemplate "text/template"
	"time"
)

//go:embed templates
var templateDir embed.FS

//go:embed static
var staticDir embed.FS

var staticFS, _ = fs.Sub(staticDir, "static")

// assetVersion fingerprints the embedded static files, and every page hangs it off their URLs
// as ?v=. Without it a deployed CSS or JS change reaches nobody until each browser and the CDN
// in front of them decide on their own to look again - which is how a fixed stylesheet can sit
// on the server while every visitor still runs the old one.
var assetVersion = fingerprintStatic()

func fingerprintStatic() string {
	h := sha256.New()
	err := fs.WalkDir(staticFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		f, err := staticFS.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		// The name matters as well as the bytes: a rename with identical content is still a
		// different set of assets.
		h.Write([]byte(path))
		_, err = io.Copy(h, f)
		return err
	})
	if err != nil {
		// The files are compiled in, so this cannot fail on a build that started at all.
		panic("fingerprinting the embedded assets: " + err.Error())
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

var pages = template.Must(template.New("").Funcs(template.FuncMap{
	"bytes": humanBytes,
	"short": func(s string) string {
		if len(s) > 12 {
			return s[:12]
		}
		return s
	},
	"stamp": func(t time.Time) string { return t.Format("2006-01-02 15:04") },
}).ParseFS(templateDir, "templates/*.html"))

// The skill is rendered with text/template, not html/template: it is Markdown handed to an
// agent, and HTML-escaping it would mangle every code fence in the file.
var skillTemplate = texttemplate.Must(texttemplate.ParseFS(templateDir, "templates/skill.md"))

// view is what every page and the skill are rendered with. Every URL comes from configuration,
// so a self-hosted instance hands out its own host and never someone else's.
func (a *App) view() map[string]any {
	return map[string]any{
		"BaseURL":     a.Config.BaseURL,
		"BlobBaseURL": a.Config.BlobBaseURL(),
		"SkillURL":    a.Config.BaseURL + "/prunto-screenshot/SKILL.md",
		"Retention":   humanDuration(DefaultRetention),
		"MaxMB":       MaxBytes >> 20,
		"Accept":      acceptAttribute(),
		"Assets":      assetVersion,
	}
}

func acceptAttribute() string {
	types := make([]string, 0, len(Allowed))
	for _, k := range Allowed {
		types = append(types, k.ContentType)
	}
	return strings.Join(types, ",")
}

func humanDuration(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return plural(int(d.Hours())/24, "day")
	case d >= time.Hour:
		return plural(int(d.Hours()), "hour")
	default:
		return plural(int(d.Minutes()), "minute")
	}
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

func (a *App) render(w http.ResponseWriter, name string, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Blobs are served from this same origin now, so 'self' covers the drop page's preview and
	// the policy needs no host in it at all.
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; img-src 'self' blob: data:; media-src 'self' blob:; "+
			"base-uri 'none'; form-action 'self'")
	if err := pages.ExecuteTemplate(w, name, data); err != nil {
		a.Log.Printf("rendering %s: %v", name, err)
	}
}

func (a *App) handleDropPage(w http.ResponseWriter, r *http.Request) {
	a.render(w, "drop.html", a.view())
}

// handleSkill serves the Claude Code skill, rendered rather than static because every URL in
// it - the API endpoint, the blob host, the install command - comes from this instance's own
// configuration. Anyone using an instance can install the skill without cloning the repo.
func (a *App) handleSkill(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	if err := skillTemplate.ExecuteTemplate(w, "skill.md", a.view()); err != nil {
		a.Log.Printf("rendering the skill: %v", err)
	}
}

func (a *App) handleNewAbuseReport(w http.ResponseWriter, r *http.Request) {
	a.render(w, "abuse.html", a.view())
}

var uploadTokenPattern = regexp.MustCompile(`/([\w-]+)\.(?:png|jpg|gif|webp|mp4|webm)(?:\?|$)`)

// handleCreateAbuseReport takes a report from anyone, with no account and no token: someone
// who found the content in a pull request has neither, and no reason to get one.
func (a *App) handleCreateAbuseReport(w http.ResponseWriter, r *http.Request) {
	ip, ok := a.clientIP(r)
	if !ok {
		http.Error(w, "This request did not arrive through the trusted proxy", http.StatusForbidden)
		return
	}
	// A limiter that cannot count fails open, but it does not fail silently: an operator has to
	// be able to see why the reports stopped being throttled.
	count, err := bump(r.Context(), a.DB, "reports:"+ip, 1, time.Hour)
	if err != nil {
		a.Log.Printf("abuse report rate limit: %v", err)
	} else if count > ReportsPerHour {
		http.Error(w, "Too many reports from this address, try again later", http.StatusTooManyRequests)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "That form could not be read", http.StatusBadRequest)
		return
	}
	url := strings.TrimSpace(r.PostFormValue("url"))
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	if url == "" || reason == "" {
		view := a.view()
		view["Error"] = "Both the URL and a reason are required."
		a.render(w, "abuse.html", view)
		return
	}
	url, reason = truncate(url, 2000), truncate(reason, 2000)

	// Store the token parsed out of the URL rather than a foreign key: the upload is usually
	// already gone by the time a report arrives, and that is the normal case, not an error.
	var uploadToken string
	if m := uploadTokenPattern.FindStringSubmatch(url); m != nil {
		uploadToken = m[1]
	}
	if _, err := a.DB.ExecContext(r.Context(),
		`INSERT INTO abuse_reports (url, upload_token, reason, created_at) VALUES (?,?,?,?)`,
		url, uploadToken, reason, time.Now().Unix()); err != nil {
		a.Log.Printf("saving abuse report: %v", err)
		http.Error(w, "That report could not be saved", http.StatusInternalServerError)
		return
	}

	view := a.view()
	view["Submitted"] = true
	a.render(w, "abuse.html", view)
}

// handleBlob serves an upload off the data volume. This is the whole delivery path now that
// there is no bucket, so it carries the headers a CDN Transform Rule used to: the content type
// recorded at upload rather than anything sniffed from the bytes, and a sandbox CSP, because
// these are attacker-controlled bytes on this app's own origin.
func (a *App) handleBlob(w http.ResponseWriter, r *http.Request) {
	key := filepath.Base(r.PathValue("key"))
	// Expiry is enforced here, not just by the sweep: the sweep runs hourly and clears a
	// bounded batch, so a file whose time is up outlives it. This is the only path the bytes
	// are served on, which makes this query the thing that honours the promised expires_at.
	var contentType string
	if err := a.DB.QueryRowContext(r.Context(),
		`SELECT content_type FROM uploads WHERE object_key = ? AND expires_at > ?`,
		key, time.Now().Unix()).Scan(&contentType); err != nil {
		if err != sql.ErrNoRows {
			a.Log.Printf("looking up blob %s: %v", key, err)
		}
		http.NotFound(w, r)
		return
	}
	// Streamed, not read into memory: a 10 MB video times a handful of concurrent readers is
	// real memory on a box where the whole process otherwise sits in single-digit MB.
	f, err := a.Store.Open(key)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	// ServeContent rather than io.Copy, so Range requests work: a browser seeking within a
	// <video> asks for a byte range, and a handler that answers 200 with the whole body
	// leaves the scrubber dead. It needs a ReadSeeker, which is what the file already is.
	http.ServeContent(w, r, key, time.Time{}, f)
}
