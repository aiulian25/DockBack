package dockercli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
)

// A compose project's own folder: its scripts, READMEs and config beside the
// compose file (step 24). Captured through a sidecar that sees only that folder,
// read-only, and restored through one that sees only its parent.

const (
	projectFolderMount = "/dockback-project"
	// ProjectFolderIgnoreFile lists, in the project folder, what to leave out.
	ProjectFolderIgnoreFile = ".dockbackignore"
	// maxProjectListBytes bounds the listing, so a project rooted somewhere vast
	// is refused instead of walked into memory.
	maxProjectListBytes = 8 << 20
	// maxProjectIgnoreBytes bounds the ignore file read.
	maxProjectIgnoreBytes  = 64 << 10
	projectSidecarLifetime = "900"
	// projectStagingSuffix names the hidden folder a restore extracts into
	// before copying across what is missing; removed when it is done.
	projectStagingSuffix  = ".dockback-staging"
	projectSidecarTimeout = 15 * time.Minute
)

// ErrProjectFolderTooBig stops a capture that outgrew its cap.
var ErrProjectFolderTooBig = errors.New("the project folder is larger than the capture allows")

// ErrProjectFolderTooManyEntries refuses a folder too large to list.
var ErrProjectFolderTooManyEntries = errors.New("the project folder has too many entries to capture")

// ProjectFolder is a read-only view of one project folder on a node.
type ProjectFolder struct {
	c  *client.Client
	id string
}

// OpenProjectFolder starts the read-only sidecar. The folder is mounted through
// the mount API, which refuses a missing source instead of creating it: reading
// a project must never conjure up its folder.
func OpenProjectFolder(ctx context.Context, c *client.Client, dir string) (*ProjectFolder, error) {
	if _, _, err := validateHostPath(dir, "project folder"); err != nil {
		return nil, err
	}
	if err := ensureSidecar(ctx, c); err != nil {
		return nil, err
	}
	created, err := c.ContainerCreate(ctx,
		&container.Config{Image: sidecarRef(), Cmd: []string{"sleep", projectSidecarLifetime}, Labels: sidecarLabels()},
		&container.HostConfig{Mounts: []mount.Mount{{Type: mount.TypeBind, Source: dir, Target: projectFolderMount, ReadOnly: true}}},
		nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("project folder sidecar: %w", err)
	}
	if err := c.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		removeContainer(c, created.ID)
		return nil, fmt.Errorf("project folder sidecar start: %w", err)
	}
	return &ProjectFolder{c: c, id: created.ID}, nil
}

// Close removes the sidecar.
func (p *ProjectFolder) Close() { removeContainer(p.c, p.id) }

// Entries lists every path in the folder, relative, without walking into the
// pruned paths (relative) or into any folder with one of the pruned names.
// Names holding a newline cannot be handed to tar and are left out.
func (p *ProjectFolder) Entries(ctx context.Context, prunePaths, pruneNames []string) ([]string, error) {
	cmd := []string{"find", projectFolderMount, "-mindepth", "1"}
	var prune []string
	for _, rel := range prunePaths {
		prune = append(prune, "-o", "-path", projectFolderMount+"/"+rel)
	}
	for _, name := range pruneNames {
		prune = append(prune, "-o", "-name", name)
	}
	if len(prune) > 0 {
		cmd = append(cmd, "(")
		cmd = append(cmd, prune[1:]...)
		cmd = append(cmd, ")", "-prune", "-o")
	}
	cmd = append(cmd, "-print0")
	var listing bytes.Buffer
	lw := &limitedWriter{w: &listing, n: maxProjectListBytes}
	if err := ExecStream(ctx, p.c, p.id, cmd, lw); err != nil {
		return nil, fmt.Errorf("listing the project folder: %w", err)
	}
	if lw.over {
		return nil, ErrProjectFolderTooManyEntries
	}
	var entries []string
	for _, name := range strings.Split(listing.String(), "\x00") {
		rel, ok := strings.CutPrefix(name, projectFolderMount+"/")
		if !ok || rel == "" || strings.Contains(rel, "\n") {
			continue
		}
		entries = append(entries, rel)
	}
	return entries, nil
}

