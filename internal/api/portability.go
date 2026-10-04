package api

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types/network"

	"dockback/internal/backup"
	"dockback/internal/dockercli"
)

// Cross-host portability preflight (F94).
//
// Restoring onto a different node is a first-class feature, but nothing checked
// whether the target could honor what the container asks of its host. A
// hardware-transcoding container restored onto a machine with no GPU was
// created, started, and failed with a raw Docker error — after the data had
// already been written.
//
// This compares the recorded requirements against the target and says what it
// cannot provide, BEFORE the restore runs.
//
// The warnings never block. A device the target verifiably lacks does (step 27),
// until the operator confirms it: they may know it will be attached shortly,
// and a restore that fails after writing the data helps nobody.

// hostFacts is what we managed to learn about a target host. Separated from the
// probing so the judgement below is pure and table-testable without Docker.
type hostFacts struct {
	Devices   map[string]bool // device paths present on the host
	DevKnown  bool            // whether the device list could be read at all
	HasGPU    bool            // an NVIDIA/DRI node is present
	LogDriver string          // the daemon's default logging driver ("" = unknown)
	Target    string          // node name, for the messages
}

// missingDevices lists the recorded devices the target verifiably lacks. Only
// a fact counts: a device list that could not be read blocks nothing. Pure.
func missingDevices(req *backup.HostRequirements, facts hostFacts) []string {
	if req == nil || !facts.DevKnown {
		return nil
	}
	var missing []string
	for _, d := range req.Devices {
		if !facts.Devices[d] {
			missing = append(missing, d)
		}
	}
	return missing
}

// missingDevicesOn is missingDevices for a cross-host restore, and nothing for
// one that stays on its own node (step 27). gluetun without /dev/net/tun and
// Plex without its GPU were created, started and then failed with a raw Docker
// error, after the data had been written; a missing device now stops the
// restore until the operator says it will be there.
func (s *Server) missingDevicesOn(ctx context.Context, man *backup.Manifest, originNodeID, targetNodeID string) []string {
	if targetNodeID == "" || targetNodeID == originNodeID || man == nil || man.Requires == nil || len(man.Requires.Devices) == 0 {
		return nil
	}
	return missingDevices(man.Requires, s.targetHostFacts(ctx, targetNodeID))
}

// sharedNetworksMissing lists the networks a container joins without owning
// them that the target does not have (step 27). A network the stack owns is
// recreated as recorded, which is right; one another stack or the operator
// created (a proxy network, a macvlan) recreated as a plain bridge cuts the
// container off from everything it was meant to reach. Pure.
func sharedNetworksMissing(man *backup.Manifest, project string, existing map[string]bool) []string {
	if man == nil {
		return nil
	}
	var missing []string
	for _, n := range man.Networks {
		owned := project != "" && n.Labels["com.docker.compose.project"] == project
		if !owned && !existing[n.Name] {
			missing = append(missing, n.Name)
		}
	}
	return missing
}

// sharedNetworksMissingOn is sharedNetworksMissing for a cross-host restore.
// Silent when the target's networks cannot be listed: that is not a fact.
func (s *Server) sharedNetworksMissingOn(ctx context.Context, man *backup.Manifest, project, originNodeID, targetNodeID string) []string {
	if targetNodeID == "" || targetNodeID == originNodeID || man == nil || len(man.Networks) == 0 {
		return nil
	}
	cli, err := s.reg.Get(targetNodeID)
	if err != nil {
		return nil
	}
	lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	nets, err := cli.NetworkList(lctx, network.ListOptions{})
	if err != nil {
		return nil
	}
	existing := make(map[string]bool, len(nets))
	for _, n := range nets {
		existing[n.Name] = true
	}
	return sharedNetworksMissing(man, project, existing)
}

