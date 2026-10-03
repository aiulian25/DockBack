package backup

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
)

// Stack-atomic applications (F146).
//
// F141 established that some paths inside a container are only meaningful
// together. This is the same problem one level up, and it is worse, because the
// unit of a backup is a container: an application whose state is split across
// SERVICES gets backed up correctly one service at a time and is still
// unrestorable.
//
// Pangolin is the case that forces it. Its database — sites, resources, and the
// per-site secrets remote agents authenticate with — lives in one container. The
// WireGuard private key that IS the tunnel endpoint's identity lives in another.
// The certificate store lives in a third. Restore any one of them without the
// others and the service comes back healthy while the deployment does not work:
// agents authenticate against an identity the database no longer matches, and
// nothing anywhere says so.
//
// The set is discovered from the DEPLOYMENT, not declared in a list. One anchor
// image says "this application is stack-atomic"; every container sharing its
// compose project is a member. That is the only form of this that survives
// contact with somebody else's machine, where the services may be named
// differently, an optional one may be absent, and another may have been added.
//
// Enforced at three points:
//
//   - CAPTURE, stack path: a member that cannot be prepared fails the whole run
//     rather than being skipped with a warning. A group missing a member is the
//     archive this feature exists to prevent.
//   - CAPTURE, single-container path: a loud warning naming the stack backup,
//     and the archive records that it is one member of a set — so a restore
//     years later knows what it is holding even if the registry has changed.
//   - RESTORE: a single-member in-place restore is refused, and a stack restore
//     must come from one complete app-consistent snapshot. An isolated CLONE is
//     always allowed: it touches nothing live, and inspecting a backup must not
//     require permission.

// StackAtomicFor returns the stack-atomic declaration an image anchors, or nil.
// Only the ANCHOR image carries it — its sidecars have no profile of their own,
// which is why membership is resolved from the compose project instead.
func StackAtomicFor(image string) *StackAtomicSet {
	p := ProfileFor(image)
	if p == nil {
		return nil
	}
	return p.StackAtomic
}

// StackAtomicAnchor finds the anchor container of a stack-atomic application
// among a set of containers, returning its declaration and the anchor's image.
//
// Nil for the overwhelming majority of stacks, which anchor nothing — so the
// ordinary stack backup and restore paths are unchanged.
func StackAtomicAnchor(cs []*dockercli.Container) (*StackAtomicSet, string) {
	for _, c := range cs {
		if c == nil {
			continue
		}
		if set := StackAtomicFor(c.Image); set != nil {
			return set, c.Image
		}
	}
	return nil, ""
}

// stackAtomicContext answers, for one container's compose project, "is this
// service part of an atomic set, and what is the rest of it?"
//
// One container-list call, and only for a container that belongs to a compose
// project at all — a standalone container cannot be a member of anything. A
// listing that fails yields no set rather than a guess: this drives a warning
// and a later refusal, and neither should rest on an unanswered question.
func (e *Engine) stackAtomicContext(ctx context.Context, cli *client.Client, project string) (*StackAtomicSet, []StackMember) {
	if project == "" || cli == nil {
		return nil, nil
	}
	cs, err := dockercli.ListContainers(ctx, cli)
	if err != nil {
		return nil, nil
	}
	var members []*dockercli.Container
	for _, c := range cs {
		if c != nil && c.Stack == project {
			members = append(members, c)
		}
	}
	set, _ := StackAtomicAnchor(members)
	if set == nil {
		return nil, nil
	}
	return set, stackAtomicMembers(members)
}

// stackMemberOf records one container as a member of an atomic set.
func stackMemberOf(c *dockercli.Container) StackMember {
	if c == nil {
		return StackMember{}
	}
	svc := c.Service
	if svc == "" {
		svc = c.Name
	}
	return StackMember{Service: svc, Container: c.Name, Image: c.Image}
}

// stackAtomicMembers lists every container of a project as members of the set,
// in a deterministic order so two runs of the same stack record the same thing
// and a manifest diff means something.
func stackAtomicMembers(cs []*dockercli.Container) []StackMember {
	out := make([]StackMember, 0, len(cs))
	for _, c := range cs {
		if c != nil {
			out = append(out, stackMemberOf(c))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}

// memberServices lists the service names of a recorded set.
func memberServices(ms []StackMember) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		if m.Service != "" {
			out = append(out, m.Service)
		}
	}
	return out
}

