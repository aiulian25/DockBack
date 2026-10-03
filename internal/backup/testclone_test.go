package backup

import (
	"strconv"
	"testing"
	"time"

	"dockback/internal/dockercli"
)

// F219 — the reaper decides what gets DELETED, so its decision is tested to the
// edges. Everything here is pure: no Docker, no daemon, no timing.

func labeled(name, expiry string) *dockercli.Container {
	return &dockercli.Container{ID: "id-" + name, Name: name, State: "running",
		Labels: map[string]string{TestCloneLabel: expiry}}
}

func names(list []*dockercli.Container) []string {
	out := make([]string, 0, len(list))
	for _, c := range list {
		out = append(out, c.Name)
	}
	return out
}

func plain(name string) *dockercli.Container {
	return &dockercli.Container{ID: "id-" + name, Name: name, State: "running",
		Labels: map[string]string{"com.docker.compose.project": "blog"}}
}

// AC2 — expired clones go; unexpired and unlabeled ones stay. The second half
// is the one that matters: this function removes containers, and everything on
// the host that is not ours has to survive it.
func TestExpiredTestClonesPicksOnlyExpiredOnes(t *testing.T) {
	now := time.Now().Unix()
	list := []*dockercli.Container{
		labeled("app-test-0104", strconv.FormatInt(now-60, 10)),  // expired a minute ago
		labeled("db-test-0104", strconv.FormatInt(now+3600, 10)), // an hour to go
		plain("blog-app"),                                          // somebody's actual container
		{ID: "id-none", Name: "no-labels-at-all"},                  // no label map at all
		labeled("old-test-0101", strconv.FormatInt(now-86400, 10)), // expired yesterday
		nil,
	}

	got := expiredTestClones(list, now)
	if len(got) != 2 {
		t.Fatalf("want exactly the two expired clones, got %d: %v", len(got), names(got))
	}
	// Sorted by name, so a sweep reads the same way twice.
	if got[0].Name != "app-test-0104" || got[1].Name != "old-test-0101" {
		t.Errorf("expired set wrong or unsorted: %v", names(got))
	}
}

// A marker that cannot be read is not a verdict. "I don't understand this
// label" must never resolve to "delete the container".
func TestUnreadableExpiryIsNeverExpired(t *testing.T) {
	now := time.Now().Unix()
	for _, junk := range []string{"", "   ", "soon", "-1", "0", "1e9", "12.5", "9999999999999999999999"} {
		list := []*dockercli.Container{labeled("weird", junk)}
		if got := expiredTestClones(list, now); len(got) != 0 {
			t.Errorf("expiry %q must not be treated as expired", junk)
		}
		if _, ok := TestCloneExpiry(map[string]string{TestCloneLabel: junk}); ok {
			t.Errorf("expiry %q must not parse", junk)
		}
	}
}

// The boundary: reaching the expiry IS expiring, and one second short of it is
// not. Pinned because "expires at T" is the kind of thing that quietly drifts by
// a tick when someone rewrites the comparison.
func TestExpiryBoundary(t *testing.T) {
	now := time.Now().Unix()
	list := []*dockercli.Container{labeled("edge", strconv.FormatInt(now, 10))}
	if got := expiredTestClones(list, now-1); len(got) != 0 {
		t.Error("a second before its expiry the clone still has time")
	}
	if got := expiredTestClones(list, now); len(got) != 1 {
		t.Error("at its expiry it goes")
	}
}

// AC1 — the label round-trips: what the clone is stamped with is what the reaper
// reads back off it.
func TestTestCloneLabelRoundTrip(t *testing.T) {
	expiry := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	labels := TestCloneLabels(expiry)
	if len(labels) != 1 {
		t.Fatalf("one marker, not %d: %v", len(labels), labels)
	}
	// The com.dockback.* namespace, not the user-facing dockback.* one — a key in
	// the latter makes the UI report the container as label-managed.
	if _, ours := labels["com.dockback.test_clone"]; !ours {
		t.Errorf("the marker must stay in DockBack's own namespace: %v", labels)
	}
	got, ok := TestCloneExpiry(labels)
	if !ok {
		t.Fatal("the reaper must be able to read what the restore wrote")
	}
	if !got.Equal(expiry) {
		t.Errorf("expiry round-trip: wrote %v, read %v", expiry, got)
	}
	// A container carrying no marker is not a test clone, whatever else it has.
	if _, ok := TestCloneExpiry(map[string]string{"com.docker.compose.project": "blog"}); ok {
		t.Error("an ordinary container must never look like a test clone")
	}
	if _, ok := TestCloneExpiry(nil); ok {
		t.Error("no labels at all is not a test clone")
	}
}

// The drawer's list: every marked clone, expired or not, with its expiry, and
// nothing else on the host.
func TestLiveTestClonesListsOnlyClones(t *testing.T) {
	now := time.Now().Unix()
	list := []*dockercli.Container{
		plain("blog-app"),
		labeled("zeta-test-0104", strconv.FormatInt(now+7200, 10)),
		labeled("alpha-test-0104", strconv.FormatInt(now-10, 10)),
	}
	got := liveTestClones(list, "n1")
	if len(got) != 2 {
		t.Fatalf("want the two clones, got %d: %+v", len(got), got)
	}
	if got[0].Name != "alpha-test-0104" || got[1].Name != "zeta-test-0104" {
		t.Errorf("sorted by name: %+v", got)
	}
	if got[0].NodeID != "n1" || got[0].ExpiresAt != now-10 || got[0].State != "running" {
		t.Errorf("the row must carry what the UI shows: %+v", got[0])
	}
	// Non-nil, so a node with no clones serializes as [] rather than null.
	if liveTestClones(nil, "n1") == nil {
		t.Error("no clones is an empty list, not null")
	}
}

// The TTL wording in the confirm has to read like a person wrote it.
func TestHumanDurationReadsAsHoursOrDays(t *testing.T) {
	cases := map[time.Duration]string{
		time.Hour:       "1 hour",
		6 * time.Hour:   "6 hours",
		24 * time.Hour:  "24 hours",
		48 * time.Hour:  "2 days",
		168 * time.Hour: "7 days",
		36 * time.Hour:  "36 hours",
	}
	for d, want := range cases {
		if got := humanDuration(d); got != want {
			t.Errorf("humanDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
