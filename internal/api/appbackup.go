package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/robfig/cron/v3"

	"dockback/internal/appbackup"
	"dockback/internal/notify"
	"dockback/internal/version"
)

// maxAppRestoreBytes bounds an uploaded application-restore archive (the config
// DB is small; this is a generous DoS guard).
const maxAppRestoreBytes = 1 << 30 // 1 GiB

// appBackupDir is where stored application backups live (a dedicated subdir of
// the backups volume, kept apart from container backups).
func (s *Server) appBackupDir() string {
	// Unreachable in production (config defaults BackupsDir to /app/backups). An
	// empty value means a test built a Server without one, and joining it would
	// silently write real archives into the package's working directory.
	if s.cfg.BackupsDir == "" {
		panic("appBackupDir: cfg.BackupsDir is unset")
	}
	return filepath.Join(s.cfg.BackupsDir, "_app")
}

// handleAppBackupInfo reports what the UI needs to render the card header: the
// master-key fingerprint (so the user can confirm the key a restore needs) and
// the current database size.
func (s *Server) handleAppBackupInfo(w http.ResponseWriter, r *http.Request) {
	var dbBytes int64
	if fi, err := os.Stat(filepath.Join(s.cfg.DataDir, "dockback.db")); err == nil {
		dbBytes = fi.Size()
	}
	// F58: integrity-drill status ("last proven").
	lastAtStr, _ := s.store.GetSetting("appbackup.last_drill_at", "0")
	lastAt, _ := strconv.ParseInt(lastAtStr, 10, 64)
	lastOk, _ := s.store.GetSetting("appbackup.last_drill_ok", "")
	lastDetail, _ := s.store.GetSetting("appbackup.last_drill_detail", "")
	writeJSON(w, http.StatusOK, map[string]any{
		"key_fingerprint":     s.engine.MasterKeyFP(),
		"db_bytes":            dbBytes,
		"last_drill_at":       lastAt,
		"last_drill_ok":       lastOk == "true",
		"last_drill_detail":   lastDetail,
		"drill_interval_days": s.appBackupDrillIntervalDays(),
	})
}

// appBackupDrillIntervalDays is the configured app-backup drill cadence in days
// (setting appbackup.drill_interval_days, default 7, 0 = off — F58).
func (s *Server) appBackupDrillIntervalDays() int {
	v, _ := s.store.GetSetting("appbackup.drill_interval_days", "7")
	n, _ := strconv.Atoi(v)
	if n < 0 {
		n = 0
	}
	return n
}

// appBackupDrillTick runs the periodic app-backup integrity drill (F58) when it's
// overdue. Invoked from the app-backup scheduler ticker, so it needs no new loop.
func (s *Server) appBackupDrillTick() {
	days := s.appBackupDrillIntervalDays()
	if days <= 0 {
		return // drills disabled
	}
	lastStr, _ := s.store.GetSetting("appbackup.last_drill_at", "0")
	last, _ := strconv.ParseInt(lastStr, 10, 64)
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix()
	if last > cutoff {
		return // not yet due
	}
	s.runAppBackupDrill()
}

