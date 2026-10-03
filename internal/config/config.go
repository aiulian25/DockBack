// Package config loads DockBack runtime configuration from environment
// variables and mounted secret files. Nothing here is ever hardcoded; secrets
// are read from the environment or from files referenced by *_FILE vars
// (Docker secrets pattern). See PLAN §2.7-§2.9.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"dockback/internal/crypto"
	"dockback/internal/egress"
	"dockback/internal/store"
)

// Config holds all resolved runtime settings.
type Config struct {
	Host string // bind address; defaults to 127.0.0.1
	Port int    // default 28734 (PLAN §2.9 - uncommon, below ephemeral range)

	DataDir    string // writable: SQLite catalog + config
	BackupsDir string // writable: local backup target (/app/backups)
	TmpDir     string // writable scratch for SMALL work (tmpfs/RAM)
	WorkDir    string // DISK-backed scratch for LARGE backup spooling

	// EncryptionKey is the 32-byte master key for AES-256-GCM.
	// Supplied via DOCKBACK_ENCRYPTION_KEY or DOCKBACK_ENCRYPTION_KEY_FILE.
	EncryptionKey []byte

	// EphemeralKey is true when no key was supplied and a throwaway one was
	// generated — backups made now are UNRECOVERABLE after restart.
	// Surfaced in the UI as a hard, non-dismissible warning.
	EphemeralKey bool

	// Initial admin credentials (single-admin model, PLAN §8.2). Used only to
	// bootstrap the first account if none exists.
	AdminUser string
	AdminPass string
	// AdminUserExplicit is true only when DOCKBACK_ADMIN_USER was actually set
	// (not the "admin" default). It gates renaming the existing admin to match a
	// changed env value, so simply removing the var never reverts the name.
	AdminUserExplicit bool

	// LocalDockerHost is the DOCKER_HOST for the bundled socket-proxy sidecar
	// (transport ①, PLAN §2.14). Empty disables the auto-registered local node.
	LocalDockerHost string

	// TrustProxy enables honoring X-Forwarded-* (Secure cookies / HSTS / real
	// client IP) from a trusted reverse proxy.
	TrustProxy bool

	// TrustedProxies restricts which direct peers' X-Forwarded-* headers are
	// honored (DOCKBACK_TRUSTED_PROXIES, comma-separated CIDRs or IPs). When
	// the app is ALSO reachable directly (e.g. LAN HTTP), this prevents untrusted
	// clients from spoofing X-Forwarded-For to evade the login lockout (§10.1).
	// Empty + TrustProxy=true falls back to trusting all peers (legacy; warned).
	TrustedProxies []*net.IPNet

	// EgressAllow is an optional outbound allow-list. When empty,
	// egress is unrestricted (the app only ever connects to operator-configured
	// destinations/notifiers anyway). When set, it is default-deny: storage and
	// notification hosts must match an entry or the connection is refused. Entries
	// are hostnames, *.suffix domains, IPs, or CIDRs (DOCKBACK_EGRESS_ALLOW).
	EgressAllow []string

	// Fleet-scale backup concurrency (PLAN §4.13/§9.9): a global cap and a
	// per-node cap so a nightly window across many nodes fans out fairly and one
	// busy node can't starve the others' backups.
	MaxConcurrentBackups int
	MaxConcurrentPerNode int

	// ScheduleJitter spreads the START of scheduled backups over a random window
	// (seconds) so a nightly window across many nodes doesn't thundering-herd the
	// network/storage at the same instant. 0 disables jitter (start
	// as soon as a slot is free). Interactive/manual backups are never jittered.
	ScheduleJitter int

	// MaxUploadMbps caps the AGGREGATE offsite upload rate (megabits/sec) across
	// ALL concurrent backups, so a nightly window can't saturate the uplink
	// (PLAN §4.13/§9.9). 0 = unlimited. Only offsite mirror uploads are throttled;
	// the local archive write is never throttled.
	MaxUploadMbps int

	// DBReadyTimeout is how long a restore/verify/drill waits (seconds) for a
	// throwaway database to accept connections before giving up. This is the env
	// DEFAULT; an in-app "Performance & tuning" setting overrides it live (F15).
	DBReadyTimeout int

	// F203: authentication policy — the absolute session lifetime, the sliding
	// idle window, and the shortest password accepted. Env sets the default; an
	// in-app setting overrides it within clamped bounds. MinPasswordLen has a
	// hard floor of 12: it can be raised, never lowered.
	SessionTTLHours    int
	SessionIdleMinutes int
	MinPasswordLen     int

	// SidecarImage is the tiny helper image used for raw volume copy/measurement.
	// This is the env DEFAULT (DOCKBACK_SIDECAR_IMAGE); an in-app setting overrides
	// it live so an air-gapped node can point at a mirror (F25). Default alpine:3.20.
	SidecarImage string

	// TLS enables optional built-in HTTPS so a direct deployment encrypts traffic
	// without a reverse proxy (PLAN §3.12/§10.3). Provide a cert+key file pair, or
	// set TLSSelfSigned for a generated in-memory cert. Default: plain HTTP behind
	// the operator's ingress (§8.5).
	TLSCert       string
	TLSKey        string
	TLSSelfSigned bool
}

