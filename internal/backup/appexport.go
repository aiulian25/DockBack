package backup

import (
	"encoding/json"
	"strings"
)

// ExportProfile defines how to produce and re-import an application-native,
// version-independent export (PLAN §9.4 / Phase 4).
type ExportProfile struct {
	Tool      string   `json:"tool"`
	Dir       string   `json:"dir"`        // export directory inside the container
	ExportCmd []string `json:"export_cmd"` // produces the export into Dir
	ImportCmd []string `json:"import_cmd"` // imports the export from Dir on restore
	User      string   `json:"user"`       // optional run-as user
	Available bool     `json:"available"`  // a profile (auto or custom) exists

	// VerifyCmd runs INSIDE the container after a successful import, and a
	// non-zero exit FAILS the restore (F152).
	//
	// An app-native restore is the one path with no completeness contract to
	// check: there is no dump to tally and no volume checksum, only "the
	// importer exited 0". Many applications ship a checker that exits non-zero
	// when their data is inconsistent, and where one exists this turns the
	// importer's word into something the application itself confirms.
	//
	// Optional, and deliberately unset for images whose checker always exits 0 —
	// wiring one of those in would create a check that cannot fail, which is
	// worse than no check at all because it reads like protection.
	VerifyCmd []string `json:"verify_cmd,omitempty"`
}

// SavedExportProfile is the per-container custom export profile (settings).
type SavedExportProfile struct {
	Dir       string `json:"dir"`
	ExportCmd string `json:"export_cmd"`           // shell line
	ImportCmd string `json:"import_cmd"`           // shell line
	VerifyCmd string `json:"verify_cmd,omitempty"` // shell line, run after a successful import (F152)
	User      string `json:"user"`
}

// ExportProfileKey is where a container's custom export profile is stored.
func ExportProfileKey(nodeID, name string) string { return "export:" + nodeID + ":" + name }

// Saved converts an effective ExportProfile into a portable SavedExportProfile
// (shell lines) so a working profile can be shared/imported onto another container
// (F32). A command already wrapped as `sh -c <line>` is unwrapped to its line; a
// preset's bare argv is joined with spaces as an editable starting point.
func (p ExportProfile) Saved() SavedExportProfile {
	return SavedExportProfile{
		Dir:       p.Dir,
		ExportCmd: cmdToLine(p.ExportCmd),
		ImportCmd: cmdToLine(p.ImportCmd),
		VerifyCmd: cmdToLine(p.VerifyCmd),
		User:      p.User,
	}
}

// cmdToLine renders an argv back into a single shell line for SavedExportProfile:
// the inner line of a `/bin/sh -c <line>` wrap, else the argv space-joined.
func cmdToLine(cmd []string) string {
	if len(cmd) == 3 && cmd[0] == "/bin/sh" && cmd[1] == "-c" {
		return cmd[2]
	}
	return strings.Join(cmd, " ")
}

// ExportProfileFor exposes the effective export profile for the API/UI.
func (e *Engine) ExportProfileFor(image, nodeID, name string) ExportProfile {
	return e.exportProfile(image, nodeID, name)
}

// exportProfile returns the export profile for a container: a built-in preset
// for known images (Paperless), overridden by any saved custom profile.
func (e *Engine) exportProfile(image, nodeID, name string) ExportProfile {
	// F101: the operator's own preset library is consulted BEFORE the built-in
	// table, so a recipe they worked out for their image beats our conservative
	// starting point. A container's own saved profile still wins over both.
	p := builtinExportProfile(image)
	if preset, ok := MatchExportPreset(e.savedExportPresets(), image); ok {
		p = preset.Profile()
	}

	// A saved custom profile turns an admin-authored line into `sh -c` run inside
	// the container with its privileges (operator-level ACE) — by design, not an
	// external vector: the setter endpoint is auth+CSRF-gated (SEC-7, documented).
	if js, _ := e.Store.GetSetting(ExportProfileKey(nodeID, name), ""); js != "" {
		var sp SavedExportProfile
		if json.Unmarshal([]byte(js), &sp) == nil {
			if sp.Dir != "" {
				p.Dir = sp.Dir
			}
			if strings.TrimSpace(sp.ExportCmd) != "" {
				p.ExportCmd = []string{"/bin/sh", "-c", sp.ExportCmd}
			}
			if strings.TrimSpace(sp.ImportCmd) != "" {
				p.ImportCmd = []string{"/bin/sh", "-c", sp.ImportCmd}
			}
			if strings.TrimSpace(sp.VerifyCmd) != "" {
				p.VerifyCmd = []string{"/bin/sh", "-c", sp.VerifyCmd}
			}
			if sp.User != "" {
				p.User = sp.User
			}
			if p.Tool == "" {
				p.Tool = "custom"
			}
		}
	}
	p.Available = p.Dir != "" && len(p.ExportCmd) > 0 && len(p.ImportCmd) > 0
	return p
}

