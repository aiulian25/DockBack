package backup

import (
	"encoding/json"
	"testing"
)

// TestPlanConsistencyOrdering is the F33 acceptance: the recorded op order pauses
// every app BEFORE any database dump and resumes it only AFTER the last volume
// tar, so nothing writes to the app tier while its DB + volumes are captured.
func TestPlanConsistencyOrdering(t *testing.T) {
	// A typical stack: an app, a Postgres DB, and a Redis cache (both data-tier).
	members := []consistencyMember{
		{service: "app", isDB: false, running: true},
		{service: "db", isDB: true, running: true},
		{service: "cache", isDB: true, running: true},
	}
	ops := planConsistency(members)

	// Index helpers over the recorded op stream ("the fake exec recorder").
	firstOf := func(kind, svc string) int {
		for i, op := range ops {
			if op.kind == kind && (svc == "" || op.service == svc) {
				return i
			}
		}
		return -1
	}
	lastOf := func(kind string) int {
		last := -1
		for i, op := range ops {
			if op.kind == kind {
				last = i
			}
		}
		return last
	}

	pauseApp := firstOf("pause", "app")
	if pauseApp < 0 {
		t.Fatal("app was never paused")
	}
	// The app pause must precede EVERY dump.
	if d := firstOf("dump", ""); d < 0 || pauseApp > d {
		t.Fatalf("app pause (%d) must come before the first dump (%d)", pauseApp, d)
	}
	// The app must resume only after the LAST volume tar.
	resumeApp := firstOf("resume", "app")
	if lastTar := lastOf("tar"); resumeApp < 0 || resumeApp < lastTar {
		t.Fatalf("app resume (%d) must come after the last tar (%d)", resumeApp, lastTar)
	}
	// A database is never paused or resumed (dumped live).
	for _, op := range ops {
		if (op.kind == "pause" || op.kind == "resume") && (op.service == "db" || op.service == "cache") {
			t.Fatalf("database %q must never be %sd", op.service, op.kind)
		}
	}
	// Every service (app + both DBs) has its volumes tarred inside the window.
	for _, svc := range []string{"app", "db", "cache"} {
		if firstOf("tar", svc) < 0 {
			t.Errorf("service %q was never tarred", svc)
		}
	}
}

// TestPlanConsistencyStoppedAndReverse: a stopped app is neither paused nor
// resumed (nothing to freeze), yet is still tarred; running apps resume in the
// reverse of their pause order.
func TestPlanConsistencyStoppedAndReverse(t *testing.T) {
	members := []consistencyMember{
		{service: "web", isDB: false, running: true},
		{service: "worker", isDB: false, running: true},
		{service: "idle", isDB: false, running: false}, // stopped app
	}
	ops := planConsistency(members)

	var pauses, resumes, tars []string
	for _, op := range ops {
		switch op.kind {
		case "pause":
			pauses = append(pauses, op.service)
		case "resume":
			resumes = append(resumes, op.service)
		case "tar":
			tars = append(tars, op.service)
		}
	}
	// Stopped "idle" is paused/resumed nowhere...
	for _, p := range pauses {
		if p == "idle" {
			t.Error("stopped service should not be paused")
		}
	}
	// ...but is still captured.
	found := false
	for _, s := range tars {
		if s == "idle" {
			found = true
		}
	}
	if !found {
		t.Error("stopped service should still be tarred")
	}
	// Resume order is the reverse of pause order.
	if len(pauses) != 2 || len(resumes) != 2 {
		t.Fatalf("want 2 pauses/2 resumes, got %d/%d", len(pauses), len(resumes))
	}
	if resumes[0] != pauses[len(pauses)-1] || resumes[len(resumes)-1] != pauses[0] {
		t.Errorf("resume order %v is not the reverse of pause order %v", resumes, pauses)
	}
}

// TestManifestConsistencyGroupRoundTrip: the group id + timestamp survive JSON
// marshal/unmarshal (they're what ties a group's per-service backups together),
// and stay absent from an ordinary backup's manifest.
func TestManifestConsistencyGroupRoundTrip(t *testing.T) {
	m := &Manifest{Version: ManifestVersion, BackupID: "b1", ConsistencyGroup: "cg-123-abcd", ConsistencyAt: 123}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var got Manifest
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.ConsistencyGroup != "cg-123-abcd" || got.ConsistencyAt != 123 {
		t.Fatalf("round-trip lost group tag: %+v", got)
	}

	// omitempty: a normal backup carries neither field.
	plain, _ := json.Marshal(&Manifest{Version: ManifestVersion, BackupID: "b2"})
	var mp map[string]any
	_ = json.Unmarshal(plain, &mp)
	if _, ok := mp["consistency_group"]; ok {
		t.Error("consistency_group should be omitted from an ordinary manifest")
	}
	if _, ok := mp["consistency_at"]; ok {
		t.Error("consistency_at should be omitted from an ordinary manifest")
	}
}
