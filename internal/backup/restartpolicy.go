package backup

import (
	"fmt"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// Saying that a restart policy will not survive a reboot (#8).
//
// R1 §Issue 8's takeaway: "restart policy is NOT portable semantics — its
// practical meaning depends on the host's init integration. On cross-restore,
// surface the policy and warn when `no` / `on-failure` is carried onto a host
// where nothing else will start the stack."
//
// Reported at capture and never changed on its own. A host whose package manager
// starts its stacks — the Synology that masked this — makes the source's choice
// correct, and a backup tool that quietly rewrote it would be introducing
// configuration the source does not have. Promotion is one checkbox away and is
// the operator's call.

// reportRestartPolicy records a policy that would leave this container gone after
// a reboot.
func (e *Engine) reportRestartPolicy(man *Manifest, logID, name, policy string) {
	if dockercli.RestartPolicyBootSafe(policy) {
		return
	}
	shown := policy
	if shown == "" {
		shown = "no (Docker's default, because none is set)"
	}
	e.addFinding(man, logID, findingRestartPolicyNotBootSafe, FindingWarn, name, fmt.Sprintf(
		"This container's restart policy is %q, which does not start it when the Docker daemon starts — only when it exits badly while the daemon is already running. If this host reboots, %s does not come back, and nothing reports an error because nothing failed. "+
			"That may be correct here: a NAS whose package manager starts its own stacks masks this entirely. On a plain host it means the container is gone after the next reboot. "+
			"Restoring this backup offers to set it to %q, which starts with the daemon and still honours a deliberate stop. Whichever you choose, check the daemon itself starts at boot — `systemctl is-enabled docker` — because a perfect restore behind a disabled daemon is still an outage.",
		shown, name, dockercli.BootSafePolicy))
}

// applyRestartPolicyPromotion rewrites the policy when the operator asked for it.
//
// Off by default, deliberately: the finding is the default behaviour, and
// silently changing an availability setting is the class of deviation #31 exists
// to stop.
func (e *Engine) applyRestartPolicyPromotion(b *store.Backup, inspectBytes []byte, opts RestoreOptions) []byte {
	if !opts.PromoteRestartPolicy {
		return inspectBytes
	}
	was := dockercli.RestartPolicyName(inspectBytes)
	out, changed, err := dockercli.PromoteRestartPolicy(inspectBytes)
	if err != nil {
		e.logf(b.ID, "WARN", "Could not set the restart policy (%v) — the container keeps the one this backup recorded", err)
		return inspectBytes
	}
	if !changed {
		e.logf(b.ID, "INFO", "Restart policy is already %q, which starts with the daemon — nothing to promote", was)
		return inspectBytes
	}
	e.logf(b.ID, "INFO", "Restart policy set to %q as you asked, in place of the recorded %q — this container now starts when the Docker daemon does. Check the daemon starts at boot too: `systemctl is-enabled docker`.",
		dockercli.BootSafePolicy, was)
	return out
}
