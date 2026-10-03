package backup

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"dockback/internal/store"

	"github.com/docker/docker/client"
)

// Synthetic full baselines (F85). When an incremental chain reaches its "full
// every N" boundary, the periodic full no longer re-reads the whole selection
// from the source node: the run captures only the DELTA from the container,
// then merges the existing chain (baseline + deltas + the fresh delta) into a
// brand-new, self-contained volumes.tar — locally, as a pure streaming tar
// transcode over the already-stored (encrypted, verified) archives. The result
// is an ordinary full archive: restore, verify, drills, retention, and the
// recover tool need zero changes. Any synthesis error falls back to the
// source-read full, so correctness never depends on this optimization.
//
// The invariant the merge preserves: the synthetic full's contents equal what
// chain-restore (restoreFiles) would materialize for the same generations —
// regular files newest-wins from each generation's payload, deletions honored
// via the final index, directory/symlink structure carried from the baseline
// exactly as a chain restore would lay it down.

// synthGen is one generation feeding the merge: either an archived chain link
// (backup != nil) or the freshly-spooled local delta tar (localTar != "").
type synthGen struct {
	backup   *store.Backup
	man      *Manifest
	localTar string
}

// synthesizeFullVolumes merges chain (oldest full → newest delta, as returned
// by resolveRestoreChain) plus the fresh local delta tar into dst as a
// self-contained volumes.tar. freshIdx is the CURRENT (post-capture) index —
// the authoritative final file set; freshChanged is the fresh delta's member
// list (from DiffIndex against the chain tip). Returns the merged tar's sha256
// and the number of regular files written. All-local; the source node is never
// touched.
func (e *Engine) synthesizeFullVolumes(ctx context.Context, chain []*store.Backup, freshDeltaTar string, freshIdx VolIndex, freshChanged []string, dst string) (string, int, error) {
	if len(chain) == 0 {
		return "", 0, fmt.Errorf("empty chain")
	}
	gens := make([]synthGen, 0, len(chain)+1)
	for _, b := range chain {
		m := &Manifest{}
		if b.ManifestJSON == "" || unmarshal(b.ManifestJSON, m) != nil {
			return "", 0, fmt.Errorf("chain link %s has no readable manifest", short(b.ID))
		}
		gens = append(gens, synthGen{backup: b, man: m})
	}
	gens = append(gens, synthGen{localTar: freshDeltaTar})
	fresh := len(gens) - 1

	// Phase 1 — winner map: for every path in the FINAL state, which generation
	// holds its newest bytes. Walk oldest→newest so later generations overwrite;
	// then drop anything absent from the final index (deleted along the chain and
	// never resurrected).
	winner := map[string]int{}
	var prevIdx VolIndex
	for gi, g := range gens {
		switch {
		case gi == fresh:
			for _, p := range freshChanged {
				winner[cleanEntryName(p)] = gi
			}
		case gi == 0:
			base, err := e.loadVolIndex(ctx, g.backup)
			if err != nil {
				return "", 0, fmt.Errorf("baseline index: %w", err)
			}
			for _, en := range base.Entries {
				winner[cleanEntryName(en.Path)] = 0
			}
			prevIdx = base
		default:
			idx, err := e.loadVolIndex(ctx, g.backup)
			if err != nil {
				return "", 0, fmt.Errorf("chain index %s: %w", short(g.backup.ID), err)
			}
			changed, _ := DiffIndex(prevIdx, idx)
			for _, p := range changed {
				winner[cleanEntryName(p)] = gi
			}
			prevIdx = idx
		}
	}
	final := make(map[string]bool, len(freshIdx.Entries))
	for _, en := range freshIdx.Entries {
		final[cleanEntryName(en.Path)] = true
	}
	for p := range winner {
		if !final[p] {
			delete(winner, p)
		}
	}

	// Phase 2 — streaming merge, oldest→newest: copy each generation's payload
	// members that the winner map assigns to it. Non-regular members (dirs,
	// symlinks…) ride from the BASELINE only — the same structure a chain
	// restore lays down (deltas tar regular files only). Sizes come from each
	// inner header, so this is a pure transcode with no directory staging.
	f, err := os.Create(dst)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	tw := tar.NewWriter(io.MultiWriter(f, h))
	files := 0

	copyGen := func(gi int, inner *tar.Reader) error {
		for {
			vh, verr := inner.Next()
			if verr == io.EOF {
				return nil
			}
			if verr != nil {
				return verr
			}
			p := cleanEntryName(vh.Name)
			keep := false
			if vh.Typeflag == tar.TypeReg {
				// Explicit presence check: a map miss returns 0, which would
				// otherwise resurrect baseline files deleted later in the chain.
				wgi, ok := winner[p]
				keep = ok && p != "" && wgi == gi
			} else {
				keep = gi == 0 // baseline structure (dirs, symlinks, …)
			}
			if !keep {
				continue
			}
			if err := tw.WriteHeader(vh); err != nil {
				return err
			}
			if vh.Typeflag == tar.TypeReg {
				if _, err := io.Copy(tw, inner); err != nil {
					return err
				}
				files++
			}
		}
	}

	for gi, g := range gens {
		if g.localTar != "" {
			lf, lerr := os.Open(g.localTar)
			if lerr != nil {
				return "", 0, lerr
			}
			err := copyGen(gi, tar.NewReader(lf))
			lf.Close()
			if err != nil {
				return "", 0, fmt.Errorf("fresh delta: %w", err)
			}
			continue
		}
		member := volPayloadMember(g.man)
		seen := false
		err := e.streamArchive(ctx, g.backup, "", func(tr *tar.Reader, hdr *tar.Header) (bool, error) {
			if hdr.Name != member {
				return true, nil
			}
			seen = true
			if cerr := copyGen(gi, tar.NewReader(tr)); cerr != nil {
				return false, cerr
			}
			return false, nil // payload found and merged — stop scanning this archive
		})
		if err != nil {
			return "", 0, fmt.Errorf("generation %s: %w", short(g.backup.ID), err)
		}
		if !seen {
			return "", 0, fmt.Errorf("generation %s has no %s member", short(g.backup.ID), member)
		}
	}
	if err := tw.Close(); err != nil {
		return "", 0, err
	}
	if err := f.Sync(); err != nil {
		return "", 0, err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), files, nil
}