// runAppBackupDrill decrypts + integrity-checks the NEWEST stored app-backup and
// records the outcome (F58). Shared by the periodic tick and the manual endpoint.
// Returns pass/fail + a short detail. On a genuine failure it alerts with the same
// severity as a scrub regression, naming the file.
func (s *Server) runAppBackupDrill() (ok bool, detail string) {
	list, err := appbackup.List(s.appBackupDir())
	if err != nil {
		return false, "could not list application backups: " + err.Error()
	}
	if len(list) == 0 {
		return false, "no application backup to test yet — create one first"
	}
	newest := list[0].File // List is newest-first
	verr := appbackup.VerifyFile(s.appBackupDir(), newest, s.cfg.EncryptionKey, s.cfg.TmpDir)
	now := time.Now().Unix()
	_ = s.store.SetSetting("appbackup.last_drill_at", strconv.FormatInt(now, 10))
	if verr != nil {
		_ = s.store.SetSetting("appbackup.last_drill_ok", "false")
		_ = s.store.SetSetting("appbackup.last_drill_detail", verr.Error())
		s.logSink("app-backup", "ERR", "App-backup drill FAILED for "+newest+": "+verr.Error())
		s.notify(notify.KindScrubFailed, "App-backup drill FAILED",
			fmt.Sprintf("The application (control-plane) backup %s no longer restores cleanly: %s. This is the backup that rescues every other backup's catalog — create a fresh app-backup and investigate.", newest, verr.Error()))
		return false, verr.Error()
	}
	_ = s.store.SetSetting("appbackup.last_drill_ok", "true")
	_ = s.store.SetSetting("appbackup.last_drill_detail", "")
	s.logSink("app-backup", "INFO", "App-backup drill passed — "+newest+" restores cleanly")
	return true, ""
}

// handleAppBackupDrill runs the integrity drill on demand and returns the outcome
// synchronously, bounded to two minutes (F58).
func (s *Server) handleAppBackupDrill(w http.ResponseWriter, r *http.Request) {
	_ = s.store.Audit(userFrom(r), "app.backup.drill", "", "")
	type result struct {
		ok     bool
		detail string
	}
	ch := make(chan result, 1)
	go func() { ok, d := s.runAppBackupDrill(); ch <- result{ok, d} }()
	select {
	case res := <-ch:
		writeJSON(w, http.StatusOK, map[string]any{"ok": res.ok, "detail": res.detail, "last_drill_at": time.Now().Unix()})
	case <-time.After(2 * time.Minute):
		errJSON(w, http.StatusGatewayTimeout, "app-backup drill timed out")
	}
}

// handleAppBackupList returns the stored application backups, newest first.
func (s *Server) handleAppBackupList(w http.ResponseWriter, r *http.Request) {
	list, err := appbackup.List(s.appBackupDir())
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "could not list backups")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// snapshotMeta takes a consistent DB snapshot to dst and gathers the
// non-sensitive counts shown in the catalog.
func (s *Server) snapshotMeta(dst string) (appbackup.Entry, error) {
	e := appbackup.Entry{
		CreatedAt:      time.Now().Unix(),
		AppVersion:     version.Version,
		KeyFingerprint: s.engine.MasterKeyFP(),
	}
	if err := s.store.SnapshotTo(dst); err != nil {
		return e, err
	}
	if ns, err := s.store.ListNodes(); err == nil {
		e.Nodes = len(ns)
	}
	if n, err := s.store.CountBackups(); err == nil {
		e.Backups = n
	}
	if ds, err := s.store.ListDestinations(); err == nil {
		e.Destinations = len(ds)
	}
	return e, nil
}

// createAppBackup snapshots the app's state into a new stored, encrypted backup
// on the backups volume and audits it (PLAN §6.5/§9.3). Shared by the manual
// "Create backup" handler and the automatic schedule (F4), so both take an
// identical, verified snapshot. actor is the audit attribution (a username, or
// "scheduler" for an automatic run).
func (s *Server) createAppBackup(actor string) (appbackup.Entry, error) {
	// Read before the snapshot: a change landing during it is newer than the
	// mark, so it triggers the next backup instead of being assumed covered.
	covered, _ := s.store.MaxAuditID()
	snap := filepath.Join(s.cfg.TmpDir, fmt.Sprintf("cfgsnap-%d.db", time.Now().UnixNano()))
	defer os.Remove(snap)
	meta, err := s.snapshotMeta(snap)
	if err != nil {
		return appbackup.Entry{}, fmt.Errorf("snapshot configuration: %w", err)
	}
	entry, err := appbackup.CreateFile(s.appBackupDir(), snap, s.cfg.EncryptionKey, meta)
	if err != nil {
		return appbackup.Entry{}, fmt.Errorf("write backup: %w", err)
	}
	_ = s.store.Audit(actor, "app.backup.create", entry.File, "")
	s.setAppBackupConfigMark(covered)
	return entry, nil
}

