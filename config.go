package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MaxBytes is the per-file cap. It is a second line of defence: the first is the body limit
// on whatever proxy terminates TLS, because bytes refused here have already been paid for.
const MaxBytes = 10 << 20

// Retention defaults. A caller may ask for less, never more - longer retention is a storage
// bill and a wider abuse window, and deciding who has earned either is what accounts are for.
const (
	DefaultRetention = 14 * 24 * time.Hour
	MinRetention     = time.Minute
	EventRetention   = 90 * 24 * time.Hour
)

// Per-token quotas. Keyed by token rather than by address: a team behind one NAT should not
// share a budget, and a leaked token should not be throttleable by moving hosts.
const (
	UploadsPerHour = 20
	DailyByteCap   = 200 << 20
	ReportsPerHour = 5
)

// Failed /admin logins allowed per address before the address is refused for AdminLockout.
// Basic auth has no session to lock, so the counter is the only thing between the dashboard
// and a password list.
const (
	AdminLoginAttempts = 10
	AdminLockout       = 15 * time.Minute
)

type Config struct {
	Addr    string
	DataDir string
	BaseURL string // this app's own public origin, used for delete URLs and the skill

	AdminUser     string
	AdminPassword string

	// TrustProxy is "cloudflare" when CF-Connecting-IP is authoritative, "forwarded" when the
	// last X-Forwarded-For entry was written by a reverse proxy you control, "none" otherwise.
	TrustProxy string
}

func LoadConfig() (Config, error) {
	c := Config{
		Addr:          ":" + env("PORT", "3000"),
		DataDir:       env("DATA_DIR", "./data"),
		BaseURL:       strings.TrimSuffix(env("PRUNTO_BASE_URL", ""), "/"),
		AdminUser:     env("ADMIN_USER", ""),
		AdminPassword: env("ADMIN_PASSWORD", ""),
		TrustProxy:    env("TRUST_PROXY", "none"),
	}

	if err := os.MkdirAll(filepath.Join(c.DataDir, "blobs"), 0o750); err != nil {
		return c, err
	}

	if c.BaseURL == "" {
		c.BaseURL = "http://localhost" + c.Addr
	}

	switch c.TrustProxy {
	case "none", "cloudflare", "forwarded":
	default:
		return c, fmt.Errorf("TRUST_PROXY must be none, cloudflare or forwarded, got %q", c.TrustProxy)
	}

	// Scheme and host, nothing else. Every failure downstream of a looser value is silent: the
	// admin CSRF check compares this against an Origin header, which carries no path or query,
	// so admin POSTs 403 forever, and the delete and blob URLs are built by concatenation, so
	// anything after the host swallows the path glued onto it.
	u, err := url.Parse(c.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return c, fmt.Errorf("PRUNTO_BASE_URL must be a bare origin such as https://prunto.example.com, got %q", c.BaseURL)
	}
	return c, nil
}

// AdminEnabled reports whether /admin will answer at all. An unconfigured admin is closed,
// not open.
func (c Config) AdminEnabled() bool { return c.AdminUser != "" && c.AdminPassword != "" }

func (c Config) DBPath() string { return filepath.Join(c.DataDir, "prunto.db") }

// BlobBaseURL is where uploads are served from. Blobs share this app's origin, so the drop
// page's CSP needs no extra source for the preview.
func (c Config) BlobBaseURL() string { return c.BaseURL + "/blobs" }

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
