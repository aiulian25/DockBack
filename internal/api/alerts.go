package api

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"dockback/internal/notify"
	"dockback/internal/storage"
	"dockback/internal/store"
)

// Severity-routed operational alerting (PLAN §9.15).
//
// Beyond backup/verification failures (§4.10), these conditions are pushed as
// high-priority *warnings* so they aren't buried in a success digest (or the log
// only): a backup that missed its offsite copy, a destination running out of
// room, a missed schedule, and — the catastrophic one — the master key not being
// confirmed backed up while backups exist. Success stays info; failures and
// corruption stay critical (severity is derived in the notify package).
//
// Recurring warnings are THROTTLED with a per-condition cooldown persisted in
// settings, so a sustained condition (a destination stuck at 95%, a dead offsite
// target) alerts once per window instead of on every backup.

const alertMonitorInterval = time.Hour

// Cooldowns for throttled, recurring warnings.
const (
	offsiteAlertCooldown    = 6 * time.Hour
	destFullCooldown        = 24 * time.Hour
	keyAlertCooldown        = 24 * time.Hour
	targetMissingCooldown   = 24 * time.Hour
	lowRPOPausedCooldown    = 6 * time.Hour
	destForecastCooldown    = 24 * time.Hour
	anomalyCooldown         = 24 * time.Hour // per-container backup drift warning (F28)
	crashAlertCooldown      = 6 * time.Hour  // per-container crash/OOM warning — a crash-loop alerts once per window (F24)
	trustAllProxiesCooldown = 24 * time.Hour
)

// Default alert thresholds (F18) — each is overridable in Settings → Performance
// & tuning so users on tiny or huge destinations can tune when the warnings fire.
const (
	defaultDestFullPct  = 90 // "almost full" fires at this used percentage
	defaultForecastDays = 30 // "filling up" fires when projected to fill within this many days
)

// destFullThreshold is the used FRACTION at which a destination is "nearly full"
// (setting alert.dest_full_pct, default 90%, clamped 50..99 — F18).
func (s *Server) destFullThreshold() float64 {
	pct := s.settingInt("alert.dest_full_pct", defaultDestFullPct)
	if pct < 50 {
		pct = 50
	}
	if pct > 99 {
		pct = 99
	}
	return float64(pct) / 100
}

// forecastWarnDays is how far ahead a projected fill-up triggers a warning
// (setting alert.forecast_days, default 30, clamped >= 1 — F18).
func (s *Server) forecastWarnDays() int {
	d := s.settingInt("alert.forecast_days", defaultForecastDays)
	if d < 1 {
		d = 1
	}
	return d
}

// humanBytes formats a byte count for an alert message (e.g. "4.2 GB").
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// notify dispatches an event to the configured channels (async, best-effort) and
// — for warning/critical kinds — first persists it to the Alerts inbox (F46), so an
// operational warning is never lost when no channel is configured or the app
// restarts. Severity/priority/routing are decided in the notify package by kind.
func (s *Server) notify(kind, title, message string) {
	s.storeAlertRow(kind, "", title, message)
	go s.notifier.Send(kind, title, message)
}

// alertDedup is the dedup key stored on an alert row: the kind, optionally scoped
// (e.g. "destination.full:d1") so a recurring per-target condition is tracked
// independently.
func alertDedup(kind, dedup string) string {
	if dedup == "" {
		return kind
	}
	return kind + ":" + dedup
}

// storeAlertRow persists a warning/critical alert (F46). Info-severity events are
// deliberately NOT stored — routine successes are the digest's job, and keeping
// them out keeps the inbox meaningful. Best-effort: a store failure never blocks
// the outbound notification.
func (s *Server) storeAlertRow(kind, dedup, title, message string) {
	sev := notify.SeverityOf(kind)
	if sev == notify.SevInfo {
		return
	}
	_ = s.store.InsertAlert(&store.Alert{
		TS: time.Now().Unix(), Kind: kind, Severity: sev.String(),
		Title: title, Message: message, Dedup: alertDedup(kind, dedup),
	})
}

