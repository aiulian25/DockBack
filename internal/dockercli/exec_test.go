package dockercli

import (
	"strings"
	"testing"
)

// SEC-8: single quotes in a path must be escaped ('\”) for a '...' shell literal,
// not stripped — so a mount path containing a quote is measured correctly.
func TestShellEscape(t *testing.T) {
	cases := map[string]string{
		"/data/app":      "/data/app", // no quote: unchanged
		"/mnt/it's mine": `/mnt/it'\''s mine`,
		"'":              `'\''`,
		"a'b'c":          `a'\''b'\''c`,
	}
	for in, want := range cases {
		if got := shellEscape(in); got != want {
			t.Errorf("shellEscape(%q) = %q, want %q", in, got, want)
		}
	}
	// The escaped form, wrapped in single quotes, must contain no unescaped quote
	// break — i.e. every "'" is part of the '\'' sequence.
	wrapped := "'" + shellEscape("x'y") + "'"
	if wrapped != `'x'\''y'` {
		t.Errorf("wrapped = %q, want %q", wrapped, `'x'\''y'`)
	}
}

// SEC-5: the executable-name guard must accept every real dump/restore tool name
// and reject anything carrying shell metacharacters, whitespace, or path parts —
// so HasExecutable can never concatenate injectable input into a shell.
func TestSafeExecName(t *testing.T) {
	valid := []string{"pg_dumpall", "pg_dump", "mysqldump", "mariadb-dump", "mongodump", "psql", "mysql", "redis-cli"}
	for _, s := range valid {
		if !safeExecName.MatchString(s) {
			t.Errorf("real tool name %q should be accepted", s)
		}
	}
	invalid := []string{
		"", "pg_dump;rm -rf /", "pg_dump && id", "$(id)", "`id`", "a|b",
		"pg dump", "/usr/bin/pg_dump", "../pg_dump", "PG_DUMP", "pg_dump\n", "tool.sh",
	}
	for _, s := range invalid {
		if safeExecName.MatchString(s) {
			t.Errorf("unsafe/invalid name %q must be rejected", s)
		}
	}
}

// F11: the pre-restore readiness verdict must explain WHY an image is unavailable
// (distinguishing "no reference recorded" from "registry says gone"), so the UI
// never shows a bare or misleading "unavailable".
func TestImageUnavailableDetail(t *testing.T) {
	if got := imageUnavailableDetail(false, ""); !strings.Contains(got, "no image reference") {
		t.Errorf("no-reference case should say so, got %q", got)
	}
	got := imageUnavailableDetail(true, "manifest unknown")
	if !strings.Contains(got, "could not be resolved in a registry") {
		t.Errorf("registry-miss case should name the registry, got %q", got)
	}
	if !strings.Contains(got, "manifest unknown") {
		t.Errorf("registry error should be surfaced for troubleshooting, got %q", got)
	}
	// No leaked underlying error when there is none to report.
	if strings.Contains(imageUnavailableDetail(true, ""), "()") {
		t.Errorf("empty registry error must not render empty parens")
	}
}

// The mount probe's output is the only thing standing between a stat on the
// source machine and a path a restore will CREATE on another one, so every shape
// it can produce is pinned here. The rule throughout: a field that does not parse
// is left empty, never defaulted — an invented kind or owner is the failure this
// data exists to prevent.
func TestParseMountProbe(t *testing.T) {
	out := strings.Join([]string{
		// A whole, well-formed reading.
		"KIND|dir|101:104|700|/var/lib/postgresql/data",
		"FS|/var/lib/postgresql/data|ext2/ext3",
		"65536|/var/lib/postgresql/data",
		// A file bind, which is the case Docker gets wrong on its own.
		"KIND|file|1026:100|600|/run/secrets/vapid_private_key",
		// Symbolic owner: stat without numeric output. Kind and mode still stand.
		"KIND|dir|root:root|755|/app/uploads",
		// Mode unreadable, owner fine.
		"KIND|dir|1000:1000||/app/volumes/logs",
		// Not a kind we know; the rest of the line is still good.
		"KIND|socket|1000:1000|660|/app/volumes/sock",
		// A path containing the field separator must survive whole.
		"KIND|dir|1000:1000|755|/data/a|b",
		// Structurally short — no verdict at all.
		"KIND|dir|1000:1000",
		"ERR|/app/volumes/backups",
		"",
	}, "\n")

	sizes, unreadable, stats := parseMountProbe(out)

	for _, c := range []struct{ path, kind, owner, mode string }{
		{"/var/lib/postgresql/data", MountKindDir, "101:104", "700"},
		{"/run/secrets/vapid_private_key", MountKindFile, "1026:100", "600"},
		{"/app/uploads", MountKindDir, "", "755"},            // symbolic owner dropped
		{"/app/volumes/logs", MountKindDir, "1000:1000", ""}, // mode dropped
		{"/app/volumes/sock", "", "1000:1000", "660"},        // unknown kind dropped
		{"/data/a|b", MountKindDir, "1000:1000", "755"},      // separator in the path
	} {
		got := stats[c.path]
		if got.Kind != c.kind || got.Owner != c.owner || got.Mode != c.mode {
			t.Errorf("%s: got kind=%q owner=%q mode=%q, want %q/%q/%q",
				c.path, got.Kind, got.Owner, got.Mode, c.kind, c.owner, c.mode)
		}
	}
	if st, ok := stats["/var/lib/postgresql/data"]; !ok || st.FSType != "ext2/ext3" {
		t.Errorf("the filesystem type must still be read alongside the new fields, got %+v", st)
	}
	if sizes["/var/lib/postgresql/data"] != 65536 {
		t.Errorf("sizes must still parse, got %d", sizes["/var/lib/postgresql/data"])
	}
	if len(unreadable) != 1 || unreadable[0] != "/app/volumes/backups" {
		t.Errorf("the unreadable set must still parse, got %v", unreadable)
	}
	// The short line names a path that must have earned no entry of any kind.
	if _, ok := stats["1000:1000"]; ok {
		t.Error("a structurally short KIND line must produce no verdict")
	}
}

