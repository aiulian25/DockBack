// Package notify delivers backup outcome notifications to pluggable channels
// (Gotify, email/SMTP, generic webhook). It exists to solve the original
// "found out 3 months later" problem: verification failures (and backup
// failures) are pushed out immediately (PLAN §4.10). The package is a leaf — it
// neither imports the store nor decrypts anything; the caller supplies a loader
// that returns the (already-decrypted) Config.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/smtp"
	"strings"
	"sync/atomic"
	"time"

	"dockback/internal/egress"
)

// Event kinds (PLAN §4.10/§9.15).
const (
	KindBackupSuccess      = "backup.success"
	KindBackupFailed       = "backup.failed"
	KindVerifyFailed       = "verify.failed"
	KindScrubFailed        = "scrub.failed"            // stored backup no longer verifies (bit-rot / dest gone bad)
	KindNoOffsite          = "offsite.missing"         // a backup didn't reach one/more offsite destinations (3-2-1 broken)
	KindDestFull           = "destination.full"        // a destination is >90% full
	KindDestForecast       = "destination.forecast"    // a destination is trending to fill up soon
	KindMissedSchedule     = "schedule.missed"         // a scheduled window was missed (app was down)
	KindTargetMissing      = "schedule.target_missing" // a scheduled container target no longer exists
	KindLowRPOPaused       = "critical.rpo_paused"     // low-RPO auto-backups paused after repeated verification failures
	KindKeyUnescrowed      = "key.unescrowed"          // the master key isn't confirmed backed up, yet backups exist
	KindBackupAnomaly      = "backup.anomaly"          // a backup's duration/size deviated sharply from the container's norm (F28)
	KindContainerCrashed   = "container.crashed"       // a watched container died with a non-zero exit code (F24)
	KindContainerOOM       = "container.oom"           // a watched container was killed for running out of memory (F24)
	KindHostKeyChanged     = "node.hostkey_changed"    // an SSH host key no longer matches its pin — possible reinstall or MITM (F67)
	KindSidecarChanged     = "node.sidecar_changed"    // the volume sidecar image no longer matches its pin — possible supply-chain tamper (F88)
	KindCoverageStale      = "coverage.stale"          // a container's newest backup is older than its schedule allows
	KindProjectUnscheduled = "coverage.new_project"    // a new compose project appeared that no schedule covers
	KindAppBackupStale     = "appbackup.stale"         // DockBack's own newest backup is too old, or there is none
	KindAppBackupFailed    = "appbackup.failed"        // DockBack's own scheduled backup, or its push off the machine, failed

	// Authentication-surface events (F198).
	//
	// Every other tamper signal in this list already reaches the operator's
	// channels — a changed host key, a changed sidecar digest. An attack on the
	// LOGIN surface did not: lockouts, failed step-ups and master-key reveals
	// were written to the audit table and nowhere else, so they were invisible
	// unless somebody thought to go and read it. These are the events where the
	// gap between "recorded" and "noticed" is the whole cost.
	KindAuthLockout     = "auth.lockout"       // repeated failures locked further sign-in attempts from an address
	KindAuthFailedBurst = "auth.failed_burst"  // sustained failures from one address, before it locks — the early warning
	KindStepUpFailed    = "auth.stepup_failed" // a live session failed re-authentication for a destructive action
	KindKeyRevealed     = "key.revealed"       // the master encryption key was displayed to someone

	// KindAuditBeacon publishes the audit chain's head OUT of the machine (F200).
	//
	// Info severity on purpose: a checkpoint is routine, and routing it as an
	// alert would train an operator to ignore the channel that carries the real
	// ones. Its value is not in being READ — it is in being somewhere DockBack
	// cannot reach to change it later.
	KindAuditBeacon = "audit.beacon"

	// Restore outcomes (F92). Previously these all fired as scrub.failed, which
	// made "a stored backup rotted overnight" and "the restore I am running right
	// now failed" indistinguishable to severity routing and to the alert inbox —
	// one is hygiene, the other is an incident in progress.
	KindRestoreFailed     = "restore.failed"      // a live restore did not complete
	KindRestoreRolledBack = "restore.rolled_back" // unhealthy → rolled back to the safety snapshot
	KindRestoreCanceled   = "restore.canceled"    // operator stopped a restore mid-run

	// KindConfigTrustAllProxies: proxy trust is on with no allow-list, so every
	// caller's X-Forwarded-For is believed (§10.1). Critical rather than a
	// warning because it does not degrade a protection, it removes one: the
	// login lockout, API-token source pins and the audit trail's IP column are
	// all bypassable with a single request header while it holds.
	KindConfigTrustAllProxies = "config.trust_all_proxies"
)

