package backup

import (
	"strings"
	"testing"
)

func immichDump(exts ...string) *Manifest {
	return &Manifest{Databases: []DBDump{{
		Service: "immich-db", Engine: "postgres", Extensions: exts,
	}}}
}

// TestVectorVersionBlocksIndexFormatChange (F131). The FAMILY check already
// catches pgvecto.rs -> VectorChord. This catches the quieter one: the same
// family at a different minor version, where the index format moved underneath a
// dump that restores without a single error and leaves search returning wrong
// results.
func TestVectorVersionBlocksIndexFormatChange(t *testing.T) {
	man := immichDump("vchord 0.4.2", "vector 0.8.1")
	target := []string{"vchord 0.5.0", "vector 0.8.1"}

	warnings, blocking := RestoreCompatibility(man, "ghcr.io/immich-app/postgres:16-vectorchord0.5.0", "", target)
	if !blocking {
		t.Fatal("a minor vector-extension version change must BLOCK — the index format differs")
	}
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{"0.4.2", "0.5.0", "index format"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the warning must name %q so the operator can act on it:\n%s", want, joined)
		}
	}
	// A MAJOR change likewise.
	if _, b := RestoreCompatibility(man, "img", "", []string{"vchord 1.0.0"}); !b {
		t.Error("a major version change must BLOCK")
	}
}

// TestVectorVersionPatchWarnsOnly: 0.4.2 -> 0.4.3 is a fix release. Blocking on
// it would refuse valid restores far more often than it caught a real problem.
func TestVectorVersionPatchWarnsOnly(t *testing.T) {
	man := immichDump("vchord 0.4.2")
	warnings, blocking := RestoreCompatibility(man, "img", "", []string{"vchord 0.4.3"})
	if blocking {
		t.Fatal("a patch-level difference must not block")
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "patch-level") {
		t.Errorf("expected a single patch-level note, got %v", warnings)
	}
}

// TestVectorVersionSilentWhenMatched — the ordinary restore, and the one that
// matters most: restore-by-digest reproduces the same image, so this path must
// add nothing at all.
func TestVectorVersionSilentWhenMatched(t *testing.T) {
	man := immichDump("vchord 0.4.2", "vectors 0.2.0", "vector 0.8.1")
	same := []string{"vchord 0.4.2", "vectors 0.2.0", "vector 0.8.1", "plpgsql 1.0"}
	warnings, blocking := RestoreCompatibility(man, "ghcr.io/immich-app/postgres:16-vectorchord0.4.2-pgvectors0.2.0", "", same)
	if blocking || len(warnings) != 0 {
		t.Fatalf("identical extensions must be silent, got blocking=%v %v", blocking, warnings)
	}
}

// TestVectorVersionFailOpen keeps the discipline: an unreadable version on
// either side, or an unprobed target, produces no verdict rather than a guess.
func TestVectorVersionFailOpen(t *testing.T) {
	// Target not probed at all (nil) — falls back to the image-name family guess
	// and must not invent a version comparison.
	if _, b := RestoreCompatibility(immichDump("vchord 0.4.2"), "ghcr.io/immich-app/postgres:16-vectorchord0.4.2", "", nil); b {
		t.Error("an unprobed target must not produce a version block")
	}
	// Version missing from the recorded list.
	if _, b := RestoreCompatibility(immichDump("vchord"), "img", "", []string{"vchord 0.5.0"}); b {
		t.Error("an unreadable dump version must not block")
	}
	if _, b := RestoreCompatibility(immichDump("vchord 0.4.2"), "img", "", []string{"vchord"}); b {
		t.Error("an unreadable target version must not block")
	}
	// A dump with no vector extension at all is untouched by any of this.
	if w, b := RestoreCompatibility(immichDump("plpgsql 1.0"), "postgres:16", "", []string{"plpgsql 1.0"}); b || len(w) != 0 {
		t.Errorf("a non-vector dump must be silent, got %v %v", b, w)
	}
}

func TestExtVersionAndSameMajorMinor(t *testing.T) {
	exts := []string{"vchord 0.4.2", "vector 0.8.1", "plpgsql 1.0"}
	if got := extVersion(exts, "vchord"); got != "0.4.2" {
		t.Errorf("got %q", got)
	}
	if got := extVersion(exts, "missing"); got != "" {
		t.Errorf("an absent extension must read empty, got %q", got)
	}
	if got := extVersion(exts, ""); got != "" {
		t.Errorf("an empty name must read empty, got %q", got)
	}
	cases := []struct {
		a, b string
		same bool
	}{
		{"0.4.2", "0.4.3", true},
		{"0.4.2", "0.5.0", false},
		{"0.4.2", "1.4.2", false},
		{"0.4", "0.4.0", true},
		{"0.4.2", "0.4.2", true},
		{"x", "0.4.2", false}, // unreadable compares as NOT the same → cautious branch
	}
	for _, c := range cases {
		if got := sameMajorMinor(c.a, c.b); got != c.same {
			t.Errorf("sameMajorMinor(%q,%q) = %v, want %v", c.a, c.b, got, c.same)
		}
	}
}

// TestImmichProfile: the app images get a profile, the DATABASE image must not —
// it is already detected as a database engine, and attaching an application
// profile to it would be a category error.
func TestImmichProfile(t *testing.T) {
	for _, img := range []string{
		"ghcr.io/immich-app/immich-server:v3.0.1",
		"ghcr.io/immich-app/immich-machine-learning:v3.0.1",
	} {
		p := ProfileFor(img)
		if p == nil || p.Name != "Immich" {
			t.Fatalf("%q should match the Immich profile, got %+v", img, p)
		}
		if p.OneWayMigration == "" {
			t.Error("Immich migrates its schema on start — a downgrade must be blocked")
		}
	}
	dbImg := "ghcr.io/immich-app/postgres:16-vectorchord0.4.2-pgvectors0.2.0"
	if p := ProfileFor(dbImg); p != nil {
		t.Errorf("the Immich DATABASE image must not get an application profile, got %+v", p)
	}
	// It must still be recognised as a Postgres engine, which is what makes the
	// dump and the extension guard work at all.
	if detectDBEngine(dbImg, nil) != "postgres" {
		t.Error("the Immich database image must be detected as Postgres")
	}
}
