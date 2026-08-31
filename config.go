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

type Config struct {
	Addr    string
	DataDir string
	BaseURL string // this app's own public origin, used for delete URLs and the skill

	// Storage. With no bucket configured, blobs are written under DataDir and served from
	// /blobs/:key by this process - enough to run the whole flow with no account anywhere.
	Bucket     string
	KeyID      string
	AppKey     string
	Endpoint   string
	Region     string
	CDNBaseURL string

	AdminUser     string
	AdminPassword string

	// TrustProxy is "cloudflare" when CF-Connecting-IP is authoritative, "none" otherwise.
	TrustProxy string
}

func LoadConfig() (Config, error) {
	c := Config{
		Addr:          ":" + env("PORT", "3000"),
		DataDir:       env("DATA_DIR", "./data"),
		// ponytail: PRIITO_BASE_URL fallback is a one-release shim for deployments that
		// predate the rename; drop it once those have moved to PRUNTO_BASE_URL.
		BaseURL:       strings.TrimSuffix(env("PRUNTO_BASE_URL", os.Getenv("PRIITO_BASE_URL")), "/"),
		Bucket:        env("B2_BUCKET", ""),
		KeyID:         env("B2_KEY_ID", ""),
		AppKey:        env("B2_APPLICATION_KEY", ""),
		Endpoint:      env("B2_ENDPOINT", ""),
		Region:        env("B2_REGION", ""),
		CDNBaseURL:    strings.TrimSuffix(env("CDN_BASE_URL", ""), "/"),
		AdminUser:     env("ADMIN_USER", ""),
		AdminPassword: env("ADMIN_PASSWORD", ""),
		TrustProxy:    env("TRUST_PROXY", "none"),
	}

	if err := os.MkdirAll(filepath.Join(c.DataDir, "blobs"), 0o750); err != nil {
		return c, err
	}

	// Either every bucket variable is set or none is. A half-configured bucket is the state
	// where uploads succeed and the URLs point nowhere.
	set := 0
	for _, v := range []string{c.Bucket, c.KeyID, c.AppKey, c.Endpoint, c.Region, c.CDNBaseURL} {
		if v != "" {
			set++
		}
	}
	if set != 0 && set != 6 {
		return c, fmt.Errorf("set all of B2_BUCKET, B2_KEY_ID, B2_APPLICATION_KEY, B2_ENDPOINT, B2_REGION and CDN_BASE_URL, or none of them")
	}

	if c.BaseURL == "" {
		if c.Local() {
			c.BaseURL = "http://localhost" + c.Addr
		} else {
			return c, fmt.Errorf("PRUNTO_BASE_URL is required: it is the public origin this instance hands out in delete URLs and in the skill")
		}
	}

	// Scheme and host, nothing else. Every failure downstream of a looser value is silent: the
	// admin CSRF check compares this against an Origin header, which carries no path or query,
	// so admin POSTs 403 forever; the delete URLs are built by concatenation, so anything after
	// the host swallows the path glued onto it; and CDNOrigin below goes empty on a value with
	// no scheme, which drops the CDN from the drop page's CSP and blocks the preview.
	u, err := url.Parse(c.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return c, fmt.Errorf("PRUNTO_BASE_URL must be a bare origin such as https://prunto.example.com, got %q", c.BaseURL)
	}

	if c.Local() {
		c.CDNBaseURL = c.BaseURL + "/blobs"
	} else if u, err := url.Parse(c.CDNBaseURL); err != nil || u.Scheme == "" || u.Host == "" {
		return c, fmt.Errorf("CDN_BASE_URL must be an absolute URL such as https://cdn.example.com, got %q", c.CDNBaseURL)
	}
	return c, nil
}

// Local reports whether blobs live on disk rather than in a bucket.
func (c Config) Local() bool { return c.Bucket == "" }

// AdminEnabled reports whether /admin will answer at all. An unconfigured admin is closed,
// not open.
func (c Config) AdminEnabled() bool { return c.AdminUser != "" && c.AdminPassword != "" }

func (c Config) DBPath() string { return filepath.Join(c.DataDir, "prunto.db") }

// CDNOrigin is the scheme://host blobs are served from, with no path. The drop page previews an
// upload straight off it, so the page's CSP has to name the origin or the browser blocks the
// preview - a CSP source with a path only matches that exact path, which CDNBaseURL is not.
func (c Config) CDNOrigin() string {
	u, err := url.Parse(c.CDNBaseURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
