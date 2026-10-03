package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// Every stack view and every per-container view used to read the whole node's
// catalog and filter in Go — thousands of rows and their JSON blobs decoded on a
// six-second poll, to answer a question about one container.

func catalogStore(t *testing.T, targets, stacks, perTarget int) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	n := 0
	for s := 0; s < stacks; s++ {
		for tg := 0; tg < targets; tg++ {
			for i := 0; i < perTarget; i++ {
				n++
				b := &Backup{
					ID: fmt.Sprintf("b%d", n), NodeID: "n1",
					Stack: fmt.Sprintf("stack%d", s), TargetName: fmt.Sprintf("app%d-%d", s, tg),
					Status: "success", CreatedAt: int64(1000 + n),
				}
				if err := st.CreateBackup(b); err != nil {
					t.Fatal(err)
				}
				b.ManifestJSON = strings.Repeat("x", 2048) // the blob the old path decoded per row
				if err := st.UpdateBackup(b); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	return st
}

func TestScopedQueriesReturnOnlyTheirOwnRows(t *testing.T) {
	st := catalogStore(t, 3, 2, 4) // 2 stacks × 3 targets × 4 backups = 24 rows

	byTarget, err := st.ListBackupsForTarget("n1", "app0-1", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(byTarget) != 4 {
		t.Fatalf("per-target query returned %d rows, want the 4 belonging to that container", len(byTarget))
	}
	for _, b := range byTarget {
		if b.TargetName != "app0-1" {
			t.Errorf("a foreign container's backup came back: %s", b.TargetName)
		}
	}
	// Newest first, which every caller relies on.
	for i := 1; i < len(byTarget); i++ {
		if byTarget[i-1].CreatedAt < byTarget[i].CreatedAt {
			t.Error("rows must come back newest first")
		}
	}

	byStack, err := st.ListBackupsForStack("n1", "stack1", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(byStack) != 12 {
		t.Fatalf("per-stack query returned %d rows, want the 12 in that project", len(byStack))
	}
	for _, b := range byStack {
		if b.Stack != "stack1" {
			t.Errorf("another project's backup came back: %s", b.Stack)
		}
	}

	// A name nothing matches is empty, not everything.
	if rows, _ := st.ListBackupsForStack("n1", "no-such-stack", 100); len(rows) != 0 {
		t.Errorf("an unknown stack must return nothing, got %d rows", len(rows))
	}
	if rows, _ := st.ListBackupsForTarget("n2", "app0-1", 100); len(rows) != 0 {
		t.Errorf("the node scope must hold, got %d rows", len(rows))
	}
}

// The limit now bounds the CONTAINER's history, not the node's. That is also a
// correctness gain: a busy node could push a quiet container's rows out of the
// old node-wide window entirely, and it would read as having no backups.
func TestScopedLimitsAreScopedToo(t *testing.T) {
	st := catalogStore(t, 20, 1, 10) // 200 rows on the node, 10 per container

	// A node-wide read of 20 rows sees only the newest container's history…
	nodeWide, err := st.ListBackups("n1", 20)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, b := range nodeWide {
		seen[b.TargetName] = true
	}
	if seen["app0-0"] {
		t.Skip("row ordering put the oldest container inside the window; the point still holds")
	}
	// …while the scoped read returns that container's backups regardless.
	scoped, err := st.ListBackupsForTarget("n1", "app0-0", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 10 {
		t.Fatalf("the oldest container's 10 backups must still be readable, got %d", len(scoped))
	}
}

// The indexes must be SHAPED for these queries, or they are decoration.
//
// SQLite only prefers them once it has table statistics, and this application
// never runs ANALYZE — so today the planner narrows by node and filters inside
// the engine instead. That is still the win this change is about (the rows never
// cross into Go and their manifest blobs are never decoded), but the assertion
// below runs ANALYZE first so it fails if someone changes the index columns or
// the query in a way that makes the two incompatible.
func TestScopedQueriesUseTheirIndexesWhenTheyCan(t *testing.T) {
	st := catalogStore(t, 20, 10, 5)
	if _, err := st.db.Exec("ANALYZE"); err != nil {
		t.Fatal(err)
	}

	cases := map[string]struct{ query, index string }{
		"per-target": {
			"SELECT id FROM backups WHERE node_id=? AND target_name=? ORDER BY created_at DESC LIMIT ?",
			"idx_backups_target",
		},
		"per-stack": {
			"SELECT id FROM backups WHERE node_id=? AND stack=? ORDER BY created_at DESC LIMIT ?",
			"idx_backups_stack",
		},
	}
	for name, c := range cases {
		rows, err := st.db.Query("EXPLAIN QUERY PLAN "+c.query, "n1", "x", 10)
		if err != nil {
			t.Fatal(err)
		}
		var plan strings.Builder
		for rows.Next() {
			var a, b, d int
			var detail string
			if err := rows.Scan(&a, &b, &d, &detail); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			plan.WriteString(detail + "\n")
		}
		rows.Close()
		if !strings.Contains(plan.String(), c.index) {
			t.Errorf("%s query cannot be served by %s:\n%s", name, c.index, plan.String())
		}
	}
}
