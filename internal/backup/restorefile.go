package backup

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// Single-file restore (F96).
//
// F21 made every file in a backup browsable and F70 made every generation of it
// searchable, so DockBack could FIND the one config file you deleted in seconds
// — and then left you to download it and `docker cp` it back by hand, guessing
// at ownership and mode. This closes that gap: the archived bytes go straight
// back to the path they came from, with the mode and ownership the backup
// recorded.
//
// Scope is deliberately one file. It is not a partial restore: nothing is
// stopped, no volume is recreated, and no other file is touched.

// RestoreOneFile writes a single archived file back into a running container at
// its original path.
//
// name is the volume-relative path the browse and search UIs show, which is the
// in-container absolute path without its leading slash — the sidecar tars from
// `/`, so the mapping back is exact rather than inferred.
//
// keepBackup first copies whatever is there now to
// `<path>.dockback-<unix>.bak`, inside the same volume, so a write-back can be
// undone without another restore.
//
// Streaming throughout: the file goes archive → decrypt → tar → daemon without
// ever being buffered, so recovering a large file costs no more memory than
// recovering a small one.
func (e *Engine) RestoreOneFile(ctx context.Context, b *store.Backup, source, name, nodeID, targetID string, keepBackup bool) error {
	want := cleanEntryName(name)
	if want == "" {
		return ErrEntryNotFound
	}
	// The destination is derived from the MATCHED archive entry below, but it is
	// validated up front so an unwritable path costs nothing and, more to the
	// point, so no write is ever attempted with a path we have not checked.
	dest := "/" + want
	if path.Clean(dest) != dest {
		return ErrEntryNotFound
	}
	if targetID == "" {
		return fmt.Errorf("no target container for this restore")
	}
	cli, err := e.Reg.Get(nodeID)
	if err != nil {
		return err
	}

	write := func(hdr *tar.Header, r io.Reader) error {
		// Only a regular file can be written back. A backup's tar can legitimately
		// contain symlinks and device nodes; recreating one of those from a
		// "restore this file" button is not what the operator asked for.
		if hdr.Typeflag != tar.TypeReg {
			return fmt.Errorf("%s is not a regular file in this backup — restore the whole volume instead", want)
		}
		if keepBackup {
			aside := fmt.Sprintf("%s.dockback-%d.bak", dest, time.Now().Unix())
			if berr := dockercli.CopyFileAsideInContainer(ctx, cli, targetID, dest, aside); berr != nil {
				// Fail CLOSED: the operator asked to keep the current file, so
				// overwriting it anyway would destroy exactly what they protected.
				return berr
			}
			e.logf(b.ID, "INFO", "Kept the current %s as %s", dest, aside)
		}
		return dockercli.CopyFileToContainer(ctx, cli, targetID, dest, dockercli.FileMeta{
			Mode:    hdr.Mode,
			Size:    hdr.Size,
			UID:     hdr.Uid,
			GID:     hdr.Gid,
			ModTime: hdr.ModTime,
		}, r)
	}

	man := &Manifest{}
	if b.ManifestJSON != "" {
		_ = unmarshal(b.ManifestJSON, man)
	}
	// Incremental (F61): the newest copy of the file lives in the newest chain
	// generation that touched it — the same newest→oldest walk ExtractOne does, so
	// browse, download and write-back always agree on which bytes are "the file".
	if man.Incremental && man.Parent != "" {
		chain, cerr := e.resolveRestoreChain(b, man)
		if cerr != nil {
			return cerr
		}
		for i := len(chain) - 1; i >= 0; i-- {
			gen := chain[i]
			gman := &Manifest{}
			_ = unmarshal(gen.ManifestJSON, gman)
			member := "volumes.tar"
			src := ""
			if gman.Incremental {
				member = volumeDeltaMember
			}
			if gen.ID == b.ID {
				src = source
			}
			ok, werr := e.walkToEntry(ctx, gen, src, member, want, write)
			if werr != nil {
				return werr
			}
			if ok {
				e.logf(b.ID, "INFO", "Restored %s into the container from this backup", dest)
				return nil
			}
		}
		return ErrEntryNotFound
	}

	ok, err := e.walkToEntry(ctx, b, source, "volumes.tar", want, write)
	if err != nil {
		return err
	}
	if !ok {
		return ErrEntryNotFound
	}
	e.logf(b.ID, "INFO", "Restored %s into the container from this backup", dest)
	return nil
}

// IsVolumeOnlyBackup reports whether a backup is a standalone named-volume
// capture (F23) rather than a container's.
//
// Its archive members are volume-CONTENTS-relative, not container-absolute, so
// there is no path to write a single file back to and no container to write it
// into. Callers refuse rather than write to a plausible-looking wrong path.
func IsVolumeOnlyBackup(b *store.Backup) bool {
	return b != nil && strings.HasPrefix(b.TargetName, "volume:")
}
