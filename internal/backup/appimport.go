package backup

import (
	"regexp"
	"strings"
)

// App-native import output classification (F153).
//
// The database restore path learned this lesson years ago: a zero exit code is
// not proof that anything was imported. psql happily continues past failed
// statements and exits 0, which is how a Paperless database once restored with
// 27 of its 72 primary keys and was reported as a success. The app-native path
// looked at the exit code alone and said "restore complete".
//
// What this is NOT is a gate, and the reason is worth writing down. Paperless's
// importer was measured against a real instance: asked to import into one that
// already holds the documents, it raises FileExistsError, prints a traceback and
// exits 1 — correctly, so the existing exit-code check already fails that
// restore. No case was found here of a crash hiding behind a clean exit.
//
// Building a heuristic read of another program's output into a restore FAILURE
// on that evidence would only ever turn working recoveries into refused ones.
// So this reports what it saw and lets the restore continue. If an importer ever
// does crash while exiting 0, the operator gets told instead of finding out from
// an empty application months later.
//
// The signals are narrow for the same reason: only unambiguous evidence that a
// runtime aborted counts. A line containing the word "error" does not; a Python
// traceback does.

// appCrashSignals are the line shapes that mean an interpreter or runtime
// aborted. Each is anchored at the start of a line and specific to one runtime's
// crash output, so ordinary progress text cannot match.
var appCrashSignals = []*regexp.Regexp{
	// Python — the traceback header, and the exception line that ends one.
	regexp.MustCompile(`^Traceback \(most recent call last\):`),
	regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*(Error|Exception): \S`),
	// Go.
	regexp.MustCompile(`^panic: \S`),
	regexp.MustCompile(`^fatal error: \S`),
	// PHP.
	regexp.MustCompile(`^PHP Fatal error`),
	// Ruby — "file.rb:12:in `method': message (SomeError)". The file:line:in
	// prefix is what makes this specific; the class name is not required to end
	// in Error, because Errno::ENOENT and friends do not.
	regexp.MustCompile(`^\S+:\d+:in .+ \([A-Za-z_][A-Za-z0-9_:]*\)$`),
	// Node.
	regexp.MustCompile(`^\s*throw (new )?[A-Za-z_]`),
}

// maxAppImportEvidence bounds how many matched lines are reported. The point is
// to name the failure, not to reproduce a stack trace in a backup log.
const maxAppImportEvidence = 5

// ClassifyAppImportOutput returns the lines proving an application's importer
// crashed, or nil when nothing in the output says so (F153).
//
// Pure, so the whole verdict is unit-testable without a container.
func ClassifyAppImportOutput(out string) []string {
	if strings.TrimSpace(out) == "" {
		return nil
	}
	var found []string
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		for _, re := range appCrashSignals {
			// Matched against the line as written for the anchored patterns, and
			// against the trimmed line so an importer that indents its output is
			// still caught.
			if re.MatchString(line) || re.MatchString(trimmed) {
				if len(found) < maxAppImportEvidence {
					found = append(found, trimmed)
				}
				break
			}
		}
	}
	return found
}
