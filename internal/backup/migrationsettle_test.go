package backup

import (
	"testing"

	"dockback/internal/store"
)

// TestManifestImageOf covers the profile lookup's input. A row with no readable
// manifest must yield "" — which means "no profile applies", never a panic and
// never a wrong match.
func TestManifestImageOf(t *testing.T) {
	cases := []struct {
		name string
		b    *store.Backup
		want string
	}{
		{"normal", &store.Backup{ManifestJSON: `{"image":"solidnerd/bookstack:latest"}`}, "solidnerd/bookstack:latest"},
		{"no image key", &store.Backup{ManifestJSON: `{"version":1}`}, ""},
		{"empty manifest", &store.Backup{ManifestJSON: ""}, ""},
		{"corrupt json", &store.Backup{ManifestJSON: `{not json`}, ""},
		{"nil row", nil, ""},
	}
	for _, c := range cases {
		if got := manifestImageOf(c.b); got != c.want {
			t.Errorf("%s: manifestImageOf = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestMigrationSettleApplies documents exactly which containers the settle window
// applies to, because the cost of getting this wrong runs both ways: applying it
// too widely adds dead time to every restore, and applying it too narrowly leaves
// the silent failure it exists to catch.
//
// It applies only to an app that BOTH migrates its schema at startup AND ships no
// healthcheck — the combination where WaitForHealthy returns "running" on its
// first inspect, seconds before the migration has finished.
func TestMigrationSettleApplies(t *testing.T) {
	applies := func(image string) bool {
		p := ProfileFor(image)
		return p != nil && p.OneWayMigration != "" && p.NoHealthcheck
	}

	if !applies("solidnerd/bookstack:latest") {
		t.Error("BookStack migrates at startup and has no healthcheck — the settle window must apply")
	}
	// Wiki.js migrates too, but its images DO ship a healthcheck, so the ordinary
	// gate is already a real verdict and must not be padded.
	if applies("ghcr.io/linuxserver/wikijs:latest") {
		t.Error("Wiki.js has a healthcheck — the ordinary gate is meaningful, the settle window must NOT apply")
	}
	// Audiobookshelf migrates at startup but is not marked healthcheck-less.
	if applies("ghcr.io/advplyr/audiobookshelf:latest") {
		t.Error("Audiobookshelf must not pick up the settle window without declaring NoHealthcheck")
	}
	// The overwhelming majority: no profile at all, restore path unchanged.
	for _, img := range []string{"nginx:alpine", "postgres:16", "mariadb:11.4-noble", ""} {
		if applies(img) {
			t.Errorf("%q must not trigger the settle window", img)
		}
	}
}
