package backup

import (
	"strings"
	"testing"
)

// Plex: the SQLite raw-copy finding, identity-neutralised clones, and the
// curated exclusion set (F154–F156).

// F155 — the generated script's two safety-critical properties. Both were
// verified by RUNNING it against a realistic Preferences.xml under the sidecar's
// own shell; these pin the parts that a future edit could quietly break.
func TestRedactAttrScript(t *testing.T) {
	path := "/config/Library/Application Support/Plex Media Server/Preferences.xml"
	script := redactAttrScript(path, []string{"PlexOnlineToken", "PublishServerOnPlexOnlineKey"})

	// The verification pattern must be `Name="[^"]`, NOT `Name="..*"`. The
	// second is what this was first written as: `.` matches the closing quote, so
	// a correctly-emptied value still matched and every redacted clone was
	// refused. Caught by running it, not by reading it.
	if !strings.Contains(script, `PlexOnlineToken="[^"]`) {
		t.Error("the check must reject only a NON-empty value")
	}
	if strings.Contains(script, `="..*"`) {
		t.Error(`the "..*" pattern matches an emptied value and refuses a correct clone`)
	}
	// A file that is not there holds no identity, so there is nothing to refuse
	// over — otherwise a profile that matched an image slightly too broadly would
	// make clones of it unstartable.
	if !strings.Contains(script, noFileMarker) {
		t.Error("a missing file must be reported as nothing-to-do")
	}
	// Every occurrence of the path is single-quoted, so the spaces in Plex's own
	// directory names cannot split it into separate words.
	if strings.Contains(script, " "+path) {
		t.Error("the path must never appear unquoted")
	}
	// The value is emptied in place, and the temporary copies are removed.
	if !strings.Contains(script, `[^"]*"/\1"/g`) {
		t.Error("the value should be replaced with an empty one, keeping the attribute")
	}
	if !strings.Contains(script, "rm -f") {
		t.Error("the working copies must not be left behind")
	}
}

// The redaction is declared for Plex, required, and names both secrets.
func TestPlexCloneRedaction(t *testing.T) {
	reds := cloneRedactionsFor("plexinc/pms-docker:latest")
	if len(reds) != 1 {
		t.Fatalf("Plex must declare exactly one clone redaction, got %d", len(reds))
	}
	r := reds[0]
	if !r.Required {
		t.Error("a copy that could still publish must not be started, so this is required")
	}
	if !strings.HasSuffix(r.Path, "Preferences.xml") {
		t.Errorf("it must target Preferences.xml, got %q", r.Path)
	}
	want := map[string]bool{"PlexOnlineToken": true, "PublishServerOnPlexOnlineKey": true}
	for _, a := range r.Attrs {
		delete(want, a)
	}
	if len(want) != 0 {
		t.Errorf("both publishing secrets must be cleared; missing %v", want)
	}
	// The machine identifier is deliberately NOT cleared: it is what makes a real
	// restore the same server, and the token alone is what lets a copy publish.
	for _, a := range r.Attrs {
		if strings.Contains(a, "MachineIdentifier") {
			t.Error("the identity itself must be preserved — only the ability to publish it is removed")
		}
	}

	// Nothing else in the registry declares one, so no other clone is touched.
	if got := cloneRedactionsFor("nginx:1.27"); len(got) != 0 {
		t.Errorf("an ordinary image must declare no redaction, got %v", got)
	}
}

// The match must not claim images that merely contain the word.
func TestPlexProfileMatching(t *testing.T) {
	for _, img := range []string{"plexinc/pms-docker", "plexinc/pms-docker:1.43.3", "linuxserver/plex:latest"} {
		p := ProfileFor(img)
		if p == nil || p.Name != "Plex" {
			t.Errorf("%q should match the Plex profile, got %v", img, p)
		}
	}
	// A downgrade block and a required clone redaction attached to the wrong
	// application would be worse than having no profile at all.
	for _, img := range []string{"someone/complex-app:1", "myorg/plexamp:latest", "duplexer:2"} {
		if p := ProfileFor(img); p != nil && p.Name == "Plex" {
			t.Errorf("%q must NOT be treated as Plex", img)
		}
	}
}