// shouldStoreAlert decides whether a THROTTLED alert should be persisted this call
// (F46), independent of whether the outbound send is cooldown-suppressed:
//   - outside cooldown → always record (a fresh occurrence of the condition);
//   - inside cooldown → record only if the last stored alert for this dedup was
//     acknowledged, so a persisting condition resurfaces once the operator clears
//     it, but doesn't pile up duplicate unacked rows while it's still pending.
//
// Pure, for testability.
func shouldStoreAlert(lastForDedup *store.Alert, cooldownActive bool) bool {
	if !cooldownActive {
		return true
	}
	return lastForDedup != nil && lastForDedup.Acked
}

// alertKey is the settings key holding the last-sent timestamp for a throttled
// alert kind, optionally scoped by dedup (e.g. per-destination).
func alertKey(kind, dedup string) string {
	if dedup != "" {
		return "alert.last." + kind + "." + dedup
	}
	return "alert.last." + kind
}

// alertSuppressed reports whether a throttled alert fired within its cooldown.
func (s *Server) alertSuppressed(key string, cooldown time.Duration) bool {
	raw, _ := s.store.GetSetting(key, "0")
	if raw == "0" {
		return false
	}
	last, err := strconv.ParseInt(raw, 10, 64)
	return err == nil && time.Since(time.Unix(last, 0)) < cooldown
}

// recordAlert stamps a throttled alert's last-sent time.
func (s *Server) recordAlert(key string) {
	_ = s.store.SetSetting(key, strconv.FormatInt(time.Now().Unix(), 10))
}

// alertStampRetention is how long a throttle timestamp is kept. Every cooldown
// is measured in hours, so a stamp older than this can only ever say "not
// suppressed" — the answer an absent row already gives.
const alertStampRetention = 30 * 24 * time.Hour

// pruneAlertStamps deletes throttle timestamps nothing can still act on. There
// is one per alert kind PER SCOPE — per destination, per container, per address
// — so the key space grows with the fleet and with every address that ever
// triggered an authentication alert.
func (s *Server) pruneAlertStamps() {
	keys, err := s.store.SettingKeysWithPrefix("alert.last.")
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-alertStampRetention).Unix()
	for _, k := range keys {
		raw, _ := s.store.GetSetting(k, "")
		at, perr := strconv.ParseInt(raw, 10, 64)
		if perr != nil || at < cutoff {
			_ = s.store.DeleteSetting(k)
		}
	}
}

// notifyThrottled dispatches an event only if its per-condition cooldown has
// elapsed since the last time, so a persistent condition doesn't spam. dedup
// scopes the cooldown (e.g. per-destination); empty means one global window for
// the kind.
func (s *Server) notifyThrottled(kind, dedup, title, message string, cooldown time.Duration) {
	key := alertKey(kind, dedup)
	cooldownActive := s.alertSuppressed(key, cooldown)

	// Persist to the Alerts inbox independently of the OUTBOUND cooldown (F46): a
	// still-firing condition that was acknowledged resurfaces, but doesn't stack up
	// duplicate unacked rows while it's already pending.
	last, _ := s.store.LastAlertForDedup(alertDedup(kind, dedup))
	if shouldStoreAlert(last, cooldownActive) {
		s.storeAlertRow(kind, dedup, title, message)
	}

	if cooldownActive {
		return // outbound send is still throttled
	}
	s.recordAlert(key)
	go s.notifier.Send(kind, title, message)
}

