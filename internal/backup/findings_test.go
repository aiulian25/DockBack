package backup

import (
	"encoding/json"
	"strings"
	"testing"
)

// The manifest sidecar travels to every destination and is read by tooling that
// predates any given field, so a new one must be additive in both directions: it
// round-trips, and an archive written before it existed still parses.
func TestFindingsRoundTripAndBackCompat(t *testing.T) {
	man := &Manifest{
		Version:  ManifestVersion,
		BackupID: "b1",
		Findings: []Finding{
			{Code: "duplicate-mount-target", Severity: FindingDanger, Subject: "/volume1/docker/nc/db",
				Message: "bound at /var/lib/mysql and /etc/mysql/conf.d — the database scans its own data directory for configuration"},
			{Code: "restart-policy-not-boot-safe", Severity: FindingWarn,
				Message: "on-failure does not start the container when Docker starts; promote to unless-stopped"},
		},
	}
	raw, err := json.Marshal(man)
	if err != nil {
		t.Fatal(err)
	}

	var got Manifest
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Findings) != 2 {
		t.Fatalf("findings = %d, want 2", len(got.Findings))
	}
	if got.Findings[0] != man.Findings[0] || got.Findings[1] != man.Findings[1] {
		t.Errorf("round trip changed the findings:\n got %+v\nwant %+v", got.Findings, man.Findings)
	}
	// A finding with no subject must not gain an empty one in the JSON — the
	// field is omitempty precisely so "about the container as a whole" reads as
	// absence rather than as an empty path.
	if strings.Contains(string(raw), `"subject":""`) {
		t.Errorf("an absent subject must be omitted, got %s", raw)
	}

	// A manifest written before this field existed.
	var old Manifest
	if err := json.Unmarshal([]byte(`{"version":1,"backup_id":"b0","volumes":[]}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Findings != nil {
		t.Errorf("a pre-change manifest must have no findings, got %+v", old.Findings)
	}
	// And a manifest with none must not emit the key at all, so the sidecar of an
	// ordinary backup is byte-identical to what it was before this step.
	clean, err := json.Marshal(&Manifest{Version: ManifestVersion, BackupID: "b0"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(clean), "findings") {
		t.Errorf("no findings must emit no key, got %s", clean)
	}
}

// addFinding is the single channel every later check writes to, so its
// invariants are worth more than the checks themselves.
func TestAddFinding(t *testing.T) {
	var logged []string
	e := &Engine{Log: func(_, level, msg string) { logged = append(logged, level+" "+msg) }}
	man := &Manifest{}

	e.addFinding(man, "r1", "duplicate-mount-target", FindingDanger, "/srv/db", "bound at two destinations")
	e.addFinding(man, "r1", "local-tag-drifted", FindingInfo, "app:latest", "pulling this tag today upgrades the app")

	if len(man.Findings) != 2 {
		t.Fatalf("findings = %d, want 2: %+v", len(man.Findings), man.Findings)
	}
	// Severity decides the log level; danger has no louder level than warn in the
	// run log, and travels in the manifest for the UI to render.
	if !strings.HasPrefix(logged[0], "WARN ") {
		t.Errorf("danger must log at WARN, got %q", logged[0])
	}
	if !strings.HasPrefix(logged[1], "INFO ") {
		t.Errorf("info must log at INFO, got %q", logged[1])
	}
	for i, want := range []string{"/srv/db", "app:latest"} {
		if !strings.Contains(logged[i], want) {
			t.Errorf("the log line must name the subject %q, got %q", want, logged[i])
		}
	}

	// The same defect is reachable from several checks; it must be recorded once.
	e.addFinding(man, "r1", "duplicate-mount-target", FindingDanger, "/srv/db", "bound at two destinations")
	if len(man.Findings) != 2 {
		t.Errorf("a repeat of (code,subject) must not be appended twice: %+v", man.Findings)
	}
	// A different subject under the same code IS a different finding.
	e.addFinding(man, "r1", "duplicate-mount-target", FindingDanger, "/srv/other", "bound at two destinations")
	if len(man.Findings) != 3 {
		t.Errorf("a distinct subject must be recorded: %+v", man.Findings)
	}

	// A finding about the container as a whole omits the subject from the line.
	logged = nil
	e.addFinding(man, "r1", "healthcheck-cannot-fail", FindingWarn, "", "redis-cli ping exits 0 on NOAUTH")
	if strings.Contains(logged[0], " — ") {
		t.Errorf("no subject means no subject separator, got %q", logged[0])
	}

	// Nothing incomplete is ever recorded, and an unknown severity fails safe to
	// warn rather than reaching the UI as a level it cannot render.
	before := len(man.Findings)
	e.addFinding(man, "r1", "", FindingWarn, "x", "no code")
	e.addFinding(man, "r1", "no-message", FindingWarn, "x", "")
	e.addFinding(nil, "r1", "nil-manifest", FindingWarn, "x", "dropped")
	if len(man.Findings) != before {
		t.Errorf("incomplete findings must be dropped: %+v", man.Findings)
	}
	e.addFinding(man, "r1", "odd-severity", "critical", "", "unknown level")
	if man.Findings[len(man.Findings)-1].Severity != FindingWarn {
		t.Errorf("an unknown severity must fail safe to warn, got %q", man.Findings[len(man.Findings)-1].Severity)
	}
}
