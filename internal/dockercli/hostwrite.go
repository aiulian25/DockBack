package dockercli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// hostReconstructMount is the fixed in-sidecar mountpoint for the PARENT of the
// stack directory. We bind-mount the parent (not the stack dir itself) so the
// sidecar can read the parent's ownership and stamp the newly-created stack
// directory + compose file with the same uid:gid — keeping the restored layout
// consistent with how the user already organizes that folder.
const hostReconstructMount = "/dockback-host"

// standardComposeNames are the filenames Docker Compose looks for, in the order
// it looks for them. The FIRST one present in a directory is the file
// `docker compose` will actually use, which is what makes the order matter here:
// writing docker-compose.yml into a folder that already has compose.yaml
// produces a file nothing reads.
var standardComposeNames = []string{
	"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml",
}

// displacedSuffix marks the copy of an existing compose file that a
// reconstruction replaced (F176).
//
// This used to be handled the other way round: the reconstruction was written
// beside the existing file as docker-compose.dockback-restored.yml, on the rule
// that DockBack never overwrites a hand-edited compose. The rule was right and
// the outcome was not. It leaves two compose files in one directory where the
// canonical name — the one `docker compose` reads, the one every command and
// every set of instructions assumes — is the stale one, and the accurate file is
// the one with the odd name that has to be passed with -f every time.
//
// So the reconstruction now takes the canonical name and the existing file is
// RENAMED aside, timestamped. Nothing is destroyed, which was the point of the
// original rule; what changes is which of the two a bare `docker compose up`
// picks, and it is now the one that matches what is actually running.
const displacedSuffix = ".pre-dockback-restore"

// composeTimestamp formats the moment a file was displaced, for its new name.
// Sortable, no spaces or colons, safe as a path component.
func composeTimestamp(t time.Time) string {
	return t.UTC().Format("20060102-150405")
}

// safeHostComponent is the grammar for a single path component (a stack folder
// name or a compose filename) that we are willing to embed in a shell command or
// a bind spec. Anything outside it is rejected rather than escaped, so a crafted
// compose label can never inject a mount option, a second bind, or a shell token.
var safeHostComponent = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ownerRe validates the "uid:gid" read back from stat before it is embedded in a
// chown command — numeric only, no shell metacharacters.
var ownerRe = regexp.MustCompile(`^[0-9]+:[0-9]+$`)

// modeRe validates the octal permission bits read back from stat before they are
// recorded or embedded in a chmod — the same rule as ownerRe, for the same
// reason. Three or four digits covers the ordinary bits and the setuid/sticky
// forms; anything else is not a mode we are willing to reproduce.
var modeRe = regexp.MustCompile(`^[0-7]{3,4}$`)

// forbiddenHostRoots are absolute paths DockBack refuses to reconstruct a stack
// into — system locations where writing a directory + compose file would be
// dangerous or nonsensical. The check matches the path itself or anything under
// it, so e.g. /etc, /etc/anything, /var/lib/docker/... are all refused.
var forbiddenHostRoots = []string{
	"/", "/bin", "/boot", "/dev", "/etc", "/lib", "/lib32", "/lib64",
	"/proc", "/root", "/run", "/sbin", "/sys", "/usr", "/var/lib",
	"/var/run",
}

// ForbiddenHostRoot reports whether p is (or lives under) a system root
// DockBack refuses to write into — the single source of truth shared by
// write-time validation (validateHostStackDir) and API-side validation of a
// path-remap target base (F81), so the two can never drift.
func ForbiddenHostRoot(p string) bool {
	clean := path.Clean(strings.TrimSpace(p))
	if clean == "/" || clean == "." {
		return true
	}
	for _, bad := range forbiddenHostRoots {
		if bad == "/" {
			continue
		}
		if clean == bad || strings.HasPrefix(clean+"/", bad+"/") {
			return true
		}
	}
	return false
}

// asideSuffix marks a file restored FROM the backup that is deliberately not the
// canonical one (F57). The reconstruction keeps the name `docker compose` reads,
// because it describes the containers as they were just restored; the captured
// original describes the SOURCE machine and may reference env_file targets,
// relative paths or profiles that do not exist here. Both are on disk; only one
// of them is safe to run unread, and that is the one that keeps its name.
const asideSuffix = ".original-from-backup"

// NamedFile is one extra file to place beside the compose file — the genuine
// host compose file(s) captured at backup time.
type NamedFile struct {
	Name    string // archive basename, e.g. "docker-compose.yml"
	Content []byte
}

