package backup

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// R3 §7.2's measurement, reproduced: 234 Nextcloud tables, 229 identical, and
// the 5 that differ are all runtime state. Reported flat that is "229/234 tables
// identical", which invites a support ticket. Reported split it is a claim the
// tool can stand behind.
func TestVolatileClassification(t *testing.T) {
	const nextcloud = "nextcloud:30-apache"
	r3Volatile := []string{"oc_appconfig", "oc_authtoken", "oc_job_runs", "oc_jobs", "oc_preferences"}

	// 234 tables: 229 durable and identical, plus the five that drift.
	captured, restored := map[string]string{}, map[string]string{}
	for i := 0; i < 229; i++ {
		name := fmt.Sprintf("nextcloud.public.oc_table%03d", i)
		captured[name], restored[name] = "same", "same"
	}
	for _, name := range r3Volatile {
		captured["nextcloud.public."+name] = "captured"
		restored["nextcloud.public."+name] = "rewritten-since"
	}

	t.Run("5 volatile diffs and 229 durable identical is a CLEAN verdict", func(t *testing.T) {
		volatile, classified := volatileTablesFor(nextcloud)
		if !classified {
			t.Fatal("the Nextcloud profile must declare its volatile tables, or nothing is armed")
		}
		split := CompareTableHashes(captured, restored).Split(volatile)

		if !split.Clean() {
			t.Fatalf("all durable data came back; this must not read as a failure: %+v", split)
		}
		if split.DurableTotal != 229 || split.DurableIdentical != 229 {
			t.Errorf("durable = %d/%d, want 229/229", split.DurableIdentical, split.DurableTotal)
		}
		if len(split.VolatileDifferent) != 5 {
			t.Errorf("volatile differing = %d, want 5", len(split.VolatileDifferent))
		}
		// The register's exact wording bar.
		if got := split.Headline(); got != "229/229 durable identical; 5 volatile differ (expected)" {
			t.Errorf("headline = %q", got)
		}
	})

	t.Run("one durable difference turns it red", func(t *testing.T) {
		// oc_filecache is Nextcloud's index of every file — R3 calls it the
		// strongest single signal there is. A difference there is not runtime
		// state, and must not be absorbed by the five that are.
		red := map[string]string{}
		for k, v := range restored {
			red[k] = v
		}
		captured["nextcloud.public.oc_filecache"] = "aaa"
		red["nextcloud.public.oc_filecache"] = "DIFFERENT"

		volatile, _ := volatileTablesFor(nextcloud)
		split := CompareTableHashes(captured, red).Split(volatile)

		if split.Clean() {
			t.Fatal("a durable difference must not be absorbed by the volatile ones")
		}
		if len(split.DurableDifferent) != 1 || split.DurableDifferent[0] != "nextcloud.public.oc_filecache" {
			t.Errorf("durable differences = %v", split.DurableDifferent)
		}
		if !strings.Contains(split.Headline(), "5 volatile differ (expected)") {
			t.Errorf("the volatile count is still reported alongside: %q", split.Headline())
		}
	})

	t.Run("an unclassified application is unchanged from before", func(t *testing.T) {
		volatile, classified := volatileTablesFor("ghcr.io/nobody/app:1")
		if classified || volatile != nil {
			t.Error("nothing may be classified by guesswork")
		}
		split := CompareTableHashes(captured, restored).Split(volatile)
		if split.Clean() {
			t.Error("undeclared, the five differences are still reported")
		}
	})

	t.Run("volatile files: the .immich markers R5 §9.3 measured", func(t *testing.T) {
		globs := volatileFileGlobs("ghcr.io/immich-app/immich-server:v1.119")
		if !slices.Contains(globs, ".immich") {
			t.Fatalf("the Immich profile must declare its marker: %v", globs)
		}
		idx := VolIndex{Entries: []FileEntry{
			{Path: "usr/src/app/upload/library/admin/2024/photo.jpg"},
			{Path: "usr/src/app/upload/library/.immich"},
			{Path: "usr/src/app/upload/thumbs/.immich"},
			{Path: "usr/src/app/upload/app.lock"},
			{Path: "usr/src/app/upload/server.pid"},
			{Path: "usr/src/app/upload/notes.immich.txt"},
		}}
		if marked := markVolatileFiles(&idx, globs); marked != 4 {
			t.Fatalf("marked %d, want the two markers plus the lock and pid", marked)
		}
		for _, e := range idx.Entries {
			want := strings.HasSuffix(e.Path, "/.immich") || strings.HasSuffix(e.Path, ".lock") || strings.HasSuffix(e.Path, ".pid")
			if e.Volatile != want {
				t.Errorf("%s: volatile = %v, want %v", e.Path, e.Volatile, want)
			}
		}
	})

	t.Run("file matching is by name anywhere, and never by accident", func(t *testing.T) {
		globs := []string{".immich", "*.lock", "cache/*.tmp"}
		for _, rel := range []string{"a/b/.immich", ".immich", "x/app.lock", "cache/one.tmp"} {
			if !matchesVolatileFile(rel, globs) {
				t.Errorf("%s should match", rel)
			}
		}
		for _, rel := range []string{
			"photo.jpg",
			"notes.immich.txt",   // contains the name, is not the name
			"immich",             // missing the dot
			"deep/cache/one.tmp", // a rooted pattern is rooted
			"",
		} {
			if matchesVolatileFile(rel, globs) {
				t.Errorf("%s must NOT match — a wrong volatile label silently unguards real data", rel)
			}
		}
	})

	t.Run("the generic set stays short and deliberate", func(t *testing.T) {
		// R5's takeaway also lists .nomedia, which is a marker a USER places and
		// an application reads — its content does not drift, so labelling it
		// volatile would unguard a file that should be stable.
		if len(genericVolatileFiles) != 2 {
			t.Errorf("generic set = %v; every addition unguards data everywhere", genericVolatileFiles)
		}
		if matchesVolatileFile("media/.nomedia", genericVolatileFiles) {
			t.Error(".nomedia is not app-written state")
		}
	})
}

// R3's Nextcloud keeps its database in a SEPARATE mariadb container. Keyed on
// that container's own image the classification would never apply, and the
// 229-of-234 verdict the whole step exists for would be unreachable.
func TestVolatileTablesReachTheDatabaseContainer(t *testing.T) {
	t.Run("the app's own image classifies directly", func(t *testing.T) {
		volatile, ok := volatileTablesFor("nextcloud:30-apache")
		if !ok || !volatile["oc_appconfig"] {
			t.Errorf("got %v/%v", volatile, ok)
		}
	})

	t.Run("a database image declares nothing on its own", func(t *testing.T) {
		if _, ok := volatileTablesFor("mariadb:11.4-noble"); ok {
			t.Error("a database image knows nothing about the application's tables")
		}
	})

	t.Run("merging declaring members classifies a stack running two applications", func(t *testing.T) {
		merged, ok := volatileTableSet(ProfileFor("nextcloud:30-apache"), ProfileFor("mariadb:11.4-noble"), nil)
		if !ok || !merged["oc_jobs"] {
			t.Errorf("merged = %v", merged)
		}
		// Matching stays on exact names, so a merged list cannot label a table it
		// was not written for.
		if merged["documents_document"] {
			t.Error("a merged list must not widen into names nobody declared")
		}
		if _, ok := volatileTableSet(nil, ProfileFor("mariadb:11.4-noble")); ok {
			t.Error("members that declare nothing produce no classification")
		}
	})
}
