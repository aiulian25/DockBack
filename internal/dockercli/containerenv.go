package dockercli

import (
	"encoding/json"
	"strings"

	"github.com/docker/docker/api/types"
)

// Environment rewriting for the recreated container (F114).
//
// RemapContainerHostIP already rewrites env values, but only where the OLD IP
// literally appears. That does not cover the case this exists for: an app whose
// address is recorded as a hostname, or whose accepted-hosts list simply does
// not contain the new machine yet. Homepage returns a bare HTTP 400 for any Host
// header outside HOMEPAGE_ALLOWED_HOSTS, so a cross-machine restore is reported
// green and the dashboard is unreachable.
//
// Editing an environment variable is chosen deliberately over editing the app's
// own config: it is undone by recreating the container again, so it is the one
// class of address fix DockBack can safely apply on the operator's behalf.

// SetContainerEnv sets an environment variable in a container's inspect JSON,
// returning the updated JSON and whether anything changed.
//
// The key is only ever SET, never removed. A key absent from the container is
// added, because "the app does not currently declare this" is exactly the state
// on a first move to a new address.
func SetContainerEnv(inspectJSON []byte, key, value string) ([]byte, bool, error) {
	if key == "" {
		return inspectJSON, false, nil
	}
	var insp types.ContainerJSON
	if err := json.Unmarshal(inspectJSON, &insp); err != nil {
		return inspectJSON, false, err
	}
	if insp.Config == nil {
		return inspectJSON, false, nil
	}
	want := key + "=" + value
	found, changed := false, false
	for i, e := range insp.Config.Env {
		if k, _, ok := strings.Cut(e, "="); ok && k == key {
			found = true
			if e != want {
				insp.Config.Env[i] = want
				changed = true
			}
			break
		}
	}
	if !found {
		insp.Config.Env = append(insp.Config.Env, want)
		changed = true
	}
	if !changed {
		return inspectJSON, false, nil
	}
	out, err := json.MarshalIndent(&insp, "", "  ")
	if err != nil {
		return inspectJSON, false, err
	}
	return out, true, nil
}

// ContainerEnvValue reads one environment variable out of inspect JSON.
//
// Deliberately reads a NAMED key rather than returning the environment: a
// container environment routinely holds database passwords and API tokens, and
// the callers of this feed run-log messages an operator may share when asking
// for help. Narrowing at the source beats filtering later.
func ContainerEnvValue(inspectJSON []byte, key string) string {
	if key == "" {
		return ""
	}
	var insp types.ContainerJSON
	if err := json.Unmarshal(inspectJSON, &insp); err != nil || insp.Config == nil {
		return ""
	}
	for _, e := range insp.Config.Env {
		if k, v, ok := strings.Cut(e, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// ContainerEnv returns a saved container's environment, or nil when the inspect
// cannot be read. Used to ask which convention an image declares before
// rewriting it (F189).
func ContainerEnv(inspectJSON []byte) []string {
	var insp types.ContainerJSON
	if err := json.Unmarshal(inspectJSON, &insp); err != nil || insp.Config == nil {
		return nil
	}
	return insp.Config.Env
}

// AddToEnvList adds an entry to a comma-separated environment value, returning
// the new value and whether it changed.
//
// It ADDS rather than replaces on purpose. HOMEPAGE_ALLOWED_HOSTS and its
// equivalents are the set of addresses the app answers on, and overwriting the
// set with a single new entry would lock the app out at every address it
// currently serves — turning a fix for one address into an outage at all the
// others. An entry already present is left alone, so re-running converges.
func AddToEnvList(current, entry string) (string, bool) {
	return AddToEnvListSep(current, entry, ",")
}

// AddToEnvListSep is AddToEnvList for a variable that separates its entries with
// something other than a comma.
//
// Nextcloud's NEXTCLOUD_TRUSTED_DOMAINS is the case that forced this: its
// entrypoint word-splits the value, so the separator is a SPACE. Appending with
// a comma there does not add a domain — it produces one entry containing both,
// which matches no request at all.
func AddToEnvListSep(current, entry, sep string) (string, bool) {
	if sep == "" {
		sep = ","
	}
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return current, false
	}
	var out []string
	for _, p := range strings.Split(current, sep) {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		if strings.EqualFold(p, entry) {
			return current, false // already accepted here
		}
		out = append(out, p)
	}
	out = append(out, entry)
	return strings.Join(out, sep), true
}
