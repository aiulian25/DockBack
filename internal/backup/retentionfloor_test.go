package backup

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"dockback/internal/storage"
	"dockback/internal/store"
)

// A backup whose capture succeeded and whose verification failed is still
// Status=success. Retention selected on that alone, so a corrupt run took a
// generation slot and aged out the last point anybody could recover from. These
// run the real sweeps over a real catalog, because the sweep is what deletes.

const (
	floorNode   = "n1"
	floorTarget = "app"
)

type floorRow struct {
	id       string
	verified string
	parent   string
	label    string
	pinned   bool
}

const (
	rowVerified   = "verified"
	rowFailed     = "failed"
	rowUnverified = "unverified"
)

// floorCatalog stores rows oldest first, so the last one given is the newest.
func floorCatalog(t *testing.T, rows ...floorRow) *Engine {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	local, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i, row := range rows {
		man, err := json.Marshal(Manifest{BackupID: row.id, TargetName: floorTarget, Parent: row.parent})
		if err != nil {
			t.Fatal(err)
		}
		b := &store.Backup{ID: row.id, NodeID: floorNode, TargetName: floorTarget, Status: "success", Verified: row.verified,
			Label: row.label, Pinned: row.pinned, CreatedAt: int64(1_700_000_000 + i*86_400), ManifestJSON: string(man), StorageKey: "k/" + row.id}
		if err := st.CreateBackup(b); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateBackup(b); err != nil {
			t.Fatal(err)
		}
		if row.label != "" {
			if err := st.SetBackupLabel(row.id, row.label); err != nil {
				t.Fatal(err)
			}
		}
		if row.pinned {
			if err := st.SetBackupPin(row.id, true); err != nil {
				t.Fatal(err)
			}
		}
	}
	return &Engine{Store: st, Storage: local, Log: func(string, string, string) {}}
}

func setGenerations(t *testing.T, e *Engine, n string) {
	t.Helper()
	if err := e.Store.SetSetting("retention.generations", n); err != nil {
		t.Fatal(err)
	}
}

func catalogIDs(t *testing.T, e *Engine) []string {
	t.Helper()
	all, err := e.Store.ListBackupsForTarget(floorNode, floorTarget, 100)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, b := range all {
		out = append(out, b.ID)
	}
	sort.Strings(out)
	return out
}

func expectCatalog(t *testing.T, e *Engine, want ...string) {
	t.Helper()
	sort.Strings(want)
	if got := catalogIDs(t, e); !slices.Equal(got, want) {
		t.Errorf("catalog holds %v, want %v", got, want)
	}
}

// The audit's reproduction: generations=1, an older verified full and a newer
// failed one. The recovery point has to survive, and so does the evidence.
func TestSweepKeepsTheVerifiedPointWhenTheNewestFailed(t *testing.T) {
	e := floorCatalog(t, floorRow{id: "good", verified: rowVerified}, floorRow{id: "corrupt", verified: rowFailed})
	setGenerations(t, e, "1")

	e.PruneAll(context.Background())

	expectCatalog(t, e, "good", "corrupt")
}

func TestPerTargetSweepKeepsTheVerifiedPointWhenTheNewestFailed(t *testing.T) {
	e := floorCatalog(t, floorRow{id: "good", verified: rowVerified}, floorRow{id: "corrupt", verified: rowFailed})

	e.enforceRetention(context.Background(), floorNode, floorTarget, RetentionConfig{Generations: 1})

	expectCatalog(t, e, "good", "corrupt")
}

// A run still being verified has no verdict, and must not evict the point it
// might yet turn out not to replace.
func TestSweepKeepsTheVerifiedPointWhileTheNewestIsPending(t *testing.T) {
	e := floorCatalog(t, floorRow{id: "good", verified: rowVerified}, floorRow{id: "pending", verified: rowUnverified})
	setGenerations(t, e, "1")

	e.PruneAll(context.Background())

	expectCatalog(t, e, "good", "pending")
}

// Failed runs do not spend the budget: three of them must not push out three
// verified generations, and only the newest stays as evidence.
func TestFailedRunsDoNotSpendTheGenerationBudget(t *testing.T) {
	e := floorCatalog(t,
		floorRow{id: "v1", verified: rowVerified}, floorRow{id: "v2", verified: rowVerified},
		floorRow{id: "f1", verified: rowFailed}, floorRow{id: "f2", verified: rowFailed}, floorRow{id: "f3", verified: rowFailed})
	setGenerations(t, e, "2")

	e.PruneAll(context.Background())

	expectCatalog(t, e, "v1", "v2", "f3")
}

// A verified run also ends the case for older failed ones.
func TestAnOlderFailedRunIsStillPruned(t *testing.T) {
	e := floorCatalog(t,
		floorRow{id: "v1", verified: rowVerified}, floorRow{id: "f1", verified: rowFailed}, floorRow{id: "v2", verified: rowVerified})
	setGenerations(t, e, "2")

	e.PruneAll(context.Background())

	expectCatalog(t, e, "v1", "v2")
}

// With no known-good point at all, a failed archive may be the only data left. It
// is not for a retention rule to decide which one that is.
func TestNothingIsPrunedWhenEveryRunFailed(t *testing.T) {
	e := floorCatalog(t, floorRow{id: "f1", verified: rowFailed}, floorRow{id: "f2", verified: rowFailed}, floorRow{id: "f3", verified: rowFailed})
	setGenerations(t, e, "1")

	e.PruneAll(context.Background())

	expectCatalog(t, e, "f1", "f2", "f3")
}

