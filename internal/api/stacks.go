package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"dockback/internal/backup"
	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// handleListStacks lists the compose projects on a node with their service and
// backup counts (PLAN §4.8 stack DR).
func (s *Server) handleListStacks(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cli, err := s.reg.Get(id)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	cs, err := dockercli.ListContainers(ctx, cli)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	stacks := dockercli.GroupStacks(cs)

	// Count backed-up services per stack from the catalog.
	backups, _ := s.store.ListBackups(id, 10000)
	backedServices := map[string]map[string]bool{} // stack -> set of target names
	for _, b := range backups {
		if b.Stack == "" || b.Status != "success" {
			continue
		}
		if backedServices[b.Stack] == nil {
			backedServices[b.Stack] = map[string]bool{}
		}
		backedServices[b.Stack][b.TargetName] = true
	}

	type stackView struct {
		Name     string `json:"name"`
		Services int    `json:"services"`
		Running  int    `json:"running"`
		BackedUp int    `json:"backed_up"`
	}
	out := make([]stackView, 0, len(stacks))
	for _, st := range stacks {
		out = append(out, stackView{
			Name:     st.Name,
			Services: st.Total,
			Running:  st.Running,
			BackedUp: len(backedServices[st.Name]),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleBackupStack backs up every container in a compose project as one
// consistent stack run. The destination set comes from the request body when
// the caller chose one (DestinationsExplicit), otherwise it falls back to the
// global policy destinations. Local is always kept regardless.
func (s *Server) handleBackupStack(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	project := r.PathValue("project")

	// Optional per-run destination selection (nil body / nil field = use policy)
	// and an optional per-run pause mode applied to the whole stack. An empty
	// pause_mode leaves each service to its remembered per-container setting; a
	// valid one ("none"/"pause"/"stop") forces consistency for this run. Databases
	// are never paused regardless — the engine dumps them live (PLAN §4.1/§4.2).
	var req struct {
		Destinations *[]string `json:"destinations"`
		PauseMode    string    `json:"pause_mode"`
		// Consistent (F33) captures the whole stack as one app-consistent snapshot:
		// the app tier is quiesced while every service's DB dump + volumes are
		// captured together, tagged with a shared consistency group. Off => the
		// default concurrent per-service path (byte-for-byte unchanged).
		Consistent bool `json:"consistent"`
		// Compression (F79) is an optional per-run override applied to every
		// service. Empty = each service's remembered manual choice (falling back
		// to balanced), exactly like a scheduled run.
		Compression string `json:"compression"`
	}
	if r.ContentLength != 0 {
		if err := readJSON(r, &req); err != nil {
			errJSON(w, http.StatusBadRequest, "invalid request")
			return
		}
	}
	pauseMode := ""
	if req.PauseMode != "" && backup.ValidPauseMode(req.PauseMode) {
		pauseMode = req.PauseMode
	}
	if !backup.ValidCompression(req.Compression) {
		errJSON(w, http.StatusBadRequest, "invalid compression")
		return
	}

	node, err := s.store.GetNode(id)
	if err != nil {
		errJSON(w, http.StatusNotFound, "node not found")
		return
	}
	cli, err := s.reg.Get(id)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	cs, err := dockercli.ListContainers(ctx, cli)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}

	// Honor this node's per-node destination override (e.g. a Synology node that
	// shouldn't copy back to the same Synology box), falling back to the global
	// default. An explicit per-run selection below still wins.
	dests := s.effectivePolicy(id).Destinations
	if req.Destinations != nil {
		dests = *req.Destinations
	}

	// F83: capture each shared host bind exactly once per stack run — the owner
	// service keeps it; every other service's selection drops it for THIS run
	// only. Both paths (consistent + fan-out) use the same per-service override.
	var members []*dockercli.Container
	for _, c := range cs {
		if c.Stack == project {
			members = append(members, c)
		}
	}
	dedupInc, owners, coverLogs := s.sharedBindDedup(id, members)
	for _, line := range coverLogs {
		s.logSink("stack:"+project, "INFO", line)
	}
	applyDedup := func(opts backup.Options, name string) backup.Options {
		if inc, ok := dedupInc[name]; ok {
			opts.IncludeMounts = inc
			opts.SelectionEphemeral = true // never overwrite the remembered selection
		}
		return opts
	}

	// F33: an app-consistent snapshot coordinates the whole stack under one
	// quiesce window, so it runs as a single stack-exclusive operation (like a
	// stack restore) rather than the concurrent per-service fan-out below.
	if req.Consistent {
		if !s.startConsistentStackBackup(id, project, dests, pauseMode, func(cid, name string) backup.Options {
			// F79: each service resolves its own remembered options; the request's
			// compression/pause-mode (validated above) overlay them per run.
			// F83: non-owners of a shared bind get a one-run selection without it.
			return applyDedup(s.stackServiceBackupOptions(id, cid, name, dests, req.Compression, pauseMode), name)
		}) {
			errJSON(w, http.StatusConflict, "a backup or restore of this stack is already in progress — try again once it finishes")
			return
		}
		_ = s.store.Audit(userFrom(r), "stack.backup.consistent", project, node.Name)
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "started", "consistent": true})
		return
	}

	// F83: enqueue shared-bind OWNERS first — the queue is FIFO within a
	// priority, so the owner's fresh capture exists (or is underway) before a
	// non-owner's skip is annotated as covered.
	ordered := make([]*dockercli.Container, 0, len(members))
	for _, c := range members {
		if owners[c.Name] {
			ordered = append(ordered, c)
		}
	}
	for _, c := range members {
		if !owners[c.Name] {
			ordered = append(ordered, c)
		}
	}
	count := 0
	for _, c := range ordered {
		// F79: same per-service merge as a scheduled run — remembered
		// compression/app-export/save-image, with the request's per-run overrides.
		// F83: non-owners of a shared bind get a one-run selection without it.
		s.runBackupAsync(node.Name, applyDedup(s.stackServiceBackupOptions(id, c.ID, c.Name, dests, req.Compression, pauseMode), c.Name))
		count++
	}
	if count == 0 {
		errJSON(w, http.StatusNotFound, "no containers found for that stack")
		return
	}
	s.logSink("stack:"+project, "INFO", "Backing up stack with "+itoa(count)+" service(s)")
	_ = s.store.Audit(userFrom(r), "stack.backup", project, node.Name)
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "started", "count": count})
}

// startConsistentStackBackup takes the stack-exclusive lock and runs the
// app-consistent snapshot in the background. Returns false when the lock is
// already held — the caller decides how to say so.
//
// F220: factored out of handleBackupStack so "Protect this stack" starts exactly
// the same run the Backup Stack button does. Two code paths that both quiesce a
// live application under one window is precisely where a divergence would be
// expensive and invisible.
func (s *Server) startConsistentStackBackup(nodeID, project string, dests []string, pauseMode string, perService func(cid, name string) backup.Options) bool {
	lockKey := stackKey(nodeID, project, "")
	if !s.locks.acquireRestore(lockKey) {
		return false
	}
	s.logSink("stack:"+project, "INFO", "Starting app-consistent snapshot of the stack")
	go func() {
		defer guardPanic("stack consistent backup", "stack:"+project, func() {
			s.logSink("stack:"+project, "ERR", "App-consistent snapshot failed: internal error (panic)")
		})
		defer s.releaseRestoreAndDispatch(lockKey)
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
		defer cancel()
		if err := s.engine.BackupStackConsistent(ctx, nodeID, project, backup.Options{
			NodeID: nodeID, Compression: "balanced",
			Destinations: dests, DestinationsExplicit: true, PauseMode: pauseMode,
		}, perService); err != nil {
			s.logSink("stack:"+project, "ERR", "App-consistent snapshot: "+err.Error())
		}
	}()
	return true
}

