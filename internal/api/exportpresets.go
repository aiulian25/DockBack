package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"dockback/internal/backup"
)

// Reusable app-native export presets (F101).
//
// SECURITY POSTURE, stated up front because it is the whole story here.
//
// A preset holds an operator-authored shell line that DockBack runs INSIDE a
// container with that container's privileges. That capability already existed
// per-container (SEC-7, auth+CSRF gated, audited) and is intentional: it is what
// makes an app-native export possible at all. What this feature adds is REUSE —
// including importing a library of presets as JSON from another deployment.
//
// That import is the one genuinely new exposure, so it is treated as such:
//
//   - Nothing is ever imported implicitly. Import is an explicit, CSRF-protected,
//     audited action, and the UI shows every command before it is saved.
//   - A preset never executes merely by matching an image. App-native export runs
//     only when it is enabled FOR THAT BACKUP, which stays an explicit choice.
//   - Presets are stored as configuration and are readable back in full, so what
//     will run is always inspectable rather than hidden behind a name.
//
// Import a preset library the way you would a shell script from the internet:
// read it first. The API's job is to make sure you CAN.

// maxExportPresets bounds the library. Presets are read on every export-profile
// lookup, and an unbounded list — trivially produced by a bad import — would put
// an ever-growing JSON parse in that path.
const maxExportPresets = 200

// maxPresetFieldLen bounds each field. A command longer than this is a script,
// and a script belongs in the image, not in a settings blob replicated into
// every app backup.
const maxPresetFieldLen = 4000

func (s *Server) loadExportPresets() []backup.ExportPreset {
	js, _ := s.store.GetSetting(backup.ExportPresetsKey, "")
	return backup.ParseExportPresets(js)
}

func (s *Server) saveExportPresets(p []backup.ExportPreset) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return s.store.SetSetting(backup.ExportPresetsKey, string(b))
}

// validateExportPreset applies the same shape checks the per-container export
// form uses, plus the bounds a shared library needs.
//
// It deliberately does NOT try to judge whether a command is "safe". There is no
// honest way to do that for an arbitrary shell line, and pretending otherwise
// would be worse than saying plainly that the operator is responsible for what
// they save — which the UI and docs do.
func validateExportPreset(p *backup.ExportPreset) error {
	p.Name = strings.TrimSpace(p.Name)
	p.Match = strings.TrimSpace(p.Match)
	p.Dir = strings.TrimSpace(p.Dir)
	p.ExportCmd = strings.TrimSpace(p.ExportCmd)
	p.ImportCmd = strings.TrimSpace(p.ImportCmd)
	p.VerifyCmd = strings.TrimSpace(p.VerifyCmd)
	p.User = strings.TrimSpace(p.User)
	// A built-in flag is never accepted from a client — it is a display property
	// the server assigns, and honouring it would let an import mark its own
	// entries read-only in the UI.
	p.Builtin = false

	if p.Name == "" {
		return fmt.Errorf("a preset needs a name")
	}
	if p.Dir == "" && p.ExportCmd == "" && p.ImportCmd == "" {
		return fmt.Errorf("preset %q is empty — it needs at least a directory or a command", p.Name)
	}
	if p.Dir != "" && !strings.HasPrefix(p.Dir, "/") {
		return fmt.Errorf("preset %q: the export directory must be an absolute path inside the container", p.Name)
	}
	for label, v := range map[string]string{
		"name": p.Name, "match": p.Match, "directory": p.Dir,
		"export command": p.ExportCmd, "import command": p.ImportCmd,
		"verify command": p.VerifyCmd, "user": p.User,
	} {
		if len(v) > maxPresetFieldLen {
			return fmt.Errorf("preset %q: %s is too long (max %d characters)", p.Name, label, maxPresetFieldLen)
		}
		if strings.ContainsRune(v, 0) {
			return fmt.Errorf("preset %q: %s contains a null byte", p.Name, label)
		}
	}
	return nil
}

// handleListExportPresets — GET /api/export-presets.
//
// Returns the operator's library plus the built-ins (flagged read-only), so the
// UI can show what a container would pick up today without knowing the built-in
// table itself.
func (s *Server) handleListExportPresets(w http.ResponseWriter, r *http.Request) {
	saved := s.loadExportPresets()
	out := make([]backup.ExportPreset, 0, len(saved)+3)
	out = append(out, saved...)
	out = append(out, backup.BuiltinExportPresets()...)
	writeJSON(w, http.StatusOK, map[string]any{"presets": out})
}

