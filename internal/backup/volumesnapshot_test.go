package backup

import (
	"testing"

	"dockback/internal/store"
)

// F208 — a standalone-volume restore takes the same rollback point a container
// restore has always taken.
//
// A volume restore is a destructive in-place overwrite: UntarToNamedVolume
// extracts over whatever is already in the volume, so every path the archive
// contains replaces the live file at that path. The container path has captured
// the current state first since PLAN §3.7; this path captured nothing, and the
// operator's "Snapshot current state" tick was discarded before it reached the
// engine.
//
// The decision is tested here rather than the untar, because the untar needs a
// daemon and the decision is where the data is lost.

func TestPlanVolumeSnapshotTakesOneBeforeAnOverwrite(t *testing.T) {
	if got := planVolumeSnapshot(true, false, true); got != volumeSnapshotTake {
		t.Fatalf("a volume with contents, about to be overwritten, must be snapshotted first (got %v)", got)
	}
}

// The operator's explicit opt-out is honoured — the dialog tells them in so many
// words that the overwrite will not be reversible, so overriding that would be
// its own kind of dishonesty.
func TestPlanVolumeSnapshotHonoursTheOptOut(t *testing.T) {
	if got := planVolumeSnapshot(false, false, true); got != volumeSnapshotSkipUnwanted {
		t.Errorf("snapshot=false must skip, got %v", got)
	}
	// …and an engine-internal rollback restore never snapshots: it is the undo,
	// and snapshotting the state being undone would loop.
	if got := planVolumeSnapshot(true, true, true); got != volumeSnapshotSkipUnwanted {
		t.Errorf("a rollback restore must skip, got %v", got)
	}
	if got := planVolumeSnapshot(false, true, true); got != volumeSnapshotSkipUnwanted {
		t.Errorf("both off must skip, got %v", got)
	}
}

// The primary use of a standalone-volume backup is restoring data whose volume
// is long gone. There is nothing to protect in that case, and — worse — a
// read-only bind to a missing volume makes Docker CREATE it, so an ungated
// snapshot would silently produce an EMPTY archive: a rollback point that passes
// verification and restores nothing.
func TestPlanVolumeSnapshotSkipsAVolumeThatIsNotThere(t *testing.T) {
	if got := planVolumeSnapshot(true, false, false); got != volumeSnapshotSkipAbsent {
		t.Fatalf("a missing volume has nothing to snapshot, got %v", got)
	}
	// The absent case is distinguishable from the opt-out case, because they are
	// reported to the operator differently — one says "nothing to overwrite", the
	// other is their own choice.
	if volumeSnapshotSkipAbsent == volumeSnapshotSkipUnwanted {
		t.Error("the two skip reasons must stay distinct")
	}
}

// The whole matrix in one place, so a future edit to the guard has to face every
// combination rather than the one the author was thinking about.
func TestPlanVolumeSnapshotMatrix(t *testing.T) {
	cases := []struct {
		name                            string
		wantSnap, isRollback, volExists bool
		want                            volumeSnapshotPlan
	}{
		{"asked, exists", true, false, true, volumeSnapshotTake},
		{"asked, absent", true, false, false, volumeSnapshotSkipAbsent},
		{"not asked, exists", false, false, true, volumeSnapshotSkipUnwanted},
		{"not asked, absent", false, false, false, volumeSnapshotSkipUnwanted},
		{"rollback, exists", true, true, true, volumeSnapshotSkipUnwanted},
		{"rollback, absent", true, true, false, volumeSnapshotSkipUnwanted},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := planVolumeSnapshot(c.wantSnap, c.isRollback, c.volExists); got != c.want {
				t.Errorf("planVolumeSnapshot(%v,%v,%v) = %v, want %v", c.wantSnap, c.isRollback, c.volExists, got, c.want)
			}
		})
	}
}

// The snapshot is labelled so retention gives it the separate budget machine-made
// protective snapshots get (F48) — otherwise every restore would permanently
// spend one of that volume's scheduled generations.
func TestPreRestoreSnapshotIsInTheAutoRetentionClass(t *testing.T) {
	if autoPreRestoreLabel == "" {
		t.Fatal("the pre-restore snapshot must carry a label")
	}
	// partitionAutoSnaps is the consumer, and it keys on the "auto:" prefix.
	// Newest-first, as SelectForRetention requires: two pre-restore snapshots
	// around one manual backup of the same volume.
	rows := []*store.Backup{
		{ID: "snap-new", Label: autoPreRestoreLabel},
		{ID: "manual", Label: ""},
		{ID: "snap-old", Label: autoPreRestoreLabel},
	}
	normal, keepAuto, pruneAuto := partitionAutoSnaps(rows, 1)
	if len(normal) != 1 {
		t.Fatalf("only the unlabelled backup belongs to the normal class, got %d", len(normal))
	}
	if len(keepAuto) != 1 || len(pruneAuto) != 1 {
		t.Fatalf("the two pre-restore snapshots share the auto budget (keep=1): keep=%d prune=%d", len(keepAuto), len(pruneAuto))
	}
	// The point of the label: the operator's own backup is NOT what gets pruned.
	if pruneAuto[0].Label != autoPreRestoreLabel {
		t.Errorf("the auto budget must prune a snapshot, not the manual backup (%q)", pruneAuto[0].Label)
	}
}

// The flags the snapshot is captured with. Each one exists for a reason that
// costs data if it is dropped, so they are pinned rather than left to a reading
// of the call site.
func TestPreRestoreVolumeSnapshotOptions(t *testing.T) {
	o := preRestoreVolumeSnapshotOptions("n1", "media-cache")

	if o.VolumeOnly != "media-cache" {
		t.Errorf("the snapshot must capture the volume being overwritten, got %q", o.VolumeOnly)
	}
	if o.NodeID != "n1" {
		t.Errorf("node = %q", o.NodeID)
	}
	if !o.DestinationsExplicit {
		t.Error("a rollback point is local-only — an offsite copy is not what this is for")
	}
	if len(o.Destinations) != 0 {
		t.Error("local-only means no destinations, not a selection")
	}
	if !o.ForceFull {
		t.Error("a rollback point must be self-contained, never a delta on an in-flight chain")
	}
	if o.Label != autoPreRestoreLabel {
		t.Errorf("label = %q, want the auto-class label", o.Label)
	}
	// The blocker this feature would otherwise have shipped.
	if !o.SkipRetention {
		t.Error("the snapshot must NOT trigger a retention sweep — its sweep can prune the archive being restored")
	}
}

// F208 regression: a pre-restore safety snapshot shares its TargetName with the
// backup being restored, so the sweep at the end of a verified backup would
// select that archive as a prune candidate. Under the defaults that is a real
// deletion — of the source, before the restore has read it.
func TestRetentionSweepWanted(t *testing.T) {
	ordinary := Options{}
	snapshot := Options{SkipRetention: true}

	if !ordinary.retentionSweepWanted("verified") {
		t.Error("an ordinary verified backup still sweeps — retention must keep working")
	}
	if snapshot.retentionSweepWanted("verified") {
		t.Fatal("a pre-restore safety snapshot must never sweep: its sweep can delete the archive being restored")
	}
	// An unverified backup is not a generation and never evicts one, whichever
	// kind of run it was.
	for _, v := range []string{"failed", "", "unverified"} {
		if ordinary.retentionSweepWanted(v) {
			t.Errorf("verified=%q must not sweep", v)
		}
		if snapshot.retentionSweepWanted(v) {
			t.Errorf("verified=%q must not sweep", v)
		}
	}
}
