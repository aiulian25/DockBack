package api

import (
	"fmt"
	"strings"
	"testing"

	"dockback/internal/backup"
	"dockback/internal/store"
)

func destOK() backup.Location { return backup.Location{Kind: "dest", Name: "NAS", Type: "smb"} }
func destFail() backup.Location {
	return backup.Location{Kind: "dest", Name: "NAS", Type: "smb", Status: "failed"}
}
func local() backup.Location  { return backup.Location{Kind: "local", Name: "local", Type: "local"} }
func drillPass() *store.Drill { return &store.Drill{OK: true, RanAt: 100} }
func drillFail() *store.Drill { return &store.Drill{OK: false, RanAt: 100} }
func partialMan() *backup.Manifest {
	return &backup.Manifest{SkippedMounts: []backup.SkippedMount{{}}}
}

// F83: every skip covered by another container's backups — intentional
// single-capture of a shared bind, not data loss.
func coveredMan() *backup.Manifest {
	return &backup.Manifest{SkippedMounts: []backup.SkippedMount{{Destination: "/upload", Type: "bind", CoveredBy: "server"}}}
}

func TestGradeBackup(t *testing.T) {
	cases := []struct {
		name  string
		b     *store.Backup
		man   *backup.Manifest
		locs  []backup.Location
		drill *store.Drill
		want  string
	}{
		// A — everything green.
		{"A verified+drilled+offsite", &store.Backup{Verified: "verified"}, &backup.Manifest{}, []backup.Location{local(), destOK()}, drillPass(), "A"},
		// B — verified + offsite + complete, but never drilled.
		{"B verified+offsite, no drill", &store.Backup{Verified: "verified"}, &backup.Manifest{}, []backup.Location{local(), destOK()}, nil, "B"},
		// C — verified only local.
		{"C verified local-only", &store.Backup{Verified: "verified"}, &backup.Manifest{}, []backup.Location{local()}, drillPass(), "C"},
		// F103: a database captured as RAW FILES (dump tools missing) caps at C even
		// when every other signal is green — a live engine's files can be torn, which
		// is exactly what the dump feature exists to prevent.
		{"C raw-file DB capped", &store.Backup{Verified: "verified"}, dbFallbackMan(), []backup.Location{local(), destOK()}, drillPass(), "C"},
		// A worse signal still dominates it.
		{"D raw-file DB with failed drill", &store.Backup{Verified: "verified"}, dbFallbackMan(), []backup.Location{local(), destOK()}, drillFail(), "D"},
		// C — PARTIAL caps at C even when drilled + offsite.
		{"C partial capped", &store.Backup{Verified: "verified"}, partialMan(), []backup.Location{local(), destOK()}, drillPass(), "C"},
		// A — a covered-only skip (F83 shared bind) is NOT partial.
		{"A covered skip not partial", &store.Backup{Verified: "verified"}, coveredMan(), []backup.Location{local(), destOK()}, drillPass(), "A"},
		// C — a mix of covered and uncovered skips is still partial.
		{"C mixed skips still partial", &store.Backup{Verified: "verified"}, &backup.Manifest{SkippedMounts: []backup.SkippedMount{{Destination: "/upload", CoveredBy: "server"}, {Destination: "/media"}}}, []backup.Location{local(), destOK()}, drillPass(), "C"},
		// D — a failed offsite copy.
		{"D offsite failed", &store.Backup{Verified: "verified"}, &backup.Manifest{}, []backup.Location{local(), destFail()}, drillPass(), "D"},
		// D — the last drill failed.
		{"D drill failed", &store.Backup{Verified: "verified"}, &backup.Manifest{}, []backup.Location{local(), destOK()}, drillFail(), "D"},
		// F — unverified.
		{"F unverified", &store.Backup{Verified: "unverified"}, &backup.Manifest{}, []backup.Location{local(), destOK()}, drillPass(), "F"},
		// F — failed verification.
		{"F verify failed", &store.Backup{Verified: "failed"}, &backup.Manifest{}, []backup.Location{local(), destOK()}, drillPass(), "F"},
		// F — key mismatch dominates every other (green) signal.
		{"F key mismatch dominates", &store.Backup{Verified: "verified", KeyMismatch: true}, &backup.Manifest{}, []backup.Location{local(), destOK()}, drillPass(), "F"},
	}
	for _, c := range cases {
		got, _ := gradeBackup(c.b, c.man, c.locs, c.drill, nil)
		if got != c.want {
			t.Errorf("%s: grade=%s want %s", c.name, got, c.want)
		}
	}
}

