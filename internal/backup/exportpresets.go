package backup

import (
	"encoding/json"
	"strings"
)

// Reusable app-native export presets (F101).
//
// The generic mechanism already existed: any container can carry a custom
// export/import command pair. What it was not is REUSABLE — an operator who
// worked out the right `vaultwarden` or `nextcloud occ` invocation had to retype
// it for every container and could not share it. This promotes that per-container
// recipe to a named, fleet-level preset.
//
// Precedence, most specific first:
//
//  1. the container's own saved profile (an explicit choice about THIS container)
//  2. a saved preset whose match hits the image (the operator's own library)
//  3. the built-in table (paperless / forgejo / gitea)
//
// A preset before a built-in is deliberate: the built-ins are conservative
// starting points, and an operator who has worked out something better for their
// own image should not have to fight them.
//
// SAFETY, unchanged by this feature: an export profile only ever runs when
// app-native export is enabled FOR THAT BACKUP (`opts.AppExport`). A preset
// matching an image does not, on its own, cause anything to execute.

// ExportPresetsKey is the setting holding the whole preset library as one JSON
// blob, mirroring how the autosnap set is stored — presets are read on every
// profile lookup, so one small read beats a per-preset settings scan.
const ExportPresetsKey = "appexport.presets"

// ExportPreset is one named, reusable export recipe.
//
// Match is a lowercase substring tested against the image reference — the same
// rule the built-in table uses, so an operator's mental model transfers. Empty
// Match means the preset is library-only: it can be applied to a container by
// hand but never claims an image automatically.
type ExportPreset struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Match     string `json:"match"`
	Dir       string `json:"dir"`
	ExportCmd string `json:"export_cmd"`
	ImportCmd string `json:"import_cmd"`
	// VerifyCmd runs after a successful import and fails the restore on a
	// non-zero exit (F152). Optional, and only worth setting for a command that
	// can actually fail — one that always exits 0 reads like proof and is not.
	VerifyCmd string `json:"verify_cmd,omitempty"`
	User      string `json:"user"`
	// Builtin marks a preset that ships with DockBack. Read-only in the UI and
	// never persisted — the built-ins are surfaced in the library so an operator
	// can see and copy them, not so they can be edited into something else.
	Builtin bool `json:"builtin,omitempty"`
}

// Complete reports whether a preset can actually drive an export. A library
// entry missing a piece is kept (it may be half-written) but never applied.
func (p ExportPreset) Complete() bool {
	return strings.TrimSpace(p.Dir) != "" &&
		strings.TrimSpace(p.ExportCmd) != "" &&
		strings.TrimSpace(p.ImportCmd) != ""
}

// Profile converts a preset into the effective profile shape. The preset's NAME
// becomes the tool recorded in the manifest, so a restore says which recipe
// produced the export rather than an anonymous "custom".
func (p ExportPreset) Profile() ExportProfile {
	tool := strings.TrimSpace(p.Name)
	if tool == "" {
		tool = "custom"
	}
	prof := ExportProfile{
		Tool:      tool,
		Dir:       strings.TrimSpace(p.Dir),
		ExportCmd: []string{"/bin/sh", "-c", strings.TrimSpace(p.ExportCmd)},
		ImportCmd: []string{"/bin/sh", "-c", strings.TrimSpace(p.ImportCmd)},
		User:      strings.TrimSpace(p.User),
	}
	if v := strings.TrimSpace(p.VerifyCmd); v != "" {
		prof.VerifyCmd = []string{"/bin/sh", "-c", v}
	}
	return prof
}

// ParseExportPresets reads a stored preset library. Malformed JSON yields an
// empty library rather than an error: a corrupt setting must degrade to "no
// presets" — which falls back to the built-ins — not break every backup.
func ParseExportPresets(js string) []ExportPreset {
	var out []ExportPreset
	if strings.TrimSpace(js) == "" {
		return nil
	}
	if json.Unmarshal([]byte(js), &out) != nil {
		return nil
	}
	return out
}

// MatchExportPreset returns the first COMPLETE preset whose match hits the image.
//
// Pure, so the precedence rule is table-testable without a store. Order is the
// library's own order, which the UI preserves, so an operator can resolve an
// overlap by moving a preset rather than by renaming their images.
func MatchExportPreset(presets []ExportPreset, image string) (ExportPreset, bool) {
	img := strings.ToLower(image)
	for _, p := range presets {
		m := strings.ToLower(strings.TrimSpace(p.Match))
		if m == "" || !strings.Contains(img, m) {
			continue
		}
		if !p.Complete() {
			continue // a half-written library entry must never drive a backup
		}
		return p, true
	}
	return ExportPreset{}, false
}

// BuiltinExportPresets exposes the shipped table as presets, so the UI can show
// them in the library alongside the operator's own — visible and copyable, but
// flagged read-only.
func BuiltinExportPresets() []ExportPreset {
	out := make([]ExportPreset, 0, len(exportPresets))
	for _, p := range exportPresets {
		out = append(out, ExportPreset{
			ID:        "builtin:" + p.match,
			Name:      p.profile.Tool,
			Match:     p.match,
			Dir:       p.profile.Dir,
			ExportCmd: cmdToLine(p.profile.ExportCmd),
			ImportCmd: cmdToLine(p.profile.ImportCmd),
			User:      p.profile.User,
			Builtin:   true,
		})
	}
	return out
}

// savedExportPresets loads the operator's preset library.
func (e *Engine) savedExportPresets() []ExportPreset {
	js, _ := e.Store.GetSetting(ExportPresetsKey, "")
	return ParseExportPresets(js)
}