// engineNotify is wired into the backup engine. It applies throttling to the
// recurring offsite-missing warning; all other engine events pass straight
// through (they're one-shot: a specific backup succeeded/failed/verified).
func (s *Server) engineNotify(kind, title, message string) {
	if kind == notify.KindNoOffsite {
		s.notifyThrottled(kind, "", title, message, offsiteAlertCooldown)
		return
	}
	// Per-backup success is suppressed when the daily digest is set to REPLACE it
	// (success-mode "digest") — the digest carries the aggregated "it ran" signal
	// instead (F15). Failures and critical alerts are never gated here.
	if kind == notify.KindBackupSuccess && s.digestReplacesPerBackup() {
		return
	}
	s.notify(kind, title, message)
}

// digestReplacesPerBackup reports whether the daily digest is enabled AND set to
// replace per-backup success pings (F15). Best-effort: any config error falls back
// to sending per-backup pings, so a bad read never silences the success signal.
func (s *Server) digestReplacesPerBackup() bool {
	cfg, err := s.loadNotifyConfig()
	if err != nil {
		return false
	}
	return cfg.Digest.Enabled && cfg.Digest.SuccessMode == "digest"
}

// startAlertMonitor runs the slow, state-based alert checks (PLAN §9.15): a
// destination >90% full, and the master key not confirmed backed up. Event-based
// alerts (verify/scrub failure, missed schedule, no offsite copy) are emitted at
// their source. It ticks hourly so probing destinations never affects app
// responsiveness.
func (s *Server) startAlertMonitor() {
	go func() {
		// A short initial delay lets startup settle before the first probe.
		time.Sleep(2 * time.Minute)
		s.alertMonitorTick()
		t := time.NewTicker(alertMonitorInterval)
		defer t.Stop()
		for range t.C {
			s.alertMonitorTick()
		}
	}()
}

func (s *Server) alertMonitorTick() {
	s.checkKeyEscrowAlert()
	s.checkDestinationsFull()
	s.checkProxyTrustAlert()
}

// checkProxyTrustAlert warns while X-Forwarded-* is trusted from EVERY peer —
// proxy trust on with no allow-list. The condition is a boot-time
// misconfiguration, so it is reported the same way as the unescrowed key: it
// persists in the Alerts inbox and resurfaces once acknowledged while it is
// still true, because the operator believes the lockout is protecting them and
// it is not. The startup stderr WARNING alone is invisible on a NAS.
func (s *Server) checkProxyTrustAlert() {
	if !s.proxyTrustIsSpoofable() {
		return
	}
	s.notifyThrottled(notify.KindConfigTrustAllProxies, "",
		"Client IP addresses can be forged",
		"Proxy trust is on but no trusted proxies are listed, so DockBack believes the client address in any request's X-Forwarded-For header. "+
			"The login lockout, API token source restrictions and the IP column of the audit trail can all be bypassed by adding one header. "+
			"Set DOCKBACK_TRUSTED_PROXIES to your reverse proxy's source ranges and restart DockBack.",
		trustAllProxiesCooldown)
}

// checkKeyEscrowAlert warns (throttled) when the master key isn't confirmed
// backed up but backups exist — losing it would destroy every backup (PLAN
// §9.2/§9.15). Silent until there's something to lose.
func (s *Server) checkKeyEscrowAlert() {
	ack, _ := s.store.GetSetting("key.escrow_acknowledged", "false")
	if ack == "true" {
		return
	}
	if n, err := s.store.CountBackups(); err != nil || n == 0 {
		return
	}
	s.notifyThrottled(notify.KindKeyUnescrowed, "",
		"Encryption key not backed up",
		"Your DockBack master key isn't confirmed backed up, but backups exist. If this key is lost, EVERY backup becomes permanently unrecoverable. Save the key and confirm it in Settings → Encryption.",
		keyAlertCooldown)
}

// localPrimaryID / localPrimaryName identify the primary backups volume
// (s.engine.Storage, /app/backups) in the capacity-sampling + alert machinery (F42).
// It isn't a destination row, so it gets a stable synthetic id so throttling and
// destination_samples key off it exactly like a real destination.
const (
	localPrimaryID   = "local:primary"
	localPrimaryName = "Local backups volume"
)

