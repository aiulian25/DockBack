package store

import "testing"

func seedBackups(t *testing.T, st *Store) {
	t.Helper()
	rows := []*Backup{
		{ID: "b1", NodeID: "n1", TargetName: "postgres", Stack: "blog", Status: "success", Verified: "verified", CreatedAt: 100},
		{ID: "b2", NodeID: "n1", TargetName: "redis", Stack: "blog", Status: "success", Verified: "unverified", CreatedAt: 200},
		{ID: "b3", NodeID: "n1", TargetName: "nginx", Stack: "web", Status: "failed", Verified: "failed", CreatedAt: 300},
		{ID: "b4", NodeID: "n2", TargetName: "postgres", Stack: "shop", Status: "success", Verified: "verified", CreatedAt: 400},
	}
	for _, b := range rows {
		if err := st.CreateBackup(b); err != nil {
			t.Fatalf("seed %s: %v", b.ID, err)
		}
	}
}

func TestListBackupsPageFilterAndPage(t *testing.T) {
	st := tStore(t)
	seedBackups(t, st)

	// Node filter.
	got, total, err := st.ListBackupsPage(BackupFilter{NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(got) != 3 {
		t.Fatalf("node filter: total=%d len=%d, want 3/3", total, len(got))
	}
	// Newest first.
	if got[0].ID != "b3" {
		t.Errorf("order: first=%s, want b3 (newest)", got[0].ID)
	}

	// Status filter.
	_, total, _ = st.ListBackupsPage(BackupFilter{Status: "failed"})
	if total != 1 {
		t.Errorf("status=failed total=%d, want 1", total)
	}

	// Verified filter.
	_, total, _ = st.ListBackupsPage(BackupFilter{Verified: "verified"})
	if total != 2 {
		t.Errorf("verified total=%d, want 2", total)
	}

	// Search on target name (matches both postgres rows).
	_, total, _ = st.ListBackupsPage(BackupFilter{Query: "postgres"})
	if total != 2 {
		t.Errorf("q=postgres total=%d, want 2", total)
	}

	// Search on stack name.
	_, total, _ = st.ListBackupsPage(BackupFilter{Query: "web"})
	if total != 1 {
		t.Errorf("q=web total=%d, want 1", total)
	}

	// Pagination: total reflects all matches; page returns a slice.
	page1, total, _ := st.ListBackupsPage(BackupFilter{NodeID: "n1", Limit: 2, Offset: 0})
	if total != 3 || len(page1) != 2 {
		t.Fatalf("page1: total=%d len=%d, want 3/2", total, len(page1))
	}
	page2, _, _ := st.ListBackupsPage(BackupFilter{NodeID: "n1", Limit: 2, Offset: 2})
	if len(page2) != 1 {
		t.Fatalf("page2 len=%d, want 1", len(page2))
	}
	if page1[0].ID == page2[0].ID {
		t.Error("pages overlap")
	}
}

func TestEscapeLikeLiteralWildcards(t *testing.T) {
	st := tStore(t)
	// A target literally containing % must not act as a wildcard.
	_ = st.CreateBackup(&Backup{ID: "x1", NodeID: "n", TargetName: "100%db", Status: "success", CreatedAt: 1})
	_ = st.CreateBackup(&Backup{ID: "x2", NodeID: "n", TargetName: "otherdb", Status: "success", CreatedAt: 2})

	_, total, _ := st.ListBackupsPage(BackupFilter{Query: "100%db"})
	if total != 1 {
		t.Errorf("literal %% search total=%d, want 1", total)
	}
	// A bare % must match literally (only the 100%db row), not everything.
	_, total, _ = st.ListBackupsPage(BackupFilter{Query: "%"})
	if total != 1 {
		t.Errorf("bare %% search total=%d, want 1 (literal)", total)
	}
}
