package dockercli

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/docker/docker/client"
)

// Reading files without being able to run anything (#2).
//
// Every file read in this tool goes through a sidecar: create a container with
// VolumesFrom the target and tar its way out. R1 §3.1 measured the estate where
// that cannot work — both NAS, reached through tecnativa/docker-socket-proxy,
// permitted container CREATE and refused START. A sidecar could be built and
// never run, so file capture died on a host where
// `GET /containers/{id}/archive` would have returned everything without any
// container at all.
//
// PLAYBOOK §4.3 is the reason that endpoint is the right fallback: it "needs
// nothing inside the container, works on scratch images, reads through bind
// mounts, and preserves ownership/mode in tar headers." Measured here too — a
// file owned 101:104 mode 640 comes back 101:104 mode 640, identical to the
// sidecar's headers for the same tree.
//
// It is a FALLBACK and is labelled as one. The sidecar path stays the primary:
// it produces one stream for all mounts in one pass, and its `tar --exclude`
// does the filtering at the source rather than after the bytes have already been
// read.

// archiveRootMismatch is returned when a member cannot be re-rooted, which would
// mean writing it to the wrong place on restore.
var archiveRootMismatch = errors.New("archive member is not under the requested path")

// SidecarStartRefused reports whether a sidecar failed because the daemon would
// not START it — the create-yes/start-no shape R1 measured.
//
// Branching on the ATTEMPT's error is PLAYBOOK §3's rule, arrived at the hard
// way: the report's author "initially took ALLOW_STOP=0 at face value and wrote
// in two reports that a cold backup was structurally impossible. That was
// wrong." The proxy's declared environment did not describe what it did. Only
// the request's own outcome does.
//
// Matched on DockBack's own wrapper text rather than on a status code alone, so
// a create failure — a different problem with a different answer — cannot be
// mistaken for this one.
func SidecarStartRefused(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "sidecar start:")
}

// TarViaArchiveAPI reads the given container paths through the archive API and
// emits ONE tar stream whose members are laid out exactly as the sidecar's
// `tar -cf - -C /` would have laid them out.
//
// The re-rooting is the whole job. The daemon roots its archive at the requested
// path's BASENAME — asking for /var/lib/foo returns `foo/`, `foo/top.txt` —
// while the sidecar returns `var/lib/foo/`, `var/lib/foo/top.txt`. Restores
// extract with `tar -xf - -C /`, so shipping the daemon's layout unchanged would
// write /foo instead of /var/lib/foo: every file present, every one in the wrong
// place. Measured against a live daemon, both shapes, before this was written.
//
// excludes are the same absolute container paths `tar --exclude` would have been
// given. The API has no exclusion of its own, so they are applied here as the
// members go past — dropping them is not cosmetic: an embedded database's live
// data directory that ships anyway lands beside the logical dump that supersedes
// it, and a restore lays the torn copy down and starts the application on it.
func TarViaArchiveAPI(ctx context.Context, c *client.Client, containerID string, paths, excludes []string) (io.ReadCloser, error) {
	if len(paths) == 0 {
		return io.NopCloser(strings.NewReader("")), nil
	}
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		for _, p := range paths {
			if err := copyArchivePath(ctx, c, containerID, p, excludes, tw); err != nil {
				_ = tw.Close()
				pw.CloseWithError(err)
				return
			}
		}
		if err := tw.Close(); err != nil {
			pw.CloseWithError(err)
			return
		}
		pw.Close()
	}()
	return pr, nil
}

// copyArchivePath streams one container path into the shared writer.
func copyArchivePath(ctx context.Context, c *client.Client, containerID, containerPath string, excludes []string, tw *tar.Writer) error {
	root := ArchiveMemberRoot(containerPath)
	if root == "" {
		return fmt.Errorf("archive read: %q is not a path that can be captured", containerPath)
	}
	rc, _, err := c.CopyFromContainer(ctx, containerID, containerPath)
	if err != nil {
		return fmt.Errorf("archive read %s: %w", containerPath, err)
	}
	defer rc.Close()
	return RewriteArchiveMembers(rc, tw, root, excludes)
}