// Severity classifies an event so channels can route/prioritize it (PLAN §9.15).
type Severity int

const (
	SevInfo     Severity = iota // routine success
	SevWarning                  // operational alert — action recommended
	SevCritical                 // data at risk — act now
)

// SeverityOf maps an event kind to its severity (PLAN §9.15). Unknown kinds are
// treated as warnings so a new alert is never silently dropped as "success".
func SeverityOf(kind string) Severity {
	switch kind {
	case KindBackupSuccess:
		return SevInfo
	case KindAuditBeacon:
		// A checkpoint is routine bookkeeping, not an alert (F200). Routing it as
		// one would train the operator to ignore the channel that carries the real
		// alerts — and its value never depended on being read, only on existing
		// somewhere this machine cannot rewrite.
		return SevInfo
	case KindBackupFailed, KindVerifyFailed, KindScrubFailed, KindHostKeyChanged, KindSidecarChanged,
		KindRestoreFailed, KindRestoreRolledBack,
		KindAuthLockout, KindKeyRevealed, KindConfigTrustAllProxies:
		// A pin mismatch is a possible MITM or supply-chain tamper (F67/F88); a
		// failed or rolled-back restore is a recovery that did not work (F92).
		//
		// A lockout means somebody guessed at the console until it stopped them,
		// and a key reveal means the one secret that decrypts every backup ever
		// written was put on a screen (F198). Both belong with the tamper signals:
		// if either was not you, everything else in this app is already at risk.
		return SevCritical
	case KindAuthFailedBurst, KindStepUpFailed:
		// Warning, not critical, and deliberately so. A burst is the early signal
		// BEFORE a lockout — often a person mistyping — and a failed step-up is a
		// session that already exists failing to escalate. Both are worth seeing;
		// neither is proof of compromise, and paging on a typo is how an operator
		// learns to ignore the channel that later carries the real one.
		return SevWarning
	case KindRestoreCanceled:
		// The operator chose to stop it, so nothing is wrong — but a destructive
		// operation was interrupted mid-run and that is worth a record (F92).
		return SevWarning
	case KindContainerCrashed, KindContainerOOM:
		return SevWarning // operational — a watched container crashed/was OOM-killed (F24)
	default: // offsite.missing, destination.full, schedule.missed, key.unescrowed, …
		return SevWarning
	}
}

func (s Severity) label() string {
	switch s {
	case SevCritical:
		return "critical"
	case SevWarning:
		return "warning"
	default:
		return "info"
	}
}

// String is the stable lower-case severity name ("info"|"warning"|"critical"),
// used when persisting an alert row (F46).
func (s Severity) String() string { return s.label() }

// subjectPrefix tags an email subject so severity is visible for inbox filtering
// — the email-native equivalent of the Gotify priority band and the webhook's
// severity field (PLAN §9.15). Info is unprefixed to keep routine successes tidy.
func (s Severity) subjectPrefix() string {
	switch s {
	case SevCritical:
		return "[CRITICAL] "
	case SevWarning:
		return "[WARNING] "
	default:
		return ""
	}
}

// priority maps severity to a channel priority band (Gotify-style 1-10).
func (s Severity) priority() int {
	switch s {
	case SevCritical:
		return 9
	case SevWarning:
		return 7
	default:
		return 5
	}
}

// IsFailure reports whether a kind is anything but a routine success — used to
// gate the dead-man's-switch heartbeat (only a verified success pings).
func IsFailure(kind string) bool { return kind != KindBackupSuccess }

// MinSeverity (F14) is an optional per-channel routing floor: when set to
// "info" | "warning" | "critical" it SUPERSEDES the OnSuccess/OnFailure toggles,
// so an event reaches the channel only if its severity is at least this level
// (e.g. email on "critical" only, Gotify on "warning" and up). Empty = use the
// legacy OnSuccess/OnFailure toggles, so existing configs are unchanged.

// GotifyConfig pushes to a Gotify server (self-hosted push).
type GotifyConfig struct {
	Enabled     bool   `json:"enabled"`
	OnSuccess   bool   `json:"on_success"`
	OnFailure   bool   `json:"on_failure"`
	MinSeverity string `json:"min_severity,omitempty"` // "" | info | warning | critical (F14)
	URL         string `json:"url"`
	Token       string `json:"token"` // secret (app token)
	Priority    int    `json:"priority"`
}

