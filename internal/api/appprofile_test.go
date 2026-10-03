package api

import (
	"strings"
	"testing"

	"dockback/internal/backup"
)

func absManifest() *backup.Manifest {
	return &backup.Manifest{
		Image:   "ghcr.io/advplyr/audiobookshelf:latest",
		Volumes: []backup.VolumeRef{{Destination: "/config", Type: "bind"}, {Destination: "/metadata", Type: "bind"}},
		SkippedMounts: []backup.SkippedMount{
			{Destination: "/audiobooks", Type: "bind", Reason: "large bind"},
		},
	}
}

// TestLocalOnlyStorageVerdictBlocks is the failure the guard exists for: an
// embedded database restored onto a CIFS share keeps working for days and then
// corrupts, far from the restore that caused it.
func TestLocalOnlyStorageVerdictBlocks(t *testing.T) {
	warns, blocking := localOnlyStorageVerdict(absManifest(), map[string]string{
		"/config":     "cifs",
		"/metadata":   "ext4",
		"/audiobooks": "cifs", // media on a NAS is normal and must NOT block
	})
	if !blocking {
		t.Fatal("a local-only path measured on CIFS must block")
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "/config") {
		t.Fatalf("expected exactly one warning naming /config, got %v", warns)
	}
	if strings.Contains(warns[0], "/audiobooks") {
		t.Error("media on a network share is expected — it must not be reported")
	}
}

// TestLocalOnlyStorageVerdictAllows: ordinary local filesystems pass silently,
// including one the guard does not recognise. Blocking on anything unfamiliar
// would refuse valid restores far more often than it caught a real problem.
func TestLocalOnlyStorageVerdictAllows(t *testing.T) {
	for _, fs := range []string{"ext4", "btrfs", "zfs", "xfs", "overlay", "somethingnew"} {
		warns, blocking := localOnlyStorageVerdict(absManifest(), map[string]string{"/config": fs})
		if blocking || len(warns) > 0 {
			t.Errorf("%q must pass silently, got blocking=%v warns=%v", fs, blocking, warns)
		}
	}
}

// TestLocalOnlyStorageVerdictUnmeasured keeps the three-state honesty: nothing
// probed means nothing claimed. A recreate has no target container to inspect,
// and the guard must not invent a verdict for it.
func TestLocalOnlyStorageVerdictUnmeasured(t *testing.T) {
	if _, blocking := localOnlyStorageVerdict(absManifest(), nil); blocking {
		t.Error("an unprobed target must never block")
	}
	if _, blocking := localOnlyStorageVerdict(absManifest(), map[string]string{"/config": ""}); blocking {
		t.Error("an empty filesystem reading must never block")
	}
	// An image with no profile is never gated, whatever it sits on.
	unprofiled := &backup.Manifest{Image: "nginx:alpine", Volumes: []backup.VolumeRef{{Destination: "/config"}}}
	if _, blocking := localOnlyStorageVerdict(unprofiled, map[string]string{"/config": "cifs"}); blocking {
		t.Error("an image with no profile must never be gated")
	}
	if _, blocking := localOnlyStorageVerdict(nil, map[string]string{"/config": "cifs"}); blocking {
		t.Error("a nil manifest must never block")
	}
}

// TestNewSiteAddressValidationAtTheEdge (F114) — the value the restore endpoint
// accepts here ends up in an application trust list and in commands run inside
// the container, so the boundary check is part of the endpoint's contract, not
// an implementation detail of the engine.
func TestNewSiteAddressValidationAtTheEdge(t *testing.T) {
	// Blank is the normal case: the address is not changing and nothing is done.
	if err := backup.ValidSiteAddress(""); err == nil {
		t.Error("an empty address must not validate — the handler treats blank as 'no change' and never calls this")
	}
	for _, ok := range []string{"cloud.example.com", "https://cloud.example.com", "10.168.1.50:8080"} {
		if err := backup.ValidSiteAddress(ok); err != nil {
			t.Errorf("%q should be accepted, got %v", ok, err)
		}
	}
	// The rejection that matters: a wildcard would make the restore "work" by
	// disabling the protection the value exists to provide.
	for _, bad := range []string{"*", "*.example.com", "a;id", "a && b", "$(id)", "a b"} {
		if err := backup.ValidSiteAddress(bad); err == nil {
			t.Errorf("%q must be rejected at the endpoint", bad)
		}
	}
}

// TestAppPreconditionsForEndpoint proves the readiness endpoint's helper stays a
// thin pass-through, and that an ordinary container's restore dialog is
// unchanged (nil = the block is not rendered at all).
func TestAppPreconditionsForEndpoint(t *testing.T) {
	pre := appPreconditionsFor(absManifest())
	if pre == nil || pre.App != "Audiobookshelf" {
		t.Fatalf("expected Audiobookshelf preconditions, got %+v", pre)
	}
	if len(pre.DataPaths) == 0 || len(pre.Notes) == 0 {
		t.Errorf("expected both paths and notes, got %+v", pre)
	}
	if appPreconditionsFor(&backup.Manifest{Image: "nginx:alpine"}) != nil {
		t.Error("an unprofiled image must render nothing")
	}
}
