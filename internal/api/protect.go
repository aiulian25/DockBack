package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"dockback/internal/backup"
	"dockback/internal/dockercli"
)

// One-click "Protect this container" (Fable-UI-UX B5). Instead of the full manual
// form, this composes sensible defaults from the existing detectors and the
// global policy, persists the minimum that actually makes a container protected —
// schedule coverage (+ a live-dump pause hint for databases) — and kicks an
// immediate first backup so protection is instant. Everything else (mounts,
// destinations, compression) already defaults correctly, so nothing per-container
// is stored for those. This is the fix-side of the B2 "unprotected" surfacing.

type protectResp struct {
	Container       string   `json:"container"`
	IsDatabase      bool     `json:"is_database"`
	Engine          string   `json:"engine,omitempty"`
	PauseMode       string   `json:"pause_mode"`
	Destinations    []string `json:"destinations"`
	Scheduled       bool     `json:"scheduled"`
	ScheduleEnabled bool     `json:"schedule_enabled"`
	ScheduleID      string   `json:"schedule_id"` // F38: so the UI can offer a one-click "Enable the schedule"
	ScheduleAdded   bool     `json:"schedule_added"`
	BackupStarted   bool     `json:"backup_started"`
	Summary         string   `json:"summary"`
}

// handleProtectContainer applies smart-default protection to one container (B5).
func (s *Server) handleProtectContainer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cid := r.PathValue("cid")
	node, err := s.store.GetNode(id)
	if err != nil {
		errJSON(w, http.StatusNotFound, "node not found")
		return
	}

	var req struct {
		BackupNow *bool `json:"backup_now"`
	}
	_ = readJSON(r, &req) // body is optional
	backupNow := req.BackupNow == nil || *req.BackupNow

	// Resolve name + image (cached first, no Docker call; live fallback).
	name, image := "", ""
	if c := s.cachedContainer(id, cid); c != nil {
		name, image = c.Name, c.Image
	}
	if name == "" {
		if n, e := s.resolveContainerName(id, cid); e == nil {
			name = n
		} else {
			errJSON(w, http.StatusBadGateway, e.Error())
			return
		}
	}
	if image == "" { // cache miss — inspect once for the DB-engine hint
		if cli, e := s.reg.Get(id); e == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			if insp, ie := cli.ContainerInspect(ctx, cid); ie == nil && insp.Config != nil {
				image = insp.Config.Image
			}
			cancel()
		}
	}

	engine := backup.DBEngine(image)
	isDB := engine != ""

	// Databases are dumped live and must keep running — pin pause_mode to none so
	// the intent is explicit (the engine also never pauses a detected DB). Non-DB
	// containers inherit the global default (pause) for a consistent snapshot.
	pauseMode := backup.PausePause
	if isDB {
		pauseMode = backup.PauseNone
		_ = s.store.SetSetting(backup.PauseModeKey(id, name), backup.PauseNone)
	}

	// Destination names from the effective policy (Local is always kept).
	dests := []string{"Local"}
	for _, did := range s.effectivePolicy(id).Destinations {
		if d, e := s.store.GetDestination(did); e == nil && d.Enabled {
			dests = append(dests, d.Name)
		}
	}

	// Schedule coverage: add a specific target to a named schedule if this
	// container isn't already covered (a whole-node target or an existing name
	// match across any schedule). Creates an enabled schedule if none exists, so
	// protection actually automates. The scheduler re-reads schedules each tick.
	covered, scheduleAdded, scheduleEnabled, scheduleID := s.scheduleProtect(id, name)

	// Kick an immediate first backup so protection is instant (defaults: policy
	// destinations, all eligible mounts, balanced compression, inherited pause).
	backupStarted := false
	if backupNow {
		s.runBackupAsync(node.Name, backup.Options{NodeID: id, ContainerID: cid})
		backupStarted = true
	}

	_ = s.store.Audit(userFrom(r), "container.protect", name,
		fmt.Sprintf("db=%v scheduled=%v added=%v backup=%v", isDB, covered, scheduleAdded, backupStarted))

	resp := protectResp{
		Container: name, IsDatabase: isDB, Engine: engine, PauseMode: pauseMode,
		Destinations: dests, Scheduled: covered, ScheduleEnabled: scheduleEnabled,
		ScheduleID: scheduleID, ScheduleAdded: scheduleAdded, BackupStarted: backupStarted,
	}
	resp.Summary = protectSummary(resp)
	writeJSON(w, http.StatusOK, resp)
}

