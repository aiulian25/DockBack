package dockercli

import (
	"errors"
	"strings"
	"testing"
)

// The sidecar runs on every managed node with read access to the data being
// backed up. These tests pin the refusal contract: a changed image is REFUSED,
// callers can recognise that refusal specifically, and the message tells the
// operator which of the two very different causes they are looking at.

func TestSidecarDigestChangedIsRecognisable(t *testing.T) {
	err := error(&SidecarDigestChangedError{
		Ref: "alpine:3.20", WantRef: "alpine:3.20",
		Want: "sha256:aaaa1111bbbb2222", Got: "sha256:cccc3333dddd4444",
	})
	if !errors.Is(err, ErrSidecarDigestChanged) {
		t.Fatal("a refusal must satisfy errors.Is(err, ErrSidecarDigestChanged)")
	}
	// Callers up the stack match with errors.As to reach the detail.
	var typed *SidecarDigestChangedError
	if !errors.As(err, &typed) || typed.Got == "" {
		t.Fatal("the typed error must survive errors.As")
	}
	// An ordinary failure must NOT be mistaken for a pin mismatch.
	if errors.Is(errors.New("connection refused"), ErrSidecarDigestChanged) {
		t.Fatal("an unrelated error must not match the sentinel")
	}
}

// A hijacked tag and a deliberate image change are different events needing
// different action, so they must not share a message.
func TestSidecarDigestChangedDistinguishesCause(t *testing.T) {
	sameTag := (&SidecarDigestChangedError{
		Ref: "alpine:3.20", WantRef: "alpine:3.20",
		Want: "sha256:aaaa1111bbbb2222cccc", Got: "sha256:dddd3333eeee4444ffff",
	}).Error()
	if !strings.Contains(sameTag, "changed since it was pinned") {
		t.Fatalf("a same-reference change must read as a possible tamper: %s", sameTag)
	}
	if !strings.Contains(sameTag, "alpine:3.20") {
		t.Fatalf("the message must name the image: %s", sameTag)
	}

	changedRef := (&SidecarDigestChangedError{
		Ref: "alpine:3.21", WantRef: "alpine:3.20", Want: "sha256:aaaa", Got: "sha256:bbbb",
	}).Error()
	if !strings.Contains(changedRef, "was changed from alpine:3.20 to alpine:3.21") {
		t.Fatalf("a reference change must say so plainly: %s", changedRef)
	}
	if strings.Contains(changedRef, "changed since it was pinned") {
		t.Fatal("a deliberate reference change must not be worded as a tamper")
	}
	// Both must point at the fix.
	for _, m := range []string{sameTag, changedRef} {
		if !strings.Contains(m, "re-pin") {
			t.Fatalf("every refusal must name the remedy: %s", m)
		}
	}
}

// The pin carries the reference alongside the digest, so the two causes above
// can be told apart at all.
func TestSidecarPinEncoding(t *testing.T) {
	pin := encodePin("alpine:3.20", "sha256:abc")
	ref, digest := decodePin(pin)
	if ref != "alpine:3.20" || digest != "sha256:abc" {
		t.Fatalf("round-trip failed: ref=%q digest=%q", ref, digest)
	}
	// A pin written before the reference was recorded still yields its digest,
	// so an upgrade doesn't invalidate every existing pin.
	ref, digest = decodePin("sha256:legacy")
	if ref != "" || digest != "sha256:legacy" {
		t.Fatalf("legacy pin must decode to a bare digest: ref=%q digest=%q", ref, digest)
	}
	// Exposed to the API layer for display.
	if r, d := SplitSidecarPin(pin); r != "alpine:3.20" || d != "sha256:abc" {
		t.Fatalf("SplitSidecarPin = %q/%q", r, d)
	}
}

func TestShortDigestStaysReadable(t *testing.T) {
	full := "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	got := shortDigest(full)
	if len(got) >= len(full) {
		t.Fatalf("a digest must be shortened for display, got %q", got)
	}
	if !strings.HasPrefix(got, "sha256:0123456789ab") {
		t.Fatalf("the shortened form must keep enough to compare: %q", got)
	}
	// A short or malformed value is passed through rather than sliced out of range.
	if shortDigest("sha256:ab") != "sha256:ab" {
		t.Fatal("a short digest must pass through unchanged")
	}
	if shortDigest("") != "" {
		t.Fatal("an empty digest must not panic")
	}
}

// An un-wired registry must behave exactly as the app did before this feature —
// pinning is a hardening layer, never a new way to fail.
func TestSidecarPinDisabledWhenUnwired(t *testing.T) {
	prev := activeRegistry.Load()
	t.Cleanup(func() { activeRegistry.Store(prev) })

	activeRegistry.Store(nil)
	if reg := activeRegistry.Load(); reg != nil {
		t.Fatal("with no registry published there is nothing to pin against")
	}

	// A registry that exists but was never wired to the store must also fall
	// through: LoadSidecarPin nil means "pinning not configured", not "refuse".
	r := NewRegistry()
	if r.LoadSidecarPin != nil {
		t.Fatal("a fresh registry must not claim to have pins")
	}
}

// nodeFor is what lets the choke point find a pin without threading a node id
// through every sidecar call site.
func TestRegistryNodeFor(t *testing.T) {
	r := NewRegistry()
	if got := r.nodeFor(nil); got != "" {
		t.Fatalf("a nil client must resolve to no node, got %q", got)
	}
}