// asideName renders the non-canonical name a captured original is written under,
// or "" when the basename is not one we are willing to put on a host.
//
// Reuses safeComposeName's grammar so a crafted archive entry can never reach
// the shell or escape the directory: anything outside the safe-component
// alphabet collapses to the default, and the suffix is appended after that.
// Pure, so the rule is unit-testable.
func asideName(base string) string {
	b := strings.TrimSpace(base)
	if b == "" || b == "." || b == ".." || !safeHostComponent.MatchString(b) {
		return ""
	}
	return b + asideSuffix
}

// HostReconstructResult reports what a successful stack-directory reconstruction
// wrote, so the caller can log it precisely for the user.
type HostReconstructResult struct {
	Path string // absolute host path of the compose file that was written
	Dir  string // absolute host stack directory that now holds it
	// EnvPath is where the .env was written, when the caller supplied one and
	// it needed writing (F194). Empty otherwise.
	EnvPath string
	// EnvDisplaced is where a previous .env was renamed to, empty when there was
	// none or it already matched.
	EnvDisplaced string
	// Displaced is the absolute path the previous compose file was renamed to,
	// empty when there was nothing there or when it already matched (F176).
	Displaced string
	// Originals are the absolute paths of the genuine host compose file(s)
	// restored from the backup alongside the reconstruction (F57), each under
	// asideSuffix so neither `docker compose` nor a reader mistakes one for the
	// canonical file. Empty when the backup carried none.
	Originals []string
	// Unchanged is true when a compose file was already present and byte-identical
	// to the reconstruction, so nothing was written or renamed. Re-running a
	// restore has to converge, not accumulate a .bak per attempt.
	Unchanged bool
	Owner     string // uid:gid stamped on the created dir/file (matched to the parent), "" if unknown
}

// validateHostStackDir vets an absolute host directory we are about to create and
// write a compose file into, returning the parent to bind-mount and the leaf
// component to create under it. It is deliberately strict — this is the one place
// DockBack writes to arbitrary host paths, so the rules are allow-list shaped:
//
//   - must be a clean, absolute path with no bind/shell-hostile characters,
//   - at least two components deep (never a top-level dir like /app),
//   - not a system root (see forbiddenHostRoots), and
//   - the leaf name must match the safe component grammar.
//
// Split out as a pure function so the security rules are unit-tested without a
// live daemon.
func validateHostStackDir(dir string) (parent, base string, err error) {
	return validateHostPath(dir, "stack directory")
}

// validateHostPath is those rules, shared by every host write.
//
// One implementation on purpose. There are now two things DockBack puts on a
// host filesystem — a stack directory with its compose file, and a bind mount
// whose root is a single file — and they need identical guarantees. Two copies
// of a security check drift, and the copy that drifts is the one nobody
// remembered to update.
//
// `what` names the thing in the error only; the rules never vary by caller.
func validateHostPath(p, what string) (parent, base string, err error) {
	d := strings.TrimSpace(p)
	if d == "" {
		return "", "", fmt.Errorf("empty %s", what)
	}
	// ':' would break the "src:dst" bind spec; NUL/newline are shell-hostile.
	if strings.ContainsAny(d, ":\n\r\x00") {
		return "", "", fmt.Errorf("%s %q contains an unsafe character", what, p)
	}
	if !path.IsAbs(d) {
		return "", "", fmt.Errorf("%s %q must be an absolute path", what, p)
	}
	clean := path.Clean(d)
	parts := strings.Split(strings.Trim(clean, "/"), "/")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("refusing to write to the top-level path %q — too shallow to be a %s", clean, what)
	}
	// Depth guard above already rejected "/" — the shared helper handles the rest.
	// For a file this also rules out its PARENT being a system directory, because
	// the parent is what gets bound read-write to do the write.
	if ForbiddenHostRoot(clean) {
		return "", "", fmt.Errorf("refusing to write into the system path %q", clean)
	}
	base = path.Base(clean)
	if base == "." || base == ".." || !safeHostComponent.MatchString(base) {
		return "", "", fmt.Errorf("unsafe %s name %q", what, base)
	}
	return path.Dir(clean), base, nil
}

// ValidHostTarget reports whether DockBack is willing to create or write p on a
// host, returning the reason when it is not.
//
// The exported face of validateHostPath, so a caller deciding what it can do
// about a missing path asks the SAME question the write itself will ask. Without
// it a planner would have its own idea of what is safe, and the two would answer
// differently the day one of them changed — which for a check whose whole job is
// keeping writes out of system locations is the failure that matters.
func ValidHostTarget(p string) error {
	_, _, err := validateHostPath(p, "bind mount source")
	return err
}

