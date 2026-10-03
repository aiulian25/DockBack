package dockercli

import "testing"

// TestRunAsIDs covers the read that decides whether restored data gets re-owned
// (F117). Getting it wrong in either direction is bad: a false positive chowns
// data to numbers no image runs as, a false negative leaves the app unable to
// write its own database.
func TestRunAsIDs(t *testing.T) {
	uid, gid, key, ok := RunAsIDs([]string{"TZ=UTC", "PUID=1026", "PGID=100", "PASSWORD=hunter2"})
	if !ok || uid != 1026 || gid != 100 || key != "PUID/PGID" {
		t.Fatalf("got (%d,%d,%q,%v), want (1026,100,PUID/PGID,true)", uid, gid, key, ok)
	}
	// The other conventions, and the precedence between them.
	if u, g, k, ok := RunAsIDs([]string{"UID=1000", "GID=1000"}); !ok || u != 1000 || g != 1000 || k != "UID/GID" {
		t.Errorf("UID/GID form: got (%d,%d,%q,%v)", u, g, k, ok)
	}
	if u, g, k, ok := RunAsIDs([]string{"USER_ID=33", "GROUP_ID=33"}); !ok || u != 33 || g != 33 || k != "USER_ID/GROUP_ID" {
		t.Errorf("USER_ID/GROUP_ID form: got (%d,%d,%q,%v)", u, g, k, ok)
	}
	if _, _, k, _ := RunAsIDs([]string{"PUID=1000", "PGID=1000", "UID=999", "GID=999"}); k != "PUID/PGID" {
		t.Errorf("PUID/PGID is the most common convention and must win, got %q", k)
	}

	// Nothing to act on. Most images declare none of this, and a guess would be
	// worse than leaving ownership alone.
	for _, env := range [][]string{
		nil,
		{"TZ=UTC"},
		{"PUID=1000"}, // half a pair is not a pair
		{"PGID=1000"},
		{"PUID=abc", "PGID=1000"}, // non-numeric
		{"PUID=1000", "PGID=-5"},  // negative
		{"PUID=", "PGID="},
	} {
		if _, _, _, ok := RunAsIDs(env); ok {
			t.Errorf("%v must yield no ids", env)
		}
	}

	// A pair must come from ONE convention: mixing PUID with GROUP_ID would give
	// a plausible pair of numbers that no image actually runs as.
	if _, _, _, ok := RunAsIDs([]string{"PUID=1000", "GROUP_ID=100"}); ok {
		t.Error("ids must not be assembled across two different conventions")
	}
}

// TestChownVolumePathsRejectsNegative — the ids reach a command, so a negative
// value is refused before it can get there rather than after.
func TestChownVolumePathsRejectsNegative(t *testing.T) {
	if _, err := ChownVolumePaths(nil, nil, "c1", []string{"/config"}, -1, 0); err == nil {
		t.Error("a negative uid must be refused")
	}
	if _, err := ChownVolumePaths(nil, nil, "c1", []string{"/config"}, 0, -1); err == nil {
		t.Error("a negative gid must be refused")
	}
	// No paths is a no-op, not an error — a container with no recorded volumes
	// is ordinary.
	if n, err := ChownVolumePaths(nil, nil, "c1", nil, 1000, 1000); err != nil || n != 0 {
		t.Errorf("no paths must be a silent no-op, got (%d,%v)", n, err)
	}
}

func TestLastLine(t *testing.T) {
	cases := map[string]string{
		"a\nb\nc":       "c",
		"a\nb\n\n":      "b",
		"only":          "only",
		"":              "",
		"\n\n":          "",
		"x\n  spaced  ": "spaced",
	}
	for in, want := range cases {
		if got := lastLine(in); got != want {
			t.Errorf("lastLine(%q) = %q, want %q", in, got, want)
		}
	}
}

// F182 — the Synology-to-Linux move, which is the case this whole alignment
// exists for and the one it was missing.
//
// A Synology share is owned by an account like 1026:100, so the compose file
// carries those ids; moving to an ordinary Linux host changes them to 1000:1000.
// paperless-ngx spells that pair USERMAP_UID/USERMAP_GID, which was not in the
// list — so RunAsIDs returned ok=false, the alignment had nothing to align to,
// and the restored documents kept an owner that does not exist on the new
// machine. Silently: no warning, because "this image declares no ids" is a
// normal answer for most images.
func TestRunAsIDsUsermapPair(t *testing.T) {
	uid, gid, key, ok := RunAsIDs([]string{
		"PAPERLESS_REDIS=redis://broker:6379",
		"USERMAP_UID=1000",
		"USERMAP_GID=1000",
	})
	if !ok {
		t.Fatal("USERMAP_UID/USERMAP_GID is how paperless-ngx names this pair")
	}
	if uid != 1000 || gid != 1000 {
		t.Errorf("got %d:%d, want 1000:1000", uid, gid)
	}
	if key != "USERMAP_UID/USERMAP_GID" {
		t.Errorf("the log should name the pair it read: %q", key)
	}

	// The pair must come from ONE convention. A container setting PUID and
	// USERMAP_GID has not told us a uid/gid it runs as; it has told us half of
	// two different things, and chowning to that combination would produce an
	// owner no image ever runs as.
	if _, _, _, ok := RunAsIDs([]string{"PUID=1026", "USERMAP_GID=100"}); ok {
		t.Error("half of two conventions is not a pair")
	}

	// PUID/PGID still wins when both are present: it is the more common
	// convention, and an image honouring both honours that one.
	if _, _, key, _ := RunAsIDs([]string{"PUID=1000", "PGID=1000", "USERMAP_UID=1026", "USERMAP_GID=100"}); key != "PUID/PGID" {
		t.Errorf("PUID/PGID should stay first, got %q", key)
	}
}
