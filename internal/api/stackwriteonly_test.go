package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dockback/internal/backup"
	"dockback/internal/config"
	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// F209 — the offline private key reaches the stack restore without reaching
// anything that keeps it.
//
// The key unlocks the entire backup history, so where it travels matters as much
// as that it travels. Every other option on this handler is a query parameter;
// this one deliberately is not, because a URL is written to the access log of
// every proxy in front of the app, kept in browser history, and sent in Referer
// headers.

const woStackKey = "an-offline-private-key-that-must-not-leak"

func stackKeyServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	if err := st.UpsertNode(&store.Node{ID: "n1", Name: "node-one", Transport: "socket", Address: "unix:///var/run/docker.sock"}); err != nil {
		t.Fatal(err)
	}
	return &Server{
		store: st,
		cfg:   &config.Config{EncryptionKey: key},
		locks: newOpLocks(),
		// The cached inventory the background refresh would fill in — F216 reads
		// it precisely because the node itself may be unreachable.
		stats:  map[string]*nodeStat{},
		engine: &backup.Engine{Store: st, Key: key, Log: func(string, string, string) {}},
	}
}

func postStackRestore(s *Server, body string) *httptest.ResponseRecorder {
	var r = httptest.NewRequest("POST", "/api/nodes/n1/stacks/blog/restore", strings.NewReader(body))
	r.SetPathValue("id", "n1")
	r.SetPathValue("project", "blog")
	rec := httptest.NewRecorder()
	s.handleRestoreStack(rec, r)
	return rec
}

// AC2 — with the key in the POST body, the restore is accepted and the audit
// trail records only that a key was supplied.
func TestStackRestoreAuditRecordsTheFactNotTheKey(t *testing.T) {
	s := stackKeyServer(t)

	rec := postStackRestore(s, `{"private_key":"`+woStackKey+`"}`)
	if rec.Code >= 400 {
		t.Fatalf("the body must be accepted: %d %s", rec.Code, rec.Body.String())
	}

	entries, err := s.store.ListAudit(100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		blob := e.Action + " " + e.Target + " " + e.Detail + " " + e.Actor
		if strings.Contains(blob, woStackKey) {
			t.Fatalf("the private key reached the audit trail: %q", blob)
		}
		if strings.HasPrefix(e.Action, "stack.") {
			found = true
			if !strings.Contains(e.Detail, "write-only key supplied") {
				t.Errorf("the FACT that a key was supplied should be recorded: %q", e.Detail)
			}
		}
	}
	if !found {
		t.Fatal("the restore should have been audited")
	}
}

// A restore with no key is audited exactly as before — no new mention, so the
// ordinary case reads identically.
func TestStackRestoreWithoutAKeyIsAuditedUnchanged(t *testing.T) {
	s := stackKeyServer(t)
	if rec := postStackRestore(s, ""); rec.Code >= 400 {
		t.Fatalf("an empty body must still be accepted: %d %s", rec.Code, rec.Body.String())
	}
	entries, _ := s.store.ListAudit(100)
	for _, e := range entries {
		if strings.HasPrefix(e.Action, "stack.") && strings.Contains(e.Detail, "write-only") {
			t.Errorf("no key means no mention: %q", e.Detail)
		}
	}
}

// The key must never become a query parameter. Asserted against the handler's
// SOURCE, because the regression to catch is somebody adding one more
// `q.Get(...)` beside the fifteen that are already there.
func TestStackRestoreKeyIsNeverReadFromTheURL(t *testing.T) {
	src, err := readSourceFile("stacks.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`q.Get("private_key")`, `Get("private_key")`, `qs.set("private_key"`} {
		if strings.Contains(src, bad) {
			t.Errorf("the private key must not be read from the query string (%s) — URLs reach access logs, history and Referer headers", bad)
		}
	}
	// And it IS read from the body.
	if !strings.Contains(src, "json:\"private_key\"") {
		t.Error("the handler should decode private_key from the request body")
	}
}

// readSourceFile reads a file from this package's own directory, so an invariant
// about how the handler is written can be asserted rather than described.
func readSourceFile(name string) (string, error) {
	b, err := os.ReadFile(name)
	return string(b), err
}

