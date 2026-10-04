package backup

import (
	"context"
	"errors"
	"fmt"
	"go.yaml.in/yaml/v3"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// parseDependsOn extracts service names from a compose `depends_on` label, e.g.
// "mariadb:service_started:false,redis:service_healthy:false" -> [mariadb redis].
func parseDependsOn(label string) []string {
	if label == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(label, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, strings.SplitN(part, ":", 2)[0])
	}
	return out
}

func isDataImage(image string) bool {
	// Match the REPOSITORY only — an app image's TAG frequently names the DB
	// backend it talks to ("athou/commafeed:latest-postgresql"), which is not
	// a database.
	repo := imageRepository(image)
	for _, k := range []string{"postgres", "mysql", "mariadb", "mongo", "redis", "valkey", "memcached"} {
		if strings.Contains(repo, k) {
			return true
		}
	}
	return false
}

// isDataServiceName is the last-resort data-tier hint: the compose service or
// container NAME says it's a database ("postgresql", "app-db", "mariadb", …).
// Needed because the two stronger signals can both miss — a DB captured while
// stopped (or without its dump CLI) stores raw files, so its manifest has no
// dumps, and a custom/renamed image defeats the image heuristic. A false
// positive merely restores a service earlier than strictly needed — harmless;
// a false negative restores an app before its database, which breaks DR.
func isDataServiceName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return false
	}
	for _, k := range []string{"postgres", "postgresql", "mysql", "mariadb", "mongo", "mongodb", "redis", "valkey", "db", "database"} {
		if n == k || strings.HasPrefix(n, k+"-") || strings.HasPrefix(n, k+"_") ||
			strings.HasSuffix(n, "-"+k) || strings.HasSuffix(n, "_"+k) {
			return true
		}
	}
	return strings.HasSuffix(n, "db") // commafeed-db, appdb, influxdb, …
}

// stackService is one service's chosen backup + ordering metadata.
type stackService struct {
	service  string
	backup   *store.Backup
	man      *Manifest
	dataTier bool
	// tierRank orders within the topo layers: 2 = proven database (its backup
	// CONTAINS dumps — ground truth), 1 = looks like a data service (image
	// repo / name heuristics), 0 = application. Ranked, not boolean, so a
	// false-positive hint (an app tagged ":latest-postgresql") can never tie
	// with — or beat — a real database.
	tierRank int
}

// StackRestoreOptions controls a stack-wide restore.
type StackRestoreOptions struct {
	// Recreate turns the restore into a "Revert update": each service is
	// recreated from its backup's image digest (rolling a bad upgrade back to the
	// backed-up version), not just re-filled with data into the current container.
	Recreate bool
	// Snapshot takes a local safety backup of each service's CURRENT state before
	// overwriting it, so a bad revert is itself reversible.
	Snapshot bool
	// ReconstructHost rebuilds each service's on-host stack directory + compose
	// file when it is recreated during the restore (opt-in; DR onto a fresh host).
	ReconstructHost bool
	// PromoteRestartPolicy sets a policy that will not survive a reboot to
	// `unless-stopped` on every restored service (#8). Opt-in.
	PromoteRestartPolicy bool
	// InjectHealthchecks adds a probe to any recreated DATABASE service that has
	// none (#16), so the services waiting on it can wait for readiness rather
	// than for a container to exist. Opt-in, and never over an existing probe.
	InjectHealthchecks bool
	// HostBaseDir is the fallback base directory for non-compose services (see
	// RestoreOptions.HostBaseDir).
	HostBaseDir string
	// RemapFromIP/RemapToIP rewrite the source machine IP to the target machine IP
	// in each recreated service's config + compose (see RestoreOptions).
	RemapFromIP string
	RemapToIP   string

	// RemapFromDomain/RemapToDomain (F195): the literal domain rewrite, applied
	// to every member. See RestoreOptions for the reasoning.
	RemapFromDomain string
	RemapToDomain   string
	// RemapFromPath/RemapToPath (F81) move each recreated service's bind sources,
	// reconstructed compose paths, and stack folder from the source machine's base
	// directory to the target machine's (see RestoreOptions).
	RemapFromPath string
	RemapToPath   string
	// NewSiteAddress and NewUpstreamAddress carry the same two address changes a
	// single-service restore accepts (F114 / F160), through to every member of
	// the stack (F173).
	//
	// They were missing here, and the omission was worst exactly where it mattered
	// most: the applications that need an address change on a move are the
	// multi-service ones, so the stack dialog is the one an operator actually
	// uses for them. Its pre-restore panel said "enter it below" and there was
	// nothing below.
	//
	// Blank is the normal case for both and changes nothing. Each is applied by
	// the per-service restore to whichever service declares the binding — an
	// application's address belongs to its own container, and the members that
	// declare nothing are untouched.
	NewSiteAddress     string
	NewUpstreamAddress string

	// PrivateKey is the offline X25519 private key for the stack's WRITE-ONLY
	// members (F86/F209). Supplied once for the whole restore, held in memory for
	// that operation only, never persisted and never logged — the audit trail
	// records only that a key was supplied, exactly as the single-service restore
	// does.
	//
	// One key for the stack, not one per service, because write-only mode seals
	// every backup to the SAME instance keypair: a stack's members share a
	// keypair by construction, and asking for the same string once per service
	// would be ceremony, not security. A member sealed to a different keypair is
	// caught by the pre-flight check in RestoreStack, which names it.
	PrivateKey string

	// Source picks WHICH COPY every member is read from (F214): "" = auto (the
	// integrity-checked local copy first, then offsite), "local", or a
	// destination id.
	//
	// It existed on a single-backup restore and not here, which had it exactly
	// backwards: a stack is the case where the choice matters most. When the local
	// disk is the thing you distrust — ransomware, a bad controller, a host being
	// rebuilt — "read every service from the offsite copy" is the whole request,
	// and auto would quietly prefer local for any member that still had one.
	//
	// A member with no copy on the chosen destination falls back exactly as
	// bestLocation always has, and says so in its log rather than failing.
	Source string

	// Services, when non-empty, restores ONLY these compose services and leaves
	// every other member of the stack alone (F213). Empty — the default and what
	// every caller before this sent — restores the whole project.
	//
	// It exists for the shape recovery actually takes: a stack restore that
	// stopped part-way leaves some services back and some not, and putting the
	// remaining ones right meant hunting each of their individual backups on the
	// Backups page. The dependency order among the KEPT services is unchanged —
	// this narrows the set, it does not reorder it.
	Services []string

	// AllowDifferentImage is RestoreOptions.AllowDifferentImage for every member.
	AllowDifferentImage bool

	// FilesOnly is RestoreOptions.FilesOnly for the whole stack: its folder,
	// once, and every member's missing single-file binds (step 23).
	FilesOnly bool

	// MissingMembers are the stack's services with no backup at all, which this
	// restore cannot bring back. Like a narrowed Services list, they make the
	// restore cover only part of the project, which decides what may be written
	// as the stack's compose file.
	MissingMembers []string

	// GroupID, when set, restricts selection to backups whose manifest
	// ConsistencyGroup matches — so every service is restored from ONE app-consistent
	// snapshot (F33/F43), a single coherent point-in-time, instead of each service's
	// newest backup (which can span different capture times). Empty = newest per
	// service (unchanged default). A service the stack has but the group lacks is a
	// hard error rather than a silent fall-back to a different point in time.
	GroupID string
}

