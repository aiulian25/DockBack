package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"dockback/internal/appbackup"
	"dockback/internal/backup"
	"dockback/internal/dockercli"
	"dockback/internal/store"
	"dockback/internal/version"
)

// Auto-generated Disaster-Recovery runbook (Fable-UI-UX C3). This assembles the
// pieces DockBack already holds — manifests (what's inside each backup, incl. DB
// extensions and skipped large binds), copy locations, restore-drill results, key
// escrow status, and the control-plane app-backup — into the artifact you actually
// need at 2 a.m.: the correct RESTORE ORDER (databases first, extensions to
// reinstall, then app volumes), where every copy lives, and the last proven
// restore per service. Everything is read from SQLite/disk (no Docker/network
// calls), honoring the no-egress posture and keeping it cheap.

type runbookKey struct {
	Fingerprint  string `json:"fingerprint"`
	Acknowledged bool   `json:"acknowledged"`
	Ephemeral    bool   `json:"ephemeral"` // in-memory key: lost on restart if not escrowed
}

type runbookAppBackup struct {
	Exists   bool  `json:"exists"`
	NewestAt int64 `json:"newest_at"`
	Count    int   `json:"count"`
}

type runbookDest struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Enabled bool   `json:"enabled"`
}

type runbookLoc struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Status    string `json:"status,omitempty"` // "failed" = intended copy that didn't upload
	Detail    string `json:"detail,omitempty"`
	Immutable bool   `json:"immutable,omitempty"`
}

type runbookSkipped struct {
	Destination string `json:"destination"`
	Type        string `json:"type"`
	Reason      string `json:"reason"`
	Bytes       int64  `json:"bytes,omitempty"`
	// CoveredBy (F83): another container on the node captures this same host
	// path in its own backups — listed for completeness but not counted as PARTIAL.
	CoveredBy string `json:"covered_by,omitempty"`
}

type runbookService struct {
	Order         int              `json:"order"`
	Container     string           `json:"container"`
	Stack         string           `json:"stack,omitempty"`
	Service       string           `json:"service,omitempty"`
	Role          string           `json:"role"` // "database" | "application"
	Engine        string           `json:"engine,omitempty"`
	Image         string           `json:"image,omitempty"`
	ImageDigest   string           `json:"image_digest,omitempty"`
	ImageBundled  bool             `json:"image_bundled"` // image.tar saved inside the backup (restores offline)
	Extensions    []string         `json:"extensions,omitempty"`
	Volumes       int              `json:"volumes"`
	Databases     int              `json:"databases"`
	SkippedMounts []runbookSkipped `json:"skipped_mounts,omitempty"`
	OrigCompose   bool             `json:"original_compose,omitempty"` // F57: genuine host compose captured
	Locations     []runbookLoc     `json:"locations"`
	LastBackupAt  int64            `json:"last_backup_at"`
	LastDrillAt   int64            `json:"last_drill_at,omitempty"`
	LastDrillOK   *bool            `json:"last_drill_ok,omitempty"`
	Partial       bool             `json:"partial"`
	// DBFallback (F103): this service was detected as a database engine but its
	// dump tools were missing, so it was captured as raw files. The restore path
	// is file-level, NOT a dump import — which changes what the operator does
	// mid-incident, so it belongs in the runbook rather than only in the UI.
	DBFallback string `json:"db_fallback,omitempty"`
	// StaleStaging (F157): interrupted-write leftovers this archive carries. In
	// the runbook because somebody restoring under pressure will see these
	// directories in the recovered tree and wonder whether the restore went
	// wrong. It did not — and knowing that in advance is worth a line.
	StaleStaging []string `json:"stale_staging,omitempty"`
	// Pilot-light standby (F62): the designated fallback node and whether the newest
	// verified backup is PROVEN to boot there, and how stale that proof is. Nil
	// StandbyOK + empty StandbyNode = not configured.
	StandbyNode string `json:"standby_node,omitempty"`
	StandbyOK   *bool  `json:"standby_ok,omitempty"`
	StandbyAt   int64  `json:"standby_at,omitempty"`
	// ConfigDrift (F73): the live container's config no longer matches what this
	// backup captured — a restore returns the OLDER config.
	ConfigDrift bool     `json:"config_drift,omitempty"`
	Notes       []string `json:"notes,omitempty"`

	// Internal, NOT serialized (unexported): the newest backup id and its manifest
	// container id, so the whole-node restore executor can drive each service (F6).
	backupID    string
	containerID string
}

type runbookNode struct {
	NodeID    string           `json:"node_id"`
	NodeName  string           `json:"node_name"`
	Reachable bool             `json:"reachable"`
	Services  []runbookService `json:"services"`
}

