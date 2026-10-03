package backup

import (
	"strings"
	"testing"
)

// TestRestoreCompatibility locks in the extension/engine gate (F10), notably the
// Immich pgvecto.rs -> VectorChord class of break.
func TestRestoreCompatibility(t *testing.T) {
	pgvecto := &Manifest{Databases: []DBDump{{Service: "db", Engine: "postgres", Extensions: []string{"vectors 0.2.0"}}}}
	pgvector := &Manifest{Databases: []DBDump{{Service: "db", Engine: "postgres", Extensions: []string{"vector 0.7.0"}}}}
	plainPG := &Manifest{Databases: []DBDump{{Service: "db", Engine: "postgres"}}}

	cases := []struct {
		name        string
		man         *Manifest
		targetImage string
		wantBlock   bool
	}{
		// The headline break: a pgvecto.rs dump into a VectorChord image.
		{"pgvecto -> vchord blocks", pgvecto, "tensorchord/vchord-postgres:pg17", true},
		// Same family is fine.
		{"pgvecto -> pgvecto ok", pgvecto, "tensorchord/pgvecto-rs:pg16-v0.2.0", false},
		// A vector dump into a plain Postgres image (no vector ext) must block —
		// this is the acceptance-criteria scenario.
		{"pgvecto -> plain postgres blocks", pgvecto, "postgres:16", true},
		{"pgvector -> plain postgres blocks", pgvector, "postgres:16", true},
		{"pgvector -> pgvector ok", pgvector, "pgvector/pgvector:pg16", false},
		// No extensions never blocks, regardless of target.
		{"no extensions never blocks", plainPG, "tensorchord/vchord-postgres:pg17", false},
		{"plain -> plain ok", plainPG, "postgres:16", false},
		// Outright engine-family mismatch blocks.
		{"postgres dump into mysql image blocks", plainPG, "mariadb:11", true},
		// Nil manifest is safe.
		{"nil manifest", nil, "postgres:16", false},
	}
	for _, c := range cases {
		// No db.Version set on these manifests (F36 inert), and targetExts=nil so the
		// vector check uses the image-name guess (F41 fallback == existing behavior).
		_, blocking := RestoreCompatibility(c.man, c.targetImage, "", nil)
		if blocking != c.wantBlock {
			t.Errorf("%s: blocking = %v, want %v", c.name, blocking, c.wantBlock)
		}
	}

	// A blocking result must carry an actionable, specific warning.
	if w, _ := RestoreCompatibility(pgvecto, "postgres:16", "", nil); len(w) == 0 {
		t.Fatal("expected a warning message for a blocking mismatch")
	}
}

// TestRestoreCompatibilityProbedExtensions locks in the F41 improvement: when the
// target's REAL extensions are probed, the vector-family check compares
// measured-to-measured — so a custom-named image is judged by what it actually
// provides, not its name.
func TestRestoreCompatibilityProbedExtensions(t *testing.T) {
	pgvecto := &Manifest{Databases: []DBDump{{Service: "db", Engine: "postgres", Extensions: []string{"plpgsql 1.0", "vectors 0.2.0"}}}}

	// A custom-named image that GENUINELY has pgvecto.rs → must NOT block (the case
	// wrongly blocked today, when the family is guessed from the name).
	if _, blocking := RestoreCompatibility(pgvecto, "myreg/custom-pg:1", "", []string{"plpgsql 1.0", "vectors 0.2.0"}); blocking {
		t.Error("a target that really provides the dump's extension must NOT block, regardless of image name")
	}

	// A custom-named image that really has VectorChord → must block (a break the
	// name guess would MISS if the name looked pgvecto-ish).
	if _, blocking := RestoreCompatibility(pgvecto, "myreg/pgvecto-lookalike:1", "", []string{"plpgsql 1.0", "vchord 0.4.2"}); !blocking {
		t.Error("a target lacking the dump's extension must block, regardless of image name")
	}

	// Probed but no vector extension at all (plain Postgres) → block, and the warning
	// must NOT carry the "inferred from the image name" suffix (we measured it).
	w, blocking := RestoreCompatibility(pgvecto, "myreg/custom-pg:1", "", []string{"plpgsql 1.0"})
	if !blocking {
		t.Error("a probed plain-Postgres target must block a pgvecto dump")
	}
	for _, m := range w {
		if strings.Contains(m, "inferred from the image name") {
			t.Error("a probed decision must not claim it was inferred from the name")
		}
	}

	// targetExts=nil (couldn't probe) → falls back to the name guess AND says so.
	w, _ = RestoreCompatibility(pgvecto, "postgres:16", "", nil)
	found := false
	for _, m := range w {
		if strings.Contains(m, "inferred from the image name") {
			found = true
		}
	}
	if !found {
		t.Error("an unprobed decision must disclose it was inferred from the image name")
	}
}