// StackGroup is one app-consistent snapshot (F33) available to restore a stack
// from: a set of per-service backups captured together in a single quiesce window,
// so restoring from it yields one coherent point-in-time across the whole stack
// (F43). Complete is true only when the group covers every service in the universe
// the caller supplied (the stack's known service set).
type StackGroup struct {
	ID       string   `json:"id"`
	At       int64    `json:"at"`       // shared capture time (unix seconds)
	Services []string `json:"services"` // service names this group covers
	Complete bool     `json:"complete"` // covers every service in the supplied universe
}

// StackGroups returns the app-consistent snapshot groups available for a stack,
// newest first. It buckets the node's successful backups for the project by their
// manifest ConsistencyGroup (untagged backups are ignored, so a stack never
// captured with an app-consistent snapshot yields an empty slice), and marks a
// group Complete when it covers every service in `universe` — the stack's known
// service set, passed in by the caller (the same catalog universe RestoreStack
// requires a group to cover, so a "complete" group here is exactly one that can
// restore the whole stack). Pure (no live Docker) so it is unit-testable.
func (e *Engine) StackGroups(nodeID, project string, universe []string) []StackGroup {
	all, err := e.Store.ListBackupsForStack(nodeID, project, 10000)
	if err != nil {
		return []StackGroup{}
	}
	return stackGroupsFrom(all, project, universe)
}

