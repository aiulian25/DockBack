package dockercli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/docker/docker/client"
)

// Volume-sidecar image pinning (F88).
//
// The sidecar runs on EVERY managed node with `VolumesFrom: [target:ro]` — that
// is, with read access to the data being backed up — and on the machine-probe
// path with the host's /proc, /sys and root tree mounted read-only. Every base
// image in DockBack's own Dockerfile is pinned by digest for exactly this
// reason, but the sidecar was pinned by TAG only: a repushed or hijacked
// `alpine:3.20` would be pulled and executed with that access, silently.
//
// This applies the same trust-on-first-use-then-refuse posture SSH host keys
// already get (F1/F67): the digest actually used is recorded per node on first
// use, and any later change REFUSES the operation rather than running an image
// nobody vouched for.
//
// WHY THE CHECK LIVES HERE AND NOT AT EACH CALL SITE: roughly twenty places
// spawn a sidecar, all of them holding only a *client.Client with no idea which
// node it belongs to. Threading a "wanted digest" argument through all of them
// would work right up until someone adds the twenty-first and forgets — and a
// missed call site is an unprotected sidecar with no visible symptom. So the
// check sits at the single choke point every one of them already goes through,
// resolving the node from the client via the registry.

// ErrSidecarDigestChanged is the sentinel callers match with errors.Is to tell a
// REFUSED pin mismatch (a possibly-hijacked image) from an ordinary pull failure.
var ErrSidecarDigestChanged = errors.New("sidecar image digest changed")

// SidecarDigestChangedError is the typed refusal. It unwraps to
// ErrSidecarDigestChanged and carries both sides so the alert layer can dedup
// per node and name exactly what changed.
type SidecarDigestChangedError struct {
	Ref     string // the image reference in use, e.g. alpine:3.20
	WantRef string // the reference recorded when the pin was taken
	Want    string // pinned digest
	Got     string // digest actually present now
}

func (e *SidecarDigestChangedError) Error() string {
	// A deliberate reference change and a hijacked tag are different events and
	// need different advice, so they get different sentences.
	if e.WantRef != "" && e.WantRef != e.Ref {
		return fmt.Sprintf(
			"the volume sidecar image was changed from %s to %s — re-pin it in the node's settings if this was intended",
			e.WantRef, e.Ref)
	}
	return fmt.Sprintf(
		"the volume sidecar image changed since it was pinned (%s: was %s, now %s) — re-pin it in the node's settings if this was intended",
		e.Ref, shortDigest(e.Want), shortDigest(e.Got))
}

func (e *SidecarDigestChangedError) Unwrap() error { return ErrSidecarDigestChanged }

func shortDigest(d string) string {
	if i := strings.Index(d, ":"); i >= 0 && len(d) > i+13 {
		return d[:i+13] + "…"
	}
	return d
}

// activeRegistry lets the choke point resolve a client back to its node without
// changing twenty function signatures. Set once when the registry is created;
// nil means pinning is simply not wired (tests, or an embedder that never made
// a registry), and the sidecar behaves exactly as it did before this feature.
var activeRegistry atomic.Pointer[Registry]

// SidecarDigest returns the digest of the sidecar image as it exists on this
// node right now.
//
// Prefers the REPO digest (`alpine@sha256:…`), which is the registry's content
// address and therefore what a pin should be about. Falls back to the local
// image ID for an image that was side-loaded (`docker load`) and so has no repo
// digest — pinning that is still worth doing, it just pins the local content.
func SidecarDigest(ctx context.Context, c *client.Client) (string, error) {
	insp, _, err := c.ImageInspectWithRaw(ctx, sidecarRef())
	if err != nil {
		return "", err
	}
	for _, rd := range insp.RepoDigests {
		if i := strings.Index(rd, "@"); i >= 0 {
			return rd[i+1:], nil
		}
	}
	if insp.ID != "" {
		return insp.ID, nil
	}
	return "", fmt.Errorf("image %s reports no digest", sidecarRef())
}

// encodePin / decodePin store the reference alongside the digest.
//
// Keeping the ref matters: an operator who deliberately points
// DOCKBACK_SIDECAR_IMAGE at a new image should be told "you changed the image",
// not "your image may have been hijacked". Same refusal, honest reason.
func encodePin(ref, digest string) string { return ref + "|" + digest }

func decodePin(pin string) (ref, digest string) {
	if i := strings.Index(pin, "|"); i >= 0 {
		return pin[:i], pin[i+1:]
	}
	return "", pin // a pin written before the ref was recorded
}

// SplitSidecarPin exposes a stored pin's parts to the API layer for display.
func SplitSidecarPin(pin string) (ref, digest string) { return decodePin(pin) }

// ensureSidecar makes the sidecar image available AND verifies it against this
// node's pin. Every sidecar spawn goes through it.
//
// On a node with no pin yet it records what is present (trust on first use) and
// proceeds. On a mismatch it REFUSES — the image is not run.
func ensureSidecar(ctx context.Context, c *client.Client) error {
	if err := ensureImage(ctx, c, sidecarRef()); err != nil {
		return err
	}
	reg := activeRegistry.Load()
	if reg == nil || reg.LoadSidecarPin == nil {
		return nil // pinning not wired — behave exactly as before the feature
	}
	nodeID := reg.nodeFor(c)
	if nodeID == "" {
		return nil // a client the registry doesn't own (a one-off probe); nothing to pin against
	}

	got, err := SidecarDigest(ctx, c)
	if err != nil {
		// Never fail a backup because the DIGEST couldn't be read — that would turn
		// a hardening measure into an outage. The image itself already pulled fine.
		return nil
	}

	pin, ok := reg.LoadSidecarPin(nodeID)
	if !ok || pin == "" {
		if reg.SaveSidecarPin != nil {
			reg.SaveSidecarPin(nodeID, encodePin(sidecarRef(), got))
		}
		return nil
	}
	wantRef, want := decodePin(pin)
	if want == got && (wantRef == "" || wantRef == sidecarRef()) {
		return nil
	}
	return &SidecarDigestChangedError{Ref: sidecarRef(), WantRef: wantRef, Want: want, Got: got}
}

// nodeFor resolves the node a cached client belongs to, so the choke point can
// find that node's pin. Empty when the client isn't one of ours.
func (r *Registry) nodeFor(c *client.Client) string {
	if c == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, cl := range r.clients {
		if cl == c {
			return id
		}
	}
	return ""
}
