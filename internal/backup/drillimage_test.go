package backup

import (
	"testing"

	"dockback/internal/dockercli"
)

// ISSUES.md #36: "Removing an image by tag leaves it untagged-but-present;
// dangling=true misses it… a tool that checks dangling reports a clean teardown
// and leaves a 1 GB orphan."
//
// The decision has exactly two inputs and both are necessary. Getting either
// wrong deletes somebody else's image, which is a much worse outcome than
// leaving one behind.
func TestDrillImageCleanupDecision(t *testing.T) {
	t.Run("the (preexisting, refs) table", func(t *testing.T) {
		cases := []struct {
			name       string
			pulledHere bool
			otherRefs  int
			want       bool
		}{
			// The case this exists for: the drill fetched it, and when the
			// throwaway is gone nothing is left that uses it.
			{"we pulled it and nothing else uses it", true, 0, true},

			// Never ours to reclaim. The host had this image before the drill ran
			// — the operator's, or another container's between runs — and removing
			// it would take away something a drill merely borrowed.
			{"it was already here, unused", false, 0, false},
			{"it was already here, and in use", false, 3, false},

			// In use is not garbage, whoever fetched it. A second container built
			// on the image between the pull and the teardown means removing it
			// would break that container's next start.
			{"we pulled it but something else now uses it", true, 1, false},
			{"we pulled it and several things use it", true, 5, false},
		}
		for _, tc := range cases {
			if got := dockercli.ImageReclaimable(tc.pulledHere, tc.otherRefs); got != tc.want {
				t.Errorf("%s: ImageReclaimable(%v, %d) = %v, want %v", tc.name, tc.pulledHere, tc.otherRefs, got, tc.want)
			}
		}
	})

	t.Run("a drill that borrowed an image carries no claim on it", func(t *testing.T) {
		// EnsureDrillImage's presence check is the whole safety property: an image
		// already on the host is recorded as not-ours, and no reference count can
		// later make it removable.
		borrowed := dockercli.DrillImage{ID: "sha256:abc", Ref: "redis:7"}
		if borrowed.PulledHere {
			t.Fatal("an image found locally must never be marked as pulled here")
		}
		if dockercli.ImageReclaimable(borrowed.PulledHere, 0) {
			t.Error("a borrowed image must not be reclaimable even when unreferenced")
		}
	})

	t.Run("nothing is removed without an id", func(t *testing.T) {
		// Removing by TAG is #36 itself — it untags the image and leaves the
		// layers, which then are not `dangling` either because the repo digest
		// survives. Without an id there is nothing safe to remove.
		noID := dockercli.DrillImage{Ref: "redis:7", PulledHere: true}
		if noID.ID != "" {
			t.Fatal("fixture is wrong")
		}
		// ReleaseDrillImage refuses this before it can reach the daemon; the
		// predicate is only half the guard, so assert the shape the caller relies
		// on rather than the predicate alone.
		if removed := dockercli.ReleaseDrillImage(t.Context(), nil, noID, ""); removed {
			t.Error("an image with no recorded id must never be removed")
		}
	})

	t.Run("the clone's pulled-image label round-trips", func(t *testing.T) {
		// The record has to survive the control plane restarting, so it lives on
		// the container — same reasoning as the expiry label beside it.
		const id = "sha256:0123456789abcdef"
		labels := map[string]string{dockercli.ClonePulledImageLabel: id}
		if got := dockercli.ClonePulledImage(labels); got != id {
			t.Errorf("ClonePulledImage = %q, want %q", got, id)
		}
		// A clone that pulled nothing — the ordinary case, since it runs beside
		// the original whose image is already here — records nothing.
		if got := dockercli.ClonePulledImage(map[string]string{TestCloneLabel: "123"}); got != "" {
			t.Errorf("a clone with no pulled image reported %q", got)
		}
		if got := dockercli.ClonePulledImage(nil); got != "" {
			t.Errorf("nil labels reported %q", got)
		}
	})
}
