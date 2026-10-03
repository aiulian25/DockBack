package backup

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

const gib = int64(1) << 30

// testStoreT opens a throwaway store for the selection tests.
func testStoreT(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// F12: the large-bind default must follow the effective threshold — a 3 GiB bind
// is auto-selected under a 10 GiB cutoff and skipped-by-default under a 1 GiB one,
// with an honest human-readable reason on the skip.
func TestDefaultSelectedHonorsThreshold(t *testing.T) {
	bind := MountInfo{Destination: "/data", Type: "bind", SizeBytes: 3 * gib, SizeKnown: true}

	sel, reason := defaultSelected(bind, false, nil, nil, 10*gib)
	if !sel {
		t.Fatalf("3 GiB bind should be selected by default under a 10 GiB threshold")
	}
	if reason != "" {
		t.Errorf("selected bind should carry no skip reason, got %q", reason)
	}

	sel, reason = defaultSelected(bind, false, nil, nil, 1*gib)
	if sel {
		t.Fatalf("3 GiB bind should be skipped by default under a 1 GiB threshold")
	}
	if !strings.Contains(reason, "large bind (3.0 GB)") {
		t.Errorf("skip reason should name the measured size, got %q", reason)
	}

	// A named volume is always on regardless of threshold.
	vol := MountInfo{Destination: "/v", Type: "volume", SizeBytes: 100 * gib, SizeKnown: true}
	if on, _ := defaultSelected(vol, false, nil, nil, 1*gib); !on {
		t.Errorf("named volumes must be selected by default irrespective of the bind threshold")
	}

	// A remembered selection always wins over the size default.
	if on, _ := defaultSelected(bind, true, map[string]bool{"/data": true}, nil, 1*gib); !on {
		t.Errorf("a remembered selection must override the size-based default")
	}
}

// F12: the effective threshold resolves per-container override > global setting >
// shipped default, clamps out-of-range values, and clears cleanly.
func TestBindSkipThresholdResolution(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := &Engine{Store: st, Log: func(string, string, string) {}}

	// Default when nothing is set.
	if got := e.BindSkipGiBGlobal(); got != defaultBindSkipGiB {
		t.Errorf("global default = %d, want %d", got, defaultBindSkipGiB)
	}
	if got := e.bindSkipThreshold("n1", "app"); got != defaultBindSkipGiB*gib {
		t.Errorf("effective threshold bytes = %d, want %d", got, defaultBindSkipGiB*gib)
	}

	// Global setting takes effect.
	_ = st.SetSetting(bindSkipGiBKey, "20")
	if got := e.bindSkipThreshold("n1", "app"); got != 20*gib {
		t.Errorf("with global=20, effective = %d, want %d", got, 20*gib)
	}

	// Per-container override wins over the global value.
	if err := e.SetBindSkipGiBOverride("n1", "app", 2); err != nil {
		t.Fatal(err)
	}
	if n, ok := e.BindSkipGiBOverride("n1", "app"); !ok || n != 2 {
		t.Errorf("override = (%d,%v), want (2,true)", n, ok)
	}
	if got := e.bindSkipThreshold("n1", "app"); got != 2*gib {
		t.Errorf("override should win: effective = %d, want %d", got, 2*gib)
	}
	// A different container still sees the global value, not the override.
	if got := e.bindSkipThreshold("n1", "other"); got != 20*gib {
		t.Errorf("unrelated container should use global: %d, want %d", got, 20*gib)
	}

	// An out-of-range override is clamped; a non-positive value clears it.
	_ = e.SetBindSkipGiBOverride("n1", "app", 999999)
	if n, _ := e.BindSkipGiBOverride("n1", "app"); n != maxBindSkipGiB {
		t.Errorf("override should clamp to %d, got %d", maxBindSkipGiB, n)
	}
	_ = e.SetBindSkipGiBOverride("n1", "app", 0)
	if _, ok := e.BindSkipGiBOverride("n1", "app"); ok {
		t.Errorf("override should be cleared by a non-positive value")
	}
	if got := e.bindSkipThreshold("n1", "app"); got != 20*gib {
		t.Errorf("after clearing override, effective falls back to global: %d, want %d", got, 20*gib)
	}
}

// F3: a selected bind mount whose files the reader couldn't access (UID/GID
// mismatch / NFS root_squash) must be recorded as a PARTIAL skipped-mount, not
// silently dropped — so the manifest, Backups "Partial" chip, and DR runbook all
// reflect the gap.
func TestUnreadableSkippedMounts(t *testing.T) {
	got := unreadableSkippedMounts(
		[]string{"/data", "/config"},
		map[string]string{"/data": "/mnt/nfs/data"},
	)
	if len(got) != 2 {
		t.Fatalf("want 2 skipped mounts, got %d", len(got))
	}

	byDest := map[string]SkippedMount{}
	for _, sm := range got {
		byDest[sm.Destination] = sm
	}

	data, ok := byDest["/data"]
	if !ok {
		t.Fatalf("missing skipped mount for /data")
	}
	if data.Type != "bind" {
		t.Errorf("type = %q, want bind", data.Type)
	}
	if data.Source != "/mnt/nfs/data" {
		t.Errorf("source = %q, want /mnt/nfs/data (host path carried through)", data.Source)
	}
	if data.Reason != unreadableReason {
		t.Errorf("reason = %q, want the permission-denied reason", data.Reason)
	}

	// A bind with no known host source still records a skipped mount (empty source).
	if byDest["/config"].Source != "" {
		t.Errorf("source for /config = %q, want empty", byDest["/config"].Source)
	}
}

func TestUnreadableSkippedMountsEmpty(t *testing.T) {
	if got := unreadableSkippedMounts(nil, nil); got != nil {
		t.Fatalf("want nil for no unreadable binds, got %v", got)
	}
}

// The set a restore may have to MATERIALISE is wider than the set actually
// captured, and webapp is the case that shows why: its bundled database's
// data directory is superseded by its dump and never archived, and its web-push
// secret is a file that cannot be restored through its own mount point — yet the
// container will not start unless both paths exist on the target.
func TestBindMountRootsIncludesReadOnlyAndExcludesSystemPaths(t *testing.T) {
	roots := bindMountRoots([]types.MountPoint{
		{Type: "bind", Source: "/volume1/docker/webapp/volumes/pgdata", Destination: "/var/lib/postgresql/data", RW: true},
		{Type: "bind", Source: "/volume1/docker/webapp/secrets/vapid_private_key", Destination: "/run/secrets/vapid_private_key", RW: false},
		{Type: "bind", Source: "/volume1/docker/webapp/volumes/geoip", Destination: "/app/volumes/geoip", RW: false},
		// On every host already, and never ours to create.
		{Type: "bind", Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock", RW: false},
		{Type: "bind", Source: "/etc/localtime", Destination: "/etc/localtime", RW: false},
		{Type: "bind", Source: "/proc/uptime", Destination: "/host/uptime", RW: false},
		// Docker's to create, recorded by MountedVolumes instead.
		{Type: "volume", Name: "appvol", Source: "/var/lib/docker/volumes/appvol/_data", Destination: "/var/lib/app"},
		// A duplicate destination must not be probed (or recorded) twice.
		{Type: "bind", Source: "/volume1/docker/webapp/volumes/pgdata", Destination: "/var/lib/postgresql/data", RW: true},
	})

	var got []string
	for _, m := range roots {
		got = append(got, m.Destination)
		if m.Type != "bind" {
			t.Errorf("%s: every root must be recorded as a bind, got %q", m.Destination, m.Type)
		}
	}
	want := []string{"/var/lib/postgresql/data", "/run/secrets/vapid_private_key", "/app/volumes/geoip"}
	if len(got) != len(want) {
		t.Fatalf("got %d roots %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("root %d: got %q, want %q", i, got[i], want[i])
		}
	}
	// The read-only flag rides along: it is why these two hold no data to restore.
	if roots[1].RW || roots[2].RW {
		t.Error("a read-only bind must be recorded as read-only")
	}
}

// The captured refs and MountedBinds are stamped from ONE probe, matched on the
// container path — the host source is rewritten by the path remap and would not
// match on the machine this is read back on.
func TestApplyBindStatsMatchesOnContainerPath(t *testing.T) {
	refs := []VolumeRef{
		{Destination: "/app/uploads", Type: "bind", Source: "/volume1/docker/webapp/volumes/uploads"},
		{Destination: "/var/lib/app", Type: "volume", Name: "appvol"},
		{Destination: "/app/volumes/logs", Type: "bind", Source: "/volume1/docker/webapp/volumes/logs"},
	}
	roots := []VolumeRef{
		// Same container path, DIFFERENT host source — as it reads after a remap.
		{Destination: "/app/uploads", Type: "bind", Source: "/home/user/docker/webapp/volumes/uploads",
			Kind: dockercli.MountKindDir, Owner: "1000:1000", Mode: "755"},
		// A root with no captured ref: superseded by the dump, still needs creating.
		{Destination: "/var/lib/postgresql/data", Type: "bind", Source: "/x/pgdata",
			Kind: dockercli.MountKindDir, Owner: "101:104", Mode: "700"},
	}

	got := applyBindStats(refs, roots)

	if got[0].Kind != dockercli.MountKindDir || got[0].Owner != "1000:1000" || got[0].Mode != "755" {
		t.Errorf("a captured bind must be stamped from its root: %+v", got[0])
	}
	if got[0].Source != "/volume1/docker/webapp/volumes/uploads" {
		t.Errorf("stamping must not overwrite the captured host source, got %q", got[0].Source)
	}
	if got[1].Kind != "" || got[1].Owner != "" || got[1].Mode != "" {
		t.Errorf("a named volume has no bind root to stamp: %+v", got[1])
	}
	if got[2].Kind != "" || got[2].Owner != "" || got[2].Mode != "" {
		t.Errorf("a bind with no matching root must be left empty, not guessed: %+v", got[2])
	}
	if len(got) != len(refs) {
		t.Errorf("stamping must not add or drop refs: got %d, want %d", len(got), len(refs))
	}
}

// Nothing read means nothing claimed. A backup taken against a node whose probe
// failed still records the sources; a consumer must be able to tell that apart
// from "this is a directory owned by root".
func TestBindVolumeRefLeavesUnknownStatsEmpty(t *testing.T) {
	ref := bindVolumeRef(
		MountInfo{Destination: "/app/uploads", Source: "/srv/uploads", Type: "bind"},
		dockercli.MountStat{},
	)
	if ref.Destination != "/app/uploads" || ref.Source != "/srv/uploads" || ref.Type != "bind" {
		t.Errorf("the mount's identity must be recorded regardless of the probe: %+v", ref)
	}
	if ref.Kind != "" || ref.Owner != "" || ref.Mode != "" {
		t.Errorf("an unread probe must leave no verdict: %+v", ref)
	}
}

// A read-only bind is app data like any other. The capture sidecar attaches
// --volumes-from :ro, so the mount's own flag was never what let DockBack read
// it — and hiding these mounts is how a backup came to report success while
// missing the key its application needs to start.
func TestCandidateMountsIncludesReadOnlyBinds(t *testing.T) {
	got := candidateMounts([]types.MountPoint{
		{Type: "bind", Source: "/srv/webapp/secrets/vapid_private_key", Destination: "/run/secrets/vapid_private_key", RW: false},
		{Type: "bind", Source: "/srv/webapp/volumes/geoip", Destination: "/app/volumes/geoip", RW: false},
		{Type: "bind", Source: "/srv/webapp/volumes/uploads", Destination: "/app/uploads", RW: true},
		{Type: "volume", Name: "cfg", Destination: "/config", RW: true},
	})
	want := []string{"/run/secrets/vapid_private_key", "/app/volumes/geoip", "/app/uploads", "/config"}
	if len(got) != len(want) {
		t.Fatalf("got %d candidates %v, want %d", len(got), got, len(want))
	}
	for i, w := range want {
		if got[i].Destination != w {
			t.Errorf("candidate %d: got %q, want %q", i, got[i].Destination, w)
		}
	}
	// The flag still travels — the picker shows it, and it is why these mounts
	// hold the things a new machine cannot regenerate.
	if got[0].RW || got[1].RW {
		t.Error("a read-only bind must still be recorded as read-only")
	}
}

// Dropping the read-only filter must not drop the exclusions that filter was
// sitting next to. These are the host's, not the container's.
func TestBackupableBindStillExcludesSystemPaths(t *testing.T) {
	for _, src := range []string{
		"/var/run/docker.sock", "/etc/localtime", "/etc/timezone",
		"/etc/hosts", "/etc/resolv.conf", "/etc/hostname",
		"/proc", "/proc/uptime", "/sys", "/sys/fs/cgroup", "/dev", "/dev/dri", "",
		// The whole host filesystem — node-exporter's /:/rootfs:ro and friends.
		// Never a container's own data, and measuring it would spend the size
		// probe's entire budget arriving at "too large".
		"/",
	} {
		if backupableBind(src) {
			t.Errorf("%q must never be a backup candidate", src)
		}
	}
	for _, src := range []string{"/srv/data", "/volume1/docker/app", "/etc/myapp", "/devices", "/procession"} {
		if !backupableBind(src) {
			t.Errorf("%q is ordinary application data and must stay a candidate", src)
		}
	}
}

// A bind whose root is a FILE is captured and restored by a different mechanism,
// so it must leave the tar member list. The volume archive is extracted in a
// sidecar where every bind is a live mount, and replacing a bind-mounted file
// needs an unlink the kernel refuses with EBUSY — which fails the WHOLE
// extraction, so one small file would cost every volume in the archive.
func TestSplitFileBindsPartitionsByKind(t *testing.T) {
	dests := []string{"/var/lib/postgresql/data", "/run/secrets/vapid_private_key", "/app/uploads", "/config"}
	refs := []VolumeRef{
		{Destination: "/var/lib/postgresql/data", Type: "bind", Source: "/x/pgdata"},
		{Destination: "/run/secrets/vapid_private_key", Type: "bind", Source: "/x/secrets/vapid_private_key"},
		{Destination: "/app/uploads", Type: "bind", Source: "/x/uploads"},
		{Destination: "/config", Type: "volume", Name: "cfg"},
	}
	roots := []VolumeRef{
		{Destination: "/var/lib/postgresql/data", Type: "bind", Kind: dockercli.MountKindDir},
		{Destination: "/run/secrets/vapid_private_key", Type: "bind", Kind: dockercli.MountKindFile},
		{Destination: "/app/uploads", Type: "bind", Kind: dockercli.MountKindDir},
	}

	dirDests, dirRefs, fileRefs := splitFileBinds(dests, refs, roots)

	for _, d := range dirDests {
		if d == "/run/secrets/vapid_private_key" {
			t.Error("a file bind must not be a member of the volume tar")
		}
	}
	if len(dirDests) != 3 || len(dirRefs) != 3 {
		t.Errorf("only the file bind may move: dests=%v refs=%d", dirDests, len(dirRefs))
	}
	if len(fileRefs) != 1 {
		t.Fatalf("the file bind must be partitioned out for its own capture, got %v", fileRefs)
	}
	if fileRefs[0].Destination != "/run/secrets/vapid_private_key" {
		t.Errorf("wrong ref partitioned out: %+v", fileRefs[0])
	}
	// The host path is what the restore writes the bytes back to.
	if fileRefs[0].Source != "/x/secrets/vapid_private_key" {
		t.Errorf("the file ref must keep its host source, got %q", fileRefs[0].Source)
	}
	if fileRefs[0].Kind != dockercli.MountKindFile {
		t.Errorf("the file ref must carry its kind so the restore recognises it, got %q", fileRefs[0].Kind)
	}
	// A named volume is always a directory and is never partitioned out.
	for _, r := range fileRefs {
		if r.Type == "volume" {
			t.Error("a named volume must never be treated as a file bind")
		}
	}
}

// Nothing known about a bind's kind means nothing moved. A pre-F81 backup, or a
// probe that could not run, must capture exactly what it captured before —
// guessing "file" would silently drop a directory of real data out of the tar.
func TestSplitFileBindsLeavesUnknownKindsInTheArchive(t *testing.T) {
	dests := []string{"/app/uploads"}
	refs := []VolumeRef{{Destination: "/app/uploads", Type: "bind", Source: "/x/uploads"}}
	roots := []VolumeRef{{Destination: "/app/uploads", Type: "bind"}} // no Kind

	dirDests, dirRefs, fileRefs := splitFileBinds(dests, refs, roots)

	if len(dirDests) != 1 || len(dirRefs) != 1 || len(fileRefs) != 0 {
		t.Errorf("an unknown kind must change nothing: dests=%v refs=%d files=%v", dirDests, len(dirRefs), fileRefs)
	}
}

// The staged member has to land where the restore looks for it. VolumeRef.Archive
// records "config/bind-files/<n>", so the work-dir path must map to exactly that.
func TestLayoutPathPlacesBindFilesUnderConfig(t *testing.T) {
	if got := layoutPath(bindFileWorkDir + "/0"); got != bindFileArchivePrefix+"0" {
		t.Errorf("layoutPath(%q) = %q, want %q", bindFileWorkDir+"/0", got, bindFileArchivePrefix+"0")
	}
	// The neighbouring cases must not have moved.
	if got := layoutPath("inspect.json"); got != "config/inspect.json" {
		t.Errorf("inspect.json = %q", got)
	}
	if got := layoutPath("volumes.tar"); got != "volumes.tar" {
		t.Errorf("volumes.tar = %q", got)
	}
}

// What a restore says about a file whose ownership or mode the backup never
// recorded. Silence would read as "reproduced exactly", which is the one thing
// it does not mean — the file is left root-owned and owner-only instead.
func TestRecordedOwnerAndModeLabelsNameTheUnknown(t *testing.T) {
	if got := recordedOwnerLabel("1026:100"); got != "1026:100" {
		t.Errorf("a recorded owner is reported as-is, got %q", got)
	}
	if got := recordedModeLabel("600"); got != "600" {
		t.Errorf("a recorded mode is reported as-is, got %q", got)
	}
	for _, blank := range []string{"", "   "} {
		if got := recordedOwnerLabel(blank); !strings.Contains(got, "not recorded") {
			t.Errorf("an unrecorded owner must say so, got %q", got)
		}
		if got := recordedModeLabel(blank); !strings.Contains(got, "not recorded") {
			t.Errorf("an unrecorded mode must say so, got %q", got)
		}
	}
}

// planBindSources decides whether DockBack writes to a host filesystem, so every
// outcome is pinned. The shape is webapp's: a captured data directory, a
// directory superseded by the dump, the read-only secret, and the two cases
// where creating something would be a guess.
func TestPlanBindSourcesBucketsEveryOutcome(t *testing.T) {
	missing := []dockercli.BindMount{
		{Source: "/home/user/docker/webapp/volumes/uploads", Destination: "/app/uploads"},
		{Source: "/home/user/docker/webapp/volumes/pgdata", Destination: "/var/lib/postgresql/data"},
		{Source: "/home/user/docker/webapp/secrets/vapid_private_key", Destination: "/run/secrets/vapid_private_key"},
		{Source: "/home/user/docker/webapp/volumes/legacy", Destination: "/app/legacy"},
		{Source: "/etc/ssl/private/app.key", Destination: "/run/tls/app.key"},
	}
	roots := map[string]VolumeRef{
		"/app/uploads":                   {Kind: dockercli.MountKindDir, Owner: "1000:1000", Mode: "755"},
		"/var/lib/postgresql/data":       {Kind: dockercli.MountKindDir, Owner: "101:104", Mode: "700"},
		"/run/secrets/vapid_private_key": {Kind: dockercli.MountKindFile, Owner: "1026:100", Mode: "600", Source: "/volume1/docker/webapp/secrets/vapid_private_key"},
		// "/app/legacy" deliberately absent: a backup that predates the record.
		"/run/tls/app.key": {Kind: dockercli.MountKindFile, Owner: "0:0", Mode: "600"},
	}
	captured := map[string]bool{"/app/uploads": true}

	got := planBindSources(missing, roots, captured)
	if len(got) != len(missing) {
		t.Fatalf("every missing source needs a verdict: got %d, want %d", len(got), len(missing))
	}
	by := map[string]bindSourceAction{}
	for _, a := range got {
		by[a.Destination] = a
	}

	// 1. Captured directory: created bare. Ownership is left alone because the
	//    volume archive carries the mount root and sets it on extraction.
	up := by["/app/uploads"]
	if !up.Create || up.Kind != dockercli.MountKindDir || up.Note != bindNoteWillBeFilled {
		t.Errorf("captured dir: %+v", up)
	}
	if up.Owner != "" || up.Mode != "" {
		t.Errorf("a captured dir must not be pre-stamped — the untar sets it: %+v", up)
	}

	// 2. Uncaptured directory: created WITH the recorded ownership, because
	//    nothing else is going to set it.
	pg := by["/var/lib/postgresql/data"]
	if !pg.Create || pg.Owner != "101:104" || pg.Mode != "700" || pg.Note != bindNoteEmptyDir {
		t.Errorf("uncaptured dir must carry its recorded ownership: %+v", pg)
	}

	// 3. File: an empty placeholder, naming the SOURCE machine's path to fetch.
	key := by["/run/secrets/vapid_private_key"]
	if !key.Create || key.Kind != dockercli.MountKindFile || key.Owner != "1026:100" || key.Mode != "600" {
		t.Errorf("file placeholder: %+v", key)
	}
	if !strings.Contains(key.Note, "/volume1/docker/webapp/secrets/vapid_private_key") {
		t.Errorf("the note must name the source machine's path to copy from, got %q", key.Note)
	}

	// 4. Unrecorded kind: refused on its own, everything else still created.
	legacy := by["/app/legacy"]
	if legacy.Create || legacy.Note != bindNoteUnknownKind {
		t.Errorf("an unrecorded kind must be refused rather than guessed: %+v", legacy)
	}

	// 5. A system location is refused however well described it is.
	tls := by["/run/tls/app.key"]
	if tls.Create {
		t.Errorf("a path inside a system location must never be created: %+v", tls)
	}
	if !strings.Contains(tls.Note, "/etc/ssl/private/app.key") {
		t.Errorf("the refusal must name the path it refused, got %q", tls.Note)
	}
}

// The kind, owner and mode come from the CONTAINER path, because the host path
// remap makes the manifest's source and the restore's source different strings
// on purpose. MountedBinds is authoritative; an older backup's captured entries
// fill the gaps rather than reading as unrecorded.
func TestBindRootsByDestinationPrefersTheCompleteRecord(t *testing.T) {
	man := &Manifest{
		MountedBinds: []VolumeRef{
			{Destination: "/app/uploads", Type: "bind", Source: "/volume1/x/uploads", Kind: dockercli.MountKindDir, Owner: "1000:1000", Mode: "755"},
			{Destination: "/var/lib/postgresql/data", Type: "bind", Source: "/volume1/x/pgdata", Kind: dockercli.MountKindDir, Owner: "101:104", Mode: "700"},
		},
		Volumes: []VolumeRef{
			{Destination: "/app/uploads", Type: "bind", Source: "/volume1/x/uploads"}, // no kind — must not win
			{Destination: "/app/older", Type: "bind", Source: "/volume1/x/older", Kind: dockercli.MountKindDir, Owner: "33:33"},
			{Destination: "/config", Type: "volume", Name: "cfg"},
		},
	}
	got := bindRootsByDestination(man)

	if got["/app/uploads"].Owner != "1000:1000" {
		t.Errorf("MountedBinds must win over a thinner captured entry: %+v", got["/app/uploads"])
	}
	if got["/var/lib/postgresql/data"].Mode != "700" {
		t.Errorf("a mount absent from Volumes must still be described: %+v", got["/var/lib/postgresql/data"])
	}
	if got["/app/older"].Owner != "33:33" {
		t.Errorf("a backup predating MountedBinds must still describe what it captured: %+v", got["/app/older"])
	}
	if _, ok := got["/config"]; ok {
		t.Error("a named volume is not a bind source and must not appear")
	}
	if got := bindRootsByDestination(nil); len(got) != 0 {
		t.Errorf("a nil manifest describes nothing, got %v", got)
	}
}

// F117 re-owns restored data to the ids the target's app runs as. For a
// container that bundles its own services that is destructive: a PostgreSQL data
// directory chowned off uid 101 does not start. The plan's rule — compare each
// path's recorded owner against what the app ran as — is exact when the image
// declares a PUID/PGID-style pair.
func TestAlignablePathsUsesTheRecordedAppOwner(t *testing.T) {
	volumes := []VolumeRef{
		{Destination: "/config", Type: "bind", Owner: "1000:1000"},                // the app's
		{Destination: "/var/lib/postgresql/data", Type: "bind", Owner: "101:104"}, // bundled postgres
		{Destination: "/data", Type: "bind", Owner: "100:102"},                    // bundled redis
		{Destination: "/app/uploads", Type: "bind"},                               // unrecorded — old archive
		{Destination: "", Type: "bind", Owner: "1000:1000"},                       // no destination at all
	}
	align, leave := alignablePaths(volumes, &RunAs{UID: 1000, GID: 1000}, nil)

	if strings.Join(align, ",") != "/config,/app/uploads" {
		t.Errorf("align = %v; want the app's own path and the unrecorded one", align)
	}
	if strings.Join(leave, ",") != "/var/lib/postgresql/data,/data" {
		t.Errorf("leave = %v; want both bundled service directories", leave)
	}
}

// webapp is the case the recorded-owner rule alone cannot reach: the image
// declares no PUID/PGID pair, so the backup records no run_as, so there is
// nothing to compare against. Verified against the real container and its real
// backup — .env sets PUID=1026/PGID=100 but the compose passes them nowhere and
// the image never reads them, so they are vestigial and correctly ignored.
//
// The fallback is not a guess about intent: an id below the host-account range
// is a service account shipped inside the image, and those are the same number
// on every machine, so they are never what F117 is for.
func TestAlignablePathsLeavesServiceAccountsWhenNoRunAsRecorded(t *testing.T) {
	volumes := []VolumeRef{
		{Destination: "/app/uploads", Type: "bind", Owner: "1000:1000"},
		{Destination: "/app/volumes/geoip", Type: "bind", Owner: "1026:100"},             // a DSM host account
		{Destination: "/run/secrets/vapid_private_key", Type: "bind", Owner: "1026:100"}, // the app must read this
		{Destination: "/data", Type: "bind", Owner: "100:102"},                           // redis
		{Destination: "/var/lib/postgresql/data", Type: "bind", Owner: "101:104"},        // postgres
		{Destination: "/srv/rootowned", Type: "bind", Owner: "0:0"},                      // root is not a service account
	}
	align, leave := alignablePaths(volumes, nil, nil)

	for _, want := range []string{"/app/uploads", "/app/volumes/geoip", "/run/secrets/vapid_private_key", "/srv/rootowned"} {
		if !slicesContains(align, want) {
			t.Errorf("%s is host-owned data and must still be aligned; got align=%v", want, align)
		}
	}
	if strings.Join(leave, ",") != "/data,/var/lib/postgresql/data" {
		t.Errorf("leave = %v; want only the ids the image ships", leave)
	}
}

// The one rule that protects a backup taken before ownership was recorded at
// all — which is the state of the webapp archive on disk today: run_as
// absent, every volume owner empty, and /var/lib/postgresql/data captured raw
// because the embedded dump did not run. Without this, pinning an owner would
// chown that directory and postgres would refuse to start.
func TestAlignablePathsLeavesTheDeclaredEmbeddedDataDir(t *testing.T) {
	volumes := []VolumeRef{
		{Destination: "/app/uploads", Type: "bind"},
		{Destination: "/var/lib/postgresql/data", Type: "bind"},
		{Destination: "/data", Type: "bind"},
	}
	embedded := &EmbeddedDump{Engine: "postgres", DataDir: "/var/lib/postgresql/data"}
	align, leave := alignablePaths(volumes, nil, embedded)

	if strings.Join(leave, ",") != "/var/lib/postgresql/data" {
		t.Errorf("leave = %v; want the image's declared database directory", leave)
	}
	// Everything else with no recorded owner behaves exactly as it did before,
	// so an older archive does not regress.
	if strings.Join(align, ",") != "/app/uploads,/data" {
		t.Errorf("align = %v; an unrecorded owner must keep today's behaviour", align)
	}
}

func TestServiceAccountOwner(t *testing.T) {
	for _, owner := range []string{"1:1", "100:102", "101:104", "999:999"} {
		if !serviceAccountOwner(owner) {
			t.Errorf("%q is an image's own account", owner)
		}
	}
	for _, owner := range []string{"0:0", "1000:1000", "1026:100", "65534:65534"} {
		if serviceAccountOwner(owner) {
			t.Errorf("%q is a host account (or root) and must stay alignable", owner)
		}
	}
	// Unreadable input is never excluded on a guess.
	for _, owner := range []string{"", "   ", "abc:def", "1000", "postgres:postgres"} {
		if serviceAccountOwner(owner) {
			t.Errorf("%q cannot be read as an id and must not be treated as one", owner)
		}
	}
}

func slicesContains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// Which uid the application reads its files as, when nothing states it outright.
// The evidence is the data it must be able to WRITE: read-only mounts are
// excluded because a reference set is often owned by whoever put it on the host,
// and webapp is exactly that case — its read-only geoip directory is
// 1026:100 while everything the app writes is 1000:1000.
func TestWritableDataOwnerUID(t *testing.T) {
	webapp := &Manifest{MountedBinds: []VolumeRef{
		{Destination: "/app/uploads", Kind: dockercli.MountKindDir, Owner: "1000:1000"},
		{Destination: "/app/volumes/attachments", Kind: dockercli.MountKindDir, Owner: "1000:1000"},
		{Destination: "/app/volumes/logs", Kind: dockercli.MountKindDir, Owner: "1000:1000"},
		{Destination: "/app/volumes/geoip", Kind: dockercli.MountKindDir, Owner: "1026:100", ReadOnly: true},
		{Destination: "/data", Kind: dockercli.MountKindDir, Owner: "100:102"},                    // redis, image account
		{Destination: "/var/lib/postgresql/data", Kind: dockercli.MountKindDir, Owner: "101:104"}, // postgres
		{Destination: "/run/secrets/vapid_private_key", Kind: dockercli.MountKindFile, Owner: "1026:100", ReadOnly: true},
	}}
	got, ok := writableDataOwnerUID(webapp)
	if !ok || got != "1000" {
		t.Errorf("got (%q,%v); the writable data is unanimously 1000:1000", got, ok)
	}

	// Genuine disagreement means no answer — a wrong uid becomes a chown someone
	// pastes into a root shell.
	split := &Manifest{MountedBinds: []VolumeRef{
		{Destination: "/a", Kind: dockercli.MountKindDir, Owner: "1000:1000"},
		{Destination: "/b", Kind: dockercli.MountKindDir, Owner: "1026:100"},
	}}
	if _, ok := writableDataOwnerUID(split); ok {
		t.Error("two different writable owners must produce no answer")
	}
	// Nothing recorded at all, and a manifest that is not there.
	if _, ok := writableDataOwnerUID(&Manifest{}); ok {
		t.Error("no recorded binds must produce no answer")
	}
	if _, ok := writableDataOwnerUID(nil); ok {
		t.Error("a nil manifest must produce no answer")
	}
	// Only service accounts: the application's own ids are still unknown.
	onlyService := &Manifest{MountedBinds: []VolumeRef{
		{Destination: "/data", Kind: dockercli.MountKindDir, Owner: "100:102"},
	}}
	if _, ok := writableDataOwnerUID(onlyService); ok {
		t.Error("accounts the image ships say nothing about the application's uid")
	}
}

// Ownership only decides the outcome when the mode denies everyone else. A file
// the world can read is the permission audit's business, not this one's.
func TestOwnerOnlyReadable(t *testing.T) {
	for _, mode := range []string{"600", "400", "700", "0600", "200"} {
		if !ownerOnlyReadable(mode) {
			t.Errorf("%s denies group and other, so ownership decides", mode)
		}
	}
	for _, mode := range []string{"644", "640", "604", "777", "444"} {
		if ownerOnlyReadable(mode) {
			t.Errorf("%s is readable beyond its owner, so this is not the finding", mode)
		}
	}
	// A mode that could not be read must never produce a finding.
	for _, mode := range []string{"", "   ", "abc", "-"} {
		if ownerOnlyReadable(mode) {
			t.Errorf("%q is unreadable and must not be judged", mode)
		}
	}
}

func TestOwnerUID(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"1026:100", "1026"}, {"0:0", "0"}, {"1000:1000", "1000"}, {" 101:104 ", "101"},
	} {
		got, ok := ownerUID(c.in)
		if !ok || got != c.want {
			t.Errorf("ownerUID(%q) = (%q,%v), want %q", c.in, got, ok, c.want)
		}
	}
	for _, bad := range []string{"", "1000", "postgres:postgres", ":100", "abc:def"} {
		if _, ok := ownerUID(bad); ok {
			t.Errorf("%q is not a numeric uid:gid", bad)
		}
	}
}

