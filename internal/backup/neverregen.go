package backup

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
)

// Values no application can re-create, and what to do when one is missing (#11).
//
// Almost everything a backup carries has another copy somewhere: the image can
// be pulled, the compose file rewritten, the configuration typed again. A
// handful of values have no other copy at all. Laravel's APP_KEY is the case R2
// §Issue 11 documents — it encrypts database columns, and a restore without it
// produces an application that starts fine, logs in fine, and has permanently
// unreadable encrypted fields. Nothing recovers from that, because the
// ciphertext is all that is left.
//
// So this is the one class where a capture that quietly misses something is
// worse than no capture: it produces an archive that looks complete and is not,
// in exactly the disaster the archive exists for. Absence is a hard failure.
//
// Two halves, deliberately asymmetric:
//
//   - A PROFILED application declares its own irreplaceable keys by name, and
//     their absence FAILS the backup. Evidence-led and short, because a wrong
//     entry here does not degrade a backup, it stops one.
//   - An UNPROFILED application gets a name-shaped guess reported as a finding
//     and nothing else. A suffix list cannot know what an unknown app requires,
//     and hard-failing on a guess would refuse to back up most stacks.

// genericSecretSuffixes name environment variables that usually hold something
// irreplaceable. Used ONLY to report, never to refuse.
var genericSecretSuffixes = []string{"_KEY", "_SECRET", "_SALT"}

// publicKeyNames are names that match the suffixes above but hold something
// public by definition. Excluded because a finding an operator learns to
// disbelieve is worse than no finding: official Python and Node images set
// GPG_KEY to a signing-key fingerprint, and it would appear in this list on
// every backup of them.
func looksPublic(key string) bool {
	upper := strings.ToUpper(key)
	return upper == "GPG_KEY" || strings.Contains(upper, "PUBLIC")
}

// envValues turns a container's environment into a lookup.
//
// Last assignment wins, which is what Docker itself does with a repeated key.
func envValues(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			continue
		}
		out[key] = value
	}
	return out
}

