package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"dockback/internal/egress"
	"dockback/internal/store"
)

// Egress audit mode (F207).
//
// Turning on a default-deny allow-list is all-or-nothing, and getting it wrong
// fails in the worst possible way: silently, hours later, when a scheduled
// backup cannot reach a destination the operator forgot to list. The only way to
// find out what a list would break was to break it.
//
// Audit mode evaluates every outbound host exactly as enforcement would, records
// each one that WOULD have been refused, and lets the connection through. The
// operator turns enforcement on once that list is empty or accounted for.
//
// WHY AN OBSERVATION IS NOT AUTOMATICALLY SAFE TO ALLOW
//
// The dial-time guard is the one that catches HTTP redirects and DNS rebinds —
// a host arriving there may be somewhere DockBack was NEVER configured to reach,
// which is precisely the attack the allow-list exists to stop. So every
// observation is annotated against the configured-endpoint inventory
// (collectEgressHosts), and the ones that match nothing are marked. "Add all"
// takes only the recognised hosts; an unrecognised one has to be added
// deliberately, one at a time, having been told what it is.
//
// A one-click "allow everything that was observed" would let a redirect write
// itself into the security policy, which would make this feature a way to defeat
// the control it is meant to help configure.

const (
	// egressAuditSetting holds the observations as JSON. Plain settings storage:
	// these are hostnames DockBack tried to reach, which is not secret — the
	// allow-list itself is stored the same way, for the same reason.
	egressAuditSetting = "security.egress_audit_log"

	// egressAuditModeSetting is the dry-run toggle.
	egressAuditModeSetting = "security.egress_audit"

	// maxEgressAuditHosts bounds the record. Observations partly come from
	// redirects, so the set is not purely operator-controlled — an unbounded list
	// would be a way to grow the settings table from outside. Well above any real
	// deployment's endpoint count, so a legitimate audit never truncates.
	maxEgressAuditHosts = 200

	// maxEgressAuditHostLen bounds one entry. A hostname longer than this is not
	// a hostname anybody configured.
	maxEgressAuditHostLen = 253
)

// egressObservation is one host that would have been refused.
type egressObservation struct {
	Host      string `json:"host"`
	FirstSeen int64  `json:"first_seen"`
	LastSeen  int64  `json:"last_seen"`
	Count     int    `json:"count"`
}

// egressAuditEntry is an observation as the API reports it: the record plus what
// the host is, resolved fresh at read time so a destination renamed since the
// observation is described correctly.
type egressAuditEntry struct {
	egressObservation
	// Source names the configured endpoint this host belongs to
	// ("destination: Nextcloud", "notification: Gotify", …). Empty when nothing
	// configured uses it.
	Source string `json:"source,omitempty"`
	// Configured is the safety-relevant bit: false means DockBack was never told
	// to reach this host, so it arrived via a redirect, a rebind, or something
	// else worth looking at before allow-listing it.
	Configured bool `json:"configured"`
	// AllowedNow reports whether the CURRENT list already permits it — an
	// observation can be stale, and showing a resolved one as still-blocked would
	// send the operator chasing a problem they already fixed.
	AllowedNow bool `json:"allowed_now"`
}

// egressAuditRecorder is the observer installed into the egress package. It runs
// ON THE DIAL PATH, so it must be cheap: an in-memory map answers the common
// case (a host already seen) without touching the database, and only the FIRST
// sighting of each host writes. That bounds total writes to maxEgressAuditHosts
// for the life of an audit, no matter how many connections are made.
type egressAuditRecorder struct {
	store *store.Store
	mu    sync.Mutex
	seen  map[string]*egressObservation
	full  bool // cap reached — stop accepting new hosts, keep counting known ones
}

func newEgressAuditRecorder(st *store.Store) *egressAuditRecorder {
	r := &egressAuditRecorder{store: st, seen: map[string]*egressObservation{}}
	r.load()
	return r
}

// load restores previous observations so a restart does not erase an audit an
// operator is halfway through reviewing.
func (r *egressAuditRecorder) load() {
	js, _ := r.store.GetSetting(egressAuditSetting, "")
	if strings.TrimSpace(js) == "" {
		return
	}
	var list []egressObservation
	if err := json.Unmarshal([]byte(js), &list); err != nil {
		return
	}
	for i := range list {
		o := list[i]
		if o.Host == "" {
			continue
		}
		r.seen[o.Host] = &o
	}
	r.full = len(r.seen) >= maxEgressAuditHosts
}

// record notes one would-be-denied host. Called from the dial path.
func (r *egressAuditRecorder) record(host string) {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" || len(host) > maxEgressAuditHostLen {
		return
	}
	now := time.Now().Unix()

	r.mu.Lock()
	o, known := r.seen[host]
	if known {
		// Already recorded: update in memory only. The counts are advisory and a
		// database write per connection would put storage I/O on the dial path.
		o.LastSeen, o.Count = now, o.Count+1
		r.mu.Unlock()
		return
	}
	if r.full {
		r.mu.Unlock()
		return
	}
	r.seen[host] = &egressObservation{Host: host, FirstSeen: now, LastSeen: now, Count: 1}
	r.full = len(r.seen) >= maxEgressAuditHosts
	snapshot := r.snapshotLocked()
	r.mu.Unlock()

	// First sighting only — at most maxEgressAuditHosts writes for a whole audit.
	b, err := json.Marshal(snapshot)
	if err != nil {
		return
	}
	_ = r.store.SetSetting(egressAuditSetting, string(b))
}