// EmailConfig sends via SMTP (STARTTLS on submission ports like 587).
type EmailConfig struct {
	Enabled     bool   `json:"enabled"`
	OnSuccess   bool   `json:"on_success"`
	OnFailure   bool   `json:"on_failure"`
	MinSeverity string `json:"min_severity,omitempty"` // "" | info | warning | critical (F14)
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Username    string `json:"username"`
	Password    string `json:"password"` // secret
	From        string `json:"from"`
	To          string `json:"to"`
}

// WebhookConfig POSTs the event as JSON to an arbitrary URL (covers Slack, etc.).
type WebhookConfig struct {
	Enabled     bool   `json:"enabled"`
	OnSuccess   bool   `json:"on_success"`
	OnFailure   bool   `json:"on_failure"`
	MinSeverity string `json:"min_severity,omitempty"` // "" | info | warning | critical (F14)
	URL         string `json:"url"`
}

// HeartbeatConfig pings an external monitor (healthchecks.io / Uptime Kuma /
// generic URL) on every VERIFIED backup — a dead-man's-switch: if DockBack
// dies entirely, the *absence* of the ping alerts you, which in-app alerting
// never can (PLAN §9.11). IntervalMinutes optionally also sends a keep-alive
// even between backups (0 = only on verified backup).
type HeartbeatConfig struct {
	Enabled         bool   `json:"enabled"`
	URL             string `json:"url"`
	IntervalMinutes int    `json:"interval_minutes"`
}

// DigestConfig controls the optional daily summary (F15): instead of (or in
// addition to) one notification per verified backup, DockBack sends a single
// digest at a chosen local time — "Last 24h: N backups, N verified, N failed" —
// so a nightly fleet window doesn't bury the "it ran" signal in dozens of pings.
// SuccessMode governs the per-backup success ping: "per_backup" (default/empty,
// today's behavior), "digest" (suppress per-backup success — the digest replaces
// it), or "both". Failures and critical alerts are never affected — they always
// notify immediately.
type DigestConfig struct {
	Enabled     bool   `json:"enabled"`
	Time        string `json:"time"`         // "HH:MM" local time to send the summary
	SuccessMode string `json:"success_mode"` // "" | per_backup | digest | both
	// IncludeDR appends a DR-confidence block (drills/partial/key status) to the
	// digest (F60). A POINTER so absent = on: existing sealed configs opt in
	// automatically, and the UI can still explicitly turn it off.
	IncludeDR *bool `json:"include_dr,omitempty"`
}

// HeadBeaconConfig publishes the audit trail's chain head to the configured
// channels on a cadence (F200).
//
// The audit chain is HMAC-linked under the master key, which makes it
// tamper-evident — but only against someone who cannot recompute it. An
// attacker with a shell in this container holds the key AND the anchor AND the
// database, so they can rewrite history end to end and pass verification. The
// same threat model the write-only backup mode already takes seriously.
//
// The fix is not more cryptography, it is somewhere else to keep the answer. A
// head that left the machine — sitting in an inbox, a webhook receiver, a push
// notification — is one the attacker never controlled, and comparing today's
// database against it is what turns "tamper-evident if the database is honest"
// into "tamper-evident against a compromise of this container".
//
// IntervalHours is the cadence; 0 uses the default (24h).
type HeadBeaconConfig struct {
	Enabled       bool `json:"enabled"`
	IntervalHours int  `json:"interval_hours"`
}

// Config is the full notification configuration.
type Config struct {
	Gotify     GotifyConfig     `json:"gotify"`
	Email      EmailConfig      `json:"email"`
	Webhook    WebhookConfig    `json:"webhook"`
	Heartbeat  HeartbeatConfig  `json:"heartbeat"`
	Digest     DigestConfig     `json:"digest"`
	HeadBeacon HeadBeaconConfig `json:"head_beacon"`
}

// wants reports whether a channel subscribed to this event's class. Success
// (info) routes on the success toggle; warnings AND criticals both route on the
// failure toggle — i.e. "failures & operational alerts" — so severity §9.15
// alerts are never buried in (or gated behind) a success digest, without adding
// per-event config the user would have to manage.
func wants(onSuccess, onFailure bool, sev Severity) bool {
	if sev == SevInfo {
		return onSuccess
	}
	return onFailure
}