func TestGradeReasons(t *testing.T) {
	// A leaves no reasons.
	if _, reasons := gradeBackup(&store.Backup{Verified: "verified"}, &backup.Manifest{}, []backup.Location{destOK()}, drillPass(), nil); len(reasons) != 0 {
		t.Errorf("A grade must have no reasons, got %v", reasons)
	}
	// A local-only, never-drilled C names BOTH gaps (grade set by local-only, but the
	// tooltip must still surface the drill gap).
	_, reasons := gradeBackup(&store.Backup{Verified: "verified"}, &backup.Manifest{}, []backup.Location{local()}, nil, nil)
	joined := strings.Join(reasons, " | ")
	if !strings.Contains(joined, "offsite") || !strings.Contains(joined, "drill") {
		t.Errorf("C reasons should name both the missing offsite and the missing drill: %q", joined)
	}
	// Key mismatch reason is present even though the grade is F.
	_, r2 := gradeBackup(&store.Backup{Verified: "verified", KeyMismatch: true}, &backup.Manifest{}, []backup.Location{destOK()}, drillPass(), nil)
	if !strings.Contains(strings.Join(r2, " "), "different key") {
		t.Errorf("key-mismatch reason missing: %v", r2)
	}
}

// Perf Fix 8: the slim list-row summary carries exactly what the chips read,
// with F83 covered-skip semantics, and blanks the raw blobs.
func TestSummarizeListRow(t *testing.T) {
	man := &backup.Manifest{
		Incremental: true, ChainDepth: 3, Image: "example/app:1", ImageDigest: "sha256:" + strings.Repeat("a", 64),
		ImageTar:  &backup.ImageTarRef{Ref: "example/app:1", Bytes: 10},
		Databases: []backup.DBDump{{Service: "db", Engine: "postgres"}},
		SkippedMounts: []backup.SkippedMount{
			{Destination: "/upload", CoveredBy: "server"},
			{Destination: "/media"}, // uncovered → partial
		},
	}
	locs := []backup.Location{
		{Kind: "local", Name: "Local", Type: "local"},
		{Kind: "dest", Name: "offsite", Type: "s3", Status: "failed", Detail: "timeout", Immutable: true},
	}
	b := &store.Backup{ID: "b1", Status: "success", ManifestJSON: "{...}", VerificationJSON: "{...}", LocationsJSON: "[...]"}
	summarizeListRow(b, man, locs)

	s := b.Summary
	if s == nil || !s.Partial || s.CoveredSkips != 1 || s.CoveredBy != "server" {
		t.Fatalf("skip semantics wrong: %+v", s)
	}
	if !s.Incremental || s.ChainDepth != 3 || !s.ImageBundled || s.DBCount != 1 || s.Image != "example/app:1" {
		t.Fatalf("manifest digest wrong: %+v", s)
	}
	if len(s.Copies) != 2 || s.Copies[1].Status != "failed" || !s.Copies[1].Immutable || s.Copies[1].Detail != "timeout" {
		t.Fatalf("copies digest wrong: %+v", s.Copies)
	}
	if b.ManifestJSON != "" || b.VerificationJSON != "" || b.LocationsJSON != "" {
		t.Fatal("slim rows must not carry raw blobs")
	}

	// Covered-only skips: NOT partial (F83).
	b2 := &store.Backup{ID: "b2", Status: "success"}
	summarizeListRow(b2, &backup.Manifest{SkippedMounts: []backup.SkippedMount{{Destination: "/u", CoveredBy: "srv"}}}, nil)
	if b2.Summary.Partial {
		t.Fatal("covered-only skips must not be partial in the summary")
	}
}

