package backup

import (
	"context"
	"sort"
	"strings"

	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// Can the application actually WRITE to what it just got back? (#34)
//
// The restore gate is Docker health, which says the container came up. It does
// not say the application can write, and that failure does not surface until it
// tries — days later, when someone uploads a file.
//
// R4 §Issue 34 is why this has to run as the application's own uid. A check that
// ran as root "reported all five binds writable" and "would have passed even
// with completely wrong ownership — the exact failure the check exists to
// catch". Root succeeding proves nothing at all, so root is not tested: that is
// the finding, not a shortcut.
//
// Reported, never fatal. The data is intact and the fix is one chown, which is
// the same philosophy F117's ownership alignment already states — a restore that
// refused over a permission bit would be withholding recovered data over
// something the operator can correct in a second.

// Write-test verdict tags. Only ever a destination follows.
const (
	writeOK   = "WOK"
	writeFail = "WFAIL"
	writeSkip = "WSKIP"
)

// writabilityMarker is the file the test creates and removes. The PID suffix
// keeps two runs from colliding, and every exit path clears it.
const writabilityMarker = ".dockback-w"

// writabilityScript touches and removes a marker in each destination.
//
// Cleanup happens twice on purpose: inline after each attempt, and again from a
// trap that fires on EXIT, INT and TERM. A verification that leaves litter in
// somebody's data directory is a verification that has damaged what it came to
// check. The trap reads "$@" at trap time, where it is still the script's own
// argument list, so a path containing spaces survives.
//
// umask 077 so a marker is never briefly world-readable, and `: >` rather than
// touch so an existing file is never given a new mtime.
func writabilityScript(dests []string) string {
	if len(dests) == 0 {
		return ""
	}
	var script strings.Builder
	script.WriteString("set --")
	for _, dest := range dests {
		script.WriteString(" '" + shellEscape(dest) + "'")
	}
	script.WriteString(`; trap 'for p in "$@"; do rm -f "$p"/` + writabilityMarker + `.* 2>/dev/null; done' EXIT INT TERM; `)
	script.WriteString(`for d in "$@"; do `)
	script.WriteString(`[ -d "$d" ] || { printf '` + writeSkip + `|%s\n' "$d"; continue; }; `)
	script.WriteString(`f="$d/` + writabilityMarker + `.$$"; `)
	script.WriteString(`if ( umask 077; : > "$f" ) 2>/dev/null; then printf '` + writeOK + `|%s\n' "$d"; else printf '` + writeFail + `|%s\n' "$d"; fi; `)
	script.WriteString(`rm -f "$f" 2>/dev/null; done`)
	return script.String()
}

// parseWritability reads the verdicts. A destination the script said nothing
// about is absent from both lists — silence is not a pass.
func parseWritability(out string) (writable, refused, absent []string) {
	for _, line := range strings.Split(out, "\n") {
		verdict, dest, ok := strings.Cut(strings.TrimSpace(line), "|")
		if !ok || dest == "" {
			continue
		}
		switch verdict {
		case writeOK:
			writable = append(writable, dest)
		case writeFail:
			refused = append(refused, dest)
		case writeSkip:
			absent = append(absent, dest)
		}
	}
	sort.Strings(writable)
	sort.Strings(refused)
	sort.Strings(absent)
	return writable, refused, absent
}

// writableDestinations are the mounts the application is expected to write to.
//
// Read-only mounts are excluded because failing to write to them is correct, and
// a file-rooted bind is a file rather than a directory to create a marker in.
func writableDestinations(man *Manifest) []string {
	if man == nil {
		return nil
	}
	var dests []string
	seen := map[string]bool{}
	for _, v := range man.Volumes {
		dest := normalizeMountDest(v.Destination)
		if dest == "" || dest == "/" || v.ReadOnly || seen[dest] {
			continue
		}
		if v.Kind == dockercli.MountKindFile || v.Archive != "" {
			continue
		}
		seen[dest] = true
		dests = append(dests, dest)
	}
	sort.Strings(dests)
	return dests
}

// hostSourceFor is the host path behind a destination, so a fix can name the
// path the operator would actually type.
func hostSourceFor(man *Manifest, dest string) string {
	if man == nil {
		return ""
	}
	want := normalizeMountDest(dest)
	for _, v := range man.MountedBinds {
		if normalizeMountDest(v.Destination) == want {
			return v.Source
		}
	}
	for _, v := range man.Volumes {
		if normalizeMountDest(v.Destination) == want {
			return v.Source
		}
	}
	return ""
}

// verifyWritable tests that the application's own uid can write where it must.
//
// Never fails a restore. The data is back; what is wrong is a permission, and a
// restore that withheld recovered data over one would be trading a real recovery
// for a tidy report.
func (e *Engine) verifyWritable(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, opts RestoreOptions) {
	if man == nil || !opts.Volumes || opts.AsName != "" {
		return
	}
	dests := writableDestinations(man)
	if len(dests) == 0 {
		return
	}
	uid, source, known := e.secretReaderUID(ctx, cli, b, man, opts)
	if !known {
		// Never a guess. Testing as root would pass whatever the ownership is,
		// which is the failure this exists to catch rather than a fallback.
		e.logf(b.ID, "INFO", "Writability not tested — this image declares no runtime uid, and testing as root would pass even if the ownership were wrong")
		return
	}
	if uid == "0" {
		e.logf(b.ID, "INFO", "Writability not tested — %s root, and root can write anywhere, so the test would pass whatever the ownership is", source)
		return
	}

	out, err := dockercli.CaptureSidecarAs(ctx, cli, opts.TargetID, uid, []string{"/bin/sh", "-c", writabilityScript(dests)})
	if err != nil {
		e.logf(b.ID, "INFO", "Could not test whether %s can write to its restored data (%v) — the data is restored, but it was not confirmed writable", b.TargetName, err)
		return
	}
	writable, refused, absent := parseWritability(string(out))

	if len(absent) > 0 {
		e.logf(b.ID, "INFO", "%d recorded %s not present on the target to test: %s",
			len(absent), plural(len(absent), "mount is", "mounts are"), namedTables(absent))
	}
	if len(refused) == 0 {
		if len(writable) > 0 {
			e.logf(b.ID, "INFO", "Writable as uid %s (%s that): all %d restored %s can be written by the application itself, not merely by root",
				uid, source, len(writable), plural(len(writable), "mount", "mounts"))
		}
		return
	}

	e.logf(b.ID, "WARN", "%s cannot write to %d of its restored %s as uid %s (%s that). The data is intact — this is ownership, and the application will fail on its first write until it is fixed:",
		b.TargetName, len(refused), plural(len(refused), "mount", "mounts"), uid, source)
	for _, dest := range refused {
		if host := hostSourceFor(man, dest); host != "" {
			e.logf(b.ID, "WARN", "  %s — on the host: chown -R %s %s", dest, uid, host)
			continue
		}
		e.logf(b.ID, "WARN", "  %s — it is a named volume; fix it from inside: docker run --rm -v <volume>:/d alpine chown -R %s /d", dest, uid)
	}
}