// parseMinSeverity maps a per-channel minimum-severity string to its threshold
// (F14). An empty or unrecognized value returns SevWarning — but callers only
// reach this with a non-empty, explicit value (empty routes via the legacy
// toggles), so an unknown string degrades to the safe "alerts & failures" floor
// rather than silently dropping events.
func parseMinSeverity(min string) Severity {
	switch strings.ToLower(strings.TrimSpace(min)) {
	case "info":
		return SevInfo
	case "critical":
		return SevCritical
	default: // "warning" or any unknown explicit value
		return SevWarning
	}
}

// meetsMin reports whether an event's severity is at least a channel's configured
// minimum ("info" | "warning" | "critical"), i.e. info <= warning <= critical, so
// a higher-severity event always passes a lower floor (F14).
func meetsMin(sev Severity, min string) bool {
	return sev >= parseMinSeverity(min)
}

// subscribes decides whether a channel should receive an event (F14): an explicit
// per-channel MinSeverity floor drives routing when set; otherwise it falls back
// to the legacy OnSuccess/OnFailure toggles, so existing configs behave exactly as
// before.
func subscribes(minSeverity string, onSuccess, onFailure bool, sev Severity) bool {
	if strings.TrimSpace(minSeverity) == "" {
		return wants(onSuccess, onFailure, sev)
	}
	return meetsMin(sev, minSeverity)
}

// Dispatcher fans an event out to the enabled, subscribed channels.
type Dispatcher struct {
	Load     func() (Config, error)
	Log      func(level, msg string)
	hc       *http.Client
	lastBeat atomic.Int64 // unix secs of the last successful heartbeat (PLAN §9.11)
}

// New builds a Dispatcher. load returns the decrypted config; log is best-effort.
func New(load func() (Config, error), log func(level, msg string)) *Dispatcher {
	if log == nil {
		log = func(string, string) {}
	}
	// Guard outbound HTTP at the dial so a redirect to a non-allowed host is
	// refused too (PLAN §3.10 default-deny egress allow-list).
	tr := http.DefaultTransport.(*http.Transport).Clone()
	// Resolved at dial time, not here: this Dispatcher is built once at boot and
	// outlives every live allow-list change made in Settings (F39).
	tr.DialContext = egress.GuardDialDefault(tr.DialContext)
	return &Dispatcher{Load: load, Log: log, hc: &http.Client{Timeout: 15 * time.Second, Transport: tr}}
}

func (d *Dispatcher) logf(level, format string, a ...any) {
	if d == nil || d.Log == nil {
		return
	}
	d.Log(level, fmt.Sprintf(format, a...))
}

// Send delivers an event to every enabled channel that subscribes to its
// success/failure class. Best-effort: a channel error is logged, never returned,
// so a flaky notifier never affects a backup. Call it from a goroutine.
//
// A dispatcher with no config loader sends nothing rather than panicking. That
// matters more than it looks: Send is always invoked as `go d.Send(...)`, and a
// nil dereference inside a goroutine is unrecoverable — it takes the whole
// process down, not the one request. Since F198 wired this into the LOGIN path,
// an un-wired dispatcher would have turned a failed sign-in into a crash, which
// is the one place a notification must never be able to reach.
func (d *Dispatcher) Send(kind, title, message string) {
	if d == nil || d.Load == nil {
		return
	}
	cfg, err := d.Load()
	if err != nil {
		d.logf("WARN", "notifications: could not load config: %v", err)
		return
	}
	sev := SeverityOf(kind)
	priority := sev.priority()
	if cfg.Gotify.Enabled && subscribes(cfg.Gotify.MinSeverity, cfg.Gotify.OnSuccess, cfg.Gotify.OnFailure, sev) {
		if err := d.sendGotify(cfg.Gotify, title, message, priority); err != nil {
			d.logf("WARN", "notifications: gotify failed: %v", err)
		}
	}
	if cfg.Email.Enabled && subscribes(cfg.Email.MinSeverity, cfg.Email.OnSuccess, cfg.Email.OnFailure, sev) {
		if err := d.sendEmail(cfg.Email, sev, title, message); err != nil {
			d.logf("WARN", "notifications: email failed: %v", err)
		}
	}
	if cfg.Webhook.Enabled && subscribes(cfg.Webhook.MinSeverity, cfg.Webhook.OnSuccess, cfg.Webhook.OnFailure, sev) {
		if err := d.sendWebhook(cfg.Webhook, kind, sev, title, message); err != nil {
			d.logf("WARN", "notifications: webhook failed: %v", err)
		}
	}
	// Heartbeat / dead-man's-switch (PLAN §9.11): ping the external monitor on a
	// VERIFIED backup success only. A failure deliberately does NOT ping, so the
	// monitor's absence-of-heartbeat catches sustained failure.
	if kind == KindBackupSuccess && cfg.Heartbeat.Enabled {
		if err := d.sendHeartbeat(cfg.Heartbeat); err != nil {
			d.logf("WARN", "notifications: heartbeat failed: %v", err)
		}
	}
}

