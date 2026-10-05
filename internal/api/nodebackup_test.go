package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"dockback/internal/backup"
	"dockback/internal/config"
	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// F221 — "Back up this node now".
//
// The button existed and looped from the browser, hardcoding balanced
// compression over whatever each container had remembered and deduplicating
// nothing. These tests hold the two things that makes it worth moving to the
// server: every container keeps its own options, and a folder two members of a
// stack both mount is captured once.

func nodeBackupServer(t *testing.T) *Server {
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
	reg := dockercli.NewRegistry()
	return &Server{
		store: st,
		cfg:   &config.Config{EncryptionKey: key},
		locks: newOpLocks(),
		stats: map[string]*nodeStat{},
		jobs:  map[string]*backupJob{},
		// A real (empty) registry rather than nil: a node lookup then answers
		// "not registered" instead of dereferencing nil inside Registry.Get.
		reg:      reg,
		queueSig: make(chan struct{}, 1),
		engine:   &backup.Engine{Store: st, Reg: reg, Key: key, Log: func(string, string, string) {}},
	}
}

// queuedOpts returns the options of every job sitting in the queue, keyed by
// container name — nothing dispatches in a test, so the queue IS the record of
// what this call decided.
func queuedOpts(s *Server, names map[string]string) map[string]backup.Options {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	out := map[string]backup.Options{}
	for _, j := range s.queue {
		out[names[j.opts.ContainerID]] = j.opts
	}
	return out
}

func postBackupAll(s *Server, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest("POST", "/api/nodes/n1/backup-all", nil)
	} else {
		r = httptest.NewRequest("POST", "/api/nodes/n1/backup-all", strings.NewReader(body))
	}
	r.SetPathValue("id", "n1")
	rec := httptest.NewRecorder()
	s.handleBackupNodeAll(rec, r)
	return rec
}

// AC1 — one call enqueues a backup per running container, each carrying that
// container's OWN remembered options rather than a blanket default.
func TestNodeBackupAllUsesRememberedOptions(t *testing.T) {
	s := nodeBackupServer(t)
	seedInventory(t, s, "n1", []*dockercli.Container{
		{ID: "c1", Name: "app", State: "running"},
		{ID: "c2", Name: "db", State: "running"},
		{ID: "c3", Name: "old", State: "exited"},
	})
	// "app" remembers a deliberate non-default; "db" has never been configured.
	sb, _ := json.Marshal(backup.SavedBackupOptions{Compression: "max", AppExport: true})
	if err := s.store.SetSetting(backup.BackupOptionsKey("n1", "app"), string(sb)); err != nil {
		t.Fatal(err)
	}

	rec := postBackupAll(s, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
	}
	var out nodeBackupAllResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Count != 2 || out.Status != "started" {
		t.Fatalf("want 2 running containers started, got %+v", out)
	}
	// The stopped one is left alone unless asked for.
	for _, n := range out.Names {
		if n == "old" {
			t.Error("a stopped container must not be swept in by default")
		}
	}

	got := queuedOpts(s, map[string]string{"c1": "app", "c2": "db"})
	if len(got) != 2 {
		t.Fatalf("want two queued jobs, got %d", len(got))
	}
	if got["app"].Compression != "max" || !got["app"].AppExport {
		t.Errorf("app must keep its remembered options, got %+v", got["app"])
	}
	if !got["app"].CompressionExplicit {
		t.Error("a remembered non-default is a deliberate choice and must suppress autotune")
	}
	// An unconfigured container gets the same default a scheduled run gives it —
	// balanced, and still autotune-eligible.
	if got["db"].Compression != "balanced" || got["db"].CompressionExplicit {
		t.Errorf("db must get the scheduled default, got %+v", got["db"])
	}
}

// include_stopped mirrors the schedule's own flag: opt-in, never assumed.
func TestNodeBackupAllHonorsIncludeStopped(t *testing.T) {
	s := nodeBackupServer(t)
	seedInventory(t, s, "n1", []*dockercli.Container{
		{ID: "c1", Name: "app", State: "running"},
		{ID: "c3", Name: "old", State: "exited"},
	})

	rec := postBackupAll(s, `{"include_stopped":true}`)
	var out nodeBackupAllResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Count != 2 {
		t.Fatalf("want both containers, got %+v", out)
	}
	if len(out.Names) != 2 || out.Names[0] != "app" || out.Names[1] != "old" {
		t.Errorf("names must be reported and stable: %v", out.Names)
	}
}

// AC3 — a second click coalesces. The first call queued them, so the second
// must add nothing and say so rather than reporting a silent zero.
func TestNodeBackupAllCoalescesRepeatClicks(t *testing.T) {
	s := nodeBackupServer(t)
	seedInventory(t, s, "n1", []*dockercli.Container{
		{ID: "c1", Name: "app", State: "running"},
		{ID: "c2", Name: "db", State: "running"},
	})

	if rec := postBackupAll(s, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("first call: %d %s", rec.Code, rec.Body.String())
	}
	rec := postBackupAll(s, "")
	var out nodeBackupAllResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Count != 0 || out.Skipped != 2 || out.Status != "already_running" {
		t.Errorf("a repeat click must queue nothing and say why: %+v", out)
	}
	s.queueMu.Lock()
	n := len(s.queue)
	s.queueMu.Unlock()
	if n != 2 {
		t.Errorf("queue holds %d jobs after two clicks, want 2", n)
	}
}

