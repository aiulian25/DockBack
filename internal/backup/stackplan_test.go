package backup

import (
	"encoding/json"
	"testing"

	"dockback/internal/store"
)

// mkPlanBackup fabricates a stack backup row for the F82 plan tests, with a
// controllable image (data-tier detection) and extra manifest fields.
func mkPlanBackup(id, project, service, image, group string, created int64, man Manifest) *store.Backup {
	man.Service, man.ConsistencyGroup, man.Image = service, group, image
	mb, _ := json.Marshal(man)
	return &store.Backup{
		ID: id, Stack: project, TargetName: service, Status: "success",
		Verified: "verified", CreatedAt: created, ManifestJSON: string(mb),
	}
}

// F82: the plan mirrors RestoreStack exactly — same selection, same order
// (data tier first), same group semantics and errors.
func TestPlanStackFrom(t *testing.T) {
	rows := []*store.Backup{
		mkPlanBackup("a2", "blog", "app", "app:2", "", 2000, Manifest{Incremental: true, ChainDepth: 2}),
		mkPlanBackup("d1", "blog", "db", "postgres:16", "", 1500, Manifest{}),
		mkPlanBackup("a1", "blog", "app", "app:1", "", 1000, Manifest{}),
		mkPlanBackup("x1", "other", "web", "web:1", "", 900, Manifest{}), // different stack
	}

	plan, _, err := planStackFrom(rows, "blog", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 2 {
		t.Fatalf("want 2 services, got %d: %+v", len(plan), plan)
	}

	// Regression (cross-host CommaFeed failure): a database service whose IMAGE
	// name doesn't look like a DB must still restore first when its backup
	// carries database dumps — the manifest is ground truth for the data tier.
	rows4 := []*store.Backup{
		mkPlanBackup("cf1", "feeds", "app", "example/feedapp:1", "", 200, Manifest{}),
		mkPlanBackup("cf2", "feeds", "db", "example/feed-db:1", "", 100, // no "postgres" in the name
			Manifest{Databases: []DBDump{{Service: "db", Engine: "postgres", Path: "db/db.dump"}}}),
	}
	plan4, _, err := planStackFrom(rows4, "feeds", "")
	if err != nil {
		t.Fatal(err)
	}
	if plan4[0].Service != "db" || !plan4[0].DataTier {
		t.Fatalf("a dump-carrying service must be data-tier and restore FIRST, got order %s → %s", plan4[0].Service, plan4[1].Service)
	}

	// The observed production case: the DB backup has NO dumps (captured while
	// stopped / no dump CLI → raw files) AND a non-DB image name — only the
	// SERVICE/TARGET NAME says database. It must still restore first.
	rows5 := []*store.Backup{
		mkPlanBackup("nf1", "feeds2", "commafeed", "example/feedapp:1", "", 200, Manifest{}),
		mkPlanBackup("nf2", "feeds2", "postgresql", "example/custom:1", "", 100, Manifest{}), // no dumps, generic image
	}
	plan5, _, err := planStackFrom(rows5, "feeds2", "")
	if err != nil {
		t.Fatal(err)
	}
	if plan5[0].Service != "postgresql" || !plan5[0].DataTier {
		t.Fatalf("a db-NAMED service must restore first even with no dumps and a generic image, got %s → %s", plan5[0].Service, plan5[1].Service)
	}
	// Data tier first — the same order RestoreStack executes.
	if plan[0].Service != "db" || !plan[0].DataTier || plan[0].Order != 1 {
		t.Fatalf("db must be first: %+v", plan[0])
	}
	if plan[1].Service != "app" || plan[1].Order != 2 {
		t.Fatalf("app must be second: %+v", plan[1])
	}
	// Newest per service, with its chips.
	if plan[1].BackupID != "a2" || !plan[1].Incremental {
		t.Fatalf("app must plan its newest (incremental) backup: %+v", plan[1])
	}
	if plan[0].Verified != "verified" || plan[0].CreatedAt != 1500 {
		t.Fatalf("db row fields not carried: %+v", plan[0])
	}

	// Partial (F83): only UNCOVERED skips flag the entry.
	rows2 := []*store.Backup{
		mkPlanBackup("c1", "blog2", "covered", "app:1", "", 100, Manifest{SkippedMounts: []SkippedMount{{Destination: "/u", CoveredBy: "srv"}}}),
		mkPlanBackup("u1", "blog2", "uncov", "app:1", "", 100, Manifest{SkippedMounts: []SkippedMount{{Destination: "/media"}}}),
	}
	plan2, _, err := planStackFrom(rows2, "blog2", "")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]StackPlanEntry{}
	for _, p := range plan2 {
		byName[p.Service] = p
	}
	if byName["covered"].Partial {
		t.Fatal("covered-only skips must not flag the plan entry as partial")
	}
	if !byName["uncov"].Partial {
		t.Fatal("an uncovered skip must flag the plan entry as partial")
	}

	// Group semantics: the same named error selectStackServicesFrom raises.
	rows3 := []*store.Backup{
		mkPlanBackup("g1", "blog3", "app", "app:1", "g9", 100, Manifest{}),
		mkPlanBackup("g2", "blog3", "db", "postgres:16", "", 90, Manifest{}),
	}
	if _, _, err := planStackFrom(rows3, "blog3", "g9"); err == nil || err.Error() != `no backup in group g9 for service "db"` {
		t.Fatalf("plan must surface the exact group selection error, got %v", err)
	}
}

