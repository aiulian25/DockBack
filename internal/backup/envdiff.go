package backup

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/client"

	"dockback/internal/store"
)

// Proving the environment came back, rather than trusting that it did (#38).
//
// Env fidelity holds by construction — the recreate is driven by the recorded
// inspect — but nothing ever checked it afterwards, and R5 §4 shows the check
// catching two real defects in seconds. Both were omissions, and both were
// silent: a `TZ` filtered out by a grep during capture, which then produced
// nineteen false table mismatches; and a service block typed by hand, missing
// SEVENTEEN variables including the one that gives its own bind mount meaning.
//
// R5's rule, and the reason this compares the way it does: "reproduce the
// environment exactly — OMISSION is as much a deviation as addition." The audit
// that found it compared "each clone container's resolved env against the
// source's, both diffed against their image's baked-in env" — subtracting the
// image's own keys is what removes PATH, LANG and the rest of the noise a
// container never chose.
//
// Values are never printed. A key's value is compared by hash, which is also
// what makes the check safe on the 128-byte metacharacter secrets R5 §4 covers:
// the comparison is exact and the log stays free of the thing being compared.

// envHashLen is how much of a value's digest is shown. Enough to tell two values
// apart by eye; not enough to be a value.
const envHashLen = 8

// envHashPrefix renders a value as a short digest. Empty stays empty, so
// "missing" and "empty" never look alike.
func envHashPrefix(value string) string {
	if value == "" {
		return "(empty)"
	}
	sum := md5.Sum([]byte(value))
	return hex.EncodeToString(sum[:])[:envHashLen]
}

// EnvChange is one variable whose value is not what was captured. Hashes only.
type EnvChange struct {
	Key string `json:"key"`
	Was string `json:"was"`
	Now string `json:"now"`
}

// String renders it the way the log shows it: the name, and both sides as
// digests.
func (c EnvChange) String() string { return fmt.Sprintf("%s (%s → %s)", c.Key, c.Was, c.Now) }

// EnvDelta is what the restored container's environment has that the backup's
// did not, and the other way round.
type EnvDelta struct {
	// Missing were captured and are not on the restored container. R5 §4.2's
	// seventeen.
	Missing []string `json:"missing,omitempty"`
	// Extra are on the restored container and were not captured. Usually a newer
	// image's own defaults rather than a restore defect, which is why they are
	// reported apart from the other two.
	Extra []string `json:"extra,omitempty"`
	// Changed came back with a different value.
	Changed []EnvChange `json:"changed,omitempty"`
	// Explained are the keys the restore itself rewrote — a remap, an ownership
	// pin, a clone's neutralisation. Named so the report shows the deviations it
	// performed rather than hiding them.
	Explained []string `json:"explained,omitempty"`
}

// Faithful reports whether the environment is what it should be: what was
// captured, plus exactly the deviations the restore performed.
func (d EnvDelta) Faithful() bool { return len(d.Missing) == 0 && len(d.Changed) == 0 }

// CompareEnv diffs a restored container's environment against the captured one.
//
// bakedKeys are the IMAGE's own variables, subtracted from both sides — that is
// R5's method, and without it every comparison drowns in PATH and LANG.
// explained are the keys this restore deliberately rewrote; a difference in one
// of them is the restore working, not failing.
func CompareEnv(captured, live, bakedKeys, explained []string) EnvDelta {
	baked, skip := setOfKeys(bakedKeys), setOfKeys(explained)
	was, now := envValues(captured), envValues(live)

	var delta EnvDelta
	for _, key := range sortedEnvKeys(was) {
		if baked[key] {
			continue
		}
		value, present := now[key]
		if skip[key] {
			delta.Explained = append(delta.Explained, key)
			continue
		}
		switch {
		case !present:
			delta.Missing = append(delta.Missing, key)
		case value != was[key]:
			delta.Changed = append(delta.Changed, EnvChange{Key: key, Was: envHashPrefix(was[key]), Now: envHashPrefix(value)})
		}
	}
	for _, key := range sortedEnvKeys(now) {
		if baked[key] || skip[key] {
			continue
		}
		if _, recorded := was[key]; !recorded {
			delta.Extra = append(delta.Extra, key)
		}
	}
	return delta
}

// setOfKeys is a lookup over a key list.
func setOfKeys(keys []string) map[string]bool {
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		if k = strings.TrimSpace(k); k != "" {
			out[k] = true
		}
	}
	return out
}

