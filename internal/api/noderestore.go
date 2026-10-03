package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"dockback/internal/backup"
	"dockback/internal/notify"
	"dockback/internal/store"
)

// Whole-node disaster-recovery restore (F6). The runbook already computes the
// exact per-node restore ORDER (databases first with their extensions, then
// dependent apps); this executes that order in one action instead of the operator
// restoring each stack/standalone container by hand at 2 a.m.
//
// Safety: it takes EVERY affected stack's restore lock up front (so it can't
// overlap a backup or per-stack restore of any of them) and drives each service
// through the same single-backup restore path a manual restore uses — which
// includes the F4 post-restore health gate, so a database is proven healthy
// before any dependent app is started. Progress streams under log id node:<id>.

// nodeRestorePlanEntry is one row of the whole-node restore plan (F211) — what
// the executor will do, in the order it will do it, before anything is touched.
type nodeRestorePlanEntry struct {
	Order     int    `json:"order"`
	Label     string `json:"label"` // "stack/container", or the container name
	Container string `json:"container"`
	Stack     string `json:"stack,omitempty"`
	Service   string `json:"service,omitempty"`
	Role      string `json:"role"` // "database" | "application"
	BackupID  string `json:"backup_id"`
	CreatedAt int64  `json:"created_at"`
	Verified  string `json:"verified"`
	// WriteOnly marks a member sealed to the offline keypair. On this path that
	// is not decoration: the whole-node executor supplies no private key, so such
	// a member cannot be restored here at all — see nodeRestoreBlockReason.
	WriteOnly bool `json:"write_only,omitempty"`
	// Blocked is why this member cannot be restored by a whole-node run, or empty
	// when it can. A non-empty value used to be discovered MID-RUN, after the
	// services ordered before it had already been overwritten.
	Blocked string `json:"blocked,omitempty"`
	// Atomic marks a member of an application whose services are only meaningful
	// together (F146). F227: the selector may not drop one of these on its own —
	// the engine refuses such a set anyway, and learning that after confirming
	// something destructive is the wrong place to learn it.
	Atomic bool `json:"atomic,omitempty"`
}

// nodeRestoreBlockReason says why a service cannot be restored by a whole-node
// run, or "" when it can (F211).
//
// ONE definition, consulted by both the plan endpoint and the executor, so the
// preview and the destructive run can never disagree about what is restorable —
// the same reason the stack plan and RestoreStack share their selection.
//
// What is NOT blocked, deliberately: a standalone-volume backup. Its manifest
// records no container id, which the executor's old guard treated as "no
// restorable backup" and aborted the entire node restore over — so a single
// orphan volume on a node stopped its disaster recovery part-way through. But
// Engine.Restore dispatches a "volume:" target to restoreVolumeOnly, which needs
// no container at all. The backup was always restorable; only the guard said
// otherwise.
func (s *Server) nodeRestoreBlockReason(svc runbookService, b *store.Backup, man *backup.Manifest, haveKey bool) string {
	if svc.backupID == "" {
		return "no backup recorded for this service"
	}
	// A write-only backup is sealed to the offline keypair, and a whole-node
	// restore has nowhere to ask for it: it restores many services in one
	// unattended run. Refusing up front beats failing at service seven.
	if backup.IsWriteOnly(man) && !haveKey {
		return "write-only encrypted — paste the offline private key to include it, or restore it separately from its own backup"
	}
	// F174: this service's own configuration would stop its restore — a database
	// whose environment cannot re-initialise an empty data directory. Manifest-
	// derived, so it costs nothing and is known before anything is touched.
	if blk := backup.DBInitBlock(man); blk != "" {
		return blk
	}
	// A container backup that records no container id, which is not the volume
	// case above. Rare (a legacy or truncated manifest) and genuinely unrunnable.
	if svc.containerID == "" && !backup.IsVolumeOnlyBackup(b) {
		return "this backup records no container to restore into"
	}
	return ""
}

// nodeRestorePlanRows resolves the executor's plan into displayable rows plus
// the blocked reasons, reading each service's backup row once (F211).
func (s *Server) nodeRestorePlanRows(plan []runbookService) []nodeRestorePlanEntry {
	return s.nodeRestorePlanRowsWithKey(plan, false)
}