// F75: a configured-and-failing standby adds a reason line but NEVER changes
// the grade — standby readiness belongs to the target, not to this backup.
func TestGradeStandbyReason(t *testing.T) {
	failing := &store.Standby{LastRun: 100, LastOK: false}
	grade, reasons := gradeBackup(&store.Backup{Verified: "verified"}, &backup.Manifest{}, []backup.Location{destOK()}, drillPass(), failing)
	if grade != "A" {
		t.Fatalf("failing standby must not dent the grade: got %s", grade)
	}
	if !strings.Contains(strings.Join(reasons, " "), "standby rehearsal") {
		t.Fatalf("failing standby must appear in reasons: %v", reasons)
	}
	// Proven and never-rehearsed standbys add nothing.
	for _, sb := range []*store.Standby{{LastRun: 100, LastOK: true}, {LastRun: 0}} {
		if _, r := gradeBackup(&store.Backup{Verified: "verified"}, &backup.Manifest{}, []backup.Location{destOK()}, drillPass(), sb); len(r) != 0 {
			t.Fatalf("standby %+v must add no reasons, got %v", sb, r)
		}
	}
	// On a worse grade the standby reason still rides along with the others.
	grade, reasons = gradeBackup(&store.Backup{Verified: "verified"}, &backup.Manifest{}, []backup.Location{local()}, nil, failing)
	if grade != "C" || !strings.Contains(strings.Join(reasons, " "), "standby rehearsal") {
		t.Fatalf("standby reason must coexist with other reasons: grade=%s reasons=%v", grade, reasons)
	}
}

// dbFallbackMan is a manifest whose container was detected as a database but had
// no dump tools, so its data was captured as raw files (F103).
func dbFallbackMan() *backup.Manifest {
	return &backup.Manifest{DBFallback: "postgres: dump tools not found — captured as raw files"}
}

// The grade cap is only half the point: the operator has to be told WHY, in
// terms that name the risk rather than the mechanism.
func TestGradeReasonsNamesRawFileDBCapture(t *testing.T) {
	grade, reasons := gradeBackup(&store.Backup{Verified: "verified"}, dbFallbackMan(),
		[]backup.Location{local(), destOK()}, drillPass(), nil)
	if grade != "C" {
		t.Fatalf("grade = %q, want C", grade)
	}
	joined := strings.Join(reasons, "\n")
	for _, want := range []string{"raw files", "dump tools", "torn"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the reason must name %q:\n%s", want, joined)
		}
	}
	// An ordinary backup must gain no such reason — this cannot become noise on
	// every row, or operators stop reading the list.
	if _, clean := gradeBackup(&store.Backup{Verified: "verified"}, &backup.Manifest{},
		[]backup.Location{local(), destOK()}, drillPass(), nil); len(clean) != 0 {
		t.Fatalf("a normal backup must stay reason-free: %v", clean)
	}
	// Whitespace-only is not a fallback — an empty note must not cap the grade.
	if g, _ := gradeBackup(&store.Backup{Verified: "verified"}, &backup.Manifest{DBFallback: "   "},
		[]backup.Location{local(), destOK()}, drillPass(), nil); g != "A" {
		t.Fatalf("a blank db_fallback must not cap the grade, got %q", g)
	}
}

// The slim list row carries the flag so the Backups page can chip it without
// shipping a manifest blob per row.
func TestSummarizeListRowCarriesDBFallback(t *testing.T) {
	b := &store.Backup{ID: "b1"}
	summarizeListRow(b, dbFallbackMan(), nil)
	if b.Summary == nil || !b.Summary.DBFallback {
		t.Fatalf("summary must flag the raw-file DB capture: %+v", b.Summary)
	}
	plain := &store.Backup{ID: "b2"}
	summarizeListRow(plain, &backup.Manifest{}, nil)
	if plain.Summary.DBFallback {
		t.Fatal("an ordinary backup must not be flagged")
	}
}

