package backup

import (
	"sort"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types"
)

// hostRequirementsOf reads what a container needs from its host (F94).
//
// Everything here is already replayed faithfully by the recreate path — the gap
// was that none of it was ever INSPECTED for portability, so a cross-host
// restore happily created a container the target could never start.
//
// Returns nil for an ordinary container, so its manifest is byte-identical to
// before this existed.
func hostRequirementsOf(insp types.ContainerJSON) *HostRequirements {
	if insp.HostConfig == nil {
		return nil
	}
	hc := insp.HostConfig
	req := HostRequirements{Privileged: hc.Privileged}

	for _, d := range hc.Devices {
		if d.PathOnHost != "" {
			req.Devices = append(req.Devices, d.PathOnHost)
		}
	}
	sort.Strings(req.Devices)

	// A GPU reservation needs the NVIDIA container runtime on the target, which
	// is a different question from whether a card is physically present.
	for _, dr := range hc.DeviceRequests {
		if dr.Count == -1 {
			req.GPUs++ // "all"
			continue
		}
		if dr.Count > 0 {
			req.GPUs += dr.Count
			continue
		}
		req.GPUs += len(dr.DeviceIDs)
	}

	if len(hc.Sysctls) > 0 {
		req.Sysctls = make(map[string]string, len(hc.Sysctls))
		for k, v := range hc.Sysctls {
			req.Sysctls[k] = v
		}
	}
	for _, c := range hc.CapAdd {
		req.CapAdd = append(req.CapAdd, string(c))
	}
	sort.Strings(req.CapAdd)

	// Only a NON-default driver is worth recording: "json-file" is what almost
	// every host uses, and flagging it would make the warning meaningless.
	if d := strings.TrimSpace(hc.LogConfig.Type); d != "" && d != "json-file" {
		req.LogDriver = d
	}

	// F128: the Docker socket. Read from the container's resolved MOUNTS rather
	// than its Binds string, because the same mount can be expressed either way
	// and only the resolved view reports the mode reliably.
	req.DockerSocket = dockerSocketMode(insp)

	if req.Empty() {
		return nil
	}
	return &req
}

// publishedPortsOf records the HOST ports a container publishes (F144).
//
// Taken from the resolved host config, so a range published as `8000-8005:80`
// arrives here already expanded into the individual bindings Docker will
// actually try to bind. A container that publishes nothing returns nil and adds
// nothing to its manifest.
//
// The point is entirely about the restore: a container recreated on a machine
// where something else already holds its port is created and then fails to
// start, with a Docker error that names the port and nothing else. Recording
// what it needs lets the pre-restore dialog name the container holding it.
func publishedPortsOf(insp types.ContainerJSON) []PublishedPort {
	if insp.HostConfig == nil || len(insp.HostConfig.PortBindings) == 0 {
		return nil
	}
	var out []PublishedPort
	for port, binds := range insp.HostConfig.PortBindings {
		for _, b := range binds {
			// An empty HostPort means "pick a free one", which can never
			// conflict — there is nothing to warn about.
			n, err := strconv.Atoi(strings.TrimSpace(b.HostPort))
			if err != nil || n <= 0 || n > 65535 {
				continue
			}
			out = append(out, PublishedPort{HostIP: b.HostIP, HostPort: n, Proto: port.Proto()})
		}
	}
	// Map iteration is random; a manifest that differs run to run over nothing
	// would show up as config drift.
	sort.Slice(out, func(i, j int) bool {
		if out[i].HostPort != out[j].HostPort {
			return out[i].HostPort < out[j].HostPort
		}
		if out[i].Proto != out[j].Proto {
			return out[i].Proto < out[j].Proto
		}
		return out[i].HostIP < out[j].HostIP
	})
	return out
}

// DockerSocketPath is the host path whose presence in a container means it can
// drive the Docker daemon.
const DockerSocketPath = "/var/run/docker.sock"

// dockerSocketMode reports "rw", "ro", or "" for a container's Docker-socket
// mount (F128).
//
// Deliberately keyed on the SOURCE, not the destination: what matters is which
// host object was handed over, and an unusual in-container path changes nothing
// about the authority granted. A read-write socket is root-equivalent control of
// the host.
func dockerSocketMode(insp types.ContainerJSON) string {
	for _, m := range insp.Mounts {
		if m.Source != DockerSocketPath {
			continue
		}
		if m.RW {
			return "rw"
		}
		return "ro"
	}
	return ""
}

// DockerSocketWidened reports whether `now` grants MORE Docker authority than
// `recorded` — the invariant a restore must never break (F128).
//
// Three ways to widen it, and all three are treated the same: read-only becoming
// read-write, and a socket appearing where the backup had none at all.
// Narrowing (rw → ro, or dropping it) is a hardening change and is never
// flagged; an operator tightening their own deployment should not be nagged.
func DockerSocketWidened(recorded, now string) bool {
	switch recorded {
	case "rw":
		return false // already the widest there is
	case "ro":
		return now == "rw"
	default: // none recorded
		return now == "rw" || now == "ro"
	}
}
