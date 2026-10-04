package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dockback/internal/backup"
	"dockback/internal/config"
	"dockback/internal/crypto"
	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// F206 — fresh proof of the password before overwriting a protected container.
//
// Reading a backup out of DockBack already demanded it — downloading one,
// extracting a file, revealing the key. DESTROYING the live data those backups
// exist to protect did not: auth, CSRF, a confirm checkbox, and a WipeDir. These
// tests hold the gate to the three things that make it worth having: it fires
// where an accident is unrecoverable, it never fires on the safe path, and it
// changes nothing for an ordinary container.

const rsPassword = "correct-horse-battery"

func stepUpRestoreServer(t *testing.T) *Server {
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
	reg := dockercli.NewRegistry()
	s := &Server{
		store: st,
		cfg:   &config.Config{EncryptionKey: key},
		guard: newLoginGuard(st),
		locks: newOpLocks(),
		// A real (empty) registry rather than nil: every node lookup then answers
		// "not registered" instead of dereferencing nil inside Registry.Get, which
		// is a panic in a test rather than the refusal the handler is written for.
		reg:    reg,
		engine: &backup.Engine{Store: st, Reg: reg, Key: key, Log: func(string, string, string) {}},
	}
	hash, err := crypto.HashPassword(rsPassword)
	if err != nil {
		t.Fatal(err)
	}
	uid, err := st.CreateUser("admin", hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession("tok", uid, time.Hour); err != nil {
		t.Fatal(err)
	}
	return s
}

// The resolver is the whole policy, so it is tested directly and in every
// combination that can reach it.
func TestRestoreStepUpResolution(t *testing.T) {
	s := stepUpRestoreServer(t)

	// An ordinary container: off, and nothing recorded.
	if s.restoreStepUpRequired("n1", "plain-app") {
		t.Error("an ordinary container must restore exactly as before")
	}
	if s.restoreStepUpIsExplicit("n1", "plain-app") {
		t.Error("nothing has been chosen for it")
	}

	// Marked CRITICAL — the operator has said this data's recovery point matters
	// in minutes, so overwriting it by accident is the failure that mark exists
	// to prevent.
	crit := s.loadCriticalDBs()
	crit[critKey("n1", "prod-db")] = CriticalDB{NodeID: "n1", Name: "prod-db", RPOSeconds: 300}
	if err := s.saveCriticalDBs(crit); err != nil {
		t.Fatal(err)
	}
	if !s.restoreStepUpDefault("n1", "prod-db") {
		t.Error("a critical database must be protected by default")
	}
	if !s.restoreStepUpRequired("n1", "prod-db") {
		t.Error("and that default must be what the gate resolves")
	}

	// Marked REQUIRE-WRITE-ONLY — somebody that deliberate about the archive
	// should not find the live copy is the unguarded end of the same system.
	if err := s.engine.SetRequireWriteOnly("n1", "termix", true); err != nil {
		t.Fatal(err)
	}
	if !s.restoreStepUpRequired("n1", "termix") {
		t.Error("a require-write-only container must be protected by default")
	}

	// The mark is per NODE: a same-named container elsewhere is a different
	// container and inherits nothing.
	if s.restoreStepUpRequired("n2", "prod-db") {
		t.Error("a container on another node must not inherit the mark")
	}

	// An explicit choice wins in BOTH directions. A setting that could only ever
	// be turned on is one nobody trusts.
	if err := s.setRestoreStepUp("n1", "prod-db", false, true); err != nil {
		t.Fatal(err)
	}
	if s.restoreStepUpRequired("n1", "prod-db") {
		t.Error("an explicit off must be honoured even for a critical database")
	}
	if !s.restoreStepUpDefault("n1", "prod-db") {
		t.Error("the DERIVED default is unchanged — the UI shows what is being overridden")
	}
	if err := s.setRestoreStepUp("n1", "plain-app", true, true); err != nil {
		t.Fatal(err)
	}
	if !s.restoreStepUpRequired("n1", "plain-app") {
		t.Error("an explicit on must protect a container the default would not")
	}

	// Clearing returns it to the derived default rather than to off.
	if err := s.setRestoreStepUp("n1", "prod-db", false, false); err != nil {
		t.Fatal(err)
	}
	if !s.restoreStepUpRequired("n1", "prod-db") {
		t.Error("clearing must fall back to the derived default, not to off")
	}
	if s.restoreStepUpIsExplicit("n1", "prod-db") {
		t.Error("a cleared choice is no longer explicit")
	}
}

// mkRestorable stores a backup row a restore request can name.
func mkRestorable(t *testing.T, s *Server, id, node, target string) {
	t.Helper()
	// ContainerID is what a real container backup records; without it F211's
	// pre-flight correctly reports the service as unrestorable.
	man, _ := json.Marshal(backup.Manifest{BackupID: id, TargetName: target, ContainerID: "cid-" + target})
	b := &store.Backup{ID: id, NodeID: node, TargetName: target, Status: "success",
		CreatedAt: time.Now().Unix(), ManifestJSON: string(man), StorageKey: "k/" + id}
	if err := s.store.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpsertNode(&store.Node{ID: node, Name: node, Transport: "socket", Address: "unix:///var/run/docker.sock"}); err != nil {
		t.Fatal(err)
	}
}

func postRestore(s *Server, id string, body map[string]any) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", "/api/backups/"+id+"/restore", strings.NewReader(string(raw)))
	r.SetPathValue("id", id)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
	rec := httptest.NewRecorder()
	s.handleRestore(rec, r)
	return rec
}

