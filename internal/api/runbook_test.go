package api

import (
	"encoding/json"
	"strings"
	"testing"

	"dockback/internal/backup"
	"dockback/internal/config"
	"dockback/internal/store"
)

// Regression: a node with NO successful backups must serialize its restore plan
// as "services": [] — never null. null crashed the Recovery page on
// `node.services.length` the moment a fresh node joined the fleet. Same for the
// fleet-level "nodes" and "destinations" arrays on an empty install.
func TestRunbookArraysNeverNull(t *testing.T) {
	s := &Server{
		store:  testStore(t),
		cfg:    &config.Config{BackupsDir: t.TempDir()},
		engine: &backup.Engine{},
	}

	// Empty fleet: nodes + destinations must still be [].
	js, err := json.Marshal(s.buildRunbook())
	if err != nil {
		t.Fatal(err)
	}
	for _, frag := range []string{`"nodes":[]`, `"destinations":[]`} {
		if !strings.Contains(string(js), frag) {
			t.Fatalf("empty fleet runbook missing %s: %s", frag, js)
		}
	}

	// A node that has never completed a backup: services must be [].
	if err := s.store.UpsertNode(&store.Node{ID: "n1", Name: "fresh"}); err != nil {
		t.Fatal(err)
	}
	js, err = json.Marshal(s.buildRunbook())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(js), `"services":null`) {
		t.Fatalf("runbook serialized services as null: %s", js)
	}
	if !strings.Contains(string(js), `"services":[]`) {
		t.Fatalf("fresh node should have an empty services array: %s", js)
	}
}

// nodeRestorePlan itself must return a non-nil slice for an unknown node — it
// is also consumed directly by the whole-node restore executor.
func TestNodeRestorePlanNonNil(t *testing.T) {
	s := &Server{store: testStore(t)}
	if plan := s.nodeRestorePlan("ghost"); plan == nil {
		t.Fatal("nodeRestorePlan must never return nil")
	}
}

// F103: a service captured as raw files because its dump tools were missing has
// a DIFFERENT restore procedure — file-level, not a dump import. The runbook is
// what an operator reads mid-incident, so that has to be stated there.
func TestRunbookNotesRawFileDatabaseCapture(t *testing.T) {
	svc := runbookService{
		Container:  "postgres",
		Role:       "application", // no Databases[] — that is exactly the problem
		DBFallback: "postgres: dump tools not found — captured as raw files",
	}
	joined := strings.Join(runbookNotes(svc), "\n")
	for _, want := range []string{"RAW FILES", "torn", "not a dump import", "pg_dumpall"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the note must mention %q:\n%s", want, joined)
		}
	}

	// An ordinary service gains no such note — a runbook of advice that doesn't
	// apply is a runbook nobody reads.
	if got := strings.Join(runbookNotes(runbookService{Container: "web", Role: "application"}), "\n"); strings.Contains(got, "RAW FILES") {
		t.Fatalf("an ordinary service must not carry the fallback note:\n%s", got)
	}
	// …and a real database with a real dump does not either.
	healthy := runbookService{Container: "db", Role: "database", Engine: "mysql", Databases: 1}
	if got := strings.Join(runbookNotes(healthy), "\n"); strings.Contains(got, "RAW FILES") {
		t.Fatalf("a properly-dumped database must not carry the fallback note:\n%s", got)
	}
}
