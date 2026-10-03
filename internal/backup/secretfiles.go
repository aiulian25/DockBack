package backup

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// Secret-file permission audit (F147).
//
// A private key left world-readable is readable by every process in every
// container that shares the directory and by every user on the host, and nothing
// about a working deployment ever complains. It is invisible precisely because
// it works — which is why it survives for years, and why a backup, which reads
// the file anyway, is a good place to notice it.
//
// DockBack does NOT fix it. That is a deliberate limit, and it is the same rule
// that stops a restore quietly LOOSENING a mode: a restore reproduces the
// permissions it captured and never changes an operator's security posture
// without being asked. Tightening is a change too. The rule is worth more than
// the fix, and the fix is one command, so the finding is reported with that
// command at the two moments it is actionable — when the backup is taken, and
// when the files land on a new machine.
//
// Only the MODE is read. The file is never opened, so this adds no exposure of
// its own; a permission bit is not a secret.

// parseOctalMode parses a "600"-style mode. ok is false for anything it cannot
// read, so an unrecorded or malformed mode produces no verdict at all.
func parseOctalMode(s string) (uint32, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, false
	}
	return uint32(n), true
}

// octal renders a mode the way an operator will type it.
func octal(m uint32) string { return strconv.FormatUint(uint64(m), 8) }

// auditSecretFiles records the permissions of this application's sensitive files
// (F147). No-op for an image that declares none, which is nearly all of them.
func (e *Engine) auditSecretFiles(ctx context.Context, cli *client.Client, containerID, image string, man *Manifest, logID string) {
	p := ProfileFor(image)
	if p == nil || len(p.SecretFiles) == 0 {
		return
	}
	want := map[string]SecretFile{}
	paths := make([]string, 0, len(p.SecretFiles))
	for _, sf := range p.SecretFiles {
		if sf.Path == "" {
			continue
		}
		want[sf.Path] = sf
		paths = append(paths, sf.Path)
	}
	modes, err := dockercli.FileModesBounded(ctx, cli, containerID, paths)
	if err != nil {
		e.logf(logID, "INFO", "Permission check skipped (%v) — the files are captured either way", err)
		return
	}
	man.SecretModes = secretModesOf(want, modes)
	for _, f := range findingsFor(man.SecretModes) {
		e.logf(logID, "WARN", "%s", f)
	}
}