// hostFileTempSuffix names the staging file a host write goes through. A restore
// interrupted mid-stream must leave the previous file intact rather than a
// truncated one, so the bytes land beside the target and are renamed over it.
const hostFileTempSuffix = ".dockback-partial"

// WriteHostFile writes one file onto a node's host filesystem, with a given
// owner and mode, through a short-lived root sidecar (F81).
//
// It exists for the bind mounts whose root is a FILE. Those cannot be restored
// the way every other mount is: the volume archive is extracted in a sidecar
// where each bind is a live mount, and replacing a bind-mounted file means
// unlinking it, which the kernel refuses with EBUSY — taking the whole restore
// down with it. So a file bind's contents travel outside that archive and are
// written here, to the host, BEFORE the container that mounts them exists.
//
// Safety of the write itself:
//
//   - the path goes through validateHostPath, so it is absolute, deep enough,
//     not a system location, and its leaf is an ordinary filename;
//   - only the file's PARENT is bound, so the sidecar can reach nothing else;
//   - the bytes land in a staging file created under umask 077 and are renamed
//     into place, so an interrupted restore never leaves a truncated secret and
//     the content is never briefly world-readable;
//   - owner and mode are validated as numeric uid:gid and octal bits before they
//     reach the command, and both are passed as argv.
//
// An unreadable mode is NOT guessed. The staging file's 0600 stands instead,
// because for the files this feature exists to carry — keys, licences, tokens —
// too tight is recoverable and too loose is not.
func WriteHostFile(ctx context.Context, c *client.Client, hostPath string, content []byte, owner, mode string) error {
	parent, base, err := validateHostPath(hostPath, "host file")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	if err := ensureSidecar(ctx, c); err != nil {
		return err
	}
	// Bind the PARENT read-write and nothing else. Docker creates it (mkdir -p)
	// when it is missing, which on a fresh host it usually is.
	created, err := c.ContainerCreate(ctx,
		&container.Config{Image: sidecarRef(), Cmd: []string{"sleep", "180"}, Labels: sidecarLabels()},
		&container.HostConfig{Binds: []string{parent + ":" + hostReconstructMount}},
		nil, nil, "")
	if err != nil {
		return fmt.Errorf("host-file sidecar create: %w", err)
	}
	defer removeContainer(c, created.ID)
	if err := c.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("host-file sidecar start: %w", err)
	}

	target := hostReconstructMount + "/" + base
	staging := target + hostFileTempSuffix
	if err := ExecStdin(ctx, c, created.ID, []string{"sh", "-c",
		"umask 077; cat > " + shQuote(staging)}, bytes.NewReader(content)); err != nil {
		return fmt.Errorf("writing %s: %w", hostPath, err)
	}

	// Ownership and permissions before the rename, so the file is never visible
	// at the real path under the wrong ones.
	if ownerRe.MatchString(owner) {
		if _, cerr := ExecCapture(ctx, c, created.ID, []string{"chown", owner, staging}); cerr != nil {
			return fmt.Errorf("setting ownership %s on %s: %w", owner, hostPath, cerr)
		}
	}
	if modeRe.MatchString(mode) {
		if _, cerr := ExecCapture(ctx, c, created.ID, []string{"chmod", mode, staging}); cerr != nil {
			return fmt.Errorf("setting mode %s on %s: %w", mode, hostPath, cerr)
		}
	}
	if _, merr := ExecCapture(ctx, c, created.ID, []string{"mv", "-f", staging, target}); merr != nil {
		_, _ = ExecCapture(ctx, c, created.ID, []string{"rm", "-f", staging})
		return fmt.Errorf("putting %s into place: %w", hostPath, merr)
	}
	return nil
}

// safeComposeName returns a compose filename we are willing to write, defaulting
// to docker-compose.yml when the recorded name is empty or fails the safe-component
// grammar. Keeps a crafted config_files label from ever reaching the shell.
func safeComposeName(name string) string {
	n := strings.TrimSpace(name)
	if n == "" || n == "." || n == ".." || !safeHostComponent.MatchString(n) {
		return "docker-compose.yml"
	}
	return n
}