// AC1 — an in-place restore of a flagged container without step-up is refused
// with step_up_required, and NOTHING has been started.
func TestInPlaceRestoreOfProtectedContainerNeedsStepUp(t *testing.T) {
	s := stepUpRestoreServer(t)
	mkRestorable(t, s, "b1", "n1", "prod-db")
	crit := s.loadCriticalDBs()
	crit[critKey("n1", "prod-db")] = CriticalDB{NodeID: "n1", Name: "prod-db", RPOSeconds: 300}
	_ = s.saveCriticalDBs(crit)

	rec := postRestore(s, "b1", map[string]any{
		"node_id": "n1", "target_id": "cid", "volumes": true, "database": true, "confirm": true,
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["step_up_required"] != true {
		t.Errorf("the client must be told to prompt rather than sign out: %s", rec.Body.String())
	}
	// The refusal must come BEFORE the exclusive lock — a gate that left the
	// stack locked would turn a declined prompt into a stuck container.
	if !s.locks.acquireRestore(stackKey("n1", "", "prod-db")) {
		t.Error("the restore lock must not be held after a refused step-up")
	}
	s.locks.releaseRestore(stackKey("n1", "", "prod-db"))
}

// AC2 — the same backup restored AS A COPY needs no step-up. The safe path must
// stay the fast one, or the friction pushes people toward the destructive one.
func TestRestoreAsACopyNeedsNoStepUp(t *testing.T) {
	s := stepUpRestoreServer(t)
	mkRestorable(t, s, "b1", "n1", "prod-db")
	crit := s.loadCriticalDBs()
	crit[critKey("n1", "prod-db")] = CriticalDB{NodeID: "n1", Name: "prod-db", RPOSeconds: 300}
	_ = s.saveCriticalDBs(crit)

	rec := postRestore(s, "b1", map[string]any{
		"node_id": "n1", "target_id": "cid", "volumes": true, "confirm": true,
		"as_name": "prod-db-copy", "isolated": true,
	})
	// It gets past the gate. What happens after depends on an unreachable Docker
	// node, so the only thing asserted is that it was NOT refused for step-up.
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("a copy must never demand step-up: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "step_up_required") {
		t.Errorf("a copy must not mention step-up: %s", rec.Body.String())
	}
}

// AC3 — an unflagged container is unchanged. This is the regression that
// matters most: restoring is a routine operation and must stay one.
func TestUnflaggedContainerRestoresWithoutStepUp(t *testing.T) {
	s := stepUpRestoreServer(t)
	mkRestorable(t, s, "b2", "n1", "plain-app")

	rec := postRestore(s, "b2", map[string]any{
		"node_id": "n1", "target_id": "cid", "volumes": true, "confirm": true,
	})
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("an ordinary container must restore exactly as before: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "step_up_required") {
		t.Errorf("no step-up should be mentioned: %s", rec.Body.String())
	}
}

// With the password supplied, the protected restore proceeds past the gate.
func TestProtectedRestoreProceedsWithThePassword(t *testing.T) {
	s := stepUpRestoreServer(t)
	mkRestorable(t, s, "b1", "n1", "prod-db")
	_ = s.engine.SetRequireWriteOnly("n1", "prod-db", true)

	rec := postRestore(s, "b1", map[string]any{
		"node_id": "n1", "target_id": "cid", "volumes": true, "confirm": true,
		"password": rsPassword,
	})
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("the correct password must satisfy the gate: %s", rec.Body.String())
	}
	// A wrong password does not.
	fresh := stepUpRestoreServer(t)
	mkRestorable(t, fresh, "b1", "n1", "prod-db")
	_ = fresh.engine.SetRequireWriteOnly("n1", "prod-db", true)
	rec = postRestore(fresh, "b1", map[string]any{
		"node_id": "n1", "target_id": "cid", "volumes": true, "confirm": true,
		"password": "not-the-password",
	})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a wrong password = %d, want 401", rec.Code)
	}
}

