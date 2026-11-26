package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"

	"dockback/internal/dockercli"
)

// Fresh proof for the actions that hand a container over (SEC-001 to SEC-003).
//
// A signed-in session is enough to look and to run backups. It is not enough to
// store commands DockBack will run inside a container, to choose the helper image
// that is started against every container's data, to accept a helper image that
// changed, or to stop a container's restores from asking for the password. Each of
// those asks for the password (and code) again, like revealing the key does, so a
// browser left signed in or a copied cookie stops short of the containers.

// savedSettingFields decodes a stored per-container setting, leaving out every
// field that is empty. Two values with the same fields run the same commands, so
// saving a form unchanged, or clearing one that was already empty, is no change.
func savedSettingFields(encoded string) map[string]any {
	fields := map[string]any{}
	if err := json.Unmarshal([]byte(encoded), &fields); err != nil {
		return map[string]any{}
	}
	for name, value := range fields {
		if isEmptyField(value) {
			delete(fields, name)
		}
	}
	return fields
}

func isEmptyField(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(typed) == ""
	case []any:
		return len(typed) == 0
	}
	return false
}

// addsOrChangesCommands reports whether replacing current with proposed gives
// DockBack something new to run. Clearing commands needs no proof: it only takes
// power away.
func addsOrChangesCommands(current, proposed string) bool {
	proposedFields := savedSettingFields(proposed)
	if len(proposedFields) == 0 {
		return false
	}
	return !reflect.DeepEqual(savedSettingFields(current), proposedFields)
}

// freshProofForCommands gates a save of commands that run inside a container
// (backup hooks, an export profile). It returns true when the save may go ahead;
// on false it has already answered.
func (s *Server) freshProofForCommands(w http.ResponseWriter, r *http.Request, credentials stepUpBody, settingKey, proposed string) bool {
	current, _ := s.store.GetSetting(settingKey, "")
	if !addsOrChangesCommands(current, proposed) {
		return true
	}
	return s.requireFreshAuth(w, r, credentials)
}

// helperImageSettingKey names the image started against containers' volumes.
const helperImageSettingKey = "backup.sidecar_image"

// helperImageChange reports whether a settings save would change the helper image.
// The settings form sends the field with every save, so only a different value
// counts.
func helperImageChange(req map[string]string) bool {
	proposed, present := req[helperImageSettingKey]
	proposed = strings.TrimSpace(proposed)
	if !present || proposed == "" {
		return false
	}
	return proposed != dockercli.SidecarImage()
}

// restoreProtectedWith resolves whether a container's restores would ask for the
// password if it were marked critical and write-only as given: the operator's
// recorded choice when there is one, otherwise what those two marks imply.
func (s *Server) restoreProtectedWith(nodeID, name string, critical, requireWriteOnly bool) bool {
	if require, recorded := s.explicitRestoreStepUp(nodeID, name); recorded {
		return require
	}
	return critical || requireWriteOnly
}

// isCriticalDB reports whether the container is marked as a critical database.
func (s *Server) isCriticalDB(nodeID, name string) bool {
	_, critical := s.loadCriticalDBs()[critKey(nodeID, name)]
	return critical
}

// freshProofToUnprotectRestores gates a change that would leave a protected
// container's restores unprotected. Protection that only an intruder's own session
// has to switch off protects against accidents and nothing else. It returns true
// when the change may go ahead; on false it has already answered.
func (s *Server) freshProofToUnprotectRestores(w http.ResponseWriter, r *http.Request, credentials stepUpBody, nodeID, name string, protectedAfter bool) bool {
	if protectedAfter || !s.restoreStepUpRequired(nodeID, name) {
		return true
	}
	return s.requireFreshAuth(w, r, credentials)
}
