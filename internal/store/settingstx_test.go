package store

import (
	"path/filepath"
	"testing"
)

// A retention policy is one decision spread over eleven settings rows. Written
// one at a time, a crash or a database error partway through leaves a policy
// nobody chose — new keep counts against an old schedule, or a prune schedule
// with no baseline, which fires on the very next tick instead of its window.

func settingsStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestSetSettingsWritesEveryValue(t *testing.T) {
	st := settingsStore(t)
	want := map[string]string{"a": "1", "b": "2", "c": "3"}
	if err := st.SetSettings(want); err != nil {
		t.Fatal(err)
	}
	for k, v := range want {
		got, err := st.GetSetting(k, "")
		if err != nil {
			t.Fatal(err)
		}
		if got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}

	// It overwrites, like SetSetting does.
	if err := st.SetSettings(map[string]string{"a": "9"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetSetting("a", ""); got != "9" {
		t.Errorf("a = %q after rewrite, want 9", got)
	}
}

// The whole point: one failing write must undo the others.
func TestSetSettingsRollsBackEveryValueOnFailure(t *testing.T) {
	st := settingsStore(t)
	if err := st.SetSettings(map[string]string{"a": "old", "b": "old", "poison": "old"}); err != nil {
		t.Fatal(err)
	}

	// Make one specific row refuse to be written, the way a disk or constraint
	// error would partway through the batch.
	if _, err := st.db.Exec(`CREATE TRIGGER refuse BEFORE INSERT ON settings
		WHEN NEW.key = 'poison' BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}

	err := st.SetSettings(map[string]string{"a": "new", "b": "new", "poison": "new"})
	if err == nil {
		t.Fatal("a failing write must be reported, not swallowed")
	}
	for _, k := range []string{"a", "b"} {
		got, _ := st.GetSetting(k, "")
		if got != "old" {
			t.Errorf("%s = %q — a partial save survived; the batch is not atomic", k, got)
		}
	}
}

func TestSetSettingsOnAnEmptyMap(t *testing.T) {
	if err := settingsStore(t).SetSettings(nil); err != nil {
		t.Errorf("an empty batch is a no-op, not an error: %v", err)
	}
}
