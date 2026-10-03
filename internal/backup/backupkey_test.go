package backup

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"dockback/internal/store"
)

// filename = <absolute-date>_<time-to-second>_<id8>.dback, e.g.
// 2026-07-05_14-30-22_5677777e.dback
var keyFileRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2}_[0-9a-f]{8}\.dback$`)

func TestBackupKey(t *testing.T) {
	// A stack groups its containers under one folder:
	// <node>/<stack>/<container>/<timestamp>_<id>.dback
	k := backupKey("nas01", "nextcloud", "Nextcloud-DB", "5677777e9012")
	dir, file := filepath.Split(k)
	if want := filepath.Join("nas01", "nextcloud", "Nextcloud-DB") + string(filepath.Separator); dir != want {
		t.Fatalf("stack layout dir = %q, want %q", dir, want)
	}
	if !keyFileRe.MatchString(file) {
		t.Fatalf("filename %q is not <absolute-timestamp>_<id8>.dback", file)
	}

	// A standalone container (no compose stack) skips the stack level:
	// <node>/<container>/<timestamp>_<id>.dback
	k2 := backupKey("Acer", "", "npm", "deadbeef1234")
	if got, want := filepath.Dir(k2), filepath.Join("Acer", "npm"); got != want {
		t.Fatalf("standalone layout dir = %q, want %q", got, want)
	}

	// Hostile inputs are sanitized per SEGMENT so none can escape its directory:
	// no path element is ever exactly "." or ".." (a real traversal), and the
	// depth stays node/stack/container/file = 4 elements.
	k3 := backupKey("../../etc", "..", "../evil", "0011223344")
	segs := strings.Split(filepath.ToSlash(k3), "/")
	for _, s := range segs {
		if s == "." || s == ".." || s == "" {
			t.Fatalf("hostile path element %q survived sanitization in %q", s, k3)
		}
	}
	if len(segs) != 4 {
		t.Fatalf("want 4 path elements (node/stack/container/file), got %d: %q", len(segs), k3)
	}
}

// F77: CanonicalKey shares layoutKey with backupKey — one layout definition.
func TestCanonicalKey(t *testing.T) {
	man := &Manifest{NodeName: "nas01", Stack: "nextcloud"}
	b := &store.Backup{ID: "5677777e9012", TargetName: "Nextcloud-DB", Stack: "nextcloud",
		CreatedAt: 1751725822, StorageKey: "nas01-Nextcloud-DB-old-flat.dback"}

	// Stack case: same directory shape as a capture-time backupKey.
	got := CanonicalKey(b, man)
	wantDirOf := backupKey("nas01", "nextcloud", "Nextcloud-DB", "5677777e9012")
	if filepath.Dir(got) != filepath.Dir(wantDirOf) {
		t.Fatalf("canonical dir %q != capture dir %q", filepath.Dir(got), filepath.Dir(wantDirOf))
	}
	// The legacy basename is preserved (sanitized) — uniqueness and timestamp kept.
	if filepath.Base(got) != "nas01-Nextcloud-DB-old-flat.dback" {
		t.Fatalf("basename not preserved: %q", got)
	}

	// No-stack case.
	b2 := &store.Backup{ID: "deadbeef1234", TargetName: "npm", CreatedAt: 1751725822, StorageKey: "npm-old.dback"}
	got2 := CanonicalKey(b2, &Manifest{NodeName: "Acer"})
	if filepath.Dir(got2) != filepath.Dir(backupKey("Acer", "", "npm", "deadbeef1234")) {
		t.Fatalf("no-stack canonical dir wrong: %q", got2)
	}

	// A garbage basename falls back to CreatedAt + short id.
	b3 := &store.Backup{ID: "0011223344556677", TargetName: "app", CreatedAt: 1751725822, StorageKey: "weird/../thing"}
	got3 := CanonicalKey(b3, &Manifest{NodeName: "Node"})
	if filepath.Base(got3) != "2025-07-05_14-30-22_00112233.dback" {
		t.Fatalf("derived basename wrong: %q", got3)
	}

	// Already-canonical rows report canonical; legacy rows don't.
	b.StorageKey = got
	if !IsCanonicalKey(b, man) {
		t.Fatal("a row at its canonical key must be canonical")
	}
	b.StorageKey = "nas01-Nextcloud-DB-old-flat.dback"
	if IsCanonicalKey(b, man) {
		t.Fatal("a flat key must not be canonical")
	}
	if IsCanonicalKey(&store.Backup{}, &Manifest{}) {
		t.Fatal("an empty storage key must never be canonical")
	}
}
