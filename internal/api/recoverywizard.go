package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// F223 — "Recover this fleet": the empty state IS the runbook.
//
// Every piece of total-loss recovery already existed, on a different screen:
// restore the app-backup (Settings), re-adopt the destinations so the catalog
// points at real archives (Settings), rebuild each dead node from that catalog
// (Recovery). What did not exist was the SEQUENCE. A fresh install after a fire
// showed an ordinary empty dashboard, and the operator had to already know the
// order — which is the one thing nobody knows at that moment.
//
// This is state and guidance only. It starts nothing, restores nothing, and
// deletes nothing; every step links to the screen that already does the work.
// That is deliberate: a recovery wizard that reimplements recovery is a second
// implementation of the most dangerous code in the product.

const (
	// wizardStateKey holds the step. Absent = never started.
	wizardStateKey = "recovery.wizard_state"

	// wizardMarkerFile is written beside the staged restore database, because
	// the app-restore REPLACES the database a setting would have been written
	// into. The marker survives that swap for exactly the same reason
	// restore.db does — it is a file in the data dir, not a row.
	wizardMarkerFile = "recovery.wizard"
)

// The steps, in order. Unknown values from a hand-edited setting are treated as
// no state at all rather than trusted into the UI.
const (
	wizardStepAppRestored  = "app-restored"
	wizardStepDestsChecked = "dests-verified"
	wizardStepDone         = "done"
	wizardStepDismissed    = "dismissed"
)

func validWizardStep(s string) bool {
	switch s {
	case wizardStepAppRestored, wizardStepDestsChecked, wizardStepDone, wizardStepDismissed:
		return true
	}
	return false
}

// localNodeID is the node this app registers for itself on first run. It is not
// evidence that an operator has connected anything.
const localNodeID = "local"

type recoveryStateResp struct {
	// FreshInstall means this instance holds nothing: no backups, and no node
	// the operator added. See freshInstall for why "no node" is not literal.
	FreshInstall bool   `json:"fresh_install"`
	Step         string `json:"step"`
	Nodes        int    `json:"nodes"`   // operator-added nodes
	Backups      int    `json:"backups"` // rows in the catalog
	// HasAppDestinations tells the wizard whether step 1 can offer "fetch it from
	// a destination" or only "upload the file" — after a total loss there is
	// usually nothing configured, and offering an empty picker would read as a
	// broken feature rather than an expected state.
	HasAppDestinations bool `json:"has_app_destinations"`
}

// freshInstall reports whether this instance looks like a brand-new one.
//
// NOT literally "zero nodes": main.go auto-registers the `local` node at boot
// whenever a local Docker socket is configured, which is the shipped default. A
// literal count would therefore be 1 on every normal install and the wizard
// would never appear at all. What matters is whether the OPERATOR has put
// anything here — a node they connected, or a backup that exists.
func (s *Server) freshInstall() (fresh bool, operatorNodes, backups int) {
	nodes, err := s.store.ListNodes()
	if err != nil {
		return false, 0, 0 // cannot tell: never claim a populated install is empty
	}
	for _, n := range nodes {
		if n.ID != localNodeID {
			operatorNodes++
		}
	}
	n, err := s.store.CountBackups()
	if err != nil {
		return false, operatorNodes, 0
	}
	backups = n
	return operatorNodes == 0 && backups == 0, operatorNodes, backups
}

// handleRecoveryState reports whether to show the wizard, and where it is up to.
func (s *Server) handleRecoveryState(w http.ResponseWriter, r *http.Request) {
	// Convert the marker the pre-restart instance left behind. Done on read
	// rather than at boot so it lands in the RESTORED database — the one this
	// process is now serving — with no ordering dependency on startup.
	s.adoptWizardMarker()

	fresh, nodes, backups := s.freshInstall()
	step, _ := s.store.GetSetting(wizardStateKey, "")
	if !validWizardStep(step) {
		step = ""
	}
	dests, _ := s.store.ListAppDestinations()
	writeJSON(w, http.StatusOK, recoveryStateResp{
		FreshInstall: fresh, Step: step, Nodes: nodes, Backups: backups,
		HasAppDestinations: len(dests) > 0,
	})
}

// handleSetRecoveryState records the step the operator has reached.
func (s *Server) handleSetRecoveryState(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Step string `json:"step"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	step := strings.TrimSpace(body.Step)
	if step != "" && !validWizardStep(step) {
		errJSON(w, http.StatusBadRequest, "unknown recovery step")
		return
	}
	if err := s.store.SetSetting(wizardStateKey, step); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"step": step})
}

// markWizardAppRestored writes the marker file that survives the database swap.
//
// Called from finishRestore, BEFORE the restart: at that moment the live
// database is about to be replaced by the restored one, so anything written to
// a settings row is thrown away seconds later. The file is not.
//
// Best-effort by design. This is a hint about which paragraph to show; a
// read-only or full data dir must never turn a successful app-restore into a
// failed one.
func (s *Server) markWizardAppRestored() {
	if s.cfg == nil || s.cfg.DataDir == "" {
		return
	}
	_ = os.WriteFile(filepath.Join(s.cfg.DataDir, wizardMarkerFile), []byte(wizardStepAppRestored), 0o600)
}

// adoptWizardMarker converts the marker left by the pre-restart instance into a
// setting in the database that is now live, then removes it. Idempotent, and
// silent about a marker it cannot read — the wizard is guidance, and guidance
// that fails should disappear rather than assert something wrong.
func (s *Server) adoptWizardMarker() {
	if s.cfg == nil || s.cfg.DataDir == "" {
		return
	}
	path := filepath.Join(s.cfg.DataDir, wizardMarkerFile)
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	_ = os.Remove(path)
	step := strings.TrimSpace(string(b))
	if !validWizardStep(step) {
		return
	}
	// Never move BACKWARDS. If the restored database already recorded a later
	// step — or the operator dismissed the wizard — a stale marker from an
	// earlier restore must not reopen it.
	if cur, _ := s.store.GetSetting(wizardStateKey, ""); validWizardStep(cur) {
		return
	}
	_ = s.store.SetSetting(wizardStateKey, step)
}