// syntheticFullEnabled reads the F85 toggle (default on).
func (e *Engine) syntheticFullEnabled() bool {
	v, _ := e.Store.GetSetting("backup.synthetic_full", "true")
	return v != "false"
}

// captureSyntheticFull runs the F85 boundary capture: a delta-only source read
// (changed files vs the chain tip), then a local chain merge into a normal
// self-contained full. tip is the newest usable chain member (the would-be
// parent that hit the depth cap). On error the caller falls back to
// captureFullBaseline — this function never partially commits manifest state.
func (e *Engine) captureSyntheticFull(ctx context.Context, cli *client.Client, opts Options, man *Manifest, work string, id string, tip *store.Backup, curIdx VolIndex) error {
	tipIdx, err := e.loadVolIndex(ctx, tip)
	if err != nil {
		return fmt.Errorf("tip index: %w", err)
	}
	tipMan := &Manifest{}
	if unmarshal(tip.ManifestJSON, tipMan) != nil {
		return fmt.Errorf("tip manifest unreadable")
	}
	changed, _ := DiffIndex(tipIdx, curIdx)
	e.logf(id, "INFO", "Synthetic full: capturing only the delta from the source (%d changed file(s))", len(changed))

	// The fresh delta spools OUTSIDE the work dir — work's contents are packed
	// into the archive, and a synthetic full must look exactly like a normal one.
	tmp, err := os.CreateTemp(e.WorkDir, "dback-synth-*.tar")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)
	if _, terr := e.spoolVolumeTarFiles(ctx, cli, opts.ContainerID, changed, tmpPath); terr != nil {
		return fmt.Errorf("delta spool: %w", terr)
	}

	chain, cerr := e.resolveRestoreChain(tip, tipMan) // pin-checked, fail-closed
	if cerr != nil {
		return fmt.Errorf("chain resolve: %w", cerr)
	}
	sha, files, merr := e.synthesizeFullVolumes(ctx, chain, tmpPath, curIdx, changed, filepath.Join(work, "volumes.tar"))
	if merr != nil {
		return merr
	}
	if werr := writeVolIndex(work, curIdx); werr != nil {
		return werr
	}
	man.VolumesSHA256 = sha
	man.Incremental = false
	man.Parent = ""
	man.ParentCipherSHA256 = ""
	man.ChainDepth = 0
	man.Deleted = nil
	man.VolIndex = volumeIndexMember
	e.logf(id, "INFO", "Synthesized full baseline locally from the chain (%d generations merged, %d files) — source read was delta-only", len(chain)+1, files)
	return nil
}
