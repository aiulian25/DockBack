package backup

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// Rules that apply because of a measured host difference, not because a restore
// is happening (#7, PLAYBOOK §2.1).
//
// Migration #5 was the first with matching platforms, and rules that had fired in
// every previous run did not fire. §2.1's own table: the identity rewrite skipped
// because uid 1000 existed on the target; the localhost rewrite skipped because
// "no probe used localhost, and no engine gap to expose the IPv6 difference" —
// "#7 is a version-gap artifact, not a universal rule."
//
// The takeaway is the framework: "classify each rule as INTRINSIC (any
// cross-restore) or ENVIRONMENTAL (triggered by a measured host difference).
// Derive the applicable set from a host diff, apply only those, and TELL THE USER
// WHICH WERE SKIPPED AND WHY. Rewriting identity when the uid already matches is
// gratuitous churn that makes the clone less faithful, not more correct."
//
// So a skipped rule is reported as deliberately as an applied one. Silence would
// leave an operator unable to tell "this did not need doing" from "this was never
// considered".

// hostDiff is what actually differs between the machine a backup came from and
// the one it is going to. Empty fields mean unknown, which is never treated as
// "the same".
type hostDiff struct {
	SourceEngine string
	TargetEngine string
}

// ruleVerdict is one environmental rule's decision, and the reason for it.
type ruleVerdict struct {
	Name    string
	Applied bool
	Why     string
}

// String renders a verdict the way the block reads.
func (r ruleVerdict) String() string {
	state := "SKIPPED"
	if r.Applied {
		state = "APPLIED"
	}
	return fmt.Sprintf("%s %s (%s)", r.Name, state, r.Why)
}

// localhostRewriteRule decides whether R1 §Issue 7's rewrite applies here.
//
// Two conditions, both measured. The probe must actually say `localhost`, and
// the target's engine must be NEWER than the source's — the resolution order
// that breaks it arrived with a newer Docker, so a same or older engine cannot
// exhibit it. Either missing and the rule is skipped, with which one said.
func localhostRewriteRule(diff hostDiff, probeUsesLocalhost bool) ruleVerdict {
	const name = "localhost-rewrite"
	if !probeUsesLocalhost {
		return ruleVerdict{Name: name, Why: "no probe uses localhost"}
	}
	sourceMajor, sourceOK := engineMajor(diff.SourceEngine)
	targetMajor, targetOK := engineMajor(diff.TargetEngine)
	if !sourceOK || !targetOK {
		// Unknown is not "the same". Without both versions the gap cannot be
		// measured, and a rule this one applies on a guess is churn.
		return ruleVerdict{Name: name, Why: "the engine version on one side is unknown, so no gap could be measured"}
	}
	if targetMajor <= sourceMajor {
		return ruleVerdict{Name: name, Why: fmt.Sprintf("engine %s → %s is not a newer engine", diff.SourceEngine, diff.TargetEngine)}
	}
	return ruleVerdict{
		Name:    name,
		Applied: true,
		Why:     fmt.Sprintf("engine %s → %s, where localhost resolves to ::1 first", diff.SourceEngine, diff.TargetEngine),
	}
}

// engineMajor reads the leading major version out of a Docker version string.
func engineMajor(version string) (int, bool) {
	v := strings.TrimSpace(version)
	if v == "" {
		return 0, false
	}
	head, _, _ := strings.Cut(v, ".")
	n, err := strconv.Atoi(strings.TrimSpace(head))
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// reportEnvironmentalRules states every rule's verdict in one block, applied and
// skipped alike.
func (e *Engine) reportEnvironmentalRules(b *store.Backup, verdicts []ruleVerdict) {
	if len(verdicts) == 0 {
		return
	}
	parts := make([]string, 0, len(verdicts))
	for _, v := range verdicts {
		parts = append(parts, v.String())
	}
	e.logf(b.ID, "INFO", "Environmental rules: %s", strings.Join(parts, "; "))
}

// applyEnvironmentalRules decides and reports the rules that depend on how these
// two machines differ, and applies the ones that do.
//
// Only on a cross-host restore: restoring onto the machine the backup came from
// has no host difference to measure, and applying a rule anyway would make the
// container less faithful rather than more correct.
func (e *Engine) applyEnvironmentalRules(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, inspectBytes []byte) []byte {
	if man == nil {
		return inspectBytes
	}
	diff := hostDiff{SourceEngine: man.DockerVersion}
	if v, err := cli.ServerVersion(ctx); err == nil {
		diff.TargetEngine = v.Version
	}

	verdict := localhostRewriteRule(diff, dockercli.HealthcheckUsesLocalhost(inspectBytes))
	if verdict.Applied {
		out, changes, err := dockercli.RewriteHealthcheckLocalhost(inspectBytes)
		switch {
		case err != nil:
			verdict = ruleVerdict{Name: verdict.Name, Why: "could not be applied: " + err.Error()}
		case changes == 0:
			verdict = ruleVerdict{Name: verdict.Name, Why: "nothing in the probe needed changing"}
		default:
			inspectBytes = out
			verdict.Why += fmt.Sprintf(", %d %s rewritten to 127.0.0.1", changes, plural(changes, "probe argument", "probe arguments"))
		}
	}
	e.reportEnvironmentalRules(b, []ruleVerdict{verdict})
	return inspectBytes
}
