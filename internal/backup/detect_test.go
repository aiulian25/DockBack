package backup

import (
	"strings"
	"testing"
)

// TestDetectDBEngine locks in the Postgres-family detection, including the
// vector/extension builds that were previously misclassified as non-databases
// (which is what produced the inconsistent hot data-dir "DB backup" for Immich's
// tensorchord/pgvecto-rs image).
func TestDetectDBEngine(t *testing.T) {
	cases := map[string]string{
		"postgres:16":                                     "postgres",
		"postgis/postgis:16-3.4":                          "postgres",
		"pgvector/pgvector:pg16":                          "postgres",
		"tensorchord/pgvecto-rs:pg16-v0.2.0":              "postgres", // the Immich case
		"ghcr.io/immich-app/postgres:16-vectorchord0.4.2": "postgres",
		"tensorchord/vchord-postgres:pg17":                "postgres",
		"timescale/timescaledb:latest-pg16":               "postgres",
		"citusdata/citus:12":                              "postgres",
		"mariadb:11":                                      "mysql",
		"mysql:8":                                         "mysql",
		"percona:8":                                       "mysql",
		"mongo:7":                                         "mongodb",
		"redis:7":                                         "redis",
		"redis/redis-stack-server:latest":                 "redis",
		"valkey/valkey:8":                                 "redis",
		"eqalpha/keydb:latest":                            "redis",
		"ghcr.io/immich-app/immich-server:release":        "",
		"nginx:latest":                                    "",
	}
	for image, want := range cases {
		if got := detectDBEngine(image, nil); got != want {
			t.Errorf("detectDBEngine(%q) = %q, want %q", image, got, want)
		}
	}
}

// TestPGReadinessCmd locks the shape of the read-only probe command: a
// /bin/sh -c wrapper whose script runs psql with the container's own
// credentials and the exact wal_level/archive_mode/max_wal_senders query. It
// must contain NO write/DDL — this only ever reads settings (PLAN §9.7).
func TestPGReadinessCmd(t *testing.T) {
	cmd := PGReadinessCmd([]string{"POSTGRES_USER=app", "POSTGRES_PASSWORD=s3cr et"})
	if len(cmd) != 3 || cmd[0] != "/bin/sh" || cmd[1] != "-c" {
		t.Fatalf("PGReadinessCmd shape = %v, want [/bin/sh -c <script>]", cmd)
	}
	script := cmd[2]
	for _, want := range []string{"psql", "-tAX", "wal_level", "archive_mode", "max_wal_senders", "-U 'app'"} {
		if !strings.Contains(script, want) {
			t.Errorf("probe script missing %q: %s", want, script)
		}
	}
	// The password must NOT be in the script. This whole command is handed to
	// `sh -c`, so the script text is the exec's argv: it shows in `docker inspect`
	// of the exec and in the container's own process list. The script reads the
	// container's own variable instead, which is where the value came from.
	if strings.Contains(script, "s3cr et") {
		t.Errorf("the password is readable in the process list: %s", script)
	}
	if !strings.Contains(script, "POSTGRES_PASSWORD") {
		t.Errorf("the script must still reach the password by NAME: %s", script)
	}
	// Read-only: no mutation keywords.
	for _, bad := range []string{"insert ", "update ", "delete ", "alter ", "create ", "drop "} {
		if strings.Contains(strings.ToLower(script), bad) {
			t.Errorf("probe script must be read-only but contains %q: %s", bad, script)
		}
	}
}

// TestParsePGReadiness covers the a|b|c parser: only replica/logical + on is
// PITR-ready; everything else (incl. archiving off, garbled, or empty output)
// is full-dump-only, and an empty probe yields an empty WALLevel so callers can
// treat it as "unknown" rather than a false verdict.
func TestParsePGReadiness(t *testing.T) {
	cases := []struct {
		out         string
		ready       bool
		wal, arch   string
		wantUnknown bool
	}{
		{"replica|on|10", true, "replica", "on", false},
		{"logical|on|5", true, "logical", "on", false},
		{"replica|off|0", false, "replica", "off", false},
		{"minimal|off|0", false, "minimal", "off", false},
		{"  replica | on | 10  ", true, "replica", "on", false}, // trims whitespace/newline
		{"", false, "", "", true},
		{"garbage", false, "garbage", "", false},
	}
	for _, c := range cases {
		r := ParsePGReadiness(c.out)
		if r.Ready != c.ready {
			t.Errorf("ParsePGReadiness(%q).Ready = %v, want %v", c.out, r.Ready, c.ready)
		}
		if r.WALLevel != c.wal || r.ArchiveMode != c.arch {
			t.Errorf("ParsePGReadiness(%q) wal=%q arch=%q, want %q/%q", c.out, r.WALLevel, r.ArchiveMode, c.wal, c.arch)
		}
		if c.wantUnknown && r.WALLevel != "" {
			t.Errorf("empty probe should yield unknown (empty WALLevel), got %q", r.WALLevel)
		}
		// The ready verdict is about the SERVER's configuration, not about what
		// DockBack can restore: its base is a logical dump, which comes back as a
		// freshly initialised cluster that archived WAL cannot be replayed onto.
		// A backup tool must not overstate what it can bring back.
		if r.Ready && !strings.Contains(r.Detail, "server") {
			t.Errorf("the ready detail must say whose capability it is: %q", r.Detail)
		}
		if r.Ready && !strings.Contains(r.Detail, "last dump") {
			t.Errorf("the ready detail must say what DockBack itself restores to: %q", r.Detail)
		}
		if r.Ready && r.Detail == "" {
			t.Errorf("ready detail = %q", r.Detail)
		}
		if !r.Ready && !strings.Contains(r.Detail, "wal_level=replica") {
			t.Errorf("not-ready detail should carry guidance, got %q", r.Detail)
		}
	}
}