// handleAppBackupCreate snapshots the app's state into a new stored, encrypted
// backup on the backups volume (PLAN §6.5/§9.3).
func (s *Server) handleAppBackupCreate(w http.ResponseWriter, r *http.Request) {
	entry, err := s.createAppBackup(userFrom(r))
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "could not create backup")
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// handleAppBackupDownload streams a stored backup (or, with no ?file, creates and
// streams a fresh one without storing it). The archive is AES-256-GCM encrypted
// with the master key, so it's opaque without it.
func (s *Server) handleAppBackupDownload(w http.ResponseWriter, r *http.Request) {
	// F199: step-up gated like the backup exports.
	//
	// Weaker in kind than those two — this archive is AES-256-GCM encrypted with
	// the master key, so on its own it opens nothing. It is still every sealed
	// destination credential, node SSH key and 2FA secret the app holds, in one
	// file, and an attacker who takes it now only needs the key later. A password
	// is a small price for the one download that is worth keeping forever.
	if !s.requireExportTicket(w, r, exportPurposeAppBackup, exportPurposeAppBackup) {
		return
	}
	name := r.URL.Query().Get("file")
	if name != "" {
		if !appbackup.ValidName(name) {
			errJSON(w, http.StatusBadRequest, "invalid backup name")
			return
		}
		rc, err := appbackup.OpenFile(s.appBackupDir(), name)
		if err != nil {
			errJSON(w, http.StatusNotFound, "backup not found")
			return
		}
		defer rc.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
		if _, err := io.Copy(w, rc); err != nil {
			s.logSink("app-backup", "ERR", "download failed: "+err.Error())
		}
		_ = s.store.Audit(userFrom(r), "app.backup.download", name, "")
		return
	}

	// No file given: generate an ephemeral archive and stream it.
	snap := filepath.Join(s.cfg.TmpDir, fmt.Sprintf("cfgsnap-%d.db", time.Now().UnixNano()))
	defer os.Remove(snap)
	if err := s.store.SnapshotTo(snap); err != nil {
		errJSON(w, http.StatusInternalServerError, "could not snapshot configuration")
		return
	}
	fname := fmt.Sprintf("dockback-config-%s.dback", time.Now().UTC().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fname))
	m := appbackup.Manifest{Format: appbackup.Format, Version: appbackup.Version, CreatedAt: time.Now().Unix(), AppVersion: version.Version, KeyFingerprint: s.engine.MasterKeyFP()}
	if err := appbackup.Create(w, snap, s.cfg.EncryptionKey, m); err != nil {
		s.logSink("app-backup", "ERR", "application backup failed: "+err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "app.backup.download", "", "")
}

// handleAppBackupDelete removes a stored backup.
func (s *Server) handleAppBackupDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if !appbackup.ValidName(name) {
		errJSON(w, http.StatusBadRequest, "invalid backup name")
		return
	}
	if err := appbackup.DeleteFile(s.appBackupDir(), name); err != nil {
		errJSON(w, http.StatusInternalServerError, "could not delete backup")
		return
	}
	_ = s.store.Audit(userFrom(r), "app.backup.delete", name, "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleAppBackupRestoreLocal restores from a stored backup, then restarts.
func (s *Server) handleAppBackupRestoreLocal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		File string `json:"file"`
		stepUpBody
	}
	if err := readJSON(r, &req); err != nil || !appbackup.ValidName(req.File) {
		errJSON(w, http.StatusBadRequest, "invalid backup name")
		return
	}
	// F199: DOWNLOADING this archive already demands the password. Restoring it
	// is the more destructive half of the same pair — it replaces every node
	// credential, the catalog and the admin account, then restarts the app — so a
	// hijacked session must not be able to do it silently. Checked after the name
	// is validated, so a malformed request never costs a prompt.
	if !s.requireFreshAuth(w, r, req.stepUpBody) {
		return
	}
	m, err := appbackup.RestoreFile(s.appBackupDir(), req.File, s.cfg.EncryptionKey, s.cfg.DataDir, s.cfg.TmpDir)
	if err != nil {
		_ = s.store.Audit(userFrom(r), "app.restore.failed", req.File, "ip="+s.clientIP(r)+" err="+err.Error())
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	s.finishRestore(w, r, req.File, m.CreatedAt, m.KeyFingerprint)
}

