package backup

import (
	"context"
	"fmt"
	"strings"

	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// Identity-neutralised clones (F155).
//
// "Restore as a copy" is the safest thing DockBack offers: an isolated clone
// gets fresh volumes, no published ports and a throwaway network, so it can be
// brought up beside a running production container, inspected, and deleted. That
// isolation is about the HOST — nothing on the network can reach the clone.
//
// It says nothing about what the clone can reach. An application that registers
// itself with a cloud service needs only outbound access, which a throwaway
// bridge network provides, and the restored copy carries the same identity and
// the same account token as the original. So the clone announces itself as the
// server the operator is still running, and the two contend: the rehearsal
// intended to prove a backup works instead disturbs the thing it was proving.
//
// The fix is to remove the identity from the CLONE'S OWN COPY of the file before
// its first start. Three properties make that safe:
//
//   - It runs only for a clone. An in-place or recreate restore is left
//     byte-identical, because preserving identity is the entire point there.
//   - It edits a copy that is a throwaway by construction.
//   - It happens BEFORE the container is started, because after is too late.
//
// A required redaction that cannot be applied does not start the clone. That is
// the deliberate severity: the consequence of getting this wrong is not confined
// to the clone, and a rehearsal is never worth disturbing production for.

// noFileMarker is printed by the in-container script when the declared file does
// not exist, so "nothing to do" is distinguishable from "could not do it".
const noFileMarker = "DockBack: no such file"

// redactedMarker is what an emptied value is replaced with — nothing. Emptying
// rather than deleting the key keeps the file's shape, which matters for an
// application that expects to find it.
const redactedMarker = ""

// cloneRedactionsFor returns the redactions an image's application declares.
func cloneRedactionsFor(image string) []CloneRedaction {
	p := ProfileFor(image)
	if p == nil {
		return nil
	}
	return p.CloneRedactions
}

// redactAttrScript builds the in-container edit for one file.
//
// Each attribute is matched as `Name="…"` and its value emptied. The pattern is
// anchored on the attribute name followed immediately by `="`, so a longer
// attribute that merely ends with the same word is not caught, and a value
// containing the word is not either.
//
// Written with sed because it has to run inside an arbitrary application image
// with no assumption beyond a POSIX shell. Nothing is read back out of the
// file — the verification below re-reads only whether the values are now empty,
// never what they were.
func redactAttrScript(path string, attrs []string) string {
	var b strings.Builder
	q := "'" + shellEscape(path) + "'"
	// A file that is not there holds no identity to clear, so there is nothing to
	// neutralise and nothing to refuse over. Reported, not failed — the opposite
	// would turn a profile matching an image slightly too broadly into a clone
	// that cannot be started at all.
	b.WriteString("[ -f " + q + " ] || { echo '" + noFileMarker + "'; exit 0; }; set -e; ")
	b.WriteString("cp " + q + " " + q + ".dockback-pre-redact; ")
	for _, a := range attrs {
		esc := shellEscape(a)
		// sed -i is not universally available; write to a temp file and move.
		b.WriteString("sed 's/\\(" + esc + `="\)[^"]*"/\1` + redactedMarker + "\"/g' " + q +
			" > " + q + ".dockback-tmp && cat " + q + ".dockback-tmp > " + q + "; ")
	}
	b.WriteString("rm -f " + q + ".dockback-tmp " + q + ".dockback-pre-redact; ")
	// Prove it: any character other than the closing quote immediately after the
	// opening one means a value is still there.
	//
	// The pattern is `Name="[^"]` and not `Name="..*"`, which is what this was
	// first written as and which is wrong in the direction that matters: `.`
	// matches the closing quote, so an emptied `Name=""` still matched and every
	// correctly-redacted clone was refused. Caught by running it against a real
	// Preferences.xml rather than by reading it.
	//
	// grep prints nothing of the file's contents — only its exit code is used, so
	// no part of the value can reach a log.
	for _, a := range attrs {
		esc := shellEscape(a)
		b.WriteString("if grep -q '" + esc + `="[^"]' ` + q + "; then echo 'DockBack: value still present after redaction' >&2; exit 4; fi; ")
	}
	b.WriteString("exit 0")
	return b.String()
}

// neutralizeClone applies an application's declared clone redactions (F155).
//
// Called only on the clone path, only before the container starts. Returns an
// error when a REQUIRED redaction could not be applied, which fails the restore
// rather than starting something that would reach outside its sandbox.
func (e *Engine) neutralizeClone(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, opts RestoreOptions) error {
	reds := cloneRedactionsFor(manifestImage(man, b))
	if len(reds) == 0 {
		return nil
	}
	for _, r := range reds {
		if r.Path == "" || len(r.Attrs) == 0 {
			continue
		}
		e.logf(b.ID, "INFO", "Neutralising this copy before it starts: clearing %s in %s — %s",
			strings.Join(r.Attrs, ", "), r.Path, r.Why)
		out, err := dockercli.ExecHook(ctx, cli, opts.TargetID,
			[]string{"/bin/sh", "-c", redactAttrScript(r.Path, r.Attrs)}, "", "")
		if err == nil {
			if strings.Contains(string(out), noFileMarker) {
				e.logf(b.ID, "INFO", "%s is not present in this copy, so there is nothing to clear", r.Path)
				continue
			}
			e.logf(b.ID, "INFO", "This copy carries no %s — it cannot announce itself as the original", strings.Join(r.Attrs, " or "))
			continue
		}
		detail := strings.TrimSpace(string(out))
		if !r.Required {
			e.logf(b.ID, "WARN", "Could not clear %s in %s (%v%s) — %s", strings.Join(r.Attrs, ", "), r.Path, err, detailSuffix(detail), r.Why)
			continue
		}
		e.logf(b.ID, "ERR", "Refusing to start this copy: %s could not be cleared in %s (%v%s)",
			strings.Join(r.Attrs, ", "), r.Path, err, detailSuffix(detail))
		return fmt.Errorf("this copy was not started because it could not be made safe to start: %s. "+
			"%s Restoring a copy must never disturb the original, so the copy is left stopped — "+
			"inspect its files directly, or clear that value by hand before starting it",
			r.Why, strings.TrimSpace(detail))
	}
	return nil
}

// detailSuffix renders a command's own message as a parenthetical, or nothing.
func detailSuffix(s string) string {
	if s = strings.TrimSpace(s); s == "" {
		return ""
	}
	return ": " + s
}