// A malformed body is refused rather than silently treated as "no key" — a
// truncated paste must not turn into a restore that stops mid-stack.
func TestStackRestoreRefusesAMalformedBody(t *testing.T) {
	s := stackKeyServer(t)
	rec := postStackRestore(s, `{"private_key": `)
	if rec.Code != 400 {
		t.Errorf("a malformed body = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// F214 — the source-copy picker offers copies the MEMBERS actually hold.
//
// A picker fed from the destination list would offer one where half the stack
// has no copy; choosing it makes bestLocation fall back to local for those
// members, silently — the exact outcome somebody reaching for "restore from the
// offsite copy, not this disk" is trying to avoid.
func TestStackSourceCopiesCountsMembersPerCopy(t *testing.T) {
	e := &backup.Engine{}
	entries := []backup.StackPlanEntry{
		{Service: "db", BackupID: "b-db"},
		{Service: "app", BackupID: "b-app"},
		{Service: "cache", BackupID: "b-cache"},
	}
	rows := map[string]*store.Backup{
		// db and app are mirrored to the NAS; cache never was.
		"b-db":    {ID: "b-db", LocationsJSON: `[{"kind":"local","name":"local","type":"local"},{"kind":"dest","dest_id":"d-nas","name":"NAS","type":"smb"}]`},
		"b-app":   {ID: "b-app", LocationsJSON: `[{"kind":"local","name":"local","type":"local"},{"kind":"dest","dest_id":"d-nas","name":"NAS","type":"smb"}]`},
		"b-cache": {ID: "b-cache", LocationsJSON: `[{"kind":"local","name":"local","type":"local"}]`},
	}

	got := stackSourceCopies(e, entries, func(id string) *store.Backup { return rows[id] })
	byID := map[string]stackSourceCopy{}
	for _, c := range got {
		byID[c.ID] = c
	}
	if byID["local"].Services != 3 {
		t.Errorf("every member has a local copy: %+v", byID["local"])
	}
	if byID["d-nas"].Services != 2 {
		t.Errorf("only two members are on the NAS — the dialog must be able to say so: %+v", byID["d-nas"])
	}
	if byID["d-nas"].Name != "NAS" || byID["d-nas"].Type != "smb" {
		t.Errorf("the copy carries its display name and type: %+v", byID["d-nas"])
	}
	// Local sorts first: it is the fast default.
	if len(got) == 0 || got[0].ID != "local" {
		t.Errorf("local should lead the list: %+v", got)
	}
	// A destination nothing was mirrored to never appears, so it cannot be chosen.
	if _, offered := byID["d-unused"]; offered {
		t.Error("a destination no member has a copy on must not be offered")
	}
}

// A copy that never uploaded is not somewhere to read from.
func TestStackSourceCopiesIgnoresFailedCopies(t *testing.T) {
	e := &backup.Engine{}
	entries := []backup.StackPlanEntry{{Service: "db", BackupID: "b-db"}}
	rows := map[string]*store.Backup{
		"b-db": {ID: "b-db", LocationsJSON: `[{"kind":"local","name":"local","type":"local"},{"kind":"dest","dest_id":"d-nas","name":"NAS","type":"smb","status":"failed"},{"kind":"dest","dest_id":"d-s3","name":"S3","type":"s3","status":"deferred"}]`},
	}
	for _, c := range stackSourceCopies(e, entries, func(id string) *store.Backup { return rows[id] }) {
		if c.ID == "d-nas" || c.ID == "d-s3" {
			t.Errorf("an intended copy that never uploaded is not readable: %+v", c)
		}
	}
	// A backup row that cannot be read contributes nothing rather than panicking.
	if got := stackSourceCopies(e, entries, func(string) *store.Backup { return nil }); len(got) != 0 {
		t.Errorf("an unreadable row contributes no copies: %+v", got)
	}
}

// AC2 — an unknown source id is refused before the lock, not silently ignored.
func TestStackRestoreRefusesAnUnknownSource(t *testing.T) {
	s := stackKeyServer(t)
	r := httptest.NewRequest("POST", "/api/nodes/n1/stacks/blog/restore?source=ghost-dest", strings.NewReader(""))
	r.SetPathValue("id", "n1")
	r.SetPathValue("project", "blog")
	rec := httptest.NewRecorder()
	s.handleRestoreStack(rec, r)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unknown source copy") {
		t.Errorf("the refusal should say what was wrong: %s", rec.Body.String())
	}
	// "local" and Auto are always valid and must not be refused.
	for _, src := range []string{"", "local"} {
		rr := httptest.NewRequest("POST", "/api/nodes/n1/stacks/blog/restore?source="+src, strings.NewReader(""))
		rr.SetPathValue("id", "n1")
		rr.SetPathValue("project", "blog")
		rec2 := httptest.NewRecorder()
		s.handleRestoreStack(rec2, rr)
		if rec2.Code == http.StatusBadRequest {
			t.Errorf("source=%q must be accepted: %s", src, rec2.Body.String())
		}
	}
}

// F215 — the cross-host choices a route needed are remembered, so the next
// restore of that project onto that machine offers them instead of asking again.
//
// Restoring the same stack onto the same machine twice used to mean typing the
// same domain, the same base directory and the same address a second time. The
// IP remap derived itself and the host base directory was remembered globally;
// the domain, the paths and the addresses were remembered nowhere — which is
// exactly the set nothing can derive.

func stackDefaults(t *testing.T, s *Server, project, target string) crossRestoreMemory {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/nodes/n1/stacks/"+project+"/restore-defaults?target_node="+target, nil)
	r.SetPathValue("id", "n1")
	r.SetPathValue("project", project)
	rec := httptest.NewRecorder()
	s.handleStackRestoreDefaults(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("defaults = %d: %s", rec.Code, rec.Body.String())
	}
	var m crossRestoreMemory
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// AC2 — one cross-restore with a path remap, and the next open of that route
// offers the same from → to.
func TestCrossRestoreChoicesAreRememberedPerRoute(t *testing.T) {
	s := stackKeyServer(t)

	// Nothing remembered yet is an empty object, not an error — "never restored
	// this route" is a normal state.
	if got := stackDefaults(t, s, "wikijs", "n2"); got.any() {
		t.Errorf("an unused route remembers nothing: %+v", got)
	}

	s.rememberCrossRestore("wikijs", "n2", crossRestoreMemory{
		RemapPath: true, RemapFromPath: "/opt/docker", RemapToPath: "/opt/stacks",
		RemapDomain: true, RemapFromDomain: "wiki.old.net", RemapToDomain: "wiki.new.net",
		NewSiteAddress: "https://wiki.new.net",
	})

	got := stackDefaults(t, s, "wikijs", "n2")
	if got.RemapFromPath != "/opt/docker" || got.RemapToPath != "/opt/stacks" {
		t.Errorf("the path remap must come back: %+v", got)
	}
	if got.RemapFromDomain != "wiki.old.net" || got.RemapToDomain != "wiki.new.net" {
		t.Errorf("the domain remap must come back — nothing can derive it: %+v", got)
	}
	if got.NewSiteAddress != "https://wiki.new.net" {
		t.Errorf("the address must come back: %+v", got)
	}
	if got.At == 0 {
		t.Error("the memory should be stamped, so a stale one can be recognised")
	}

	// Memory is PER ROUTE: another target, or another project, remembers nothing.
	if stackDefaults(t, s, "wikijs", "n3").any() {
		t.Error("a different target machine is a different route")
	}
	if stackDefaults(t, s, "blog", "n2").any() {
		t.Error("a different project is a different route")
	}
}

// A plain same-host restore with nothing to remember writes no row at all — the
// settings table must not grow a record per restore for no reason.
func TestNothingToRememberWritesNothing(t *testing.T) {
	s := stackKeyServer(t)
	s.rememberCrossRestore("blog", "n1", crossRestoreMemory{})
	keys, err := s.store.SettingKeysWithPrefix("restore.remaps.")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Errorf("an empty choice set writes nothing, got %v", keys)
	}
	// And a route with no project or no target is not a route.
	s.rememberCrossRestore("", "n1", crossRestoreMemory{ReconstructHost: true})
	s.rememberCrossRestore("blog", "", crossRestoreMemory{ReconstructHost: true})
	if keys, _ := s.store.SettingKeysWithPrefix("restore.remaps."); len(keys) != 0 {
		t.Errorf("an incomplete route writes nothing, got %v", keys)
	}
}

// AC3 — the offline private key must never reach the settings table. It is not a
// field of the memory struct at all, which is the real guarantee; this asserts
// it end to end through the handler that receives both.
func TestRememberedChoicesNeverContainThePrivateKey(t *testing.T) {
	s := stackKeyServer(t)

	rec := postStackRestore(s, `{"private_key":"`+woStackKey+`"}`)
	if rec.Code >= 400 {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body.String())
	}
	// Also drive a route that DOES write a memory row, with the key in the same
	// request — the row must carry the remaps and not the credential.
	s.rememberCrossRestore("blog", "n1", crossRestoreMemory{
		RemapDomain: true, RemapFromDomain: "a.example", RemapToDomain: "b.example",
	})

	keys, err := s.store.SettingKeysWithPrefix("restore.remaps.")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) == 0 {
		t.Fatal("expected at least one remembered route")
	}
	for _, k := range keys {
		v, _ := s.store.GetSetting(k, "")
		if strings.Contains(v, woStackKey) {
			t.Fatalf("the private key reached a remembered route (%s): %s", k, v)
		}
		if strings.Contains(v, "private_key") {
			t.Errorf("the memory must not even have a field for it (%s): %s", k, v)
		}
	}
}