// The observed production log, verbatim: the APP image's TAG names its DB
// backend ("athou/commafeed:latest-postgresql") — it must NOT rank as a
// database, and the dump-carrying postgres service must restore first.
func TestPlanOrderTagFalsePositive(t *testing.T) {
	rows := []*store.Backup{
		mkPlanBackup("t1", "cf", "commafeed", "athou/commafeed:latest-postgresql", "", 200, Manifest{}),
		mkPlanBackup("t2", "cf", "postgresql", "postgres:16", "", 100,
			Manifest{Databases: []DBDump{{Service: "postgresql", Engine: "postgres", Path: "db/postgresql.dump"}}}),
	}
	plan, _, err := planStackFrom(rows, "cf", "")
	if err != nil {
		t.Fatal(err)
	}
	if plan[0].Service != "postgresql" {
		t.Fatalf("dump-carrying DB must restore before the tag-false-positive app: got %s → %s", plan[0].Service, plan[1].Service)
	}
	if plan[1].DataTier {
		t.Fatalf("an app with a db-flavored TAG must not be data-tier: %+v", plan[1])
	}
}

// Image heuristic matches the REPOSITORY only — tags never classify.
func TestIsDataImageRepoOnly(t *testing.T) {
	yes := []string{"postgres:16", "postgres", "library/mysql:8", "registry.example:5000/custom-postgres:1", "ghcr.io/acme/immich-postgres:v1", "redis:7-alpine"}
	no := []string{"athou/commafeed:latest-postgresql", "example/app:mysql", "ghcr.io/acme/web:redis-flavor", "nginx:1"}
	for _, i := range yes {
		if !isDataImage(i) {
			t.Errorf("%q should match (repo names a DB engine)", i)
		}
	}
	for _, i := range no {
		if isDataImage(i) {
			t.Errorf("%q must NOT match (only the tag names a DB)", i)
		}
	}
}

// Name-based data-tier hint: db-ish names match, app names don't.
func TestIsDataServiceName(t *testing.T) {
	yes := []string{"postgresql", "postgres", "CommaFeed-DB", "app_db", "mariadb", "influxdb", "appdb", "redis-cache" /* prefix */, "db"}
	no := []string{"commafeed", "web", "nginx", "adguard", "traefik", "dashboard", ""}
	for _, n := range yes {
		if !isDataServiceName(n) {
			t.Errorf("%q should be detected as a data service", n)
		}
	}
	for _, n := range no {
		if isDataServiceName(n) {
			t.Errorf("%q must NOT be detected as a data service", n)
		}
	}
}

// F82/F81: ONE resolution shared by the plan preview and the real
// reconstruction — recorded dir, base-dir fallback, prefix remap.
func TestResolveStackDir(t *testing.T) {
	cases := []struct {
		name                                       string
		workingDir, svc, base, fromPath, toPath, w string
	}{
		{"recorded dir wins", "/opt/docker/blog", "blog", "/ignored", "", "", "/opt/docker/blog"},
		{"fallback base/name", "", "app1", "/opt/docker", "", "", "/opt/docker/app1"},
		{"fallback trims trailing slash", "", "app1", "/opt/docker/", "", "", "/opt/docker/app1"},
		{"nothing known", "", "app1", "", "", "", ""},
		{"no name no dir", "", "", "/opt/docker", "", "", ""},
		{"remap recorded dir", "/opt/docker/blog", "blog", "", "/opt/docker", "/opt/stacks", "/opt/stacks/blog"},
		{"remap fallback dir", "", "app1", "/opt/docker", "/opt/docker", "/opt/stacks", "/opt/stacks/app1"},
		{"remap non-matching untouched", "/srv/blog", "blog", "", "/opt/docker", "/opt/stacks", "/srv/blog"},
		{"remap sibling untouched", "/opt/dockerx/blog", "blog", "", "/opt/docker", "/opt/stacks", "/opt/dockerx/blog"},
		{"equal bases no-op", "/opt/docker/blog", "blog", "", "/opt/docker", "/opt/docker", "/opt/docker/blog"},
	}
	for _, c := range cases {
		if got := ResolveStackDir(c.workingDir, c.svc, c.base, c.fromPath, c.toPath); got != c.w {
			t.Errorf("%s: got %q, want %q", c.name, got, c.w)
		}
	}
}