// TLSEnabled reports whether the app should serve HTTPS directly.
func (c *Config) TLSEnabled() bool {
	return (c.TLSCert != "" && c.TLSKey != "") || c.TLSSelfSigned
}

// Load resolves configuration from the environment, applying safe defaults.
func Load() (*Config, error) {
	trustedProxies, err := parseCIDRs(os.Getenv("DOCKBACK_TRUSTED_PROXIES"))
	if err != nil {
		return nil, err
	}
	c := &Config{
		Host:              envOr("HOST", "127.0.0.1"),
		DataDir:           envOr("DOCKBACK_DATA_DIR", "/app/data"),
		BackupsDir:        envOr("DOCKBACK_BACKUPS_DIR", "/app/backups"),
		TmpDir:            envOr("DOCKBACK_TMP_DIR", "/tmp"),
		AdminUser:         envOr("DOCKBACK_ADMIN_USER", "admin"),
		AdminUserExplicit: os.Getenv("DOCKBACK_ADMIN_USER") != "",
		AdminPass:         os.Getenv("DOCKBACK_ADMIN_PASSWORD"),
		LocalDockerHost:   envOr("DOCKER_HOST", "tcp://socket-proxy:2375"),
		TrustProxy:        envBool("DOCKBACK_TRUST_PROXY", false),
		TrustedProxies:    trustedProxies,
		EgressAllow:       parseList(os.Getenv("DOCKBACK_EGRESS_ALLOW")),

		MaxConcurrentBackups: envIntMin("DOCKBACK_MAX_CONCURRENT_BACKUPS", 3, 1),
		MaxConcurrentPerNode: envIntMin("DOCKBACK_MAX_CONCURRENT_PER_NODE", 2, 1),
		ScheduleJitter:       envIntMin("DOCKBACK_SCHEDULE_JITTER", 30, 0),
		MaxUploadMbps:        envIntMin("DOCKBACK_MAX_UPLOAD_MBPS", 0, 0),
		DBReadyTimeout:       envIntMin("DOCKBACK_DB_READY_TIMEOUT", 300, 10),

		// F203: the authentication policy, which was the one part of the app that
		// could not be adjusted without a rebuild. A deployment reached only over a
		// VPN may want a longer session; one on a shared machine a much shorter
		// one. Both were previously a recompile.
		//
		// MinPasswordLen has a FLOOR rather than a default: envIntMin already
		// refuses anything below its minimum, so 12 is not merely the starting
		// value but the shortest password this app will ever accept. Making the
		// baseline weaker is not a supported configuration.
		SessionTTLHours:    envIntMin("DOCKBACK_SESSION_TTL_HOURS", 12, 1),
		SessionIdleMinutes: envIntMin("DOCKBACK_SESSION_IDLE_MINUTES", 30, 1),
		MinPasswordLen:     envIntMin("DOCKBACK_MIN_PASSWORD_LEN", 12, 12),
		SidecarImage:       envOr("DOCKBACK_SIDECAR_IMAGE", "alpine:3.20"),
	}
	// F203: hand the store its session idle window. The store is a leaf package
	// that deliberately imports no config, so the value is pushed in at boot —
	// the same shape as the egress policy just below.
	store.SetSessionIdleTTL(time.Duration(c.SessionIdleMinutes) * time.Minute)

	// Install the process-wide egress policy. Empty = unrestricted.
	egress.Configure(c.EgressAllow)
	if len(c.EgressAllow) > 0 {
		fmt.Fprintf(os.Stderr, "Egress allow-list active (%d entr%s): outbound storage/notification hosts not listed in DOCKBACK_EGRESS_ALLOW will be refused.\n",
			len(c.EgressAllow), plural(len(c.EgressAllow), "y", "ies"))
	}
	// Large backups spool their raw volume tar to disk here, NOT to the RAM-backed
	// /tmp — so a multi-GB volume (Jellyfin/Plex config) doesn't exhaust memory
	//. Defaults to a hidden dir on the backups volume; override to a
	// dedicated/larger disk with DOCKBACK_WORK_DIR.
	c.WorkDir = envOr("DOCKBACK_WORK_DIR", filepath.Join(c.BackupsDir, ".work"))

	if c.TrustProxy && len(c.TrustedProxies) == 0 {
		fmt.Fprintln(os.Stderr, "WARNING: DOCKBACK_TRUST_PROXY=true with no DOCKBACK_TRUSTED_PROXIES set — "+
			"X-Forwarded-* is trusted from ANY peer. If the app is also reachable directly (e.g. LAN HTTP), set "+
			"DOCKBACK_TRUSTED_PROXIES to your proxy's source CIDR(s) so clients can't spoof X-Forwarded-For (§10.1).")
	}

	port, err := strconv.Atoi(envOr("PORT", "28734"))
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid PORT %q", os.Getenv("PORT"))
	}
	c.Port = port

	// Optional built-in TLS. cert+key must be provided together;
	// self-signed is a separate quick-LAN option.
	c.TLSCert = os.Getenv("DOCKBACK_TLS_CERT")
	c.TLSKey = os.Getenv("DOCKBACK_TLS_KEY")
	c.TLSSelfSigned = envBool("DOCKBACK_TLS_SELF_SIGNED", false)
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return nil, fmt.Errorf("DOCKBACK_TLS_CERT and DOCKBACK_TLS_KEY must be set together")
	}

	key, ephemeral, err := loadKey()
	c.EphemeralKey = ephemeral
	if err != nil {
		return nil, err
	}
	c.EncryptionKey = key

	return c, nil
}