// StackAtomicSoloRestoreVerdict refuses an in-place restore of ONE member of a
// stack-atomic application (F146).
//
// clone is true for an isolated copy under a new name, which is always allowed:
// it overwrites nothing, it is how a backup is inspected, and refusing it would
// mean the only way to look inside this archive is to restore it for real.
//
// Judged from what the ARCHIVE recorded. A backup taken before this existed
// carries no marker and is not blocked by a rule it predates — the profile is
// deliberately not consulted as a fallback here, because unlike a missing volume
// (a fact about the archive) this is a fact about the deployment at capture
// time, and an old archive simply does not have it.
func StackAtomicSoloRestoreVerdict(man *Manifest, clone bool) error {
	if man == nil || clone || man.StackAtomic == nil {
		return nil
	}
	sa := man.StackAtomic
	if len(sa.Members) < 2 {
		// A one-member "set" is not a set. Nothing to hold together.
		return nil
	}
	self := man.Service
	if self == "" {
		self = man.TargetName
	}
	others := make([]string, 0, len(sa.Members))
	for _, m := range sa.Members {
		if m.Service != self {
			others = append(others, m.Service)
		}
	}
	sort.Strings(others)

	// Naming the snapshot turns the advice into an instruction the operator can
	// act on without going looking for which one to pick.
	from := ""
	if sa.GroupID != "" {
		from = " This backup belongs to snapshot " + sa.GroupID + " — restore the stack from that one."
	}
	return fmt.Errorf("%s. This backup is one service (%s) of a set that also contains %s, so %s. %s%s "+
		"Nothing on the target has been stopped, overwritten or deleted",
		sa.Why, self, strings.Join(others, ", "), sa.Symptom, sa.SoloRestore, from)
}

// StackAtomicGroupVerdict decides whether a STACK restore may proceed for a
// stack-atomic application (F146).
//
// Two things have to be true, and the second is the one that is easy to miss:
//
//   - every member of the recorded set must have a backup in this restore, and
//   - they must all come from ONE app-consistent snapshot.
//
// The second matters because the default stack restore takes each service's
// newest backup, which for an ordinary stack is a reasonable convenience and for
// this one silently mixes points in time. A tunnel key from Tuesday and a
// database from Thursday are both real backups and together they are not a
// deployment.
//
// selected maps service name → the manifest chosen for it.
func StackAtomicGroupVerdict(selected map[string]*Manifest) error {
	var set *StackAtomicRef
	for _, m := range selected {
		if m != nil && m.StackAtomic != nil && len(m.StackAtomic.Members) >= 2 {
			set = m.StackAtomic
			break
		}
	}
	if set == nil {
		return nil
	}

	var missing []string
	for _, want := range memberServices(set.Members) {
		if selected[want] == nil {
			missing = append(missing, want)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		return fmt.Errorf("%s. This restore does not include %s, so %s. "+
			"Restore from an app-consistent snapshot that covers every service, or take one first — nothing has been changed",
			set.Why, strings.Join(missing, " or "), set.Symptom)
	}

	// One snapshot, not several. An empty group id means the member was captured
	// on its own, which is exactly the mixed-point-in-time case.
	groups := map[string]bool{}
	var solo []string
	for svc, m := range selected {
		if m == nil {
			continue
		}
		g := m.ConsistencyGroup
		if g == "" {
			solo = append(solo, svc)
			continue
		}
		groups[g] = true
	}
	sort.Strings(solo)
	switch {
	case len(solo) > 0:
		return fmt.Errorf("%s. %s %s captured on %s own rather than as part of a snapshot of the whole stack, so this restore would mix moments in time — %s. "+
			"Take an app-consistent stack backup and restore from that",
			set.Why, strings.Join(solo, ", "), plural(len(solo), "was", "were"), plural(len(solo), "its", "their"), set.Symptom)
	case len(groups) > 1:
		return fmt.Errorf("%s. The chosen backups come from %d different snapshots, so this restore would mix moments in time — %s. "+
			"Pick one app-consistent snapshot that covers every service",
			set.Why, len(groups), set.Symptom)
	}
	return nil
}

// plural picks between two words by count, so a message reads correctly for one
// service and for several without building a sentence out of fragments.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