// The curated keep/drop set: what is excluded, and — just as important — what is
// not. The paths all contain spaces, which is the detail an exclusion list gets
// wrong silently.
func TestPlexExclusions(t *testing.T) {
	p := ProfileFor("plexinc/pms-docker")
	if p == nil {
		t.Fatal("no profile")
	}
	excluded := map[string]bool{}
	for _, nb := range p.NeverBackup {
		excluded[nb.Path] = true
		if nb.Why == "" {
			t.Errorf("%q is excluded without saying why", nb.Path)
		}
		if !strings.HasPrefix(nb.Path, "/config/") {
			t.Errorf("%q should be inside the data directory", nb.Path)
		}
	}
	base := "/config/Library/Application Support/Plex Media Server/"
	for _, want := range []string{"Cache", "Logs", "Crash Reports", "Codecs", "Drivers", "Updates", "Diagnostics", "Plug-in Support/Caches"} {
		if !excluded[base+want] {
			t.Errorf("%q should be excluded", want)
		}
	}
	// The expensive, slow-to-rebuild trees must NOT be excluded — dropping them
	// would mean hours of re-matching after every restore.
	for _, keep := range []string{"Metadata", "Media", "Plug-in Support", "Plug-in Support/Databases", "Scanners"} {
		if excluded[base+keep] {
			t.Errorf("%q must be kept, not excluded", keep)
		}
	}
}

// F142 reused: an empty library after a restore is a fresh install, not a
// shortfall, and the message should say so.
func TestPlexCriticalTables(t *testing.T) {
	p := ProfileFor("plexinc/pms-docker")
	if len(p.CriticalTables) == 0 {
		t.Fatal("Plex must declare critical tables")
	}
	for _, c := range p.CriticalTables {
		if !strings.HasSuffix(c.DB, "com.plexapp.plugins.library.db") {
			t.Errorf("critical tables must name the library database, got %q", c.DB)
		}
		if c.Means == "" {
			t.Errorf("%q says nothing about what an empty table means", c.Table)
		}
	}
}

// The pre-restore panel has to say the things that decide whether a restored
// Plex is usable: where its media must be mounted, and what it does about
// identity.
func TestPlexPreconditions(t *testing.T) {
	man := &Manifest{
		Image: "plexinc/pms-docker:latest",
		Volumes: []VolumeRef{
			{Destination: "/config", Type: "bind", Source: "/home/user/docker/plex/database"},
		},
		SkippedMounts: []SkippedMount{
			{Destination: "/mnt", Type: "bind", Source: "/mnt", Reason: "large bind excluded by default"},
		},
	}
	pre := AppPreconditionsFor(man)
	if pre == nil {
		t.Fatal("Plex must have preconditions")
	}
	joined := strings.Join(pre.Notes, " ")
	// The media mount is the single biggest cross-machine constraint, and it is
	// the one that is NOT in the archive — so it has to come from the skipped
	// list, not the captured one.
	var sawMnt bool
	for _, d := range pre.DataPaths {
		if d == "/mnt" {
			sawMnt = true
		}
	}
	if !sawMnt {
		t.Errorf("the media mount must be listed as a path the target has to provide, got %v", pre.DataPaths)
	}
	// /config is local-disk-only, so it is listed as a requirement rather than as
	// a path to reproduce.
	for _, d := range pre.DataPaths {
		if d == "/config" {
			t.Error("/config is a local-disk requirement, not a mount to reproduce")
		}
	}
	for _, want := range []string{"software", "one server", "downloads"} {
		if !strings.Contains(strings.ToLower(joined), want) {
			t.Errorf("preconditions should mention %q; got %q", want, joined)
		}
	}
}

// The version guard: newer is a warning, older is refused, because a migrated
// database will not open on an older server.
func TestPlexVersionGuard(t *testing.T) {
	p := ProfileFor("plexinc/pms-docker")
	if v := AppVersionCompatibility(p, "1.43.3.10828", "1.40.0.1000"); !v.Blocking {
		t.Error("restoring a newer backup into an older server must be blocked")
	}
	if v := AppVersionCompatibility(p, "1.40.0.1000", "1.43.3.10828"); v.Blocking || v.Warning == "" {
		t.Error("restoring into a newer server must warn, not block")
	}
	// An unreadable version on either side yields no verdict at all.
	if v := AppVersionCompatibility(p, "", "1.43.3.10828"); v.Warning != "" || v.Blocking {
		t.Error("an unknown version must produce no verdict")
	}
}