// protectStackResp is the stack counterpart of protectResp (F220).
//
// Not protectResp itself: is_database and pause_mode are per-MEMBER decisions,
// and a stack made of a database and five app services has no single answer to
// either. The fields the UI actually acts on — summary, schedule_enabled,
// schedule_id for the F38 "enable the schedule" offer — are the same names, so
// the same toast handles both.
type protectStackResp struct {
	Stack     string   `json:"stack"`
	Services  int      `json:"services"`
	Databases []string `json:"databases,omitempty"` // members pinned to a live dump
	// Consistent is set when the whole stack is captured under one quiesce window
	// — true for every multi-service project, which is the point of the feature.
	Consistent      bool     `json:"consistent"`
	Destinations    []string `json:"destinations"`
	Scheduled       bool     `json:"scheduled"`
	ScheduleEnabled bool     `json:"schedule_enabled"`
	ScheduleID      string   `json:"schedule_id"`
	ScheduleAdded   bool     `json:"schedule_added"`
	// ReplacedTargets counts per-container schedule targets this stack target
	// subsumed and removed. Reported, never silent — it is somebody's schedule.
	ReplacedTargets int    `json:"replaced_targets,omitempty"`
	BackupStarted   bool   `json:"backup_started"`
	Summary         string `json:"summary"`
}

// handleProtectStack applies smart-default protection to a whole compose project
// (F220): ONE schedule target for the stack rather than one per service, the
// members' pause modes pinned the way each of them needs, and an immediate first
// backup — app-consistent when there is more than one service to be consistent
// across.
func (s *Server) handleProtectStack(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	project := r.PathValue("project")
	node, err := s.store.GetNode(id)
	if err != nil {
		errJSON(w, http.StatusNotFound, "node not found")
		return
	}

	var req struct {
		BackupNow *bool `json:"backup_now"`
	}
	_ = readJSON(r, &req) // body is optional
	backupNow := req.BackupNow == nil || *req.BackupNow

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
	// Resolved LIVE, not from the cache: this decides what gets a schedule and
	// what gets a first backup, and a stale inventory would quietly protect the
	// wrong set. F219: never DockBack's own test clones — a copy it will delete
	// tomorrow is not a member of anything.
	var members []*dockercli.Container
	for _, c := range cs {
		if c.Stack == project && !isTestClone(c) {
			members = append(members, c)
		}
	}
	if len(members) == 0 {
		errJSON(w, http.StatusNotFound, "no containers found for that stack")
		return
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })

	// Databases are dumped live and must keep running — pin pause_mode to none per
	// member, exactly as the single-container path does. A stack is a mix, so this
	// is the one part that cannot be decided once for the whole project.
	var databases []string
	names := make([]string, 0, len(members))
	for _, c := range members {
		names = append(names, c.Name)
		if backup.DBEngine(c.Image) != "" {
			databases = append(databases, c.Name)
			_ = s.store.SetSetting(backup.PauseModeKey(id, c.Name), backup.PauseNone)
		}
	}

	// One service is not a group, and an app-consistent run of one container is
	// just a backup with extra ceremony (and a stack-exclusive lock nothing else
	// needs). So consistency is claimed only where it means something.
	consistent := len(members) > 1

	dests := []string{"Local"}
	destIDs := s.effectivePolicy(id).Destinations
	for _, did := range destIDs {
		if d, e := s.store.GetDestination(did); e == nil && d.Enabled {
			dests = append(dests, d.Name)
		}
	}

	covered, scheduleAdded, scheduleEnabled, scheduleID, replaced := s.scheduleProtectStack(id, project, consistent, names)

	backupStarted := false
	if backupNow {
		if consistent {
			// F83: the same shared-bind dedup a manual stack backup applies, so a
			// folder two services both mount is captured once rather than twice.
			dedupInc, _, coverLogs := s.sharedBindDedup(id, members)
			for _, line := range coverLogs {
				s.logSink("stack:"+project, "INFO", line)
			}
			backupStarted = s.startConsistentStackBackup(id, project, destIDs, "", func(cid, name string) backup.Options {
				opts := s.stackServiceBackupOptions(id, cid, name, destIDs, "", "")
				if inc, ok := dedupInc[name]; ok {
					opts.IncludeMounts = inc
					opts.SelectionEphemeral = true // never overwrite the remembered selection
				}
				return opts
			})
			if !backupStarted {
				s.logSink("stack:"+project, "INFO", "Protected — the first backup was not started because a backup or restore of this stack is already running")
			}
		} else {
			s.runBackupAsync(node.Name, backup.Options{NodeID: id, ContainerID: members[0].ID})
			backupStarted = true
		}
	}

	_ = s.store.Audit(userFrom(r), "stack.protect", project,
		fmt.Sprintf("node=%s services=%d consistent=%v scheduled=%v added=%v replaced=%d backup=%v",
			node.Name, len(members), consistent, covered, scheduleAdded, replaced, backupStarted))

	resp := protectStackResp{
		Stack: project, Services: len(members), Databases: databases, Consistent: consistent,
		Destinations: dests, Scheduled: covered, ScheduleEnabled: scheduleEnabled,
		ScheduleID: scheduleID, ScheduleAdded: scheduleAdded, ReplacedTargets: replaced,
		BackupStarted: backupStarted,
	}
	resp.Summary = protectStackSummary(resp)
	writeJSON(w, http.StatusOK, resp)
}

