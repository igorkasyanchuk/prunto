package main

import (
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MaxBytes is the per-file cap. It is a second line of defence: the first is the body limit
// on whatever proxy terminates TLS, because bytes refused here have already been paid for.
const MaxBytes = 10 << 20

// Retention. The instance-wide value comes from RETENTION; unset or "never" keeps uploads until
// they are deleted by hand, which the Config carries as a zero Retention. A caller may ask for
// less per upload, never more - longer retention is a storage bill and a wider abuse window,
// and deciding who has earned either is what accounts are for.
const (
	MinRetention   = time.Minute
	EventRetention = 90 * 24 * time.Hour
)

// Per-token quotas. Keyed by token rather than by address: a team behind one NAT should not
// share a budget, and a leaked token should not be throttleable by moving hosts.
const (
	UploadsPerHour = 20
	DailyByteCap   = 200 << 20
	ReportsPerHour = 5
)

// Wrong /admin passwords allowed per address per AdminLockout; reaching the count refuses the
// address for a full AdminLockout from that moment, and a successful login clears it. Basic
// auth has no session to lock, so the counter is the only thing between the dashboard and a
// password list.
const (
	AdminLoginAttempts = 10
	AdminLockout       = 15 * time.Minute
)

type Config struct {
	Addr    string
	DataDir string
	BaseURL string // this app's own public origin, used for delete URLs and the skill

	// Retention is how long an upload lives unless the uploader asks for less. Zero means
	// forever: nothing expires unless the upload itself carried an expires_in.
	Retention time.Duration

	AdminUser     string
	AdminPassword string

	// TrustProxy is "cloudflare" when CF-Connecting-IP is authoritative, "forwarded" when the
	// last X-Forwarded-For entry was written by a reverse proxy you control, "none" otherwise.
	TrustProxy string

	// Analytics is an HTML snippet the operator pastes in (ANALYTICS_HTML), typically one
	// <script> tag for Umami, Plausible or similar. It is rendered unescaped into the home
	// page's <head> and nowhere else. AnalyticsOrigins are the https origins its src
	// attributes point at, which the home page's CSP has to allow for scripts and beacons.
	Analytics        string
	AnalyticsOrigins []string
}

func LoadConfig() (Config, error) {
	c := Config{
		Addr:          ":" + env("PORT", "3000"),
		DataDir:       env("DATA_DIR", "./data"),
		BaseURL:       strings.TrimSuffix(env("PRUNTO_BASE_URL", ""), "/"),
		AdminUser:     env("ADMIN_USER", ""),
		AdminPassword: env("ADMIN_PASSWORD", ""),
		TrustProxy:    env("TRUST_PROXY", "none"),
		Analytics:     strings.TrimSpace(env("ANALYTICS_HTML", "")),
	}
	c.AnalyticsOrigins = scriptOrigins(c.Analytics)

	err := os.MkdirAll(filepath.Join(c.DataDir, "blobs"), 0o750)
	if err != nil {
		return c, err
	}

	if c.BaseURL == "" {
		c.BaseURL = "http://localhost" + c.Addr
	}

	// "never", "0" and blank all mean keep forever; anything else is a duration of at least a
	// minute. Each message names the spelling for forever, since that is what an operator who
	// typed 0 or 30s was most likely reaching for.
	switch retention := strings.ToLower(strings.TrimSpace(env("RETENTION", ""))); retention {
	case "", "never", "0":
	default:
		c.Retention, err = parseDuration(retention)
		if err != nil {
			return c, fmt.Errorf("RETENTION %v (or never): got %q", err, retention)
		}
		if c.Retention < MinRetention {
			return c, fmt.Errorf("RETENTION must be at least a minute, or never: got %q", retention)
		}
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

// HTTPS reports whether the public origin is served over TLS. The app never terminates TLS
// itself, so the configured origin is the only source of truth: HSTS and the Secure cookie
// flag both key on it.
func (c Config) HTTPS() bool { return strings.HasPrefix(c.BaseURL, "https://") }

func (c Config) DBPath() string { return filepath.Join(c.DataDir, "prunto.db") }

// BlobBaseURL is where uploads are served from, on this app's own origin.
func (c Config) BlobBaseURL() string { return c.BaseURL + "/blobs" }

var (
	retentionPattern = regexp.MustCompile(`^(\d+)([smhd]?)$`)
	retentionUnits   = map[string]time.Duration{
		"": time.Second, "s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour,
	}
)

// parseDuration reads "30m", "6h", "7d" or a plain number of seconds. The bound is per unit:
// a Duration is an int64 of nanoseconds, so the largest safe amount depends on what it is
// multiplied by, and one fixed ceiling either overflows for days or rejects valid seconds.
func parseDuration(value string) (time.Duration, error) {
	m := retentionPattern.FindStringSubmatch(strings.ToLower(strings.TrimSpace(value)))
	if m == nil {
		return 0, fmt.Errorf("should look like 30m, 6h or 7d")
	}
	unit := retentionUnits[m[2]]
	amount, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || amount > math.MaxInt64/int64(unit) {
		return 0, fmt.Errorf("is out of range")
	}
	return time.Duration(amount) * unit, nil
}

var srcOrigin = regexp.MustCompile(`\bsrc\s*=\s*["']?(https://[^/"'\s>]+)`)

// scriptOrigins lists each distinct https origin a snippet loads from, so the CSP can name
// exactly those and nothing wider. http: is left out on purpose: a tracker over plain HTTP on
// an https page is blocked by the browser anyway.
func scriptOrigins(snippet string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range srcOrigin.FindAllStringSubmatch(snippet, -1) {
		if o := strings.ToLower(m[1]); !seen[o] {
			seen[o] = true
			out = append(out, o)
		}
	}
	return out
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