// portabilityWarnings judges a backup's host requirements against a target.
//
// Distinguishes three states deliberately, because they call for different
// action: VERIFIED-MISSING ("the target does not have this"), UNVERIFIABLE
// ("could not check"), and NOT-CHECKABLE ("only the target can tell you"). Only
// the first is a statement of fact.
func portabilityWarnings(req *backup.HostRequirements, facts hostFacts) []string {
	if req == nil {
		return nil
	}
	target := facts.Target
	if target == "" {
		target = "the target host"
	}
	var out []string

	for _, d := range req.Devices {
		switch {
		case !facts.DevKnown:
			out = append(out, fmt.Sprintf("could not verify device %s on %s — check it exists before relying on this restore", d, target))
		case facts.Devices[d]:
			// Present: say nothing. A warning for something that is fine trains
			// the operator to skip the list.
		default:
			out = append(out, fmt.Sprintf("requires device %s — not present on %s", d, target))
		}
	}

	if req.GPUs > 0 {
		switch {
		case !facts.DevKnown:
			out = append(out, fmt.Sprintf("could not verify GPU availability on %s — this container reserves %d GPU(s)", target, req.GPUs))
		case !facts.HasGPU:
			out = append(out, fmt.Sprintf("reserves %d GPU(s) — no GPU device is present on %s", req.GPUs, target))
		default:
			// A card being present is not the same as the NVIDIA container runtime
			// being installed, and only the latter makes `--gpus` work.
			out = append(out, fmt.Sprintf("reserves %d GPU(s) — %s has a GPU, but this also needs its container runtime (nvidia-container-toolkit) installed", req.GPUs, target))
		}
	}

	if req.LogDriver != "" {
		switch {
		case facts.LogDriver == "":
			out = append(out, fmt.Sprintf("could not verify the logging driver on %s — this container uses %q", target, req.LogDriver))
		case facts.LogDriver != req.LogDriver:
			out = append(out, fmt.Sprintf("uses the %q logging driver — %s defaults to %q; the driver must be available there or the container will not start", req.LogDriver, target, facts.LogDriver))
		}
	}

	// Recorded but not probed: whether a host permits these depends on its kernel
	// and daemon policy, and the only reliable test is trying. Saying "could not
	// verify" for each would be noise, so they are reported once, plainly.
	if req.Privileged {
		out = append(out, fmt.Sprintf("runs PRIVILEGED — %s must allow privileged containers", target))
	}
	// F128: a Docker socket is authority, not data. Restoring such a container
	// onto another machine hands that machine's daemon to it, which is a decision
	// worth stating rather than replaying in silence. The mount is reproduced
	// exactly as recorded — never widened — but the operator should know what
	// they are moving.
	switch req.DockerSocket {
	case "rw":
		out = append(out, fmt.Sprintf("is given READ-WRITE access to the Docker socket — on %s that is full control of its Docker daemon, which is equivalent to root on that host. It is reproduced exactly as configured, never widened.", target))
	case "ro":
		out = append(out, fmt.Sprintf("is given read-only access to the Docker socket — it will see %s's containers instead of the original host's. Reproduced exactly as configured, never widened.", target))
	}
	if len(req.CapAdd) > 0 {
		out = append(out, fmt.Sprintf("needs extra capabilities (%s) — %s must permit them", strings.Join(req.CapAdd, ", "), target))
	}
	if len(req.Sysctls) > 0 {
		keys := make([]string, 0, len(req.Sysctls))
		for k := range req.Sysctls {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out = append(out, fmt.Sprintf("sets kernel parameters (%s) — %s's kernel must accept them", strings.Join(keys, ", "), target))
	}
	return out
}

// targetHostFacts gathers what can be learned about a node, cheaply.
//
// Device presence comes from the CACHED machine probe (F105) rather than a new
// container per dialog open — opening a restore dialog must not spawn work on a
// production host. When nothing is cached yet a refresh is kicked off in the
// background and the warnings honestly say "could not verify"; the next open has
// the answer.
func (s *Server) targetHostFacts(ctx context.Context, nodeID string) hostFacts {
	facts := hostFacts{Devices: map[string]bool{}}
	if n, err := s.store.GetNode(nodeID); err == nil {
		facts.Target = n.Name
	}
	cli, err := s.reg.Get(nodeID)
	if err != nil {
		return facts // unreachable: everything reads as "could not verify"
	}

	if e := s.machineLoad(nodeID); e != nil && e.info != nil {
		facts.DevKnown = len(e.info.Devices) > 0
		for _, d := range e.info.Devices {
			facts.Devices[d] = true
			if strings.HasPrefix(d, "/dev/nvidia") || strings.HasPrefix(d, "/dev/dri/") {
				facts.HasGPU = true
			}
		}
		if len(e.info.GPUs) > 0 {
			facts.HasGPU = true
		}
	} else {
		// Nothing cached: start one so the NEXT check can answer, and report
		// honestly in the meantime rather than guessing.
		s.machineRefresh(nodeID, cli)
	}

	ictx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if info, ierr := cli.Info(ictx); ierr == nil {
		facts.LogDriver = info.LoggingDriver
	}
	return facts
}

// portabilityFor is the endpoint-facing helper: warnings for restoring this
// backup onto targetNodeID, or nothing when that is where it came from.
func (s *Server) portabilityFor(ctx context.Context, man *backup.Manifest, originNodeID, targetNodeID string) []string {
	if targetNodeID == "" || targetNodeID == originNodeID || man == nil || man.Requires == nil {
		return nil
	}
	return portabilityWarnings(man.Requires, s.targetHostFacts(ctx, targetNodeID))
}

// Application restore preconditions (F110).
//
// F94 above answers "can this HOST run the container?" — devices, GPU, kernel.
// This answers the question that comes after it and had no answer at all: "will
// the APPLICATION mean anything once it does?"
//
// An app that stores absolute container paths in its own database restores onto
// a new host perfectly — same image, same data, healthy container — and then
// reports its entire library as missing, because the new host mounts the media
// somewhere else. Nothing in the archive is wrong. Only saying so beforehand,
// with the recorded destinations named, prevents it.
//
// Shown for BOTH same-host and cross-host restores: a same-host recreate can
// still land on an edited compose file with different mount paths.

// appPreconditionsFor returns the profile-driven preconditions for a restore,
// or nil for the overwhelming majority of images, which have no profile.
func appPreconditionsFor(man *backup.Manifest) *backup.AppRestorePreconditions {
	return backup.AppPreconditionsFor(man)
}

// Shared-data restore coupling (F119).
//
// F83 already knows which containers mount the same host directory, and uses it
// at BACKUP time to capture a shared folder once instead of several times. The
// same fact matters just as much at RESTORE time, and nothing said so.
//
// Two apps that share a directory are one logical unit: Calibre manages a
// library that calibre-web reads, and calibre-web's own database references
// books by the ids in that library. Rolling the library back to last week while
// the reader's database stays current leaves a catalogue full of entries
// pointing at books that are no longer there — from a restore that reported
// success.

// sharedDataWarnings reports containers on the target node that mount the same
// host directories this backup restores, so the operator knows the restore
// reaches beyond the container they selected.
//
// Reads the cached inventory only — no Docker round-trip, so opening a restore
// dialog costs nothing. Silent when nothing is shared, which is most containers.
func (s *Server) sharedDataWarnings(nodeID string, man *backup.Manifest, selfName string) []string {
	if man == nil || len(man.Volumes) == 0 {
		return nil
	}
	st := s.getStat(nodeID)
	if st == nil {
		return nil
	}
	// Host sources this restore will write into. Named volumes are excluded:
	// their Source is a daemon-managed path that another container reaches by
	// volume NAME, and treating those as shared would fire on ordinary setups.
	mine := map[string]bool{}
	for _, v := range man.Volumes {
		if v.Type == "bind" && v.Source != "" {
			mine[v.Source] = true
		}
	}
	if len(mine) == 0 {
		return nil
	}
	sharers := map[string]map[string]bool{} // other container -> the paths it shares
	for _, c := range st.Containers {
		if c == nil || c.Name == selfName {
			continue
		}
		for _, m := range c.Mounts {
			if m.Type != "bind" || m.Source == "" || !mine[m.Source] {
				continue
			}
			if sharers[c.Name] == nil {
				sharers[c.Name] = map[string]bool{}
			}
			sharers[c.Name][m.Source] = true
		}
	}
	if len(sharers) == 0 {
		return nil
	}
	names := make([]string, 0, len(sharers))
	for n := range sharers {
		names = append(names, n)
	}
	sort.Strings(names) // deterministic message order

	out := make([]string, 0, len(names))
	for _, n := range names {
		paths := make([]string, 0, len(sharers[n]))
		for p := range sharers[n] {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		out = append(out, fmt.Sprintf("%s also uses %s — this restore overwrites it. If the two share data, restore them together; rolling one back while the other stays current can leave them referring to files that are no longer there.",
			n, strings.Join(paths, ", ")))
	}
	return out
}

// Published-port conflicts (F144).
//
// A container recreated on a machine where something else already holds one of
// its ports is created and then fails to start, with a Docker error that names
// the port and nothing else — leaving the operator to work out which of the
// forty containers on that host is holding it. The backup records what the
// container published; the cached inventory knows what the target is already
// using; saying so before the restore costs nothing.
//
// Advisory, like everything else here. The conflicting container may be the one
// this restore is about to replace, or one the operator is about to stop.

// portConflicts reports which of a backup's published host ports are already
// held on the target node, and by what.
//
// Reads the cached inventory only — no Docker round-trip. Silent for a backup
// that published nothing, for a target with no inventory yet, and for the
// container being restored into itself, which legitimately holds its own ports.
func (s *Server) portConflicts(nodeID string, man *backup.Manifest, selfName string) []string {
	if man == nil || len(man.PublishedPorts) == 0 || nodeID == "" {
		return nil
	}
	st := s.getStat(nodeID)
	if st == nil {
		return nil
	}
	holder := map[int]string{}
	for _, c := range st.Containers {
		if c == nil || c.Name == selfName {
			continue
		}
		// A stopped container holds nothing. Its ports are recorded, but the
		// binding only exists while it runs, so reporting it as a conflict would
		// be wrong.
		if c.State != "running" && c.State != "restarting" {
			continue
		}
		for _, p := range dockercli.PublishedHostPorts(c.Ports) {
			if _, taken := holder[p]; !taken {
				holder[p] = c.Name
			}
		}
	}
	if len(holder) == 0 {
		return nil
	}
	// One line per conflicting port, in port order, so the list reads the same
	// way twice.
	seen := map[int]bool{}
	var ports []int
	for _, p := range man.PublishedPorts {
		if !seen[p.HostPort] {
			seen[p.HostPort] = true
			ports = append(ports, p.HostPort)
		}
	}
	sort.Ints(ports)
	var out []string
	for _, p := range ports {
		if h := holder[p]; h != "" {
			out = append(out, fmt.Sprintf("port %d is already published by %s — the restored container cannot bind it until that one releases it", p, h))
		}
	}
	return out
}

// localOnlyStorageVerdict checks the profile's local-disk-only paths against the
// filesystem actually backing them on the target (F110).
//
// SQLite over CIFS/NFS does not fail loudly — it loses the advisory locking its
// durability depends on and corrupts days later, far from the restore that
// caused it. That is why a MEASURED network filesystem blocks rather than warns.
//
// Keeps the same three-state honesty as portabilityWarnings: only a filesystem
// we actually read blocks. When the target container does not exist yet (a
// recreate) or cannot be probed, nothing is measured, so nothing is claimed —
// the advisory precondition text already carries the requirement.
func localOnlyStorageVerdict(man *backup.Manifest, fstypes map[string]string) (warnings []string, blocking bool) {
	if man == nil || len(fstypes) == 0 {
		return nil, false
	}
	p := backup.ProfileFor(man.Image)
	if p == nil {
		return nil, false
	}
	dests := make([]string, 0, len(fstypes))
	for d := range fstypes {
		dests = append(dests, d)
	}
	sort.Strings(dests) // deterministic message order
	for _, dest := range dests {
		fs := fstypes[dest]
		if fs == "" || !p.LocalOnlyPath(dest) {
			continue
		}
		if backup.IsNetworkFilesystem(fs) {
			warnings = append(warnings, fmt.Sprintf(
				"%s on the target is backed by %s, a network filesystem. %s keeps its database there, and SQLite on a network share loses the file locking it depends on — the database corrupts silently, days after the restore looks successful. Move %s onto local disk on the target first.",
				dest, fs, p.Name, dest))
			blocking = true
		}
	}
	return warnings, blocking
}

// targetMountFSTypes reads the filesystem type backing each of a container's
// mounts, for the local-only storage guard (F110).
//
// Best-effort and read-only: an unreachable node or a target that does not exist
// yet returns nothing, and the guard then measures nothing rather than guessing.
func (s *Server) targetMountFSTypes(ctx context.Context, nodeID, containerID string) map[string]string {
	if nodeID == "" || containerID == "" {
		return nil
	}
	cli, err := s.reg.Get(nodeID)
	if err != nil {
		return nil
	}
	ictx, icancel := context.WithTimeout(ctx, 15*time.Second)
	insp, ierr := cli.ContainerInspect(ictx, containerID)
	icancel()
	if ierr != nil {
		return nil
	}
	var dests []string
	for _, m := range insp.Mounts {
		if m.Destination != "" {
			dests = append(dests, m.Destination)
		}
	}
	if len(dests) == 0 {
		return nil
	}
	sctx, scancel := context.WithTimeout(ctx, 60*time.Second)
	defer scancel()
	_, _, stats, ferr := dockercli.MountSizesFrom(sctx, cli, containerID, dests)
	if ferr != nil {
		return nil
	}
	// The guard judges the backing filesystem only; the probe's other fields
	// describe the mount root and are not its business.
	fstypes := make(map[string]string, len(stats))
	for dest, st := range stats {
		if st.FSType != "" {
			fstypes[dest] = st.FSType
		}
	}
	return fstypes
}