type saveExportPresetsReq struct {
	// One preset upserts a single entry; Presets replaces the whole library
	// (the import path). Exactly one form is accepted per request, so an import
	// can never be mistaken for an edit or silently merged.
	Preset  *backup.ExportPreset   `json:"preset,omitempty"`
	Presets *[]backup.ExportPreset `json:"presets,omitempty"`
	// Replace must be set explicitly alongside Presets. Wholesale replacement of
	// a library of commands is not something to do by accident.
	Replace bool `json:"replace,omitempty"`
}

// handleSaveExportPresets — POST /api/export-presets (auth + csrf, audited).
func (s *Server) handleSaveExportPresets(w http.ResponseWriter, r *http.Request) {
	var req saveExportPresetsReq
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	switch {
	case req.Preset != nil && req.Presets != nil:
		errJSON(w, http.StatusBadRequest, "send either one preset or a full library, not both")
		return

	case req.Preset != nil:
		p := *req.Preset
		if err := validateExportPreset(&p); err != nil {
			errJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		list := s.loadExportPresets()
		replaced := false
		if p.ID != "" {
			for i := range list {
				if list[i].ID == p.ID {
					list[i] = p
					replaced = true
					break
				}
			}
		}
		if !replaced {
			if p.ID == "" {
				p.ID = backup.NewID()
			}
			if len(list) >= maxExportPresets {
				errJSON(w, http.StatusBadRequest, fmt.Sprintf("the preset library is full (max %d)", maxExportPresets))
				return
			}
			list = append(list, p)
		}
		if err := s.saveExportPresets(list); err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		// The audit records WHICH preset and what image it claims — enough to
		// answer "when did this start running in my containers?" — but not the
		// command body, which is readable from the library itself.
		_ = s.store.Audit(userFrom(r), "exportpreset.save", p.ID, "name="+p.Name+" match="+p.Match)
		writeJSON(w, http.StatusOK, map[string]any{"preset": p})

	case req.Presets != nil:
		if !req.Replace {
			errJSON(w, http.StatusBadRequest, "importing a whole library replaces the current one — set replace=true to confirm")
			return
		}
		list := *req.Presets
		if len(list) > maxExportPresets {
			errJSON(w, http.StatusBadRequest, fmt.Sprintf("too many presets (max %d)", maxExportPresets))
			return
		}
		seen := map[string]bool{}
		for i := range list {
			if err := validateExportPreset(&list[i]); err != nil {
				errJSON(w, http.StatusBadRequest, err.Error())
				return
			}
			// Ids from an imported file are re-minted when they collide, so an
			// import can never overwrite an unrelated local preset by id reuse.
			if list[i].ID == "" || seen[list[i].ID] {
				list[i].ID = backup.NewID()
			}
			seen[list[i].ID] = true
		}
		if err := s.saveExportPresets(list); err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		_ = s.store.Audit(userFrom(r), "exportpreset.import", "", fmt.Sprintf("replaced the library with %d preset(s)", len(list)))
		writeJSON(w, http.StatusOK, map[string]any{"presets": list})

	default:
		errJSON(w, http.StatusBadRequest, "nothing to save")
	}
}

// handleDeleteExportPreset — DELETE /api/export-presets/{id} (auth + csrf, audited).
//
// Deleting a preset does not touch any container: a container that was given
// these commands holds its own copy, so removing the library entry stops it
// being applied to NEW containers and changes nothing already configured.
func (s *Server) handleDeleteExportPreset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if strings.HasPrefix(id, "builtin:") {
		errJSON(w, http.StatusBadRequest, "built-in presets cannot be deleted")
		return
	}
	list := s.loadExportPresets()
	out := make([]backup.ExportPreset, 0, len(list))
	var removed *backup.ExportPreset
	for i := range list {
		if list[i].ID == id {
			removed = &list[i]
			continue
		}
		out = append(out, list[i])
	}
	if removed == nil {
		errJSON(w, http.StatusNotFound, "no such preset")
		return
	}
	if err := s.saveExportPresets(out); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "exportpreset.delete", id, "name="+removed.Name)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