// The chown an operator runs is on the HOST, so the container path has to be
// followed back through the bind it lives under and the remap this restore
// applied. A file bind maps exactly; a file inside a directory bind keeps its
// tail; the longest matching bind wins.
func TestHostPathForFollowsTheBindAndTheRemap(t *testing.T) {
	e := &Engine{Log: func(string, string, string) {}}
	man := &Manifest{MountedBinds: []VolumeRef{
		{Destination: "/run/secrets/vapid_private_key", Source: "/volume1/docker/gc/secrets/vapid_private_key", Kind: dockercli.MountKindFile},
		{Destination: "/config", Source: "/volume1/docker/gc/config", Kind: dockercli.MountKindDir},
		{Destination: "/config/keys", Source: "/volume1/docker/gc/keys", Kind: dockercli.MountKindDir},
	}}
	opts := RestoreOptions{RemapFromPath: "/volume1/docker/gc", RemapToPath: "/home/user/docker/gc"}

	if got := e.hostPathFor(man, "/run/secrets/vapid_private_key", opts); got != "/home/user/docker/gc/secrets/vapid_private_key" {
		t.Errorf("file bind: got %q", got)
	}
	if got := e.hostPathFor(man, "/config/app.ini", opts); got != "/home/user/docker/gc/config/app.ini" {
		t.Errorf("inside a directory bind: got %q", got)
	}
	// /config/keys is a bind of its own and is the longer match, so it wins over
	// /config — otherwise the instruction names a path that does not hold the file.
	if got := e.hostPathFor(man, "/config/keys/id_ed25519", opts); got != "/home/user/docker/gc/keys/id_ed25519" {
		t.Errorf("longest matching bind must win: got %q", got)
	}
	// A sibling whose name merely starts the same is not inside it.
	if got := e.hostPathFor(man, "/configuration/x", opts); got != "/configuration/x" {
		t.Errorf("a prefix that is not a path boundary must not match: got %q", got)
	}
	// No remap configured: the source path stands as recorded.
	if got := e.hostPathFor(man, "/config/app.ini", RestoreOptions{}); got != "/volume1/docker/gc/config/app.ini" {
		t.Errorf("without a remap the recorded source stands: got %q", got)
	}
	// Nothing covers it: the container path is a partly-useful answer, and better
	// than none.
	if got := e.hostPathFor(man, "/elsewhere/key", opts); got != "/elsewhere/key" {
		t.Errorf("uncovered path: got %q", got)
	}
}

