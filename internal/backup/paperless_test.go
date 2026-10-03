package backup

import (
	"strings"
	"testing"
)

// Paperless-ngx: the Redis restore verdict, the export-directory cleanup guard,
// and the post-import verification hook (F150–F152).

// F150 — the case measured against a real Redis: 8 keys captured, 5 of them
// volatile, RDB loaded after the TTLs passed → 3 keys back. Under the old rule
// ("fewer than captured is a failure") that restore FAILED. It is a perfectly
// good backup of a task broker.
func TestRedisImportVerdictExpiredKeysAreNotLoss(t *testing.T) {
	exp := DumpExpect{Keys: 8, VolatileKeys: 5}
	fail, note := redisImportVerdict(exp, 3)
	if fail != nil {
		t.Fatalf("every key that could not expire came back — this must not fail: %v", fail)
	}
	if note == "" {
		t.Fatal("the numbers should still be explained rather than passing in silence")
	}
	for _, want := range []string{"3 of the 8", "expiry", "nothing was lost"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note should contain %q; got %q", want, note)
		}
	}
	// The old rule, for contrast: it would have refused this restore.
	if err := VerifyImportCounts("redis", exp, 3); err == nil {
		t.Error("guarding the regression: the generic rule DOES fail this, which is why redis has its own")
	}
}

// ...but a key that could not expire going missing is real loss, and still fails.
func TestRedisImportVerdictPersistentLossFails(t *testing.T) {
	// 8 keys, 5 volatile → 3 could not expire. Only 2 came back.
	fail, note := redisImportVerdict(DumpExpect{Keys: 8, VolatileKeys: 5}, 2)
	if fail == nil {
		t.Fatal("losing a key that cannot expire must fail the restore")
	}
	if note != "" {
		t.Error("a failure and a reassurance must not be produced together")
	}
	for _, want := range []string{"cannot have expired", "1 key(s) are genuinely missing"} {
		if !strings.Contains(fail.Error(), want) {
			t.Errorf("the failure should say %q; got %q", want, fail)
		}
	}
}

func TestRedisImportVerdictQuietWhenWhole(t *testing.T) {
	if fail, note := redisImportVerdict(DumpExpect{Keys: 8, VolatileKeys: 5}, 8); fail != nil || note != "" {
		t.Errorf("a complete restore says nothing: %v / %q", fail, note)
	}
	// More keys than captured — the container has been running and taking work.
	if fail, note := redisImportVerdict(DumpExpect{Keys: 8, VolatileKeys: 5}, 12); fail != nil || note != "" {
		t.Errorf("a target holding more is not a shortfall: %v / %q", fail, note)
	}
	// Nothing recorded, or a genuinely empty Redis: nothing to claim either way.
	if fail, note := redisImportVerdict(DumpExpect{}, 0); fail != nil || note != "" {
		t.Errorf("no recorded expectation means no verdict: %v / %q", fail, note)
	}
}

// A backup taken before the volatile count existed cannot make the split, so it
// falls back to the one claim that is still unambiguous.
func TestRedisImportVerdictLegacyBackup(t *testing.T) {
	// Empty where keys were captured: the snapshot did not load. Still a failure —
	// this is the silent case the check was written for.
	fail, _ := redisImportVerdict(DumpExpect{Keys: 8}, 0)
	if fail == nil {
		t.Fatal("an empty Redis where keys were captured must fail")
	}
	if !strings.Contains(fail.Error(), "did not load") {
		t.Errorf("the failure should name the cause; got %q", fail)
	}
	// Any other shortfall: reported, never failed. An unknown must not be
	// resolved by guessing in the direction that refuses a recovery.
	fail, note := redisImportVerdict(DumpExpect{Keys: 8}, 3)
	if fail != nil {
		t.Errorf("without a volatile count, a partial shortfall must not fail: %v", fail)
	}
	if !strings.Contains(note, "predates") {
		t.Errorf("the note should say why it cannot be judged precisely; got %q", note)
	}
}

