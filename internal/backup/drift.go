package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/docker/docker/api/types"

	"dockback/internal/dockercli"
)

// Config-drift detection (F73). Every backup stores the container's full
// inspect; a user who then edits env/mounts/image without re-backing up holds a
// stale backup that will restore the OLD config. These pure helpers compare the
// captured config against the live one, normalized down to the fields that
// MATTER for a restore — cosmetic runtime fields (timestamps, container id,
// SandboxKey, …) are excluded by construction, since only the listed sections
// ever enter the comparison tuple.

// DriftReport is the outcome of a config comparison: whether anything material
// changed and which sections did (friendly names, e.g. "environment").
type DriftReport struct {
	Changed bool     `json:"changed"`
	Fields  []string `json:"fields,omitempty"`
}

// driftSections fixes the comparison sections and their user-facing labels.
var driftSections = []struct{ key, label string }{
	{"image", "image"},
	{"env", "environment"},
	{"mounts", "mounts"},
	{"ports", "ports"},
	{"restart", "restart policy"},
	{"cmd", "command"},
}

// mountPointsToMounts adapts an inspect's mount points to the discovery Mount
// shape so both drift and the fleet cache share candidateMountDests' rules.
func mountPointsToMounts(mps []types.MountPoint) []dockercli.Mount {
	out := make([]dockercli.Mount, 0, len(mps))
	for _, m := range mps {
		out = append(out, dockercli.Mount{Type: string(m.Type), Name: m.Name, Source: m.Source, Destination: m.Destination, RW: m.RW})
	}
	return out
}

// driftTuple normalizes an inspect into the section → canonical-string map the
// fingerprint and the field-by-field comparison BOTH consume (one definition,
// no drift between them).
func driftTuple(insp types.ContainerJSON) map[string]string {
	t := map[string]string{}

	imageRef, imageID := "", ""
	if insp.Config != nil {
		imageRef = insp.Config.Image
	}
	if insp.ContainerJSONBase != nil {
		imageID = insp.Image
	}
	t["image"] = imageRef + "|" + imageID

	// Environment: sorted set, minus the runtime-injected PATH/HOSTNAME noise.
	var env []string
	if insp.Config != nil {
		for _, e := range insp.Config.Env {
			if strings.HasPrefix(e, "PATH=") || strings.HasPrefix(e, "HOSTNAME=") {
				continue
			}
			env = append(env, e)
		}
	}
	sort.Strings(env)
	t["env"] = strings.Join(env, "\x1f")

	// Mounts: the backup-relevant destination set (same filter the mount picker
	// uses), sorted — inspect vs list APIs return mounts in different orders.
	dests := candidateMountDests(mountPointsToMounts(insp.Mounts))
	sort.Strings(dests)
	t["mounts"] = strings.Join(dests, "\x1f")

	// Published ports: each binding key with its sorted host-side bindings.
	var ports []string
	if insp.ContainerJSONBase != nil && insp.HostConfig != nil {
		for port, binds := range insp.HostConfig.PortBindings {
			var bs []string
			for _, pb := range binds {
				bs = append(bs, pb.HostIP+":"+pb.HostPort)
			}
			sort.Strings(bs)
			ports = append(ports, string(port)+">"+strings.Join(bs, ","))
		}
	}
	sort.Strings(ports)
	t["ports"] = strings.Join(ports, "\x1f")

	restart := ""
	if insp.ContainerJSONBase != nil && insp.HostConfig != nil {
		restart = string(insp.HostConfig.RestartPolicy.Name)
	}
	t["restart"] = restart

	cmd, entry := "", ""
	if insp.Config != nil {
		cmd = strings.Join(insp.Config.Cmd, "\x1f")
		entry = strings.Join(insp.Config.Entrypoint, "\x1f")
	}
	t["cmd"] = cmd + "|" + entry

	return t
}

// parseInspect unmarshals an inspect JSON, reporting whether it carries enough
// to compare (a Config section).
func parseInspect(inspectJSON []byte) (types.ContainerJSON, bool) {
	var insp types.ContainerJSON
	if err := json.Unmarshal(inspectJSON, &insp); err != nil || insp.Config == nil {
		return types.ContainerJSON{}, false
	}
	return insp, true
}

// ConfigFingerprint hashes the normalized comparison tuple of an inspect JSON —
// the cheap equality probe stored in the manifest as ConfigFP. "" when the
// JSON is unusable (a legacy/broken record never claims drift).
func ConfigFingerprint(inspectJSON []byte) string {
	insp, ok := parseInspect(inspectJSON)
	if !ok {
		return ""
	}
	t := driftTuple(insp)
	parts := make([]string, 0, len(driftSections))
	for _, s := range driftSections {
		parts = append(parts, s.key+"="+t[s.key])
	}
	sum := sha256.Sum256([]byte("drift-v1|" + strings.Join(parts, "\x1e")))
	return hex.EncodeToString(sum[:])
}

// ConfigDrift compares a stored inspect against the live one field by field,
// returning the friendly names of every changed section. Either side unusable →
// zero report (no claim), never a false positive.
func ConfigDrift(storedInspect, liveInspect []byte) DriftReport {
	stored, ok1 := parseInspect(storedInspect)
	live, ok2 := parseInspect(liveInspect)
	if !ok1 || !ok2 {
		return DriftReport{}
	}
	st, lt := driftTuple(stored), driftTuple(live)
	var rep DriftReport
	for _, s := range driftSections {
		if st[s.key] != lt[s.key] {
			rep.Changed = true
			rep.Fields = append(rep.Fields, s.label)
		}
	}
	return rep
}

// ConfigFingerprintLite fingerprints ONLY the fields the fleet inventory cache
// carries per container (image ref, image id, backupable mount destinations),
// so the runbook can flag drift with zero live Docker calls — the cache is
// refreshed continuously anyway. Env/ports/command drift is caught by the full
// fingerprint on the container page, where a full inspect is in hand.
func ConfigFingerprintLite(imageRef, imageID string, mounts []dockercli.Mount) string {
	if imageRef == "" && imageID == "" {
		return ""
	}
	dests := candidateMountDests(mounts)
	sort.Strings(dests)
	sum := sha256.Sum256([]byte("drift-lite-v1|" + imageRef + "|" + imageID + "|" + strings.Join(dests, "\x1f")))
	return hex.EncodeToString(sum[:])
}

// configFPLiteFromInspect derives the lite fingerprint from a full inspect —
// the backup-time side of the runbook comparison.
func configFPLiteFromInspect(insp types.ContainerJSON) string {
	ref, id := "", ""
	if insp.Config != nil {
		ref = insp.Config.Image
	}
	if insp.ContainerJSONBase != nil {
		id = insp.Image
	}
	return ConfigFingerprintLite(ref, id, mountPointsToMounts(insp.Mounts))
}