// crossRestoreInput is the raw, unvalidated cross-host restore request as it
// arrives — from query parameters on the stack path, from a JSON body on the
// whole-node path (F212). Kept separate from the validated result so the two
// entry points share ONE grammar: these values decide where bind mounts land and
// which addresses are rewritten inside recreated containers, and two copies of
// that validation drifting apart is exactly how a "protected system path" check
// stops covering one of the doors.
type crossRestoreInput struct {
	ReconstructHost bool
	HostBaseDir     string
	RemapIP         bool
	RemapFromIP     string
	RemapToIP       string
	RemapDomain     bool
	RemapFromDomain string
	RemapToDomain   string
	RemapPath       bool
	RemapFromPath   string
	RemapToPath     string
}

// crossRestoreParams is the validated, engine-ready result.
type crossRestoreParams struct {
	ReconstructHost                bool
	HostBaseDir                    string
	RemapFromIP, RemapToIP         string
	RemapFromDomain, RemapToDomain string
	RemapFromPath, RemapToPath     string
	// PathRemapPending marks a path remap that was asked for with no explicit
	// SOURCE base. Only the whole-node path can produce this: a machine hosts
	// several stacks and each records its own layout, so the base is resolved per
	// stack at execution time. The stack path always resolves it here.
	PathRemapPending bool
}

// parseCrossRestoreParams validates one cross-host restore request (F212).
//
// project scopes the path-remap source base: a stack name resolves it here from
// that project's recorded bind layout, and "" (the whole-node path) defers it,
// because one machine's stacks do not share one base directory.
//
// Every refusal is returned rather than written, so both callers can answer in
// their own shape — and every one of them happens BEFORE any lock is taken, so a
// malformed request costs nothing.
func (s *Server) parseCrossRestoreParams(ctx context.Context, in crossRestoreInput, sourceID, targetID, project string) (crossRestoreParams, error) {
	out := crossRestoreParams{ReconstructHost: in.ReconstructHost}

	// Opt-in host-side reconstruction: remember/reuse the base directory used for
	// a non-compose container's folder so the operator needn't retype it.
	if in.ReconstructHost {
		if v := strings.TrimSpace(in.HostBaseDir); v != "" && strings.HasPrefix(v, "/") {
			out.HostBaseDir = v
			_ = s.store.SetSetting("restore.host_base_dir", v)
		} else {
			out.HostBaseDir, _ = s.store.GetSetting("restore.host_base_dir", "")
		}
	}

	// Host-IP remap: source/target machine IPs, defaulting to the node addresses.
	if in.RemapIP {
		out.RemapFromIP = strings.TrimSpace(in.RemapFromIP)
		if out.RemapFromIP == "" {
			if src, err := s.store.GetNode(sourceID); err == nil {
				// F197: a node registered by hostname is resolved — the machine's
				// IP is known either way.
				out.RemapFromIP = dockercli.ResolveHostIP(ctx, src.Address)
			}
		}
		out.RemapToIP = strings.TrimSpace(in.RemapToIP)
		if out.RemapToIP == "" {
			if tgt, err := s.store.GetNode(targetID); err == nil {
				out.RemapToIP = dockercli.ResolveHostIP(ctx, tgt.Address)
			}
		}
		if (out.RemapFromIP != "" && net.ParseIP(out.RemapFromIP) == nil) || (out.RemapToIP != "" && net.ParseIP(out.RemapToIP) == nil) {
			return out, fmt.Errorf("IP remap needs valid IPv4/IPv6 addresses")
		}
		// F197: an opt-in remap with an underivable endpoint is a refusal, not a
		// silent no-op — the engine skips the remap when either side is empty.
		if out.RemapFromIP == "" || out.RemapToIP == "" {
			return out, fmt.Errorf("IP remap: could not derive the source or target machine IP from the node addresses — fill both fields in the dialog")
		}
	}

	// Domain remap (F195): the literal from -> to rewrite, plain hostnames only.
	if in.RemapDomain {
		out.RemapFromDomain = strings.TrimSpace(in.RemapFromDomain)
		out.RemapToDomain = strings.TrimSpace(in.RemapToDomain)
		if err := dockercli.ValidRemapDomain(out.RemapFromDomain); err != nil {
			return out, err
		}
		if err := dockercli.ValidRemapDomain(out.RemapToDomain); err != nil {
			return out, err
		}
	}

	// Host-path remap (F81): move bind sources + compose + stack folder from the
	// source machine's base directory to the target's. The TARGET base is
	// validated fail-closed — it must never be, or live under, a system root.
	if in.RemapPath {
		out.RemapFromPath = strings.TrimSpace(in.RemapFromPath)
		if out.RemapFromPath == "" && project != "" {
			out.RemapFromPath = s.stackRecordedBase(sourceID, project)
		}
		out.RemapToPath = strings.TrimSpace(in.RemapToPath)
		if out.RemapToPath == "" {
			out.RemapToPath, _ = s.store.GetSetting("restore.host_base_dir", "")
		}
		if out.RemapFromPath == "" {
			// Whole-node path with nothing given: resolved per stack later. The
			// TARGET base is still checked now, because it is the same for every
			// stack and a bad one must not be discovered mid-run.
			if project != "" {
				// A DIFFERENT failure from a badly-typed base, and it used to share
				// that message: nothing was typed, and the stack's own base could
				// not be derived from the catalog either.
				return out, fmt.Errorf("path remap: leave \"From base folder\" blank and DockBack derives it from the stack's recorded bind paths — but no successful backup of %q carries one. Fill it in with the base directory the stack lived under on the source machine (e.g. /volume1/docker)", project)
			}
			if _, _, verr := validateRemapPathBases("/placeholder-source", out.RemapToPath); verr != nil {
				return out, verr
			}
			out.PathRemapPending = true
			return out, nil
		}
		var verr error
		out.RemapFromPath, out.RemapToPath, verr = validateRemapPathBases(out.RemapFromPath, out.RemapToPath)
		if verr != nil {
			return out, verr
		}
	}
	return out, nil
}

// crossRestoreMemory is the set of cross-host choices an operator made for one
// route — one project, one target machine (F215).
//
// Restoring the same stack onto the same machine twice used to mean typing the
// same domain, the same base directory and the same address a second time. The
// IP remap already derived itself from the node addresses and the host base
// directory was already remembered globally; the domain, the paths and the
// addresses were remembered nowhere, which is exactly the set nothing can derive.
//
// PLAIN SETTINGS STORAGE IS CORRECT HERE, and it is worth saying why rather than
// assuming it. Every field is a hostname, an IP, or a filesystem path — the same
// class as restore.host_base_dir, which has been stored this way since F81. The
// address fields cannot smuggle a credential either: ValidSiteAddress accepts no
// userinfo, so "https://user:pass@host" never validates and so is never stored.
// The offline private key is deliberately absent from this struct, and a test
// asserts it never reaches the settings table.
type crossRestoreMemory struct {
	RemapIP         bool   `json:"remap_ip,omitempty"`
	RemapFromIP     string `json:"remap_from_ip,omitempty"`
	RemapToIP       string `json:"remap_to_ip,omitempty"`
	RemapDomain     bool   `json:"remap_domain,omitempty"`
	RemapFromDomain string `json:"remap_from_domain,omitempty"`
	RemapToDomain   string `json:"remap_to_domain,omitempty"`
	RemapPath       bool   `json:"remap_path,omitempty"`
	RemapFromPath   string `json:"remap_from_path,omitempty"`
	RemapToPath     string `json:"remap_to_path,omitempty"`
	ReconstructHost bool   `json:"reconstruct_host,omitempty"`
	HostBaseDir     string `json:"host_base_dir,omitempty"`
	// The two address changes (F114/F160) — the fields most worth remembering,
	// because nothing can derive them and they are the ones people retype.
	NewSiteAddress     string `json:"new_site_address,omitempty"`
	NewUpstreamAddress string `json:"new_upstream_address,omitempty"`
	At                 int64  `json:"at,omitempty"`
}