// The count command must emit BOTH numbers, since the verdict is arithmetic on
// them. Shape-checked here; the parse itself is exercised below.
func TestRedisCountCmdEmitsVolatile(t *testing.T) {
	script := strings.Join(redisCountCmd(), " ")
	for _, want := range []string{"INFO keyspace", `echo "keys|$N"`, `echo "volatile|$V"`, "REDISCLI_AUTH"} {
		if !strings.Contains(script, want) {
			t.Errorf("the count command should contain %q", want)
		}
	}
	// The password is exported into the environment, never placed on argv where
	// it would show in the process list.
	if strings.Contains(script, "-a ") || strings.Contains(script, "--pass") {
		t.Error("the Redis password must never reach argv")
	}
}

func TestRedisCountParse(t *testing.T) {
	// Exactly what a real redis:7-alpine printed for 8 keys, 5 with a TTL.
	out := "keys|8\nvolatile|5\n"
	if n, ok := parseNameCount(out, "keys"); !ok || n != 8 {
		t.Errorf("keys: got %d/%v", n, ok)
	}
	if v, ok := parseNameCount(out, "volatile"); !ok || v != 5 {
		t.Errorf("volatile: got %d/%v", v, ok)
	}
	// A server with no volatile keys still prints the line, as zero.
	if v, ok := parseNameCount("keys|3\nvolatile|0\n", "volatile"); !ok || v != 0 {
		t.Errorf("zero volatile must parse as a recorded zero: got %d/%v", v, ok)
	}
}

// F151 — the export directory holds a plaintext copy of everything. Emptying it
// is worth doing and is exactly the kind of thing that must not go wrong, so the
// path is validated before anything is deleted.
func TestCleanableExportDir(t *testing.T) {
	ok := []struct{ in, want string }{
		{"/usr/src/paperless/export", "/usr/src/paperless/export"},
		{"/tmp/dockback-export", "/tmp/dockback-export"},
		{"/var/lib/app/export/", "/var/lib/app/export"},
	}
	for _, c := range ok {
		got, err := cleanableExportDir(c.in)
		if err != nil || got != c.want {
			t.Errorf("%q should be cleanable as %q; got %q / %v", c.in, c.want, got, err)
		}
	}

	// Every one of these must be refused rather than repaired.
	bad := []string{
		"", "   ", "relative/path",
		"/", "/data", "/export", "/var", // a top-level directory is not an export dir
		"/usr/src/paperless/export/*",       // a glob makes the target something else
		"/usr/src/../../etc",                // traversal
		"/usr/src/paperless/export/../../.", // traversal that cleans to a shallow path
	}
	for _, in := range bad {
		if got, err := cleanableExportDir(in); err == nil {
			t.Errorf("%q must be refused, got %q", in, got)
		}
	}
}

// F152 — the profile carries the check, and it survives the round trip through
// the shareable saved form.
func TestExportProfileVerifyCmdRoundTrip(t *testing.T) {
	p := ExportProfile{
		Dir:       "/export",
		ExportCmd: []string{"/bin/sh", "-c", "do-export"},
		ImportCmd: []string{"/bin/sh", "-c", "do-import"},
		VerifyCmd: []string{"/bin/sh", "-c", "do-verify"},
	}
	saved := p.Saved()
	if saved.VerifyCmd != "do-verify" {
		t.Errorf("the verify line must survive being saved: %q", saved.VerifyCmd)
	}
	// A preset carries it too, since presets are how a recipe is shared.
	prof := ExportPreset{Name: "x", Dir: "/export", ExportCmd: "e", ImportCmd: "i", VerifyCmd: "v"}.Profile()
	if len(prof.VerifyCmd) != 3 || prof.VerifyCmd[2] != "v" {
		t.Errorf("a preset's verify command must reach the profile: %v", prof.VerifyCmd)
	}
	// And a preset without one produces no command at all, rather than an empty
	// shell invocation that would "succeed" and look like a check.
	if got := (ExportPreset{Name: "x", Dir: "/e", ExportCmd: "e", ImportCmd: "i"}).Profile(); len(got.VerifyCmd) != 0 {
		t.Errorf("an unset verify command must stay unset, got %v", got.VerifyCmd)
	}
}