// A verified delta on a failed parent cannot be restored, so it is not the floor:
// the older complete chain is.
func TestTheFloorIsACompleteHealthyChain(t *testing.T) {
	e := floorCatalog(t,
		floorRow{id: "good-full", verified: rowVerified},
		floorRow{id: "bad-full", verified: rowFailed},
		floorRow{id: "delta", verified: rowVerified, parent: "bad-full"})
	setGenerations(t, e, "1")

	e.PruneAll(context.Background())

	expectCatalog(t, e, "good-full", "bad-full", "delta")
}

// The floor brings the chain it stands on.
func TestTheFloorKeepsItsAncestors(t *testing.T) {
	e := floorCatalog(t,
		floorRow{id: "full", verified: rowVerified},
		floorRow{id: "delta", verified: rowVerified, parent: "full"},
		floorRow{id: "corrupt", verified: rowFailed})
	setGenerations(t, e, "1")

	e.PruneAll(context.Background())

	expectCatalog(t, e, "full", "delta", "corrupt")
}

func TestAutoSnapshotBudgetCannotTakeTheFloor(t *testing.T) {
	e := floorCatalog(t,
		floorRow{id: "good-auto", verified: rowVerified, label: "auto:pre-change"},
		floorRow{id: "newer-auto", verified: rowUnverified, label: "auto:pre-change"})
	if err := e.Store.SetSetting("retention.autosnap_keep", "1"); err != nil {
		t.Fatal(err)
	}
	setGenerations(t, e, "0")

	e.PruneAll(context.Background())

	expectCatalog(t, e, "good-auto", "newer-auto")
}

func TestGFSRulesCannotTakeTheFloor(t *testing.T) {
	e := floorCatalog(t, floorRow{id: "good", verified: rowVerified}, floorRow{id: "corrupt", verified: rowFailed})
	setGenerations(t, e, "0")
	if err := e.Store.SetSetting("retention.daily", "1"); err != nil {
		t.Fatal(err)
	}

	e.PruneAll(context.Background())

	expectCatalog(t, e, "good", "corrupt")
}

func TestAContainerOverrideCannotTakeTheFloor(t *testing.T) {
	e := floorCatalog(t, floorRow{id: "good", verified: rowVerified}, floorRow{id: "corrupt", verified: rowFailed})
	setGenerations(t, e, "5")
	if err := e.Store.SetPolicyOverride(store.ContainerScope(floorNode, floorTarget), store.PolicyOverride{OverrideRetention: true, Generations: 1}); err != nil {
		t.Fatal(err)
	}

	e.PruneAll(context.Background())

	expectCatalog(t, e, "good", "corrupt")
}

// The floor adds nothing when a healthy point is already kept.
func TestAHealthyCatalogPrunesAsBefore(t *testing.T) {
	e := floorCatalog(t,
		floorRow{id: "v1", verified: rowVerified}, floorRow{id: "v2", verified: rowVerified}, floorRow{id: "v3", verified: rowVerified})
	setGenerations(t, e, "1")

	e.PruneAll(context.Background())

	expectCatalog(t, e, "v3")
}

func TestPinsAndHoldsStillApply(t *testing.T) {
	pinned := floorCatalog(t,
		floorRow{id: "pinned-failed", verified: rowFailed, pinned: true},
		floorRow{id: "v1", verified: rowVerified}, floorRow{id: "v2", verified: rowVerified})
	setGenerations(t, pinned, "1")
	pinned.PruneAll(context.Background())
	expectCatalog(t, pinned, "pinned-failed", "v2")

	held := floorCatalog(t, floorRow{id: "v1", verified: rowVerified}, floorRow{id: "v2", verified: rowVerified})
	setGenerations(t, held, "1")
	if err := held.Store.SetSetting(RetentionHoldKey(floorNode, floorTarget), "suspect|1"); err != nil {
		t.Fatal(err)
	}
	held.PruneAll(context.Background())
	expectCatalog(t, held, "v1", "v2")
}

// Adopted archives have no verdict but are readable history: the budget still
// applies, as it always did, and the absence of a known-good point is reported.
func TestUnverifiedHistoryStillAgesOutAndIsReported(t *testing.T) {
	rows := []floorRow{{id: "u1", verified: rowUnverified}, {id: "u2", verified: rowUnverified}}
	e := floorCatalog(t, rows...)
	setGenerations(t, e, "1")
	list, err := e.Store.ListBackupsForTarget(floorNode, floorTarget, 100)
	if err != nil {
		t.Fatal(err)
	}

	plan, err := e.PlanRetention(floorNode, floorTarget, list, RetentionConfig{Generations: 1})
	if err != nil {
		t.Fatal(err)
	}

	if !plan.NoKnownGood {
		t.Error("nothing here is known to be good, and the plan should say so")
	}
	if len(plan.Prune) != 1 || plan.Prune[0].ID != "u1" {
		t.Errorf("prune = %v, want the older one", plan.Prune)
	}
}

func TestThePlanSaysWhyAPointStays(t *testing.T) {
	e := floorCatalog(t,
		floorRow{id: "full", verified: rowVerified},
		floorRow{id: "delta", verified: rowVerified, parent: "full"},
		floorRow{id: "pending", verified: rowUnverified})
	list, err := e.Store.ListBackupsForTarget(floorNode, floorTarget, 100)
	if err != nil {
		t.Fatal(err)
	}

	plan, err := e.PlanRetention(floorNode, floorTarget, list, RetentionConfig{Generations: 1})
	if err != nil {
		t.Fatal(err)
	}

	if plan.Reasons["delta"] != RetainedLastHealthy || plan.Reasons["full"] != RetainedChain {
		t.Errorf("reasons = %v", plan.Reasons)
	}
	if len(plan.Prune) != 0 {
		t.Errorf("nothing should be pruned: %v", plan.Prune)
	}
}