// TestGradeCredentialStoreWithoutWriteOnly (F122): an archive whose contents are
// the keys to OTHER systems, protected by a key that lives on a running server —
// often one of the very machines the archive grants access to.
//
// The backup itself is sound; what it is protected BY is the finding, so it caps
// at C rather than failing. Grading it A with no reasons was the real problem:
// nothing anywhere said the most dangerous archive in the fleet was openable by
// the machine holding it.
func TestGradeCredentialStoreWithoutWriteOnly(t *testing.T) {
	man := &backup.Manifest{Image: "fnsys/dockhand:latest"}
	grade, reasons := gradeBackup(&store.Backup{Verified: "verified"}, man,
		[]backup.Location{local(), destOK()}, drillPass(), nil)
	if grade != "C" {
		t.Fatalf("grade = %q, want C — a fleet-credential archive this server can open is not an A", grade)
	}
	joined := strings.Join(reasons, "\n")
	for _, want := range []string{"decryptable by this server", "write-only"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the reason must name %q:\n%s", want, joined)
		}
	}
}

// Sealed to an offline key, the finding is answered and the grade is unaffected —
// which is the whole point of the recommendation.
func TestGradeCredentialStoreWithWriteOnlyIsClean(t *testing.T) {
	man := &backup.Manifest{Image: "fnsys/dockhand:latest", WrappedKeyPub: "seal", BackupPubFP: "fp"}
	grade, reasons := gradeBackup(&store.Backup{Verified: "verified"}, man,
		[]backup.Location{local(), destOK()}, drillPass(), nil)
	for _, r := range reasons {
		if strings.Contains(r, "decryptable by this server") {
			t.Fatalf("a write-only archive must not carry the credential-store reason: %v", reasons)
		}
	}
	// B, not A: write-only backups can't be drilled by DockBack, which the
	// existing grading already accounts for. The point is that OUR reason is gone.
	if grade == "C" {
		t.Fatalf("write-only answers the finding — grade should not be capped at C, got %q", grade)
	}
}

// An ordinary app must gain nothing here, whatever its own data holds. A warning
// that appears on every row is one nobody reads.
func TestGradeOrdinaryAppNoCredentialStoreReason(t *testing.T) {
	for _, img := range []string{"nginx:alpine", "ghcr.io/advplyr/audiobookshelf:latest", "", "postgres:16"} {
		grade, reasons := gradeBackup(&store.Backup{Verified: "verified"},
			&backup.Manifest{Image: img}, []backup.Location{local(), destOK()}, drillPass(), nil)
		if grade != "A" || len(reasons) != 0 {
			t.Errorf("%q must grade A with no reasons, got %q %v", img, grade, reasons)
		}
	}
}

// TestPartialReasonNamesTheSize (F130) is the Immich case: 20.7 KB of
// configuration captured and 58 GB of photos skipped by the large-bind default.
// "Some data mounts were skipped" made that read as a detail. It was 99.97% of
// the data, and a restore from it would have had metadata and no photos.
func TestPartialReasonNamesTheSize(t *testing.T) {
	man := &backup.Manifest{SkippedMounts: []backup.SkippedMount{
		{Destination: "/usr/src/app/upload", Type: "bind", Bytes: 58 << 30, Reason: "large bind"},
	}}
	grade, reasons := gradeBackup(&store.Backup{Verified: "verified"}, man,
		[]backup.Location{local(), destOK()}, drillPass(), nil)
	if grade != "C" {
		t.Fatalf("a partial capture still caps at C, got %q", grade)
	}
	joined := strings.Join(reasons, "\n")
	for _, want := range []string{"58", "GB", "/usr/src/app/upload", "NOT captured"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the reason must name %q so the scale is legible:\n%s", want, joined)
		}
	}
}