// nodeRestorePlanRowsWithKey is nodeRestorePlanRows told whether an offline key
// is in hand (F212), which is what decides whether a write-only member is
// restorable by this run at all.
func (s *Server) nodeRestorePlanRowsWithKey(plan []runbookService, haveKey bool) []nodeRestorePlanEntry {
	out := make([]nodeRestorePlanEntry, 0, len(plan))
	for i, svc := range plan {
		e := nodeRestorePlanEntry{
			Order: i + 1, Label: serviceLabel(svc), Container: svc.Container,
			Stack: svc.Stack, Service: svc.Service, Role: svc.Role,
			BackupID: svc.backupID, CreatedAt: svc.LastBackupAt,
		}
		var man backup.Manifest
		b, err := s.store.GetBackup(svc.backupID)
		if err == nil {
			e.Verified = b.Verified
			_ = json.Unmarshal([]byte(b.ManifestJSON), &man)
		}
		e.WriteOnly = backup.IsWriteOnly(&man)
		e.Atomic = man.StackAtomic != nil
		e.Blocked = s.nodeRestoreBlockReason(svc, b, &man, haveKey)
		out = append(out, e)
	}
	return out
}

// handleRestoreNodeAllPlan returns the whole-node restore plan without touching
// anything (F211): the exact services the executor would restore, in its order,
// with the reason any of them cannot be.
//
// Read-only and idempotent, so it is auth-gated but not CSRF-gated, like the
// other pre-restore previews.
func (s *Server) handleRestoreNodeAllPlan(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.GetNode(id); err != nil {
		errJSON(w, http.StatusNotFound, "node not found")
		return
	}
	rows := s.nodeRestorePlanRows(s.nodeRestorePlan(id))
	blocked := 0
	for _, e := range rows {
		if e.Blocked != "" {
			blocked++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"services": rows,
		"blocked":  blocked,
	})
}

// trimmedSet turns a request's list into a lookup, dropping blanks. An empty
// result means "no selection given", which is how the caller distinguishes
// "everything" from "nothing".
func trimmedSet(in []string) map[string]bool {
	out := map[string]bool{}
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out[v] = true
		}
	}
	return out
}

// validateAtomicSelection refuses a selection that would restore PART of an
// application whose services are only meaningful together (F146). Returns the
// refusal, or "" when the selection is sound.
//
// Enforced here and not only in the browser: a partial atomic set produces a
// deployment that starts and does not work — an app pointing at a database
// restored from a different moment, or never restored at all — and that is worth
// refusing whatever asked for it. Pure, so the rule is unit-testable.
func validateAtomicSelection(rows []nodeRestorePlanEntry, sel map[string]bool) string {
	type group struct{ missing []string }
	byStack := map[string]*group{}
	touched := map[string]bool{}
	for _, e := range rows {
		if e.Stack == "" || !e.Atomic || e.Blocked != "" {
			continue
		}
		g := byStack[e.Stack]
		if g == nil {
			g = &group{}
			byStack[e.Stack] = g
		}
		if sel[e.Label] {
			touched[e.Stack] = true
			continue
		}
		g.missing = append(g.missing, e.Label)
	}
	stacks := make([]string, 0, len(byStack))
	for st := range byStack {
		stacks = append(stacks, st)
	}
	sort.Strings(stacks)
	for _, st := range stacks {
		g := byStack[st]
		if !touched[st] || len(g.missing) == 0 {
			continue
		}
		sort.Strings(g.missing)
		return fmt.Sprintf(
			"%q is an application whose services are only meaningful together, so it is rebuilt as a whole or not at all. "+
				"Deselected: %s. Include them, or untick the whole project — nothing has been changed.",
			st, strings.Join(g.missing, ", "))
	}
	return ""
}

