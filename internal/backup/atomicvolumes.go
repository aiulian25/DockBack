package backup

import (
	"fmt"
	"strings"
)

// Atomic volume sets (F141).
//
// Mount selection is per-mount, and for almost every container that is exactly
// right: leaving a large media bind out produces a smaller backup of the same
// application, and putting it back produces the same application again.
//
// A few applications are not like that. Nginx Proxy Manager keeps its proxy
// hosts, users and signing keys in /data and the certificates those hosts are
// configured to serve in /etc/letsencrypt. The two are one configuration stored
// in two places, and half of it is not a smaller backup — it is an archive whose
// restore produces either a proxy that will not start ("cannot load
// certificate") or, worse, a proxy that starts factory-fresh with every route
// gone and an unauthenticated setup wizard waiting.
//
// So the set is enforced at both ends:
//
//   - CAPTURE keeps the members together. A member that was deselected — or
//     that fell out of the size-based default because its size could not be
//     measured — is put back, loudly, and the archive records the set it was
//     required to contain.
//
//   - RESTORE refuses an archive that holds only part of the set. That is a
//     refusal to act, not a data-destroying one: the archive is intact, every
//     file in it can still be extracted individually, and the live deployment
//     is left exactly as it was.

// AtomicVolumesFor returns the atomic volume set an image's application
// requires, or nil for the overwhelming majority of images, which have none.
func AtomicVolumesFor(image string) *AtomicVolumeSet {
	p := ProfileFor(image)
	if p == nil {
		return nil
	}
	return p.AtomicVolumes
}

// CertificateRootsFor returns the container directories whose TLS certificates
// this image's application depends on (F143). Nil for images with no profile.
func CertificateRootsFor(image string) []string {
	p := ProfileFor(image)
	if p == nil {
		return nil
	}
	return p.CertificateRoots
}

// enforceAtomicSelection adds back any member of an atomic set that the
// selection left out, given the destinations the container actually mounts.
//
// It returns the destinations that were re-added, so the caller can say so. The
// selection stored for the container is deliberately NOT rewritten: the
// operator's choice stays theirs, and every run re-applies this on top of it, so
// turning the requirement off later restores their selection unchanged.
//
// Force-including rather than refusing is a deliberate trade. Refusing would
// leave an operator who deselected one 400 KB directory with no backup of their
// ingress configuration at all, which is a worse outcome than a backup slightly
// larger than they asked for. The members of a set like this are small by
// nature — they are configuration, not data — so the cost of being wrong in
// this direction is bytes, and the cost of being wrong in the other is the
// configuration.
func enforceAtomicSelection(set *AtomicVolumeSet, mounted []string, chosen map[string]bool) []string {
	if set == nil || len(chosen) == 0 {
		return nil
	}
	// Only act when this backup is actually capturing part of the set. A backup
	// that touches none of it is not a partial capture of the set — it is a
	// backup of something else on the same container.
	touches := false
	for d := range chosen {
		if set.Has(d) {
			touches = true
			break
		}
	}
	if !touches {
		return nil
	}
	var added []string
	for _, d := range mounted {
		if set.Has(d) && !chosen[d] {
			chosen[d] = true
			added = append(added, d)
		}
	}
	return added
}

// atomicSetRequired lists the members of the set that the container actually
// mounts, in the set's own order — what the archive is required to contain, as
// opposed to what the application would like it to.
//
// A member the source container does not mount at all cannot be required of the
// archive: there is nothing to capture. That is its own problem (the data lives
// in the container's writable layer and disappears with the container), and it
// is reported separately at capture rather than turned into a restore that can
// never succeed.
func atomicSetRequired(set *AtomicVolumeSet, mounted []string) (required, absent []string) {
	if set == nil {
		return nil, nil
	}
	have := map[string]bool{}
	for _, d := range mounted {
		have[d] = true
	}
	for _, p := range set.Paths {
		if have[p] {
			required = append(required, p)
		} else {
			absent = append(absent, p)
		}
	}
	return required, absent
}

// AtomicVolumeVerdict decides whether a backup may be restored at all, given the
// atomic volume set its application requires (F141).
//
// The error it returns is the whole feature: a restore that would produce a
// half-configured proxy is refused BEFORE anything is stopped, snapshotted or
// overwritten, and the message says what is missing, why it matters, and what
// can still be done with the archive.
//
// Two sources of truth, in order:
//
//   - man.AtomicVolumes, recorded at capture. Authoritative, because it says
//     what THIS archive was required to contain, and stays right even if the
//     profile changes later.
//   - the profile, for archives taken before this existed. Those recorded
//     nothing, so the check falls back to what the application requires — but
//     only when the archive holds part of the set already, so an old backup of
//     something unrelated is never blocked by a rule it predates.
func AtomicVolumeVerdict(man *Manifest) error {
	if man == nil {
		return nil
	}
	captured := map[string]bool{}
	for _, v := range man.Volumes {
		if v.Destination != "" {
			captured[v.Destination] = true
		}
	}

	set := AtomicVolumesFor(man.Image)
	required := man.AtomicVolumes
	if len(required) == 0 {
		if set == nil {
			return nil
		}
		// Legacy archive: judge it only if it plainly holds part of the set.
		any := false
		for _, p := range set.Paths {
			if captured[p] {
				any = true
				break
			}
		}
		if !any {
			return nil
		}
		required = set.Paths
	}

	var missing []string
	for _, p := range required {
		if !captured[p] {
			missing = append(missing, p)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	// Prefer the profile's own words. Without a profile — an archive whose image
	// DockBack no longer recognises — the recorded set still stands on its own.
	reason := "This backup was required to contain " + strings.Join(required, " and ") + ", which are only meaningful together"
	symptom := "restoring part of it would leave the application with half a configuration"
	if set != nil {
		reason, symptom = set.Why, set.Symptom
	}

	return fmt.Errorf("%s. This archive is missing %s, so %s. "+
		"The restore was refused before anything was changed — nothing on the target has been stopped, overwritten or deleted. "+
		"Restore a backup that contains all of %s, or open this one's file list and put individual files back by hand",
		reason, strings.Join(missing, " and "), symptom, strings.Join(required, " and "))
}