// The dialog and the restore must never disagree about what is going to happen,
// so both go through the same bucketing. This is the exported view the pre-restore
// panel renders: one captured directory, one the backup does not hold, one file
// whose bytes ARE in the archive, one whose bytes are not, and one path DockBack
// refuses to create.
func TestPlanBindSourcesExportedView(t *testing.T) {
	man := &Manifest{
		MountedBinds: []VolumeRef{
			{Destination: "/app/uploads", Source: "/volume1/gc/uploads", Kind: dockercli.MountKindDir, Owner: "1000:1000", Mode: "755"},
			{Destination: "/var/lib/postgresql/data", Source: "/volume1/gc/pgdata", Kind: dockercli.MountKindDir, Owner: "101:104", Mode: "700"},
			{Destination: "/run/secrets/vapid_private_key", Source: "/volume1/gc/secrets/vapid_private_key", Kind: dockercli.MountKindFile, Owner: "1026:100", Mode: "600"},
			{Destination: "/run/secrets/legacy", Source: "/volume1/gc/secrets/legacy", Kind: dockercli.MountKindFile, Owner: "1026:100", Mode: "600"},
			{Destination: "/run/tls/app.key", Source: "/etc/ssl/private/app.key", Kind: dockercli.MountKindFile},
		},
		Volumes: []VolumeRef{
			{Destination: "/app/uploads", Type: "bind", Source: "/volume1/gc/uploads"},
			// Captured WITH its bytes: restoreBindFiles writes this one.
			{Destination: "/run/secrets/vapid_private_key", Type: "bind", Source: "/volume1/gc/secrets/vapid_private_key", Kind: dockercli.MountKindFile, Archive: "config/bind-files/0"},
		},
	}
	// Every source is missing on the target, remapped onto this host's layout.
	missing := RecordedBindSources(man, "/volume1/gc", "/home/user/docker/gc")
	got := PlanBindSources(man, missing)

	by := map[string]BindSourcePlan{}
	for _, r := range got {
		by[r.Destination] = r
	}
	if len(got) != 5 {
		t.Fatalf("every missing source needs a row, got %d: %+v", len(got), got)
	}
	for _, c := range []struct{ dest, action string }{
		{"/app/uploads", BindActionFill},
		{"/var/lib/postgresql/data", BindActionCreateEmpty},
		{"/run/secrets/vapid_private_key", BindActionWriteFile},
		{"/run/secrets/legacy", BindActionPlaceholder},
		{"/run/tls/app.key", BindActionManual},
	} {
		if by[c.dest].Action != c.action {
			t.Errorf("%s: action = %q, want %q (note %q)", c.dest, by[c.dest].Action, c.action, by[c.dest].Note)
		}
		if by[c.dest].Note == "" {
			t.Errorf("%s: every row must say what will happen", c.dest)
		}
	}
	// The panel shows the path on the TARGET, so the remap has to be applied.
	if by["/app/uploads"].Source != "/home/user/docker/gc/uploads" {
		t.Errorf("the row must name the remapped host path, got %q", by["/app/uploads"].Source)
	}
	// A path outside the remap base is untouched, and still refused on its merits.
	if by["/run/tls/app.key"].Source != "/etc/ssl/private/app.key" {
		t.Errorf("a path outside the base must not be rewritten, got %q", by["/run/tls/app.key"].Source)
	}
	// The placeholder names the SOURCE machine's path — where the operator goes
	// to fetch the real file, not where it will land.
	if !strings.Contains(by["/run/secrets/legacy"].Note, "/volume1/gc/secrets/legacy") {
		t.Errorf("the placeholder note must name the source host's path, got %q", by["/run/secrets/legacy"].Note)
	}
}