// any reports whether this memory holds anything worth remembering, so a plain
// same-host restore never writes a settings row.
func (m crossRestoreMemory) any() bool {
	return m.RemapIP || m.RemapDomain || m.RemapPath || m.ReconstructHost ||
		m.NewSiteAddress != "" || m.NewUpstreamAddress != "" || m.HostBaseDir != ""
}

// crossRestoreMemoryKey names one route's remembered choices: this project, onto
// this machine. Always under the same prefix, so it can never collide with a
// setting in another namespace whatever a project is called.
func crossRestoreMemoryKey(project, targetNodeID string) string {
	return "restore.remaps." + project + "." + targetNodeID
}

// rememberCrossRestore stores the choices for this route (F215). Best-effort: a
// failed write costs the operator one retyping, never the restore.
func (s *Server) rememberCrossRestore(project, targetNodeID string, m crossRestoreMemory) {
	if project == "" || targetNodeID == "" || !m.any() {
		return
	}
	m.At = time.Now().Unix()
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	_ = s.store.SetSetting(crossRestoreMemoryKey(project, targetNodeID), string(b))
}

// handleStackRestoreDefaults returns the choices last used for this route, so
// the dialog can offer them instead of asking again (F215).
//
// Read-only and idempotent: auth-gated, not CSRF-gated, like the other
// pre-restore reads. An unset route answers with an empty object rather than a
// 404 — "nothing remembered" is a normal state, not an error.
func (s *Server) handleStackRestoreDefaults(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	project := r.PathValue("project")
	if _, err := s.store.GetNode(id); err != nil {
		errJSON(w, http.StatusNotFound, "node not found")
		return
	}
	target := strings.TrimSpace(r.URL.Query().Get("target_node"))
	if target == "" {
		target = id
	}
	var m crossRestoreMemory
	if v, _ := s.store.GetSetting(crossRestoreMemoryKey(project, target), ""); strings.TrimSpace(v) != "" {
		_ = json.Unmarshal([]byte(v), &m)
	}
	writeJSON(w, http.StatusOK, m)
}

// missingMembersRefusal is the answer to a stack restore that would leave
// members without a backup behind, when the caller has not confirmed that. ""
// lets it proceed.
func missingMembersRefusal(missing []string, confirmed bool) string {
	if len(missing) == 0 || confirmed {
		return ""
	}
	return fmt.Sprintf("%d member(s) of this stack have no backup and would not exist after this restore: %s. Back them up first, or confirm to restore the stack without them.",
		len(missing), strings.Join(missing, ", "))
}

// stackMissingMembers names the stack's members that have NO backup, and so
// cannot be recreated by a restore (F216).
//
// Computed from the CACHED inventory, deliberately. The dialog used to derive
// this from a live container list, which fails exactly when it matters: a source
// node that is down answers nothing, the list comes back empty, and a restore
// missing three services looks complete in the one situation — the machine is
// gone — that the warning exists for. The server has known better all along; it
// simply never said so until after the run had started.
//
// Returned sorted, so the same gap reads the same way twice. The inventory's own
// order is Docker's, which is not stable between calls.
//
// Always non-nil, so the plan serializes "missing_members": [] rather than null —
// the same rule nodeRestorePlan states for its own list. A caller iterating the
// field must not have to special-case the healthy answer, and "no gap" is the
// commonest answer there is.
func (s *Server) stackMissingMembers(sourceID, project string) []string {
	missing := []string{}
	st := s.getStat(sourceID)
	if st == nil {
		// Never polled: no inventory to judge against, so claim nothing. NOT the
		// same as "nothing is missing" — this reports no gap because none is
		// known, which is why it must never be inferred from an empty inventory.
		return missing
	}
	covered := map[string]bool{}
	for _, svc := range s.stackServiceUniverse(sourceID, project) {
		covered[svc] = true
	}
	seen := map[string]bool{}
	for _, c := range st.Containers {
		if c == nil || c.Stack != project {
			continue
		}
		svc := c.Service
		if svc == "" {
			svc = c.Name
		}
		if covered[svc] || covered[c.Name] || seen[svc] {
			continue
		}
		seen[svc] = true
		missing = append(missing, svc)
	}
	sort.Strings(missing)
	return missing
}

// stackSourceCopy is one copy the stack's members can be read from (F214), with
// how many of them actually have it — so the picker can say "3 of 5 services"
// instead of implying a destination covers the whole stack.
type stackSourceCopy struct {
	ID       string `json:"id"`   // "local", or a destination id
	Name     string `json:"name"` // display name
	Type     string `json:"type"`
	Services int    `json:"services"` // members holding a copy here
}

