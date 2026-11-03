package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func TestFinishingARestoreExecutionHappensOnce(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.BeginRestoreExecution("e1", "b1", "app"); err != nil {
		t.Fatal(err)
	}
	if won, err := st.FinishRestoreExecution("e1", "ok", "done"); err != nil || !won {
		t.Fatalf("%v %v", won, err)
	}
	if won, err := st.FinishRestoreExecution("e1", "interrupted", "late"); err != nil || won {
		t.Fatalf("a finished run must not change: %v %v", won, err)
	}
	got, err := st.GetRestoreExecution("b1", "e1")
	if err != nil || got.Outcome != "ok" || got.Message != "done" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := st.GetRestoreExecution("b1", "absent"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("%v", err)
	}
}

func TestRestoreExecutionHistoryIsBoundedAndKeepsWhatIsRunning(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.BeginRestoreExecution("still-running", "long", "app"); err != nil {
		t.Fatal(err)
	}
	for i := range restoreExecutionsKept + 10 {
		id := fmt.Sprintf("e%d", i)
		if err := st.BeginRestoreExecution(id, "b", "app"); err != nil {
			t.Fatal(err)
		}
		if _, err := st.FinishRestoreExecution(id, "ok", ""); err != nil {
			t.Fatal(err)
		}
	}
	var rows int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM restore_executions`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows > restoreExecutionsKept+2 {
		t.Fatalf("%d rows", rows)
	}
	if _, err := st.GetRestoreExecution("long", "still-running"); err != nil {
		t.Fatalf("a running row must survive trimming: %v", err)
	}
}