// A backup taken before bind roots were recorded describes nothing, so the panel
// shows nothing rather than an empty list that reads as "all clear".
func TestRecordedBindSourcesIsEmptyForAnOlderBackup(t *testing.T) {
	if got := RecordedBindSources(&Manifest{Volumes: []VolumeRef{{Destination: "/a", Type: "bind", Source: "/x/a"}}}, "", ""); len(got) != 0 {
		t.Errorf("only MountedBinds describes the roots; got %+v", got)
	}
	if got := RecordedBindSources(nil, "", ""); got != nil {
		t.Errorf("a nil manifest describes nothing, got %+v", got)
	}
}

// The regression this exists to stop, using the EXACT value in the live database
// and webapp's real mounts.
//
// Making read-only binds selectable did not add them to selections already
// saved, so the picker showed the application's own web-push key unticked, with
// no reason given — indistinguishable from a mount the operator had excluded on
// purpose. Backing up then would have produced the same archive-without-the-key
// that started all of this.
func TestRememberedSelectionDoesNotHideNewlyOfferedMounts(t *testing.T) {
	st := testStoreT(t)
	e := &Engine{Store: st, Log: func(string, string, string) {}}

	// Verbatim from settings: six destinations, no record of what was on offer.
	const legacy = `["/app/uploads","/app/volumes/logs","/var/lib/postgresql/data","/data","/app/volumes/attachments","/app/volumes/backups"]`
	if err := st.SetSetting(MountSelectionKey("a3645d112b1e", "webapp"), legacy); err != nil {
		t.Fatal(err)
	}
	sel, offered, ok := e.loadMountSelection("a3645d112b1e", "webapp")
	if !ok || len(sel) != 6 {
		t.Fatalf("the legacy array must still load: %v (ok=%v)", sel, ok)
	}
	if offered != nil {
		t.Fatal("a legacy selection records nothing about what was on offer, and must say so with nil")
	}

	selSet := setOf(sel)
	const thr = int64(5) << 30
	for _, c := range []struct {
		name    string
		mount   MountInfo
		wantOn  bool
		wantWhy string
	}{
		// The two the operator asked about: read-only, tiny, absent from a
		// selection saved when they could not have been ticked.
		{"the web-push key", MountInfo{Destination: "/run/secrets/vapid_private_key", Type: "bind", RW: false, SizeBytes: 43, SizeKnown: true}, true, newlyOfferedReason},
		{"the geoip set", MountInfo{Destination: "/app/volumes/geoip", Type: "bind", RW: false, SizeBytes: 0, SizeKnown: true}, true, newlyOfferedReason},
		// Everything the operator actually chose is untouched.
		{"a chosen bind", MountInfo{Destination: "/app/uploads", Type: "bind", RW: true, SizeBytes: 12 << 20, SizeKnown: true}, true, ""},
		{"the database dir", MountInfo{Destination: "/var/lib/postgresql/data", Type: "bind", RW: true, SizeBytes: 64 << 20, SizeKnown: true}, true, ""},
		// A WRITABLE bind absent from the selection was always offerable, so its
		// absence IS a decision and must be respected.
		{"a deliberately excluded bind", MountInfo{Destination: "/app/media", Type: "bind", RW: true, SizeBytes: 1 << 20, SizeKnown: true}, false, ""},
	} {
		on, why := defaultSelected(c.mount, true, selSet, offered, thr)
		if on != c.wantOn {
			t.Errorf("%s: selected = %v, want %v", c.name, on, c.wantOn)
		}
		if why != c.wantWhy {
			t.Errorf("%s: reason = %q, want %q", c.name, why, c.wantWhy)
		}
	}

	// A newly offered mount too large for the default is still left off — the
	// size rule decides it, and it says why rather than going quiet.
	huge := MountInfo{Destination: "/app/volumes/media", Type: "bind", RW: false, SizeBytes: 40 << 30, SizeKnown: true}
	if on, why := defaultSelected(huge, true, selSet, offered, thr); on || !strings.Contains(why, "large bind") {
		t.Errorf("a huge newly-offered bind must stay off with the size reason, got (%v,%q)", on, why)
	}
}

