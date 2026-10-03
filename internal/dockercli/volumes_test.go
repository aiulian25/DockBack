package dockercli

import (
	"reflect"
	"testing"
)

// TestOrphanVolumeNames locks in the F23 set-difference: a named volume is orphaned
// exactly when NO container references it via a volume mount. Bind mounts and
// tmpfs never count as volume references, and the result is sorted.
func TestOrphanVolumeNames(t *testing.T) {
	containers := []*Container{
		{Name: "web", Mounts: []Mount{
			{Type: "volume", Name: "web_data"},
			{Type: "bind", Source: "/etc/web", Destination: "/etc"}, // bind — not a volume ref
		}},
		{Name: "db", Mounts: []Mount{
			{Type: "volume", Name: "db_data"},
		}},
		nil, // a nil container must be skipped, not panic
		{Name: "cache", Mounts: []Mount{
			{Type: "tmpfs", Destination: "/tmp"}, // tmpfs — not a volume ref
		}},
	}
	all := []string{"db_data", "old_sonarr", "web_data", "leftover_pg", "web_data"}

	got := orphanVolumeNames(containers, all)
	want := []string{"leftover_pg", "old_sonarr"} // in-use (web_data, db_data) excluded, sorted
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("orphanVolumeNames = %v, want %v", got, want)
	}
}

// TestOrphanVolumeNamesEmpty covers the no-orphans and no-volumes cases.
func TestOrphanVolumeNamesEmpty(t *testing.T) {
	// Every volume in use → none orphaned.
	inUse := []*Container{{Name: "a", Mounts: []Mount{{Type: "volume", Name: "v1"}, {Type: "volume", Name: "v2"}}}}
	if got := orphanVolumeNames(inUse, []string{"v1", "v2"}); len(got) != 0 {
		t.Errorf("expected no orphans, got %v", got)
	}
	// No volumes at all.
	if got := orphanVolumeNames(nil, nil); len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
	// No containers → every volume is orphaned.
	if got := orphanVolumeNames(nil, []string{"z", "a"}); !reflect.DeepEqual(got, []string{"a", "z"}) {
		t.Errorf("expected all volumes orphaned (sorted), got %v", got)
	}
}

// TestValidVolumeName guards the name that gets embedded in a sidecar bind spec.
func TestValidVolumeName(t *testing.T) {
	ok := []string{"web_data", "app.db-1", "A", "sonarr_config", "x0"}
	for _, n := range ok {
		if !ValidVolumeName(n) {
			t.Errorf("%q should be valid", n)
		}
	}
	bad := []string{"", "-leading", ".hidden", "has space", "a:b", "a/b", "a;rm -rf", "a$(x)", "évol"}
	for _, n := range bad {
		if ValidVolumeName(n) {
			t.Errorf("%q should be INVALID", n)
		}
	}
}
