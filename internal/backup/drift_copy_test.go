package backup

import (
	"strings"
	"testing"
)

// R3 §Issue 25: 53,159 files copied live, exactly one differed — nextcloud.log
// grew 3,573 bytes during the copy. Benign, and the point is that the tool
// should be able to say so rather than leave the operator to find out.
func TestDriftClassification(t *testing.T) {
	const nextcloud = "nextcloud:30-apache"
	before := VolIndex{Entries: []FileEntry{
		{Path: "var/www/html/data/nextcloud.log", Size: 102501127, MtimeUnix: 100},
		{Path: "var/www/html/data/admin/files/report.odt", Size: 40960, MtimeUnix: 100},
		{Path: "var/www/html/data/admin/files/photo.jpg", Size: 2048, MtimeUnix: 100},
	}}

	t.Run("a log that grew is expected, and names the file and the delta", func(t *testing.T) {
		after := VolIndex{Entries: []FileEntry{
			{Path: "var/www/html/data/nextcloud.log", Size: 102504700, MtimeUnix: 160},
			{Path: "var/www/html/data/admin/files/report.odt", Size: 40960, MtimeUnix: 100},
			{Path: "var/www/html/data/admin/files/photo.jpg", Size: 2048, MtimeUnix: 100},
		}}
		ordinary, unexpected := diffCopyDrift(before, after, driftExpectationFor(nextcloud))

		if len(unexpected) != 0 {
			t.Fatalf("an append-only log is not a reason to distrust a backup: %+v", unexpected)
		}
		if len(ordinary) != 1 {
			t.Fatalf("got %+v", ordinary)
		}
		if ordinary[0].SizeDelta != 3573 {
			t.Errorf("delta = %d, want R3's 3573", ordinary[0].SizeDelta)
		}
		described := ordinary[0].describe()
		if !strings.Contains(described, "nextcloud.log") || !strings.HasPrefix(strings.Split(described, " ")[1], "+") {
			t.Errorf("the report must name the file and the delta: %q", described)
		}
	})

	t.Run("a data file that changed is a warning", func(t *testing.T) {
		after := VolIndex{Entries: []FileEntry{
			{Path: "var/www/html/data/nextcloud.log", Size: 102501127, MtimeUnix: 100},
			{Path: "var/www/html/data/admin/files/report.odt", Size: 45000, MtimeUnix: 160},
			{Path: "var/www/html/data/admin/files/photo.jpg", Size: 2048, MtimeUnix: 100},
		}}
		ordinary, unexpected := diffCopyDrift(before, after, driftExpectationFor(nextcloud))
		if len(ordinary) != 0 {
			t.Errorf("a document is not a log: %+v", ordinary)
		}
		if len(unexpected) != 1 || !strings.Contains(unexpected[0].Path, "report.odt") {
			t.Fatalf("got %+v", unexpected)
		}
	})

	t.Run("a file REWRITTEN in place is caught, though its size did not move", func(t *testing.T) {
		// R3's warning: an append arrives short, but a rewrite mid-read can be
		// captured torn — and that one does not change the size.
		after := VolIndex{Entries: []FileEntry{
			{Path: "var/www/html/data/nextcloud.log", Size: 102501127, MtimeUnix: 100},
			{Path: "var/www/html/data/admin/files/report.odt", Size: 40960, MtimeUnix: 100},
			{Path: "var/www/html/data/admin/files/photo.jpg", Size: 2048, MtimeUnix: 160},
		}}
		_, unexpected := diffCopyDrift(before, after, driftExpectationFor(nextcloud))
		if len(unexpected) != 1 || !unexpected[0].Rewritten {
			t.Fatalf("a same-size rewrite is the dangerous case: %+v", unexpected)
		}
		if got := unexpected[0].describe(); !strings.Contains(got, "rewritten in place") {
			t.Errorf("describe = %q", got)
		}
	})

	t.Run("a quiet copy reports nothing", func(t *testing.T) {
		ordinary, unexpected := diffCopyDrift(before, before, driftExpectationFor(nextcloud))
		if len(ordinary) != 0 || len(unexpected) != 0 {
			t.Errorf("nothing moved: %v / %v", ordinary, unexpected)
		}
	})

	t.Run("appearing and vanishing files are not drift", func(t *testing.T) {
		// One that appeared is not something the archive got wrong, and one that
		// vanished is the capture's own accounting.
		after := VolIndex{Entries: []FileEntry{
			{Path: "var/www/html/data/new.odt", Size: 1, MtimeUnix: 160},
		}}
		ordinary, unexpected := diffCopyDrift(before, after, driftExpectationFor(nextcloud))
		if len(ordinary) != 0 || len(unexpected) != 0 {
			t.Errorf("got %v / %v", ordinary, unexpected)
		}
	})

	t.Run("an application's own declared logs, caches and scratch are expected", func(t *testing.T) {
		// Reuses what each profile already declares (F138 never-backup, F132
		// regenerable, #41 volatile) rather than inventing a second vocabulary.
		// Jellyfin declares /config/data/trickplay regenerable (F132) — 11 GB of
		// thumbnails it redraws by itself.
		jellyfin := driftExpectationFor("jellyfin/jellyfin:10.9")
		if !jellyfin.expected("config/data/trickplay/1/000.jpg") {
			t.Error("a directory the application rebuilds is expected to move")
		}
		if jellyfin.expected("config/data/library.db") {
			t.Error("the database is not scratch")
		}
		// Mealie declares a rotated-log GLOB and a scratch directory (F138).
		mealie := driftExpectationFor("mealie-recipes/mealie:v1.12")
		for _, rel := range []string{"app/data/mealie.log.3", "app/data/.temp/x"} {
			if !mealie.expected(rel) {
				t.Errorf("%s is declared never-backup and must be expected", rel)
			}
		}
		if mealie.expected("app/data/recipes/r.json") {
			t.Error("a recipe is not scratch")
		}
	})

	t.Run("generic expectations cover logs and process state anywhere", func(t *testing.T) {
		expect := driftExpectationFor("ghcr.io/nobody/app:1")
		for _, rel := range []string{"a/b/app.log", "a/b/app.log.1", "run/app.pid", "run/app.sock"} {
			if !expect.expected(rel) {
				t.Errorf("%s should be expected", rel)
			}
		}
		for _, rel := range []string{"data/photo.jpg", "data/catalog.db", "data/logbook.odt"} {
			if expect.expected(rel) {
				t.Errorf("%s must NOT be waved through", rel)
			}
		}
	})

	t.Run("the list is bounded like every other named-item report", func(t *testing.T) {
		var many []CopyDrift
		for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
			many = append(many, CopyDrift{Path: n, SizeDelta: 10})
		}
		got := describeDrift(many)
		if !strings.Contains(got, "and 2 more") {
			t.Errorf("describe = %q", got)
		}
	})
}