// The key is namespaced so a project name can never collide with a setting in
// another namespace.
func TestCrossRestoreMemoryKeyIsNamespaced(t *testing.T) {
	for _, project := range []string{"blog", "", "..", "security.egress_allow", "restore.stepup.n1"} {
		k := crossRestoreMemoryKey(project, "n1")
		if !strings.HasPrefix(k, "restore.remaps.") {
			t.Errorf("project %q produced an unnamespaced key: %q", project, k)
		}
	}
	// Different routes are different keys.
	if crossRestoreMemoryKey("blog", "n1") == crossRestoreMemoryKey("blog", "n2") {
		t.Error("the target machine is part of the route")
	}
	if crossRestoreMemoryKey("blog", "n1") == crossRestoreMemoryKey("shop", "n1") {
		t.Error("the project is part of the route")
	}
}

// F216 — the gap list survives a dead source node.
//
// The dialog used to derive "will be skipped — no backup" from a LIVE container
// list. A source node that is down answers nothing, the list comes back empty,
// and a restore missing three services looks complete in the one situation — the
// machine is gone — that the warning exists for. The server has known better all
// along from its cached inventory; it simply never said so until after the run
// had started.

// seedInventory installs a cached container inventory for a node, as the
// background refresh would, WITHOUT the node being reachable.
func seedInventory(t *testing.T, s *Server, nodeID string, containers []*dockercli.Container) {
	t.Helper()
	s.setStat(nodeID, &nodeStat{Containers: containers, Reachable: false})
}

