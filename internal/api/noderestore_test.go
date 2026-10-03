package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dockback/internal/backup"
	"dockback/internal/crypto"
	"dockback/internal/store"
)

// seedBackup inserts a successful backup with a manifest into the test store.
func seedBackup(t *testing.T, st *store.Store, id, node, stack, target string, createdAt int64, man backup.Manifest) {
	t.Helper()
	if err := st.CreateBackup(&store.Backup{ID: id, NodeID: node, Stack: stack, TargetName: target, Status: "success", CreatedAt: createdAt}); err != nil {
		t.Fatalf("create backup %s: %v", id, err)
	}
	mj, _ := json.Marshal(man)
	if err := st.UpdateBackup(&store.Backup{ID: id, Status: "success", Verified: "verified", ManifestJSON: string(mj), CompletedAt: createdAt}); err != nil {
		t.Fatalf("update backup %s: %v", id, err)
	}
}

// F6: nodeRestorePlan must order databases before applications, group by stack,
// carry the newest backup id + container id, and number services 1..N.
func TestNodeRestorePlanOrdering(t *testing.T) {
	st := testStore(t)
	s := &Server{store: st}

	// A stack "immich" with an app (immich-server) and a postgres db; plus a
	// standalone app "caddy". Databases must come first regardless of insert order.
	seedBackup(t, st, "app-old", "n1", "immich", "immich-server", 100, backup.Manifest{ContainerID: "c-app-old", Stack: "immich", Service: "server", Image: "immich:1", Volumes: []backup.VolumeRef{{Destination: "/data"}}})
	seedBackup(t, st, "app-new", "n1", "immich", "immich-server", 200, backup.Manifest{ContainerID: "c-app-new", Stack: "immich", Service: "server", Image: "immich:2", Volumes: []backup.VolumeRef{{Destination: "/data"}}})
	seedBackup(t, st, "db-1", "n1", "immich", "immich-postgres", 150, backup.Manifest{ContainerID: "c-db", Stack: "immich", Service: "postgres", Image: "pg:16", Databases: []backup.DBDump{{Engine: "postgres", Service: "postgres"}}})
	seedBackup(t, st, "caddy-1", "n1", "", "caddy", 120, backup.Manifest{ContainerID: "c-caddy", Image: "caddy:2", Volumes: []backup.VolumeRef{{Destination: "/config"}}})
	// A failed backup must never be chosen.
	if err := st.CreateBackup(&store.Backup{ID: "app-failed", NodeID: "n1", Stack: "immich", TargetName: "immich-server", Status: "failed", CreatedAt: 999}); err != nil {
		t.Fatal(err)
	}

	plan := s.nodeRestorePlan("n1")
	if len(plan) != 3 {
		t.Fatalf("want 3 services (newest per target), got %d: %+v", len(plan), plan)
	}

	// Databases first.
	if plan[0].Role != "database" {
		t.Errorf("plan[0] role = %q, want database (DBs restore first)", plan[0].Role)
	}
	for i := 1; i < len(plan); i++ {
		if plan[i].Role == "database" && plan[i-1].Role != "database" {
			t.Errorf("a database appears after an application at index %d — order broken", i)
		}
	}

	// Order numbering is 1..N in slice order.
	for i, svc := range plan {
		if svc.Order != i+1 {
			t.Errorf("service %s Order = %d, want %d", svc.Container, svc.Order, i+1)
		}
	}

	// The executor fields must carry the NEWEST backup id + its container id.
	byTarget := map[string]runbookService{}
	for _, svc := range plan {
		byTarget[svc.Container] = svc
	}
	if got := byTarget["immich-server"]; got.backupID != "app-new" || got.containerID != "c-app-new" {
		t.Errorf("immich-server should use newest backup app-new/c-app-new, got %s/%s", got.backupID, got.containerID)
	}
	if got := byTarget["immich-postgres"]; got.backupID != "db-1" || got.containerID != "c-db" {
		t.Errorf("immich-postgres should use db-1/c-db, got %s/%s", got.backupID, got.containerID)
	}

	// Lock keys: the stack collapses to one key; the standalone keys by name.
	keys := nodeRestoreLockKeys("n1", plan)
	if len(keys) != 2 {
		t.Fatalf("want 2 distinct lock keys (immich stack + caddy standalone), got %d: %v", len(keys), keys)
	}
}