// stackSourceCopies collects the copies the planned members actually hold,
// newest-first stable order: local first (it is the fast default), then
// destinations by name (F214). getBackup is injected so the collection is
// testable without a store.
func stackSourceCopies(e *backup.Engine, entries []backup.StackPlanEntry, getBackup func(string) *store.Backup) []stackSourceCopy {
	type agg struct {
		name, typ string
		n         int
	}
	byID := map[string]*agg{}
	for _, entry := range entries {
		b := getBackup(entry.BackupID)
		if b == nil {
			continue
		}
		for _, loc := range e.LocationsOf(b) {
			// A copy that never uploaded is not somewhere to read from.
			if loc.Status == "failed" || loc.Status == "deferred" {
				continue
			}
			id := loc.DestID
			if loc.Kind == "local" {
				id = "local"
			}
			if id == "" {
				continue
			}
			a := byID[id]
			if a == nil {
				a = &agg{name: loc.Name, typ: loc.Type}
				byID[id] = a
			}
			a.n++
		}
	}
	out := make([]stackSourceCopy, 0, len(byID))
	for id, a := range byID {
		out = append(out, stackSourceCopy{ID: id, Name: a.name, Type: a.typ, Services: a.n})
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].ID == "local") != (out[j].ID == "local") {
			return out[i].ID == "local" // local first — it is the fast default
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// validateRemapPathBases vets the F81 path-remap bases: both must be absolute
// and cleaned, neither may be "/", they must differ, and the TARGET base may
// never be (or live under) a protected system root — the same list write-time
// validation enforces (dockercli.ForbiddenHostRoot), fail-closed at the API.
func validateRemapPathBases(from, to string) (string, string, error) {
	f := path.Clean(strings.TrimSpace(from))
	t := path.Clean(strings.TrimSpace(to))
	// Name the field and echo the value. The first version of this said only
	// "needs two absolute base directories" for BOTH bases and for a third,
	// unrelated cause below — so an operator whose "to" was missing one leading
	// slash had no way to tell which of three things the dialog meant.
	if err := absoluteBase("The \"From base folder\"", from, f); err != nil {
		return "", "", err
	}
	if err := absoluteBase("The \"To base folder\"", to, t); err != nil {
		return "", "", err
	}
	if f == "/" || t == "/" {
		return "", "", fmt.Errorf("path remap refuses / as a base — it would rewrite every absolute path")
	}
	if f == t {
		return "", "", fmt.Errorf("path remap bases are identical (%s) — nothing to remap", f)
	}
	if dockercli.ForbiddenHostRoot(t) {
		return "", "", fmt.Errorf("path remap target %q is a protected system path — choose a data directory like /opt/stacks", t)
	}
	return f, t, nil
}

// absoluteBase rejects a remap base that is not an absolute path, naming the
// field and quoting what was actually received.
//
// The cleaned form is what the check runs on, but the RAW value is what gets
// quoted back: an operator who typed "home/user/docker" needs to see their own
// text with the fix pointed at, not a normalised version of it.
func absoluteBase(field, raw, cleaned string) error {
	if strings.HasPrefix(cleaned, "/") {
		return nil
	}
	got := strings.TrimSpace(raw)
	if got == "" {
		return fmt.Errorf("%s is empty — path remap needs an absolute base directory on both sides (e.g. /opt/docker → /opt/stacks)", field)
	}
	return fmt.Errorf("%s is %q, which is not an absolute path — it has to start with a slash (did you mean %q?)", field, got, "/"+strings.TrimLeft(got, "/"))
}

// stackRecordedBase derives the stack's original base directory — the parent of
// the newest successful backup's recorded compose working dir — used to prefill
// the F81 remap "from" base when the caller leaves it blank.
func (s *Server) stackRecordedBase(nodeID, project string) string {
	backups, err := s.store.ListBackupsForStack(nodeID, project, 5000) // newest first
	if err != nil {
		return ""
	}
	// F192: derive the base from where the stack's DATA lives, not from the
	// compose working directory. They are the same for a hand-run compose
	// project and different for one deployed through a management tool: Portainer
	// runs compose from its own /data/compose/<numeric id>, while the bind
	// mounts the operator actually cares about live under something like
	// /volume1/docker/<project>. A from-base taken from the working dir then
	// matches no bind at all — the remap silently rewrites nothing, and Docker
	// recreates the old machine's layout on the new host the moment the
	// container starts.
	votes := map[string]int{}
	workingBase := ""
	seen := 0
	for _, b := range backups {
		if b.Status != "success" || b.ManifestJSON == "" {
			continue
		}
		var man backup.Manifest
		if json.Unmarshal([]byte(b.ManifestJSON), &man) != nil {
			continue
		}
		if workingBase == "" && man.StackWorkingDir != "" {
			workingBase = path.Dir(man.StackWorkingDir)
		}
		for _, base := range backup.BindBasesForProject(man.Volumes, project) {
			votes[base]++
		}
		// One manifest per service is enough; newest first means the freshest
		// layout wins the vote.
		seen++
		if seen >= 32 {
			break
		}
	}
	best, n := "", 0
	for base, c := range votes {
		if c > n || (c == n && base < best) {
			best, n = base, c
		}
	}
	if best != "" {
		return best
	}
	return workingBase
}

// sharedBindDedup plans one-capture-per-shared-bind for a stack run (F83).
// For every host bind Source mounted by two or more members, a deterministic
// owner is chosen (backup.SharedBindOwner precedence); every OTHER member with
// a STORED mount selection gets a one-run selection with that bind removed.
// Returns (name -> one-run IncludeMounts, owner-name set, log lines).
//
// Members without a stored selection are left on the size-based default: the
// engine persists the computed default after their first run, so in practice
// this covers everything after one backup — and forcing a selection here would
// require size scans of every service just to start the run.
func (s *Server) sharedBindDedup(nodeID string, members []*dockercli.Container) (map[string][]string, map[string]bool, []string) {
	selList := map[string][]string{}
	selSet := map[string]map[string]bool{}
	for _, c := range members {
		if sel, ok := s.engine.MountSelection(nodeID, c.Name); ok {
			selList[c.Name] = sel
			m := make(map[string]bool, len(sel))
			for _, d := range sel {
				m[d] = true
			}
			selSet[c.Name] = m
		}
	}
	cover := backup.CoverageMap(members, func(name string) map[string]bool { return selSet[name] })
	if len(cover) == 0 {
		return nil, nil, nil
	}

	out := map[string][]string{}
	owners := map[string]bool{}
	var logs []string
	// Deterministic source order so the log lines are stable run to run.
	sources := make([]string, 0, len(cover))
	for src := range cover {
		sources = append(sources, src)
	}
	sort.Strings(sources)
	for _, src := range sources {
		owner := cover[src]
		owners[owner] = true
		var skippedIn []string
		for _, c := range members {
			if c.Name == owner || selSet[c.Name] == nil {
				continue // no stored selection: default-skipped or first-run — annotation still covers it
			}
			for _, m := range c.Mounts {
				if m.Type != "bind" || m.Source != src || !selSet[c.Name][m.Destination] {
					continue
				}
				cur := out[c.Name]
				if cur == nil {
					cur = selList[c.Name]
				}
				filtered := make([]string, 0, len(cur))
				for _, d := range cur {
					if d != m.Destination {
						filtered = append(filtered, d)
					}
				}
				out[c.Name] = filtered
				skippedIn = append(skippedIn, c.Name)
			}
		}
		if len(skippedIn) > 0 {
			logs = append(logs, fmt.Sprintf("Shared bind %s: captured once via %s; skipped in %s (covered)", src, owner, strings.Join(skippedIn, ", ")))
		}
	}
	return out, owners, logs
}

// stackServiceBackupOptions resolves one stack member's per-run Options (F79):
// the same remembered-manual-choices merge a scheduled run uses
// (scheduledBackupOptions — compression / app-native export / save-image, keyed
// by container NAME), overlaid with the stack run's optional per-run overrides.
// Empty compression/pauseMode = the service's own remembered setting. Mounts,
// incremental, hooks, and pause resolution stay engine-side, as everywhere else.
func (s *Server) stackServiceBackupOptions(nodeID, containerID, name string, dests []string, compression, pauseMode string) backup.Options {
	opts := s.scheduledBackupOptions(nodeID, containerID, name, dests)
	if compression != "" {
		opts.Compression = compression
		// F84: the stack dialog has a real "no choice" option (""), so ANY
		// picked value — balanced included — is deliberate and suppresses
		// autotune for this run.
		opts.CompressionExplicit = true
	}
	if pauseMode != "" {
		opts.PauseMode = pauseMode
	}
	return opts
}

// handleRestoreStack rebuilds an entire compose project in dependency order.
func (s *Server) handleRestoreStack(w http.ResponseWriter, r *http.Request) {
	// The path node is the SOURCE — the node the stack's backups are cataloged
	// under. The restore TARGET may differ (cross-host DR): it comes from the
	// target_node query param and defaults to the source for an in-place restore.
	sourceID := r.PathValue("id")
	project := r.PathValue("project")
	if _, err := s.store.GetNode(sourceID); err != nil {
		errJSON(w, http.StatusNotFound, "source node not found")
		return
	}
	q := r.URL.Query()
	targetID := strings.TrimSpace(q.Get("target_node"))
	if targetID == "" {
		targetID = sourceID
	} else if targetID != sourceID {
		if _, err := s.store.GetNode(targetID); err != nil {
			errJSON(w, http.StatusNotFound, "target node not found")
			return
		}
	}
	// "Revert update": recreate each service from its backed-up image digest.
	recreate := q.Get("recreate") == "true" || q.Get("recreate") == "1"
	// Step 22: run a newer image when the one the backup ran is gone — only when
	// asked, because the data may be migrated beyond going back.
	allowDifferentImage := q.Get("allow_different_image") == "true" || q.Get("allow_different_image") == "1"
	// Step 23: put back the stack's files only — no service stopped or changed.
	filesOnly := q.Get("files_only") == "true" || q.Get("files_only") == "1"
	if filesOnly && recreate {
		errJSON(w, http.StatusBadRequest, "a files-only restore changes no service, so it cannot also revert one")
		return
	}
	// #8: opt-in promotion of a restart policy that will not survive a reboot.
	promoteRestart := q.Get("promote_restart_policy") == "true" || q.Get("promote_restart_policy") == "1"
	injectHealthchecks := q.Get("inject_healthchecks") == "true" || q.Get("inject_healthchecks") == "1"
	snapshot := stackRestoreSnapshotWanted(q)
	// Optional app-consistent snapshot group (F43): restore every service from one
	// coherent point-in-time instead of newest-per-service. Empty = default.
	groupID := strings.TrimSpace(q.Get("group"))
	// F212: every cross-host option goes through ONE validated grammar, shared
	// with the whole-node rebuild. These decide where bind mounts land and which
	// addresses are rewritten inside recreated containers; two copies of that
	// validation drifting apart is how a protected-system-path check stops
	// covering one of the doors.
	cross, cerr := s.parseCrossRestoreParams(r.Context(), crossRestoreInput{
		ReconstructHost: q.Get("reconstruct_host") == "true" || q.Get("reconstruct_host") == "1",
		HostBaseDir:     q.Get("host_base_dir"),
		RemapIP:         q.Get("remap_ip") == "true" || q.Get("remap_ip") == "1",
		RemapFromIP:     q.Get("remap_from_ip"),
		RemapToIP:       q.Get("remap_to_ip"),
		RemapDomain:     q.Get("remap_domain") == "true" || q.Get("remap_domain") == "1",
		RemapFromDomain: q.Get("remap_from_domain"),
		RemapToDomain:   q.Get("remap_to_domain"),
		RemapPath:       q.Get("remap_path") == "true" || q.Get("remap_path") == "1",
		RemapFromPath:   q.Get("remap_from_path"),
		RemapToPath:     q.Get("remap_to_path"),
	}, sourceID, targetID, project)
	if cerr != nil {
		errJSON(w, http.StatusBadRequest, cerr.Error())
		return
	}
	reconstructHost, hostBaseDir := cross.ReconstructHost, cross.HostBaseDir
	remapFrom, remapTo := cross.RemapFromIP, cross.RemapToIP
	remapFromDomain, remapToDomain := cross.RemapFromDomain, cross.RemapToDomain
	remapFromPath, remapToPath := cross.RemapFromPath, cross.RemapToPath

	// F173: the two optional address changes, validated up front — before the
	// lock, before anything is stopped — because a malformed value must cost
	// nothing, and because these end up in a trust list and in commands run
	// inside a container. Blank is the normal case for both.
	//
	// The single-service dialog has accepted these since they were added; the
	// stack dialog did not, which is the wrong way round: the applications that
	// need an address change on a move are the multi-service ones.
	newSiteAddress := strings.TrimSpace(q.Get("new_site_address"))
	newUpstreamAddress := strings.TrimSpace(q.Get("new_upstream_address"))
	for _, v := range []string{newSiteAddress, newUpstreamAddress} {
		if v == "" {
			continue
		}
		if err := backup.ValidSiteAddress(v); err != nil {
			errJSON(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	// F209: the offline private key for the stack's write-only members arrives in
	// the BODY, not the query string.
	//
	// Every other option on this handler is a query param and stays one — but a
	// URL is written to the access log of every proxy in front of this app, kept
	// in browser history, and sent in Referer headers. A private key that unlocks
	// the entire backup history has no business in any of them. The body is
	// optional, so a caller that sends none is byte-for-byte unchanged.
	var body struct {
		// F210: the step-up credentials for a protected member ride the same body,
		// for the same reason the key does — a password has no business in a URL.
		stepUpBody
		PrivateKey string `json:"private_key"`
	}
	if r.ContentLength != 0 {
		if err := readJSON(r, &body); err != nil {
			errJSON(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	privateKey := strings.TrimSpace(body.PrivateKey)

	// F210: fresh proof of the password before overwriting a PROTECTED member.
	//
	// The single-container restore has demanded this since F206; restoring the
	// same container as part of its stack demanded nothing, which made the stack
	// dialog a way around the gate rather than a bigger version of it.
	//
	// The member set comes from the PLAN — the same selection the restore will
	// execute — rather than from the live inventory, because a node that has not
	// been polled yet reports no containers, and an empty member list would
	// silently skip a security check. Resolved against the TARGET node: only what
	// is there can be overwritten.
	//
	// Placed before the locks below, so a declined prompt leaves nothing held.
	// F213: which services this restore covers. Repeated `service=` params, in
	// the query like every other selector on this handler. Empty is the default
	// and means the whole stack, so an existing caller is unchanged.
	selectedServices := []string(nil)
	for _, v := range q["service"] {
		if v = strings.TrimSpace(v); v != "" {
			selectedServices = append(selectedServices, v)
		}
	}

	// F214: which COPY every member is read from. Validated here, before the lock
	// — an unknown destination id must be a refusal, not a silent fallback to
	// auto: bestLocation simply matches nothing and reads local, so the operator
	// would believe they had restored from the offsite copy when they had not.
	// That is the exact failure this picker exists to prevent.
	source := strings.TrimSpace(q.Get("source"))
	if source != "" && source != "local" {
		if _, derr := s.store.GetDestination(source); derr != nil {
			errJSON(w, http.StatusBadRequest, "unknown source copy — choose Auto, the local copy, or a configured destination")
			return
		}
	}

	// F216: a member with no backup cannot come back, and the run still ends
	// "restored" — commafeed came back without its database. The dialog asks;
	// the server insists, so no caller can restore an incomplete stack without
	// saying so. Before the step-up and the locks, so a refusal costs nothing.
	missing := s.stackMissingMembers(sourceID, project)
	// A files-only restore touches no service, so a member without a backup is
	// not left behind by it.
	confirmedMissing := filesOnly || q.Get("confirm_missing") == "true" || q.Get("confirm_missing") == "1"
	if refusal := missingMembersRefusal(missing, confirmedMissing); refusal != "" {
		errJSON(w, http.StatusConflict, refusal)
		return
	}

	planned, _, perr := s.engine.PlanStack(sourceID, project, groupID)
	if perr != nil {
		// The same selection error the plan endpoint returns and the restore would
		// have raised in its goroutine. Surfaced here instead, because a request
		// whose member set cannot be determined must not proceed past this gate.
		errJSON(w, http.StatusBadRequest, perr.Error())
		return
	}
	// Step 27: the same for a shared network the target does not have.
	if !filesOnly && q.Get("confirm_missing_networks") != "true" && q.Get("confirm_missing_networks") != "1" {
		seen := map[string]bool{}
		var lackingNets []string
		for _, p := range planned {
			if b, gerr := s.store.GetBackup(p.BackupID); gerr == nil {
				for _, n := range s.sharedNetworksMissingOn(r.Context(), manifestOf(b), project, b.NodeID, targetID) {
					if !seen[n] {
						seen[n] = true
						lackingNets = append(lackingNets, n)
					}
				}
			}
		}
		if len(lackingNets) > 0 {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":            "the target machine has no " + strings.Join(lackingNets, ", ") + " network, which this stack joins but does not own. DockBack would create it as a plain bridge, cutting the services off from what they reach through it. Create it on the target as it should be, or confirm the plain bridge.",
				"missing_networks": lackingNets,
			})
			return
		}
	}
	// Step 27: a device the target verifiably lacks stops the restore before
	// any service is touched, unless the operator says it will be there.
	if !filesOnly && q.Get("confirm_missing_devices") != "true" && q.Get("confirm_missing_devices") != "1" {
		var lacking []string
		for _, p := range planned {
			if b, gerr := s.store.GetBackup(p.BackupID); gerr == nil {
				for _, d := range s.missingDevicesOn(r.Context(), manifestOf(b), b.NodeID, targetID) {
					lacking = append(lacking, p.Service+": "+d)
				}
			}
		}
		if len(lacking) > 0 {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":           "the target machine lacks devices these services need — " + strings.Join(lacking, ", ") + ". They would be created and then fail to start. Attach the devices, or confirm that they will be there.",
				"missing_devices": lacking,
			})
			return
		}
	}
	// F213: the protected-member check follows the SELECTION. A service the
	// operator deselected is not overwritten, so it must not be what makes them
	// prove their password — the gate exists for what this run destroys.
	keep := map[string]bool{}
	for _, name := range selectedServices {
		keep[name] = true
	}
	memberNames := make([]string, 0, len(planned))
	for _, p := range planned {
		if len(keep) > 0 && !keep[p.Service] && !keep[p.TargetName] {
			continue
		}
		memberNames = append(memberNames, p.TargetName)
	}
	protected := s.protectedRestoreMembers(targetID, memberNames)
	if len(protected) > 0 && !filesOnly && !s.requireFreshAuth(w, r, body.stepUpBody) {
		return
	}

	// Stack-exclusive locks (PLAN §9.10 / F52): hold the project on the TARGET node
	// (where the destructive recreate happens) AND, for a cross-node migration, on
	// the SOURCE node (whose catalog we read) — so a backup or restore of this stack
	// on EITHER node can't race the migration. A same-node restore holds one lock.
	targetLock := stackKey(targetID, project, "")
	if !s.locks.acquireRestore(targetLock) {
		errJSON(w, http.StatusConflict, "a backup or restore of this stack is already in progress — try again once it finishes")
		return
	}
	sourceLock := ""
	if sourceID != targetID {
		sourceLock = stackKey(sourceID, project, "")
		if !s.locks.acquireRestore(sourceLock) {
			s.releaseRestoreAndDispatch(targetLock)
			errJSON(w, http.StatusConflict, "a backup or restore of this stack is already in progress on the source node — try again once it finishes")
			return
		}
	}
	action := "stack.restore"
	if recreate {
		action = "stack.revert"
	}
	// F209: the FACT that a key was supplied, never the key. The audit trail is
	// readable by any admin and is exported inside app backups — the one place an
	// offline recovery key must never appear. Same wording the single-service
	// restore uses.
	detail := "source=" + sourceID + " target=" + targetID
	if privateKey != "" {
		detail += " (write-only key supplied)"
	}
	if len(selectedServices) > 0 {
		detail += " (services: " + strings.Join(selectedServices, ", ") + ")" // F213
	}
	if source != "" {
		detail += " (source: " + source + ")" // F214
	}
	detail += stepUpDetail(protected) // F210 — parity with the single-container restore
	// F215: remember what this route needed, so the next restore of this project
	// onto this machine offers it instead of asking again. Written here — after
	// every value has been validated and before anything is started — so a
	// refused request never leaves a remembered choice behind. The offline key is
	// deliberately not part of this.
	s.rememberCrossRestore(project, targetID, crossRestoreMemory{
		RemapIP: remapFrom != "" && remapTo != "", RemapFromIP: remapFrom, RemapToIP: remapTo,
		RemapDomain: remapFromDomain != "" && remapToDomain != "", RemapFromDomain: remapFromDomain, RemapToDomain: remapToDomain,
		RemapPath: remapFromPath != "" && remapToPath != "", RemapFromPath: remapFromPath, RemapToPath: remapToPath,
		ReconstructHost: reconstructHost, HostBaseDir: hostBaseDir,
		NewSiteAddress: newSiteAddress, NewUpstreamAddress: newUpstreamAddress,
	})
	_ = s.store.Audit(userFrom(r), action, project, detail)
	go func() {
		defer guardPanic("stack restore", "stack:"+project, func() { s.logSink("stack:"+project, "ERR", "Stack restore failed: internal error (panic)") })
		defer s.releaseRestoreAndDispatch(targetLock)
		if sourceLock != "" {
			defer s.releaseRestoreAndDispatch(sourceLock)
		}
		// ALL-services guarantee check: a restore can only recreate what was
		// captured. If the source stack (live or cached inventory) has members
		// with NO backup, say so LOUDLY up front — never let a service be
		// silently absent from the restored stack. Non-blocking: DR must still
		// run when the source machine is gone.
		if len(missing) > 0 {
			s.logSink("stack:"+project, "WARN", fmt.Sprintf("%d stack member(s) have NO backup and will NOT be restored: %s — confirmed by the operator. Run a stack backup to cover every service", len(missing), strings.Join(missing, ", ")))
		}
		// Cancelable under the stack's own run id (what the UI streams).
		runID := "stack:" + project
		ctx, finish, ok := s.beginRestoreRun(context.Background(), runID, "stack "+project, targetID, 3*time.Hour)
		if !ok {
			s.logSink(runID, "ERR", "Stack restore failed: another restore of this stack is already running")
			return
		}
		defer finish()
		err := s.engine.RestoreStack(ctx, sourceID, targetID, project, backup.StackRestoreOptions{Recreate: recreate, Snapshot: snapshot, ReconstructHost: reconstructHost, HostBaseDir: hostBaseDir, PromoteRestartPolicy: promoteRestart, InjectHealthchecks: injectHealthchecks, RemapFromIP: remapFrom, RemapToIP: remapTo, RemapFromPath: remapFromPath, RemapToPath: remapToPath, GroupID: groupID,
			RemapFromDomain: remapFromDomain, RemapToDomain: remapToDomain, // F195
			NewSiteAddress: newSiteAddress, NewUpstreamAddress: newUpstreamAddress,
			PrivateKey:          privateKey, // F209 — in memory for this restore only
			Services:            selectedServices,
			MissingMembers:      missing,
			AllowDifferentImage: allowDifferentImage,
			FilesOnly:           filesOnly,
			Source:              source}) // F214 — read every member from the same copy
		s.restoreOutcome(runID, err, "Stack restore completed")
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

// handleStackGroups returns the app-consistent snapshot groups available to
// restore a stack from (F43), each flagged complete when it covers every service
// the stack has a backup for. Read-only; returns [] for stacks that were never
// captured with an app-consistent snapshot, so the UI simply keeps its default
// newest-per-service flow.
func (s *Server) handleStackGroups(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	project := r.PathValue("project")
	if _, err := s.store.GetNode(id); err != nil {
		errJSON(w, http.StatusNotFound, "node not found")
		return
	}
	writeJSON(w, http.StatusOK, s.engine.StackGroups(id, project, s.stackServiceUniverse(id, project)))
}

// stackServiceOptions is one row of the stack-backup panel (F80): a service's
// EFFECTIVE per-container backup settings — the same values the container's own
// page reads and writes, so the panel and the page can never disagree.
type stackServiceOptions struct {
	ContainerID     string                    `json:"container_id"`
	Name            string                    `json:"name"`
	Service         string                    `json:"service,omitempty"`
	State           string                    `json:"state"`
	IsDatabase      bool                      `json:"is_database"`
	Engine          string                    `json:"engine,omitempty"`
	PauseMode       string                    `json:"pause_mode"`
	BackupOptions   backup.SavedBackupOptions `json:"backup_options"`
	MountsSelected  int                       `json:"mounts_selected"` // -1 = no stored selection (size-based default)
	MountsTotal     int                       `json:"mounts_total"`
	ExportAvailable bool                      `json:"export_available"`
	CoveredBinds    int                       `json:"covered_binds,omitempty"` // F83: candidate binds captured by another container
	// VersionSkew (#23) explains, in one sentence, that this service shares a
	// directory with another container running a different build of the same
	// image. Built here rather than in the panel so the sentence has one home.
	VersionSkew string `json:"version_skew,omitempty"`
}

// stackServiceOptionsFor builds one panel row from the SAME settings reads the
// container page uses (PauseModeKey / BackupOptionsKey / mount selection).
// Pure aside from store/engine reads — unit-tested against a seeded store.
// skews carries the whole project's version disagreements, computed once by the
// caller: the question is about a PAIR of containers, so no single row can
// answer it on its own. Nil is a project with none, and the row simply says
// nothing.
func (s *Server) stackServiceOptionsFor(nodeID string, c *dockercli.Container, skews []backup.SharedMountSkew) stackServiceOptions {
	eng := backup.DBEngine(c.Image)
	row := stackServiceOptions{
		ContainerID: c.ID, Name: c.Name, Service: c.Service, State: c.State,
		IsDatabase: eng != "", Engine: eng,
		MountsSelected:  -1,
		MountsTotal:     len(backup.CandidateMountDests(c.Mounts)),
		ExportAvailable: s.engine.ExportProfileFor(c.Image, nodeID, c.Name).Available,
	}
	row.PauseMode, _ = s.store.GetSetting(backup.PauseModeKey(nodeID, c.Name), backup.PausePause)
	if js, _ := s.store.GetSetting(backup.BackupOptionsKey(nodeID, c.Name), ""); js != "" {
		_ = json.Unmarshal([]byte(js), &row.BackupOptions)
	}
	if row.BackupOptions.Compression == "" {
		row.BackupOptions.Compression = "balanced"
	}
	if sel, ok := s.engine.MountSelection(nodeID, c.Name); ok {
		row.MountsSelected = len(sel)
	}
	// F83: shared binds another container's recent backups already capture.
	hasBind := false
	for _, m := range c.Mounts {
		if m.Type == "bind" {
			hasBind = true
			break
		}
	}
	if hasBind {
		cov := s.engine.CoveredSources(nodeID, c.Name)
		seen := map[string]bool{}
		for _, m := range c.Mounts {
			if m.Type == "bind" && m.Source != "" && !seen[m.Source] && cov[m.Source] != "" {
				seen[m.Source] = true
				row.CoveredBinds++
			}
		}
	}
	// #23: a shared directory whose mounters run different builds of the same
	// image. The same sentence the backup's finding carries, so the panel and
	// the report cannot describe the same fact two different ways.
	for _, skew := range skews {
		if slices.Contains(skew.Containers(), c.Name) {
			row.VersionSkew = skew.Describe()
			break
		}
	}
	return row
}

// handleStackOptions (F80) lists every project container with its effective
// per-container backup settings, for the stack-backup panel. Read-only.
func (s *Server) handleStackOptions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	project := r.PathValue("project")
	if _, err := s.store.GetNode(id); err != nil {
		errJSON(w, http.StatusNotFound, "node not found")
		return
	}
	cli, err := s.reg.Get(id)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	cs, err := dockercli.ListContainers(ctx, cli)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	var members []*dockercli.Container
	for _, c := range cs {
		if c.Stack == project {
			members = append(members, c)
		}
	}
	// #23: computed once over the whole project — a version disagreement is a
	// fact about a pair, so it cannot be derived from one row.
	skews := backup.SharedMountSkews(members)
	out := []stackServiceOptions{}
	for _, c := range members {
		out = append(out, s.stackServiceOptionsFor(id, c, skews))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleStackRestorePlan (F82) returns the read-only pre-restore plan: which
// backup each service restores from and in what order — computed by the exact
// selection/ordering RestoreStack executes — plus, when host reconstruction is
// requested, the resolved target folder per service (shared ResolveStackDir, so
// preview and reality cannot diverge). Extends the handleStackGroups pre-flight
// pattern; touches nothing.
func (s *Server) handleStackRestorePlan(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	project := r.PathValue("project")
	if _, err := s.store.GetNode(id); err != nil {
		errJSON(w, http.StatusNotFound, "node not found")
		return
	}
	q := r.URL.Query()
	entries, atomicBlock, err := s.engine.PlanStack(id, project, strings.TrimSpace(q.Get("group")))
	if err != nil {
		// A group missing a service is the client-facing selection error
		// RestoreStack would raise — surface it the same way, pre-confirm.
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	// F174: a database whose environment cannot initialise an empty data
	// directory. Said here because the restore that discovers it may be service
	// three of four, and by then the ones before it have already been restored.
	// Unlike the portability loop below this applies to a SAME-host restore too:
	// what it depends on is the container's own configuration, not the target.
	for i := range entries {
		b, gerr := s.store.GetBackup(entries[i].BackupID)
		if gerr != nil || b.ManifestJSON == "" {
			continue
		}
		var m backup.Manifest
		if json.Unmarshal([]byte(b.ManifestJSON), &m) != nil {
			continue
		}
		entries[i].RestoreBlock = backup.DBInitBlock(&m)
	}

	// F214: the copies these members ACTUALLY have, so the source picker offers
	// real choices instead of every configured destination.
	//
	// The distinction is the point. Members of one stack do not necessarily share
	// a destination — one may have been mirrored offsite and another not — and a
	// picker fed from the destination list would happily offer one where half the
	// stack has no copy at all. Choosing it makes bestLocation fall back to local
	// for those members, silently, which is precisely what somebody reaching for
	// "restore from the offsite copy, not this disk" is trying to avoid. The
	// per-destination count rides along so the dialog can say "3 of 5 services"
	// rather than implying it covers everything.
	sourceCopies := stackSourceCopies(s.engine, entries, func(bid string) *store.Backup {
		b, gerr := s.store.GetBackup(bid)
		if gerr != nil {
			return nil
		}
		return b
	})

	// F94: cross-host portability, per service. Host facts are resolved ONCE and
	// reused for every entry — a stack of twelve services must not mean twelve
	// probes of the same host.
	var facts *hostFacts
	for i := range entries {
		b, gerr := s.store.GetBackup(entries[i].BackupID)
		if gerr != nil || b.NodeID == id || b.ManifestJSON == "" {
			continue // same-host restore, or nothing to judge
		}
		var m backup.Manifest
		if json.Unmarshal([]byte(b.ManifestJSON), &m) != nil {
			continue
		}
		// F177 does not need host facts, so it is answered before the probe and
		// regardless of whether this service declares any host requirements.
		entries[i].AddressVars = backup.AddressEnvKeys(&m)
		if m.Requires == nil {
			continue
		}
		if facts == nil {
			f := s.targetHostFacts(r.Context(), id)
			facts = &f
		}
		entries[i].Portability = portabilityWarnings(m.Requires, *facts)
		entries[i].MissingDevices = missingDevices(m.Requires, *facts)
	}

	// F81: what the target does not have yet, and what the restore will do about
	// it. Computed with the remap resolved LENIENTLY — this panel refreshes while
	// the operator is still typing a path, and a half-typed one should show no
	// plan rather than turn the dialog into an error.
	if bindFrom, bindTo, ok := s.plannedRemap(q, id, project); ok {
		s.attachBindPlans(r.Context(), q, id, entries, bindFrom, bindTo)
	}

	targetDirs := map[string]string{}
	if q.Get("reconstruct_host") == "true" || q.Get("reconstruct_host") == "1" {
		hostBase := strings.TrimSpace(q.Get("host_base_dir"))
		if hostBase == "" {
			hostBase, _ = s.store.GetSetting("restore.host_base_dir", "")
		}
		fromPath, toPath := "", ""
		if q.Get("remap_path") == "true" || q.Get("remap_path") == "1" {
			fromPath = strings.TrimSpace(q.Get("remap_from_path"))
			if fromPath == "" {
				fromPath = s.stackRecordedBase(id, project)
			}
			toPath = strings.TrimSpace(q.Get("remap_to_path"))
			if toPath == "" {
				toPath, _ = s.store.GetSetting("restore.host_base_dir", "")
			}
			var verr error
			fromPath, toPath, verr = validateRemapPathBases(fromPath, toPath)
			if verr != nil {
				errJSON(w, http.StatusBadRequest, verr.Error())
				return
			}
		}
		for _, e := range entries {
			if d := backup.ResolveStackDir(e.StackWorkingDir, e.TargetName, hostBase, fromPath, toPath); d != "" {
				targetDirs[e.Service] = d
			}
		}
	}
	// F146: the atomic-set refusal, if this selection would be refused. Shown
	// rather than discovered on confirm — the fix is to pick a different snapshot,
	// which is a choice the operator can only make in this dialog.
	// F110/F149: the anchor application's own preconditions, so the things that
	// decide whether a stack restore actually works — DNS, ports, agents — are in
	// front of the operator before they commit, not only in the per-backup drawer.
	out := map[string]any{
		"services": entries, "target_dirs": targetDirs, "source_copies": sourceCopies,
		// F216: members the restore CANNOT bring back, from the cached inventory —
		// so the dialog shows the gap whether or not the source node answers.
		"missing_members": s.stackMissingMembers(id, project),
	}
	// #37: what this restore will write, and what the destination filesystem has
	// left — reported before the operator commits, not after the bytes land.
	if bindFrom, bindTo, ok := s.plannedRemap(q, id, project); ok {
		hostBase := strings.TrimSpace(q.Get("host_base_dir"))
		if hostBase == "" {
			hostBase, _ = s.store.GetSetting("restore.host_base_dir", "")
		}
		// Sized to what the operator actually kept. The full member list still
		// goes out above so the picker can offer every service, but a verdict
		// about disk space has to describe the set being restored — deselecting
		// half a stack and still being refused for the whole stack's bytes is a
		// refusal the operator cannot act on.
		if capacity := s.restoreCapacity(r.Context(), id, selectedEntries(q, entries), hostBase, bindFrom, bindTo); capacity != nil {
			out["capacity"] = capacity
		}
	}
	if atomicBlock != "" {
		out["atomic_block"] = atomicBlock
	}
	if pre := s.stackAppPreconditions(entries); pre != nil {
		out["app_preconditions"] = pre
	}
	writeJSON(w, http.StatusOK, out)
}

// stackRestoreSnapshotWanted reads the stack restore's "Safety snapshot first"
// box, and honours it on EVERY stack restore.
//
// It used to be honoured only together with "Revert update", so a plain stack
// restore took no snapshot at all while the dialog — ticked by default — said
// "Current state is snapshotted first". That is how a failed restore during a
// real recovery had nothing to roll back to. Absent means yes: the safe default
// for a destructive call made by a script.
func stackRestoreSnapshotWanted(q url.Values) bool {
	v := q.Get("snapshot")
	return v != "false" && v != "0"
}

// selectedEntries narrows a stack plan to the services the operator kept.
//
// An absent or empty "services" parameter means the whole stack, so a client
// that does not send one is unaffected. Unknown names are ignored rather than
// refused: this feeds a read-only panel that refreshes while the selection is
// still being changed.
func selectedEntries(q url.Values, entries []backup.StackPlanEntry) []backup.StackPlanEntry {
	raw := strings.TrimSpace(q.Get("services"))
	if raw == "" {
		return entries
	}
	kept := map[string]bool{}
	for _, name := range strings.Split(raw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			kept[name] = true
		}
	}
	if len(kept) == 0 {
		return entries
	}
	out := make([]backup.StackPlanEntry, 0, len(entries))
	for _, e := range entries {
		if kept[e.Service] {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		return entries
	}
	return out
}

// plannedRemap resolves the host-path remap for the bind plan, leniently.
//
// The strict validator stays where it is, guarding the restore itself and the
// reconstruction preview. This one only decides which paths a READ-ONLY panel
// describes, and it refreshes while the operator is mid-keystroke — so an
// incomplete pair means "no remap yet", never an error dialog. ok is false only
// when the pair is asked for and unusable, which suppresses the plan rather than
// showing one computed from paths the restore would refuse.
func (s *Server) plannedRemap(q url.Values, nodeID, project string) (from, to string, ok bool) {
	if q.Get("remap_path") != "true" && q.Get("remap_path") != "1" {
		return "", "", true
	}
	from = strings.TrimSpace(q.Get("remap_from_path"))
	if from == "" {
		from = s.stackRecordedBase(nodeID, project)
	}
	to = strings.TrimSpace(q.Get("remap_to_path"))
	if to == "" {
		to, _ = s.store.GetSetting("restore.host_base_dir", "")
	}
	from, to, verr := validateRemapPathBases(from, to)
	if verr != nil {
		return "", "", false
	}
	return from, to, true
}

// attachBindPlans fills each service's BindPlan with the host paths the TARGET
// does not have and what the restore will do about them (F81).
//
// One probe for the whole stack, not one per service: the paths are gathered and
// deduplicated first, so a twelve-service project costs the same single sidecar
// as a one-service project. Entirely best-effort — a target that cannot be
// probed leaves every BindPlan empty, and the restore's own materialiser remains
// the thing that decides.
func (s *Server) attachBindPlans(ctx context.Context, q url.Values, sourceNodeID string, entries []backup.StackPlanEntry, fromPath, toPath string) {
	targetNodeID := strings.TrimSpace(q.Get("target_node"))
	if targetNodeID == "" {
		targetNodeID = sourceNodeID
	}
	// Per service: the binds it needs. Gathered before anything is probed so the
	// probe sees the union.
	needed := make(map[string][]dockercli.BindMount, len(entries))
	seen := map[string]bool{}
	var paths []string
	for _, e := range entries {
		b, gerr := s.store.GetBackup(e.BackupID)
		if gerr != nil || b.ManifestJSON == "" {
			continue
		}
		var man backup.Manifest
		if json.Unmarshal([]byte(b.ManifestJSON), &man) != nil {
			continue
		}
		binds := backup.RecordedBindSources(&man, fromPath, toPath)
		if len(binds) == 0 {
			continue
		}
		needed[e.Service] = binds
		for _, bind := range binds {
			if !seen[bind.Source] {
				seen[bind.Source] = true
				paths = append(paths, bind.Source)
			}
		}
	}
	if len(paths) == 0 {
		return
	}
	cli, cerr := s.reg.Get(targetNodeID)
	if cerr != nil {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, stackBindProbeTimeout)
	defer cancel()
	kinds, perr := dockercli.ProbeHostPaths(pctx, cli, paths)
	if perr != nil {
		return // nothing measured, so nothing claimed
	}

	for i := range entries {
		binds := needed[entries[i].Service]
		if len(binds) == 0 {
			continue
		}
		var missing []dockercli.BindMount
		for _, bind := range binds {
			if kinds[bind.Source] == dockercli.HostPathMissing {
				missing = append(missing, bind)
			}
		}
		if len(missing) == 0 {
			continue
		}
		b, gerr := s.store.GetBackup(entries[i].BackupID)
		if gerr != nil {
			continue
		}
		var man backup.Manifest
		if json.Unmarshal([]byte(b.ManifestJSON), &man) != nil {
			continue
		}
		entries[i].BindPlan = backup.PlanBindSources(&man, missing)
	}
}

// stackBindProbeTimeout bounds the pre-restore bind probe. It stats a handful of
// paths in one sidecar; longer than this is a stuck node, and a dialog must not
// wait on one.
const stackBindProbeTimeout = 45 * time.Second

// stackServiceUniverse is the set of service names a stack has a successful backup
// for, from the catalog — the same universe RestoreStack requires a group to cover,
// so StackGroups' "complete" flag agrees exactly with which groups can restore the
// whole stack. Catalog-derived (no live node round-trip) so it works offline and
// cross-host.
func (s *Server) stackServiceUniverse(id, project string) []string {
	backups, err := s.store.ListBackupsForStack(id, project, 10000)
	if err != nil {
		return nil
	}
	set := map[string]bool{}
	for _, b := range backups {
		if b.Status != "success" {
			continue
		}
		var man backup.Manifest
		_ = json.Unmarshal([]byte(b.ManifestJSON), &man)
		svc := man.Service
		if svc == "" {
			svc = b.TargetName
		}
		if svc != "" {
			set[svc] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// stackAppPreconditions returns the preconditions of the application a stack
// anchors (F149), or nil for a stack that anchors none — which is nearly all.
//
// A stack restore is where a multi-service application's real preconditions
// matter most, and until now they were only ever shown in the single-backup
// drawer, which is the dialog an operator restoring a whole stack never opens.
// Manifest-derived, so it costs nothing and needs no node call.
func (s *Server) stackAppPreconditions(entries []backup.StackPlanEntry) *backup.AppRestorePreconditions {
	for _, e := range entries {
		b, err := s.store.GetBackup(e.BackupID)
		if err != nil || b.ManifestJSON == "" {
			continue
		}
		var m backup.Manifest
		if json.Unmarshal([]byte(b.ManifestJSON), &m) != nil {
			continue
		}
		if pre := backup.AppPreconditionsFor(&m); pre != nil {
			return pre
		}
	}
	return nil
}
