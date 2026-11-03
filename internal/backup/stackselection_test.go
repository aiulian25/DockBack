package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"dockback/internal/crypto"
	"dockback/internal/storage"
	"dockback/internal/store"
)

// F213 — restore some of a stack's services, not all of it.
//
// A stack restore that stopped part-way leaves some services back and some not,
// and putting the remaining ones right meant hunting each of their individual
// backups on the Backups page one at a time. The set narrows; the dependency
// order among what is kept does not change.

func stackSet(names ...string) map[string]*stackService {
	out := map[string]*stackService{}
	for _, n := range names {
		out[n] = &stackService{
			service: n,
			man:     &Manifest{Service: n, TargetName: n},
			backup:  &store.Backup{ID: "b-" + n, TargetName: n, Status: "success"},
		}
	}
	return out
}

// AC1 — only the named services survive the filter; everything else is left
// alone, which on this path means genuinely untouched.
func TestFilterStackServicesKeepsOnlyTheSelection(t *testing.T) {
	all := stackSet("db", "cache", "app", "worker")

	got, err := filterStackServices(all, []string{"db"})
	if err != nil {
		t.Fatalf("selecting one service is valid: %v", err)
	}
	if len(got) != 1 || got["db"] == nil {
		t.Fatalf("want just db, got %v", selectedNames(got))
	}

	got, err = filterStackServices(all, []string{"db", "app"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["db"] == nil || got["app"] == nil {
		t.Errorf("want db and app, got %v", selectedNames(got))
	}
	// The source map is not mutated — the caller may still need the full set.
	if len(all) != 4 {
		t.Errorf("filtering must not modify the stack's own selection: %v", selectedNames(all))
	}
	// Whitespace and blanks are tolerated, not treated as service names.
	if got, err := filterStackServices(all, []string{" db ", "", "  "}); err != nil || len(got) != 1 {
		t.Errorf("a padded name is still that name: %v / %v", selectedNames(got), err)
	}
}

// A name the stack does not have is an ERROR, not a silent omission: "restore db
// and cache" that quietly restores only db is how somebody comes to believe a
// service came back when it never did.
func TestFilterStackServicesRefusesUnknownNames(t *testing.T) {
	all := stackSet("db", "app")

	_, err := filterStackServices(all, []string{"db", "redis"})
	if err == nil {
		t.Fatal("an unknown service must be refused, not dropped")
	}
	if !strings.Contains(err.Error(), "redis") {
		t.Errorf("the refusal must name what was not found: %v", err)
	}
	// …and say what IS there, so the operator can correct it without guessing.
	for _, known := range []string{"db", "app"} {
		if !strings.Contains(err.Error(), known) {
			t.Errorf("the refusal should list the real services (%q): %v", known, err)
		}
	}
	// Selecting nothing at all is refused rather than silently restoring
	// everything — the opposite of what was asked.
	if _, err := filterStackServices(all, []string{"", "   "}); err == nil {
		t.Error("an empty selection must not fall through to the whole stack")
	}
}

// AC2 — a member of an application whose services are only meaningful together
// cannot be restored on its own. The guard that already knows which subsets are
// meaningless does the refusing, with its own wording.
func TestAtomicApplicationRefusesASubset(t *testing.T) {
	atomic := &StackAtomicRef{
		Why:     "Immich splits one library across two services",
		Symptom: "photos resolve to rows that no longer exist",
		Members: []StackMember{{Service: "immich-server"}, {Service: "immich-db"}},
	}
	full := map[string]*Manifest{
		"immich-server": {Service: "immich-server", StackAtomic: atomic, ConsistencyGroup: "cg-1"},
		"immich-db":     {Service: "immich-db", StackAtomic: atomic, ConsistencyGroup: "cg-1"},
	}
	if err := StackAtomicGroupVerdict(full); err != nil {
		t.Fatalf("the complete set is restorable: %v", err)
	}

	// Deselect one half — exactly what the checkbox would do.
	subset := map[string]*Manifest{"immich-server": full["immich-server"]}
	err := StackAtomicGroupVerdict(subset)
	if err == nil {
		t.Fatal("restoring half of an atomic application must be refused")
	}
	if !strings.Contains(err.Error(), "immich-db") {
		t.Errorf("the refusal must name what is missing: %v", err)
	}
	if !strings.Contains(err.Error(), "nothing has been changed") {
		t.Errorf("and make clear the refusal cost nothing: %v", err)
	}
}

// AC3 — no selection means the whole stack, byte-for-byte as before.
func TestNoSelectionRestoresEverything(t *testing.T) {
	// The filter is only reached when Services is non-empty; an empty slice must
	// therefore never narrow anything.
	var opts StackRestoreOptions
	if len(opts.Services) != 0 {
		t.Fatal("the zero value must mean the whole stack")
	}
	// And a caller that passes an explicitly empty slice is the same thing.
	opts.Services = []string{}
	if len(opts.Services) != 0 {
		t.Error("an empty selection is not a selection")
	}
}

// The plan marks members of an atomic application, so the dialog can disable
// their checkboxes instead of letting the operator discover the refusal after
// confirming something destructive.
func TestStackPlanMarksAtomicMembers(t *testing.T) {
	atomic := StackAtomicRef{
		Why:     "these services share one library",
		Symptom: "it breaks",
		Members: []StackMember{{Service: "immich-server"}, {Service: "immich-db"}},
	}
	atomicMan, _ := json.Marshal(Manifest{Service: "immich-db", TargetName: "immich-db", StackAtomic: &atomic})
	plainMan, _ := json.Marshal(Manifest{Service: "caddy", TargetName: "caddy"})
	rows := []*store.Backup{
		{ID: "b-db", Stack: "photos", TargetName: "immich-db", Status: "success", CreatedAt: 200, ManifestJSON: string(atomicMan)},
		{ID: "b-caddy", Stack: "photos", TargetName: "caddy", Status: "success", CreatedAt: 100, ManifestJSON: string(plainMan)},
	}

	entries, _, err := planStackFrom(rows, "photos", "")
	if err != nil {
		t.Fatal(err)
	}
	byService := map[string]StackPlanEntry{}
	for _, e := range entries {
		byService[e.Service] = e
	}
	if !byService["immich-db"].Atomic {
		t.Error("a member of an atomic application must be flagged so its checkbox can be disabled")
	}
	if byService["caddy"].Atomic {
		t.Error("an ordinary member must be deselectable")
	}
	// omitempty: an ordinary row serializes exactly as it did before this feature.
	js, _ := json.Marshal(byService["caddy"])
	if strings.Contains(string(js), "atomic") {
		t.Errorf("an ordinary plan row must not gain a field: %s", js)
	}
}

func selectedNames(m map[string]*stackService) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// F214 — every member of a stack restore reads from the SAME chosen copy.
//
// The picker existed on a single-backup restore and not here, which had it
// backwards: a stack is the case where the choice matters most. When the local
// disk is the thing you distrust — ransomware, a bad controller, a host being
// rebuilt — "read every service from the offsite copy" is the whole request, and
// auto would quietly prefer local for any member that still had one.
func TestStackSourceReachesEveryMember(t *testing.T) {
	member := &stackService{
		service: "db",
		man:     &Manifest{Service: "db", TargetName: "db", ContainerID: "cid-db"},
		backup:  &store.Backup{ID: "b-db", TargetName: "db", Status: "success"},
	}

	got := stackServiceRestoreOptions(member, "n1", StackRestoreOptions{Source: "dest-nas"})
	if got.Source != "dest-nas" {
		t.Errorf("the chosen copy must reach the per-service restore, got %q", got.Source)
	}

	// AC3 — omitted means auto, exactly as before this existed.
	if o := stackServiceRestoreOptions(member, "n1", StackRestoreOptions{}); o.Source != "" {
		t.Errorf("no choice means auto, got %q", o.Source)
	}
	// "local" is a real choice and must survive as one, not be normalised away.
	if o := stackServiceRestoreOptions(member, "n1", StackRestoreOptions{Source: "local"}); o.Source != "local" {
		t.Errorf(`"local" must reach the member verbatim, got %q`, o.Source)
	}
}

// A chosen copy is tried first and is not a pin: the restore goes on without it.
// Somebody who chose "not this disk" and was read from it anyway has to be told,
// and in the stack's console, because that is the log they are watching.
func TestReadingAnotherCopyThanTheChosenOneWarns(t *testing.T) {
	const stackLog = "stack:blog"
	be, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	payload := []byte("ciphertext")
	if _, err := be.Put(ctx, "k", bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	sum, err := crypto.CipherSHA256(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	b := &store.Backup{ID: "b1", TargetName: "blog-db", StorageKey: "k", CipherSHA256: sum,
		LocationsJSON: `[{"kind":"local","name":"local","type":"local"}]`}

	warningsReading := func(source string, ctx context.Context) map[string]string {
		warnings := map[string]string{}
		e := &Engine{Storage: be, Log: func(id, level, msg string) {
			if level == "WARN" {
				warnings[id] = msg
			}
		}}
		rc, err := e.openVerified(ctx, b, source)
		if err != nil {
			t.Fatalf("source %q: the read must go on from the copy there is: %v", source, err)
		}
		rc.Close()
		return warnings
	}

	inStack := context.WithValue(ctx, stackRunLogKey{}, stackLog)
	got := warningsReading("dest-nas", inStack)
	for _, id := range []string{b.ID, stackLog} {
		if !strings.Contains(got[id], "local") || !strings.Contains(got[id], b.TargetName) {
			t.Errorf("log %s must name the service and the copy read instead: %q", id, got[id])
		}
	}
	if got := warningsReading("dest-nas", ctx); got[b.ID] == "" || len(got) != 1 {
		t.Errorf("outside a stack restore only the backup's own log has it: %v", got)
	}
	// What was asked for, or nothing asked for at all, is nothing to warn about.
	for _, source := range []string{"local", ""} {
		if got := warningsReading(source, inStack); len(got) != 0 {
			t.Errorf("source %q was honoured, so nothing should warn: %v", source, got)
		}
	}
}