// secretModesOf pairs what was measured with what the profile expects.
//
// A path that does not exist is dropped rather than recorded: a profile lists
// the places a file MAY live across image variants and deployment styles, and
// most of them legitimately will not be there. Recording those as findings would
// make every backup of every such app noisy, which is how a finding stops being
// read.
func secretModesOf(want map[string]SecretFile, modes []dockercli.FileMode) []SecretFileMode {
	var out []SecretFileMode
	for _, m := range modes {
		sf, known := want[m.Path]
		if !known || m.Missing || m.Mode == "" {
			continue
		}
		out = append(out, SecretFileMode{
			Path: m.Path, Mode: m.Mode, Max: octal(sf.MaxMode), What: sf.What,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// findingsFor renders the over-permissive files as sentences that name what is
// exposed and end in the command that fixes it.
func findingsFor(modes []SecretFileMode) []string {
	var out []string
	for _, m := range modes {
		if !m.TooOpen() {
			continue
		}
		what := m.What
		if what == "" {
			what = "a sensitive file"
		}
		out = append(out, fmt.Sprintf(
			"%s is mode %s — %s should be %s, readable only by its owner. DockBack restores permissions exactly as captured and never changes them, so this travels with the backup until you fix it: chmod %s %s",
			m.Path, m.Mode, what, m.Max, m.Max, m.Path))
	}
	return out
}

// SecretFileFindings is the exported view for the API: what is over-permissive
// in a backup, in the operator's words. Empty for everything with nothing to
// report, which is nearly every backup.
func SecretFileFindings(man *Manifest) []string {
	if man == nil {
		return nil
	}
	return findingsFor(man.SecretModes)
}

// reportRestoredSecretModes re-reads the audited files after a restore and says
// what came back (F147).
//
// It never fails a restore. The permissions are a property of the data, faithful
// restoration of them is correct behaviour, and refusing to complete a recovery
// over a mode bit would be choosing the wrong thing to be strict about. What it
// does is make sure the finding is in front of the operator at the moment they
// are already looking at this deployment — which, on a new machine, is the best
// chance it will ever get of being fixed.
func (e *Engine) reportRestoredSecretModes(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, opts RestoreOptions) {
	p := ProfileFor(manifestImage(man, b))
	if p == nil || len(p.SecretFiles) == 0 {
		return
	}
	want := map[string]SecretFile{}
	paths := make([]string, 0, len(p.SecretFiles))
	for _, sf := range p.SecretFiles {
		if sf.Path != "" {
			want[sf.Path] = sf
			paths = append(paths, sf.Path)
		}
	}
	modes, err := dockercli.FileModesBounded(ctx, cli, opts.TargetID, paths)
	if err != nil {
		return // nothing measured, so nothing claimed
	}
	found := secretModesOf(want, modes)
	findings := findingsFor(found)
	for _, f := range findings {
		e.logf(b.ID, "WARN", "After the restore: %s", f)
	}
	unreadable := e.reportUnreadableSecrets(ctx, cli, b, man, opts, want, modes)
	if len(findings) == 0 && unreadable == 0 && len(found) > 0 {
		e.logf(b.ID, "INFO", "Restored %d sensitive file(s) with their permissions intact and no wider than they should be", len(found))
	}
}

// reportUnreadableSecrets says when a restored secret came back owned by an
// account that is not the one the application runs as, and returns how many.
//
// The permission audit above asks whether a file is too OPEN. This asks the
// opposite question, and it is the one a move actually raises: a 0600 key owned
// by the account that created it on the old machine is perfectly secured and
// perfectly useless, because the user the application drops to cannot open it.
// Nothing in a working deployment ever says so — the container starts, reports
// healthy, and one feature quietly does not work.
//
// Reported, never fixed. That is the same rule that stops a restore silently
// LOOSENING a mode (F147): DockBack reproduces what it captured and does not
// change an operator's security posture unasked. Tightening and re-owning are
// both changes. What it owes them is the sentence and the exact command.
func (e *Engine) reportUnreadableSecrets(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, opts RestoreOptions, want map[string]SecretFile, modes []dockercli.FileMode) int {
	readerUID, readerSource, known := e.secretReaderUID(ctx, cli, b, man, opts)
	if !known {
		return 0 // nothing to compare against; a guess here would be a wrong chown
	}
	reported := 0
	for _, m := range modes {
		sf, isSecret := want[m.Path]
		if !isSecret || m.Missing || m.Owner == "" {
			continue
		}
		fileUID, ok := ownerUID(m.Owner)
		if !ok || fileUID == readerUID {
			continue
		}
		// Only the owner's own bits matter here. A file anyone can read is a
		// finding for the audit above, not for this one.
		if !ownerOnlyReadable(m.Mode) {
			continue
		}
		what := sf.What
		if what == "" {
			what = "a sensitive file"
		}
		reported++
		e.logf(b.ID, "WARN", "After the restore: %s (%s) is owned by %s with mode %s, but %s %s — that user cannot read it, so the feature it belongs to will not work. Fix it on the host with: chown %s %s",
			m.Path, what, m.Owner, m.Mode, readerSource, readerUID, readerUID, e.hostPathFor(man, m.Path, opts))
	}
	return reported
}

// ownerUID takes the uid half of a "uid:gid" pair. Only the uid decides whether
// an owner-only file can be opened.
func ownerUID(owner string) (string, bool) {
	uid, _, found := strings.Cut(strings.TrimSpace(owner), ":")
	if !found || uid == "" {
		return "", false
	}
	if _, err := strconv.Atoi(uid); err != nil {
		return "", false
	}
	return uid, true
}

// ownerOnlyReadable reports whether a mode denies read to group and other, which
// is what makes ownership decide the outcome. An unreadable mode answers no: a
// check that could not run must never produce a finding.
func ownerOnlyReadable(mode string) bool {
	bits, ok := parseOctalMode(mode)
	if !ok {
		return false
	}
	const groupAndOtherRead = 0o044
	return bits&groupAndOtherRead == 0
}

// secretReaderUID names the uid the application will read its files as, and how
// that was established, in decreasing order of how directly it is known.
//
// The first three are statements: the image says so, the backup recorded it, or
// the operator pinned it. The fourth is evidence — the ids owning the data this
// application must be able to WRITE. Read-only mounts are excluded from that
// deliberately: a reference set is often owned by whoever placed it on the host
// rather than by the application, and including it makes the answer ambiguous
// exactly where it matters. Service accounts the image ships are excluded for
// the reason Step 5 gives.
//
// Returns known=false rather than a best guess. A wrong uid here becomes a chown
// an operator pastes into a root shell.
func (e *Engine) secretReaderUID(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, opts RestoreOptions) (uid, source string, known bool) {
	ictx, cancel := context.WithTimeout(ctx, 15*time.Second)
	insp, err := cli.ContainerInspect(ictx, opts.TargetID)
	cancel()
	if err == nil && insp.Config != nil {
		if u, ok := ownerUID(strings.TrimSpace(insp.Config.User) + ":"); ok {
			return u, "the image is configured to run as", true
		}
	}
	if man != nil && man.RunAsIDs != nil {
		return strconv.Itoa(man.RunAsIDs.UID), "this application runs as", true
	}
	if u, _, set := e.RestoreOwnership(opts.NodeID, b.TargetName); set {
		return strconv.Itoa(u), "you pinned this container's data to", true
	}
	if u, ok := writableDataOwnerUID(man); ok {
		return u, "the rest of this application's writable data is owned by", true
	}
	return "", "", false
}

// writableDataOwnerUID returns the uid that owns every one of this container's
// writable bind mounts, when they agree. Disagreement means no answer.
func writableDataOwnerUID(man *Manifest) (string, bool) {
	if man == nil {
		return "", false
	}
	seen := map[string]bool{}
	for _, v := range man.MountedBinds {
		if v.ReadOnly || v.Kind != dockercli.MountKindDir || v.Owner == "" {
			continue
		}
		if serviceAccountOwner(v.Owner) {
			continue // an account the image ships, not the application's
		}
		if u, ok := ownerUID(v.Owner); ok {
			seen[u] = true
		}
	}
	if len(seen) != 1 {
		return "", false
	}
	for u := range seen {
		return u, true
	}
	return "", false
}

// hostPathFor turns a container path into the host path an operator would run
// chown against, following the bind it lives under and the path remap this
// restore applied. Falls back to the container path when the backup records no
// bind that covers it — a partly-useful instruction beats none.
func (e *Engine) hostPathFor(man *Manifest, containerPath string, opts RestoreOptions) string {
	if man == nil {
		return containerPath
	}
	best := VolumeRef{}
	for _, v := range man.MountedBinds {
		if v.Destination == "" || v.Source == "" {
			continue
		}
		if containerPath != v.Destination && !strings.HasPrefix(containerPath, strings.TrimSuffix(v.Destination, "/")+"/") {
			continue
		}
		if len(v.Destination) > len(best.Destination) {
			best = v
		}
	}
	if best.Source == "" {
		return containerPath
	}
	host := best.Source + strings.TrimPrefix(containerPath, best.Destination)
	return dockercli.RemapHostPath(host, opts.RemapFromPath, opts.RemapToPath)
}
