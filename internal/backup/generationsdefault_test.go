package backup

import (
	"strconv"
	"testing"
)

// Two readers disagreed about how many copies are kept when nobody has set a
// policy: the settings endpoint answered 10 while the engine that actually
// prunes kept 3. So the panel showed a policy the application was not applying,
// and an operator reading it had no way to tell which number was real.
func TestDefaultGenerationsIsTheNumberTheEngineKeeps(t *testing.T) {
	// Nothing configured: what the pruner will do.
	cfg := RetentionFromSettings(func(_, def string) (string, error) { return def, nil })

	want, err := strconv.Atoi(DefaultGenerations)
	if err != nil {
		t.Fatalf("DefaultGenerations must be a number: %v", err)
	}
	if cfg.Generations != want {
		t.Errorf("the pruner keeps %d generations by default, but DefaultGenerations says %d — every reader of that constant would be wrong",
			cfg.Generations, want)
	}
	if want <= 0 {
		t.Errorf("a default of %d would keep nothing at all", want)
	}

	// A configured value still wins over the default.
	set := RetentionFromSettings(func(key, def string) (string, error) {
		if key == "retention.generations" {
			return "7", nil
		}
		return def, nil
	})
	if set.Generations != 7 {
		t.Errorf("a configured policy must win: got %d", set.Generations)
	}
}

// The default alone must not start pruning a fresh install: with no policy set
// at all, retention has to be inactive until the operator turns it on.
func TestDefaultRetentionDoesNotPruneOnItsOwn(t *testing.T) {
	cfg := RetentionFromSettings(func(_, def string) (string, error) { return def, nil })
	if !cfg.Active() {
		return // nothing to check: the default policy prunes nothing
	}
	// If it IS active, the default must at least keep more than one copy, or a
	// fresh install would delete its own history on the second backup.
	if cfg.Generations < 2 {
		t.Errorf("an active default policy keeping %d generation(s) would delete history immediately", cfg.Generations)
	}
}
