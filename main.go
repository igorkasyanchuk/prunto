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
	"strings"
	"syscall"
	"time"
)

type App struct {
	Config Config
	DB     *sql.DB
	Store  *Store
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

	app := &App{Config: cfg, DB: db, Store: NewStore(cfg), Log: logger}

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

	logger.Printf("blobs are on disk under %s and served from %s", cfg.DataDir, cfg.BlobBaseURL())
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

func runCommand(app *App, args []string) error {
	switch args[0] {
	case "token":
		if len(args) < 2 {
			return fmt.Errorf(`usage: prunto token "label"`)
		}
		raw, err := CreateToken(context.Background(), app.DB, strings.Join(args[1:], " "))
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
	mux.HandleFunc("GET /abuse_reports/new", a.handleNewAbuseReport)
	mux.HandleFunc("POST /abuse_reports", a.handleCreateAbuseReport)
	mux.HandleFunc("GET /admin", a.guard(a.handleAdmin))
	mux.HandleFunc("POST /admin/actions", a.guard(a.handleAdminAction))
	// Clients that ignore the <link rel="icon"> tags ask for this by reflex; without the route
	// it falls through to the 404 handler on every page load.
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/static/favicon-32.png?v="+assetVersion, http.StatusFound)
	})

	// No directory listings: FileServer's autoindex is the one HTML page the app would serve
	// without a lang attribute or a title, and there is nothing to browse anyway.
	static := http.StripPrefix("/static/", http.FileServerFS(staticFS))
	mux.Handle("GET /static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		// Every page asks for these with ?v=<fingerprint of the embedded files>, so a given
		// URL's bytes never change and a deploy changes the URL. Without the fingerprint this
		// header would pin a stale stylesheet in every browser and CDN for a year.
		if r.URL.Query().Get("v") == assetVersion {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			// Unversioned or stale-versioned: someone typed the path, or is running a page
			// from before the last deploy. Let them cache it, but make them check.
			w.Header().Set("Cache-Control", "no-cache")
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

	mux.HandleFunc("GET /blobs/{key}", a.handleBlob)

	return a.withSecurityHeaders(mux)
}

// withSecurityHeaders sets on every response what a CDN in front of a bucket used to.
func (a *App) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Robots-Tag", "noindex, nofollow, noarchive")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		if a.Config.HTTPS() {
			// TLS is terminated upstream, so HSTS has to be asserted from here.
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP resolves the address every rate limit and quota is keyed on.
//
// "cloudflare": Cloudflare overwrites CF-Connecting-IP on every request it forwards, so a
// client cannot forge it. X-Forwarded-For is ignored: anyone can set it, and trusting it would
// turn every limit here into decoration.
//
// "forwarded": for a plain reverse proxy (Traefik, Caddy, nginx) that appends the address it
// accepted the connection from to X-Forwarded-For. Only the last entry is used - the one the
// proxy itself wrote - so whatever the client put in the header first is never read.
//
// In either mode the header's absence means the request did not come through the proxy at
// all, which is itself worth refusing over: the origin should not be reachable directly.
func (a *App) clientIP(r *http.Request) (string, bool) {
	var raw string
	switch a.Config.TrustProxy {
	case "cloudflare":
		raw = r.Header.Get("CF-Connecting-IP")
	case "forwarded":
		// Values, not Get: a proxy that adds its own header line instead of appending to
		// the client's (HAProxy does) would otherwise leave the client's line first, and
		// Get returns only the first.
		xff := strings.Join(r.Header.Values("X-Forwarded-For"), ",")
		raw = xff[strings.LastIndex(xff, ",")+1:]
	default:
		raw = r.RemoteAddr
	}
	ip := hostIP(raw)
	return ip, ip != ""
}

// hostIP normalises what a header or RemoteAddr carries to a bare IP, or "" if it is not
// one. A proxy that writes ip:port would otherwise give every connection its own rate-limit
// key, and a trailing comma or garbage would become a key too.
func hostIP(raw string) string {
	raw = strings.TrimSpace(raw)
	if host, _, err := net.SplitHostPort(raw); err == nil {
		raw = host
	}
	if ip := net.ParseIP(strings.Trim(raw, "[]")); ip != nil {
		return ip.String()
	}
	return ""
}