// TestChannel synchronously sends a test notification to one channel and returns
// the error (for a "Send test" button). channel is "gotify"|"email"|"webhook".
func (d *Dispatcher) TestChannel(cfg Config, channel string) error {
	const t, m = "DockBack test notification", "If you can read this, notifications are configured correctly."
	switch channel {
	case "gotify":
		return d.sendGotify(cfg.Gotify, t, m, 5)
	case "email":
		return d.sendEmail(cfg.Email, SevInfo, t, m)
	case "webhook":
		return d.sendWebhook(cfg.Webhook, "test", SevInfo, t, m)
	case "heartbeat":
		return d.sendHeartbeat(cfg.Heartbeat)
	}
	return fmt.Errorf("unknown channel %q", channel)
}

// httpStatusError turns a non-2xx response into an error that includes a short,
// cleaned snippet of the response body. The body usually carries the ACTIONABLE
// reason a bare status code hides — e.g. a paused Uptime Kuma push monitor
// answers 404 with `{"ok":false,"msg":"Monitor not found or not active."}`, which
// tells the operator exactly what to fix. Bounded + whitespace-collapsed so a
// large HTML error page can't bloat or wrap the log line.
func httpStatusError(what string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	snippet := strings.Join(strings.Fields(string(body)), " ") // collapse newlines/runs of space
	if len(snippet) > 200 {
		snippet = snippet[:200] + "…"
	}
	if snippet == "" {
		return fmt.Errorf("%s HTTP %d", what, resp.StatusCode)
	}
	return fmt.Errorf("%s HTTP %d: %s", what, resp.StatusCode, snippet)
}

// sendHeartbeat pings the external monitor URL (healthchecks.io / Uptime Kuma /
// generic). A plain GET is the most widely compatible "I'm alive" ping. Egress
// allow-listed and bounded by the shared client timeout (PLAN §9.11/§3.10).
func (d *Dispatcher) sendHeartbeat(hb HeartbeatConfig) error {
	if hb.URL == "" {
		return fmt.Errorf("heartbeat URL required")
	}
	if err := egress.Default().Enforce(hb.URL); err != nil { // F207: audit mode observes instead of blocking
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hb.URL, nil)
	if err != nil {
		return err
	}
	resp, err := d.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return httpStatusError("heartbeat", resp)
	}
	d.lastBeat.Store(time.Now().Unix())
	return nil
}

// HeartbeatTick sends a keep-alive heartbeat when the configured interval has
// elapsed since the last successful ping (PLAN §9.11). The server calls this
// periodically; it's a no-op unless the interval mode is enabled. The first tick
// after startup pings immediately to establish liveness.
func (d *Dispatcher) HeartbeatTick() {
	cfg, err := d.Load()
	if err != nil || !cfg.Heartbeat.Enabled || cfg.Heartbeat.IntervalMinutes <= 0 {
		return
	}
	last := d.lastBeat.Load()
	if last != 0 && time.Now().Unix()-last < int64(cfg.Heartbeat.IntervalMinutes)*60 {
		return
	}
	if err := d.sendHeartbeat(cfg.Heartbeat); err != nil {
		d.logf("WARN", "notifications: heartbeat keep-alive failed: %v", err)
	}
}

func (d *Dispatcher) sendGotify(g GotifyConfig, title, message string, priority int) error {
	// Trim HERE as well as on save: a token stored before trimming existed would
	// otherwise keep failing until the operator noticed and re-pasted it. A
	// copied token routinely carries a trailing space or newline, and Gotify
	// compares the header value exactly — one invisible byte reads as a 401 that
	// looks identical to a wrong token.
	g.URL = strings.TrimSpace(g.URL)
	g.Token = strings.TrimSpace(g.Token)
	if g.URL == "" || g.Token == "" {
		return fmt.Errorf("gotify URL and token required")
	}
	if err := egress.Default().Enforce(g.URL); err != nil { // F207: audit mode observes instead of blocking
		return err
	}
	if g.Priority > 0 {
		priority = g.Priority
	}
	body, _ := json.Marshal(map[string]any{"title": title, "message": message, "priority": priority})
	url := strings.TrimRight(g.URL, "/") + "/message"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gotify-Key", g.Token)
	resp, err := d.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		if resp.StatusCode == http.StatusUnauthorized {
			return gotifyAuthError(resp, g.Token)
		}
		return httpStatusError("gotify", resp)
	}
	return nil
}