type runbookSummary struct {
	Services       int `json:"services"`
	Databases      int `json:"databases"`
	PartialBackups int `json:"partial_backups"`
	Undrilled      int `json:"undrilled"`
}

type runbookResp struct {
	GeneratedAt  int64            `json:"generated_at"`
	AppVersion   string           `json:"app_version"`
	Key          runbookKey       `json:"key"`
	AppBackup    runbookAppBackup `json:"app_backup"`
	Destinations []runbookDest    `json:"destinations"`
	Nodes        []runbookNode    `json:"nodes"`
	Summary      runbookSummary   `json:"summary"`
}

// handleRunbook composes the fleet disaster-recovery runbook (C3).
func (s *Server) handleRunbook(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.buildRunbook())
}

// buildRunbook composes the fleet disaster-recovery runbook (C3), shared by the
// authenticated JSON endpoint and the redacted public share view (F59).
func (s *Server) buildRunbook() runbookResp {
	// Slices start non-nil so an empty fleet serializes as [] rather than null —
	// the UI indexes these directly (a node with no successful backups used to
	// crash the Recovery page with "services is null").
	resp := runbookResp{
		GeneratedAt: time.Now().Unix(), AppVersion: version.Version,
		Destinations: []runbookDest{}, Nodes: []runbookNode{},
	}

	// Master-key recovery status (fingerprint only — never the key itself).
	ack, _ := s.store.GetSetting("key.escrow_acknowledged", "false")
	resp.Key = runbookKey{Fingerprint: s.engine.MasterKeyFP(), Acknowledged: ack == "true", Ephemeral: s.cfg.EphemeralKey}

	// Control-plane (app) backup: restore this first if DockBack itself is lost.
	if list, err := appbackup.List(s.appBackupDir()); err == nil {
		resp.AppBackup.Count = len(list)
		for _, e := range list {
			if e.CreatedAt > resp.AppBackup.NewestAt {
				resp.AppBackup.NewestAt = e.CreatedAt
			}
		}
		resp.AppBackup.Exists = len(list) > 0
	}

	if dests, err := s.store.ListDestinations(); err == nil {
		for _, d := range dests {
			resp.Destinations = append(resp.Destinations, runbookDest{Name: d.Name, Type: d.Type, Enabled: d.Enabled})
		}
	}

	nodes, _ := s.store.ListNodes()
	for _, n := range nodes {
		rn := runbookNode{NodeID: n.ID, NodeName: n.Name}
		if st := s.getStat(n.ID); st != nil {
			rn.Reachable = st.Reachable
		}
		rn.Services = s.nodeRestorePlan(n.ID)
		for i := range rn.Services {
			resp.Summary.Services++
			if rn.Services[i].Role == "database" {
				resp.Summary.Databases++
			}
			if rn.Services[i].Partial {
				resp.Summary.PartialBackups++
			}
			if rn.Services[i].LastDrillOK == nil {
				resp.Summary.Undrilled++
			}
		}
		resp.Nodes = append(resp.Nodes, rn)
	}

	return resp
}

