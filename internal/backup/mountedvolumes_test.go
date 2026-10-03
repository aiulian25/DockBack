package backup

import (
	"testing"
)

// F226 — a restore must CREATE every named volume the container mounts, not just
// the ones whose contents it captured.
//
// The failure this fixes: a database's data directory is captured by its dump
// rather than copied, so it was never recorded, so the restore left it to
// Docker — which auto-creates it with no labels. A volume without the
// com.docker.compose.* labels its project expects is one `docker compose up`
// refuses to adopt afterwards, and both remedies that error suggests are wrong
// after a restore: `external: true` permanently disowns the volume, and removing
// it deletes the data that was just recovered.
//
// It is deliberately NOT specific to databases. Every reason a volume goes
// uncaptured — a shared folder captured once via another container, an app's
// regenerable cache excluded on purpose, anything the operator unticked — ends
// in the same place, so the fix is keyed on "the container mounts it", not on
// why its contents came from somewhere else.

func TestVolumesToEnsureCoversUncapturedMounts(t *testing.T) {
	man := &Manifest{
		// What was captured: the app's own volume only.
		Volumes: []VolumeRef{
			{Name: "mediaapp_app_data", Type: "volume", Destination: "/data", Driver: "local"},
		},
		// What the containers MOUNT: the app volume plus the database's data
		// directory, which the dump covers and nothing copies.
		MountedVolumes: []VolumeRef{
			{Name: "mediaapp_app_data", Type: "volume", Destination: "/data", Driver: "local",
				Labels: map[string]string{"com.docker.compose.project": "mediaapp", "com.docker.compose.volume": "app_data"}},
			{Name: "mediaapp_pg_data", Type: "volume", Destination: "/var/lib/postgresql/data", Driver: "local",
				Labels: map[string]string{"com.docker.compose.project": "mediaapp", "com.docker.compose.volume": "pg_data"}},
		},
	}

	got := volumesToEnsure(man)
	names := map[string]VolumeRef{}
	for _, v := range got {
		if _, dup := names[v.Name]; !dup {
			names[v.Name] = v
		}
	}
	if _, ok := names["mediaapp_pg_data"]; !ok {
		t.Fatal("the database's data directory must be created by the restore — it is mounted, even though its contents come from the dump")
	}
	// The mounted record wins, because it is the one carrying the labels: a
	// captured entry from an older code path may have none.
	if names["mediaapp_app_data"].Labels["com.docker.compose.project"] != "mediaapp" {
		t.Errorf("the richer record must win the dedup: %+v", names["mediaapp_app_data"])
	}
}

// A backup taken before MountedVolumes existed still behaves exactly as it did:
// the captured set is what gets created.
func TestVolumesToEnsureFallsBackForOldManifests(t *testing.T) {
	man := &Manifest{
		Volumes: []VolumeRef{{Name: "legacy_data", Type: "volume", Driver: "local"}},
	}
	got := volumesToEnsure(man)
	if len(got) != 1 || got[0].Name != "legacy_data" {
		t.Fatalf("an old manifest must keep the behaviour it had: %+v", got)
	}
	if volumesToEnsure(nil) != nil {
		t.Error("no manifest is nothing to create, not a panic")
	}
	if len(volumesToEnsure(&Manifest{})) != 0 {
		t.Error("an empty manifest creates nothing")
	}
}

// The guard that decides whether a recorded volume is worth creating.
//
// It used to require driver options or a driver, which is why this bug existed:
// a plain local compose volume has neither, so it fell through to Docker and
// lost its labels. Labels now count as something worth recreating — they are, in
// fact, the ONLY thing that distinguishes a volume compose will adopt from one
// it will not.
func TestPlainLocalVolumeWithLabelsIsWorthCreating(t *testing.T) {
	worth := func(v VolumeRef) bool {
		// Mirrors the guard in ensureRecordedVolumes.
		return !(len(v.Options) == 0 && len(v.Labels) == 0 && v.Driver == "")
	}
	cases := []struct {
		name string
		v    VolumeRef
		want bool
	}{
		{"compose volume, no driver opts", VolumeRef{Name: "p_data", Labels: map[string]string{"com.docker.compose.project": "p"}}, true},
		{"nfs volume", VolumeRef{Name: "nas", Driver: "local", Options: map[string]string{"type": "nfs"}}, true},
		{"driver only", VolumeRef{Name: "d", Driver: "local"}, true},
		{"nothing recorded at all", VolumeRef{Name: "bare"}, false},
	}
	for _, c := range cases {
		if got := worth(c.v); got != c.want {
			t.Errorf("%s: worth creating = %v, want %v", c.name, got, c.want)
		}
	}
}

// The compose labels are the point, so their shape is pinned: this is what
// `docker compose up` looks for before deciding a volume is one of its own.
func TestComposeLabelsSurviveTheRecord(t *testing.T) {
	v := VolumeRef{
		Name:   "mediaapp_pg_data",
		Type:   "volume",
		Driver: "local",
		Labels: map[string]string{
			"com.docker.compose.project": "mediaapp",
			"com.docker.compose.volume":  "pg_data",
			"com.docker.compose.version": "2.29.0",
		},
	}
	man := &Manifest{MountedVolumes: []VolumeRef{v}}
	got := volumesToEnsure(man)
	if len(got) != 1 {
		t.Fatalf("want the one volume, got %+v", got)
	}
	for _, k := range []string{"com.docker.compose.project", "com.docker.compose.volume"} {
		if got[0].Labels[k] != v.Labels[k] {
			t.Errorf("label %q must reach the restore verbatim: %+v", k, got[0].Labels)
		}
	}
}
