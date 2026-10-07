package backup

import "strings"

// Pause behavior for application containers during volume capture (PLAN §4.2).
// Databases are NEVER paused — they're dumped live (§4.1) — so these apply only
// to non-DB containers with volumes.
const (
	PauseNone  = "none"  // live copy, no quiesce — fast, but a busy app may snapshot inconsistently
	PausePause = "pause" // docker pause/unpause for what changed during a live copy — a short freeze (default)
	PauseStop  = "stop"  // docker stop/start for what changed during a live copy — full quiesce, short downtime
)

func validPauseMode(m string) bool {
	switch m {
	case PauseNone, PausePause, PauseStop:
		return true
	}
	return false
}

// ValidPauseMode reports whether m is an accepted pause mode
// ("none"/"pause"/"stop"). Exported so the API layer can validate a per-run
// stack pause selection before passing it to the engine (PLAN §4.2).
func ValidPauseMode(m string) bool { return validPauseMode(m) }

// PauseModeKey is where a container's remembered pause behavior is stored
// (keyed by node+name), so scheduled and bulk backups honor it too.
func PauseModeKey(nodeID, name string) string { return "pause:" + nodeID + ":" + name }

// SavedBackupOptions are a container's remembered manual backup choices —
// compression, app-native export, and image bundling — so scheduled and
// whole-node runs reuse what the user set for a manual backup (F3). Pause mode
// and mount selection are remembered separately (PauseModeKey / mount settings).
type SavedBackupOptions struct {
	Compression string `json:"compression"`
	AppExport   bool   `json:"app_export"`
	SaveImage   bool   `json:"save_image"`
	// Incremental volume capture (F61): after a full baseline, capture only files
	// changed since the parent, forcing a fresh full every IncrementalFullEvery
	// backups. Off by default (byte-identical archives to before this feature).
	Incremental          bool `json:"incremental,omitempty"`
	IncrementalFullEvery int  `json:"incremental_full_every,omitempty"`
}

// incrementalPolicy resolves whether incremental volume capture is enabled for a
// container and how often a fresh full is forced, from its remembered backup
// options (F61). Mirrors resolvePauseMode: the engine reads the per-container
// setting so scheduled/whole-node runs honor the same choice. Off ⇒ (false, _).
func (e *Engine) incrementalPolicy(nodeID, name string) (enabled bool, fullEvery int) {
	var sb SavedBackupOptions
	if js, _ := e.Store.GetSetting(BackupOptionsKey(nodeID, name), ""); js != "" {
		_ = unmarshal(js, &sb)
	}
	return sb.Incremental, ClampFullEvery(sb.IncrementalFullEvery)
}

// BackupOptionsKey is where a container's remembered backup options are stored
// (keyed by node+name, matching PauseModeKey), so scheduled/bulk backups honor
// the same compression/export/save-image the user picked manually.
func BackupOptionsKey(nodeID, name string) string { return "backupopts:" + nodeID + ":" + name }

// shouldPause reports whether an app container should be quiesced (paused or
// stopped) during the volume copy. Only a non-database, currently-RUNNING
// container that has volume data is quiesced: a database is dumped live (§4.1),
// and a stopped container has no live writes to freeze — pausing it would just
// error (F9). PauseNone disables quiescing entirely.
func shouldPause(pm, engineKind string, running bool, volCount int) bool {
	return pm != PauseNone && engineKind == "" && running && volCount > 0
}

// resolvePauseMode picks the effective pause behavior for a run: an explicit
// per-run choice wins, then the legacy StopApp flag (→ stop), then the
// remembered per-container setting, then an image-aware default — a full STOP
// for embedded-SQLite apps (Pangolin), otherwise a brief PAUSE freeze during
// the volume copy for a consistent snapshot (PLAN §4.2). Databases are still
// never paused: the caller gates pausing on a non-DB engineKind, and a DB is
// dumped live because pausing it would break the logical dump (§4.1).
func (e *Engine) resolvePauseMode(opts Options, image, name string) string {
	mode, _ := e.pauseModeFor(opts, image, name)
	return mode
}

// pauseModeFor is resolvePauseMode, plus whether the OPERATOR chose this mode or
// it came from a default.
//
// The difference matters to #3: a marker-detected database that nobody knows is
// a database gets its copy window quiesced automatically, but an operator who
// has explicitly said how this container should be handled has already made that
// call, and a backup tool that overrides an explicit instruction is worse than
// one that leaves a warning.
func (e *Engine) pauseModeFor(opts Options, image, name string) (mode string, byOperator bool) {
	if opts.PauseMode != "" && validPauseMode(opts.PauseMode) {
		return opts.PauseMode, true
	}
	if opts.StopApp {
		return PauseStop, true
	}
	// A remembered per-container choice wins over the image default (so a user who
	// explicitly picks "pause"/"none" for Pangolin is respected). Empty sentinel
	// distinguishes "no choice stored" from an explicit selection.
	if v, _ := e.Store.GetSetting(PauseModeKey(opts.NodeID, name), ""); validPauseMode(v) {
		return v, true
	}
	// F145: an application-declared default. The shipped default — a brief pause
	// around the volume copy — is the right trade for an app whose files could be
	// written mid-copy. It is the wrong one for an ingress proxy, where the pause
	// stalls every request to every service behind it and buys nothing, because
	// the only thing in there that could tear is captured by a consistent online
	// snapshot that does not need the container held still.
	if p := ProfileFor(image); p != nil && validPauseMode(p.DefaultPause) {
		return p.DefaultPause, false
	}
	if prefersStopByDefault(image) {
		return PauseStop, false
	}
	return PausePause, false
}

// AppPauseDefault returns the quiesce mode an application declares for itself
// and the reason for it (F145), or ("", "") for an image that declares none —
// which is nearly all of them.
//
// Exported so the container's backup settings can SHOW the default rather than
// silently applying it. A default that differs from the shipped one is a
// decision, and an operator who disagrees can still choose any mode; a decision
// made invisibly is one they cannot disagree with.
func AppPauseDefault(image string) (mode, why string) {
	if p := ProfileFor(image); p != nil && validPauseMode(p.DefaultPause) {
		return p.DefaultPause, p.DefaultPauseWhy
	}
	// The image-aware stop default predates the profile registry and is the same
	// kind of fact, so it is reported through the same door — otherwise the
	// settings page shows "pause" for a container the engine will stop.
	if prefersStopByDefault(image) {
		return PauseStop, "its entire state lives in an embedded SQLite database, and a clean shutdown gives a pristine copy rather than one SQLite has to recover on next open"
	}
	return "", ""
}

// prefersStopByDefault reports whether an image should default to a full STOP
// quiesce (rather than a brief pause) when the user hasn't chosen a mode. Apps
// whose entire state lives in an embedded SQLite database — e.g. Pangolin — are
// safest with a clean shutdown: a paused copy is only crash-consistent (SQLite
// replays its write-ahead log on the next open), while a stop removes even that
// recovery step for a pristine, guaranteed-consistent snapshot. The extra cost
// is a few seconds of downtime during the volume copy, which the user can opt
// out of per container (choose "pause"/"none").
func prefersStopByDefault(image string) bool {
	return isPangolin(image)
}

// isPangolin recognizes the Pangolin control-plane image (fossorial/pangolin),
// which keeps its sites, resources, WireGuard keys and target mappings in an
// embedded SQLite database rather than a separate database container.
func isPangolin(image string) bool {
	return strings.Contains(strings.ToLower(image), "pangolin")
}