// appRestoreFormMemory is how much of an uploaded restore is held in RAM while
// the form is parsed; the rest spills to a temp file. The whole upload is
// already capped by maxAppRestoreBytes.
const appRestoreFormMemory = 8 << 20 // 8 MiB

// handleAppBackupRestore restores from an UPLOADED archive (an external file the
// user kept), validates it, stages it, and restarts.
func (s *Server) handleAppBackupRestore(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAppRestoreBytes)
	// Parse the form up front so the credentials can be read wherever the client
	// placed them, then re-authenticate BEFORE the archive is opened, decrypted
	// or staged. F199: same gate as the stored-backup restore.
	if err := r.ParseMultipartForm(appRestoreFormMemory); err != nil {
		errJSON(w, http.StatusBadRequest, "no backup file uploaded")
		return
	}
	if !s.requireFreshAuth(w, r, stepUpBody{Password: r.FormValue("password"), Code: r.FormValue("code")}) {
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		errJSON(w, http.StatusBadRequest, "no backup file uploaded")
		return
	}
	defer f.Close()

	m, err := appbackup.Restore(f, s.cfg.EncryptionKey, s.cfg.DataDir, s.cfg.TmpDir)
	if err != nil {
		_ = s.store.Audit(userFrom(r), "app.restore.failed", "", "ip="+s.clientIP(r)+" err="+err.Error())
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	s.finishRestore(w, r, "(uploaded)", m.CreatedAt, m.KeyFingerprint)
}

// ---------------- Automatic application backup schedule (F4) ----------------

// appBackupSchedule is the automatic control-plane backup schedule. It reuses
// the container Schedule's frequency machinery (cronSpec/nextRun/scheduleDue),
// plus a local retention count and an offsite-push toggle. Custom cron is
// deliberately omitted — the control plane doesn't need per-minute granularity.
type appBackupSchedule struct {
	Enabled      bool   `json:"enabled"`
	Kind         string `json:"kind"`     // daily | weekly | monthly
	Time         string `json:"time"`     // "HH:MM" local
	Weekday      int    `json:"weekday"`  // 0=Sun (weekly)
	Monthday     int    `json:"monthday"` // 1-28 (monthly)
	Keep         int    `json:"keep"`     // newest N kept locally (0 = keep all)
	PushExternal bool   `json:"push_external"`
}

const appBackupScheduleKey = "appbackup.schedule"
const appBackupLastRunKey = "appbackup.schedule.last_run"
const appBackupTickInterval = 30 * time.Second

// toSchedule adapts the app-backup schedule to the shared Schedule type so the
// existing cronSpec/nextRun/scheduleDue apply unchanged.
func (a appBackupSchedule) toSchedule() Schedule {
	return Schedule{Enabled: a.Enabled, Kind: a.Kind, Time: a.Time, Weekday: a.Weekday, Monthday: a.Monthday}
}

// loadAppBackupSchedule reads the schedule (JSON blob). With none stored, the
// default is ON: weekly, keeping 7, pushed to an off-machine destination when
// one is set. It used to be off, and in the 2026-10-04 recovery DockBack's own
// newest backup was five weeks old while the folder holding its key was gone.
// A schedule the operator saved, including one switched off, is kept as saved.
func (s *Server) loadAppBackupSchedule() appBackupSchedule {
	a := appBackupSchedule{Enabled: true, Kind: "weekly", Time: "04:00", Weekday: 0, Monthday: 1, Keep: 7, PushExternal: true}
	if js, _ := s.store.GetSetting(appBackupScheduleKey, ""); js != "" {
		_ = json.Unmarshal([]byte(js), &a)
	}
	if a.Kind == "" {
		a.Kind = "weekly"
	}
	if a.Time == "" {
		a.Time = "04:00"
	}
	return a
}