// handleRestoreNodeAll runs the whole-node restore in the runbook's proven order.
func (s *Server) handleRestoreNodeAll(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	node, err := s.store.GetNode(id)
	if err != nil {
		errJSON(w, http.StatusNotFound, "node not found")
		return
	}
	plan := s.nodeRestorePlan(id)
	if len(plan) == 0 {
		errJSON(w, http.StatusNotFound, "no restorable backups found for this node")
		return
	}

	// Optional body: the F210 step-up credentials, plus the operator's explicit
	// decision to leave the blocked services out (F211). A caller that sends none
	// and has nothing blocked or protected is unchanged.
	var body struct {
		stepUpBody
		SkipBlocked bool `json:"skip_blocked"`
		// F227: which services this run covers, by the same label the plan shows
		// ("stack/container", or the container name). Omitted or empty means every
		// service — what every caller before this sent, and still the default.
		//
		// Validated against the server's OWN plan below, never trusted as given:
		// the catalog can change between the preview and the confirm, and which
		// containers get overwritten is not a decision to take on a request's word.
		Services []string `json:"services"`
		// F212: rebuild this node's services onto a DIFFERENT machine. Empty means
		// the node itself, which is what every caller before this sent.
		TargetNode string `json:"target_node"`
		// The offline key for write-only members. In the body, never a URL (F209).
		PrivateKey string `json:"private_key"`
		// The cross-host grammar, identical to the stack dialog's (F212).
		ReconstructHost bool   `json:"reconstruct_host"`
		HostBaseDir     string `json:"host_base_dir"`
		RemapIP         bool   `json:"remap_ip"`
		RemapFromIP     string `json:"remap_from_ip"`
		RemapToIP       string `json:"remap_to_ip"`
		RemapDomain     bool   `json:"remap_domain"`
		RemapFromDomain string `json:"remap_from_domain"`
		RemapToDomain   string `json:"remap_to_domain"`
		RemapPath       bool   `json:"remap_path"`
		RemapFromPath   string `json:"remap_from_path"`
		RemapToPath     string `json:"remap_to_path"`
	}
	if r.ContentLength != 0 {
		if err := readJSON(r, &body); err != nil {
			errJSON(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}

	// F212: the machine this rebuild lands on. "Restore entire node" could only
	// ever target the node whose death is the reason you need it — so rebuilding a
	// dead host meant opening the stack dialog once per stack and restoring the
	// standalone containers one at a time, re-entering the same remaps each time.
	targetID := strings.TrimSpace(body.TargetNode)
	if targetID == "" {
		targetID = id
	}
	targetNode := node
	if targetID != id {
		tn, terr := s.store.GetNode(targetID)
		if terr != nil {
			errJSON(w, http.StatusNotFound, "target node not found")
			return
		}
		targetNode = tn
	}
	cross, cerr := s.parseCrossRestoreParams(r.Context(), crossRestoreInput{
		ReconstructHost: body.ReconstructHost, HostBaseDir: body.HostBaseDir,
		RemapIP: body.RemapIP, RemapFromIP: body.RemapFromIP, RemapToIP: body.RemapToIP,
		RemapDomain: body.RemapDomain, RemapFromDomain: body.RemapFromDomain, RemapToDomain: body.RemapToDomain,
		RemapPath: body.RemapPath, RemapFromPath: body.RemapFromPath, RemapToPath: body.RemapToPath,
	}, id, targetID, "") // "" — a machine's stacks each record their own base, resolved per stack
	if cerr != nil {
		errJSON(w, http.StatusBadRequest, cerr.Error())
		return
	}
	privateKey := strings.TrimSpace(body.PrivateKey)

	// F211: refuse a run that would stop part-way, BEFORE it starts.
	//
	// The executor used to discover an unrestorable member at its turn in the
	// sequence — service seven of twelve — and abort there, leaving six recreated
	// and five untouched. Every reason it can fail for is knowable from the
	// manifest, so it is known here instead, and the operator either fixes it or
	// says explicitly which services to leave out.
	//
	// Recomputed server-side rather than trusted from the client's plan: the
	// catalog can change between the preview and the confirm, and "which services
	// may be skipped" is not a decision to take on a request's word.
	rows := s.nodeRestorePlanRowsWithKey(plan, privateKey != "")
	var blockedNames []string
	blockedByLabel := map[string]bool{}
	for _, e := range rows {
		if e.Blocked != "" {
			blockedNames = append(blockedNames, e.Label+" ("+e.Blocked+")")
			blockedByLabel[e.Label] = true
		}
	}
	if len(blockedNames) > 0 && !body.SkipBlocked {
		errJSON(w, http.StatusConflict,
			fmt.Sprintf("%d service(s) on this node cannot be restored by a whole-node run: %s. Restore them individually, or re-run with the blocked services skipped.",
				len(blockedNames), strings.Join(blockedNames, "; ")))
		return
	}
	skipped := []string(nil)
	var deselected []string
	if len(blockedByLabel) > 0 {
		kept := plan[:0:0]
		for _, svc := range plan {
			if blockedByLabel[serviceLabel(svc)] {
				skipped = append(skipped, serviceLabel(svc))
				continue
			}
			kept = append(kept, svc)
		}
		plan = kept
		if len(plan) == 0 {
			errJSON(w, http.StatusConflict, "every service on this node is blocked — there is nothing left to restore")
			return
		}
	}

	// F227: narrow the run to the services the operator picked.
	//
	// Applied AFTER the blocked filter, so an unticked service and an unrestorable
	// one are two different things and each is reported as itself. Narrowing only:
	// a selection can never introduce a service the plan did not already contain,
	// so this cannot widen what the run touches.
	if sel := trimmedSet(body.Services); len(sel) > 0 {
		if verr := validateAtomicSelection(rows, sel); verr != "" {
			errJSON(w, http.StatusConflict, verr)
			return
		}
		kept := plan[:0:0]
		var left []string
		for _, svc := range plan {
			if sel[serviceLabel(svc)] {
				kept = append(kept, svc)
				continue
			}
			left = append(left, serviceLabel(svc))
		}
		if len(kept) == 0 {
			errJSON(w, http.StatusBadRequest, "no services were selected — pick at least one to rebuild")
			return
		}
		plan = kept
		if len(left) > 0 {
			sort.Strings(left)
			deselected = left
		}
	}

	// F210: fresh proof of the password before overwriting a PROTECTED container.
	//
	// This is the widest destructive action in the app — it recreates every
	// service on a node — and it was the one with no step-up at all, while
	// restoring any single one of those containers on its own demanded a password
	// since F206. The plan above is already the exact set that will be
	// overwritten, so the check costs nothing beyond the marks themselves.
	//
	// Credentials arrive in an optional JSON body; a caller that sends none and
	// touches no protected container is unchanged. Placed before the locks below,
	// so a declined prompt leaves nothing held.
	names := make([]string, 0, len(plan))
	for _, svc := range plan {
		names = append(names, svc.Container)
	}
	protected := s.protectedRestoreMembers(targetID, names) // F212 — the overwrite happens on the target
	if len(protected) > 0 && !s.requireFreshAuth(w, r, body.stepUpBody) {
		return
	}

	// Take every affected stack's restore lock up front — the whole-node restore
	// must not overlap a backup or restore of any of its stacks. All-or-nothing: if
	// any is busy, release what we took and 409.
	keys := nodeRestoreLockKeys(targetID, plan)
	if targetID != id {
		// A cross-host rebuild reads the SOURCE node's catalog while writing the
		// target, so both are held — the same both-ends exclusion a cross-node
		// stack migration takes, so neither side can race a backup mid-rebuild.
		keys = append(keys, nodeRestoreLockKeys(id, plan)...)
		sort.Strings(keys)
	}
	var held []string
	ok := true
	for _, k := range keys {
		if !s.locks.acquireRestore(k) {
			ok = false
			break
		}
		held = append(held, k)
	}
	if !ok {
		for _, h := range held {
			s.releaseRestoreAndDispatch(h)
		}
		errJSON(w, http.StatusConflict, "a backup or restore of one of this node's stacks is already in progress — try again once it finishes")
		return
	}

	logID := "node:" + id
	auditDetail := node.Name
	if targetID != id {
		auditDetail += " -> " + targetNode.Name // F212: a rebuild onto another machine
	}
	if privateKey != "" {
		auditDetail += " (write-only key supplied)" // F209 — the fact, never the key
	}
	if len(skipped) > 0 {
		auditDetail += " (skipped: " + strings.Join(skipped, ", ") + ")"
	}
	// F227: what the operator deliberately left out, recorded separately from what
	// could not be restored. "I chose not to" and "it could not" are different
	// answers to "why is this service not back", and the trail should not blur them.
	if len(deselected) > 0 {
		auditDetail += fmt.Sprintf(" (deselected %d: %s)", len(deselected), strings.Join(deselected, ", "))
	}
	_ = s.store.Audit(userFrom(r), "node.restore.all", id, auditDetail+stepUpDetail(protected)) // F210
	go func() {
		defer guardPanic("node restore", logID, func() { s.logSink(logID, "ERR", "Node restore failed: internal error (panic)") })
		defer func() {
			for _, h := range held {
				s.releaseRestoreAndDispatch(h)
			}
		}()
		ctx, finish, ok := s.beginRestoreRun(context.Background(), logID, "node "+node.Name, targetID, 6*time.Hour)
		if !ok {
			s.logSink(logID, "ERR", "Node restore failed: another restore of this node is already running")
			return
		}
		defer finish()
		s.runNodeRestore(ctx, nodeRestoreRun{
			SourceID: id, TargetID: targetID, TargetName: targetNode.Name,
			Plan: plan, Skipped: skipped, Deselected: deselected, LogID: logID,
			Cross: cross, PrivateKey: privateKey,
		})
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

// nodeRestoreLockKeys returns the DISTINCT stack-exclusion keys covering every
// service in the plan (a stack collapses to one key; a standalone container keys
// by its name), sorted for a stable acquire order.
func nodeRestoreLockKeys(nodeID string, plan []runbookService) []string {
	seen := map[string]bool{}
	var keys []string
	for _, svc := range plan {
		k := stackKey(nodeID, svc.Stack, svc.Container)
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// nodeRestoreRun is one whole-node rebuild: what to restore, where, and how
// (F212). Grouped into a struct because the executor now needs the target
// machine and the whole cross-host grammar, and a nine-argument function is how
// the wrong node id ends up in the wrong slot.
type nodeRestoreRun struct {
	SourceID   string // the node whose catalog the backups come from
	TargetID   string // the node they are restored ONTO — the same one, unless rebuilding
	TargetName string
	Plan       []runbookService
	Skipped    []string
	// Deselected is what the operator chose to leave out (F227) — distinct from
	// Skipped, which is what could not be restored.
	Deselected []string
	LogID      string
	Cross      crossRestoreParams
	PrivateKey string
}

// nodeRestoreUnit is one step of the rebuild: a whole compose project, or a
// single standalone container.
type nodeRestoreUnit struct {
	Stack    string // "" for a standalone container
	Services []runbookService
}

// nodeRestoreUnits groups the plan into the units the executor drives, keeping
// each unit at the position of its FIRST member in the runbook order (F212).
//
// A stack is restored as a stack rather than service by service, and that is not
// only for the merged compose file it produces. The per-service path sets no
// stackMember flag, so an application whose services are only meaningful
// together (F146) refused its own member mid-run and took the whole node restore
// down with it — the guard that exists to steer people towards a stack restore
// was firing inside one.
//
// The cost is stated honestly: ordering is now stack-at-a-time rather than every
// database on the machine before every application. Within a stack the
// dependency order is unchanged, and each service still passes its health gate
// before the next starts; a service that depends on a DIFFERENT stack's database
// may now come up before it. That failure is visible and recoverable — the health
// gate reports it and the stack restore re-checks stragglers once everything is
// up — where an atomic-set refusal was neither.
func nodeRestoreUnits(plan []runbookService) []nodeRestoreUnit {
	var units []nodeRestoreUnit
	at := map[string]int{} // stack -> index in units
	for _, svc := range plan {
		if svc.Stack == "" {
			units = append(units, nodeRestoreUnit{Services: []runbookService{svc}})
			continue
		}
		if i, ok := at[svc.Stack]; ok {
			units[i].Services = append(units[i].Services, svc)
			continue
		}
		at[svc.Stack] = len(units)
		units = append(units, nodeRestoreUnit{Stack: svc.Stack, Services: []runbookService{svc}})
	}
	return units
}

// unitLabel names one step for the log.
func unitLabel(u nodeRestoreUnit) string {
	if u.Stack != "" {
		return "stack " + u.Stack
	}
	return serviceLabel(u.Services[0])
}

// runNodeRestore executes the plan in order, aborting on the first failure so a
// broken database restore never leaves dependent apps started against it.
func (s *Server) runNodeRestore(ctx context.Context, run nodeRestoreRun) {
	logID, plan := run.LogID, run.Plan
	// F211: what is NOT being restored, said FIRST — before the log fills with the
	// services that are. An operator reading this afterwards must not have to
	// infer an omission from a list they never see the end of.
	if len(run.Skipped) > 0 {
		s.logSink(logID, "WARN", fmt.Sprintf("Leaving out %d service(s) that a whole-node restore cannot handle: %s — restore those individually", len(run.Skipped), strings.Join(run.Skipped, ", ")))
	}
	if run.TargetID != run.SourceID {
		s.logSink(logID, "INFO", fmt.Sprintf("Rebuilding onto %q — every service is recreated there; the original node is not touched", run.TargetName))
	}

	onto := ""
	if run.TargetID != run.SourceID {
		onto = " onto " + run.TargetName
	}
	units := nodeRestoreUnits(plan)
	names := make([]string, len(units))
	for i, u := range units {
		names[i] = unitLabel(u)
	}
	s.logSink(logID, "INFO", fmt.Sprintf("Whole-node restore of %q — %d service(s) in %d step(s), databases first", run.TargetName, len(plan), len(units)))
	// F227: say what is NOT in this run, in the log the operator watches. A run
	// that covers four of five services must never read like one that covered the
	// node.
	if len(run.Deselected) > 0 {
		s.logSink(logID, "INFO", fmt.Sprintf("Not in this run — %d service(s) left as they are: %s", len(run.Deselected), strings.Join(run.Deselected, ", ")))
	}
	s.logSink(logID, "INFO", "Restore order: "+strings.Join(names, " → "))

	for i, u := range units {
		label := unitLabel(u)
		// Operator canceled between steps: stop before starting another
		// destructive restore. What's already restored stays up and is named.
		if ctx.Err() != nil {
			msg := fmt.Sprintf("Node restore CANCELED after %d of %d step(s) — those are up; the rest were NOT touched", i, len(units))
			s.logSink(logID, "WARN", msg)
			s.publishRunDone(logID, runOutcomeCanceled, msg)
			return
		}
		// F212: name the destination on EVERY step of a rebuild, not only in the
		// header. Somebody watching a twelve-step rebuild scroll past should never
		// have to page back to be sure which machine it is landing on — and on a
		// same-node restore the suffix is omitted, so that log reads exactly as it
		// did before this existed.
		s.logSink(logID, "INFO", fmt.Sprintf("[%d/%d] Restoring %s%s…", i+1, len(units), label, onto))

		var err error
		if u.Stack != "" {
			err = s.restoreNodeStackUnit(ctx, run, u)
		} else {
			err = s.restoreNodeServiceUnit(ctx, run, u.Services[0])
		}
		switch {
		case err == nil:
			s.logSink(logID, "INFO", fmt.Sprintf("[%d/%d] %s restored%s", i+1, len(units), label, onto))
		case errors.Is(err, backup.ErrRestoreCanceled):
			// A cancel is the operator's decision, not a failure — report it as
			// such (and don't fire a FAILED alert for something they asked for).
			msg := fmt.Sprintf("[%d/%d] %s: %v — node restore stopped here; the %d step(s) before it are up", i+1, len(units), label, err, i)
			s.logSink(logID, "WARN", msg)
			s.publishRunDone(logID, runOutcomeCanceled, msg)
			return
		default:
			msg := fmt.Sprintf("[%d/%d] %s failed: %v — aborting node restore", i+1, len(units), label, err)
			s.logSink(logID, "ERR", msg)
			s.publishRunDone(logID, runOutcomeFailed, msg)
			s.notify(notify.KindRestoreFailed, "Node restore FAILED: "+run.TargetName,
				fmt.Sprintf("Whole-node restore of %s stopped at %s: %v. Services already restored are up; the rest were NOT restored.", run.TargetName, label, err))
			return
		}
	}
	done := fmt.Sprintf("Node %q restored — all %d service(s) are back up in the proven order", run.TargetName, len(plan))
	s.logSink(logID, "INFO", done)
	// #N10: the console stops guessing from the wording of the line above.
	s.publishRunDone(logID, runOutcomeOK, done)
}

// restoreNodeStackUnit restores one compose project through the stack path, so
// it gets the dependency order, the atomic-set handling and the single merged
// compose file that a service-by-service loop cannot produce (F212).
func (s *Server) restoreNodeStackUnit(ctx context.Context, run nodeRestoreRun, u nodeRestoreUnit) error {
	sopts := backup.StackRestoreOptions{
		Recreate: true, Snapshot: true,
		ReconstructHost: run.Cross.ReconstructHost,
		HostBaseDir:     run.Cross.HostBaseDir,
		RemapFromIP:     run.Cross.RemapFromIP,
		RemapToIP:       run.Cross.RemapToIP,
		RemapFromDomain: run.Cross.RemapFromDomain,
		RemapToDomain:   run.Cross.RemapToDomain,
		RemapFromPath:   run.Cross.RemapFromPath,
		RemapToPath:     run.Cross.RemapToPath,
		PrivateKey:      run.PrivateKey,
	}
	// F212: a path remap asked for with no explicit source base is resolved from
	// THIS stack's own recorded bind layout — one machine's projects do not share
	// a base directory, and guessing one for all of them would rewrite paths that
	// were never under it. A stack whose layout cannot be read keeps its paths
	// rather than failing the rebuild: an un-remapped stack is recoverable, an
	// aborted rebuild at step four is not.
	if run.Cross.PathRemapPending {
		from := s.stackRecordedBase(run.SourceID, u.Stack)
		if from == "" {
			s.logSink(run.LogID, "WARN", fmt.Sprintf("Stack %q records no base directory — its bind paths are left as they are", u.Stack))
		} else if f, t, verr := validateRemapPathBases(from, run.Cross.RemapToPath); verr != nil {
			s.logSink(run.LogID, "WARN", fmt.Sprintf("Stack %q: path remap skipped — %v", u.Stack, verr))
		} else {
			sopts.RemapFromPath, sopts.RemapToPath = f, t
		}
	}
	return s.engine.RestoreStack(ctx, run.SourceID, run.TargetID, u.Stack, sopts)
}

// restoreNodeServiceUnit restores one standalone container — the same path a
// manual restore uses, so it keeps the health gate and the safety snapshot.
func (s *Server) restoreNodeServiceUnit(ctx context.Context, run nodeRestoreRun, svc runbookService) error {
	// F211: a last-moment sanity check only. Everything knowable was decided
	// before the run started; this catches a backup id that vanished since.
	//
	// It does not demand a container id: a standalone-volume backup has none by
	// construction, and Engine.Restore dispatches a "volume:" target to the volume
	// path, which needs no container. Requiring one here is what made a single
	// orphan volume abort an entire node's disaster recovery part-way through.
	if svc.backupID == "" {
		return fmt.Errorf("no restorable backup recorded")
	}
	return s.engine.Restore(ctx, backup.RestoreOptions{
		BackupID: svc.backupID, NodeID: run.TargetID, TargetID: svc.containerID,
		Volumes: true, Database: true, Recreate: true, Snapshot: true,
		ReconstructHost: run.Cross.ReconstructHost,
		HostBaseDir:     run.Cross.HostBaseDir,
		RemapFromIP:     run.Cross.RemapFromIP,
		RemapToIP:       run.Cross.RemapToIP,
		RemapFromDomain: run.Cross.RemapFromDomain,
		RemapToDomain:   run.Cross.RemapToDomain,
		RemapFromPath:   run.Cross.RemapFromPath,
		RemapToPath:     run.Cross.RemapToPath,
		PrivateKey:      run.PrivateKey,
	})
}

// serviceLabel is the human name for a service in the node log (stack/container
// when part of a stack, else the container name).
func serviceLabel(svc runbookService) string {
	if svc.Stack != "" {
		return svc.Stack + "/" + svc.Container
	}
	return svc.Container
}