// AC1 — an uncovered member is named even though the node is unreachable.
func TestStackMissingMembersFromCachedInventory(t *testing.T) {
	s := stackKeyServer(t)
	// The stack has three services live; only two were ever backed up.
	mkStackMember(t, s, "b-db", "n1", "blog", "db", "blog-db")
	mkStackMember(t, s, "b-app", "n1", "blog", "app", "blog-app")
	seedInventory(t, s, "n1", []*dockercli.Container{
		{ID: "c1", Name: "blog-db", Stack: "blog", Service: "db"},
		{ID: "c2", Name: "blog-app", Stack: "blog", Service: "app"},
		{ID: "c3", Name: "blog-search", Stack: "blog", Service: "search"},
		{ID: "c4", Name: "unrelated", Stack: "shop", Service: "web"},
	})

	got := s.stackMissingMembers("n1", "blog")
	if len(got) != 1 || got[0] != "search" {
		t.Fatalf("the never-backed-up member must be named: %v", got)
	}
	// A member of ANOTHER stack is not this stack's gap.
	for _, m := range got {
		if m == "web" {
			t.Error("another project's service must not appear")
		}
	}
}

// AC3 — a fully covered stack reports nothing, so the dialog shows no false rows.
func TestStackMissingMembersEmptyWhenCovered(t *testing.T) {
	s := stackKeyServer(t)
	mkStackMember(t, s, "b-db", "n1", "blog", "db", "blog-db")
	seedInventory(t, s, "n1", []*dockercli.Container{
		{ID: "c1", Name: "blog-db", Stack: "blog", Service: "db"},
	})
	got := s.stackMissingMembers("n1", "blog")
	if len(got) != 0 {
		t.Errorf("a fully covered stack has no gap: %v", got)
	}
	// AC3 asks for an empty LIST, not null. A caller iterating this field must
	// not have to special-case the commonest answer there is, and the same rule
	// is stated for nodeRestorePlan's own list.
	if got == nil {
		t.Error("the healthy answer must be an empty list, not nil — it serializes as null")
	}
	// A node never polled claims nothing rather than inventing a gap from an
	// empty inventory — "unknown" and "missing" are different answers.
	if got := s.stackMissingMembers("never-polled", "blog"); len(got) != 0 || got == nil {
		t.Errorf("no inventory means no claim, still as a list: %v", got)
	}
}