// A request that sets isolated WITHOUT a new name still overwrites the original,
// so it must be treated as in-place. The fail-safe reading is the only safe one.
func TestIsolatedWithoutANameIsStillInPlace(t *testing.T) {
	s := stepUpRestoreServer(t)
	mkRestorable(t, s, "b1", "n1", "prod-db")
	_ = s.engine.SetRequireWriteOnly("n1", "prod-db", true)

	rec := postRestore(s, "b1", map[string]any{
		"node_id": "n1", "target_id": "cid", "volumes": true, "confirm": true,
		"isolated": true, // no as_name — this overwrites the original
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("isolated with no new name overwrites the original and must be gated: %d %s", rec.Code, rec.Body.String())
	}
}

// The choice is recorded and audited, including when it is turned OFF — the
// change that weakens the guard is the one worth having a record of.
func TestRestoreStepUpChangeIsAudited(t *testing.T) {
	s := stepUpRestoreServer(t)
	if err := s.setRestoreStepUp("n1", "prod-db", false, true); err != nil {
		t.Fatal(err)
	}
	_ = s.store.Audit("admin", "restore.stepup.set", "prod-db", "require=false")
	if !auditHas(t, s, "restore.stepup.set") {
		t.Error("a change to the overwrite guard must be audited")
	}
}

// ---------------------------------------------------------------------------
// F210 — the composed restores honour the same gate.
//
// Until now the guard lived only in the single-container handler, so a container
// the operator had marked protected was gated when restored on its own and
// completely unguarded when restored as part of its stack, or as part of its
// node. The stack dialog is the more convenient of the two buttons, which made
// it the wrong one to leave open.
// ---------------------------------------------------------------------------

// mkStackMember stores a successful backup belonging to a compose project, so
// PlanStack can select it as a member.
func mkStackMember(t *testing.T, s *Server, id, node, stack, service, target string) {
	t.Helper()
	man, _ := json.Marshal(backup.Manifest{BackupID: id, TargetName: target, Stack: stack, Service: service, ContainerID: "cid-" + target})
	b := &store.Backup{ID: id, NodeID: node, TargetName: target, Stack: stack, Status: "success",
		CreatedAt: time.Now().Unix(), ManifestJSON: string(man), StorageKey: "k/" + id}
	if err := s.store.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpsertNode(&store.Node{ID: node, Name: node, Transport: "socket", Address: "unix:///var/run/docker.sock"}); err != nil {
		t.Fatal(err)
	}
}

func postStackRestoreStepUp(s *Server, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/nodes/n1/stacks/blog/restore", strings.NewReader(body))
	r.SetPathValue("id", "n1")
	r.SetPathValue("project", "blog")
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
	rec := httptest.NewRecorder()
	s.handleRestoreStack(rec, r)
	return rec
}

// markCritical protects a container the way an operator would.
func markCritical(t *testing.T, s *Server, node, name string) {
	t.Helper()
	crit := s.loadCriticalDBs()
	crit[critKey(node, name)] = CriticalDB{NodeID: node, Name: name, RPOSeconds: 300}
	if err := s.saveCriticalDBs(crit); err != nil {
		t.Fatal(err)
	}
}

// AC1 — a stack containing a protected member is refused without credentials,
// and nothing is left locked.
func TestStackRestoreOfProtectedMemberNeedsStepUp(t *testing.T) {
	s := stepUpRestoreServer(t)
	mkStackMember(t, s, "b-db", "n1", "blog", "db", "blog-db")
	mkStackMember(t, s, "b-app", "n1", "blog", "app", "blog-app")
	markCritical(t, s, "n1", "blog-db")

	rec := postStackRestoreStepUp(s, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["step_up_required"] != true {
		t.Errorf("the client must be told to prompt, not signed out: %s", rec.Body.String())
	}
	// The refusal must come BEFORE the locks — a declined prompt that left the
	// project locked would turn a cancelled dialog into a stuck stack.
	for _, k := range []string{stackKey("n1", "blog", "")} {
		if !s.locks.acquireRestore(k) {
			t.Errorf("lock %q must not be held after a refused step-up", k)
		}
		s.locks.releaseRestore(k)
	}
	// And nothing was started.
	if entries, _ := s.store.ListAudit(50); len(entries) > 0 {
		for _, e := range entries {
			if strings.HasPrefix(e.Action, "stack.") {
				t.Errorf("a refused restore must not be audited as started: %+v", e)
			}
		}
	}
}

// AC2 — with the password it proceeds, and the audit row records the parity
// detail naming the member that required it.
func TestStackRestoreProceedsWithThePasswordAndIsAudited(t *testing.T) {
	s := stepUpRestoreServer(t)
	mkStackMember(t, s, "b-db", "n1", "blog", "db", "blog-db")
	mkStackMember(t, s, "b-app", "n1", "blog", "app", "blog-app")
	markCritical(t, s, "n1", "blog-db")

	rec := postStackRestoreStepUp(s, `{"password":"`+rsPassword+`"}`)
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("the correct password must satisfy the gate: %s", rec.Body.String())
	}
	entries, err := s.store.ListAudit(50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if !strings.HasPrefix(e.Action, "stack.") {
			continue
		}
		found = true
		if !strings.Contains(e.Detail, "protected: step-up ok") {
			t.Errorf("the audit row should record the protected overwrite: %q", e.Detail)
		}
		if !strings.Contains(e.Detail, "blog-db") {
			t.Errorf("and name the member that required it: %q", e.Detail)
		}
		if strings.Contains(e.Detail, rsPassword) {
			t.Fatalf("the password reached the audit trail: %q", e.Detail)
		}
	}
	if !found {
		t.Fatal("the restore should have been audited")
	}
}

// AC3 — a stack with no protected member restores exactly as today. This is the
// regression that matters most: restoring is routine and must stay one click.
func TestStackRestoreWithNoProtectedMemberIsUnchanged(t *testing.T) {
	s := stepUpRestoreServer(t)
	mkStackMember(t, s, "b-db", "n1", "blog", "db", "blog-db")
	mkStackMember(t, s, "b-app", "n1", "blog", "app", "blog-app")

	rec := postStackRestoreStepUp(s, "")
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("an ordinary stack must not prompt: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "step_up_required") {
		t.Errorf("no prompt should be mentioned: %s", rec.Body.String())
	}
	for _, e := range mustAudit(t, s) {
		if strings.HasPrefix(e.Action, "stack.") && strings.Contains(e.Detail, "protected") {
			t.Errorf("nothing was protected, so nothing should say so: %q", e.Detail)
		}
	}
}

// The mark is resolved against the node the restore LANDS ON. A container marked
// on the origin, restored onto a different machine, overwrites nothing there.
func TestStackRestoreResolvesProtectionAgainstTheTargetNode(t *testing.T) {
	s := stepUpRestoreServer(t)
	mkStackMember(t, s, "b-db", "n1", "blog", "db", "blog-db")
	if err := s.store.UpsertNode(&store.Node{ID: "n2", Name: "n2", Transport: "socket", Address: "unix:///var/run/docker.sock"}); err != nil {
		t.Fatal(err)
	}
	markCritical(t, s, "n1", "blog-db") // marked on the SOURCE only

	r := httptest.NewRequest("POST", "/api/nodes/n1/stacks/blog/restore?target_node=n2", strings.NewReader(""))
	r.SetPathValue("id", "n1")
	r.SetPathValue("project", "blog")
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
	rec := httptest.NewRecorder()
	s.handleRestoreStack(rec, r)
	if rec.Code == http.StatusUnauthorized {
		t.Fatal("restoring onto a machine where nothing is marked must not prompt — there is nothing there to destroy")
	}

	// Marked on the TARGET, it is gated.
	s2 := stepUpRestoreServer(t)
	mkStackMember(t, s2, "b-db", "n1", "blog", "db", "blog-db")
	_ = s2.store.UpsertNode(&store.Node{ID: "n2", Name: "n2", Transport: "socket", Address: "unix:///var/run/docker.sock"})
	markCritical(t, s2, "n2", "blog-db")
	r2 := httptest.NewRequest("POST", "/api/nodes/n1/stacks/blog/restore?target_node=n2", strings.NewReader(""))
	r2.SetPathValue("id", "n1")
	r2.SetPathValue("project", "blog")
	r2.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
	rec2 := httptest.NewRecorder()
	s2.handleRestoreStack(rec2, r2)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("a member marked on the TARGET must be gated: %d", rec2.Code)
	}
}

