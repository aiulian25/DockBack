//go:build testhooks

package api

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"dockback/internal/backup"
	"dockback/internal/notify"
	"dockback/internal/store"
)

// F200 — a chain head kept outside the machine.
//
// The audit chain is HMAC-linked under the master key, which makes it
// tamper-evident against everyone except the one attacker who matters here: a
// shell in this container holds the key, the anchor and the database, rewrites
// history end to end, and passes the built-in check. The beacon exists so there
// is a copy of the answer somewhere DockBack cannot reach.

// beaconServer is a server with a working audit chain and some history.
func beaconServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer(t)
	s.engine = &backup.Engine{Key: make([]byte, 32), KeyFP: "test-fp"}
	s.installAuditChain()
	for i := 0; i < 5; i++ {
		if err := s.store.Audit("admin", "test.action", "target-"+strconv.Itoa(i), "detail"); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// The beacon carries the FULL chain value and the entry number — the two things
// an operator needs to check it later. A truncated digest would make their own
// copy useless, which is the only copy that counts.
func TestBeaconPublishesTheFullHead(t *testing.T) {
	s := beaconServer(t)

	id, chain, err := s.auditHead()
	if err != nil || id == 0 || chain == "" {
		t.Fatalf("head = %d/%q err=%v", id, chain, err)
	}

	s.beaconAuditHead("test")

	// Recorded locally for display…
	gotID, gotChain, at, ok := s.lastAuditBeacon()
	if !ok {
		t.Fatal("the published head should be recorded for the UI")
	}
	if gotID != id || gotChain != chain {
		t.Errorf("recorded head = %d/%q, want %d/%q", gotID, gotChain, id, chain)
	}
	if time.Since(time.Unix(at, 0)) > time.Minute {
		t.Error("the record should carry when it was published")
	}
	// …and the message an operator keeps must carry the WHOLE value, not a
	// prefix: they paste it back to verify, and a truncated digest cannot be
	// compared against anything.
	msg := auditBeaconMessage(id, chain, time.Now(), "test")
	if !strings.Contains(msg, chain) {
		t.Error("the checkpoint message must carry the full chain value")
	}
	if !strings.Contains(msg, strconv.FormatInt(id, 10)) {
		t.Error("the checkpoint message must name the entry number")
	}
	// It must also say what to DO with it — a hex string with no instructions is
	// a message people delete.
	if !strings.Contains(msg, "Keep this message") || !strings.Contains(msg, "Verify against a checkpoint") {
		t.Errorf("the checkpoint should tell the operator how to use it: %q", msg)
	}
	// Info severity: a checkpoint is routine, and routing it as an alert would
	// train the operator to ignore the channel that carries the real ones.
	if notify.SeverityOf(notify.KindAuditBeacon) != notify.SevInfo {
		t.Error("a checkpoint is info, not an alert")
	}
}

// THE test for this feature: an attacker who can recompute the whole chain
// passes the built-in check and is still caught by the external checkpoint.
func TestRewrittenHistoryPassesLocalCheckButFailsTheBeacon(t *testing.T) {
	s := beaconServer(t)

	// The operator's checkpoint, taken while the trail was honest.
	beaconID, beaconChain, err := s.auditHead()
	if err != nil {
		t.Fatal(err)
	}

	// Now the attacker: they hold the master key, so they edit a row and re-mint
	// every link after it exactly as the app would.
	var rows []*store.AuditChainRow
	if err := s.store.WalkAuditSince(0, func(r *store.AuditChainRow) bool {
		rows = append(rows, r)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if len(rows) < 3 {
		t.Fatal("need history to rewrite")
	}
	victim := rows[1]
	if err := s.store.TestOnlyRewriteAudit(victim.ID, "detail", "innocent detail"); err != nil {
		t.Fatal(err)
	}
	// Re-chain from the edited row forward, under the same key.
	prev, _ := s.store.AuditChainAt(rows[0].ID)
	for _, r := range rows[1:] {
		detail := r.Detail
		if r.ID == victim.ID {
			detail = "innocent detail"
		}
		link := auditChainValue(s.engine.Key, prev, strconv.FormatInt(r.TS, 10), r.Actor, r.Action, r.Target, detail)
		if err := s.store.TestOnlyRewriteAudit(r.ID, "chain", link); err != nil {
			t.Fatal(err)
		}
		prev = link
	}

	// The built-in walk is satisfied — this is the gap the feature exists for.
	ok, _, _, err := s.auditChainVerify()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("precondition: a re-minted chain should pass the internal walk (that is the whole problem)")
	}

	// The operator's off-host checkpoint catches it.
	v, err := s.verifyAgainstBeacon(beaconID, beaconChain)
	if err != nil {
		t.Fatal(err)
	}
	if v.OK {
		t.Fatal("a rewritten trail must NOT match a checkpoint taken before the rewrite")
	}
	if !strings.Contains(v.Reason, "rewritten") {
		t.Errorf("the verdict should say what happened, got %q", v.Reason)
	}
}

// An honest trail matches its checkpoint, and rows added afterwards do not
// invalidate it — the checkpoint attests to a prefix, not to the whole future.
func TestBeaconMatchesAnUntamperedTrail(t *testing.T) {
	s := beaconServer(t)
	id, chain, _ := s.auditHead()

	v, err := s.verifyAgainstBeacon(id, chain)
	if err != nil {
		t.Fatal(err)
	}
	if !v.OK {
		t.Fatalf("an untouched trail must match its checkpoint: %s", v.Reason)
	}

	// New activity after the checkpoint is normal and must still match.
	_ = s.store.Audit("admin", "later.action", "t", "d")
	v, _ = s.verifyAgainstBeacon(id, chain)
	if !v.OK {
		t.Errorf("entries added after a checkpoint must not invalidate it: %s", v.Reason)
	}
	if v.Current <= v.HeadID {
		t.Error("the verdict should report the trail has grown past the checkpoint")
	}
}

// Deleting the tail is the other way to lie, and the checkpoint names it
// distinctly — "the trail ends before your checkpoint" is a different fact from
// "your entry was rewritten", and they need different responses.
func TestBeaconDetectsTailTruncation(t *testing.T) {
	s := beaconServer(t)
	id, chain, _ := s.auditHead()

	if err := s.store.TestOnlyDeleteAuditFrom(id); err != nil {
		t.Fatal(err)
	}
	v, err := s.verifyAgainstBeacon(id, chain)
	if err != nil {
		t.Fatal(err)
	}
	if v.OK {
		t.Fatal("a truncated trail must not match its checkpoint")
	}
	if !strings.Contains(v.Reason, "deleted") {
		t.Errorf("the verdict should name the deletion, got %q", v.Reason)
	}
}

// Empty or malformed input is a prompt, not a pass — a blank form must never
// read as "verified".
func TestBeaconRejectsEmptyInput(t *testing.T) {
	s := beaconServer(t)
	for _, c := range []struct {
		id    int64
		chain string
	}{
		{0, ""}, {0, "abc"}, {3, ""}, {3, "   "},
	} {
		v, err := s.verifyAgainstBeacon(c.id, c.chain)
		if err != nil {
			t.Fatal(err)
		}
		if v.OK {
			t.Errorf("id=%d chain=%q must not verify", c.id, c.chain)
		}
	}
}

// The cadence is clamped: a misconfiguration can neither flood the channel nor
// quietly stop attesting.
func TestAuditBeaconHoursClamped(t *testing.T) {
	s := newTestServer(t)
	if got := s.auditBeaconHours(notify.HeadBeaconConfig{IntervalHours: 0}); got != defaultAuditBeaconHours {
		t.Errorf("unset = %d, want the %d-hour default", got, defaultAuditBeaconHours)
	}
	if got := s.auditBeaconHours(notify.HeadBeaconConfig{IntervalHours: -5}); got != defaultAuditBeaconHours {
		t.Errorf("negative should fall back to the default, got %d", got)
	}
	if got := s.auditBeaconHours(notify.HeadBeaconConfig{IntervalHours: 100000}); got != 24*7 {
		t.Errorf("absurdly high = %d, want a week", got)
	}
	if got := s.auditBeaconHours(notify.HeadBeaconConfig{IntervalHours: 6}); got != 6 {
		t.Errorf("a sane value should be honoured, got %d", got)
	}
}

// A fresh install has nothing to attest to, and must publish nothing rather
// than a head that means nothing.
func TestBeaconSaysNothingWithNoHistory(t *testing.T) {
	s := newTestServer(t)
	s.engine = &backup.Engine{Key: make([]byte, 32)}
	s.beaconAuditHead("test")
	if _, _, _, ok := s.lastAuditBeacon(); ok {
		t.Error("an empty trail must not produce a checkpoint")
	}
}