// missingNeverRegenerate names the declared keys this container does not carry.
//
// Empty is missing: a key present with no value carries nothing, and a backup
// that recorded APP_KEY= would restore into the same unreadable columns as one
// that recorded nothing at all.
func missingNeverRegenerate(keys []string, env []string) []string {
	if len(keys) == 0 {
		return nil
	}
	values := envValues(env)
	var missing []string
	for _, key := range keys {
		if strings.TrimSpace(values[key]) == "" {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	return missing
}

// genericSecretKeys names the environment variables that LOOK irreplaceable.
//
// Names only, always — the whole point is to say "this archive is now the only
// copy of these" without putting another copy of them anywhere.
func genericSecretKeys(env []string) []string {
	var found []string
	for key, value := range envValues(env) {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if looksPublic(key) {
			continue
		}
		upper := strings.ToUpper(key)
		for _, suffix := range genericSecretSuffixes {
			if strings.HasSuffix(upper, suffix) {
				found = append(found, key)
				break
			}
		}
	}
	sort.Strings(found)
	return found
}

// ChangedNeverRegenerateKeys names the declared keys whose value on the TARGET
// is not the one the backup was taken with.
//
// This is the restore-side half of #11, and it catches the mirror image of a
// missing capture: the data comes back encrypted under the backup's key, into a
// container configured with a different one. The application starts, and the
// restored columns are unreadable — the same permanent loss, arriving from the
// other direction.
//
// A volumes-only restore does not touch the target's environment, so this is not
// something the restore can fix; it is something the operator must decide about
// before it happens.
//
// Returns NAMES. The values are compared here and go nowhere else: not into a
// log line, not into an API response, not into a finding.
func ChangedNeverRegenerateKeys(keys, recordedEnv, liveEnv []string) []string {
	if len(keys) == 0 {
		return nil
	}
	recorded, live := envValues(recordedEnv), envValues(liveEnv)
	var changed []string
	for _, key := range keys {
		was := strings.TrimSpace(recorded[key])
		if was == "" {
			continue // the backup does not carry it; nothing to compare against
		}
		if strings.TrimSpace(live[key]) != was {
			changed = append(changed, key)
		}
	}
	sort.Strings(changed)
	return changed
}

// NeverRegenerateFor returns the keys an image's profile declares irreplaceable.
func NeverRegenerateFor(image string) []string {
	profile := ProfileFor(image)
	if profile == nil {
		return nil
	}
	return profile.NeverRegenerate
}

// ChangedSecretWarning is the sentence shown when a restore would land data
// encrypted under a key the target no longer has.
func ChangedSecretWarning(appName string, keys []string) string {
	return fmt.Sprintf(
		"restoring would keep data encrypted under a key this target no longer has: %s %s a different value here than when this backup was taken. "+
			"%s stores encrypted columns, and it can only read them with the value it wrote them under — restoring the data without the key that matches it produces an application that starts, logs in, and has permanently unreadable fields. "+
			"Set %s on this container to the value it had when the backup was taken, then restore.",
		strings.Join(keys, ", "), plural(len(keys), "has", "have"), appName,
		plural(len(keys), "that variable", "those variables"))
}

// assertNeverRegenerate is the capture-side half: a profiled application whose
// irreplaceable value is not in this container's environment cannot be backed up
// meaningfully, so it is not backed up at all.
//
// Returns an error the caller turns into a failed backup. A warning would be the
// wrong shape: the archive it warned about would sit in the list looking like
// every other successful backup, and would be discovered to be useless at the
// only moment it was ever going to be used.
func (e *Engine) assertNeverRegenerate(man *Manifest, logID, name, image string, env []string) error {
	profile := ProfileFor(image)
	if profile == nil {
		// Unprofiled: nothing is known about what this app requires, so nothing
		// is asserted. What CAN be said is which values look irreplaceable, so an
		// operator knows what this archive is now the only copy of.
		e.reportGenericSecrets(man, logID, env)
		return nil
	}
	missing := missingNeverRegenerate(profile.NeverRegenerate, env)
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("%s cannot be backed up without %s: %s %s no value in this container's environment. "+
		"%s uses %s to encrypt data it stores, and a backup taken without it restores into an application whose encrypted fields can never be read again — so this is refused rather than saved as a backup that would fail when you needed it. "+
		"Set %s on %s and run the backup again",
		profile.Name, strings.Join(missing, " and "), strings.Join(missing, " and "),
		plural(len(missing), "has", "have"), profile.Name,
		plural(len(missing), "it", "them"), strings.Join(missing, " and "), name)
}

// reportGenericSecrets records what an unprofiled application appears to hold
// that could never be re-created.
//
// Never a failure, and never a value. A suffix match is a guess about a name,
// and a guess is worth a sentence, not a refusal.
func (e *Engine) reportGenericSecrets(man *Manifest, logID string, env []string) {
	keys := genericSecretKeys(env)
	if len(keys) == 0 {
		return
	}
	e.addFinding(man, logID, findingIrreplaceableSecret, FindingInfo, "", fmt.Sprintf(
		"This container's environment holds %d %s that usually cannot be re-created: %s. "+
			"They are captured with the backup, which makes this archive the only other copy — keep it somewhere you would still have it if this host were gone. "+
			"If any of them encrypts stored data, restoring that data onto a container configured with a different value leaves it permanently unreadable.",
		len(keys), plural(len(keys), "value", "values"), strings.Join(keys, ", ")))
}

// The other place an irreplaceable value lives: a configuration file (#11).
//
// Nextcloud keeps secret and passwordsalt in config.php and neither is ever in
// the environment, so the environment-keyed assertion above cannot see them —
// and listing them there would find them absent on every container and refuse
// every backup of the application. This half asks the container instead.
//
// It reads PRESENCE, never a value: one grep per key whose only output is the
// key's own name and a verdict. The values stay where they are.

// NeverRegenerateFile names a configuration location and the keys that must be
// in it with a non-empty value.
type NeverRegenerateFile struct {
	// Path is the directory (or file) inside the container to search. A
	// directory is searched recursively, which is what makes it work for
	// applications that split configuration into fragments beside the main file.
	Path string
	// Keys are the setting names, used both to search and in every message.
	Keys []string
}

// safeConfigKey bounds what may be built into a regular expression. These are
// our own constants, so this is an assertion rather than a sanitiser — a key
// that could not be a setting name never becomes a pattern.
var safeConfigKey = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]*$`)

// configKeyPattern matches a key assigned a NON-EMPTY value, in any of the
// shapes configuration files actually use: PHP's `'secret' => 'x'`, ini's
// `secret=x`, YAML's `secret: x`, JSON's `"secret": "x"`.
//
// Two details are load-bearing, and both were found by running it:
//
//   - The leading (^|[^A-Za-z0-9_]) stops 'dbsecret' from vouching for 'secret'.
//     Without it, an application that has the wrong one of the two passes.
//   - '>' is excluded from the value class. The '=' alternative would otherwise
//     let the '>' of '=>' serve as the value, so `'secret' => ”` — the exact
//     shape of a key that exists but holds nothing — reported as present.
func configKeyPattern(key string) string {
	return `(^|[^A-Za-z0-9_])['"]?` + key + `['"]?[[:space:]]*(=>|=|:)[[:space:]]*['"]?[^'",>[:space:]]`
}

// Verdict tags for the configuration probe. Only ever a key name follows.
const (
	configKeyFound      = "FOUND"
	configKeyMissing    = "MISSING"
	configUnreadable    = "UNREADABLE"
	configProbeMaxDepth = 2
)

// neverRegenerateFileScript builds the probe for one declared location.
func neverRegenerateFileScript(spec NeverRegenerateFile) string {
	var script strings.Builder
	script.WriteString("p='" + shellEscape(spec.Path) + "'; ")
	script.WriteString(`if [ ! -e "$p" ]; then printf '` + configUnreadable + `|%s\n' "$p"; exit 0; fi; `)
	for _, key := range spec.Keys {
		if !safeConfigKey.MatchString(key) {
			continue
		}
		quoted := "'" + shellEscape(key) + "'"
		script.WriteString(`if grep -r -s -q -E '` + shellEscape(configKeyPattern(key)) + `' "$p"; then `)
		script.WriteString(`printf '` + configKeyFound + `|%s\n' ` + quoted + `; else `)
		script.WriteString(`printf '` + configKeyMissing + `|%s\n' ` + quoted + `; fi; `)
	}
	return script.String()
}

// parseConfigKeyProbe reads the probe. unreadable means no assertion can be made
// at all — which is never the same thing as a key being absent.
func parseConfigKeyProbe(out string) (present map[string]bool, unreadable bool) {
	present = map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		verdict, name, ok := strings.Cut(strings.TrimSpace(line), "|")
		if !ok || name == "" {
			continue
		}
		switch verdict {
		case configKeyFound:
			present[name] = true
		case configKeyMissing:
			if _, seen := present[name]; !seen {
				present[name] = false
			}
		case configUnreadable:
			unreadable = true
		}
	}
	return present, unreadable
}