// stackGroupsFrom is the pure core of StackGroups (no store), so it is unit-tested
// with fabricated backup rows.
func stackGroupsFrom(all []*store.Backup, project string, universe []string) []StackGroup {
	type bucket struct {
		at   int64
		svcs map[string]bool
	}
	groups := map[string]*bucket{}
	for _, b := range all {
		if b.Stack != project || b.Status != "success" {
			continue
		}
		man := &Manifest{}
		_ = unmarshal(b.ManifestJSON, man)
		if man.ConsistencyGroup == "" {
			continue
		}
		g, ok := groups[man.ConsistencyGroup]
		if !ok {
			g = &bucket{svcs: map[string]bool{}}
			groups[man.ConsistencyGroup] = g
		}
		svc := man.Service
		if svc == "" {
			svc = b.TargetName
		}
		g.svcs[svc] = true
		// All members share ConsistencyAt; keep the max, falling back to the backup's
		// own timestamp if the window time was never stamped.
		if man.ConsistencyAt > g.at {
			g.at = man.ConsistencyAt
		}
		if g.at == 0 && b.CreatedAt > g.at {
			g.at = b.CreatedAt
		}
	}

	want := map[string]bool{}
	for _, s := range universe {
		if s != "" {
			want[s] = true
		}
	}
	out := make([]StackGroup, 0, len(groups))
	for id, g := range groups {
		svcs := make([]string, 0, len(g.svcs))
		for s := range g.svcs {
			svcs = append(svcs, s)
		}
		sort.Strings(svcs)
		complete := len(want) > 0
		for u := range want {
			if !g.svcs[u] {
				complete = false
				break
			}
		}
		out = append(out, StackGroup{ID: id, At: g.at, Services: svcs, Complete: complete})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].At != out[j].At {
			return out[i].At > out[j].At // newest first
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// selectStackServicesFrom picks the backup to restore for each service of a stack
// from the node's backup rows — newest success per service by default, or, when
// groupID is set, the member of that one app-consistent snapshot group (F43). It is
// the pure core of RestoreStack's selection (no store, no Docker) so it is
// unit-tested. When restricting to a group, EVERY service the stack has a backup for
// must be present in the group; a missing one is a named error rather than a silent
// fall-back to a different point in time.
func selectStackServicesFrom(all []*store.Backup, project, groupID string) (map[string]*stackService, error) {
	byService := map[string]*stackService{}
	serviceSeen := map[string]bool{} // every service the stack has a successful backup for
	for _, b := range all {
		if b.Stack != project || b.Status != "success" {
			continue
		}
		man := &Manifest{}
		_ = unmarshal(b.ManifestJSON, man)
		svc := man.Service
		if svc == "" {
			svc = b.TargetName
		}
		serviceSeen[svc] = true
		if groupID != "" && man.ConsistencyGroup != groupID {
			continue
		}
		if existing, ok := byService[svc]; ok && existing.backup.CreatedAt >= b.CreatedAt {
			continue // keep the newer one (list is newest-first, so first wins)
		}
		// Data-tier detection drives the restore order (db before app), from
		// three independent signals — each can miss alone (a stopped DB stores
		// raw files with no dumps; a custom image name defeats the regex; a
		// generic name defeats the name hint). RANKED, not OR'd: a backup that
		// actually CONTAINS dumps (ground truth) always outranks a heuristic
		// match, so a false-positive hint can never restore before a real DB.
		rank := 0
		switch {
		case len(man.Databases) > 0:
			rank = 2
		case isDataImage(man.Image), isDataServiceName(svc), isDataServiceName(b.TargetName):
			rank = 1
		}
		byService[svc] = &stackService{service: svc, backup: b, man: man, dataTier: rank > 0, tierRank: rank}
	}
	if groupID != "" {
		for svc := range serviceSeen {
			if _, ok := byService[svc]; !ok {
				return nil, fmt.Errorf("no backup in group %s for service %q", groupID, svc)
			}
		}
	}
	return byService, nil
}

// StackPlanEntry is one row of a stack-restore plan preview (F82): which backup
// a service would restore from and in what order — computed by the SAME
// selection + ordering RestoreStack executes, so the preview cannot diverge
// from the destructive run. TargetName/StackWorkingDir feed the API's target
// folder resolution and are not serialized redundantly.
type StackPlanEntry struct {
	Order       int    `json:"order"`
	Service     string `json:"service"`
	BackupID    string `json:"backup_id"`
	TargetName  string `json:"target_name"`
	CreatedAt   int64  `json:"created_at"`
	Verified    string `json:"verified"`
	DataTier    bool   `json:"data_tier"`
	Partial     bool   `json:"partial"`     // uncovered skips only (F83)
	Incremental bool   `json:"incremental"` // F61 delta chain member
	GroupID     string `json:"group_id,omitempty"`
	// Portability lists what the TARGET host cannot honor for this service on a
	// cross-host restore (F94). Empty when restoring onto the origin node, or
	// when the service asks nothing special of its host.
	Portability []string `json:"portability,omitempty"`
	// MissingDevices are the recorded devices the target verifiably lacks; a
	// restore stops on them until confirmed (step 27).
	MissingDevices []string `json:"missing_devices,omitempty"`

	// RestoreBlock (F174) is a problem that would stop THIS service's restore,
	// known from its manifest before anything is touched. Distinct from
	// Portability above: that is what the target host cannot provide, this is
	// what the service's own configuration cannot survive.
	RestoreBlock string `json:"restore_block,omitempty"`

	// AddressVars (F177) names the environment variables this service records an
	// address in, on a cross-host restore. Names only, from the manifest.
	AddressVars []string `json:"address_vars,omitempty"`

	// BindPlan (F81) is what the restore will do about the host paths this
	// service binds that the TARGET does not have — created and filled, created
	// empty, written from the archive, or left for the operator.
	//
	// Shown before the operator confirms rather than discovered in the run log
	// afterwards, because a path appearing on a host is only reassuring if they
	// were told it was going to. Empty when the target already has everything,
	// and absent for a backup taken before bind roots were recorded, which
	// cannot be planned for at all.
	BindPlan []BindSourcePlan `json:"bind_plan,omitempty"`

	// Atomic (F213) marks a member of an application whose services are only
	// meaningful together (F146). Such a member cannot be deselected — restoring
	// a subset of it produces a healthy-looking deployment that does not work —
	// so the dialog disables its checkbox rather than letting the operator
	// discover the refusal after confirming something destructive.
	Atomic bool `json:"atomic,omitempty"`

	// WriteOnly (F209) marks a member sealed to the offline keypair, so the
	// dialog can ask for the private key BEFORE the operator commits instead of
	// the restore stopping dead at that service with the earlier ones already
	// overwritten.
	WriteOnly bool `json:"write_only,omitempty"`

	// StackWorkingDir is the manifest's recorded compose dir — used by the plan
	// handler to resolve the target folder; not part of the JSON contract.
	StackWorkingDir string `json:"-"`
}

// PlanStack computes the restore plan for a stack without touching anything:
// the exact backups and order RestoreStack would use (selectStackServicesFrom +
// topoOrder — the same calls, same group semantics, same errors).
func (e *Engine) PlanStack(sourceNodeID, project, groupID string) ([]StackPlanEntry, string, error) {
	all, err := e.Store.ListBackupsForStack(sourceNodeID, project, 10000) // newest first
	if err != nil {
		return nil, "", err
	}
	return planStackFrom(all, project, groupID)
}

// planStackFrom is the pure core of PlanStack, unit-tested with fabricated rows.
//
// It returns the SAME atomic verdict RestoreStack would raise (F146) rather than
// an error, so the dialog can show the refusal — and what to pick instead —
// before the operator commits, instead of the plan going blank on them.
func planStackFrom(all []*store.Backup, project, groupID string) ([]StackPlanEntry, string, error) {
	byService, err := selectStackServicesFrom(all, project, groupID)
	if err != nil {
		return nil, "", err
	}
	blocked := ""
	if aerr := StackAtomicGroupVerdict(manifestsOf(byService)); aerr != nil {
		blocked = aerr.Error()
	}
	order := topoOrder(byService)
	out := make([]StackPlanEntry, 0, len(order))
	for i, s := range order {
		out = append(out, StackPlanEntry{
			Order:           i + 1,
			Service:         s.service,
			BackupID:        s.backup.ID,
			TargetName:      s.backup.TargetName,
			CreatedAt:       s.backup.CreatedAt,
			Verified:        s.backup.Verified,
			DataTier:        s.dataTier,
			Partial:         s.man.HasUncoveredSkip(),
			Incremental:     s.man.Incremental,
			GroupID:         s.man.ConsistencyGroup,
			WriteOnly:       IsWriteOnly(s.man),
			Atomic:          s.man != nil && s.man.StackAtomic != nil && len(s.man.StackAtomic.Members) >= 2,
			StackWorkingDir: s.man.StackWorkingDir,
		})
	}
	return out, blocked, nil
}

// RestoreStack rebuilds an entire compose project from the latest successful
// backup of each service, recreating + restoring them in dependency order
// (data services first, the app last), waiting for databases before dependents
// (PLAN §4.8 stack DR). It logs progress under "stack:<project>".
//
// sourceNodeID is the node the backups are CATALOGED under (their origin);
// targetNodeID is the node to restore them ONTO. The two differ for a cross-host
// stack restore (recreate an origin node's stack on a different machine); when
// targetNodeID is empty it defaults to the source, i.e. an in-place restore.
func (e *Engine) RestoreStack(ctx context.Context, sourceNodeID, targetNodeID, project string, sopts StackRestoreOptions) error {
	if targetNodeID == "" {
		targetNodeID = sourceNodeID
	}
	logID := "stack:" + project
	if sopts.Recreate {
		e.logf(logID, "INFO", "Reverting stack %q — rolling each service back to its latest backup (image + data)", project)
	} else {
		e.logf(logID, "INFO", "Restoring stack %q — gathering latest backups per service", project)
	}
	if targetNodeID != sourceNodeID {
		e.logf(logID, "INFO", "Cross-host stack restore: recreating %q on the selected target node (backups read from the origin node)", project)
	}

	// Gather the per-service backups from the SOURCE node (where they're cataloged).
	all, err := e.Store.ListBackupsForStack(sourceNodeID, project, 10000) // newest first
	if err != nil {
		return err
	}
	byService, err := selectStackServicesFrom(all, project, sopts.GroupID)
	if err != nil {
		return err
	}
	if sopts.GroupID != "" {
		e.logf(logID, "INFO", "Restoring from app-consistent snapshot group %s — every service from one coherent point in time", sopts.GroupID)
	}
	if len(byService) == 0 {
		return fmt.Errorf("no backups found for stack %q on the source node", project)
	}
	// F213: narrow to the operator's selection BEFORE any guard runs, so every
	// check below judges what will actually be restored rather than what the
	// stack contains. That ordering is the whole point: the atomic verdict must
	// see the subset to refuse it, and the offline-key check must not demand a
	// key for a member nobody selected.
	if len(sopts.Services) > 0 {
		filtered, ferr := filterStackServices(byService, sopts.Services)
		if ferr != nil {
			e.logf(logID, "ERROR", "Stack restore refused: %v", ferr)
			return ferr
		}
		byService = filtered
		e.logf(logID, "INFO", "Restoring %d of the stack's services — the rest are left exactly as they are", len(byService))
	}
	// F146: an application whose services are only meaningful together must be
	// restored as one unit, from one snapshot. Judged here, before the first
	// service is touched, so a refusal costs nothing. Files-only touches no
	// service, so it is not judged.
	if aerr := StackAtomicGroupVerdict(manifestsOf(byService)); aerr != nil && !sopts.FilesOnly {
		e.logf(logID, "ERROR", "Stack restore refused: %v", aerr)
		return aerr
	}
	// F209: and the offline key, for the same reason and in the same place.
	//
	// Before this, a stack with one write-only member restored the app services,
	// then stopped dead at that member with "supply the offline private key" —
	// leaving a half-restored stack and no way to supply it, because the stack
	// restore had no field to put it in. The key is now checked against EVERY
	// write-only member up front, so a missing or wrong one costs zero services.
	if kerr := checkStackPrivateKey(byService, sopts.PrivateKey); kerr != nil {
		e.logf(logID, "ERROR", "Stack restore refused: %v", kerr)
		return kerr
	}

	if sopts.FilesOnly {
		return e.restoreStackFilesOnly(ctx, targetNodeID, project, topoOrder(byService), sopts, logID)
	}

	e.fillDependsOnFromCompose(ctx, byService, sopts.Source, logID)
	order := topoOrder(byService)
	names := make([]string, len(order))
	for i, s := range order {
		names[i] = s.service
		// Observability: name the tier decision + dependency edges so a wrong
		// order is diagnosable from the run log, not by reading source.
		e.logf(logID, "INFO", "Order input %q: tier=%d (2=has dumps, 1=looks like a data service, 0=app; dumps=%d, image=%q) depends_on=%v",
			s.service, s.tierRank, len(s.man.Databases), s.man.Image, s.man.DependsOn)
	}
	e.logf(logID, "INFO", "Restore order: %s", strings.Join(names, " → "))

	// A service that restored fully but failed only its HEALTH GATE doesn't
	// abort the stack: on a fresh host it frequently becomes healthy once the
	// services after it (or its late-ordered dependency) exist.
	//
	// A real failure doesn't abort it either. It used to: in a real recovery a
	// one-shot helper was restored first, failed, and the six services that
	// mattered were never attempted. Now only the services that DEPEND on a
	// failed one are skipped — they would come up against nothing — and the rest
	// are restored. The run ends with what happened to each.
	var unhealthy []*stackService
	failed := map[string]error{}
	skipped := map[string]string{}
	for i, s := range order {
		// Operator canceled between services: stop cleanly here rather than start
		// another destructive service restore. Everything already restored stays.
		if ctx.Err() != nil {
			e.logf(logID, "WARN", "Stack restore CANCELED after %d of %d service(s) — the services restored so far are in place; the rest were not touched", i, len(order))
			return fmt.Errorf("%w after %d of %d service(s)", ErrRestoreCanceled, i, len(order))
		}
		if dep := failedDependency(s, failed, skipped); dep != "" {
			skipped[s.service] = dep
			e.logf(logID, "WARN", "[%d/%d] Skipping service %q — it depends on %q, which did not restore", i+1, len(order), s.service, dep)
			continue
		}
		e.logf(logID, "INFO", "[%d/%d] Restoring service %q (%s)…", i+1, len(order), s.service, s.backup.TargetName)
		memberOpts := stackServiceRestoreOptions(s, targetNodeID, sopts)
		memberOpts.SnapshotLogID = logID
		err := e.Restore(ctx, memberOpts)
		switch {
		case errors.Is(err, ErrRestoreCanceled):
			e.logf(logID, "WARN", "[%d/%d] Service %q: %v — stopping the stack restore here", i+1, len(order), s.service, err)
			return fmt.Errorf("service %q: %w", s.service, err)
		case err == nil:
			e.logf(logID, "INFO", "[%d/%d] Service %q restored", i+1, len(order), s.service)
		case errors.Is(err, ErrRestoreUnhealthy):
			e.logf(logID, "WARN", "[%d/%d] Service %q restored but not healthy yet — continuing with the remaining services and re-checking it afterwards", i+1, len(order), s.service)
			unhealthy = append(unhealthy, s)
		default:
			failed[s.service] = err
			e.logf(logID, "ERR", "[%d/%d] Service %q failed: %v — carrying on with the services that do not depend on it", i+1, len(order), s.service, err)
		}
	}

	// F190: one compose file for the whole project, written once now that every
	// service is in place — not one per service into the same path.
	if sopts.ReconstructHost {
		// F213: a partial restore writes no file rebuilt from its subset —
		// reconstructStackCompose decides that, because the operator's own file
		// describes the whole project and is still worth writing.
		e.reconstructStackCompose(ctx, targetNodeID, project, order, sopts, logID)
	} else if len(order) > 0 {
		// Said once for the stack rather than once per service: the files are one
		// set, in one folder, and the operator makes one decision about them.
		e.noteUnwrittenOriginals(logID, order[0].man, false)
	}

	// Final pass: with the whole stack present, re-check the stragglers by NAME
	// (a recreate gives the container a new ID, but keeps its original name).
	if len(unhealthy) > 0 {
		cli, cerr := e.Reg.Get(targetNodeID)
		var still []string
		reasons := map[string]string{}
		for _, s := range unhealthy {
			name := s.backup.TargetName
			recovered := false
			if cerr == nil {
				if id, found := dockercli.FindContainerByName(ctx, cli, name); found {
					recovered = dockercli.WaitForHealthy(ctx, cli, id, e.restoreHealthTimeout(targetNodeID, name))
				}
			}
			if recovered {
				e.logf(logID, "INFO", "Service %q became healthy once the rest of the stack was up", s.service)
				continue
			}
			still = append(still, s.service)
			// F187: the application's own last words, HERE, at the moment the
			// verdict is final. They were printed once during the per-service
			// attempt, several services and often several minutes earlier — by the
			// time the run ends with a list of names, the line that explains it has
			// scrolled past. The reason a service did not come up is the whole
			// content of this failure, so it is repeated where the failure is
			// announced, and the first line of it rides along in the error itself.
			if cerr == nil {
				if id, found := dockercli.FindContainerByName(ctx, cli, name); found {
					if excerpt := e.restoreForensics(ctx, cli, s.backup, id, name); excerpt != "" && reasons[s.service] == "" {
						reasons[s.service] = firstStatement(excerpt)
					}
				}
			}
		}
		if len(still) > 0 {
			e.logf(logID, "ERR", "Stack %q restored, but %d service(s) did not become healthy: %s — investigate them; their data IS restored", project, len(still), strings.Join(still, ", "))
			if len(failed) == 0 && len(skipped) == 0 {
				return fmt.Errorf("stack restored, but service(s) not healthy: %s%s", strings.Join(still, ", "), whyNotHealthy(still, reasons))
			}
		}
	}
	if len(failed) > 0 || len(skipped) > 0 {
		e.logStackOutcome(logID, order, failed, skipped)
		return stackIncompleteError(len(order), failed, skipped)
	}
	// F170: with every service up, ask the application's own database which
	// optional subsystems are switched on and say what each one still needs. The
	// profile belongs to the application and the database lives in a different
	// container, so this is the one place both are in hand. Read-only, advisory,
	// and silent for a deployment using the defaults — which is most of them.
	e.stackPostRestoreNotes(ctx, targetNodeID, order, logID)

	e.logf(logID, "INFO", "Stack %q restored — all %d services are running", project, len(order))
	if targetNodeID != sourceNodeID {
		var addressVars []string
		for _, s := range order {
			addressVars = append(addressVars, AddressEnvKeys(s.man)...)
		}
		slices.Sort(addressVars)
		e.logMovedChecklist(logID, sourceNodeID, targetNodeID, slices.Compact(addressVars))
	}
	return nil
}

// failedDependency names the first dependency of s that failed or was itself
// skipped, or "" when every dependency is in place. The order is topological,
// so a skip propagates down a chain without looking further than one level.
func failedDependency(s *stackService, failed map[string]error, skipped map[string]string) string {
	for _, dep := range s.man.DependsOn {
		if _, bad := failed[dep]; bad {
			return dep
		}
		if _, gone := skipped[dep]; gone {
			return dep
		}
	}
	return ""
}

// logStackOutcome ends an incomplete stack restore with one line per service,
// so what came back and what did not is in one place, not scattered through the
// run.
func (e *Engine) logStackOutcome(logID string, order []*stackService, failed map[string]error, skipped map[string]string) {
	e.logf(logID, "INFO", "What happened to each service:")
	for _, s := range order {
		switch {
		case failed[s.service] != nil:
			e.logf(logID, "ERR", "  %s — FAILED: %v", s.service, failed[s.service])
		case skipped[s.service] != "":
			e.logf(logID, "WARN", "  %s — skipped: it depends on %s, which did not restore", s.service, skipped[s.service])
		default:
			e.logf(logID, "INFO", "  %s — restored", s.service)
		}
	}
}

// stackIncompleteError summarises an incomplete stack restore in one line.
func stackIncompleteError(total int, failed map[string]error, skipped map[string]string) error {
	failedNames := make([]string, 0, len(failed))
	for name := range failed {
		failedNames = append(failedNames, name)
	}
	sort.Strings(failedNames)
	skippedNames := make([]string, 0, len(skipped))
	for name := range skipped {
		skippedNames = append(skippedNames, name)
	}
	sort.Strings(skippedNames)
	restored := total - len(failed) - len(skipped)
	msg := fmt.Sprintf("stack restore incomplete: %d of %d service(s) restored; failed: %s", restored, total, strings.Join(failedNames, ", "))
	if len(skippedNames) > 0 {
		msg += "; skipped because a dependency failed: " + strings.Join(skippedNames, ", ")
	}
	return errors.New(msg)
}

// whyNotHealthy appends the one service's own reason to the failure, when there
// is exactly one and it said something (F187).
//
// Only for a single service, deliberately: two applications failing for two
// reasons is a list, and a list belongs in the log where it is already printed
// in full, not crammed into a one-line error that has to fit a toast.
func whyNotHealthy(still []string, reasons map[string]string) string {
	if len(still) != 1 {
		return ""
	}
	if why := strings.TrimSpace(reasons[still[0]]); why != "" {
		return " — it says: " + why
	}
	return ""
}

// reconstructStackCompose writes ONE compose file describing the whole project
// (F190).
//
// Each backup carries a one-service reconstruction of its own container, which
// is right for a single-container restore and wrong for a stack: all five
// members record the same working directory and the same filename, so written in
// turn they overwrite each other and what remains describes whichever service
// went last.
//
// Non-fatal in every direction. The containers are already restored and running;
// this rebuilds the folder beside them, and an operator who keeps their real
// compose file in version control needs none of it.
func (e *Engine) reconstructStackCompose(ctx context.Context, targetNodeID, project string, order []*stackService, sopts StackRestoreOptions, logID string) {
	cli, err := e.Reg.Get(targetNodeID)
	if err != nil {
		return
	}
	var (
		docs        [][]byte
		workingDir  string
		composeFile string
		anchorName  string
	)
	// #16: which services will share the written file. `depends_on` is only
	// emitted for the ones that are actually in it — compose refuses to start a
	// project whose dependency is undefined.
	siblings := map[string]bool{}
	for _, s := range order {
		if s != nil {
			siblings[s.service] = true
		}
	}
	for _, s := range order {
		if s == nil || s.man == nil {
			continue
		}
		// F192: build each member's document from the LIVE restored container,
		// not from the archive. The archived document describes the machine the
		// backup came from; by this point the recreate has already applied
		// everything the operator asked for — the new address, the rewritten
		// user-mapping ids, the IP and path remaps — and a written file that
		// contradicts the running container is how "the compose file still shows
		// the old domain" happens. The archive is only the fallback for a member
		// whose live container cannot be read.
		raw := e.liveComposeDoc(ctx, cli, s, siblings)
		if len(raw) == 0 {
			var xerr error
			raw, xerr = e.extractEntry(ctx, s.backup, "", "config/docker-compose.yml")
			if xerr != nil || len(raw) == 0 {
				e.logf(logID, "WARN", "No compose source for %q (live read failed and the backup has no reconstruction) — that service will be missing from the written file", s.service)
				continue
			}
			e.logf(logID, "INFO", "Using the archived reconstruction for %q — its live container could not be read, so this entry describes the source machine", s.service)
		}
		docs = append(docs, raw)
		if workingDir == "" {
			workingDir, composeFile, anchorName = s.man.StackWorkingDir, s.man.ComposeFile, s.backup.TargetName
		}
	}
	if len(docs) == 0 {
		return
	}
	merged, promoted, merr := mergeComposeDocs(docs)
	if merr != nil {
		e.logf(logID, "WARN", "Could not assemble the stack's compose file: %v", merr)
		return
	}
	// #16: the written file now waits for a database to be READY where the source
	// waited only for its container to exist. Said out loud, because the file
	// deliberately differs from what was captured — it is the artifact the
	// operator edits and re-applies, not the containers already running.
	if promoted > 0 {
		e.logf(logID, "INFO", "The stack's compose file waits for %d database dependenc(ies) to report healthy, where the source waited only for the container to start. Nothing running changed — this is the file you would next `docker compose up` from.", promoted)
	}

	stackDir := ResolveStackDir(workingDir, anchorName, sopts.HostBaseDir, sopts.RemapFromPath, sopts.RemapToPath)
	if stackDir == "" {
		e.logf(logID, "INFO", "Skipping host stack reconstruction — %q has no recorded compose directory and no base directory was provided", project)
		return
	}
	// F192: a working directory recorded by a management tool is that tool's
	// internal layout — Portainer runs compose from /data/compose/26 — and
	// reproducing it hands the operator a folder literally named "26". The
	// project name wins when the two differ and there is a target base to
	// build under.
	if fixed, changed := PreferProjectLeaf(stackDir, project, firstNonEmpty(sopts.RemapToPath, sopts.HostBaseDir)); changed {
		e.logf(logID, "INFO", "The recorded compose folder %s looks like a deployment tool's internal path — writing to %s instead, named after the project", stackDir, fixed)
		stackDir = fixed
	}
	// The written file has to agree with the containers beside it, so it gets the
	// same remaps their configuration did.
	if sopts.RemapFromIP != "" && sopts.RemapToIP != "" {
		if out, n := dockercli.RemapTextHostIP(merged, sopts.RemapFromIP, sopts.RemapToIP); n > 0 {
			merged = out
			e.logf(logID, "INFO", "Remapped host IP %s → %s in %d place(s) of the stack's compose file", sopts.RemapFromIP, sopts.RemapToIP, n)
		} else {
			// F232: nothing matched. On a move that is a question, not an all-clear.
			e.logf(logID, "WARN", "The host IP remap found nothing to change in the stack's compose file: %s does not appear literally in it. "+
				"A compose file that reads ${VAR} carries the value in its .env instead — check the .env written beside it, because "+
				"`docker compose up` re-reads that file and would put the old value back", sopts.RemapFromIP)
		}
	}
	if sopts.RemapFromPath != "" && sopts.RemapToPath != "" && sopts.RemapFromPath != sopts.RemapToPath {
		if out, n := dockercli.RemapTextHostPath(merged, sopts.RemapFromPath, sopts.RemapToPath); n > 0 {
			merged = out
			e.logf(logID, "INFO", "Remapped host path %s → %s in %d place(s) of the stack's compose file", sopts.RemapFromPath, sopts.RemapToPath, n)
		}
	}
	if sopts.RemapFromDomain != "" && sopts.RemapToDomain != "" {
		if out, n := dockercli.RemapTextHostDomain(merged, sopts.RemapFromDomain, sopts.RemapToDomain); n > 0 {
			merged = out
			e.logf(logID, "INFO", "Remapped domain %s → %s in %d place(s) of the stack's compose file", sopts.RemapFromDomain, sopts.RemapToDomain, n)
		} else {
			// F232: nothing matched. On a move that is a question, not an all-clear.
			e.logf(logID, "WARN", "The domain remap found nothing to change in the stack's compose file: %s does not appear literally in it. "+
				"A compose file that reads ${VAR} carries the value in its .env instead — check the .env written beside it, because "+
				"`docker compose up` re-reads that file and would put the old value back", sopts.RemapFromDomain)
		}
	}

	// F194: the secrets move to a .env beside the file, referenced as ${VAR} —
	// the shape an operator keeps in version control, instead of a compose file
	// that leaks on sight.
	merged, envFile, movedSecrets := splitComposeSecrets(merged)
	// F231: the project's OWN .env, captured from the source host and put through
	// the same remaps. Taken from the anchor member, because a compose project
	// has one .env beside its one compose file.
	var originalEnv []byte
	for _, s := range order {
		if s == nil || s.backup == nil {
			continue
		}
		if orig := e.stackEnvFromArchive(ctx, s.backup, sopts.Source, logID,
			sopts.RemapFromIP, sopts.RemapToIP, sopts.RemapFromDomain, sopts.RemapToDomain,
			sopts.RemapFromPath, sopts.RemapToPath); len(orig) > 0 {
			originalEnv = orig
			break
		}
	}
	// F230: the folder belongs to whoever the stack's data was restored for, when
	// DockBack just created its parent. Taken from the anchor service — the
	// members of a project share one folder, and its owner is one answer.
	stackOwner := ""
	for _, s := range order {
		if s == nil || s.man == nil || s.backup == nil {
			continue
		}
		u, g, pinned := e.RestoreOwnership(targetNodeID, s.backup.TargetName)
		if o := HostFileOwner(u, g, pinned, s.man.ContainerEnvKeys); o != "" {
			stackOwner = o
			break
		}
	}
	// F57: the project's OWN compose file(s) from the source host, taken from the
	// same anchor member the .env came from — a compose project has one folder,
	// and the originals in it describe that one project.
	var originals []dockercli.NamedFile
	for _, s := range order {
		if s == nil || s.backup == nil {
			continue
		}
		if found := e.originalsForHost(ctx, s.backup, sopts.Source, logID,
			sopts.RemapFromIP, sopts.RemapToIP, sopts.RemapFromDomain, sopts.RemapToDomain,
			sopts.RemapFromPath, sopts.RemapToPath); len(found) > 0 {
			originals = found
			break
		}
	}
	layout := planStackFolder(composeFile, merged, originals)
	// F213: a restore that covers only part of the project — services left out,
	// or members with no backup — rebuilds a file from that part alone. Written
	// as the stack's compose file, it would describe a fraction of the project,
	// so it is not written at all. The operator's own file describes the whole
	// project and still is: commafeed came back without one because its database
	// had no backup.
	if len(sopts.Services) > 0 || len(sopts.MissingMembers) > 0 {
		narrowed, ok := partialStackFolder(layout)
		if !ok {
			e.logf(logID, "INFO", "Not writing %q's compose file — this restore covers only some of its services, the backup holds no original compose file, and one rebuilt from a subset would describe a fraction of the project", project)
			return
		}
		layout, envFile, movedSecrets = narrowed, originalEnv, 0
		e.logf(logID, "INFO", "This restore covers only some of %q's services, so only your own compose file and .env are written — they describe the whole project", project)
	} else if len(originalEnv) > 0 {
		envFile = MergeEnvFiles(originalEnv, envFile)
	}
	e.warnDuplicateEnvKeys(logID, envFile)

	checker := e.openComposeChecker(ctx, cli, logID)
	if checker != nil {
		defer checker.Close()
	}
	layout = e.guardReconstruction(ctx, checker, layout, envFile, project, logID)

	e.logf(logID, "INFO", "Writing the compose file for %q at %s", project, stackDir)
	res, rerr := dockercli.ReconstructStackDirWithEnv(ctx, cli, stackDir, composeFile, layout.Primary, envFile, stackOwner, layout.Beside...)
	if rerr != nil {
		e.logf(logID, "WARN", "Host stack reconstruction skipped: %v", rerr)
		return
	}
	e.logStackFolder(logID, res, layout, movedSecrets)
	// The project's other files, from the first member that captured them: a
	// project has one folder, and every member that captured it holds the same.
	for _, s := range order {
		if s != nil && s.man != nil && s.man.ProjectFolder != nil && s.man.ProjectFolder.Entries > 0 {
			e.restoreProjectFolder(ctx, cli, s.backup, s.man, sopts.Source, logID, res.Dir)
			break
		}
	}
	e.reportComposeCheck(ctx, cli, checker, res, layout, project, true, logID)
}

// restoreStackFilesOnly puts back the stack's folder once, and every member's
// missing single-file binds (step 23). No service is stopped or changed.
func (e *Engine) restoreStackFilesOnly(ctx context.Context, targetNodeID, project string, order []*stackService, sopts StackRestoreOptions, logID string) error {
	cli, err := e.Reg.Get(targetNodeID)
	if err != nil {
		return err
	}
	e.logf(logID, "INFO", "Files-only restore of stack %q: putting back its files on the host. No service is stopped or changed, and nothing that exists is overwritten.", project)
	for _, s := range order {
		memberOpts := stackServiceRestoreOptions(s, targetNodeID, sopts)
		e.restoreMissingBindFiles(ctx, cli, s.backup, s.man, memberOpts, logID)
		e.reportMissingDataFolders(ctx, cli, s.man, memberOpts, logID)
	}
	e.reconstructStackCompose(ctx, targetNodeID, project, order, sopts, logID)
	e.logf(logID, "INFO", "Stack %q: files restored — no service was stopped or changed", project)
	return nil
}

// liveComposeDoc builds one service's compose document from its container AS
// RESTORED, resolved by name because a recreate changes the id (F192).
//
// Empty on any failure — the caller falls back to the archived reconstruction
// and says so, which is honest but describes the source machine.
func (e *Engine) liveComposeDoc(ctx context.Context, cli *client.Client, s *stackService, siblings map[string]bool) []byte {
	ictx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	id, ok := dockercli.FindContainerByName(ictx, cli, s.backup.TargetName)
	if !ok {
		return nil
	}
	insp, err := cli.ContainerInspect(ictx, id)
	if err != nil || insp.Config == nil {
		return nil
	}
	doc, err := composeFromInspect(insp, s.man.Image, s.man.Networks, siblings, dockercli.ImageEnv(ctx, cli, insp.Image))
	if err != nil {
		return nil
	}
	return doc
}

// stackPostRestoreNotes pairs the application that declares notes with the
// service holding its database, and asks (F170).
//
// Both halves have to be found: a stack where nothing declares notes, or one
// with no database service, simply produces nothing. Best-effort in every
// direction — the restore has already succeeded and this cannot change that.
func (e *Engine) stackPostRestoreNotes(ctx context.Context, targetNodeID string, order []*stackService, logID string) {
	appImage, dbName, engine := "", "", ""
	for _, s := range order {
		if s == nil || s.man == nil {
			continue
		}
		if appImage == "" && len(postRestoreNotesFor(s.man.Image)) > 0 {
			appImage = s.man.Image
		}
		if engine == "" && len(s.man.Databases) > 0 {
			engine = s.man.Databases[0].Engine
			dbName = s.backup.TargetName
		}
	}
	if appImage == "" || dbName == "" {
		return
	}
	cli, err := e.Reg.Get(targetNodeID)
	if err != nil {
		return
	}
	// The database container was recreated, so resolve it by NAME — its id has
	// changed since the manifest recorded one.
	id, ok := dockercli.FindContainerByName(ctx, cli, dbName)
	if !ok {
		return
	}
	e.reportPostRestoreNotes(ctx, cli, logID, appImage, id, engine)
}

// stackServiceRestoreOptions builds the RestoreOptions for one stack member (F52):
// its own backup, restored ONTO the target node (which may differ from the source
// for a cross-node migration), carrying the stack-wide restore flags. Pure, so a
// test can assert the target node is threaded through every service without Docker.
func stackServiceRestoreOptions(s *stackService, targetNodeID string, sopts StackRestoreOptions) RestoreOptions {
	return RestoreOptions{
		// F146: the whole set was judged before this loop began, so the
		// per-service solo guard must not refuse the restore it exists to
		// recommend.
		stackMember: true,
		BackupID:    s.backup.ID,
		// F173: threaded to every member. Only the service whose profile declares
		// the binding acts on it; for the rest it is inert.
		NewSiteAddress:     sopts.NewSiteAddress,
		NewUpstreamAddress: sopts.NewUpstreamAddress,
		// F209: the one key, given to every member. Inert for a member that is not
		// write-only — Restore only installs it when the manifest needs it.
		PrivateKey: sopts.PrivateKey,
		// F214: every member reads from the SAME chosen copy — that is the whole
		// request when the local disk is the thing being distrusted.
		Source:               sopts.Source,
		NodeID:               targetNodeID,
		TargetID:             s.man.ContainerID,
		Volumes:              true,
		Database:             true,
		Recreate:             sopts.Recreate,
		AllowDifferentImage:  sopts.AllowDifferentImage,
		FilesOnly:            sopts.FilesOnly,
		Snapshot:             sopts.Snapshot,
		ReconstructHost:      sopts.ReconstructHost,
		HostBaseDir:          sopts.HostBaseDir,
		PromoteRestartPolicy: sopts.PromoteRestartPolicy,
		InjectHealthchecks:   sopts.InjectHealthchecks,
		RemapFromIP:          sopts.RemapFromIP,
		RemapFromDomain:      sopts.RemapFromDomain,
		RemapToDomain:        sopts.RemapToDomain,
		RemapToIP:            sopts.RemapToIP,
		RemapFromPath:        sopts.RemapFromPath,
		RemapToPath:          sopts.RemapToPath,
	}
}

// filterStackServices narrows the selected members to the operator's choice
// (F213), refusing a name the stack does not have.
//
// An unknown name is an error rather than a silent omission: "restore db and
// cache" that quietly restores only db is how somebody ends up believing a
// service came back when it never did. The names are the same compose service
// keys the restore plan displays, so a typo is the operator's, not a mismatch
// between two vocabularies.
func filterStackServices(byService map[string]*stackService, want []string) (map[string]*stackService, error) {
	out := make(map[string]*stackService, len(want))
	var unknown []string
	for _, name := range want {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		s, ok := byService[name]
		if !ok {
			unknown = append(unknown, name)
			continue
		}
		out[name] = s
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		known := make([]string, 0, len(byService))
		for k := range byService {
			known = append(known, k)
		}
		sort.Strings(known)
		return nil, fmt.Errorf("this stack has no service named %s — it has %s",
			strings.Join(unknown, " or "), strings.Join(known, ", "))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no services selected — nothing to restore")
	}
	return out, nil
}

// checkStackPrivateKey validates one offline key against every write-only member
// of the stack, before anything is restored (F209).
//
// Fingerprint comparison only — CheckPrivateKey derives the key's own public half
// and compares it with the manifest's recorded keypair, so nothing is decrypted
// and a wrong key is named as a wrong key rather than surfacing later as a
// decryption failure that reads like corruption.
//
// The error names the SERVICE, not just the failure: an operator holding several
// recovery sheets needs to know which member disagreed with the key they used.
// Members are checked in a stable order so the same wrong key always reports the
// same service, rather than whichever one a map iteration reached first.
func checkStackPrivateKey(byService map[string]*stackService, priv string) error {
	names := make([]string, 0, len(byService))
	for name := range byService {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		s := byService[name]
		if s == nil || !IsWriteOnly(s.man) {
			continue
		}
		if err := CheckPrivateKey(s.man, priv); err != nil {
			return fmt.Errorf("service %q is write-only encrypted: %w", name, err)
		}
	}
	return nil
}

// fillDependsOnFromCompose recovers depends_on from the stack's own compose
// file when no member's container recorded one — a stack manager that creates
// containers itself may not set the label. Without it the order falls back to
// tiers and names, which is how a helper that depends on two services was
// restored before both of them in a real recovery.
//
// Only when NO member recorded any: a stack where some did has its labels, and
// a service with no dependencies is then genuinely independent. Reads the
// smallest member's archive that holds the compose file.
func (e *Engine) fillDependsOnFromCompose(ctx context.Context, svcs map[string]*stackService, source, logID string) {
	if len(svcs) < 2 {
		return
	}
	var donor *stackService
	for _, s := range svcs {
		if len(s.man.DependsOn) > 0 {
			return
		}
		if s.man.HasOriginalCompose && (donor == nil || s.backup.SizeBytes < donor.backup.SizeBytes) {
			donor = s
		}
	}
	if donor == nil {
		return
	}
	merged := map[string][]string{}
	for _, body := range e.originalComposeFromArchive(ctx, donor.backup, source) {
		for service, deps := range dependsOnDeclaredIn(body) {
			merged[service] = append(merged[service], deps...)
		}
	}
	if filled := fillMissingDependsOn(svcs, merged); len(filled) > 0 {
		e.logf(logID, "INFO", "depends_on for %s taken from the stack's own compose file — its containers carried no depends_on label", strings.Join(filled, ", "))
	}
}

// dependsOnDeclaredIn reads each service's depends_on from a compose file, in
// either form Compose accepts: a list of names, or a map of name → {condition}.
func dependsOnDeclaredIn(compose []byte) map[string][]string {
	var doc struct {
		Services map[string]struct {
			DependsOn yaml.Node `yaml:"depends_on"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(compose, &doc); err != nil {
		return nil
	}
	out := map[string][]string{}
	for service, def := range doc.Services {
		node := def.DependsOn
		switch node.Kind {
		case yaml.SequenceNode:
			for _, item := range node.Content {
				if item.Kind == yaml.ScalarNode && item.Value != "" {
					out[service] = append(out[service], item.Value)
				}
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(node.Content); i += 2 {
				out[service] = append(out[service], node.Content[i].Value)
			}
		}
	}
	return out
}

// fillMissingDependsOn gives each member with no recorded depends_on the one
// the compose file declares, and returns the members it filled, sorted.
func fillMissingDependsOn(svcs map[string]*stackService, declared map[string][]string) []string {
	var filled []string
	for service, s := range svcs {
		deps := declared[service]
		if len(s.man.DependsOn) > 0 || len(deps) == 0 {
			continue
		}
		s.man.DependsOn = deps
		filled = append(filled, service)
	}
	sort.Strings(filled)
	return filled
}

// topoOrder returns services ordered so each comes after its dependencies; among
// otherwise-equal services, data-tier (db/redis) come first, then alphabetical —
// giving "db/redis → support → app" without an explicit compose file.
func topoOrder(svcs map[string]*stackService) []*stackService {
	indeg := map[string]int{}
	for name := range svcs {
		indeg[name] = 0
	}
	for name, s := range svcs {
		for _, dep := range s.man.DependsOn {
			if _, ok := svcs[dep]; ok {
				indeg[name]++
			}
		}
	}
	var order []*stackService
	placed := map[string]bool{}
	for len(order) < len(svcs) {
		// Collect ready (indegree 0, not placed).
		var ready []string
		for name := range svcs {
			if !placed[name] && indeg[name] == 0 {
				ready = append(ready, name)
			}
		}
		if len(ready) == 0 {
			// Cycle / leftover — append the rest in tier+alpha order.
			for name := range svcs {
				if !placed[name] {
					ready = append(ready, name)
				}
			}
		}
		sort.Slice(ready, func(a, b int) bool {
			sa, sb := svcs[ready[a]], svcs[ready[b]]
			if sa.tierRank != sb.tierRank {
				return sa.tierRank > sb.tierRank // proven DBs, then data-ish, then apps
			}
			return ready[a] < ready[b]
		})
		pick := ready[0]
		placed[pick] = true
		order = append(order, svcs[pick])
		// Decrement dependents.
		for name, s := range svcs {
			if placed[name] {
				continue
			}
			for _, dep := range s.man.DependsOn {
				if dep == pick {
					indeg[name]--
				}
			}
		}
	}
	return order
}

// manifestsOf reduces the selection to service → manifest, the shape the atomic
// verdict works on (F146).
func manifestsOf(byService map[string]*stackService) map[string]*Manifest {
	out := make(map[string]*Manifest, len(byService))
	for svc, s := range byService {
		if s != nil {
			out[svc] = s.man
		}
	}
	return out
}