// Once the offered set IS recorded, it is the authority and the read-only rule
// stops applying — so a read-only mount the operator unticks stays unticked.
func TestRecordedOfferedSetMakesAnExclusionStick(t *testing.T) {
	st := testStoreT(t)
	e := &Engine{Store: st, Log: func(string, string, string) {}}

	// The operator sees three mounts and unticks the read-only one.
	if err := e.SetMountSelection("n1", "app", []string{"/data", "/config"},
		[]string{"/data", "/config", "/ref"}); err != nil {
		t.Fatal(err)
	}
	sel, offered, ok := e.loadMountSelection("n1", "app")
	if !ok || offered == nil || !offered["/ref"] {
		t.Fatalf("the offered set must round-trip: sel=%v offered=%v", sel, offered)
	}
	selSet := setOf(sel)
	const thr = int64(5) << 30

	ref := MountInfo{Destination: "/ref", Type: "bind", RW: false, SizeBytes: 10, SizeKnown: true}
	if on, _ := defaultSelected(ref, true, selSet, offered, thr); on {
		t.Error("a read-only mount that WAS on offer and was unticked must stay unticked")
	}
	// And a mount that appears later still reads as new.
	fresh := MountInfo{Destination: "/added-later", Type: "bind", RW: true, SizeBytes: 10, SizeKnown: true}
	if on, why := defaultSelected(fresh, true, selSet, offered, thr); !on || why != newlyOfferedReason {
		t.Errorf("a mount absent from the recorded offer is new, got (%v,%q)", on, why)
	}
}