// nodeRestorePlan builds a node's ordered restore plan: the newest SUCCESSFUL
// backup per container, enriched (role/engine/extensions/locations/last-drill),
// then sorted into the disaster-recovery order — databases first (with their
// extensions), then applications; within a role by stack then name (DB-first is
// the key rule). Each service carries its 1-based Order and, for the executor,
// the backup id + manifest container id to restore into. Shared by the runbook
// view and the whole-node restore executor so both use ONE ordering (F6).
func (s *Server) nodeRestorePlan(nodeID string) []runbookService {
	list, _ := s.store.ListBackups(nodeID, 5000)
	// Live containers from the inventory cache, by name (F73 config drift).
	liveByName := map[string]*dockercli.Container{}
	if st := s.getStat(nodeID); st != nil {
		for _, c := range st.Containers {
			liveByName[c.Name] = c
		}
	}
	// Standby rehearsal config/result per target on this node (F62), fetched once.
	standby := map[string]*store.Standby{}
	if all, err := s.store.ListStandby(); err == nil {
		for _, sb := range all {
			if sb.NodeID == nodeID {
				standby[sb.Target] = sb
			}
		}
	}
	newest := map[string]int{} // target_name -> index of newest success in `list`
	for i, b := range list {
		if b.Status != "success" {
			continue
		}
		if j, ok := newest[b.TargetName]; !ok || list[i].CreatedAt > list[j].CreatedAt {
			newest[b.TargetName] = i
		}
	}

	// Non-nil so a node with no successful backups serializes as "services": []
	// (never null) — the Recovery page indexes it unconditionally.
	services := []runbookService{}
	for _, idx := range newest {
		b := list[idx]
		var m backup.Manifest
		_ = json.Unmarshal([]byte(b.ManifestJSON), &m)

		svc := runbookService{
			Container: b.TargetName, Stack: m.Stack, Service: m.Service,
			Image: m.Image, ImageDigest: m.ImageDigest, ImageBundled: m.ImageTar != nil,
			Volumes: len(m.Volumes), Databases: len(m.Databases),
			OrigCompose:  m.HasOriginalCompose,
			LastBackupAt: b.CompletedAt,
			backupID:     b.ID,
			containerID:  m.ContainerID,
		}
		if svc.LastBackupAt == 0 {
			svc.LastBackupAt = b.CreatedAt
		}
		svc.DBFallback = m.DBFallback                      // F103
		svc.StaleStaging = backup.StaleStagingFindings(&m) // F157
		// Role + engine + Postgres extensions to reinstall (the Immich gotcha).
		if len(m.Databases) > 0 {
			svc.Role = "database"
			svc.Engine = m.Databases[0].Engine
			seen := map[string]bool{}
			for _, db := range m.Databases {
				for _, ext := range db.Extensions {
					if !seen[ext] {
						seen[ext] = true
						svc.Extensions = append(svc.Extensions, ext)
					}
				}
			}
		} else {
			svc.Role = "application"
		}

		// Skipped mounts → PARTIAL warning (data NOT in the backup). A skip
		// covered by another container's backups (F83 shared bind) stays listed
		// but doesn't make the service PARTIAL.
		uncovered := 0
		for _, sk := range m.SkippedMounts {
			dst := sk.Destination
			if dst == "" {
				dst = sk.Source
			}
			svc.SkippedMounts = append(svc.SkippedMounts, runbookSkipped{Destination: dst, Type: sk.Type, Reason: sk.Reason, Bytes: sk.Bytes, CoveredBy: sk.CoveredBy})
			if sk.CoveredBy == "" {
				uncovered++
			}
		}
		svc.Partial = uncovered > 0

		// Where every copy lives.
		var locs []backup.Location
		if b.LocationsJSON != "" {
			_ = json.Unmarshal([]byte(b.LocationsJSON), &locs)
		}
		if len(locs) == 0 {
			locs = []backup.Location{{Kind: "local", Name: "Local", Type: "local"}}
		}
		for _, l := range locs {
			svc.Locations = append(svc.Locations, runbookLoc{Name: l.Name, Type: l.Type, Status: l.Status, Detail: l.Detail, Immutable: l.Immutable})
		}

		// Last proven restore (drill).
		if dr, err := s.store.GetDrill(b.ID); err == nil && dr != nil {
			svc.LastDrillAt = dr.RanAt
			ok := dr.OK
			svc.LastDrillOK = &ok
		}

		// Standby readiness: is this service proven to boot on its fallback node? (F62)
		if sb := standby[b.TargetName]; sb != nil {
			svc.StandbyNode = sb.StandbyNode
			svc.StandbyAt = sb.LastRun
			if sb.LastRun > 0 {
				ok := sb.LastOK
				svc.StandbyOK = &ok
			}
		}

		// Config drift (F73): the live container (from the inventory cache — zero
		// probes, keeping this plan honest for unreachable nodes) no longer matches
		// the config this backup captured. Lite fingerprints compare only what the
		// cache carries (image + mounts); the container page catches the rest.
		if m.ConfigFPLite != "" {
			if c := liveByName[b.TargetName]; c != nil {
				if fp := backup.ConfigFingerprintLite(c.Image, c.ImageID, c.Mounts); fp != "" && fp != m.ConfigFPLite {
					svc.ConfigDrift = true
				}
			}
		}

		svc.Notes = runbookNotes(svc)
		services = append(services, svc)
	}

	// Restore order: databases first, then applications; within a role, group by
	// stack then name (DB-first is the key rule).
	sort.Slice(services, func(i, j int) bool {
		a, b := services[i], services[j]
		ra, rb := roleRank(a.Role), roleRank(b.Role)
		if ra != rb {
			return ra < rb
		}
		if a.Stack != b.Stack {
			return a.Stack < b.Stack
		}
		return a.Container < b.Container
	})
	for i := range services {
		services[i].Order = i + 1
	}
	return services
}

func roleRank(role string) int {
	if role == "database" {
		return 0
	}
	return 1
}

