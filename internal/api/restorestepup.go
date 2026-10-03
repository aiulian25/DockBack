package api

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Step-up to overwrite a protected container in place (F206).
//
// THE ASYMMETRY THIS FIXES
//
// Reading a backup out of DockBack demands fresh proof of the password:
// downloading one, extracting a file from one, revealing the encryption key,
// minting a token. Destroying the live data those backups exist to protect did
// not. An in-place restore was auth + CSRF and a confirm checkbox, and it runs a
// WipeDir over the container's data directory.
//
// That is the wrong way round. A leaked export is a confidentiality incident an
// operator can respond to; a production database overwritten with a six-week-old
// snapshot is gone. The same unattended browser that this app already refuses to
// let mint an API token could wipe the database that token would have read.
//
// WHY IT IS NOT ON FOR EVERYTHING
//
// Because a prompt everybody dismisses protects nobody. Restoring is a routine
// operation on most containers — that is the whole point of a backup tool, and
// making every restore a two-step ceremony would train the operator to type
// their password without reading what they are typing it for. So it defaults on
// exactly where an accident is unrecoverable and off everywhere else, and an
// operator can set it either way per container.
//
// WHY "RESTORE AS A COPY" IS EXEMPT
//
// A copy touches nothing that exists. It brings the backup up beside the
// original under a new name, so the worst outcome of a mistaken click is a
// container to delete. Gating it would attach the friction to the SAFE option
// and push people toward the destructive one, which is precisely backwards. This
// is the one place in the app where the safer path is deliberately the faster
// one.

// restoreStepUpKey is the per-container setting, following the same shape as
// every other per-container backup choice so scheduled and manual runs agree.
const restoreStepUpKey = "restore.stepup"

func restoreStepUpKeyFor(nodeID, name string) string {
	return restoreStepUpKey + "." + nodeID + "." + name
}

// restoreStepUpDefault reports whether a container would be protected with no
// explicit choice recorded: one whose data is either irreplaceable or already
// declared too sensitive to hold readably.
//
//   - CRITICAL (F31): the operator has said this database's recovery point
//     matters in minutes. Overwriting it by accident is the exact failure that
//     mark exists to prevent, at a scale no RPO setting can undo.
//   - REQUIRE-WRITE-ONLY (F163): the operator has said they would rather have no
//     backup than a readable one. Someone that deliberate about the archive
//     should not find the live copy is the unguarded end of the same system.
//
// Derived rather than stored, so a container marked critical tomorrow is
// protected tomorrow without anyone remembering to come back here.
func (s *Server) restoreStepUpDefault(nodeID, name string) bool {
	if _, ok := s.loadCriticalDBs()[critKey(nodeID, name)]; ok {
		return true
	}
	return s.engine.RequireWriteOnly(nodeID, name)
}

// restoreStepUpRequired resolves the effective policy: an explicit per-container
// choice when one is recorded, otherwise the derived default.
//
// An explicit "false" is honoured. An operator who has looked at the prompt and
// decided this container does not need it is making an informed choice, and a
// setting that could only ever be turned ON would be a setting nobody trusts.
func (s *Server) restoreStepUpRequired(nodeID, name string) bool {
	explicit, _ := s.store.GetSetting(restoreStepUpKeyFor(nodeID, name), "")
	switch strings.TrimSpace(explicit) {
	case "true":
		return true
	case "false":
		return false
	}
	return s.restoreStepUpDefault(nodeID, name)
}

// restoreStepUpIsExplicit reports whether a per-container choice is recorded, so
// the UI can show "following the default" rather than implying somebody chose.
func (s *Server) restoreStepUpIsExplicit(nodeID, name string) bool {
	v, _ := s.store.GetSetting(restoreStepUpKeyFor(nodeID, name), "")
	v = strings.TrimSpace(v)
	return v == "true" || v == "false"
}

// protectedRestoreMembers returns, in stable order, the containers in this set
// whose in-place restore demands fresh proof of the password (F210).
//
// Composed restores — a whole stack, a whole node — overwrite exactly the same
// live data a single-container restore does, and until this they did it with no
// step-up at all. That made them a bypass: a container the operator had marked
// protected was gated when restored on its own and unguarded when restored as
// part of its stack, which is the more convenient button of the two.
//
// nodeID is the node the restore LANDS ON, never the one the backups came from.
// Only the target's containers can be overwritten — a cross-host restore leaves
// the source exactly as it was — so a container marked on the origin node and
// restored onto a fresh machine correctly needs no step-up: there is nothing
// there to destroy. Same resolution the single-backup readiness check uses.
//
// Returned as a LIST rather than a bool because the operator being asked for
// their password deserves to know which member caused it; a stack of twelve
// services should not present an unexplained prompt.
func (s *Server) protectedRestoreMembers(nodeID string, names []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, name := range names {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if s.restoreStepUpRequired(nodeID, name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// stepUpDetail is the audit suffix recording that a protected overwrite was
// authorised, mirroring the single-container restore so the trail reads the same
// whichever door the restore came through (F210).
func stepUpDetail(protected []string) string {
	if len(protected) == 0 {
		return ""
	}
	return " (protected: step-up ok — " + strings.Join(protected, ", ") + ")"
}

// setRestoreStepUp records an explicit choice, or clears it back to the derived
// default when explicit is false.
func (s *Server) setRestoreStepUp(nodeID, name string, on, explicit bool) error {
	key := restoreStepUpKeyFor(nodeID, name)
	if !explicit {
		return s.store.SetSetting(key, "")
	}
	if on {
		return s.store.SetSetting(key, "true")
	}
	return s.store.SetSetting(key, "false")
}

// handleSetRestoreStepUp stores whether an in-place restore of this container
// demands fresh proof of the password (F206).
//
// Auth + CSRF gated and audited, like every other per-container backup option.
// Deliberately NOT step-up gated itself: it is an opt-in guard on a future
// action, and requiring a password to arm a safety catch would stop people
// arming it. Turning it OFF is audited for the same reason the guard exists.
func (s *Server) handleSetRestoreStepUp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cid := r.PathValue("cid")
	var body struct {
		Require *bool `json:"require"` // null clears the override, back to the derived default
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	name, err := s.containerNameFor(r, id, cid)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	on := body.Require != nil && *body.Require
	if err := s.setRestoreStepUp(id, name, on, body.Require != nil); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	detail := "require=" + boolWord(on)
	if body.Require == nil {
		detail = "cleared — following the derived default"
	}
	_ = s.store.Audit(userFrom(r), "restore.stepup.set", name, detail)
	writeJSON(w, http.StatusOK, map[string]any{
		"restore_step_up":         s.restoreStepUpRequired(id, name),
		"restore_step_up_default": s.restoreStepUpDefault(id, name),
		"restore_step_up_set":     s.restoreStepUpIsExplicit(id, name),
	})
}

func boolWord(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// containerNameFor resolves a container's NAME from its id on a node — the key
// every per-container setting uses, because a name survives the recreates a
// container id does not.
func (s *Server) containerNameFor(r *http.Request, nodeID, containerID string) (string, error) {
	cli, err := s.reg.Get(nodeID)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	insp, err := cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(insp.Name, "/"), nil
}