// startAppBackupSchedule launches the automatic application-backup loop (F4),
// mirroring startScheduler: a 30s tick that fires when the next scheduled time
// has passed since the last run, never backfilling a missed window on first
// enable.
func (s *Server) startAppBackupSchedule() {
	go func() {
		t := time.NewTicker(appBackupTickInterval)
		defer t.Stop()
		for range t.C {
			s.appBackupTick(time.Now())
			s.appBackupDrillTick() // F58: periodic integrity drill of the newest app-backup
		}
	}()
}

func (s *Server) appBackupTick(now time.Time) {
	a := s.loadAppBackupSchedule()
	if !a.Enabled {
		return
	}
	lrStr, _ := s.store.GetSetting(appBackupLastRunKey, "0")
	lastRun, _ := strconv.ParseInt(lrStr, 10, 64)
	if lastRun == 0 {
		// First activation: one backup now, so an install that has never had one
		// is covered today rather than at the next weekly window.
		_ = s.store.SetSetting(appBackupLastRunKey, strconv.FormatInt(now.Unix(), 10))
		s.runAppBackup(a)
		return
	}
	fire, _, _ := scheduleDue(a.toSchedule(), time.Unix(lastRun, 0), now, scheduleMissThreshold)
	if !fire {
		s.appBackupAfterConfigChange(a, now)
		return
	}
	// Catch up at most one window (no stampede): advance last_run to now.
	_ = s.store.SetSetting(appBackupLastRunKey, strconv.FormatInt(now.Unix(), 10))
	s.runAppBackup(a)
}

// runAppBackup creates a stored application backup, prunes to the newest Keep,
// and optionally pushes it to the app external destinations (F4). Everything is
// logged to the operational stream so a failure is visible.
func (s *Server) runAppBackup(a appBackupSchedule) {
	entry, err := s.createAppBackup("scheduler")
	if err != nil {
		s.logSink("app-backup", "ERR", "Scheduled application backup failed: "+err.Error())
		s.notify(notify.KindAppBackupFailed, "DockBack could not back itself up",
			"The scheduled application backup failed: "+err.Error()+". This is the backup that brings back DockBack's catalog, settings and nodes if its own data is lost.")
		return
	}
	s.logSink("app-backup", "INFO", fmt.Sprintf("Scheduled application backup created: %s", entry.File))

	if a.Keep > 0 {
		if pruned := s.pruneAppBackups(a.Keep); pruned > 0 {
			s.logSink("app-backup", "INFO", fmt.Sprintf("Pruned %d old application backup(s), keeping the newest %d", pruned, a.Keep))
		}
	}

	if a.PushExternal {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		fname, pushed, failed, perr := s.appExternalBackupCore(ctx)
		switch {
		case perr == errNoEnabledAppDest:
			s.logSink("app-backup", "INFO", "No off-machine destination is set for the application backup, so it stays on this machine — add one under Settings → Advanced → Application Backup & Restore → External destinations")
		case perr != nil:
			s.logSink("app-backup", "ERR", "Scheduled external push failed: "+perr.Error())
			s.notify(notify.KindAppBackupFailed, "DockBack's own backup did not leave this machine",
				"The application backup was made, but pushing it off this machine failed: "+perr.Error()+". Until a push succeeds, losing this machine loses DockBack's catalog with it.")
		case len(failed) > 0:
			s.logSink("app-backup", "WARN", fmt.Sprintf("Pushed %s to %d external destination(s), %d failed", fname, len(pushed), len(failed)))
			s.notify(notify.KindAppBackupFailed, "DockBack's own backup missed a destination",
				fmt.Sprintf("The application backup reached %d destination(s) but not %d: %s.", len(pushed), len(failed), failedDestinations(failed)))
		default:
			s.logSink("app-backup", "INFO", fmt.Sprintf("Pushed %s to %d external destination(s)%s", fname, len(pushed), func() string {
				if len(failed) > 0 {
					return fmt.Sprintf(" (%d failed)", len(failed))
				}
				return ""
			}()))
		}
	}
}

