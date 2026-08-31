package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type App struct {
	Config Config
	DB     *sql.DB
	Store  Store
	Log    *log.Logger
}

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags)

	// Before LoadConfig so the warning also lands when the old variable's value fails
	// validation - the error below names only PRUNTO_BASE_URL.
	if os.Getenv("PRIITO_BASE_URL") != "" && os.Getenv("PRUNTO_BASE_URL") == "" {
		logger.Printf("PRIITO_BASE_URL is deprecated and will stop working: set PRUNTO_BASE_URL")
	}
	cfg, err := LoadConfig()
	if err != nil {
		logger.Fatalf("configuration: %v", err)
	}
	adoptRenamedDB(cfg, logger)
	db, err := OpenDB(cfg.DBPath())
	if err != nil {
		logger.Fatalf("opening %s: %v", cfg.DBPath(), err)
	}
	defer db.Close()

	store, err := NewStore(cfg)
	if err != nil {
		logger.Fatalf("storage: %v", err)
	}
	app := &App{Config: cfg, DB: db, Store: store, Log: logger}

	// Subcommands. `prunto token "my laptop"` is the whole administrative surface that has to
	// exist before the first upload.
	if len(os.Args) > 1 && os.Args[1] != "serve" {
		if err := runCommand(app, os.Args[1:]); err != nil {
			logger.Fatal(err)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if !cfg.Local() {
		check, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := VerifyCDNHeaders(check, store)
		cancel()
		if err != nil {
			logger.Fatalf("%v", err)
		}
	} else {
		logger.Printf("no bucket configured: blobs are on disk under %s and served from /blobs", cfg.DataDir)
		logger.Printf("URLs will point at %s, which GitHub cannot reach - this mode is for local work", cfg.BaseURL)
	}
	if !cfg.AdminEnabled() {
		logger.Printf("ADMIN_USER/ADMIN_PASSWORD unset: /admin is closed")
	}

	app.StartPurge(ctx, time.Hour)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           app.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute, // a 10 MB upload on a slow line
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
	}

	// ListenAndServe returns the moment Shutdown is called, so main has to wait for the drain
	// to finish. Falling straight through would run `defer db.Close()` under the requests
	// still being served, and the 15 seconds below would buy nothing.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			logger.Printf("shutdown did not drain within 15s: %v", err)
		}
	}()

	logger.Printf("prunto listening on %s, serving %s", cfg.Addr, cfg.BaseURL)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Fatal(err)
	}
	<-drained
}

// adoptRenamedDB picks up a database written before the rename to prunto. Without it an
// upgraded deployment boots against a fresh empty prunto.db while every token digest and
// upload row sits in the abandoned priito.db — silently, since SQLite creates on open.
// A prunto.db that already exists but holds no data is moved aside and adopted over: the
// shimless rename release created exactly such empty files on every boot, and skipping
// them would strand the very fleet this shim exists for. A prunto.db with data wins, with
// a log line so the operator knows priito.db was left behind.
// ponytail: one-release shim, delete once pre-rename deployments are gone.
func adoptRenamedDB(cfg Config, logger *log.Logger) {
	old := filepath.Join(cfg.DataDir, "priito.db")
	if _, err := os.Stat(old); err != nil {
		return
	}
	if _, err := os.Stat(cfg.DBPath()); err == nil {
		if dbHasData(cfg.DBPath()) {
			logger.Printf("both %s and %s exist; keeping %s (move %s away by hand)",
				old, cfg.DBPath(), cfg.DBPath(), old)
			return
		}
		for _, ext := range []string{"-shm", "-wal", ""} {
			if _, err := os.Stat(cfg.DBPath() + ext); err != nil {
				continue
			}
			if err := os.Rename(cfg.DBPath()+ext, cfg.DBPath()+ext+".empty"); err != nil {
				logger.Fatalf("moving empty database aside: %v", err)
			}
		}
	}
	// Sidecars first, main file last: the main file is the adoption's commit point, so a
	// crash mid-way resumes on the next boot instead of stranding the WAL.
	for _, ext := range []string{"-shm", "-wal", ""} {
		o := old + ext
		if _, err := os.Stat(o); err != nil {
			continue
		}
		if err := os.Rename(o, cfg.DBPath()+ext); err != nil {
			logger.Fatalf("adopting pre-rename database %s: %v", o, err)
		}
		logger.Printf("adopted pre-rename database file %s as %s", o, cfg.DBPath()+ext)
	}
}

