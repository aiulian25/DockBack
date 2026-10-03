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
	body := runFunctionBody(t, string(src))

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

// runFunctionBody returns the text of Engine.Run, so the assertion is about the
// capture path and not about wherever a helper happens to be defined in the file.
func runFunctionBody(t *testing.T, src string) string {
	t.Helper()
	const sig = "func (e *Engine) Run("
	start := strings.Index(src, sig)
	if start < 0 {
		t.Fatal("Engine.Run not found in engine.go")
	}
	// Up to the next top-level declaration.
	rest := src[start+len(sig):]
	if end := strings.Index(rest, "\nfunc "); end > 0 {
		return rest[:end]
	}
	return rest
}