// ReconstructStackDir recreates the on-host compose project directory and writes
// the reconstructed compose file into it, through a short-lived sidecar that
// bind-mounts the directory's PARENT (Docker auto-creates a missing parent). It
// NEVER overwrites an existing compose file: if one is already present at the
// target name, the reconstruction is written alongside as SiblingComposeName and
// the result is flagged accordingly. The created directory and file are chowned
// to match the parent's owner so the layout stays consistent with the user's
// other stacks. Errors here are the caller's to treat as non-fatal — the
// container has already been recreated; this only rebuilds the host-side folder.
func ReconstructStackDir(ctx context.Context, c *client.Client, stackDir, composeName string, compose []byte) (*HostReconstructResult, error) {
	return ReconstructStackDirWithEnv(ctx, c, stackDir, composeName, compose, nil, "")
}

// ReconstructStackDirWithEnv is ReconstructStackDir plus an optional .env
// written beside the compose file (F194).
//
// The .env gets the same rules the compose file earned: never destroyed (an
// existing different one is renamed aside, timestamped), never rewritten when
// identical (a re-run converges), and mode 0600 — it exists precisely because
// it holds the secrets the compose file no longer does, so it is readable by
// its owner and nobody else.
// preferOwner (F230) is the "uid:gid" the restore is being performed FOR, used
// only when the parent reads as root — which is what a parent Docker created a
// moment ago always reads as. Empty keeps the original behaviour exactly.
//
// originals (F57) are the genuine host compose file(s) captured at backup time,
// written beside the reconstruction under asideSuffix — never canonical, never
// clobbering. Variadic so the two existing callers are unchanged.
func ReconstructStackDirWithEnv(ctx context.Context, c *client.Client, stackDir, composeName string, compose, envFile []byte, preferOwner string, originals ...NamedFile) (*HostReconstructResult, error) {
	parent, base, err := validateHostStackDir(stackDir)
	if err != nil {
		return nil, err
	}
	if len(compose) == 0 {
		return nil, fmt.Errorf("no reconstructed compose file to write")
	}
	cname := safeComposeName(composeName)

	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	if err := ensureSidecar(ctx, c); err != nil {
		return nil, err
	}
	// Bind the PARENT read-write. Docker creates it (mkdir -p) if missing; the
	// sidecar then creates the leaf stack dir under it. The parent path was
	// validated above and contains no ':' so the bind spec is unambiguous.
	created, err := c.ContainerCreate(ctx,
		&container.Config{Image: sidecarRef(), Cmd: []string{"sleep", "180"}, Labels: sidecarLabels()},
		&container.HostConfig{Binds: []string{parent + ":" + hostReconstructMount}},
		nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("host-write sidecar create: %w", err)
	}
	defer removeContainer(c, created.ID)
	if err := c.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("host-write sidecar start: %w", err)
	}

	dir := hostReconstructMount + "/" + base // e.g. /dockback-host/<stack>
	// Read the parent's ownership so we can stamp the created items to match.
	owner := ""
	if out, oerr := ExecCapture(ctx, c, created.ID, []string{"sh", "-c", "stat -c '%u:%g' " + shQuote(hostReconstructMount) + " 2>/dev/null || true"}); oerr == nil {
		if o := strings.TrimSpace(string(out)); ownerRe.MatchString(o) {
			owner = o
		}
	}
	// F230: a parent DOCKER just made is owned by root, and matching it hands the
	// operator a stack folder they need sudo to touch.
	//
	// The bind above creates the parent with `mkdir -p` if it is missing — as the
	// daemon, so root:root. On a cross-host restore the target's base folder
	// usually IS missing, so the rule "match the parent" resolved to root for the
	// whole layout: the folder, the compose file and the .env. That is not the
	// rule's intent; it is the rule meeting a directory that did not exist a
	// moment ago.
	//
	// So when the parent reads as root and the restore knows who the data is
	// being restored FOR, the created items get that owner instead. The parent
	// itself is deliberately left alone — /opt or /srv being root-owned is
	// normal and not ours to change; only what DockBack creates under it is.
	if preferOwner != "" && ownerRe.MatchString(preferOwner) && (owner == "" || owner == "0:0") {
		owner = preferOwner
	}
	if _, err := ExecCapture(ctx, c, created.ID, []string{"sh", "-c", "mkdir -p " + shQuote(dir)}); err != nil {
		return nil, fmt.Errorf("creating stack directory: %w", err)
	}

	// F176: write the file `docker compose` will actually read.
	//
	// The recorded name is the one this stack was deployed from, so it wins. But
	// if the folder holds a DIFFERENT standard name and not that one, writing the
	// recorded name would leave a file nothing reads — so the existing one is the
	// target instead.
	present := existingComposeNames(ctx, c, created.ID, dir)
	target := cname
	if !present[cname] {
		for _, n := range standardComposeNames {
			if present[n] {
				target = n
				break
			}
		}
	}

	// Already correct? Then leave the compose file alone. A restore run twice
	// must converge, not leave a .bak behind on every attempt. The .env is still
	// processed below — the compose being current says nothing about whether the
	// secrets beside it are (F194).
	if present[target] && sameContent(ctx, c, created.ID, dir+"/"+target, compose) {
		res := &HostReconstructResult{
			Path:      path.Join(path.Dir(stackDir), base, target),
			Dir:       path.Join(path.Dir(stackDir), base),
			Unchanged: true,
			Owner:     owner,
		}
		err := writeEnvBeside(ctx, c, created.ID, dir, owner, envFile, res)
		writeOriginalsBeside(ctx, c, created.ID, dir, owner, originals, res)
		return res, err
	}

	// Rename the existing file aside rather than overwriting it. `mv -n` refuses
	// to clobber, so a name already taken leaves BOTH files intact and the write
	// below is abandoned — the old behaviour of never destroying anything is what
	// the failure path still falls back to.
	displaced := ""
	if present[target] {
		aside := target + displacedSuffix + "-" + composeTimestamp(time.Now()) + ".yml"
		if _, merr := ExecCapture(ctx, c, created.ID, []string{"sh", "-c",
			"mv -n " + shQuote(dir+"/"+target) + " " + shQuote(dir+"/"+aside)}); merr != nil {
			return nil, fmt.Errorf("moving the existing compose file aside: %w", merr)
		}
		if out, terr := ExecCapture(ctx, c, created.ID, []string{"sh", "-c",
			"[ -e " + shQuote(dir+"/"+target) + " ] && echo yes || echo no"}); terr != nil || strings.TrimSpace(string(out)) != "no" {
			return nil, fmt.Errorf("the existing compose file could not be moved aside — nothing was overwritten")
		}
		displaced = path.Join(path.Dir(stackDir), base, aside)
	}

	if err := ExecStdin(ctx, c, created.ID, []string{"sh", "-c", "cat > " + shQuote(dir+"/"+target)}, bytes.NewReader(compose)); err != nil {
		return nil, fmt.Errorf("writing compose file: %w", err)
	}

	// Stamp ownership to match the parent (best-effort — a root-owned parent on a
	// fresh host just leaves it root-owned, which the caller surfaces).
	if owner != "" {
		_, _ = ExecCapture(ctx, c, created.ID, []string{"sh", "-c", "chown " + owner + " " + shQuote(dir) + " " + shQuote(dir+"/"+target) + " 2>/dev/null || true"})
	}

	res := &HostReconstructResult{
		Path:      path.Join(path.Dir(stackDir), base, target),
		Dir:       path.Join(path.Dir(stackDir), base),
		Displaced: displaced,
		Owner:     owner,
	}

	err = writeEnvBeside(ctx, c, created.ID, dir, owner, envFile, res)
	writeOriginalsBeside(ctx, c, created.ID, dir, owner, originals, res)
	return res, err
}