// loadKey resolves the 32-byte master encryption key. Accepts either a 64-char
// hex string or a file containing one. If none is provided we generate an
// ephemeral key and warn (dev only) — production MUST supply a persistent key,
// since losing it makes every backup unrecoverable.
func loadKey() (key []byte, ephemeral bool, err error) {
	// Passphrase-wrapped keyfile: the master key is stored encrypted
	// at rest and unwrapped with an argon2id-derived KEK at boot. The passphrase
	// comes from env / a Docker secret, so unattended restart still works.
	if kf := os.Getenv("DOCKBACK_ENCRYPTION_KEYFILE"); kf != "" {
		data, rerr := os.ReadFile(kf)
		if rerr != nil {
			return nil, false, fmt.Errorf("reading DOCKBACK_ENCRYPTION_KEYFILE: %w", rerr)
		}
		pass := os.Getenv("DOCKBACK_ENCRYPTION_PASSPHRASE")
		if pf := os.Getenv("DOCKBACK_ENCRYPTION_PASSPHRASE_FILE"); pf != "" {
			b, perr := os.ReadFile(pf)
			if perr != nil {
				return nil, false, fmt.Errorf("reading DOCKBACK_ENCRYPTION_PASSPHRASE_FILE: %w", perr)
			}
			pass = strings.TrimSpace(string(b))
		}
		if pass == "" {
			return nil, false, fmt.Errorf("DOCKBACK_ENCRYPTION_KEYFILE is set but no DOCKBACK_ENCRYPTION_PASSPHRASE[_FILE] provided")
		}
		k, uerr := crypto.UnwrapKeyfile(data, pass)
		if uerr != nil {
			return nil, false, fmt.Errorf("unwrapping encryption keyfile: %w", uerr)
		}
		return k, false, nil
	}

	raw := os.Getenv("DOCKBACK_ENCRYPTION_KEY")
	if f := os.Getenv("DOCKBACK_ENCRYPTION_KEY_FILE"); f != "" {
		b, rerr := os.ReadFile(f)
		if rerr != nil {
			return nil, false, fmt.Errorf("reading DOCKBACK_ENCRYPTION_KEY_FILE: %w", rerr)
		}
		raw = strings.TrimSpace(string(b))
	}

	if raw == "" {
		k := make([]byte, 32)
		if _, rerr := rand.Read(k); rerr != nil {
			return nil, false, rerr
		}
		fmt.Fprintln(os.Stderr, "WARNING: no DOCKBACK_ENCRYPTION_KEY set; generated an EPHEMERAL key. "+
			"Backups made now are UNRECOVERABLE after restart. Set a persistent key in production.")
		return k, true, nil
	}

	key, err = hex.DecodeString(raw)
	if err != nil {
		return nil, false, fmt.Errorf("DOCKBACK_ENCRYPTION_KEY must be hex: %w", err)
	}
	if len(key) != 32 {
		return nil, false, fmt.Errorf("DOCKBACK_ENCRYPTION_KEY must decode to 32 bytes (got %d)", len(key))
	}
	return key, false, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envBool(k string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(k)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

// envIntMin reads an int env var, clamping to at least min and falling back to
// def when unset/invalid.
func envIntMin(k string, def, min int) int {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < min {
		return def
	}
	return n
}

// parseList splits a comma-separated env value into trimmed, non-empty entries.
func parseList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// plural returns one or many depending on n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// parseCIDRs parses a comma-separated list of CIDRs or bare IPs into networks.
// Bare IPs become /32 (IPv4) or /128 (IPv6). Invalid entries are skipped.
// parseCIDRs parses the comma-separated trusted-proxy allow-list. Every entry
// must parse: skipping a typo silently turns an allow-list into trust-everyone,
// which is the opposite of what the operator wrote it for, and the only notice
// was a stderr line nobody reads on a NAS. An empty value is not an error — it
// is the documented "no allow-list" mode.
func parseCIDRs(s string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, part := range strings.Split(s, ",") {
		entry := strings.TrimSpace(part)
		if entry == "" {
			continue
		}
		n, ok := egress.ParseCIDROrIP(entry)
		if !ok {
			return nil, fmt.Errorf("DOCKBACK_TRUSTED_PROXIES: cannot parse %q", entry)
		}
		out = append(out, n)
	}
	return out, nil
}
