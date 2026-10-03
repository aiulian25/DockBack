package store

import (
	"fmt"
	"path/filepath"
	"testing"
)

// The audit table is append-only and unbounded, so anything that read "all of
// it" grew with the age of the deployment until it met the container's memory
// limit — during an export or an integrity check, which are exactly the
// operations an operator needs to succeed.

func auditStore(t *testing.T, rows int) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for i := 0; i < rows; i++ {
		if err := st.Audit("admin", "backup.start", fmt.Sprintf("app%d", i), "detail"); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

// More rows than one page, so the paging itself is exercised rather than a
// single query that happens to fit.
const auditTestRows = auditPageSize*2 + 37

func TestWalkAuditSinceVisitsEveryRowOnceInOrder(t *testing.T) {
	st := auditStore(t, auditTestRows)

	var seen []int64
	if err := st.WalkAuditSince(0, func(r *AuditChainRow) bool {
		seen = append(seen, r.ID)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != auditTestRows {
		t.Fatalf("walked %d rows, want %d — paging lost or repeated some", len(seen), auditTestRows)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] <= seen[i-1] {
			t.Fatalf("ids out of order or repeated at %d: %d then %d — the chain walk depends on this", i, seen[i-1], seen[i])
		}
	}

	// An anchor skips everything before it, which is what the verify uses.
	from := seen[len(seen)/2]
	n := 0
	if err := st.WalkAuditSince(from, func(r *AuditChainRow) bool {
		if r.ID <= from {
			t.Errorf("row %d is not after the anchor %d", r.ID, from)
		}
		n++
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if n != auditTestRows-len(seen)/2-1 {
		t.Errorf("walked %d rows after the anchor, want %d", n, auditTestRows-len(seen)/2-1)
	}
}

// Stopping early is how a broken chain link ends the walk: the rows after it
// cannot be judged, so they must not be read.
func TestWalkAuditSinceStopsWhenAsked(t *testing.T) {
	st := auditStore(t, auditTestRows)
	n := 0
	if err := st.WalkAuditSince(0, func(*AuditChainRow) bool {
		n++
		return n < 5
	}); err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("visited %d rows after asking to stop at 5", n)
	}
}

func TestWalkAuditFilteredMatchesTheListing(t *testing.T) {
	st := auditStore(t, auditTestRows)

	// The walk and the paged listing must select exactly the same rows, or an
	// export would disagree with the page the operator ran it from.
	_, total, err := st.ListAuditPage(AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	walked := 0
	if err := st.WalkAuditFiltered(AuditFilter{}, func(*AuditEntry) bool { walked++; return true }); err != nil {
		t.Fatal(err)
	}
	if walked != total {
		t.Errorf("walked %d rows, the listing counts %d", walked, total)
	}

	// A filter narrows both the same way.
	f := AuditFilter{Query: "app7"}
	_, filteredTotal, err := st.ListAuditPage(f)
	if err != nil {
		t.Fatal(err)
	}
	if filteredTotal == 0 {
		t.Fatal("the query should match something")
	}
	walked = 0
	if err := st.WalkAuditFiltered(f, func(e *AuditEntry) bool {
		if e.Target == "" {
			t.Error("a walked row must be populated")
		}
		walked++
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if walked != filteredTotal {
		t.Errorf("filtered walk saw %d rows, the listing counts %d", walked, filteredTotal)
	}

	// Newest first, matching the page the export is launched from.
	var first, second int64
	seen := 0
	_ = st.WalkAuditFiltered(AuditFilter{}, func(e *AuditEntry) bool {
		if seen == 0 {
			first = e.TS
		} else if seen == 1 {
			second = e.TS
		}
		seen++
		return seen < 2
	})
	if first < second {
		t.Errorf("rows must come back newest-first: %d then %d", first, second)
	}
}

// An empty table is a clean, empty walk rather than an error.
func TestWalkAuditOnAnEmptyTable(t *testing.T) {
	st := auditStore(t, 0)
	n := 0
	if err := st.WalkAuditSince(0, func(*AuditChainRow) bool { n++; return true }); err != nil {
		t.Fatal(err)
	}
	if err := st.WalkAuditFiltered(AuditFilter{}, func(*AuditEntry) bool { n++; return true }); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("an empty table walked %d rows", n)
	}
}