// ---------------------------------------------------------------------------
// F211 — the whole-node restore says what it will do before it does it.
//
// It used to discover a service it could not restore at that service's TURN —
// seven of twelve — and abort there, leaving six containers recreated and five
// untouched. Everything it can fail on is knowable from the manifest, so it is
// now known before anything is touched, and the operator either fixes it or says
// explicitly which services to leave out.
// ---------------------------------------------------------------------------

// seedNodeBackup is seedBackup plus the node row the handlers resolve first.
func seedNodeBackup(t *testing.T, s *Server, id, node, target string, man backup.Manifest) {
	t.Helper()
	seedBackup(t, s.store, id, node, man.Stack, target, time.Now().Unix(), man)
	if err := s.store.UpsertNode(&store.Node{ID: node, Name: node, Transport: "socket", Address: "unix:///var/run/docker.sock"}); err != nil {
		t.Fatal(err)
	}
}

func nodePlanRows(t *testing.T, s *Server) []nodeRestorePlanEntry {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/nodes/n1/restore-all/plan", nil)
	r.SetPathValue("id", "n1")
	rec := httptest.NewRecorder()
	s.handleRestoreNodeAllPlan(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("plan = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Services []nodeRestorePlanEntry `json:"services"`
		Blocked  int                    `json:"blocked"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Services
}

func postNodeRestoreAll(s *Server, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/nodes/n1/restore-all", strings.NewReader(body))
	r.SetPathValue("id", "n1")
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
	rec := httptest.NewRecorder()
	s.handleRestoreNodeAll(rec, r)
	return rec
}

func rowsByContainer(rows []nodeRestorePlanEntry) map[string]nodeRestorePlanEntry {
	m := map[string]nodeRestorePlanEntry{}
	for _, e := range rows {
		m[e.Container] = e
	}
	return m
}

// AC1 — every service in execution order, with `blocked` on the one that cannot
// be restored this way.
func TestNodeRestorePlanListsOrderAndBlocked(t *testing.T) {
	s := stepUpRestoreServer(t)
	pub, _, err := crypto.NewBackupKeypair()
	if err != nil {
		t.Fatal(err)
	}
	seedNodeBackup(t, s, "b-db", "n1", "prod-db", backup.Manifest{
		ContainerID: "cid-db", Databases: []backup.DBDump{{Engine: "postgres"}},
	})
	seedNodeBackup(t, s, "b-app", "n1", "prod-app", backup.Manifest{ContainerID: "cid-app"})
	// A write-only member: the whole-node executor supplies no offline key, so it
	// genuinely cannot restore this one.
	seedNodeBackup(t, s, "b-wo", "n1", "termix", backup.Manifest{
		ContainerID: "cid-wo", WrappedKeyPub: "sealed", BackupPubFP: crypto.BackupPubFP(pub),
	})

	rows := nodePlanRows(t, s)
	if len(rows) != 3 {
		t.Fatalf("want all three services, got %d: %+v", len(rows), rows)
	}
	for i, e := range rows {
		if e.Order != i+1 {
			t.Errorf("row %d has order %d — Order is the execution sequence", i, e.Order)
		}
		if e.BackupID == "" || e.Label == "" {
			t.Errorf("every row needs a backup and a label: %+v", e)
		}
	}
	if rows[0].Role != "database" {
		t.Errorf("databases restore first, got %q first", rows[0].Role)
	}

	byName := rowsByContainer(rows)
	wo := byName["termix"]
	if !wo.WriteOnly {
		t.Error("the write-only member must be flagged")
	}
	if wo.Blocked == "" {
		t.Fatal("a write-only member cannot be restored by a whole-node run — it must be blocked")
	}
	if !strings.Contains(wo.Blocked, "offline private key") {
		t.Errorf("the reason should say why: %q", wo.Blocked)
	}
	if byName["prod-db"].Blocked != "" || byName["prod-app"].Blocked != "" {
		t.Error("an ordinary member must not be blocked")
	}
	if byName["prod-db"].Verified != "verified" {
		t.Errorf("the plan should carry the verified state: %q", byName["prod-db"].Verified)
	}
}

// AC2 — a blocked member without skip_blocked is a 409 naming it, and nothing is
// started.
func TestNodeRestoreAllRefusesBlockedMembers(t *testing.T) {
	s := stepUpRestoreServer(t)
	pub, _, _ := crypto.NewBackupKeypair()
	seedNodeBackup(t, s, "b-app", "n1", "prod-app", backup.Manifest{ContainerID: "cid-app"})
	seedNodeBackup(t, s, "b-wo", "n1", "termix", backup.Manifest{
		ContainerID: "cid-wo", WrappedKeyPub: "sealed", BackupPubFP: crypto.BackupPubFP(pub),
	})

	rec := postNodeRestoreAll(s, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "termix") {
		t.Errorf("the refusal must name the blocked service: %s", rec.Body.String())
	}
	for _, e := range mustAudit(t, s) {
		if e.Action == "node.restore.all" {
			t.Errorf("a refused run must not be audited as started: %+v", e)
		}
	}
	// And no lock was left held by the refusal.
	k := stackKey("n1", "", "prod-app")
	if !s.locks.acquireRestore(k) {
		t.Error("no lock may be held after a refusal")
	}
	s.locks.releaseRestore(k)
}

// AC3 — with skip_blocked the run starts.
func TestNodeRestoreAllSkipsBlockedWhenAsked(t *testing.T) {
	s := stepUpRestoreServer(t)
	pub, _, _ := crypto.NewBackupKeypair()
	seedNodeBackup(t, s, "b-app", "n1", "prod-app", backup.Manifest{ContainerID: "cid-app"})
	seedNodeBackup(t, s, "b-wo", "n1", "termix", backup.Manifest{
		ContainerID: "cid-wo", WrappedKeyPub: "sealed", BackupPubFP: crypto.BackupPubFP(pub),
	})

	if rec := postNodeRestoreAll(s, `{"skip_blocked":true}`); rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	found := false
	for _, e := range mustAudit(t, s) {
		if e.Action == "node.restore.all" {
			found = true
		}
	}
	if !found {
		t.Error("the run should be audited as started")
	}
}

// Every member blocked leaves nothing to do — refused rather than started empty.
func TestNodeRestoreAllRefusesWhenEverythingIsBlocked(t *testing.T) {
	s := stepUpRestoreServer(t)
	pub, _, _ := crypto.NewBackupKeypair()
	seedNodeBackup(t, s, "b-wo", "n1", "termix", backup.Manifest{
		ContainerID: "cid-wo", WrappedKeyPub: "sealed", BackupPubFP: crypto.BackupPubFP(pub),
	})
	rec := postNodeRestoreAll(s, `{"skip_blocked":true}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "nothing left to restore") {
		t.Errorf("the refusal should say the run would be empty: %s", rec.Body.String())
	}
}

// A node with nothing blocked starts exactly as before — the regression that
// matters, since this is the 2 a.m. button.
func TestNodeRestoreAllUnblockedIsUnchanged(t *testing.T) {
	s := stepUpRestoreServer(t)
	seedNodeBackup(t, s, "b-app", "n1", "prod-app", backup.Manifest{ContainerID: "cid-app"})
	seedNodeBackup(t, s, "b-db", "n1", "prod-db", backup.Manifest{
		ContainerID: "cid-db", Databases: []backup.DBDump{{Engine: "postgres"}},
	})

	if rec := postNodeRestoreAll(s, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("an unblocked node must start with no body at all: %d %s", rec.Code, rec.Body.String())
	}
	for _, e := range nodePlanRows(t, s) {
		if e.Blocked != "" {
			t.Errorf("nothing should be blocked here: %+v", e)
		}
	}
}

// A standalone-volume backup is NOT blocked. Its manifest records no container
// id, which the old guard treated as unrestorable and aborted the whole node
// restore over — but Engine.Restore dispatches "volume:" to the volume path,
// which needs no container at all.
func TestNodeRestorePlanDoesNotBlockAStandaloneVolume(t *testing.T) {
	s := stepUpRestoreServer(t)
	seedNodeBackup(t, s, "b-vol", "n1", "volume:media-cache", backup.Manifest{}) // no ContainerID
	seedNodeBackup(t, s, "b-app", "n1", "prod-app", backup.Manifest{ContainerID: "cid-app"})

	vol, ok := rowsByContainer(nodePlanRows(t, s))["volume:media-cache"]
	if !ok {
		t.Fatal("the volume backup should appear in the plan")
	}
	if vol.Blocked != "" {
		t.Errorf("a standalone volume is restorable and must not be blocked: %q", vol.Blocked)
	}
	// So the run starts with no body — previously one orphan volume aborted it.
	if rec := postNodeRestoreAll(s, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("an orphan volume must not block the node restore: %d %s", rec.Code, rec.Body.String())
	}
}

// A container backup that truly records no container is still blocked — the
// volume exemption must not become a blanket one.
func TestNodeRestorePlanBlocksAContainerBackupWithNoContainerID(t *testing.T) {
	s := stepUpRestoreServer(t)
	seedNodeBackup(t, s, "b-broken", "n1", "legacy-app", backup.Manifest{}) // not a volume

	rows := nodePlanRows(t, s)
	if len(rows) != 1 {
		t.Fatalf("want one row, got %d", len(rows))
	}
	if rows[0].Blocked == "" {
		t.Fatal("a container backup with no recorded container cannot be restored")
	}
	if !strings.Contains(rows[0].Blocked, "no container") {
		t.Errorf("the reason should say so: %q", rows[0].Blocked)
	}
}

// The plan and the executor share ONE definition of blocked, so the preview and
// the destructive run can never disagree.
func TestNodeRestoreBlockReasonIsShared(t *testing.T) {
	s := stepUpRestoreServer(t)
	pub, _, _ := crypto.NewBackupKeypair()

	wo := &store.Backup{ID: "b1", TargetName: "termix"}
	woMan := &backup.Manifest{WrappedKeyPub: "sealed", BackupPubFP: crypto.BackupPubFP(pub)}
	if s.nodeRestoreBlockReason(runbookService{backupID: "b1", containerID: "c"}, wo, woMan, false) == "" {
		t.Error("write-only with no key must block")
	}
	// F212: with the offline key in hand it is restorable, so it must NOT block.
	if got := s.nodeRestoreBlockReason(runbookService{backupID: "b1", containerID: "c"}, wo, woMan, true); got != "" {
		t.Errorf("a supplied key unblocks a write-only member: %q", got)
	}
	plain := &store.Backup{ID: "b2", TargetName: "app"}
	if got := s.nodeRestoreBlockReason(runbookService{backupID: "b2", containerID: "c"}, plain, &backup.Manifest{}, false); got != "" {
		t.Errorf("an ordinary member must not block: %q", got)
	}
	// The condition the original guard named, which the plan builder can never
	// actually produce but the executor still checks.
	if s.nodeRestoreBlockReason(runbookService{}, nil, &backup.Manifest{}, false) == "" {
		t.Error("a service with no backup must block")
	}
}

// ---------------------------------------------------------------------------
// F212 — "Restore entire node" can target a DIFFERENT machine.
//
// It could only ever target the node whose death is the reason you need it. So
// rebuilding a dead host meant opening the stack dialog once per stack, picking
// the target and re-entering the same remaps each time, then restoring the
// standalone containers one by one.
// ---------------------------------------------------------------------------

func addNode(t *testing.T, s *Server, id, addr string) {
	t.Helper()
	if err := s.store.UpsertNode(&store.Node{ID: id, Name: id, Transport: "tcp", Address: addr}); err != nil {
		t.Fatal(err)
	}
}

// AC1 — a rebuild onto another node is accepted, and the run is recorded as
// landing there.
func TestNodeRestoreAllRebuildsOntoAnotherNode(t *testing.T) {
	s := stepUpRestoreServer(t)
	seedNodeBackup(t, s, "b-app", "n1", "prod-app", backup.Manifest{ContainerID: "cid-app"})
	addNode(t, s, "n2", "tcp://10.168.1.20:2375")

	rec := postNodeRestoreAll(s, `{"target_node":"n2"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	found := false
	for _, e := range mustAudit(t, s) {
		if e.Action == "node.restore.all" {
			found = true
			if !strings.Contains(e.Detail, "-> n2") {
				t.Errorf("the audit row must record where it landed: %q", e.Detail)
			}
		}
	}
	if !found {
		t.Fatal("the rebuild should be audited")
	}
}

// An unknown target is refused before anything is locked or started.
func TestNodeRestoreAllRejectsAnUnknownTarget(t *testing.T) {
	s := stepUpRestoreServer(t)
	seedNodeBackup(t, s, "b-app", "n1", "prod-app", backup.Manifest{ContainerID: "cid-app"})

	rec := postNodeRestoreAll(s, `{"target_node":"ghost"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	for _, e := range mustAudit(t, s) {
		if e.Action == "node.restore.all" {
			t.Error("a refused rebuild must not be audited as started")
		}
	}
}

// AC2 — blank IP-remap endpoints derive from the two nodes' addresses, and an
// underivable pair is a refusal rather than a silent no-op.
func TestNodeRestoreAllDerivesTheIPRemap(t *testing.T) {
	s := stepUpRestoreServer(t)
	seedNodeBackup(t, s, "b-app", "n1", "prod-app", backup.Manifest{ContainerID: "cid-app"})
	// n1 was seeded with a unix socket address, which yields no IP — so the
	// derivation must fail closed rather than remap nothing.
	addNode(t, s, "n2", "tcp://10.168.1.20:2375")
	rec := postNodeRestoreAll(s, `{"target_node":"n2","remap_ip":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an underivable endpoint must be refused, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "could not derive") {
		t.Errorf("the refusal should say why: %s", rec.Body.String())
	}

	// With both nodes addressable by IP, the endpoints derive and it proceeds.
	s2 := stepUpRestoreServer(t)
	seedNodeBackup(t, s2, "b-app", "n1", "prod-app", backup.Manifest{ContainerID: "cid-app"})
	addNode(t, s2, "n1", "tcp://10.168.1.10:2375")
	addNode(t, s2, "n2", "tcp://10.168.1.20:2375")
	if rec := postNodeRestoreAll(s2, `{"target_node":"n2","remap_ip":true}`); rec.Code != http.StatusAccepted {
		t.Fatalf("derivable endpoints must proceed, got %d: %s", rec.Code, rec.Body.String())
	}

	// An explicitly bad address is refused too.
	if rec := postNodeRestoreAll(s2, `{"target_node":"n2","remap_ip":true,"remap_from_ip":"not-an-ip","remap_to_ip":"10.168.1.20"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("a malformed IP = %d, want 400", rec.Code)
	}
}

// The path remap's TARGET base is validated fail-closed, exactly as the stack
// dialog validates it — a system root must never become a bind destination.
func TestNodeRestoreAllRefusesAProtectedPathTarget(t *testing.T) {
	s := stepUpRestoreServer(t)
	seedNodeBackup(t, s, "b-app", "n1", "prod-app", backup.Manifest{ContainerID: "cid-app"})
	addNode(t, s, "n2", "tcp://10.168.1.20:2375")

	rec := postNodeRestoreAll(s, `{"target_node":"n2","remap_path":true,"remap_from_path":"/opt/docker","remap_to_path":"/etc"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a protected system path must be refused, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "protected system path") {
		t.Errorf("the refusal should name the reason: %s", rec.Body.String())
	}
}

// AC3 — a same-node restore-all with no new fields behaves exactly as before.
func TestNodeRestoreAllSameNodeIsUnchanged(t *testing.T) {
	s := stepUpRestoreServer(t)
	seedNodeBackup(t, s, "b-app", "n1", "prod-app", backup.Manifest{ContainerID: "cid-app"})

	if rec := postNodeRestoreAll(s, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("no body at all must still start: %d %s", rec.Code, rec.Body.String())
	}
	for _, e := range mustAudit(t, s) {
		if e.Action == "node.restore.all" && strings.Contains(e.Detail, "->") {
			t.Errorf("a same-node run must not read as a rebuild: %q", e.Detail)
		}
	}
}

// F212: with the offline key supplied, a write-only member stops being blocked —
// the whole-node run can now include it.
func TestNodeRestoreAllUnblocksWriteOnlyWithAKey(t *testing.T) {
	s := stepUpRestoreServer(t)
	pub, priv, err := crypto.NewBackupKeypair()
	if err != nil {
		t.Fatal(err)
	}
	seedNodeBackup(t, s, "b-wo", "n1", "termix", backup.Manifest{
		ContainerID: "cid-wo", WrappedKeyPub: "sealed", BackupPubFP: crypto.BackupPubFP(pub),
	})

	// Without a key it is blocked, and the reason now says a key would help.
	rows := nodePlanRows(t, s)
	if rows[0].Blocked == "" {
		t.Fatal("no key means blocked")
	}
	if !strings.Contains(rows[0].Blocked, "paste the offline private key") {
		t.Errorf("the reason should point at the way out: %q", rows[0].Blocked)
	}
	if rec := postNodeRestoreAll(s, ""); rec.Code != http.StatusConflict {
		t.Fatalf("blocked with no key = %d, want 409", rec.Code)
	}

	// With it, the run is accepted and the key is recorded as a fact only.
	rec := postNodeRestoreAll(s, `{"private_key":"`+priv+`"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("the key must unblock it, got %d: %s", rec.Code, rec.Body.String())
	}
	for _, e := range mustAudit(t, s) {
		if strings.Contains(e.Detail, priv) {
			t.Fatalf("the private key reached the audit trail: %q", e.Detail)
		}
		if e.Action == "node.restore.all" && !strings.Contains(e.Detail, "write-only key supplied") {
			t.Errorf("the FACT should be recorded: %q", e.Detail)
		}
	}
}

// The plan is grouped into units: a compose project restores as ONE step through
// the stack path, keeping its dependency order, its atomic-set handling and its
// merged compose file. Each unit sits at the position of its first member.
func TestNodeRestoreUnitsGroupStacksInPlace(t *testing.T) {
	plan := []runbookService{
		{Container: "blog-db", Stack: "blog", Role: "database"},
		{Container: "shop-db", Stack: "shop", Role: "database"},
		{Container: "caddy", Role: "application"},
		{Container: "blog-app", Stack: "blog", Role: "application"},
		{Container: "shop-app", Stack: "shop", Role: "application"},
	}
	units := nodeRestoreUnits(plan)
	if len(units) != 3 {
		t.Fatalf("want blog, shop and caddy as three units, got %d: %+v", len(units), units)
	}
	if units[0].Stack != "blog" || len(units[0].Services) != 2 {
		t.Errorf("blog must be one unit of two services, got %+v", units[0])
	}
	if units[1].Stack != "shop" || len(units[1].Services) != 2 {
		t.Errorf("shop must be one unit of two services, got %+v", units[1])
	}
	if units[2].Stack != "" || units[2].Services[0].Container != "caddy" {
		t.Errorf("the standalone container keeps its own step, got %+v", units[2])
	}
	// Every service survives the grouping — a rebuild that silently dropped one
	// would be the worst possible bug in this feature.
	seen := 0
	for _, u := range units {
		seen += len(u.Services)
	}
	if seen != len(plan) {
		t.Errorf("grouping lost services: %d of %d", seen, len(plan))
	}
	if len(nodeRestoreUnits(nil)) != 0 {
		t.Error("an empty plan is no units")
	}
	if got := unitLabel(units[0]); got != "stack blog" {
		t.Errorf("unit label = %q", got)
	}
}

// The shared cross-host grammar behaves the same whichever door it came through.
func TestParseCrossRestoreParamsSharedGrammar(t *testing.T) {
	s := stepUpRestoreServer(t)
	addNode(t, s, "n1", "tcp://10.168.1.10:2375")
	addNode(t, s, "n2", "tcp://10.168.1.20:2375")
	ctx := context.Background()

	// Nothing asked for, nothing returned.
	got, err := s.parseCrossRestoreParams(ctx, crossRestoreInput{}, "n1", "n2", "")
	if err != nil {
		t.Fatalf("an empty request is valid: %v", err)
	}
	if got.RemapFromIP != "" || got.RemapToIP != "" || got.RemapFromPath != "" {
		t.Errorf("nothing asked for must stay empty: %+v", got)
	}

	// IP endpoints derive from the node addresses.
	got, err = s.parseCrossRestoreParams(ctx, crossRestoreInput{RemapIP: true}, "n1", "n2", "")
	if err != nil {
		t.Fatalf("derivable endpoints: %v", err)
	}
	if got.RemapFromIP != "10.168.1.10" || got.RemapToIP != "10.168.1.20" {
		t.Errorf("derived %q -> %q", got.RemapFromIP, got.RemapToIP)
	}

	// A whole-node path remap with no source base is deferred, not refused —
	// each stack records its own layout.
	got, err = s.parseCrossRestoreParams(ctx, crossRestoreInput{RemapPath: true, RemapToPath: "/opt/stacks"}, "n1", "n2", "")
	if err != nil {
		t.Fatalf("a deferred path remap is valid: %v", err)
	}
	if !got.PathRemapPending {
		t.Error("with no source base the node path must resolve it per stack")
	}
	// …but a protected target base is refused right there, because it is the same
	// for every stack and must not be discovered mid-run.
	if _, err := s.parseCrossRestoreParams(ctx, crossRestoreInput{RemapPath: true, RemapToPath: "/etc"}, "n1", "n2", ""); err == nil {
		t.Error("a protected system path must be refused up front")
	}
	// A domain remap is held to the plain-hostname grammar.
	if _, err := s.parseCrossRestoreParams(ctx, crossRestoreInput{RemapDomain: true, RemapFromDomain: "https://a.example", RemapToDomain: "b.example"}, "n1", "n2", ""); err == nil {
		t.Error("a scheme is not a domain")
	}
}

// AC1, precisely: a rebuild names the destination on EVERY step, not only in the
// header — somebody watching a twelve-step rebuild scroll past should never have
// to page back to be sure which machine it is landing on. And AC3: a same-node
// restore's log is unchanged, with no destination suffix anywhere.
//
// Driven through the real executor and read back from the persisted run log, so
// this asserts the lines an operator actually sees. The units fail immediately
// (their backups do not exist), which is fine: the step line is emitted before
// the restore is attempted, and that line is what is under test.
func TestNodeRestoreLogNamesTheTargetOnEveryStep(t *testing.T) {
	lines := func(sourceID, targetID, targetName string) []string {
		s := stepUpRestoreServer(t)
		logID := "node:" + sourceID
		s.runNodeRestore(context.Background(), nodeRestoreRun{
			SourceID: sourceID, TargetID: targetID, TargetName: targetName,
			LogID: logID,
			Plan: []runbookService{
				{Container: "caddy", Role: "application", backupID: "b-missing", containerID: "c1"},
				{Container: "blog-db", Stack: "blog", Role: "database", backupID: "b2", containerID: "c2"},
			},
		})
		got, err := s.store.GetRunLog(logID)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(got))
		for _, l := range got {
			out = append(out, l.Msg)
		}
		if len(out) == 0 {
			t.Fatal("the run should have logged something")
		}
		return out
	}

	// Cross-host: the header AND every step line name the target.
	cross := lines("n1", "n2", "acer")
	header, steps := false, 0
	for _, l := range cross {
		if strings.Contains(l, "Rebuilding onto") && strings.Contains(l, "acer") {
			header = true
		}
		if strings.HasPrefix(l, "[") && strings.Contains(l, "Restoring ") {
			steps++
			if !strings.Contains(l, "onto acer") {
				t.Errorf("every step of a rebuild must name where it lands: %q", l)
			}
		}
	}
	if !header {
		t.Errorf("the run should announce the destination up front: %v", cross)
	}
	if steps == 0 {
		t.Fatalf("expected at least one step line: %v", cross)
	}

	// Same node: no destination suffix anywhere, so the log reads as it always did.
	for _, l := range lines("n1", "n1", "n1") {
		if strings.Contains(l, " onto ") || strings.Contains(l, "Rebuilding onto") {
			t.Errorf("a same-node restore must not gain a destination suffix: %q", l)
		}
	}
}

// F227 — choosing what a node rebuild covers.
//
// The default is the whole node, exactly as before. A selection narrows it, and
// is validated against the SERVER's plan rather than taken on the request's word
// — the catalog can change between the preview and the confirm, and which
// containers get overwritten is not a decision to delegate to a client.

func TestTrimmedSetIgnoresBlanks(t *testing.T) {
	got := trimmedSet([]string{" blog/app ", "", "   ", "solo"})
	if len(got) != 2 || !got["blog/app"] || !got["solo"] {
		t.Fatalf("padded names are still names, blanks are not: %v", got)
	}
	// An empty result is how "no selection" is told apart from "nothing selected".
	if len(trimmedSet(nil)) != 0 || len(trimmedSet([]string{"  "})) != 0 {
		t.Error("nothing usable must read as no selection at all")
	}
}

// An application whose services are only meaningful together is rebuilt whole or
// not at all — the engine refuses a partial set, so the refusal belongs here,
// before anything is overwritten, not after the confirm.
func TestValidateAtomicSelectionRefusesAPartialSet(t *testing.T) {
	rows := []nodeRestorePlanEntry{
		{Label: "immich/immich-db", Stack: "immich", Atomic: true},
		{Label: "immich/immich-server", Stack: "immich", Atomic: true},
		{Label: "immich/immich-redis", Stack: "immich"}, // not part of the atomic set
		{Label: "blog/app", Stack: "blog"},
		{Label: "solo"},
	}

	// Half of the atomic set: refused, and the refusal names what is missing.
	msg := validateAtomicSelection(rows, map[string]bool{"immich/immich-server": true})
	if msg == "" {
		t.Fatal("restoring half of an atomic application must be refused")
	}
	for _, want := range []string{"immich", "immich-db", "nothing has been changed"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal must name %q: %s", want, msg)
		}
	}

	// The whole atomic set, with the non-atomic sibling deliberately left out —
	// that is the case the operator asked for and it must be allowed.
	if msg := validateAtomicSelection(rows, map[string]bool{
		"immich/immich-db": true, "immich/immich-server": true,
	}); msg != "" {
		t.Errorf("dropping a NON-atomic member is the operator's call: %s", msg)
	}

	// The project untouched entirely: nothing to complain about.
	if msg := validateAtomicSelection(rows, map[string]bool{"solo": true}); msg != "" {
		t.Errorf("a project left out of the run is not a partial set: %s", msg)
	}
	// An ordinary stack can be split.
	if msg := validateAtomicSelection(rows, map[string]bool{"blog/app": true, "solo": true}); msg != "" {
		t.Errorf("an ordinary project has no such rule: %s", msg)
	}
}

// A blocked atomic member must not make its project unselectable: it is already
// excluded from the run, so it cannot be "missing" from a selection.
func TestValidateAtomicSelectionIgnoresBlockedMembers(t *testing.T) {
	rows := []nodeRestorePlanEntry{
		{Label: "immich/immich-db", Stack: "immich", Atomic: true},
		{Label: "immich/immich-server", Stack: "immich", Atomic: true, Blocked: "sealed to an offline key"},
	}
	if msg := validateAtomicSelection(rows, map[string]bool{"immich/immich-db": true}); msg != "" {
		t.Errorf("a blocked member is already out of the run and cannot be missing from a selection: %s", msg)
	}
}

// AC — the selection narrows and cannot widen. Whatever a request names, the run
// covers only what the server's own plan already contained.
func TestNodeRestoreSelectionCannotWiden(t *testing.T) {
	rows := []nodeRestorePlanEntry{{Label: "blog/app", Stack: "blog"}, {Label: "solo"}}
	sel := trimmedSet([]string{"blog/app", "something-that-is-not-on-this-node"})
	kept := 0
	for _, e := range rows {
		if sel[e.Label] {
			kept++
		}
	}
	if kept != 1 {
		t.Errorf("a name the plan does not contain must select nothing, got %d kept", kept)
	}
}