// ReadFile returns a small file from the folder, or nil when there is none.
func (p *ProjectFolder) ReadFile(ctx context.Context, rel string) []byte {
	var out bytes.Buffer
	lw := &limitedWriter{w: &out, n: maxProjectIgnoreBytes}
	if err := ExecStream(ctx, p.c, p.id, []string{"cat", projectFolderMount + "/" + rel}, lw); err != nil {
		return nil
	}
	return out.Bytes()
}

// Tar writes a tar of exactly these paths to w: folders with their own owner and
// mode, and nothing beneath a folder unless it is listed too. It stops with
// ErrProjectFolderTooBig past maxBytes, and returns how many bytes it wrote.
func (p *ProjectFolder) Tar(ctx context.Context, rels []string, w io.Writer, maxBytes int64) (int64, error) {
	const listFile = "/tmp/dockback-project.list"
	if err := ExecStdin(ctx, p.c, p.id, []string{"sh", "-c", "cat > " + listFile}, strings.NewReader(strings.Join(rels, "\n")+"\n")); err != nil {
		return 0, fmt.Errorf("handing the project list to tar: %w", err)
	}
	capped := &abortingWriter{w: w, left: maxBytes}
	err := ExecStream(ctx, p.c, p.id, []string{"tar", "-cf", "-", "-C", projectFolderMount, "--no-recursion", "-T", listFile}, capped)
	if capped.over {
		return maxBytes - capped.left, ErrProjectFolderTooBig
	}
	return maxBytes - capped.left, err
}

// abortingWriter fails the stream once it has carried left bytes, so an
// oversized capture stops instead of being read to the end.
type abortingWriter struct {
	w    io.Writer
	left int64
	over bool
}

func (a *abortingWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > a.left {
		a.over = true
		return 0, ErrProjectFolderTooBig
	}
	a.left -= int64(len(p))
	return a.w.Write(p)
}

// projectRestoreScript extracts into a staging folder beside the target, on the
// same filesystem, then copies across only what the target lacks: cp -n never
// overwrites, and -a keeps each entry's owner and mode. BusyBox tar's own -k
// gives up at the first file that exists, so it cannot do this. It prints how
// many files were already there and kept.
const projectRestoreScript = `target="$1"; staging="$2"
rm -rf "$staging" && mkdir -p "$staging" || exit 1
tar -xf - -C "$staging" || { rm -rf "$staging"; exit 1; }
kept=$(cd "$staging" && find . ! -type d | while IFS= read -r f; do [ -e "$target/$f" ] && echo x; done | wc -l)
for e in "$staging"/* "$staging"/.[!.]* "$staging"/..?*; do
  [ -e "$e" ] || [ -L "$e" ] || continue
  cp -an "$e" "$target/" || { rm -rf "$staging"; exit 1; }
done
rm -rf "$staging"
echo "kept=$kept"`

// RestoreProjectFolder extracts a captured project folder into dir, writing only
// what is missing: an existing file is the operator's and is never replaced. It
// returns how many existing files were kept as they were.
func RestoreProjectFolder(ctx context.Context, c *client.Client, dir string, archive io.Reader) (int, error) {
	parent, base, err := validateHostStackDir(dir)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, projectSidecarTimeout)
	defer cancel()
	if err := ensureSidecar(ctx, c); err != nil {
		return 0, err
	}
	created, err := c.ContainerCreate(ctx,
		&container.Config{Image: sidecarRef(), Cmd: []string{"sleep", projectSidecarLifetime}, Labels: sidecarLabels()},
		&container.HostConfig{Binds: []string{parent + ":" + hostReconstructMount}},
		nil, nil, "")
	if err != nil {
		return 0, fmt.Errorf("project restore sidecar: %w", err)
	}
	defer removeContainer(c, created.ID)
	if err := c.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return 0, fmt.Errorf("project restore sidecar start: %w", err)
	}
	target := hostReconstructMount + "/" + base
	if _, err := ExecCapture(ctx, c, created.ID, []string{"mkdir", "-p", target}); err != nil {
		return 0, fmt.Errorf("creating %s: %w", dir, err)
	}
	staging := hostReconstructMount + "/." + base + projectStagingSuffix
	out, err := ExecStdinCapture(ctx, c, created.ID, []string{"sh", "-c", projectRestoreScript, "sh", target, staging}, archive)
	if err != nil {
		return 0, fmt.Errorf("extracting the project folder into %s: %w", dir, err)
	}
	kept, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(lastLine(out), "kept=")))
	return kept, nil
}