// A container with no compose service label falls back to its NAME, which is the
// same key the backup catalog would have recorded it under.
func TestStackMissingMembersFallsBackToContainerName(t *testing.T) {
	s := stackKeyServer(t)
	mkStackMember(t, s, "b-db", "n1", "blog", "db", "blog-db")
	seedInventory(t, s, "n1", []*dockercli.Container{
		{ID: "c1", Name: "blog-db", Stack: "blog", Service: "db"},
		{ID: "c2", Name: "sidecar", Stack: "blog"}, // unlabelled
	})
	got := s.stackMissingMembers("n1", "blog")
	if len(got) != 1 || got[0] != "sidecar" {
		t.Fatalf("an unlabelled member is named by its container name: %v", got)
	}
}

// The same gap must read the same way twice — the inventory's order is Docker's,
// which is not stable between calls.
func TestStackMissingMembersIsSortedAndDeduped(t *testing.T) {
	s := stackKeyServer(t)
	mkStackMember(t, s, "b-db", "n1", "blog", "db", "blog-db")
	seedInventory(t, s, "n1", []*dockercli.Container{
		{ID: "c3", Name: "zeta", Stack: "blog", Service: "zeta"},
		{ID: "c2", Name: "alpha", Stack: "blog", Service: "alpha"},
		{ID: "c1", Name: "blog-db", Stack: "blog", Service: "db"},
		{ID: "c4", Name: "alpha-replica", Stack: "blog", Service: "alpha"}, // same service twice
	})
	got := s.stackMissingMembers("n1", "blog")
	if len(got) != 2 || got[0] != "alpha" || got[1] != "zeta" {
		t.Fatalf("want [alpha zeta] sorted and deduped, got %v", got)
	}
}

// AC1 through the endpoint the dialog actually reads.
func TestStackRestorePlanReportsMissingMembers(t *testing.T) {
	s := stackKeyServer(t)
	mkStackMember(t, s, "b-db", "n1", "blog", "db", "blog-db")
	seedInventory(t, s, "n1", []*dockercli.Container{
		{ID: "c1", Name: "blog-db", Stack: "blog", Service: "db"},
		{ID: "c2", Name: "blog-search", Stack: "blog", Service: "search"},
	})

	r := httptest.NewRequest("GET", "/api/nodes/n1/stacks/blog/restore-plan", nil)
	r.SetPathValue("id", "n1")
	r.SetPathValue("project", "blog")
	rec := httptest.NewRecorder()
	s.handleStackRestorePlan(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("plan = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		MissingMembers []string `json:"missing_members"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.MissingMembers) != 1 || out.MissingMembers[0] != "search" {
		t.Fatalf("the plan must carry the gap so the dialog can show it: %v", out.MissingMembers)
	}
}

// AC3, on the wire: a fully covered stack answers with an empty list, never null.
// The dialog guards against null, but an API caller iterating the field would
// not, and the healthy stack is the commonest response this endpoint gives.
func TestStackRestorePlanMissingMembersIsNeverNull(t *testing.T) {
	s := stackKeyServer(t)
	mkStackMember(t, s, "b-db", "n1", "blog", "db", "blog-db")
	seedInventory(t, s, "n1", []*dockercli.Container{
		{ID: "c1", Name: "blog-db", Stack: "blog", Service: "db"},
	})

	r := httptest.NewRequest("GET", "/api/nodes/n1/stacks/blog/restore-plan", nil)
	r.SetPathValue("id", "n1")
	r.SetPathValue("project", "blog")
	rec := httptest.NewRecorder()
	s.handleStackRestorePlan(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("plan = %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, `"missing_members":[]`) {
		t.Errorf(`want "missing_members":[] on a covered stack, got: %s`, body)
	}
}