// TestRestoreCompatibilityVersion locks in the F36 engine version-downgrade guard:
// a newer dump into an older engine BLOCKS; an upgrade only warns; equal or
// unparseable passes.
func TestRestoreCompatibilityVersion(t *testing.T) {
	pg := func(ver string) *Manifest {
		return &Manifest{Databases: []DBDump{{Service: "db", Engine: "postgres", Version: ver}}}
	}
	cases := []struct {
		name          string
		man           *Manifest
		targetImage   string
		targetVersion string
		wantBlock     bool
		wantWarn      bool
	}{
		// The headline: PG16 dump into a PG15 target → DOWNGRADE, blocks.
		{"pg16 -> pg15 blocks", pg("psql (PostgreSQL) 16.2"), "postgres:15", "psql (PostgreSQL) 15.6", true, true},
		// Upgrade: PG15 dump into PG16 → warns, does not block.
		{"pg15 -> pg16 warns", pg("psql (PostgreSQL) 15.6"), "postgres:16", "psql (PostgreSQL) 16.2", false, true},
		// Equal major → silent pass.
		{"pg16 -> pg16 ok", pg("psql (PostgreSQL) 16.2"), "postgres:16", "psql (PostgreSQL) 16.4", false, false},
		// Unparseable dump version → skip (never block).
		{"unparseable dump skips", pg("unknown"), "postgres:15", "psql (PostgreSQL) 15.6", false, false},
		// Unparseable target version → skip.
		{"unparseable target skips", pg("psql (PostgreSQL) 16.2"), "postgres:15", "", false, false},
		// MySQL 8 dump into MySQL 5.7 → downgrade blocks.
		{"mysql8 -> mysql5 blocks",
			&Manifest{Databases: []DBDump{{Service: "db", Engine: "mysql", Version: "mysql  Ver 8.0.35 for Linux"}}},
			"mysql:5.7", "mysql  Ver 5.7.44 for Linux", true, true},
		// MongoDB 7 dump into 6 → blocks.
		{"mongo7 -> mongo6 blocks",
			&Manifest{Databases: []DBDump{{Service: "db", Engine: "mongodb", Version: "db version v7.0.5"}}},
			"mongo:6", "db version v6.0.14", true, true},
	}
	for _, c := range cases {
		w, blocking := RestoreCompatibility(c.man, c.targetImage, c.targetVersion, nil)
		if blocking != c.wantBlock {
			t.Errorf("%s: blocking = %v, want %v", c.name, blocking, c.wantBlock)
		}
		if (len(w) > 0) != c.wantWarn {
			t.Errorf("%s: warnings=%v, want warn=%v", c.name, w, c.wantWarn)
		}
	}
}

// TestParseMajor covers the real version-string shapes each engine produces.
func TestParseMajor(t *testing.T) {
	ok := map[string]int{
		"psql (PostgreSQL) 16.2":                       16,
		"postgres (PostgreSQL) 15.6 (Debian 15.6-1)":   15,
		"mysql  Ver 8.0.35 for Linux on x86_64":        8,
		"mysql  Ver 5.7.44":                            5,
		"mariadb  Ver 15.1 Distrib 10.11.6-MariaDB, …": 10, // real server version follows Distrib
		"mariadb  Ver 15.1 Distrib 11.4.2-MariaDB":     11,
		"db version v7.0.5":                            7,
		"Redis server v=7.2.4 sha=00000000":            7,
	}
	for in, want := range ok {
		if got, k := parseMajor(in); !k || got != want {
			t.Errorf("parseMajor(%q) = %d,%v; want %d,true", in, got, k, want)
		}
	}
	for _, bad := range []string{"", "   ", "unknown", "no digits here"} {
		if _, k := parseMajor(bad); k {
			t.Errorf("parseMajor(%q) should be unparseable", bad)
		}
	}
}
