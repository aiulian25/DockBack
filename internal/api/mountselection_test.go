package api

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"

	"dockback/internal/backup"
	"dockback/internal/dockercli"
)

// inspectWithMounts builds the container inspect the validation reads, so the
// candidate-mount rules can be exercised without a Docker daemon.
func inspectWithMounts(mounts ...types.MountPoint) types.ContainerJSON {
	return types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{Name: "/web"},
		Mounts:            mounts,
		Config:            &container.Config{Image: "nginx:1"},
	}
}

// TestCandidateMountDestsOf pins what may be selected at all. The exclusions are
// the point: the Docker socket, system paths and the host root are not app data.
//
// A READ-ONLY bind is offered, and that is deliberate. The capture sidecar
// attaches --volumes-from :ro, so a mount being read-only never stopped DockBack
// reading it — hiding those mounts only meant an application's own data (a key,
// a licence, a reference set) could be absent from a backup that reported
// success. Offering it puts the choice where it belongs, and anything left out
// is recorded as skipped instead of vanishing.
func TestCandidateMountDestsOf(t *testing.T) {
	got := backup.CandidateMountDestsOf(inspectWithMounts(
		types.MountPoint{Type: "volume", Name: "cfg", Destination: "/config", RW: true},
		types.MountPoint{Type: "bind", Source: "/srv/data", Destination: "/data", RW: true},
		types.MountPoint{Type: "bind", Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock", RW: true},
		types.MountPoint{Type: "bind", Source: "/etc/localtime", Destination: "/etc/localtime", RW: false},
		types.MountPoint{Type: "bind", Source: "/srv/ro", Destination: "/ro", RW: false},
		// The host filesystem, as every monitoring agent mounts it.
		types.MountPoint{Type: "bind", Source: "/", Destination: "/rootfs", RW: false},
	))
	want := map[string]bool{"/config": true, "/data": true, "/ro": true}
	if len(got) != len(want) {
		t.Fatalf("candidate mounts = %v, want exactly %v", got, want)
	}
	for _, d := range got {
		if !want[d] {
			t.Errorf("%q must not be offered as a backup-candidate mount", d)
		}
	}
}

// TestSetMountSelectionRoundTrip: the panel writes the SAME setting the
// container page and a backup run read, so a selection made in either place is
// indistinguishable — one source of truth.
func TestSetMountSelectionRoundTrip(t *testing.T) {
	st := testStore(t)
	e := &backup.Engine{Store: st, Log: func(string, string, string) {}}

	if _, ok := e.MountSelection("n1", "web"); ok {
		t.Fatal("a fresh container must have no stored selection (size-based default)")
	}
	if err := e.SetMountSelection("n1", "web", []string{"/config"}, []string{"/config", "/data"}); err != nil {
		t.Fatal(err)
	}
	sel, ok := e.MountSelection("n1", "web")
	if !ok || len(sel) != 1 || sel[0] != "/config" {
		t.Fatalf("stored selection = %v (ok=%v), want [/config]", sel, ok)
	}

	// An EMPTY selection is meaningful — "capture none of this container's
	// mounts" is legitimate when a sibling covers them — and must NOT be
	// confused with "no selection stored", which means the size-based default.
	if err := e.SetMountSelection("n1", "web", []string{}, []string{"/config", "/data"}); err != nil {
		t.Fatal(err)
	}
	sel, ok = e.MountSelection("n1", "web")
	if !ok {
		t.Fatal("an explicit empty selection must be STORED, not treated as unset")
	}
	if len(sel) != 0 {
		t.Fatalf("expected an empty stored selection, got %v", sel)
	}

	// nil clears it, reverting to the size-based default.
	if err := e.SetMountSelection("n1", "web", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.MountSelection("n1", "web"); ok {
		t.Fatal("nil must clear the selection back to the default")
	}
}

// TestStackPanelAndContainerPageAgree is the invariant the whole panel rests on:
// a selection stored by the panel is exactly what the stack row then reports, so
// the count the operator sees can never drift from what a run would capture.
func TestStackPanelAndContainerPageAgree(t *testing.T) {
	st := testStore(t)
	e := &backup.Engine{Store: st, Log: func(string, string, string) {}}
	s := &Server{store: st, engine: e}

	if err := e.SetMountSelection("n1", "web", []string{"/config"}, []string{"/config", "/data"}); err != nil {
		t.Fatal(err)
	}
	row := s.stackServiceOptionsFor("n1", testContainer(), nil)
	if row.MountsSelected != 1 || row.MountsTotal != 2 {
		t.Fatalf("panel row must report 1 of 2 after the panel stored one mount, got %d of %d", row.MountsSelected, row.MountsTotal)
	}

	// And the settings blob the container page reads is untouched by a mount
	// edit — the two are separate keys and must not clobber each other.
	js, _ := json.Marshal(backup.SavedBackupOptions{Compression: "xz", SaveImage: true})
	if err := st.SetSetting(backup.BackupOptionsKey("n1", "web"), string(js)); err != nil {
		t.Fatal(err)
	}
	if err := e.SetMountSelection("n1", "web", []string{"/config", "/data"}, []string{"/config", "/data"}); err != nil {
		t.Fatal(err)
	}
	row = s.stackServiceOptionsFor("n1", testContainer(), nil)
	if row.BackupOptions.Compression != "xz" || !row.BackupOptions.SaveImage {
		t.Fatalf("a mount edit must not disturb the saved backup options, got %+v", row.BackupOptions)
	}
	if row.MountsSelected != 2 {
		t.Fatalf("expected 2 selected mounts, got %d", row.MountsSelected)
	}
}

// testContainer is the two-mount fixture shared by the agreement test.
func testContainer() *dockercli.Container {
	return &dockercli.Container{
		ID: "c1", Name: "web", Service: "web", Stack: "blog", State: "running", Image: "nginx:1",
		Mounts: []dockercli.Mount{
			{Type: "volume", Name: "cfg", Source: "cfg", Destination: "/config", RW: true},
			{Type: "bind", Source: "/srv/blog/data", Destination: "/data", RW: true},
		},
	}
}

// The pre-restore panel refreshes while the operator is still typing a remap
// path, so a half-finished pair must produce NO plan rather than an error
// dialog — and must never produce one computed from a pair the restore itself
// would refuse.
func TestPlannedRemapIsLenientWhileTyping(t *testing.T) {
	st := testStore(t)
	s := &Server{store: st, engine: &backup.Engine{Store: st, Log: func(string, string, string) {}}}

	// Remap not asked for: no remap, and the plan still runs.
	from, to, ok := s.plannedRemap(url.Values{}, "n1", "blog")
	if !ok || from != "" || to != "" {
		t.Errorf("without remap_path the plan runs unremapped, got (%q,%q,%v)", from, to, ok)
	}

	// A usable pair resolves.
	from, to, ok = s.plannedRemap(url.Values{
		"remap_path": {"true"}, "remap_from_path": {"/volume1/docker"}, "remap_to_path": {"/home/user/docker"},
	}, "n1", "blog")
	if !ok || from != "/volume1/docker" || to != "/home/user/docker" {
		t.Errorf("a complete pair must resolve, got (%q,%q,%v)", from, to, ok)
	}

	// A pair that is merely SHORT is still a real pair, and previewing it is the
	// helpful answer: the operator sees rows under /ho and notices before they
	// confirm. Only a pair the restore itself would refuse suppresses the plan.
	if _, to, ok := s.plannedRemap(url.Values{
		"remap_path": {"true"}, "remap_from_path": {"/volume1/docker"}, "remap_to_path": {"/ho"},
	}, "n1", "blog"); !ok || to != "/ho" {
		t.Errorf("a short but valid base must be previewed, got (%q,%v)", to, ok)
	}

	// Pairs the restore would reject produce no plan rather than an error dialog.
	for _, q := range []url.Values{
		{"remap_path": {"true"}, "remap_from_path": {"/volume1/docker"}, "remap_to_path": {"/"}},  // root base
		{"remap_path": {"true"}, "remap_from_path": {"volume1"}, "remap_to_path": {"/home/user"}}, // not absolute
		{"remap_path": {"true"}, "remap_from_path": {"/opt/x"}, "remap_to_path": {"/opt/x"}},      // identical
		{"remap_path": {"true"}}, // nothing typed yet
	} {
		if _, _, ok := s.plannedRemap(q, "n1", "blog"); ok {
			t.Errorf("%v must suppress the bind plan, not resolve", q)
		}
	}
}