// dbHasData reports whether the SQLite file at path holds any rows worth keeping. A file
// that cannot be opened or queried counts as empty — it is moved aside, never deleted.
func dbHasData(path string) bool {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return false
	}
	defer db.Close()
	var n int
	err = db.QueryRow(`SELECT (SELECT COUNT(*) FROM api_tokens) + (SELECT COUNT(*) FROM uploads)
		+ (SELECT COUNT(*) FROM abuse_reports)`).Scan(&n)
	return err == nil && n > 0
}

func runCommand(app *App, args []string) error {
	switch args[0] {
	case "token":
		if len(args) < 2 {
			return fmt.Errorf(`usage: prunto token "label"`)
		}
		raw, err := MintToken(context.Background(), app.DB, strings.Join(args[1:], " "))
		if err != nil {
			return err
		}
		fmt.Println(raw)
		fmt.Fprintln(os.Stderr, "Shown once. Only its SHA-256 digest is stored.")
		return nil
	case "purge":
		app.PurgeExpired(context.Background())
		return nil
	default:
		return fmt.Errorf("unknown command %q (known: serve, token, purge)", args[0])
	}
}

func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", a.handleDropPage)
	mux.HandleFunc("POST /api/v1/uploads", a.handleCreateUpload)
	mux.HandleFunc("DELETE /api/v1/uploads/{deleteToken}", a.handleDeleteUpload)
	mux.HandleFunc("GET /prunto-screenshot/SKILL.md", a.handleSkill)
	// ponytail: legacy alias from the rename; previously installed skills refresh themselves
	// with `curl -sf` (no -L) against this path, so serve it rather than redirect.
	mux.HandleFunc("GET /priito-screenshot/SKILL.md", a.handleSkill)
	mux.HandleFunc("GET /abuse_reports/new", a.handleNewAbuseReport)
	mux.HandleFunc("POST /abuse_reports", a.handleCreateAbuseReport)
	mux.HandleFunc("GET /admin", a.guard(a.handleAdmin))
	mux.HandleFunc("POST /admin/actions", a.guard(a.handleAdminAction))
	// No directory listings: FileServer's autoindex is the one HTML page the app would serve
	// without a lang attribute or a title, and there is nothing to browse anyway.
	static := http.StripPrefix("/static/", http.FileServerFS(staticFS))
	mux.Handle("GET /static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		static.ServeHTTP(w, r)
	}))

	mux.HandleFunc("GET /up", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	// Search indexing is what turns a file host into a distribution channel.
	mux.HandleFunc("GET /robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("User-agent: *\nDisallow: /\n"))
	})

	if a.Config.Local() {
		mux.HandleFunc("GET /blobs/{key}", a.handleLocalBlob)
	}

	return a.withSecurityHeaders(mux)
}

// withSecurityHeaders sets on this app's own responses what a Transform Rule sets on the CDN's.
func (a *App) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Robots-Tag", "noindex, nofollow, noarchive")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		if a.Config.TrustProxy == "cloudflare" {
			// TLS is terminated upstream, so HSTS has to be asserted from here.
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP resolves the address every rate limit and quota is keyed on.
//
// Cloudflare overwrites CF-Connecting-IP on every request it forwards, so a client cannot
// forge it. X-Forwarded-For can be set by anyone, and trusting it would turn every limit here
// into decoration. Its absence behind a proxy means the request did not come through
// Cloudflare at all, which is itself worth refusing over: the origin should not be reachable
// directly (see README).
func (a *App) clientIP(r *http.Request) (string, bool) {
	if a.Config.TrustProxy != "cloudflare" {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr // no port, as in a test or a unix socket
		}
		return host, host != ""
	}
	ip := strings.TrimSpace(r.Header.Get("CF-Connecting-IP"))
	return ip, ip != ""
}