// The Paperless preset: re-validated against the current image, and deliberately
// carrying no verify command.
func TestPaperlessPreset(t *testing.T) {
	p := builtinExportProfile("ghcr.io/paperless-ngx/paperless-ngx:latest")
	if p.Tool != "paperless" {
		t.Fatalf("the Paperless image must match its preset, got %q", p.Tool)
	}
	if strings.Join(p.ExportCmd, " ") != "document_exporter /usr/src/paperless/export --no-progress-bar" {
		t.Errorf("export command drifted: %v", p.ExportCmd)
	}
	if strings.Join(p.ImportCmd, " ") != "document_importer /usr/src/paperless/export --no-progress-bar" {
		t.Errorf("import command drifted: %v", p.ImportCmd)
	}
	// document_sanity_checker ALWAYS exits 0 — it renders a table and returns.
	// Wiring it in would be a check that cannot fail while reading like proof.
	if len(p.VerifyCmd) != 0 {
		t.Errorf("Paperless must ship no verify command: %v", p.VerifyCmd)
	}
}

// F153 — the crash classifier, fed the real importer's real output.
func TestClassifyAppImportOutput(t *testing.T) {
	// Copied verbatim from a live Paperless importer refusing a non-empty
	// instance. It also exits 1, so this is a diagnostic rather than the gate.
	real := `Found existing documents(s), this might indicate a non-empty installation
Checking the manifest
Copy files into paperless...
Traceback (most recent call last):
  File "/usr/src/paperless/src/manage.py", line 10, in <module>
    execute_from_command_line(sys.argv)
  File "/usr/src/paperless/src/documents/management/commands/document_importer.py", line 624, in _import_files_from_manifest
    raise FileExistsError(document.source_path)
FileExistsError: /usr/src/paperless/media/documents/originals/0000001.txt`
	got := ClassifyAppImportOutput(real)
	if len(got) != 2 {
		t.Fatalf("the traceback header and its exception line should both be caught, got %v", got)
	}
	if !strings.HasPrefix(got[0], "Traceback") || !strings.HasPrefix(got[1], "FileExistsError:") {
		t.Errorf("unexpected evidence: %v", got)
	}

	// The SUCCESSFUL run of the same importer, also verbatim. Not a single line
	// of it may be mistaken for a crash — a false positive here would put a
	// warning on every good restore, which is how warnings stop being read.
	clean := `Found existing user(s), this might indicate a non-empty installation
Checking the manifest
Copy files into paperless...
Updating search index...`
	if got := ClassifyAppImportOutput(clean); len(got) != 0 {
		t.Errorf("a successful import must be silent, got %v", got)
	}
}

func TestClassifyAppImportOutputOtherRuntimes(t *testing.T) {
	crashes := map[string]string{
		"go":     "starting import\npanic: runtime error: index out of range",
		"php":    "PHP Fatal error:  Uncaught Error: Class not found",
		"ruby":   "importer.rb:12:in `load': no such file (Errno::ENOENT)",
		"node":   "  throw new Error('bad manifest')",
		"golow":  "fatal error: concurrent map writes",
		"python": "ValueError: manifest is empty",
	}
	for name, out := range crashes {
		if got := ClassifyAppImportOutput(out); len(got) == 0 {
			t.Errorf("%s crash not detected: %q", name, out)
		}
	}

	// Ordinary importer chatter that merely mentions failure must NOT match — the
	// bar is evidence that a runtime aborted, not the word "error".
	benign := []string{
		"", "   \n\n",
		"Importing documents...",
		"0 errors, 12 documents imported",
		"WARNING: skipping one file (error reading metadata)",
		"error_count=0",
		"Done. See the log for any errors.",
	}
	for _, out := range benign {
		if got := ClassifyAppImportOutput(out); len(got) != 0 {
			t.Errorf("benign output flagged as a crash: %q -> %v", out, got)
		}
	}
}

// The evidence is bounded: naming the failure, not reproducing a stack trace in
// a backup log.
func TestClassifyAppImportOutputBounded(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString("panic: boom\n")
	}
	if got := ClassifyAppImportOutput(b.String()); len(got) != maxAppImportEvidence {
		t.Errorf("evidence should be capped at %d, got %d", maxAppImportEvidence, len(got))
	}
}
