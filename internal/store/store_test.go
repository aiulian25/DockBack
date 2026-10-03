package store

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// SEC-6: quoteIdent must double-quote valid identifiers and panic on anything
// that could carry SQL-injection payloads (only constants should ever reach it).
func TestQuoteIdent(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"users", `"users"`},
		{"totp_last_step", `"totp_last_step"`},
		{"backups", `"backups"`},
	} {
		if got := quoteIdent(tc.in); got != tc.want {
			t.Errorf("quoteIdent(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"", "users; DROP TABLE users", "a b", `a"b`, "a-b", "a.b", "a)b", "(x"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("quoteIdent(%q) should have panicked", bad)
				}
			}()
			_ = quoteIdent(bad)
		}()
	}
}

// B2: ProtectedTargets returns only container names with a SUCCESSFUL backup on
// the given node, mapped to the newest such backup's timestamp.
func TestProtectedTargets(t *testing.T) {
	st := tStore(t)
	_ = st.CreateBackup(&Backup{ID: "a", NodeID: "n1", TargetName: "immich", Status: "success", CreatedAt: 10})
	_ = st.CreateBackup(&Backup{ID: "b", NodeID: "n1", TargetName: "immich", Status: "success", CreatedAt: 30}) // newer
	_ = st.CreateBackup(&Backup{ID: "c", NodeID: "n1", TargetName: "failed-app", Status: "failed", CreatedAt: 20})
	_ = st.CreateBackup(&Backup{ID: "d", NodeID: "n2", TargetName: "otherdb", Status: "success", CreatedAt: 5})

	m, err := st.ProtectedTargets("n1")
	if err != nil {
		t.Fatalf("ProtectedTargets: %v", err)
	}
	if len(m) != 1 {
		t.Fatalf("want 1 protected target on n1, got %d (%v)", len(m), m)
	}
	if m["immich"] != 30 {
		t.Errorf("immich newest ts = %d, want 30", m["immich"])
	}
	if _, ok := m["failed-app"]; ok {
		t.Error("a failed-only target must not count as protected")
	}
	if _, ok := m["otherdb"]; ok {
		t.Error("another node's target must not leak into n1")
	}
}

// ListBackupsForTarget must return a container's own backups even when they fall
// OUTSIDE a node-wide window — the exact case the node-wide-then-filter-in-Go
// approach gets wrong (Step 9; foundation for Steps 10/11).
func TestListBackupsForTarget(t *testing.T) {
	st := tStore(t)
	// "busy" fills a node-wide window with newer rows; "quiet" has a few older ones.
	for i := 0; i < 30; i++ {
		_ = st.CreateBackup(&Backup{ID: fmt.Sprintf("busy-%02d", i), NodeID: "n1", TargetName: "busy", Status: "success", CreatedAt: int64(100 + i)})
	}
	for i := 0; i < 3; i++ {
		_ = st.CreateBackup(&Backup{ID: fmt.Sprintf("quiet-%d", i), NodeID: "n1", TargetName: "quiet", Status: "success", CreatedAt: int64(1 + i)})
	}
	// Same target name on another node must never leak in.
	_ = st.CreateBackup(&Backup{ID: "quiet-n2", NodeID: "n2", TargetName: "quiet", Status: "success", CreatedAt: 50})

	// The node-wide window of 10 is entirely "busy" — this is the bug being fixed.
	if wide, err := st.ListBackups("n1", 10); err != nil {
		t.Fatal(err)
	} else {
		for _, b := range wide {
			if b.TargetName == "quiet" {
				t.Fatal("precondition: a 10-row node window should not reach the older quiet rows")
			}
		}
	}

	// The target query finds all 3 quiet rows regardless of the node-wide window.
	quiet, err := st.ListBackupsForTarget("n1", "quiet", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(quiet) != 3 {
		t.Fatalf("quiet backups: got %d, want 3", len(quiet))
	}
	// Newest first, and scoped to this node only.
	if quiet[0].ID != "quiet-2" || quiet[2].ID != "quiet-0" {
		t.Fatalf("wrong order: %s..%s, want quiet-2..quiet-0", quiet[0].ID, quiet[2].ID)
	}
	for _, b := range quiet {
		if b.NodeID != "n1" {
			t.Fatalf("cross-node leak: %s on node %s", b.ID, b.NodeID)
		}
	}

	// limit caps the busy target, newest first.
	busy, err := st.ListBackupsForTarget("n1", "busy", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(busy) != 5 || busy[0].ID != "busy-29" || busy[4].ID != "busy-25" {
		t.Fatalf("busy top-5: got %d rows (%s..), want 5 (busy-29..busy-25)", len(busy), func() string {
			if len(busy) > 0 {
				return busy[0].ID
			}
			return ""
		}())
	}

	// An unknown target is empty (never nil), and limit<=0 falls back to 100.
	none, err := st.ListBackupsForTarget("n1", "ghost", 0)
	if err != nil {
		t.Fatal(err)
	}
	if none == nil || len(none) != 0 {
		t.Fatalf("unknown target must be an empty (non-nil) slice, got %#v", none)
	}
}

func tStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestRunLog covers persisted per-run logs: append assigns increasing seqs,
// get returns them in order, trim keeps only the newest N, and DeleteBackup
// prunes a run's rows (F8).
func TestRunLog(t *testing.T) {
	st := tStore(t)

	// Empty run has no lines.
	if ll, err := st.GetRunLog("b1"); err != nil || len(ll) != 0 {
		t.Fatalf("empty run: got %d lines err=%v", len(ll), err)
	}

	// Append 2500 lines; seqs must be strictly increasing and content preserved.
	for i := 0; i < 2500; i++ {
		if err := st.AppendRunLog("b1", "INFO", fmt.Sprintf("line %d", i)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	// A second run's lines must not bleed into the first.
	if err := st.AppendRunLog("b2", "ERR", "other run"); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetRunLog("b1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2500 {
		t.Fatalf("expected 2500 lines before trim, got %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Seq <= got[i-1].Seq {
			t.Fatalf("seq not increasing at %d: %d <= %d", i, got[i].Seq, got[i-1].Seq)
		}
	}
	if got[0].Msg != "line 0" || got[len(got)-1].Msg != "line 2499" {
		t.Fatalf("order wrong: first=%q last=%q", got[0].Msg, got[len(got)-1].Msg)
	}

	// Trim to the newest 2000 — oldest lines drop, newest survive in order.
	if err := st.TrimRunLog("b1", 2000); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetRunLog("b1")
	if len(got) != 2000 {
		t.Fatalf("expected 2000 lines after trim, got %d", len(got))
	}
	if got[0].Msg != "line 500" || got[len(got)-1].Msg != "line 2499" {
		t.Fatalf("trim kept wrong window: first=%q last=%q", got[0].Msg, got[len(got)-1].Msg)
	}
	if ll, _ := st.GetRunLog("b2"); len(ll) != 1 {
		t.Fatalf("second run must be untouched, got %d", len(ll))
	}

	// DeleteBackup prunes only that run's rows.
	if err := st.DeleteBackup("b1"); err != nil {
		t.Fatal(err)
	}
	if ll, _ := st.GetRunLog("b1"); len(ll) != 0 {
		t.Fatalf("DeleteBackup must prune run_logs, got %d", len(ll))
	}
	if ll, _ := st.GetRunLog("b2"); len(ll) != 1 {
		t.Fatalf("unrelated run log must survive, got %d", len(ll))
	}
}

// TestListAuditPage covers audit pagination, search, and date-range filtering (F7).
func TestListAuditPage(t *testing.T) {
	st := tStore(t)
	base := int64(1_700_000_000)
	// Insert with explicit timestamps (Audit() would stamp all rows with now()).
	ins := func(ts int64, actor, action, target, detail string) {
		if _, err := st.db.Exec(`INSERT INTO audit(ts,actor,action,target,detail) VALUES(?,?,?,?,?)`, ts, actor, action, target, detail); err != nil {
			t.Fatal(err)
		}
	}
	ins(base+0, "alice", "login.ok", "", "ip=1.1.1.1")
	ins(base+10, "bob", "backup.start", "web", "node=n1")
	ins(base+20, "alice", "restore.start", "db", "node=n1")
	ins(base+30, "bob", "node.delete", "n2", "")
	ins(base+40, "carol", "backup.delete", "old", "retention")

	if n, err := st.CountAudit(); err != nil || n != 5 {
		t.Fatalf("CountAudit = %d, %v; want 5", n, err)
	}

	// Newest-first paging (ORDER BY id DESC): page 1 of size 2 = carol, bob(node.delete).
	list, total, err := st.ListAuditPage(AuditFilter{Limit: 2, Offset: 0})
	if err != nil || total != 5 || len(list) != 2 {
		t.Fatalf("page1: len=%d total=%d err=%v; want 2/5", len(list), total, err)
	}
	if list[0].Actor != "carol" || list[1].Action != "node.delete" {
		t.Fatalf("page1 order wrong: %q, %q", list[0].Actor, list[1].Action)
	}
	// Page 2.
	if l2, _, _ := st.ListAuditPage(AuditFilter{Limit: 2, Offset: 2}); len(l2) != 2 || l2[0].Action != "restore.start" {
		t.Fatalf("page2 wrong: %+v", l2)
	}

	// Limit<=0 = all matching (used by export).
	if all, total, _ := st.ListAuditPage(AuditFilter{Limit: 0}); len(all) != 5 || total != 5 {
		t.Fatalf("all: len=%d total=%d; want 5/5", len(all), total)
	}

	// Search matches across actor/action/target/detail.
	if _, total, _ := st.ListAuditPage(AuditFilter{Query: "alice"}); total != 2 {
		t.Fatalf("q=alice total=%d; want 2", total)
	}
	if _, total, _ := st.ListAuditPage(AuditFilter{Query: "node=n1"}); total != 2 {
		t.Fatalf("q=node=n1 total=%d; want 2", total)
	}
	// A LIKE wildcard in the query is matched literally (escapeLike), not as a wildcard.
	if _, total, _ := st.ListAuditPage(AuditFilter{Query: "%"}); total != 0 {
		t.Fatalf("q=%% must match literally (0 rows), got total=%d", total)
	}

	// Date range (inclusive).
	if _, total, _ := st.ListAuditPage(AuditFilter{From: base + 20}); total != 3 {
		t.Fatalf("from=+20 total=%d; want 3", total)
	}
	if _, total, _ := st.ListAuditPage(AuditFilter{To: base + 20}); total != 3 {
		t.Fatalf("to=+20 total=%d; want 3", total)
	}
	if _, total, _ := st.ListAuditPage(AuditFilter{From: base + 20, To: base + 30}); total != 2 {
		t.Fatalf("from=+20 to=+30 total=%d; want 2", total)
	}
	// Search + range combine.
	if _, total, _ := st.ListAuditPage(AuditFilter{Query: "alice", From: base + 20}); total != 1 {
		t.Fatalf("q=alice from=+20 total=%d; want 1", total)
	}
}

// backdate forces a session's timestamps so idle/expiry paths are testable
// without sleeping (in-package access to the raw db).
func (s *Store) backdate(t *testing.T, token string, createdAt, expiresAt, lastSeen int64) {
	t.Helper()
	// The row is keyed by the STORED key, not the cookie value.
	if _, err := s.db.Exec(`UPDATE sessions SET created_at=?, expires_at=?, last_seen=? WHERE token=?`,
		createdAt, expiresAt, lastSeen, SessionKey(token)); err != nil {
		t.Fatalf("backdate: %v", err)
	}
}

func TestSessionIdleTimeout(t *testing.T) {
	st := tStore(t)
	uid, err := st.CreateUser("admin", "h")
	if err != nil {
		t.Fatal(err)
	}
	// Fresh session is valid.
	if err := st.CreateSession("live", uid, 12*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SessionUser("live"); err != nil {
		t.Fatalf("fresh session should be valid: %v", err)
	}
	// Idle past the window (absolute expiry still far away) → invalid + deleted.
	_ = st.CreateSession("idle", uid, 12*time.Hour)
	now := time.Now().Unix()
	st.backdate(t, "idle", now-int64(SessionIdleTTL.Seconds())-3600, now+10*3600,
		now-int64(SessionIdleTTL.Seconds())-60)
	if _, _, err := st.SessionUser("idle"); err == nil {
		t.Fatal("idle session should be rejected")
	}
	if _, _, _, err := st.SessionInfo("idle"); err == nil {
		t.Fatal("idle session should be swept on access (SessionInfo)")
	}
}

func TestTouchSessionSlidesIdle(t *testing.T) {
	st := tStore(t)
	uid, _ := st.CreateUser("admin", "h")
	_ = st.CreateSession("tok", uid, 12*time.Hour)
	now := time.Now().Unix()
	// Within the absolute window but near the idle edge.
	st.backdate(t, "tok", now-3600, now+10*3600, now-int64(SessionIdleTTL.Seconds())+60)

	if err := st.TouchSession("tok"); err != nil {
		t.Fatalf("touch valid session: %v", err)
	}
	exp, idle, _, err := st.SessionInfo("tok")
	if err != nil {
		t.Fatalf("session info after touch: %v", err)
	}
	if idle < now+int64(SessionIdleTTL.Seconds())-5 {
		t.Fatalf("idle deadline not slid forward: idle=%d now=%d", idle, now)
	}
	if exp <= now { // absolute expiry untouched and still in the future
		t.Fatal("absolute expiry should be unchanged and future")
	}

	// A dead (idle) session can't be revived by a touch.
	_ = st.CreateSession("dead", uid, 12*time.Hour)
	st.backdate(t, "dead", now-2*int64(SessionIdleTTL.Seconds()), now+10*3600,
		now-2*int64(SessionIdleTTL.Seconds()))
	if err := st.TouchSession("dead"); err == nil {
		t.Fatal("touching an idle session should fail")
	}
}

func TestSweepExpiredSessions(t *testing.T) {
	st := tStore(t)
	uid, _ := st.CreateUser("admin", "h")
	now := time.Now().Unix()

	_ = st.CreateSession("valid", uid, 12*time.Hour) // last_seen=now, far expiry
	_ = st.SetSetting("csrf:"+SessionKey("valid"), "cv")
	_ = st.SetSetting("stepup:"+SessionKey("valid"), "1700000000")

	_ = st.CreateSession("expired", uid, 12*time.Hour)
	st.backdate(t, "expired", now-13*3600, now-3600, now-3600) // past absolute expiry
	_ = st.SetSetting("csrf:"+SessionKey("expired"), "x")
	_ = st.SetSetting("stepup:"+SessionKey("expired"), "1700000000")

	_ = st.CreateSession("idle", uid, 12*time.Hour)
	st.backdate(t, "idle", now-3600, now+10*3600, now-int64(SessionIdleTTL.Seconds())-60)
	_ = st.SetSetting("csrf:"+SessionKey("idle"), "y")
	_ = st.SetSetting("stepup:"+SessionKey("idle"), "1700000000")

	n, err := st.SweepExpiredSessions()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("want 2 swept, got %d", n)
	}
	if _, _, err := st.SessionUser("valid"); err != nil {
		t.Fatal("valid session must survive the sweep")
	}
	for _, tok := range []string{"expired", "idle"} {
		if v, _ := st.GetSetting("csrf:"+SessionKey(tok), ""); v != "" {
			t.Fatalf("csrf entry for %s should be swept", tok)
		}
		// A dead session's step-up grant must die with it, not linger forever in
		// settings (and get copied into every app backup).
		if v, _ := st.GetSetting("stepup:"+SessionKey(tok), ""); v != "" {
			t.Fatalf("stepup entry for %s should be swept", tok)
		}
	}
	// The still-valid session keeps both its csrf and step-up grant.
	if v, _ := st.GetSetting("stepup:"+SessionKey("valid"), ""); v == "" {
		t.Fatal("stepup entry for the still-valid session must survive the sweep")
	}
}

// SnapshotTo must scrub sessions, csrf tokens AND step-up grants from the copy,
// so an application backup (which is streamed offsite) never carries live
// session credentials — including the step-up grants that predate this fix.
func TestSnapshotToScrubsAuthState(t *testing.T) {
	st := tStore(t)
	uid, _ := st.CreateUser("admin", "h")
	_ = st.CreateSession("tok", uid, 12*time.Hour)
	_ = st.SetSetting("csrf:"+SessionKey("tok"), "c")
	_ = st.SetSetting("stepup:tok", "1700000000")
	_ = st.SetSetting("retention.generations", "10") // a normal setting must remain

	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := st.SnapshotTo(snap); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// Open the SNAPSHOT copy read-only and confirm the scrub landed in it.
	cp, err := Open(snap)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer cp.Close()
	var sessions, csrf, stepup, keep int
	_ = cp.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions)
	_ = cp.db.QueryRow(`SELECT COUNT(*) FROM settings WHERE key LIKE 'csrf:%'`).Scan(&csrf)
	_ = cp.db.QueryRow(`SELECT COUNT(*) FROM settings WHERE key LIKE 'stepup:%'`).Scan(&stepup)
	_ = cp.db.QueryRow(`SELECT COUNT(*) FROM settings WHERE key='retention.generations'`).Scan(&keep)
	if sessions != 0 {
		t.Errorf("snapshot must carry no sessions, found %d", sessions)
	}
	if csrf != 0 {
		t.Errorf("snapshot must carry no csrf entries, found %d", csrf)
	}
	if stepup != 0 {
		t.Errorf("snapshot must carry no step-up grants, found %d", stepup)
	}
	if keep != 1 {
		t.Errorf("snapshot must retain unrelated settings, found %d", keep)
	}
}

// ClearSessions (run once at startup) must scrub step-up grants too, not only
// csrf entries — otherwise every step-up ever performed leaves a permanent row.
func TestClearSessionsScrubsStepUp(t *testing.T) {
	st := tStore(t)
	uid, _ := st.CreateUser("admin", "h")
	_ = st.CreateSession("tok", uid, 12*time.Hour)
	_ = st.SetSetting("csrf:"+SessionKey("tok"), "c")
	_ = st.SetSetting("stepup:tok", "1700000000")
	// A non-session setting must be left untouched.
	_ = st.SetSetting("retention.generations", "10")

	if err := st.ClearSessions(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SessionUser("tok"); err == nil {
		t.Fatal("session should be cleared")
	}
	if v, _ := st.GetSetting("csrf:"+SessionKey("tok"), ""); v != "" {
		t.Fatal("csrf entry should be cleared")
	}
	if v, _ := st.GetSetting("stepup:tok", ""); v != "" {
		t.Fatal("stepup entry should be cleared")
	}
	if v, _ := st.GetSetting("retention.generations", ""); v != "10" {
		t.Fatal("unrelated settings must survive ClearSessions")
	}
}