// AC2 — a host folder two members of a stack both mount is captured ONCE: the
// non-owner's selection for this run drops it, and the remembered selection is
// left alone.
func TestNodeBackupAllDedupesSharedBinds(t *testing.T) {
	s := nodeBackupServer(t)
	shared := "/mnt/media"
	members := []*dockercli.Container{
		{ID: "c1", Name: "jelly", State: "running", Stack: "media", Mounts: []dockercli.Mount{
			{Type: "bind", Source: shared, Destination: "/data"},
			{Type: "volume", Name: "jelly-cfg", Destination: "/config"},
		}},
		{ID: "c2", Name: "arr", State: "running", Stack: "media", Mounts: []dockercli.Mount{
			{Type: "bind", Source: shared, Destination: "/media"},
			{Type: "volume", Name: "arr-cfg", Destination: "/config"},
		}},
	}
	seedInventory(t, s, "n1", members)
	// Both have the shared folder in their stored selection — which is exactly
	// the situation that captures it twice.
	for _, m := range []struct{ name, dest string }{{"jelly", "/data"}, {"arr", "/media"}} {
		sel, _ := json.Marshal([]string{m.dest, "/config"})
		if err := s.store.SetSetting(backup.MountSelectionKey("n1", m.name), string(sel)); err != nil {
			t.Fatal(err)
		}
	}

	if rec := postBackupAll(s, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
	}
	got := queuedOpts(s, map[string]string{"c1": "jelly", "c2": "arr"})
	if len(got) != 2 {
		t.Fatalf("want both members queued, got %d", len(got))
	}

	// Exactly one of them keeps the shared folder; the other's run drops it.
	owners, skippers := 0, 0
	for name, o := range got {
		hasShared := false
		for _, d := range o.IncludeMounts {
			if d == "/data" || d == "/media" {
				hasShared = true
			}
		}
		if o.IncludeMounts == nil {
			// No override: this member's remembered selection stands — it is the owner.
			owners++
			continue
		}
		if hasShared {
			t.Errorf("%s was given a one-run selection that still includes the shared folder: %v", name, o.IncludeMounts)
		}
		skippers++
		// The override is for THIS run only; the remembered selection must survive.
		if !o.SelectionEphemeral {
			t.Errorf("%s: a dedup override must never overwrite the remembered selection", name)
		}
	}
	if owners != 1 || skippers != 1 {
		t.Errorf("want one owner and one skipper, got %d/%d: %+v", owners, skippers, got)
	}
	// Nothing was written back over either container's stored selection.
	for _, name := range []string{"jelly", "arr"} {
		v, _ := s.store.GetSetting(backup.MountSelectionKey("n1", name), "")
		if !strings.Contains(v, "/config") || len(v) == 0 {
			t.Errorf("%s's remembered selection was disturbed: %q", name, v)
		}
	}
}

// The target set is a pure decision, so it is tested to its edges — this is what
// decides whether somebody's container gets captured or silently skipped.
func TestNodeBackupTargets(t *testing.T) {
	clone := &dockercli.Container{ID: "c9", Name: "app-test-0104", State: "running",
		Labels: map[string]string{backup.TestCloneLabel: strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)}}
	list := []*dockercli.Container{
		{ID: "c2", Name: "zeta", State: "running"},
		{ID: "c1", Name: "alpha", State: "running"},
		{ID: "c3", Name: "stopped", State: "exited"},
		{ID: "c4", Name: "paused", State: "paused"},
		clone,
		nil,
		{ID: "", Name: "no-id", State: "running"},
	}

	got := nodeBackupTargets(list, false, nil)
	if len(got) != 2 || got[0].Name != "alpha" || got[1].Name != "zeta" {
		t.Fatalf("running only, sorted by name: %v", targetNames(got))
	}
	// F219: a throwaway test clone is never captured — it is a copy of a copy
	// under a name that stops existing tomorrow.
	for _, c := range nodeBackupTargets(list, true, nil) {
		if c.Name == "app-test-0104" {
			t.Error("a test clone must never be backed up")
		}
	}
	if got := nodeBackupTargets(list, true, nil); len(got) != 4 {
		t.Errorf("with stopped included: want 4 (running + exited + paused), got %v", targetNames(got))
	}
	if got := nodeBackupTargets(nil, true, nil); len(got) != 0 || got == nil {
		t.Error("an empty node is an empty list, not nil")
	}
}

func targetNames(list []*dockercli.Container) []string {
	out := make([]string, 0, len(list))
	for _, c := range list {
		out = append(out, c.Name)
	}
	return out
}

// An unknown node is a 404, and a node with nothing to capture says so rather
// than reporting a successful run of zero containers.
func TestNodeBackupAllEdgeCases(t *testing.T) {
	s := nodeBackupServer(t)
	r := httptest.NewRequest("POST", "/api/nodes/ghost/backup-all", nil)
	r.SetPathValue("id", "ghost")
	rec := httptest.NewRecorder()
	s.handleBackupNodeAll(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown node = %d, want 404", rec.Code)
	}

	seedInventory(t, s, "n1", []*dockercli.Container{{ID: "c3", Name: "old", State: "exited"}})
	if rec := postBackupAll(s, ""); rec.Code != http.StatusNotFound {
		t.Errorf("nothing running = %d, want 404 with a reason: %s", rec.Code, rec.Body.String())
	}
	// A malformed body is refused rather than silently treated as defaults — the
	// difference between the two is whether stopped containers get captured.
	if rec := postBackupAll(s, "{not json"); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed body = %d, want 400", rec.Code)
	}
}
