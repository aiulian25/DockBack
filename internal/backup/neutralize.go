package backup

import (
	"sort"
	"strings"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// Safe-restore mode: a clone that cannot act on the real world (#17).
//
// R2 §Issue 17: a BookStack clone brought up to prove a backup restores carried
// the original's working Gmail credentials, so any invite or password reset it
// generated went to real people from the real account. The point is not that the
// clone was insecure — it is that the operator believed they were looking at a
// sandbox. "A restored clone is not inert: it is a second live system with the
// first system's authority."
//
// Two tiers, and the split matters. A PROFILE knows the safe value for an
// application's own settings — Laravel's log mailer still exercises every code
// path that composes mail, where an empty MAIL_DRIVER would crash. A GENERIC
// name match knows nothing about the application, so it only ever empties a
// value, and only for names whose meaning is unambiguous across every app.
//
// Clone mode only. A cutover restore must keep its credentials or the thing it
// restored does not work, which is the same boundary F155 already draws.

// genericNeutralizeExact are names that mean one thing everywhere they appear.
var genericNeutralizeExact = []string{"SMTP_HOST", "MAIL_HOST"}

// genericNeutralizeContains are fragments that identify an outbound endpoint or
// its token wherever they appear in a variable's name.
var genericNeutralizeContains = []string{"WEBHOOK", "_PUSH_", "APPRISE"}

// genericNeutralizeReason is why a name-matched variable is emptied. One
// sentence, because a generic rule cannot say anything application-specific.
const genericNeutralizeReason = "so this copy cannot reach the service it points at"

// matchesGenericNeutralize reports whether a variable name is one of the
// unambiguous outbound ones.
func matchesGenericNeutralize(key string) bool {
	upper := strings.ToUpper(strings.TrimSpace(key))
	if upper == "" {
		return false
	}
	for _, exact := range genericNeutralizeExact {
		if upper == exact {
			return true
		}
	}
	for _, fragment := range genericNeutralizeContains {
		if strings.Contains(upper, fragment) {
			return true
		}
	}
	return false
}

// neutralizationPlan is what a clone's environment will be changed to, and what
// was deliberately left alone.
type neutralizationPlan struct {
	// Apply are the changes to make, in a stable order.
	Apply []EnvNeutralization
	// Baked names variables an application-level rule wanted to change but the
	// IMAGE sets. Left untouched and reported: the image's own configuration is
	// not the operator's integration settings, and overriding it is how a restore
	// breaks an application while believing it is protecting one.
	Baked []string
}

// planNeutralizations decides what a clone's environment becomes.
//
// A profile rule wins over a generic match on the same key — the profile knows
// the safe value and the generic tier only knows how to empty things.
//
// Nothing is invented: only variables the container actually SETS are changed. A
// name in a profile that this deployment does not use produces no entry, because
// adding one would put a variable into a clone that the original never had.
func planNeutralizations(profile *AppProfile, env, imageEnvKeys []string) neutralizationPlan {
	present := envValues(env)
	baked := map[string]bool{}
	for _, key := range imageEnvKeys {
		baked[key] = true
	}

	var plan neutralizationPlan
	claimed := map[string]bool{}

	if profile != nil {
		for _, rule := range profile.NeutralizeOnClone {
			if rule.Key == "" {
				continue
			}
			if _, set := present[rule.Key]; !set {
				continue
			}
			claimed[rule.Key] = true
			if baked[rule.Key] {
				plan.Baked = append(plan.Baked, rule.Key)
				continue
			}
			plan.Apply = append(plan.Apply, rule)
		}
	}

	var generic []string
	for key := range present {
		if claimed[key] || !matchesGenericNeutralize(key) {
			continue
		}
		if baked[key] {
			plan.Baked = append(plan.Baked, key)
			continue
		}
		generic = append(generic, key)
	}
	sort.Strings(generic)
	for _, key := range generic {
		plan.Apply = append(plan.Apply, EnvNeutralization{Key: key, Mode: NeutralizeClear, Why: genericNeutralizeReason})
	}
	sort.Strings(plan.Baked)
	return plan
}

// describeNeutralization names one change the way an operator reads it: the
// variable, and what it became — never what it was.
func describeNeutralization(rule EnvNeutralization) string {
	if rule.Mode == NeutralizeSet {
		return rule.Key + " (" + rule.Value + ")"
	}
	return rule.Key + " (cleared)"
}

// applyCloneNeutralizations defangs a clone's outbound integrations by rewriting
// the environment the container will be CREATED with.
//
// Before the create, because environment is fixed at creation: a clone that
// starts with the original's mail credentials has already been able to send by
// the time anything could edit it. This is the same reason CloneRedactions runs
// before the first start, one layer down.
//
// A no-op for every restore that is not a clone.
func (e *Engine) applyCloneNeutralizations(b *store.Backup, man *Manifest, inspectBytes []byte, clone bool) []byte {
	if !clone {
		return inspectBytes
	}
	var imageEnvKeys []string
	if man != nil && man.ImageConfig != nil {
		imageEnvKeys = man.ImageConfig.EnvKeys
	}
	plan := planNeutralizations(ProfileFor(manifestImage(man, b)), dockercli.ContainerEnv(inspectBytes), imageEnvKeys)

	var applied []string
	for _, rule := range plan.Apply {
		value := ""
		if rule.Mode == NeutralizeSet {
			value = rule.Value
		}
		out, changed, err := dockercli.SetContainerEnv(inspectBytes, rule.Key, value)
		if err != nil {
			e.logf(b.ID, "WARN", "Could not neutralise %s on this copy (%v) — it starts with the original's value, so it can still act through it", rule.Key, err)
			continue
		}
		if !changed {
			continue // already harmless
		}
		inspectBytes = out
		applied = append(applied, describeNeutralization(rule))
	}

	if len(applied) > 0 {
		// One block, not one line per variable: the operator needs to read this
		// as a single statement about what the copy can and cannot do.
		e.logf(b.ID, "INFO", "Neutralised for this clone: %s. This copy starts unable to act on the real world through %s — re-arm by restoring in place, which replays the recorded values.",
			strings.Join(applied, ", "), plural(len(applied), "it", "them"))
	}
	if len(plan.Baked) > 0 {
		// Said out loud because the consequence is the opposite of the one above:
		// these are still live in the clone.
		e.logf(b.ID, "WARN", "Left alone because the image sets %s itself: %s. Overriding an image's own setting is how a restore breaks the application it is copying — but it means this copy may still reach out through %s.",
			plural(len(plan.Baked), "it", "them"), strings.Join(plan.Baked, ", "), plural(len(plan.Baked), "it", "them"))
	}
	return inspectBytes
}