// missingConfigKeys names the declared keys the probe positively reported absent.
//
// A key the probe said nothing about is not counted. Silence is not absence, and
// the whole risk of this feature is a hard failure raised on something that was
// simply not looked at.
func missingConfigKeys(keys []string, present map[string]bool) []string {
	var missing []string
	for _, key := range keys {
		if found, reported := present[key]; reported && !found {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	return missing
}

// assertNeverRegenerateFiles is the configuration-file half of the capture
// assertion.
//
// It fails a backup ONLY on a positive read showing the key absent. A path that
// is not there, a probe that could not run, an unreadable directory — each is
// one log line and the backup proceeds. That asymmetry is the whole design: the
// cost of missing an assertion is a backup that might be incomplete, and the
// cost of a wrong assertion is refusing every backup of an application.
func (e *Engine) assertNeverRegenerateFiles(ctx context.Context, cli *client.Client, containerID string, man *Manifest, logID, name, image string, running bool) error {
	profile := ProfileFor(image)
	if profile == nil || len(profile.NeverRegenerateFiles) == 0 {
		return nil
	}
	for _, spec := range profile.NeverRegenerateFiles {
		if strings.TrimSpace(spec.Path) == "" || len(spec.Keys) == 0 {
			continue
		}
		out, err := e.readConfigKeys(ctx, cli, containerID, spec, running)
		if err != nil {
			e.logf(logID, "INFO", "Could not check %s for %s's irreplaceable settings (%v) — the backup continues, but it was not confirmed to carry %s",
				spec.Path, profile.Name, err, strings.Join(spec.Keys, " and "))
			continue
		}
		present, unreadable := parseConfigKeyProbe(out)
		if unreadable {
			e.logf(logID, "INFO", "%s is not present in this container, so %s's %s could not be confirmed — the backup continues",
				spec.Path, profile.Name, strings.Join(spec.Keys, " and "))
			continue
		}
		missing := missingConfigKeys(spec.Keys, present)
		if len(missing) == 0 {
			continue
		}
		return fmt.Errorf("%s cannot be backed up without %s: %s %s no value in %s. "+
			"%s uses %s to encrypt what it stores and to salt its password hashes, and a backup taken without %s restores into an instance that logs nobody in and cannot read what it had encrypted — so this is refused rather than saved as a backup that would fail when you needed it. "+
			"If this instance has not finished its first-run setup yet, there is nothing to back up; otherwise check %s on %s",
			profile.Name, strings.Join(missing, " and "), strings.Join(missing, " and "),
			plural(len(missing), "has", "have"), spec.Path, profile.Name,
			plural(len(missing), "it", "them"), plural(len(missing), "it", "them"), spec.Path, name)
	}
	return nil
}

// readConfigKeys runs the probe where it can actually see the file.
//
// A running container is asked directly, because its configuration may live in
// the image's own filesystem rather than in a mount. A stopped one is reached
// through a sidecar, which sees its mounts — less, but the only thing available,
// and a miss degrades to "could not confirm" rather than to a refusal.
func (e *Engine) readConfigKeys(ctx context.Context, cli *client.Client, containerID string, spec NeverRegenerateFile, running bool) (string, error) {
	cmd := []string{"/bin/sh", "-c", neverRegenerateFileScript(spec)}
	if running {
		out, err := dockercli.ExecCapture(ctx, cli, containerID, cmd)
		return string(out), err
	}
	out, err := dockercli.CaptureSidecarRO(ctx, cli, containerID, cmd)
	return string(out), err
}