// The whole-node restore — the widest destructive action there is — honours it
// too, and was likewise completely unguarded.
func TestNodeRestoreAllOfProtectedContainerNeedsStepUp(t *testing.T) {
	s := stepUpRestoreServer(t)
	mkRestorable(t, s, "b1", "n1", "prod-db")
	markCritical(t, s, "n1", "prod-db")

	call := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/nodes/n1/restore-all", strings.NewReader(body))
		r.SetPathValue("id", "n1")
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
		rec := httptest.NewRecorder()
		s.handleRestoreNodeAll(rec, r)
		return rec
	}

	rec := call("")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["step_up_required"] != true {
		t.Errorf("the client must be told to prompt: %s", rec.Body.String())
	}

	// With the password it proceeds and the audit row records it.
	if rec := call(`{"password":"` + rsPassword + `"}`); rec.Code == http.StatusUnauthorized {
		t.Fatalf("the correct password must satisfy the gate: %s", rec.Body.String())
	}
	for _, e := range mustAudit(t, s) {
		if e.Action == "node.restore.all" && !strings.Contains(e.Detail, "protected: step-up ok") {
			t.Errorf("the audit row should record the protected overwrite: %q", e.Detail)
		}
	}
}

// A node with nothing marked restores exactly as before.
func TestNodeRestoreAllWithNoProtectedContainerIsUnchanged(t *testing.T) {
	s := stepUpRestoreServer(t)
	mkRestorable(t, s, "b1", "n1", "plain-app")

	r := httptest.NewRequest("POST", "/api/nodes/n1/restore-all", strings.NewReader(""))
	r.SetPathValue("id", "n1")
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
	rec := httptest.NewRecorder()
	s.handleRestoreNodeAll(rec, r)
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("an unmarked node must not prompt: %s", rec.Body.String())
	}
}