// runbookNotes derives the human "gotchas under pressure" steps for a service.
func runbookNotes(svc runbookService) []string {
	var notes []string
	if svc.Role == "database" && len(svc.Extensions) > 0 {
		notes = append(notes, "Reinstall these database extensions on the restore target BEFORE importing the dump: "+joinComma(svc.Extensions)+". A missing or mismatched extension is the classic cause of a failed database restore.")
	}
	// F34 / PLAN §9.7: DockBack captures a Postgres full dump, never continuous
	// WAL, so recovery is only ever to the last backup — not an arbitrary
	// point-in-time. Make that RPO limit explicit in the runbook so it isn't a
	// surprise mid-incident, especially for a critical database.
	if svc.Role == "database" && svc.Engine == "postgres" {
		notes = append(notes, "Recovery granularity is the last full dump, not point-in-time — DockBack captures a full Postgres snapshot, not continuous WAL. Any writes since the last backup are lost on restore; tighten this database's schedule if its RPO must be shorter.")
	}
	if svc.Partial {
		notes = append(notes, "PARTIAL backup — some data was NOT captured (see skipped mounts) and must be recovered separately. Do not assume this backup is complete.")
	}
	// F103: the restore procedure for this service is NOT what the operator would
	// assume from "it's a database". Say so here, where the procedure is read.
	if svc.DBFallback != "" {
		notes = append(notes, "This service looks like a database, but its dump tools were missing ("+svc.DBFallback+
			") — its data was copied as RAW FILES from a running engine, which can be torn. Restore is file-level, not a dump import: expect to run the engine's own recovery/repair on first start, and verify the data before trusting it. Install the engine's client tools in the image (pg_dumpall / mysqldump / mongodump) and re-back up for a consistent dump.")
	}
	// F83: shared binds captured elsewhere — tell the operator WHERE that data
	// lives so a restore recovers it from the owner, not this service.
	for _, sk := range svc.SkippedMounts {
		if sk.CoveredBy != "" {
			notes = append(notes, "The shared path "+sk.Destination+" is captured via "+sk.CoveredBy+"'s backups — restore it from there; it is not missing.")
		}
	}
	// F157: the recovered tree will contain these, and they look like a failed
	// restore to anyone who has not been told otherwise.
	for _, f := range svc.StaleStaging {
		notes = append(notes, "Expect leftover staging directories in the recovered data — "+f+
			". They are not a sign the restore went wrong; the source tree has them too.")
	}
	if svc.OrigCompose {
		notes = append(notes, "This backup includes the ORIGINAL compose file(s) under config/original-compose/ — prefer them over the reconstruction when rebuilding by hand.")
	}
	if svc.ImageBundled {
		notes = append(notes, "The container image is bundled in this backup (image.tar) — it restores with no registry, fully offline.")
	} else if svc.ImageDigest != "" {
		notes = append(notes, "Restore the exact image by digest to avoid a surprise upgrade: "+svc.Image+"@"+svc.ImageDigest+".")
	} else if svc.Image != "" {
		// No digest pin and not bundled — derivable from the manifest alone (no
		// Docker/network call), so it's honest even for an unreachable node: a
		// restore can only re-pull the recorded tag, which may be gone or moved.
		notes = append(notes, "Image is not pinned by digest and not bundled — a restore depends on the tag "+svc.Image+" still existing in its registry. Re-back up with the image bundled (image.tar) for an exact, offline-proof restore.")
	}
	if svc.LastDrillOK == nil {
		notes = append(notes, "Not yet proven by a restore drill — run a drill to confirm it restores cleanly.")
	} else if !*svc.LastDrillOK {
		notes = append(notes, "The last restore drill FAILED — investigate before relying on this backup.")
	}
	// Config drift (F73): a restore of this backup returns a config the operator
	// has since changed — make that explicit before it surprises them mid-incident.
	if svc.ConfigDrift {
		notes = append(notes, "Container configuration changed AFTER this backup was taken — a restore returns the older configuration. Re-back up if the change is wanted.")
	}
	// Standby readiness (F62): a configured fallback that isn't proving out is a
	// silent DR gap — surface it in the runbook, not just the container page.
	if svc.StandbyNode != "" {
		if svc.StandbyOK != nil && !*svc.StandbyOK {
			notes = append(notes, "Standby rehearsal onto "+svc.StandbyNode+" is FAILING — this service is NOT proven to fail over there. Fix it before you need it.")
		} else if svc.StandbyOK == nil {
			notes = append(notes, "Standby onto "+svc.StandbyNode+" is configured but hasn't rehearsed yet — run a rehearsal to prove failover.")
		}
	}
	for _, l := range svc.Locations {
		if l.Status == "failed" {
			notes = append(notes, "Offsite copy to "+l.Name+" did not complete ("+l.Detail+") — this backup may exist only on the local disk.")
		}
	}
	return notes
}

func joinComma(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += ", "
		}
		out += x
	}
	return out
}