// A skip covered by a sibling container (F83) is intentional and must not be
// counted into the missing total — otherwise a deduplicated shared folder would
// read as data loss on every container that references it.
func TestPartialReasonIgnoresCoveredSkips(t *testing.T) {
	man := &backup.Manifest{SkippedMounts: []backup.SkippedMount{
		{Destination: "/shared", Type: "bind", Bytes: 100 << 30, CoveredBy: "other-app"},
		{Destination: "/media", Type: "bind", Bytes: 2 << 30},
	}}
	r := partialReason(man)
	if strings.Contains(r, "/shared") {
		t.Errorf("a covered skip must not be reported as missing: %q", r)
	}
	if !strings.Contains(r, "/media") || !strings.Contains(r, "2.0 GB") {
		t.Errorf("only the uncovered skip and its size should appear: %q", r)
	}
}

// An unmeasured skip must say what is missing without inventing a figure, and a
// wholesale failure must stay legible rather than listing every path.
func TestPartialReasonUnmeasuredAndBounded(t *testing.T) {
	unmeasured := partialReason(&backup.Manifest{SkippedMounts: []backup.SkippedMount{
		{Destination: "/data", Type: "bind"},
	}})
	if strings.Contains(unmeasured, "0 B") {
		t.Errorf("an unmeasured skip must not claim a size: %q", unmeasured)
	}
	if !strings.Contains(unmeasured, "/data") {
		t.Errorf("it must still name the path: %q", unmeasured)
	}

	var many []backup.SkippedMount
	for i := 0; i < 8; i++ {
		many = append(many, backup.SkippedMount{Destination: fmt.Sprintf("/m%d", i), Bytes: 1 << 30})
	}
	r := partialReason(&backup.Manifest{SkippedMounts: many})
	if !strings.Contains(r, "and 5 more") {
		t.Errorf("the list must be bounded and the remainder counted: %q", r)
	}
	if !strings.Contains(r, "8.0 GB") {
		t.Errorf("the TOTAL must cover every skip, not just the named ones: %q", r)
	}
}

// F141: an archive holding only part of an atomic volume set does not restore at
// all — its restore is refused — so it belongs in the same band as one that
// cannot be decrypted, not in the PARTIAL band that describes a restore missing
// some data.
func TestGradeAtomicSetIncompleteIsF(t *testing.T) {
	half := &backup.Manifest{
		Image:         "jc21/nginx-proxy-manager:latest",
		AtomicVolumes: []string{"/data", "/etc/letsencrypt"},
		Volumes:       []backup.VolumeRef{{Destination: "/data", Type: "bind"}},
	}
	grade, reasons := gradeBackup(&store.Backup{Verified: "verified"}, half,
		[]backup.Location{local(), destOK()}, drillPass(), nil)
	if grade != "F" {
		t.Fatalf("an archive whose restore is refused must grade F, got %s (%v)", grade, reasons)
	}
	if !strings.Contains(strings.Join(reasons, " "), "only part of it") {
		t.Errorf("the reason must say what is wrong, got %v", reasons)
	}

	// The same backup with both members is graded on its own merits.
	whole := &backup.Manifest{
		Image:         "jc21/nginx-proxy-manager:latest",
		AtomicVolumes: []string{"/data", "/etc/letsencrypt"},
		Volumes: []backup.VolumeRef{
			{Destination: "/data", Type: "bind"}, {Destination: "/etc/letsencrypt", Type: "bind"},
		},
	}
	// Its archive is a credential store not sealed to an offline key, which caps
	// it at C — that is F122's rule, still applying.
	if grade, reasons := gradeBackup(&store.Backup{Verified: "verified"}, whole,
		[]backup.Location{local(), destOK()}, drillPass(), nil); grade == "F" {
		t.Errorf("a complete pair must not be graded F: %v", reasons)
	}
}