// protectStackSummary renders the stack version of the one-line "here's what I
// decided" text, mirroring protectSummary's shape and order so the two read as
// the same voice.
func protectStackSummary(p protectStackResp) string {
	var parts []string
	if p.Consistent {
		parts = append(parts, fmt.Sprintf("App-consistent snapshot of all %d services", p.Services))
	} else {
		parts = append(parts, "Consistent volume snapshot")
	}
	if n := len(p.Databases); n == 1 {
		parts = append(parts, "live dump for "+p.Databases[0])
	} else if n > 1 {
		parts = append(parts, fmt.Sprintf("live dumps for %d databases", n))
	}

	ext := []string{}
	for _, d := range p.Destinations {
		if d != "Local" {
			ext = append(ext, d)
		}
	}
	if len(ext) > 0 {
		parts = append(parts, "mirrored to "+strings.Join(ext, ", "))
	} else {
		parts = append(parts, "stored locally")
	}

	if !p.Scheduled {
		parts = append(parts, "but it could NOT be added to a schedule — add one and target this stack")
	} else if p.ScheduleAdded {
		parts = append(parts, "on the automatic schedule as ONE stack target")
	} else if p.ScheduleEnabled {
		parts = append(parts, "already on the automatic schedule")
	}
	if p.Scheduled && !p.ScheduleEnabled {
		parts = append(parts, "on a schedule that is currently OFF — it will not run until you enable it")
	}

	line := strings.Join(parts, ", ") + "."
	if p.ReplacedTargets == 1 {
		line += " One per-container schedule target was replaced by it."
	} else if p.ReplacedTargets > 1 {
		line += fmt.Sprintf(" %d per-container schedule targets were replaced by it.", p.ReplacedTargets)
	}
	if p.BackupStarted {
		line += " First backup started."
	}
	return line
}

// protectSummary renders the one-line "here's what I decided" text (B5).
func protectSummary(p protectResp) string {
	var parts []string
	if p.IsDatabase {
		eng := p.Engine
		if eng == "postgres" {
			eng = "PostgreSQL"
		} else if eng == "mysql" || eng == "mariadb" {
			eng = "MySQL/MariaDB"
		} else if eng == "mongodb" {
			eng = "MongoDB"
		}
		parts = append(parts, "Live "+eng+" dump")
	} else {
		parts = append(parts, "Consistent volume snapshot")
	}

	ext := []string{}
	for _, d := range p.Destinations {
		if d != "Local" {
			ext = append(ext, d)
		}
	}
	if len(ext) > 0 {
		parts = append(parts, "mirrored to "+strings.Join(ext, ", "))
	} else {
		parts = append(parts, "stored locally")
	}

	if p.ScheduleEnabled {
		parts = append(parts, "on the automatic schedule")
	} else {
		parts = append(parts, "added to the schedule, which is currently OFF — it will not run until you enable it")
	}

	line := strings.Join(parts, ", ") + "."
	if p.BackupStarted {
		line += " First backup started."
	}
	return line
}
