package backup

import (
	"archive/tar"
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// The project folder (step 24). Only the compose files named in the labels and
// the .env were captured, so in the 2026-10-04 recovery forgejo's scripts and
// README had to come back from a how-to guide. Now the whole folder rides in
// the archive, minus the bind-mounted data, which the volume capture owns.

const (
	// projectFolderMember is the work-dir name of the captured folder; it sits
	// in the archive at config/project-folder.tar.
	projectFolderMember = "project-folder.tar"
	// maxProjectFolderBytes caps the capture. A project folder holds scripts and
	// config; anything bigger is data that belongs in a bind mount, or noise for
	// a .dockbackignore.
	maxProjectFolderBytes = 64 << 20
	// composeWorkingDirLabel is where Compose records the project's folder.
	composeWorkingDirLabel = "com.docker.compose.project.working_dir"
)

// projectFolderDefaultIgnores are never worth carrying, at any depth: rebuilt by
// a tool, or history kept elsewhere.
var projectFolderDefaultIgnores = []string{".git", "node_modules", "__pycache__", ".cache"}

// ProjectFolderRef records the captured project folder.
type ProjectFolderRef struct {
	Dir     string `json:"dir"`
	Entries int    `json:"entries,omitempty"`
	Bytes   int64  `json:"bytes,omitempty"`
	// LeftOut are the bind-mounted paths and ignore patterns not captured.
	LeftOut []string `json:"left_out,omitempty"`
	// Skipped says why nothing was captured.
	Skipped string `json:"skipped,omitempty"`
}

// parseProjectIgnore reads a .dockbackignore: one pattern per line, blank lines
// and # comments skipped. Pure.
func parseProjectIgnore(content []byte) []string {
	var patterns []string
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	return patterns
}

// ignoredBy reports whether rel, or a folder above it, matches a pattern, the
// way .gitignore reads one: a pattern with no slash matches a name at any depth
// (*.log, node_modules); one with a slash, or a leading slash, matches from the
// project's root (data/cache, /build). Pure.
func ignoredBy(rel string, patterns []string) bool {
	for _, raw := range patterns {
		anchored := strings.HasPrefix(raw, "/") || strings.Contains(strings.Trim(raw, "/"), "/")
		pattern := strings.Trim(raw, "/")
		if pattern == "" {
			continue
		}
		if anchored {
			if excludedByAny(rel, []string{pattern}) {
				return true
			}
			continue
		}
		for _, name := range strings.Split(rel, "/") {
			if matched, _ := path.Match(pattern, name); matched {
				return true
			}
		}
	}
	return false
}

// bindPathsUnder lists the bind-mount sources inside dir, relative to it. The
// volume capture owns those, captured or deliberately left out. Pure.
func bindPathsUnder(mounts []types.MountPoint, dir string) []string {
	var rels []string
	for _, m := range mounts {
		if m.Type != "bind" {
			continue
		}
		rel, err := filepath.Rel(dir, m.Source)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		rels = append(rels, filepath.ToSlash(rel))
	}
	slices.Sort(rels)
	return slices.Compact(rels)
}

// selectProjectEntries keeps the entries no pattern ignores, sorted so every
// folder comes before what is in it. Pure.
func selectProjectEntries(entries, ignores []string) []string {
	kept := slices.DeleteFunc(slices.Clone(entries), func(rel string) bool { return ignoredBy(rel, ignores) })
	slices.Sort(kept)
	return kept
}

// captureProjectFolder captures the compose project's folder into the work dir.
// Best-effort: a folder that cannot be captured is recorded with the reason,
// and the backup carries on.
func (e *Engine) captureProjectFolder(ctx context.Context, cli *client.Client, id string, insp types.ContainerJSON, work string) *ProjectFolderRef {
	if insp.Config == nil {
		return nil
	}
	dir := strings.TrimSpace(insp.Config.Labels[composeWorkingDirLabel])
	if dir == "" {
		return nil
	}
	ref := &ProjectFolderRef{Dir: dir}
	skip := func(reason string) *ProjectFolderRef {
		ref.Skipped = reason
		e.logf(id, "INFO", "Project folder %s not captured: %s", dir, reason)
		return ref
	}
	folder, err := dockercli.OpenProjectFolder(ctx, cli, dir)
	if err != nil {
		return skip(err.Error())
	}
	defer folder.Close()
	binds := bindPathsUnder(insp.Mounts, dir)
	entries, err := folder.Entries(ctx, binds, projectFolderDefaultIgnores)
	if err != nil {
		return skip(err.Error())
	}
	ignores := parseProjectIgnore(folder.ReadFile(ctx, dockercli.ProjectFolderIgnoreFile))
	keep := selectProjectEntries(entries, ignores)
	if len(keep) == 0 {
		return skip("nothing in it beyond the bind-mounted data")
	}
	out := filepath.Join(work, projectFolderMember)
	f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return skip(err.Error())
	}
	written, terr := folder.Tar(ctx, keep, f, maxProjectFolderBytes)
	if cerr := f.Close(); terr == nil {
		terr = cerr
	}
	if terr != nil {
		_ = os.Remove(out)
		if errors.Is(terr, dockercli.ErrProjectFolderTooBig) {
			e.logf(id, "WARN", "Project folder %s is over %s, so it was not captured. List what to leave out in %s/%s.",
				dir, humanBytes(maxProjectFolderBytes), dir, dockercli.ProjectFolderIgnoreFile)
			ref.Skipped = "over " + humanBytes(maxProjectFolderBytes)
			return ref
		}
		return skip(terr.Error())
	}
	ref.Entries, ref.Bytes = len(keep), written
	ref.LeftOut = append(append(binds, projectFolderDefaultIgnores...), ignores...)
	e.logf(id, "INFO", "Captured the project folder %s: %d entries, %s, without its bind-mounted data", dir, len(keep), humanBytes(written))
	return ref
}

// restoreProjectFolder puts the captured project folder back into dir, filling
// in only what is missing. Best-effort and logged: the containers and their data
// are back either way.
func (e *Engine) restoreProjectFolder(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, source, logID, dir string) {
	if man == nil || man.ProjectFolder == nil || man.ProjectFolder.Entries == 0 {
		return
	}
	var kept int
	found := false
	err := e.streamArchive(ctx, b, source, func(tr *tar.Reader, hdr *tar.Header) (bool, error) {
		if strings.TrimPrefix(hdr.Name, "./") != layoutPath(projectFolderMember) {
			return true, nil
		}
		found = true
		var rerr error
		kept, rerr = dockercli.RestoreProjectFolder(ctx, cli, dir, tr)
		return false, rerr
	})
	switch {
	case err != nil:
		e.logf(logID, "WARN", "The project folder's other files were not restored into %s: %v", dir, err)
	case !found:
		e.logf(logID, "WARN", "The backup records a project folder, but the archive holds none to restore into %s", dir)
	case kept > 0:
		e.logf(logID, "INFO", "Restored the project folder's other files into %s (scripts, READMEs, config). %d file(s) already there were kept as they are.", dir, kept)
	default:
		e.logf(logID, "INFO", "Restored the project folder's other files into %s (scripts, READMEs, config)", dir)
	}
}