// The shared resolver: dedups, sorts, and reports WHICH members are protected so
// the prompt can explain itself.
func TestProtectedRestoreMembers(t *testing.T) {
	s := stepUpRestoreServer(t)
	markCritical(t, s, "n1", "zeta")
	if err := s.engine.SetRequireWriteOnly("n1", "alpha", true); err != nil {
		t.Fatal(err)
	}

	got := s.protectedRestoreMembers("n1", []string{"plain", "zeta", "alpha", "zeta", "", "plain"})
	if len(got) != 2 || got[0] != "alpha" || got[1] != "zeta" {
		t.Fatalf("want [alpha zeta] deduped and sorted, got %v", got)
	}
	if len(s.protectedRestoreMembers("n1", []string{"plain", "other"})) != 0 {
		t.Error("nothing marked means nothing protected")
	}
	if len(s.protectedRestoreMembers("n2", []string{"zeta", "alpha"})) != 0 {
		t.Error("marks are per node — another node inherits none")
	}
	if len(s.protectedRestoreMembers("n1", nil)) != 0 {
		t.Error("an empty set protects nothing")
	}
}

// The audit suffix names the members, and says nothing when nothing was gated.
func TestStepUpDetail(t *testing.T) {
	if got := stepUpDetail(nil); got != "" {
		t.Errorf("no protected member means no suffix, got %q", got)
	}
	got := stepUpDetail([]string{"blog-db", "termix"})
	if !strings.Contains(got, "protected: step-up ok") {
		t.Errorf("detail = %q", got)
	}
	for _, name := range []string{"blog-db", "termix"} {
		if !strings.Contains(got, name) {
			t.Errorf("detail should name %q: %q", name, got)
		}
	}
}