// A path that does not exist must produce NO kind, rather than the "file" a bare
// `[ -d ]` test would fall through to. The guard is in the script, so it is
// asserted there.
func TestMountProbeScriptOnlyReadsPathsThatExist(t *testing.T) {
	script := mountProbeScript([]string{"/x"}, false)
	if !strings.Contains(script, `if [ -e "$p" ]; then`) {
		t.Errorf("the kind probe must be guarded on the path existing, got: %s", script)
	}
	if strings.Index(script, `if [ -e "$p" ]`) > strings.Index(script, "KIND|") {
		t.Error("the existence guard must come before the KIND line it protects")
	}
}

// The du walk is the expensive half and the whole reason the two probes are
// separable: identity is read on every backup, sizes are not.
func TestMountProbeScriptSeparatesSizesFromIdentity(t *testing.T) {
	withSizes := mountProbeScript([]string{"/x"}, true)
	statOnly := mountProbeScript([]string{"/x"}, false)
	if !strings.Contains(withSizes, "du -sb") || !strings.Contains(withSizes, "ERR|") {
		t.Error("the sizing probe must still measure and still flag unreadable paths")
	}
	if strings.Contains(statOnly, "du ") {
		t.Errorf("the stats-only probe must not walk directories, got: %s", statOnly)
	}
	for _, want := range []string{"KIND|", "FS|"} {
		if !strings.Contains(statOnly, want) {
			t.Errorf("the stats-only probe must still emit %s", want)
		}
	}
}

// A path with a quote or a space in it has to reach `stat` intact, or the probe
// reports a verdict about a different path than the one asked about.
func TestMountProbeScriptQuotesPaths(t *testing.T) {
	script := mountProbeScript([]string{"/srv/my data", "/srv/it's"}, true)
	for _, want := range []string{`'/srv/my data'`, `'/srv/it'\''s'`} {
		if !strings.Contains(script, want) {
			t.Errorf("script must contain %s\ngot: %s", want, script)
		}
	}
}

// Same two hazards probescript_test.go pins for the machine probe: a backtick
// terminates the Go raw string this is built from, and the sidecar runs BusyBox
// ash, not bash.
func TestMountProbeScriptStaysPOSIXAndBacktickFree(t *testing.T) {
	script := mountProbeScript([]string{"/x"}, true)
	if strings.Contains(script, "`") {
		t.Error("the mount probe script must not contain a backtick")
	}
	for tok, why := range map[string]string{
		"[[":       "bash test brackets are not in BusyBox ash",
		"function": "the bash 'function' keyword is not POSIX",
		"<<<":      "here-strings are a bashism",
		"${!":      "indirect expansion is a bashism",
	} {
		if strings.Contains(script, tok) {
			t.Errorf("mount probe script contains %q: %s", tok, why)
		}
	}
}

// A failed `sh -c` quoted its whole script in the error, and the error went
// into the backup's log: a Redis password probe ran to a screenful there.
func TestCommandForErrorStaysShort(t *testing.T) {
	if got := commandForError([]string{"pg_dumpall", "-U", "postgres"}); got != "pg_dumpall -U postgres" {
		t.Errorf("a short command is quoted whole, got %q", got)
	}
	script := []string{"/bin/sh", "-c", strings.Repeat("echo probe; ", 100)}
	got := commandForError(script)
	if len([]rune(got)) != maxCommandInError+1 || !strings.HasPrefix(got, "/bin/sh -c echo probe;") || !strings.HasSuffix(got, "…") {
		t.Errorf("a long command is cut to %d characters and marked, got %q", maxCommandInError, got)
	}
}