// writeEnvBeside writes the .env next to the compose file — same never-destroy
// and converge rules, tighter mode (F194). It exists precisely because it holds
// the secrets the compose file no longer does, so it is readable by its owner
// and nobody else.
func writeEnvBeside(ctx context.Context, c *client.Client, sidecarID, dir, owner string, envFile []byte, res *HostReconstructResult) error {
	if len(envFile) == 0 {
		return nil
	}
	const envTarget = ".env"
	if sameContent(ctx, c, sidecarID, dir+"/"+envTarget, envFile) {
		res.EnvPath = path.Join(res.Dir, envTarget)
	} else {
		if out, terr := ExecCapture(ctx, c, sidecarID, []string{"sh", "-c",
			"[ -e " + shQuote(dir+"/"+envTarget) + " ] && echo yes || echo no"}); terr == nil && strings.TrimSpace(string(out)) == "yes" {
			aside := envTarget + displacedSuffix + "-" + composeTimestamp(time.Now())
			if _, merr := ExecCapture(ctx, c, sidecarID, []string{"sh", "-c",
				"mv -n " + shQuote(dir+"/"+envTarget) + " " + shQuote(dir+"/"+aside)}); merr != nil {
				return fmt.Errorf("moving the existing .env aside: %w", merr)
			}
			res.EnvDisplaced = path.Join(res.Dir, aside)
		}
		if err := ExecStdin(ctx, c, sidecarID, []string{"sh", "-c", "cat > " + shQuote(dir+"/"+envTarget)}, bytes.NewReader(envFile)); err != nil {
			return fmt.Errorf("writing .env: %w", err)
		}
		res.EnvPath = path.Join(res.Dir, envTarget)
	}
	// 0600 always — even when the content already matched, the mode is part of
	// the contract this file exists under.
	cmd := "chmod 600 " + shQuote(dir+"/"+envTarget)
	if owner != "" {
		cmd += " && chown " + owner + " " + shQuote(dir+"/"+envTarget)
	}
	_, _ = ExecCapture(ctx, c, sidecarID, []string{"sh", "-c", cmd + " 2>/dev/null || true"})
	return nil
}

