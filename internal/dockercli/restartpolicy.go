package dockercli

import (
	"encoding/json"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
)

// Restart policy is not portable semantics (#8).
//
// R1 §Issue 8: a source using `restart: on-failure:5` restarts only on a
// non-zero exit — it does NOT start the container when the Docker daemon starts.
// The Synology masked it, because DSM's package manager starts its stacks; "on a
// stock Ubuntu host the wiki would simply be gone after a reboot, with no error
// anywhere."
//
// So the policy's practical meaning depends on the host's init integration,
// which is precisely why it is surfaced rather than silently changed: rewriting
// it by default would be introducing configuration the source does not have
// (#31), and on a host where something else starts the stack the source's choice
// is correct.

// BootSafePolicy is the policy a promotion sets. `unless-stopped` starts the
// container with the daemon and still honours a deliberate stop.
const BootSafePolicy = "unless-stopped"

// RestartPolicyBootSafe reports whether a policy brings the container back when
// the host reboots.
//
// Empty counts as not boot-safe: Docker's default is `no`, and a container with
// no policy is exactly the one that disappears after a reboot.
func RestartPolicyBootSafe(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "always", BootSafePolicy:
		return true
	}
	return false
}

// RestartPolicyName reads the recorded policy out of an inspect document.
func RestartPolicyName(inspectJSON []byte) string {
	var insp types.ContainerJSON
	if json.Unmarshal(inspectJSON, &insp) != nil || insp.HostConfig == nil {
		return ""
	}
	return string(insp.HostConfig.RestartPolicy.Name)
}

// PromoteRestartPolicy rewrites the policy to `unless-stopped`.
//
// Opt-in only, and it is the operator's decision rather than the tool's: the
// finding is the default behaviour. Returns the document byte-identical when the
// policy is already boot-safe, so choosing the option on a container that does
// not need it cannot introduce churn.
//
// The MaximumRetryCount goes with it — it is only meaningful for `on-failure`,
// and Docker rejects a non-zero count on any other policy.
func PromoteRestartPolicy(inspectJSON []byte) ([]byte, bool, error) {
	var insp types.ContainerJSON
	if err := json.Unmarshal(inspectJSON, &insp); err != nil {
		return inspectJSON, false, err
	}
	if insp.HostConfig == nil {
		return inspectJSON, false, nil
	}
	if RestartPolicyBootSafe(string(insp.HostConfig.RestartPolicy.Name)) {
		return inspectJSON, false, nil
	}
	insp.HostConfig.RestartPolicy = container.RestartPolicy{Name: BootSafePolicy}
	out, err := json.Marshal(insp)
	if err != nil {
		return inspectJSON, false, err
	}
	return out, true, nil
}