// gotifyAuthError turns Gotify's generic 401 into something an operator can act
// on, using the shape of the token to say WHICH of three different problems
// they have.
//
// Gotify answers every authentication failure with the same sentence, so the
// raw status is useless on its own. The token format is not: since Gotify's
// "enhanced token" change, an APPLICATION token is "gtfya.<key>" and a CLIENT
// token is "gtfyc.<key>" (clients may only READ messages, so posting with one
// fails exactly like a wrong token). Anything without the "gtfy" prefix is a
// pre-upgrade token, which an upgraded server no longer recognises — the
// commonest way this breaks: the server was updated, and the stored token is
// simply the old one.
//
// The token itself is never included, only what its prefix implies: this string
// reaches the UI, the run log and the alert inbox.
func gotifyAuthError(resp *http.Response, token string) error {
	const base = "gotify rejected the token (HTTP 401)"
	switch {
	case strings.HasPrefix(token, gotifyClientTokenPrefix):
		return fmt.Errorf("%s — this is a CLIENT token (it starts with %q). Clients may only read messages. Open Gotify -> Apps, create or select an application, and use ITS token instead",
			base, gotifyClientTokenPrefix)
	case strings.HasPrefix(token, gotifyAppTokenPrefix):
		return fmt.Errorf("%s — the token is the right kind (an application token) but this Gotify server does not know it. It was most likely revoked, regenerated, or belongs to a different Gotify. Copy the current token from Gotify -> Apps and save it again",
			base)
	case strings.HasPrefix(token, gotifyTokenPrefix):
		return fmt.Errorf("%s — the token has an unexpected Gotify prefix. Use the token shown for an application under Gotify -> Apps", base)
	default:
		return fmt.Errorf("%s — this looks like a token from BEFORE Gotify's token change (current tokens start with %q and are far longer). Upgrading Gotify invalidates the old ones: open Gotify -> Apps and copy the application's current token",
			base, gotifyAppTokenPrefix)
	}
}

// Gotify's token prefixes. An application token may post messages; a client
// token may only read them. See gotifyAuthError.
const (
	gotifyTokenPrefix       = "gtfy"
	gotifyAppTokenPrefix    = "gtfya"
	gotifyClientTokenPrefix = "gtfyc"
)

func (d *Dispatcher) sendWebhook(wh WebhookConfig, kind string, sev Severity, title, message string) error {
	if wh.URL == "" {
		return fmt.Errorf("webhook URL required")
	}
	if err := egress.Default().Enforce(wh.URL); err != nil { // F207: audit mode observes instead of blocking
		return err
	}
	body, _ := json.Marshal(map[string]any{
		"kind": kind, "severity": sev.label(), "title": title, "message": message,
		"text": title + "\n" + message, // Slack-compatible field
		"time": time.Now().UTC().Format(time.RFC3339),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, wh.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return httpStatusError("webhook", resp)
	}
	return nil
}

func (d *Dispatcher) sendEmail(e EmailConfig, sev Severity, title, message string) error {
	if e.Host == "" || e.From == "" || e.To == "" {
		return fmt.Errorf("email host, from and to are required")
	}
	if err := egress.Default().Enforce(e.Host); err != nil { // F207: audit mode observes instead of blocking
		return err
	}
	port := e.Port
	if port == 0 {
		port = 587
	}
	addr := fmt.Sprintf("%s:%d", e.Host, port)
	var auth smtp.Auth
	if e.Username != "" {
		auth = smtp.PlainAuth("", e.Username, e.Password, e.Host)
	}
	to := splitAddrs(e.To)
	msg := buildMessage(e.From, to, sev.subjectPrefix()+title, message)
	return smtp.SendMail(addr, auth, e.From, to, msg)
}

func splitAddrs(s string) []string {
	var out []string
	for _, a := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' }) {
		if a != "" {
			out = append(out, a)
		}
	}
	return out
}

func buildMessage(from string, to []string, subject, body string) []byte {
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + strings.Join(to, ", ") + "\r\n")
	b.WriteString("Subject: " + subject + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)
	b.WriteString("\r\n")
	return []byte(b.String())
}
