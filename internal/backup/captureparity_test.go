package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dockback/internal/store"
)

// The two capture paths drifted because each was a copy of the other. These pin
// that they now share the steps a saved per-service choice depends on.
func TestBothCapturePathsShareTheCapabilitySteps(t *testing.T) {
	engineSrc, err := os.ReadFile("engine.go")
	if err != nil {
		t.Fatal(err)
	}
	stackSrc, err := os.ReadFile("consistent.go")
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{
		"Run":                   functionBody(t, string(engineSrc), "func (e *Engine) Run("),
		"prepareServiceCapture": functionBody(t, string(stackSrc), "func (e *Engine) prepareServiceCapture("),
	}
	shared := []string{"e.resolveDatabaseCapture(", "e.resolveExclusions(", "e.bundleImage("}
	for name, body := range paths {
		for _, step := range shared {
			if !strings.Contains(body, step) {
				t.Errorf("%s does not use %s", name, step)
			}
		}
		if strings.Contains(body, "detectDBEngine(") {
			t.Errorf("%s detects its database itself again, which is how embedded servers were missed", name)
		}
	}
	if !strings.Contains(string(stackSrc), "e.indexWholeCapture(") || !strings.Contains(string(stackSrc), "e.recordShortFreeze(") {
		t.Error("a group snapshot must index both ways it copies volumes")
	}
	if !strings.Contains(paths["prepareServiceCapture"], "e.recordCaptureOverrides(") {
		t.Error("what a group snapshot cannot honour must be said before the window")
	}
}

func TestConsistentCaptureOverrides(t *testing.T) {
	cases := []struct {
		name        string
		appExport   bool
		tool        string
		incremental bool
		want        []string
	}{
		{"nothing saved", false, "", false, nil},
		{"export saved and available", true, "pg_dump", false, []string{"app-native export"}},
		{"export saved but none configured", true, "", false, nil},
		{"incremental saved", false, "", true, []string{"incremental"}},
		{"both", true, "occ", true, []string{"app-native export", "incremental"}},
	}
	for _, c := range cases {
		var got []string
		for _, o := range consistentCaptureOverrides(c.appExport, c.tool, c.incremental) {
			got = append(got, o.choice)
			if o.why == "" {
				t.Errorf("%s: %s has no reason", c.name, o.choice)
			}
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAnOverrideIsRecordedOnTheArchive(t *testing.T) {
	e := &Engine{Log: func(string, string, string) {}}
	man := &Manifest{}
	e.recordCaptureOverrides(man, "b1", consistentCaptureOverrides(true, "pg_dump", true))
	if len(man.Findings) != 2 {
		t.Fatalf("%+v", man.Findings)
	}
	for _, f := range man.Findings {
		if f.Code != findingCaptureOverridden || !strings.Contains(f.Message, "not applied") {
			t.Errorf("%+v", f)
		}
	}
}

// F132/F138 decide what the copy leaves out; a group snapshot ignored both.
func TestExclusionsResolveTheSameForEitherPath(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &Engine{Store: st, Log: func(string, string, string) {}}
	opts := Options{NodeID: "n1"}
	man := &Manifest{}
	e.resolveExclusions(&opts, man, "ghcr.io/mealie-recipes/mealie:latest", "mealie", "b1")
	excluded := strings.Join(opts.excludeSubPaths(), ",")
	if !strings.Contains(excluded, "/app/data/mealie.log*") || !strings.Contains(excluded, "/app/data/.temp") {
		t.Fatalf("the paths with no restore value were not left out: %s", excluded)
	}
	if len(man.ExcludedRegenerable) != 2 {
		t.Fatalf("the archive must say what it left out: %+v", man.ExcludedRegenerable)
	}
}