// checkDestinationsFull probes each destination's capacity — and the primary
// backups volume (F42) — warning (throttled, per target) when it's over the
// threshold or forecast to fill (PLAN §9.15). Only targets that report a real
// total are evaluated (unlimited-quota providers are skipped).
func (s *Server) checkDestinationsFull() {
	if ds, err := s.store.ListDestinations(); err == nil {
		for _, d := range ds {
			if !d.Enabled {
				continue
			}
			cfg, err := s.decryptDestConfig(d)
			if err != nil {
				continue
			}
			b, err := storage.NewFromConfig(d.Type, cfg)
			if err != nil {
				continue
			}
			s.checkOneDestination(d.ID, d.Name, b)
		}
	}

	// The primary backups volume is where EVERY backup lands, yet it isn't a
	// destination row — so before F42 it was never capacity-sampled or alerted and
	// you'd only discover it was full when a backup failed its pre-flight. Check it
	// with the exact same path as any destination.
	if s.engine != nil && s.engine.Storage != nil {
		s.checkOneDestination(localPrimaryID, localPrimaryName, s.engine.Storage)
	}
}

// checkOneDestination reads one backend's capacity, records a trend sample, and
// raises the nearly-full / forecast alerts (throttled, keyed by id). Shared by
// real destinations and the primary volume so there is exactly one implementation.
func (s *Server) checkOneDestination(id, name string, b storage.Backend) {
	// Read capacity: total/free from a Capacity backend, else used-only from a
	// Usage backend (unlimited-quota providers). A single read per cycle feeds
	// both the >90%-full alert and the trend history for forecasting.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	var total, free, used uint64
	if capB, ok := b.(storage.Capacity); ok {
		if t, terr := capB.TotalBytes(ctx); terr == nil {
			total = t
		}
		if fr, ferr := capB.FreeBytes(ctx); ferr == nil {
			free = fr
		}
	}
	switch {
	case total > 0 && free <= total:
		used = total - free
	default:
		if u, ok := b.(storage.Usage); ok {
			if uv, uerr := u.UsedBytes(ctx); uerr == nil {
				used = uv
			}
		}
	}
	cancel()

	// Record a daily trend sample whenever we have any usable figure (PLAN §9.13).
	if total > 0 || used > 0 {
		_ = s.store.RecordDestSample(id, total, used, time.Now().Unix())
	}

	// Nearly-full alert (needs a real quota).
	nearlyFull := false
	if total > 0 && used <= total {
		frac := float64(used) / float64(total)
		if frac >= s.destFullThreshold() {
			nearlyFull = true
			s.notifyThrottled(notify.KindDestFull, id,
				fmt.Sprintf("Destination almost full: %s", name),
				fmt.Sprintf("%s is %.0f%% full (%s free of %s). Backups may soon fail — free space, raise the quota, or tighten retention.",
					name, frac*100, humanBytes(int64(free)), humanBytes(int64(total))),
				destFullCooldown)
		}
	}

	// Forecast alert (PLAN §9.13): trending to fill within the warning window,
	// but not already flagged as nearly full above (avoids a duplicate alert).
	if !nearlyFull {
		f := s.destForecast(id, total, free)
		if f.DaysToFull >= 0 && f.DaysToFull <= int64(s.forecastWarnDays()) {
			s.notifyThrottled(notify.KindDestForecast, id,
				fmt.Sprintf("Destination filling up: %s", name),
				fmt.Sprintf("%s is on track to run out of space in about %d day(s) (around %s) at the current growth of ~%s/month. Free space, raise the quota, or tighten retention before backups start failing.",
					name, f.DaysToFull, time.Unix(f.FillDate, 0).UTC().Format("2 Jan 2006"), humanBytes(f.GrowthPerMonth)),
				destForecastCooldown)
		}
	}
}