// pruneAppBackups deletes stored application backups beyond the newest keep,
// returning how many were removed. keep<=0 keeps everything.
func (s *Server) pruneAppBackups(keep int) int {
	if keep <= 0 {
		return 0
	}
	list, err := appbackup.List(s.appBackupDir())
	if err != nil {
		return 0
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt > list[j].CreatedAt })
	deleted := 0
	for i := keep; i < len(list); i++ {
		if err := appbackup.DeleteFile(s.appBackupDir(), list[i].File); err == nil {
			deleted++
			_ = s.store.Audit("scheduler", "app.backup.delete", list[i].File, "retention")
		}
	}
	return deleted
}

// handleGetAppBackupSchedule returns the automatic schedule + its next fire time.
func (s *Server) handleGetAppBackupSchedule(w http.ResponseWriter, r *http.Request) {
	a := s.loadAppBackupSchedule()
	var next int64
	if a.Enabled {
		if t := a.toSchedule().nextRun(time.Now()); !t.IsZero() {
			next = t.Unix()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedule": a, "next_run": next})
}

// handleSetAppBackupSchedule validates and saves the automatic schedule.
func (s *Server) handleSetAppBackupSchedule(w http.ResponseWriter, r *http.Request) {
	var a appBackupSchedule
	if err := readJSON(r, &a); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	switch a.Kind {
	case "daily", "weekly", "monthly":
	default:
		errJSON(w, http.StatusBadRequest, "kind must be daily, weekly or monthly")
		return
	}
	if a.Weekday < 0 || a.Weekday > 6 {
		a.Weekday = 0
	}
	if a.Monthday < 1 || a.Monthday > 28 {
		a.Monthday = 1
	}
	if a.Keep < 0 {
		a.Keep = 0
	}
	// Validate the derived cron so an enabled schedule is always schedulable.
	if a.Enabled {
		if spec, err := a.toSchedule().cronSpec(); err != nil {
			errJSON(w, http.StatusBadRequest, err.Error())
			return
		} else if _, err := cron.ParseStandard(spec); err != nil {
			errJSON(w, http.StatusBadRequest, "invalid schedule: "+err.Error())
			return
		}
	}
	js, _ := json.Marshal(a)
	if err := s.store.SetSetting(appBackupScheduleKey, string(js)); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "appbackup.schedule.update", "", fmt.Sprintf("enabled=%v kind=%s keep=%d push=%v", a.Enabled, a.Kind, a.Keep, a.PushExternal))
	s.handleGetAppBackupSchedule(w, r)
}

// finishRestore audits, responds, and schedules the restart that applies the
// staged DB on startup. Running backups are interrupted and reconciled to
// "failed" on restart, as with any restart.
func (s *Server) finishRestore(w http.ResponseWriter, r *http.Request, src string, createdAt int64, keyFP string) {
	_ = s.store.Audit(userFrom(r), "app.restore.staged", src,
		fmt.Sprintf("ip=%s backup_created=%d key_fp=%s", s.clientIP(r), createdAt, keyFP))
	// F223: leave a note for the instance that comes back. A settings row would
	// be pointless — the database it lives in is about to be replaced by the
	// restored one — so this is a file beside the staged DB, for the same reason
	// the staged DB is a file.
	s.markWizardAppRestored()
	writeJSON(w, http.StatusOK, map[string]any{"status": "restarting", "backup_created": createdAt})
	go func() {
		time.Sleep(750 * time.Millisecond)
		s.requestRestart()
	}()
}