// exportPreset is a built-in app-native export/import recipe keyed by a
// lowercase image substring. These are CONSERVATIVE STARTING POINTS: app-native
// export is opt-in per backup, a saved custom profile overrides the preset, and
// the profile is editable in the UI. Ordered most-specific first.
//
// Note on restore: enabling app-native export REPLACES raw volume/DB capture, so
// a preset is fully one-click-restorable only when the app ships a symmetric
// importer that runs against the live app (Paperless). For export-only tools
// (Gitea/Forgejo `dump`) the portable, complete archive is captured, but restore
// is not one-shot — the import command surfaces clear guidance rather than
// silently doing nothing (documented in Docs → App-native exports).
type exportPreset struct {
	match   string
	profile ExportProfile
}

// gitDumpDir is a scratch dir inside a Gitea/Forgejo container to hold the dump.
const gitDumpDir = "/tmp/dockback-export"

// gitDumpExport builds the shell line that writes a complete `<bin> dump` archive
// (database + repos + config + attachments) into gitDumpDir.
func gitDumpExport(bin string) string {
	return "set -e; rm -rf " + gitDumpDir + "; mkdir -p " + gitDumpDir +
		"; " + bin + " dump -c /data/gitea/conf/app.ini -t /tmp -f " + gitDumpDir + "/" + bin + "-dump.zip"
}

// gitDumpImportGuide fails LOUDLY with recovery guidance: Gitea/Forgejo have no
// one-shot restore, so we never pretend the import succeeded (which would be a
// silent data-loss trap). The complete dump is present for a manual/cross-version
// restore per the app's official Backup-and-Restore docs.
func gitDumpImportGuide(bin string) string {
	return "echo 'This backup holds a complete portable " + bin + " dump (" + bin + "-dump.zip) in " + gitDumpDir +
		". " + bin + " has no one-shot restore: stop the server, then load the database and unpack the custom/, data/ and repos/ folders from the dump per the official Backup-and-Restore docs.' >&2; exit 1"
}

func gitPreset(bin string) ExportProfile {
	return ExportProfile{
		Tool:      bin,
		User:      "git",
		Dir:       gitDumpDir,
		ExportCmd: []string{"/bin/sh", "-c", gitDumpExport(bin)},
		ImportCmd: []string{"/bin/sh", "-c", gitDumpImportGuide(bin)},
	}
}

var exportPresets = []exportPreset{
	// Paperless-ngx — symmetric, live-importable: fully one-click-restorable.
	//
	// Both commands were re-checked against the current image: `document_exporter
	// <target> --no-progress-bar` and `document_importer <source>
	// --no-progress-bar` are still exactly right, so the preset is unchanged.
	//
	// NO VerifyCmd, and that is a decision rather than an omission (F152).
	// `document_sanity_checker` is the obvious candidate and it ALWAYS EXITS 0 —
	// it renders a table of findings and returns. Wiring it in would produce a
	// check that can never fail while reading, in the log and in the docs, like
	// proof that the import was sound. A check that cannot fail is worse than no
	// check, so the sanity checker is documented as a manual step instead.
	{match: "paperless", profile: ExportProfile{
		Tool:      "paperless",
		Dir:       "/usr/src/paperless/export",
		ExportCmd: []string{"document_exporter", "/usr/src/paperless/export", "--no-progress-bar"},
		ImportCmd: []string{"document_importer", "/usr/src/paperless/export", "--no-progress-bar"},
	}},
	// Forgejo checked before Gitea (Forgejo images don't contain "gitea", but keep
	// the specific fork first for clarity). Export-only: portable dump, guided restore.
	{match: "forgejo", profile: gitPreset("forgejo")},
	{match: "gitea", profile: gitPreset("gitea")},
}

// builtinExportProfile returns a known preset for an image, or an empty profile.
func builtinExportProfile(image string) ExportProfile {
	img := strings.ToLower(image)
	for _, p := range exportPresets {
		if strings.Contains(img, p.match) {
			return p.profile
		}
	}
	return ExportProfile{}
}