// F143: an archive whose certificates have ALL expired earns a line, not a lower
// grade — the backup is correct, and re-running it would not fix what aged.
func TestGradeExpiredCertificatesIsAReasonNotAGrade(t *testing.T) {
	man := &backup.Manifest{Certificates: []backup.CertRef{
		{Path: "/etc/letsencrypt/live/a/fullchain.pem", NotAfter: "2020-01-01T00:00:00Z"},
	}}
	grade, reasons := gradeBackup(&store.Backup{Verified: "verified"}, man,
		[]backup.Location{local(), destOK()}, drillPass(), nil)
	if grade != "A" {
		t.Errorf("an expired certificate must not change the grade, got %s", grade)
	}
	if !strings.Contains(strings.Join(reasons, " "), "expired") {
		t.Errorf("it must still be said, got %v", reasons)
	}

	// A live certificate alongside an old one is an ordinary deployment, and a
	// permanent warning on a perfectly good backup is how warnings stop working.
	mixed := &backup.Manifest{Certificates: []backup.CertRef{
		{Path: "/a", NotAfter: "2020-01-01T00:00:00Z"},
		{Path: "/b", NotAfter: "2999-01-01T00:00:00Z"},
	}}
	if _, reasons := gradeBackup(&store.Backup{Verified: "verified"}, mixed,
		[]backup.Location{local(), destOK()}, drillPass(), nil); len(reasons) != 0 {
		t.Errorf("one valid certificate is enough — must be silent, got %v", reasons)
	}

	// An unreadable date is not evidence of expiry.
	bad := &backup.Manifest{Certificates: []backup.CertRef{{Path: "/a", NotAfter: "soon"}}}
	if _, reasons := gradeBackup(&store.Backup{Verified: "verified"}, bad,
		[]backup.Location{local(), destOK()}, drillPass(), nil); len(reasons) != 0 {
		t.Errorf("an unparseable expiry must produce no claim, got %v", reasons)
	}
}

// F154: SQLite databases were found under the captured mounts and none could be
// snapshotted, so the archive holds raw copies of files that may have been
// mid-write. Caps at C, like the database-container equivalent, and the reason
// names the fix — because the commonest cause is a default nobody chose.
func TestGradeSQLiteFallback(t *testing.T) {
	man := &backup.Manifest{
		SQLiteFallback:      "the volume sidecar image has no sqlite3, so no consistent snapshot could be taken",
		SQLiteFallbackCount: 2,
	}
	grade, reasons := gradeBackup(&store.Backup{Verified: "verified"}, man,
		[]backup.Location{local(), destOK()}, drillPass(), nil)
	if grade != "C" {
		t.Fatalf("a raw-copied embedded database caps at C, got %s (%v)", grade, reasons)
	}
	joined := strings.Join(reasons, " ")
	for _, want := range []string{"2 embedded database", "sqlite3", "back up again"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the reason should contain %q; got %q", want, joined)
		}
	}

	// A backup whose snapshots worked says nothing about any of this.
	if grade, reasons := gradeBackup(&store.Backup{Verified: "verified"}, &backup.Manifest{},
		[]backup.Location{local(), destOK()}, drillPass(), nil); grade != "A" || len(reasons) != 0 {
		t.Errorf("a clean backup must stay A and silent: %s / %v", grade, reasons)
	}

	// Nor does one whose raw copy was taken while the app could not write:
	// frozen at one moment, crash-consistent, nothing in it can be torn.
	if grade, reasons := gradeBackup(&store.Backup{Verified: "verified"}, &backup.Manifest{SQLiteCrashConsistent: 1},
		[]backup.Location{local(), destOK()}, drillPass(), nil); grade != "A" || len(reasons) != 0 {
		t.Errorf("a crash-consistent copy must not be graded down: %s / %v", grade, reasons)
	}
}

// The list row shows it through the same chip as a raw-copied database
// container: from the operator's side both mean "a database in here was copied
// as a live file", which is the fact worth seeing at a glance.
func TestSummarizeListRowCarriesSQLiteFallback(t *testing.T) {
	b := &store.Backup{}
	summarizeListRow(b, &backup.Manifest{SQLiteFallback: "no sqlite3", SQLiteFallbackCount: 1}, nil)
	if b.Summary == nil || !b.Summary.DBFallback {
		t.Error("the list row must flag a raw-copied embedded database")
	}
}