// The mis-report the operator caught: the picker showed the web-push key ticked
// and the very next backup recorded it as "bind of unknown size excluded by
// default".
//
// Both were running the same rule on different data. ListMounts measures sizes,
// so the size rule said "small, include". selectMounts deliberately does NOT
// measure when a remembered selection exists — so the same rule saw no size at
// all and said "unknown, skip". A newly-offered mount therefore has to be
// measured before it is judged, or the two disagree by construction.
func TestNewlyOfferedMountIsJudgedOnAMeasuredSize(t *testing.T) {
	const thr = int64(5) << 30
	key := MountInfo{Destination: "/run/secrets/vapid_private_key", Type: "bind", RW: false}

	// Unmeasured — exactly what selectMounts holds before it measures. The size
	// rule can only skip it, which is the bug.
	if on, why := sizeDefaultSelected(key, thr); on || !strings.Contains(why, "size unknown") {
		t.Fatalf("precondition: an unmeasured bind reads as unknown, got (%v,%q)", on, why)
	}
	// Measured, as the picker has it and as the run must now have it too.
	key.SizeBytes, key.SizeKnown = 43, true
	if on, why := sizeDefaultSelected(key, thr); !on || why != "" {
		t.Errorf("a measured 43-byte bind must be included, got (%v,%q)", on, why)
	}
	// And the size rule still protects against hauling in something huge that
	// merely happens to be newly offered.
	huge := MountInfo{Destination: "/media", Type: "bind", RW: false, SizeBytes: 40 << 30, SizeKnown: true}
	if on, why := sizeDefaultSelected(huge, thr); on || !strings.Contains(why, "large bind") {
		t.Errorf("a huge newly-offered bind must still be left out, got (%v,%q)", on, why)
	}
}