// writeOriginalsBeside places the genuine host compose file(s) from the backup
// next to the reconstruction, each under asideSuffix (F57).
//
// Never canonical, never clobbering: the name is one `docker compose` does not
// read, and an existing file of that name is left exactly as it is (a re-run
// converges instead of accumulating copies). A file that cannot be written is
// skipped rather than failing the restore — the containers are already back, and
// this is a convenience copy of something that also still lives in the archive.
func writeOriginalsBeside(ctx context.Context, c *client.Client, sidecarID, dir, owner string, originals []NamedFile, res *HostReconstructResult) {
	for _, f := range originals {
		name := asideName(f.Name)
		if name == "" || len(f.Content) == 0 {
			continue
		}
		target := dir + "/" + name
		// Identical already? Leave it: converging beats rewriting.
		if sameContent(ctx, c, sidecarID, target, f.Content) {
			res.Originals = append(res.Originals, path.Join(res.Dir, name))
			continue
		}
		if out, terr := ExecCapture(ctx, c, sidecarID, []string{"sh", "-c",
			"[ -e " + shQuote(target) + " ] && echo yes || echo no"}); terr == nil && strings.TrimSpace(string(out)) == "yes" {
			// Something else owns this name. Never overwrite; the archive still
			// holds the file, so skipping costs nothing irreversible.
			continue
		}
		if err := ExecStdin(ctx, c, sidecarID, []string{"sh", "-c", "cat > " + shQuote(target)}, bytes.NewReader(f.Content)); err != nil {
			continue
		}
		// A captured compose file can carry resolved secrets in `environment:`,
		// exactly like the reconstruction it sits beside — same 0600 posture.
		cmd := "chmod 600 " + shQuote(target)
		if owner != "" {
			cmd += " && chown " + owner + " " + shQuote(target)
		}
		_, _ = ExecCapture(ctx, c, sidecarID, []string{"sh", "-c", cmd + " 2>/dev/null || true"})
		res.Originals = append(res.Originals, path.Join(res.Dir, name))
	}
}

// existingComposeNames reports which standard compose filenames — plus the
// recorded one, whatever it is called — are already in the directory.
func existingComposeNames(ctx context.Context, c *client.Client, sidecarID, dir string) map[string]bool {
	out := map[string]bool{}
	for _, n := range standardComposeNames {
		if res, err := ExecCapture(ctx, c, sidecarID, []string{"sh", "-c",
			"[ -f " + shQuote(dir+"/"+n) + " ] && echo yes || echo no"}); err == nil && strings.TrimSpace(string(res)) == "yes" {
			out[n] = true
		}
	}
	return out
}

// sameContent reports whether a file already holds exactly what is about to be
// written, by comparing digests inside the sidecar so the file itself never has
// to be read back out (a compose file holds resolved environment values, and
// those include passwords).
//
// False whenever it cannot tell. Answering "same" on a failed comparison would
// skip a write that was needed; answering "different" only costs one rename.
func sameContent(ctx context.Context, c *client.Client, sidecarID, file string, want []byte) bool {
	out, err := ExecCapture(ctx, c, sidecarID, []string{"sh", "-c",
		"sha256sum " + shQuote(file) + " 2>/dev/null | awk '{print $1}'"})
	if err != nil {
		return false
	}
	have := strings.TrimSpace(string(out))
	if len(have) != 64 {
		return false // no sha256sum in the sidecar, or an unreadable file
	}
	return have == fmt.Sprintf("%x", sha256.Sum256(want))
}

// shQuote single-quotes a string for safe embedding in a /bin/sh command. Every
// value passed here has already been validated to the safe-component grammar (no
// single quotes possible), so this is defense-in-depth, not the primary guard.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
