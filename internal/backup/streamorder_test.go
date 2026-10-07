package backup

import (
	"os"
	"strings"
	"testing"
)

// R5 §6's takeaway, as a guard rather than a hope: "Complete all introspection
// before starting bulk transfer, and treat the source as API-unavailable while a
// transfer is in flight."
//
// The engine already does this — inspect, then image config, then mounts and
// findings, then the database dump, and only then the volume archive. Nothing
// enforced it, so a future metadata read added in the wrong place would not look
// wrong: it would work on every developer machine and time out after 120 seconds
// on the one estate where the proxy is a single haproxy under a 58 GB stream.
//
// Asserted by source order inside Run, because that IS the property. There is no
// runtime symptom to test for — the failure is a timeout on somebody else's
// hardware.
func TestIntrospectionPrecedesBulkTransfer(t *testing.T) {
	src, err := os.ReadFile("engine.go")
	if err != nil {
		t.Fatal(err)
	}
	body := functionBody(t, string(src), "func (e *Engine) Run(")

	// The markers: the container introspection Run does for itself, and the
	// first call that can open the volume stream — the live copy of a short
	// freeze, which runs before the whole-copy path below it.
	const (
		inspect = "cli.ContainerInspect(ctx, opts.ContainerID)"
		stream  = "e.copyLive(ctx, cli, opts.ContainerID, volDests, opts.excludeSubPaths(), work, id)"
	)
	at := func(needle string) int {
		i := strings.Index(body, needle)
		if i < 0 {
			t.Fatalf("marker not found in Run — this test needs updating alongside the code: %q", needle)
		}
		return i
	}
	if at(inspect) >= at(stream) {
		t.Fatal("Run inspects the container at or after it starts streaming volumes; " +
			"introspection must complete BEFORE the bulk transfer saturates the socket proxy (R5 §6)")
	}

	// Everything else Run reads about the container has to be above the stream
	// too. These are the reads that exist today; a new one added below the
	// transfer is exactly the regression this catches.
	for _, read := range []string{
		"dockercli.InspectImageConfig(", // what the image declares
		"e.reportTagDrift(",             // the registry peek
		"hostRequirementsOf(insp)",      // what this container needs of its host
		"e.dumpDatabaseWith(",           // the logical dump, which needs exec
		"e.detectSQLite(",               // the SQLite scan, a sidecar of its own
	} {
		if i := strings.Index(body, read); i >= 0 && i >= at(stream) {
			t.Errorf("%s happens after the bulk transfer begins — it will time out on a saturated proxy", read)
		}
	}
}

// The SQLite copies are the only part of capturing a database that needs the
// app held still. Turning them into snapshots — vacuum, integrity check, row
// counts, minutes on a large database — works on the copies alone, so it runs
// after the resume: inside the window it would only lengthen the freeze.
func TestSQLiteIsRecordedAfterTheAppResumes(t *testing.T) {
	engineSrc, err := os.ReadFile("engine.go")
	if err != nil {
		t.Fatal(err)
	}
	stackSrc, err := os.ReadFile("consistent.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, capturePath := range []struct{ name, body, resume string }{
		{"Run", functionBody(t, string(engineSrc), "func (e *Engine) Run("), "\n\t\tdoResume()\n"},
		{"BackupStackConsistent", functionBody(t, string(stackSrc), "func (e *Engine) BackupStackConsistent("), "\n\tresumeAll()\n\trunPost()\n"},
	} {
		capture := strings.Index(capturePath.body, "e.captureSQLite(")
		resume := strings.Index(capturePath.body, capturePath.resume)
		record := strings.Index(capturePath.body, "e.recordSQLite(")
		if capture < 0 || resume < 0 || record < 0 {
			t.Fatalf("%s: marker not found — this test needs updating alongside the code", capturePath.name)
		}
		if !(capture < resume && resume < record) {
			t.Errorf("%s must copy the databases inside the window and snapshot them after the app resumes", capturePath.name)
		}
	}
}

// A short freeze holds the application only for what changed during the live
// copy, so in both capture paths the bulk is copied before the hold begins,
// the second pass runs inside it, and the merge waits until the application
// runs again. The stack copies live before its pre-hooks, so an application a
// hook puts in maintenance mode stays there only for the window.
func TestShortFreezeCopiesTheBulkBeforeTheHold(t *testing.T) {
	engineSrc, err := os.ReadFile("engine.go")
	if err != nil {
		t.Fatal(err)
	}
	stackSrc, err := os.ReadFile("consistent.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, capturePath := range []struct {
		name, body string
		order      []string
	}{
		{"Run", functionBody(t, string(engineSrc), "func (e *Engine) Run("), []string{
			"e.detectSQLite(", "e.copyLive(", "e.hold(", "e.copyFrozen(", "e.captureSQLite(", "\n\t\tdoResume()\n", "e.recordShortFreeze(",
		}},
		{"BackupStackConsistent", functionBody(t, string(stackSrc), "func (e *Engine) BackupStackConsistent("), []string{
			"e.detectSQLite(", "e.copyLive(", `"pre")`, `case "pause":`, "e.copyFrozen(", "e.captureSQLite(", "\n\tresumeAll()\n\trunPost()\n", "sc.settleLiveCopy()",
		}},
	} {
		last := -1
		for _, marker := range capturePath.order {
			at := strings.Index(capturePath.body, marker)
			if at < 0 {
				t.Fatalf("%s: marker %q not found — this test needs updating alongside the code", capturePath.name, marker)
			}
			if at < last {
				t.Errorf("%s: %q comes too early — the order is %q", capturePath.name, marker, capturePath.order)
			}
			last = at
		}
	}
}

// functionBody returns the text of the function declared by sig, so an
// assertion is about that capture path and not about wherever a helper happens
// to be defined in the file.
func functionBody(t *testing.T, src, sig string) string {
	t.Helper()
	start := strings.Index(src, sig)
	if start < 0 {
		t.Fatalf("%s not found", sig)
	}
	// Up to the next top-level declaration.
	rest := src[start+len(sig):]
	if end := strings.Index(rest, "\nfunc "); end > 0 {
		return rest[:end]
	}
	return rest
}