// captureBindRoots is the F81 work both capture paths need. It lived in only one
// of them, so an app-consistent stack backup recorded no bind roots at all —
// which silently disables the materialiser, the ownership rules and the
// pre-restore plan, all of which read MountedBinds.
func TestCaptureBindRootsRecordsRootsAndPartitionsFileBinds(t *testing.T) {
	e := &Engine{Log: func(string, string, string) {}}
	insp := types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{Name: "/webapp"},
		Mounts: []types.MountPoint{
			{Type: "bind", Source: "/volume1/gc/uploads", Destination: "/app/uploads", RW: true},
			{Type: "bind", Source: "/volume1/gc/secrets/vapid_private_key", Destination: "/run/secrets/vapid_private_key", RW: false},
			{Type: "bind", Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock", RW: false},
		},
	}
	man := &Manifest{}
	refs := []VolumeRef{
		{Destination: "/app/uploads", Type: "bind", Source: "/volume1/gc/uploads"},
		{Destination: "/run/secrets/vapid_private_key", Type: "bind", Source: "/volume1/gc/secrets/vapid_private_key"},
	}
	dests := []string{"/app/uploads", "/run/secrets/vapid_private_key"}

	// cli is nil: no daemon, so no kind is probed and nothing is partitioned —
	// but the ROOTS must still be recorded, because their identity is what a
	// restore needs and it does not depend on the probe succeeding.
	gotDests, gotRefs, _ := e.captureBindRoots(context.Background(), nil, insp, "c1", t.TempDir(), "run1", man, dests, refs)

	if len(man.MountedBinds) != 2 {
		t.Fatalf("both app binds must be recorded (the Docker socket is not ours to create), got %+v", man.MountedBinds)
	}
	byDest := map[string]VolumeRef{}
	for _, v := range man.MountedBinds {
		byDest[v.Destination] = v
	}
	if byDest["/run/secrets/vapid_private_key"].Source != "/volume1/gc/secrets/vapid_private_key" {
		t.Errorf("a recorded root must carry its host source: %+v", byDest["/run/secrets/vapid_private_key"])
	}
	if !byDest["/run/secrets/vapid_private_key"].ReadOnly {
		t.Error("the read-only flag identifies which mounts the app must be able to write")
	}
	if _, ok := byDest["/var/run/docker.sock"]; ok {
		t.Error("the Docker socket is on every host and is never DockBack's to create")
	}
	// With no probe there is no kind, so nothing may be partitioned out — the
	// alternative is guessing, which is what the whole feature exists to avoid.
	if len(gotDests) != 2 || len(gotRefs) != 2 {
		t.Errorf("an unprobed kind must leave the archive unchanged: dests=%v refs=%d", gotDests, len(gotRefs))
	}
	if len(man.Volumes) != 2 {
		t.Errorf("the captured refs must still be recorded, got %+v", man.Volumes)
	}
}

// A restore that leaves an empty stack folder behind reads as data that was
// lost. It is not — writing to a host filesystem is opt-in — but the backup
// advertises those files, so the restore has to say it is not writing them.
func TestNoteUnwrittenOriginals(t *testing.T) {
	var lines []string
	e := &Engine{Log: func(_, _, msg string) { lines = append(lines, msg) }}

	// Reconstruction ON: it writes them, so it says nothing here.
	e.noteUnwrittenOriginals("r", &Manifest{HasOriginalCompose: true}, true)
	if len(lines) != 0 {
		t.Errorf("nothing to report when the files are being written: %v", lines)
	}
	// Nothing captured: nothing to mention.
	e.noteUnwrittenOriginals("r", &Manifest{}, false)
	e.noteUnwrittenOriginals("r", nil, false)
	if len(lines) != 0 {
		t.Errorf("a backup with no originals must stay quiet: %v", lines)
	}

	// Captured, not written: say so, and say where to get them.
	e.noteUnwrittenOriginals("r", &Manifest{HasOriginalCompose: true}, false)
	if len(lines) != 1 {
		t.Fatalf("expected exactly one line, got %v", lines)
	}
	for _, want := range []string{"Reconstruct stack folder on host", originalComposeArchivePrefix, "NOT written"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the notice must contain %q, got %q", want, lines[0])
		}
	}

	// A stack whose .env was captured but whose compose file was not is still
	// worth mentioning — the .env is the half that carries the addresses.
	lines = nil
	e.noteUnwrittenOriginals("r", &Manifest{Format: Format{Layout: "manifest.json, " + originalComposeArchivePrefix + "*"}}, false)
	if len(lines) != 1 {
		t.Errorf("an archive holding only the .env must still be mentioned, got %v", lines)
	}
}

// R3's real Nextcloud layout: the webroot is a bind, and four more binds sit
// inside it. Naming all five in one tar member list walked 22 GB twice.
var r3NextcloudMounts = []string{
	"/var/www/html",
	"/var/www/html/data",
	"/var/www/html/config",
	"/var/www/html/custom_apps",
	"/var/www/html/themes",
}

