package backup

import (
	"strings"
	"testing"

	"dockback/internal/dockercli"
)

const gb = int64(1) << 30

func TestRestoreCapacityVerdict(t *testing.T) {
	for _, tc := range []struct {
		name       string
		payload    int64
		free       int64
		total      int64
		wantRefuse bool
		wantAfter  int64
	}{
		{
			// R5 §3's own numbers, pinned deliberately. 10% of a 98 GB root is
			// 9.8 GB and 16 GB would remain, so THIS RULE ALLOWS R5's placement —
			// it is reported, not refused. Refusing it would need a margin near
			// 20%, or knowledge of what the other seventeen containers will grow
			// into. The dialog's numbers are what informs that judgement.
			name:    "R5's 58 GB into 74 GB free on a 98 GB root is allowed, and reported",
			payload: 58 * gb, free: 74 * gb, total: 98 * gb,
			wantRefuse: false, wantAfter: 16 * gb,
		},
		{
			name:    "the same payload onto the SSD it was moved to",
			payload: 58 * gb, free: 2200 * gb, total: 4000 * gb,
			wantRefuse: false, wantAfter: 2142 * gb,
		},
		{
			name:    "a payload that would leave less than a tenth of the disk",
			payload: 70 * gb, free: 74 * gb, total: 98 * gb,
			wantRefuse: true, wantAfter: 4 * gb,
		},
		{
			name:    "a payload larger than the free space",
			payload: 100 * gb, free: 74 * gb, total: 98 * gb,
			wantRefuse: true, wantAfter: -26 * gb,
		},
		{
			// On a small disk the fraction is below the floor, so the floor rules.
			name:    "a small disk is held to the 2 GiB floor",
			payload: 7 * gb, free: 8 * gb, total: 10 * gb,
			wantRefuse: true, wantAfter: 1 * gb,
		},
		{
			name:    "the same small disk with room to spare",
			payload: 5 * gb, free: 8 * gb, total: 10 * gb,
			wantRefuse: false, wantAfter: 3 * gb,
		},
		{
			// Unknown must never refuse: no comparison was made, and blocking on
			// one that did not happen is worse than the risk it guards.
			name:    "an unknown payload is not a refusal",
			payload: 0, free: 1 * gb, total: 100 * gb,
			wantRefuse: false, wantAfter: 1 * gb,
		},
		{
			name:    "an unreadable filesystem is not a refusal",
			payload: 500 * gb, free: 0, total: 0,
			wantRefuse: false, wantAfter: -500 * gb,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluateRestoreCapacity(tc.payload,
				dockercli.FSUsage{FreeBytes: tc.free, TotalBytes: tc.total, MountPoint: "/"},
				"/opt/docker/immich", false)
			if got.Refuse != tc.wantRefuse {
				t.Errorf("refuse = %v, want %v (after %s, margin %s)",
					got.Refuse, tc.wantRefuse, humanBytes(got.AfterBytes), humanBytes(got.MarginBytes))
			}
			if got.AfterBytes != tc.wantAfter {
				t.Errorf("after = %s, want %s", humanBytes(got.AfterBytes), humanBytes(tc.wantAfter))
			}
		})
	}
}

// A refusal has to carry every number the decision was made from, or the
// operator cannot tell whether to free space, remap, or pick another snapshot.
func TestRestoreCapacityRefusalNamesTheNumbers(t *testing.T) {
	c := EvaluateRestoreCapacity(70*gb,
		dockercli.FSUsage{FreeBytes: 74 * gb, TotalBytes: 98 * gb, MountPoint: "/"},
		"/home/user/docker/immich", false)
	if !c.Refuse {
		t.Fatal("this shape must refuse")
	}
	msg := c.RefusalMessage("immich")
	for _, want := range []string{"immich", "/home/user/docker/immich", "70.0 GB", "74.0 GB", "remap"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal must mention %q: %s", want, msg)
		}
	}
}

func TestCapacityMarginTakesTheLargerOfFloorAndFraction(t *testing.T) {
	if got := capacityMargin(10 * gb); got != 2*gb {
		t.Errorf("small disk margin = %s, want the 2 GiB floor", humanBytes(got))
	}
	if got := capacityMargin(1000 * gb); got != 100*gb {
		t.Errorf("large disk margin = %s, want a tenth of it", humanBytes(got))
	}
}

// The path measured is where the data actually lands — the bind sources AFTER
// the remap, not where the backup came from.
func TestRestoreTargetPathFollowsTheRemap(t *testing.T) {
	man := &Manifest{MountedBinds: []VolumeRef{
		{Source: "/home/user/docker/immich/upload", Destination: "/usr/src/app/upload"},
		{Source: "/home/user/docker/immich/postgres", Destination: "/var/lib/postgresql/data"},
	}}
	if got := RestoreTargetPath(man, "", "", "/opt/docker"); got != "/home/user/docker/immich" {
		t.Errorf("target = %q, want the binds' common base", got)
	}
	if got := RestoreTargetPath(man, "/home/user/docker", "/mnt/data/docker", "/opt/docker"); got != "/mnt/data/docker/immich" {
		t.Errorf("remapped target = %q, want the SSD — measuring the old disk is the bug", got)
	}
	if got := RestoreTargetPath(&Manifest{}, "", "", "/opt/docker"); got != "/opt/docker" {
		t.Errorf("no binds recorded: target = %q, want the configured base", got)
	}
}

func TestRestorePayloadPrefersTheMeasuredSelection(t *testing.T) {
	if got, estimated := restorePayload(58*gb, 20*gb, 0.35); got != 58*gb || estimated {
		t.Errorf("measured = %s estimated=%v, want the recorded selection", humanBytes(got), estimated)
	}
	// Older archives have no selection recorded. stored/ratio, never stored*ratio:
	// multiplying under-states a compressible backup, and under-stating is the
	// direction that overflows a disk.
	got, estimated := restorePayload(0, 20*gb, 0.50)
	if got != 40*gb || !estimated {
		t.Errorf("fallback = %s estimated=%v, want ~%s marked as a guess", humanBytes(got), estimated, humanBytes(40*gb))
	}
	if got, _ := restorePayload(0, 7*gb, 0); got != int64(float64(7*gb)/estimatedCompressionRatio("")) {
		t.Errorf("with no learned ratio the preset applies, got %s", humanBytes(got))
	}
	// Nothing to go on must be zero-and-unknown, which never refuses.
	if got, estimated := restorePayload(0, 0, 0.7); got != 0 || !estimated {
		t.Errorf("unknown = %d/%v, want 0/true", got, estimated)
	}
}
