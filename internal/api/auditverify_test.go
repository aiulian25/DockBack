package api

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"dockback/internal/backup"
	"dockback/internal/store"
)

// F68: the audit trail's hash chain detects silent modification, deletion, and
// reordering of rows written after the anchor — and never flags rows before it.
func TestAuditVerifyChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st, engine: &backup.Engine{Key: make([]byte, 32)}}

	// Two legacy rows BEFORE the feature: plain, unchained.
	_ = st.Audit("legacy", "old.action", "x", "one")
	_ = st.Audit("legacy", "old.action", "y", "two")

	s.installAuditChain() // records anchor = 2, installs the chainer

	// Fresh-install semantics: nothing after the anchor → ok with 0 checked.
	ok, checked, _, err := s.auditChainVerify()
	if err != nil || !ok || checked != 0 {
		t.Fatalf("fresh chain: ok=%v checked=%d err=%v", ok, checked, err)
	}

	// Five audited actions form an intact chain.
	for i := 0; i < 5; i++ {
		if err := st.Audit("admin", "test.action", fmt.Sprintf("t%d", i), "detail"); err != nil {
			t.Fatal(err)
		}
	}
	ok, checked, _, err = s.auditChainVerify()
	if err != nil || !ok || checked != 5 {
		t.Fatalf("intact chain: ok=%v checked=%d err=%v", ok, checked, err)
	}

	// Raw second connection — the attacker's `sqlite3 dockback.db "UPDATE …"`.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	// 1) Tampering with a PRE-anchor row never fails verification (grandfathered).
	if _, err := raw.Exec(`UPDATE audit SET detail='rewritten history' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if ok, _, _, _ := s.auditChainVerify(); !ok {
		t.Fatal("pre-anchor rows must never fail verification")
	}

	// 2) Modifying a chained row is detected AT that row.
	var midID int64
	if err := raw.QueryRow(`SELECT id FROM audit WHERE target='t2'`).Scan(&midID); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE audit SET detail='' WHERE id=?`, midID); err != nil {
		t.Fatal(err)
	}
	ok, _, firstBad, _ := s.auditChainVerify()
	if ok || firstBad != midID {
		t.Fatalf("modified row: ok=%v first_bad=%d want %d", ok, firstBad, midID)
	}
	if _, err := raw.Exec(`UPDATE audit SET detail='detail' WHERE id=?`, midID); err != nil {
		t.Fatal(err) // restore — chain intact again
	}
	if ok, _, _, _ := s.auditChainVerify(); !ok {
		t.Fatal("restored row must verify again")
	}

	// 3) Reordering (swapping two rows' contents) is detected.
	if _, err := raw.Exec(`UPDATE audit SET target='t3' WHERE id=?`, midID); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE audit SET target='t2' WHERE id=?`, midID+1); err != nil {
		t.Fatal(err)
	}
	if ok, _, firstBad, _ := s.auditChainVerify(); ok || firstBad != midID {
		t.Fatalf("reordered rows must fail at the first swapped row, ok=%v first_bad=%d", ok, firstBad)
	}
	if _, err := raw.Exec(`UPDATE audit SET target='t2' WHERE id=?`, midID); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE audit SET target='t3' WHERE id=?`, midID+1); err != nil {
		t.Fatal(err)
	}
	if ok, _, _, _ := s.auditChainVerify(); !ok {
		t.Fatal("un-swapped rows must verify again")
	}

	// 4) Deleting a middle row breaks the next row's linkage.
	if _, err := raw.Exec(`DELETE FROM audit WHERE id=?`, midID); err != nil {
		t.Fatal(err)
	}
	ok, _, firstBad, _ = s.auditChainVerify()
	if ok || firstBad != midID+1 {
		t.Fatalf("deleted middle row: ok=%v first_bad=%d want %d", ok, firstBad, midID+1)
	}
}