func TestNestedWithin(t *testing.T) {
	for _, tc := range []struct {
		name  string
		dests []string
		want  map[string]string
	}{
		{
			name:  "R3's five-mount Nextcloud layout",
			dests: r3NextcloudMounts,
			want: map[string]string{
				"/var/www/html/data":        "/var/www/html",
				"/var/www/html/config":      "/var/www/html",
				"/var/www/html/custom_apps": "/var/www/html",
				"/var/www/html/themes":      "/var/www/html",
			},
		},
		{
			// A plain string prefix would call this nested and silently drop a
			// whole separate mount from the archive.
			name:  "a sibling sharing a name prefix is not nested",
			dests: []string{"/var/www/html", "/var/www/html2", "/var/www/htmlx/deep"},
			want:  map[string]string{},
		},
		{
			// Only the outermost is kept as a member, but each child must record
			// the mount it actually sits in, not the outermost one.
			name:  "mounts nested more than one deep take their nearest parent",
			dests: []string{"/var/www/html", "/var/www/html/data", "/var/www/html/data/files"},
			want: map[string]string{
				"/var/www/html/data":       "/var/www/html",
				"/var/www/html/data/files": "/var/www/html/data",
			},
		},
		{
			name:  "a mount at the root contains everything else",
			dests: []string{"/", "/data"},
			want:  map[string]string{"/data": "/"},
		},
		{
			name:  "trailing slashes describe the same mount",
			dests: []string{"/var/www/html/", "/var/www/html/data"},
			want:  map[string]string{"/var/www/html/data": "/var/www/html"},
		},
		{
			name:  "unrelated mounts",
			dests: []string{"/config", "/data", "/media"},
			want:  map[string]string{},
		},
		{
			name:  "a repeated destination is not its own parent",
			dests: []string{"/data", "/data"},
			want:  map[string]string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := nestedWithin(tc.dests)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d nested mount(s) %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for child, parent := range tc.want {
				if got[child] != parent {
					t.Errorf("%s: parent = %q, want %q", child, got[child], parent)
				}
			}
		})
	}
}

// The fold has to happen where BOTH capture paths reach it, and it must never
// drop a mount whose bytes nothing else is carrying.
func TestSelectMountsExcludesNestedChildren(t *testing.T) {
	e := &Engine{Log: func(string, string, string) {}}
	mounts := []types.MountPoint{}
	refs := []VolumeRef{}
	dests := []string{}
	for _, d := range r3NextcloudMounts {
		mounts = append(mounts, types.MountPoint{Type: "bind", Source: "/srv/nc" + d, Destination: d, RW: true})
		refs = append(refs, VolumeRef{Destination: d, Type: "bind", Source: "/srv/nc" + d})
		dests = append(dests, d)
	}
	// A sibling that merely shares a name prefix, and a mount whose own parent
	// was NOT selected — the parent's member does not exist, so this one is the
	// only copy of its bytes and must stay a member of its own.
	for _, d := range []string{"/var/www/html2", "/opt/app/data"} {
		mounts = append(mounts, types.MountPoint{Type: "bind", Source: "/srv" + d, Destination: d, RW: true})
		refs = append(refs, VolumeRef{Destination: d, Type: "bind", Source: "/srv" + d})
		dests = append(dests, d)
	}

	man := &Manifest{}
	insp := types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{Name: "/nextcloud"},
		Mounts:            mounts,
	}
	gotDests, gotRefs, _ := e.captureBindRoots(context.Background(), nil, insp, "c1", t.TempDir(), "run1", man, dests, refs)

	want := []string{"/var/www/html", "/var/www/html2", "/opt/app/data"}
	if len(gotDests) != len(want) {
		t.Fatalf("archive members = %v, want exactly %v", gotDests, want)
	}
	got := map[string]bool{}
	for _, d := range gotDests {
		got[d] = true
	}
	for _, d := range want {
		if !got[d] {
			t.Errorf("%s must stay a member — nothing else carries its bytes", d)
		}
	}

	// Every ref survives: the manifest still describes all seven mounts, because
	// a restore and the UI need the topology the member list no longer shows.
	if len(gotRefs) != len(refs) {
		t.Errorf("refs = %d, want all %d mounts still described", len(gotRefs), len(refs))
	}
	nestedIn := map[string]string{}
	for _, r := range gotRefs {
		nestedIn[r.Destination] = r.NestedIn
	}
	for _, child := range r3NextcloudMounts[1:] {
		if nestedIn[child] != "/var/www/html" {
			t.Errorf("%s: NestedIn = %q, want /var/www/html", child, nestedIn[child])
		}
	}
	for _, standalone := range []string{"/var/www/html", "/var/www/html2", "/opt/app/data"} {
		if nestedIn[standalone] != "" {
			t.Errorf("%s: NestedIn = %q, want empty — its bytes are its own member's", standalone, nestedIn[standalone])
		}
	}
}

// R3 §4: one host directory bound at both the MariaDB datadir and the directory
// the server scans for configuration. Recorded correctly, noticed by nothing —
// bind roots are keyed by destination, so the shared source is invisible.
func TestDuplicateMountTargetFinding(t *testing.T) {
	const dbDir = "/volume1/docker/nextcloud/db"

	t.Run("one source at two destinations yields exactly one finding", func(t *testing.T) {
		var logs []string
		e := &Engine{Log: func(_, _, msg string) { logs = append(logs, msg) }}
		man := &Manifest{MountedBinds: []VolumeRef{
			{Destination: "/var/lib/mysql", Source: dbDir, Type: "bind"},
			{Destination: "/etc/mysql/conf.d", Source: dbDir, Type: "bind"},
			{Destination: "/var/www/html", Source: "/volume1/docker/nextcloud/html", Type: "bind"},
		}}
		e.reportDuplicateMountTargets(man, "run1")

		if len(man.Findings) != 1 {
			t.Fatalf("want exactly one finding, got %d: %+v", len(man.Findings), man.Findings)
		}
		f := man.Findings[0]
		if f.Code != findingDuplicateMountTarget || f.Subject != dbDir {
			t.Errorf("finding = %+v, want code %q on %q", f, findingDuplicateMountTarget, dbDir)
		}
		// Both destinations must be named: the operator cannot act on "mounted
		// twice" without knowing where.
		for _, dest := range []string{"/var/lib/mysql", "/etc/mysql/conf.d"} {
			if !strings.Contains(f.Message, dest) {
				t.Errorf("message must name %s: %q", dest, f.Message)
			}
		}
		if len(logs) != 1 {
			t.Errorf("want one log line, got %d: %v", len(logs), logs)
		}
	})

	t.Run("F83's shared bind across different containers yields none", func(t *testing.T) {
		// The same host path mounted by the web and the cron container is the
		// legitimate shared-bind pattern. Each container records it ONCE, so
		// grouping within a container is what keeps this quiet.
		e := &Engine{Log: func(string, string, string) {}}
		for _, container := range []string{"web", "cron"} {
			man := &Manifest{MountedBinds: []VolumeRef{
				{Destination: "/var/www/html", Source: "/volume1/docker/nextcloud/html", Type: "bind"},
				{Destination: "/var/www/html/data", Source: "/volume1/docker/nextcloud/data", Type: "bind"},
			}}
			e.reportDuplicateMountTargets(man, "run1")
			if len(man.Findings) != 0 {
				t.Errorf("%s: shared binds are normal, got %+v", container, man.Findings)
			}
		}
	})

	t.Run("the same destination recorded twice is not a duplicate target", func(t *testing.T) {
		e := &Engine{Log: func(string, string, string) {}}
		man := &Manifest{MountedBinds: []VolumeRef{
			{Destination: "/data", Source: "/srv/data", Type: "bind"},
			{Destination: "/data/", Source: "/srv/data", Type: "bind"},
		}}
		e.reportDuplicateMountTargets(man, "run1")
		if len(man.Findings) != 0 {
			t.Errorf("one mount described twice is one mount: %+v", man.Findings)
		}
	})
}