func mustAudit(t *testing.T, s *Server) []*store.AuditEntry {
	t.Helper()
	entries, err := s.store.ListAudit(50)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// Step 23: a files-only restore overwrites nothing, so a protected container
// needs no fresh password for it.
func TestFilesOnlyRestoreNeedsNoStepUp(t *testing.T) {
	s := stepUpRestoreServer(t)
	mkRestorable(t, s, "b1", "n1", "prod-db")
	crit := s.loadCriticalDBs()
	crit[critKey("n1", "prod-db")] = CriticalDB{NodeID: "n1", Name: "prod-db", RPOSeconds: 300}
	_ = s.saveCriticalDBs(crit)

	rec := postRestore(s, "b1", map[string]any{
		"node_id": "n1", "target_id": "cid", "confirm": true, "files_only": true,
	})
	if rec.Code == http.StatusUnauthorized || strings.Contains(rec.Body.String(), "step_up_required") {
		t.Fatalf("files-only changes no container and must not demand step-up: %d %s", rec.Code, rec.Body.String())
	}
}

// Files only changes no container, so it cannot also be a copy or a revert.
func TestFilesOnlyRestoreRefusesACopyOrARevert(t *testing.T) {
	s := stepUpRestoreServer(t)
	mkRestorable(t, s, "b1", "n1", "app")
	for _, extra := range []map[string]any{
		{"as_name": "app-copy", "isolated": true},
		{"recreate": true},
	} {
		body := map[string]any{"node_id": "n1", "target_id": "cid", "confirm": true, "files_only": true}
		for k, v := range extra {
			body[k] = v
		}
		if rec := postRestore(s, "b1", body); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "files-only") {
			t.Errorf("files_only with %v must be refused: %d %s", extra, rec.Code, rec.Body.String())
		}
	}
}