// snapshotLocked copies the current observations, newest-activity first. Caller
// holds the mutex.
func (r *egressAuditRecorder) snapshotLocked() []egressObservation {
	out := make([]egressObservation, 0, len(r.seen))
	for _, o := range r.seen {
		out = append(out, *o)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastSeen != out[j].LastSeen {
			return out[i].LastSeen > out[j].LastSeen
		}
		return out[i].Host < out[j].Host
	})
	return out
}

// list returns the observations for the API.
func (r *egressAuditRecorder) list() []egressObservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotLocked()
}

// clear discards the record, in memory and on disk.
func (r *egressAuditRecorder) clear() error {
	r.mu.Lock()
	r.seen = map[string]*egressObservation{}
	r.full = false
	r.mu.Unlock()
	return r.store.SetSetting(egressAuditSetting, "")
}

// flush persists the in-memory counts. Called when the audit is read, so the
// advisory counts survive a restart without any write on the dial path.
func (r *egressAuditRecorder) flush() {
	r.mu.Lock()
	snapshot := r.snapshotLocked()
	r.mu.Unlock()
	if len(snapshot) == 0 {
		return
	}
	if b, err := json.Marshal(snapshot); err == nil {
		_ = r.store.SetSetting(egressAuditSetting, string(b))
	}
}

// installEgressAudit wires the recorder into the egress package and applies the
// persisted mode at boot (F207). Called once from NewServer, before anything can
// dial — an audit whose mode did not survive a restart would silently start
// enforcing a list the operator was still testing.
func (s *Server) installEgressAudit() {
	s.egressAudit = newEgressAuditRecorder(s.store)
	egress.SetObserver(s.egressAudit.record)
	on, _ := s.store.GetSetting(egressAuditModeSetting, "")
	egress.SetAuditMode(strings.TrimSpace(on) == "true")
}

// handleEgressAudit reports the audit state: whether the dry run is on, and
// every host enforcement would have refused, annotated with what it is (F207).
//
// The annotation is resolved HERE rather than stored, so a destination renamed
// or removed since the observation is described as it is now — and so a host
// already added to the allow-list shows as resolved instead of sending the
// operator after a problem they have already fixed.
func (s *Server) handleEgressAudit(w http.ResponseWriter, r *http.Request) {
	if s.egressAudit == nil {
		writeJSON(w, http.StatusOK, map[string]any{"audit_mode": egress.AuditMode(), "entries": []egressAuditEntry{}})
		return
	}
	// Persist the counts accumulated in memory since the last read.
	s.egressAudit.flush()

	// What DockBack is actually configured to reach, so an observation can be
	// told apart from a redirect to somewhere nobody asked for.
	known := map[string]string{}
	for _, sug := range s.configuredEgressHosts() {
		known[strings.ToLower(sug.Host)] = sug.Source
	}

	obs := s.egressAudit.list()
	out := make([]egressAuditEntry, 0, len(obs))
	unconfigured := 0
	for _, o := range obs {
		src, ok := known[o.Host]
		if !ok {
			unconfigured++
		}
		out = append(out, egressAuditEntry{
			egressObservation: o,
			Source:            src,
			Configured:        ok,
			AllowedNow:        egress.Default().Check(o.Host) == nil,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"audit_mode":   egress.AuditMode(),
		"entries":      out,
		"unconfigured": unconfigured,
		"truncated":    len(obs) >= maxEgressAuditHosts,
	})
}

// handleEgressAuditClear discards the observations so a fresh audit starts from
// nothing — the natural thing to do after fixing the allow-list and before
// re-running the workloads to confirm.
func (s *Server) handleEgressAuditClear(w http.ResponseWriter, r *http.Request) {
	if s.egressAudit != nil {
		if err := s.egressAudit.clear(); err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	_ = s.store.Audit(userFrom(r), "egress.audit.clear", "", "cleared the observed would-be-denied hosts")
	writeJSON(w, http.StatusOK, map[string]any{"cleared": true})
}

// configuredEgressHosts is the endpoint inventory the suggestions endpoint
// builds, reused here so "is this host one we configured" has exactly one
// definition. Shares its decrypt-and-extract path rather than reimplementing it.
func (s *Server) configuredEgressHosts() []egressSuggestion {
	var dests []destEndpoint
	gather := func(list []*store.Destination, prefix string) {
		for _, d := range list {
			cfg, err := s.decryptDestConfig(d)
			if err != nil {
				continue
			}
			dests = append(dests, destEndpoint{label: prefix + d.Name, typ: d.Type, cfg: cfg})
		}
	}
	if ds, err := s.store.ListDestinations(); err == nil {
		gather(ds, "destination: ")
	}
	if ds, err := s.store.ListAppDestinations(); err == nil {
		gather(ds, "app-backup destination: ")
	}
	notifyCfg, _ := s.loadNotifyConfig()
	nodes, _ := s.store.ListNodes()
	return collectEgressHosts(dests, notifyCfg, nodes)
}
