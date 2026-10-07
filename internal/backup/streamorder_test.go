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

	// The markers: the container introspection Run does for itself, and the call
	// that opens the volume stream.
	const (
		inspect = "cli.ContainerInspect(ctx, opts.ContainerID)"
		stream  = "e.captureVolumes(ctx, cli, opts, man, work, volDests, id, name, driftBefore)"
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
