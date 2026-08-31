package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
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

	cfg, err := LoadConfig()
	if err != nil {
		logger.Fatalf("configuration: %v", err)
	}
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

	// Subcommands. `priito token "my laptop"` is the whole administrative surface that has to
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

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()

	logger.Printf("priito listening on %s, serving %s", cfg.Addr, cfg.BaseURL)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Fatal(err)
	}
}

func runCommand(app *App, args []string) error {
	switch args[0] {
	case "token":
		if len(args) < 2 {
			return fmt.Errorf(`usage: priito token "label"`)
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
	mux.HandleFunc("GET /priito-screenshot/SKILL.md", a.handleSkill)
	mux.HandleFunc("GET /abuse_reports/new", a.handleNewAbuseReport)
	mux.HandleFunc("POST /abuse_reports", a.handleCreateAbuseReport)
	mux.HandleFunc("GET /admin", a.guard(a.handleAdmin))
	mux.HandleFunc("POST /admin/actions", a.guard(a.handleAdminAction))
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(staticFS)))

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
		host, _, _ := strings.Cut(r.RemoteAddr, ":")
		if i := strings.LastIndex(r.RemoteAddr, ":"); i > 0 {
			host = strings.Trim(r.RemoteAddr[:i], "[]")
		}
		return host, host != ""
	}
	ip := strings.TrimSpace(r.Header.Get("CF-Connecting-IP"))
	return ip, ip != ""
}