// ArchiveMemberRoot is the member prefix the sidecar would have produced for a
// container path: the path relative to /, with no leading or trailing slash.
//
// Empty for a path that cannot be re-rooted — "/" itself, or a relative path —
// because there is no first component to replace and the result would silently
// be laid down somewhere else.
func ArchiveMemberRoot(containerPath string) string {
	cleaned := path.Clean(strings.TrimSpace(containerPath))
	if !strings.HasPrefix(cleaned, "/") {
		return ""
	}
	trimmed := strings.Trim(cleaned, "/")
	if trimmed == "" || trimmed == "." {
		return ""
	}
	return trimmed
}

// RewriteArchiveMembers re-roots every member of a daemon archive onto root and
// writes it to tw, dropping anything under an excluded path.
//
// The daemon's first path component is the requested path's basename, whatever
// that was; it is replaced wholesale rather than trimmed, so a mount at
// /var/lib/foo and one at /foo produce different members instead of colliding.
//
// Hard links and symlinks carry a second path in Linkname. A SYMLINK's target is
// resolved inside the extracted tree and must not be touched — rewriting it
// would repoint the link. A HARD link's target names another member of this same
// archive, so it is re-rooted exactly like the member name, or extraction would
// look for a file that is not there.
//
// One difference from the sidecar's output is inherent and harmless, and was
// measured rather than reasoned about: for a hard-linked pair the two producers
// disagree about WHICH member carries the bytes and which is the link, because
// each walks the directory in its own order. Extracting either archive yields
// the same tree — identical names, ownership, modes, sizes and inode sharing,
// checked side by side — so the layouts are equivalent even though the bytes are
// not identical. Literal byte equality is not available from two independent tar
// writers and is not what the pipeline needs.
func RewriteArchiveMembers(r io.Reader, tw *tar.Writer, root string, excludes []string) error {
	dropped := normalizedExcludes(excludes)
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if hdr == nil {
			continue
		}
		name, err := reroot(hdr.Name, root)
		if err != nil {
			return err
		}
		if excluded(name, dropped) {
			continue
		}
		out := *hdr
		out.Name = name
		if hdr.Typeflag == tar.TypeLink && hdr.Linkname != "" {
			if linked, lerr := reroot(hdr.Linkname, root); lerr == nil {
				out.Linkname = linked
			}
		}
		if err := tw.WriteHeader(&out); err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
}

// reroot swaps a daemon member's first path component for the real one.
//
// The trailing slash is read BEFORE cleaning and reapplied after: path.Clean
// strips it, and it is what marks a directory member in the sidecar's own
// output. Dropping it would still extract as a directory — the typeflag says so
// — but the two layouts would no longer be the same bytes, which is the whole
// contract this exists to keep.
func reroot(name, root string) (string, error) {
	isDir := strings.HasSuffix(name, "/")
	cleaned := strings.TrimPrefix(path.Clean("/"+name), "/")
	if cleaned == "" || cleaned == "." {
		return "", archiveRootMismatch
	}
	// Everything after the daemon's own root component, whatever it named it.
	out := root
	if _, rest, hasRest := strings.Cut(cleaned, "/"); hasRest {
		out = root + "/" + rest
	}
	if isDir {
		out += "/"
	}
	return out, nil
}

// normalizedExcludes renders exclusion paths in member form.
func normalizedExcludes(excludes []string) []string {
	var out []string
	for _, x := range excludes {
		if trimmed := strings.Trim(strings.TrimSpace(x), "/"); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// excluded reports whether a member is at or beneath an excluded path — the same
// two forms `tar --exclude=X --exclude=X/*` covers.
func excluded(name string, dropped []string) bool {
	member := strings.TrimSuffix(name, "/")
	for _, x := range dropped {
		if member == x || strings.HasPrefix(member, x+"/") {
			return true
		}
	}
	return false
}