// sortedEnvKeys keeps every report in the same order run to run.
func sortedEnvKeys(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k := range env {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// changedEnvKeys names the variables whose value differs between two inspect
// documents' environments.
//
// This is how the restore knows what it rewrote, without every pass having to
// report it: the passes edit the inspect, and comparing the document before and
// after them covers all of them at once — including the IP, domain and path
// remaps, which can touch any key's value and never knew which.
func changedEnvKeys(before, after []string) []string {
	was, now := envValues(before), envValues(after)
	var changed []string
	for _, key := range sortedEnvKeys(was) {
		if value, ok := now[key]; !ok || value != was[key] {
			changed = append(changed, key)
		}
	}
	for _, key := range sortedEnvKeys(now) {
		if _, ok := was[key]; !ok {
			changed = append(changed, key)
		}
	}
	sort.Strings(changed)
	return changed
}

// describeEnvChanges renders the changed list, bounded like every other report.
func describeEnvChanges(changes []EnvChange) string {
	shown, suffix := changes, ""
	if len(changes) > maxReportedShortTables {
		shown = changes[:maxReportedShortTables]
		suffix = fmt.Sprintf(" and %d more", len(changes)-maxReportedShortTables)
	}
	parts := make([]string, 0, len(shown))
	for _, c := range shown {
		parts = append(parts, c.String())
	}
	return strings.Join(parts, ", ") + suffix
}

// assertEnvRestored checks the created container's environment against what the
// backup recorded, and refuses to go further if a variable went missing.
//
// A hard failure on MISSING or CHANGED, because that is #38's whole finding:
// both of R5 §4's defects were omissions, both were silent, and the container
// started fine either way. A variable that is gone does not announce itself —
// `MPLCONFIGDIR` was "the entire reason the /matplotlib bind exists", and its
// absence looked like nothing at all.
//
// EXTRA keys are reported and never fail. A newer image's own defaults arrive
// that way whenever the digest could not be pinned, which F95's image-drift
// report already covers; failing here would refuse a restore over the image
// having moved.
func (e *Engine) assertEnvRestored(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, containerID string, capturedEnv, explained []string) error {
	if len(capturedEnv) == 0 {
		return nil
	}
	ictx, cancel := context.WithTimeout(ctx, 20*time.Second)
	insp, err := cli.ContainerInspect(ictx, containerID)
	cancel()
	if err != nil || insp.Config == nil {
		e.logf(b.ID, "INFO", "Could not read the restored container's environment back (%v) — it was recreated from the recorded configuration, but that was not confirmed", err)
		return nil
	}
	var bakedKeys []string
	if man != nil && man.ImageConfig != nil {
		bakedKeys = man.ImageConfig.EnvKeys
	}
	delta := CompareEnv(capturedEnv, insp.Config.Env, bakedKeys, explained)

	if len(delta.Explained) > 0 {
		e.logf(b.ID, "INFO", "%d environment %s deliberately rewritten by this restore: %s",
			len(delta.Explained), plural(len(delta.Explained), "variable was", "variables were"), namedTables(delta.Explained))
	}
	if len(delta.Extra) > 0 {
		// Cross-referenced rather than judged: this is what an image that moved
		// looks like, and F95 is the report that speaks to it.
		e.logf(b.ID, "INFO", "%d environment %s set that this backup did not record: %s. Usually a newer image's own defaults — see this backup's image-drift report if the version changed.",
			len(delta.Extra), plural(len(delta.Extra), "variable is", "variables are"), namedTables(delta.Extra))
	}
	if delta.Faithful() {
		e.logf(b.ID, "INFO", "Environment verified: every variable this backup recorded is set on the restored container, with the values it was captured with")
		return nil
	}

	if len(delta.Missing) > 0 {
		e.logf(b.ID, "ERROR", "%d environment %s this backup recorded %s NOT set on the restored container: %s. A container missing a variable starts perfectly well and is wrong — one of R5's was the only reason its bind mount existed.",
			len(delta.Missing), plural(len(delta.Missing), "variable", "variables"),
			plural(len(delta.Missing), "is", "are"), namedTables(delta.Missing))
	}
	if len(delta.Changed) > 0 {
		e.logf(b.ID, "ERROR", "%d environment %s came back with a different value (shown as digests, never values): %s",
			len(delta.Changed), plural(len(delta.Changed), "variable", "variables"), describeEnvChanges(delta.Changed))
	}
	e.logf(b.ID, "ERROR", "The container was NOT started. Its environment is not the one this backup recorded, and nothing about that would be visible once it is running.")
	return fmt.Errorf("the restored container's environment does not match this backup: %d missing, %d changed — see the run log for the names",
		len(delta.Missing), len(delta.Changed))
}

// drillEnvVerdict checks the throwaway's environment against the manifest's, and
// renders it for the drill's verdict line.
//
// The drill applies no remap, so nothing is explained away: whatever the backup
// recorded is exactly what the throwaway should be running with.
func drillEnvVerdict(man *Manifest, capturedEnv, bootEnv []string) (string, bool) {
	if len(capturedEnv) == 0 {
		return "", true
	}
	var bakedKeys []string
	if man != nil && man.ImageConfig != nil {
		bakedKeys = man.ImageConfig.EnvKeys
	}
	delta := CompareEnv(capturedEnv, bootEnv, bakedKeys, nil)
	if delta.Faithful() {
		return "", true
	}
	if len(delta.Missing) > 0 {
		return fmt.Sprintf("the test-restore booted without %d environment variable(s) this backup recorded: %s",
			len(delta.Missing), namedTables(delta.Missing)), false
	}
	return fmt.Sprintf("the test-restore booted with %d environment variable(s) changed: %s",
		len(delta.Changed), describeEnvChanges(delta.Changed)), false
}
