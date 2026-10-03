package backup

import (
	"fmt"
	"sort"
	"strings"
)

// Image-requirement drift (F95).
//
// A recreate faithfully restores the OLD container configuration into whatever
// image will actually run. When the image has moved on, that is exactly wrong:
// Paperless-ngx 3.0.0 added a hard startup gate on a variable the older
// configuration had no reason to set, and the restored container died in a loop
// with an application error DockBack never surfaced.
//
// This compares what the backed-up image declared against what the image about
// to run declares, and reports the differences that commonly require new
// configuration.
//
// HONEST LIMIT, stated because it changes how much the output should be trusted:
// this can only see what an image DECLARES. A project that adds a hard
// requirement WITHOUT declaring a default — which is what Paperless did — leaves
// no trace in the image config. So a version change is itself reported as a
// signal, rather than pretending silence means safety.

// ImageConfig mirrors dockercli.ImageConfig as the manifest's own type, keeping
// the on-disk contract free of runtime dependencies (the boundary VolumeRef and
// NetworkRef already sit on).
type ImageConfig struct {
	EnvKeys     []string `json:"env_keys,omitempty"`
	Entrypoint  []string `json:"entrypoint,omitempty"`
	Cmd         []string `json:"cmd,omitempty"`
	Volumes     []string `json:"volumes,omitempty"`
	Healthcheck bool     `json:"healthcheck,omitempty"`
	Version     string   `json:"version,omitempty"`
	// User is the image's own USER instruction (#24) — what this container would
	// run as if its compose file said nothing.
	User string `json:"user,omitempty"`
}

// ImageConfigDrift reports the differences between the image a backup was taken
// from and the image that will actually run.
//
// containerEnvKeys are the keys the CONTAINER already sets. A new image default
// the container already provides is not a new requirement, so it is not
// reported — otherwise every routine image bump would produce a wall of lines
// nobody reads.
//
// Pure, so every shape is table-testable without Docker.
func ImageConfigDrift(old, next ImageConfig, containerEnvKeys []string) []string {
	var out []string

	// ENV compared by KEY only. A changed default VALUE is normal and says
	// nothing about whether the container will start.
	have := setOfStrings(containerEnvKeys)
	had := setOfStrings(old.EnvKeys)
	var newKeys []string
	for _, k := range next.EnvKeys {
		if had[k] || have[k] {
			continue
		}
		newKeys = append(newKeys, k)
	}
	sort.Strings(newKeys)
	if len(newKeys) > 0 {
		out = append(out, fmt.Sprintf(
			"the new image declares %d environment default(s) this backup does not set: %s — the newer version may require them",
			len(newKeys), strings.Join(newKeys, ", ")))
	}

	// A removed default is worth knowing too: config that used to come from the
	// image now has to come from somewhere else.
	nextSet := setOfStrings(next.EnvKeys)
	var gone []string
	for _, k := range old.EnvKeys {
		if !nextSet[k] && !have[k] {
			gone = append(gone, k)
		}
	}
	sort.Strings(gone)
	if len(gone) > 0 {
		out = append(out, fmt.Sprintf(
			"the new image no longer declares: %s — anything relying on those defaults must now set them explicitly",
			strings.Join(gone, ", ")))
	}

	if !sameStrings(old.Entrypoint, next.Entrypoint) {
		out = append(out, fmt.Sprintf("ENTRYPOINT changed (%s → %s) — a restored command override may no longer be correct",
			orNothing(old.Entrypoint), orNothing(next.Entrypoint)))
	}
	if !sameStrings(old.Cmd, next.Cmd) {
		out = append(out, fmt.Sprintf("CMD changed (%s → %s)", orNothing(old.Cmd), orNothing(next.Cmd)))
	}

	oldVols := setOfStrings(old.Volumes)
	for _, v := range next.Volumes {
		if !oldVols[v] {
			out = append(out, fmt.Sprintf("the new image declares a VOLUME at %s that the backup has no data for — it will start empty", v))
		}
	}

	switch {
	case !old.Healthcheck && next.Healthcheck:
		out = append(out, "the new image adds a HEALTHCHECK — the restore's health gate will now wait for it to pass")
	case old.Healthcheck && !next.Healthcheck:
		out = append(out, "the new image removes its HEALTHCHECK — the restore can no longer confirm the container is actually working")
	}
	return out
}

// ImageChangeNote is the headline when the exact recorded image is gone and a
// different build will run.
//
// This is the line that matters most in practice. The config diff above can only
// see what an image DECLARES, and the failure that motivated this feature
// declared nothing: Paperless added a hard startup requirement with no default
// to declare, so the image config was silent while the container died in a loop.
// Reporting the change itself is the honest fallback — "the image moved and
// DockBack cannot see inside it" beats implying that silence means safety.
//
// Versions come from the image's own label, because with a floating tag the
// REFERENCE is identical across upgrades and only the digest moves.
func ImageChangeNote(oldVersion, newVersion string) string {
	if oldVersion != "" && newVersion != "" && oldVersion != newVersion {
		return fmt.Sprintf(
			"image changed since this backup (%s → %s) — a newer version may require configuration this backup does not have; check the project's release notes",
			oldVersion, newVersion)
	}
	return "the exact image this backup was taken from is no longer available, so a different build will run — check the project's release notes for new configuration requirements"
}

func setOfStrings(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func orNothing(ss []string) string {
	if len(ss) == 0 {
		return "none"
	}
	return strings.Join(ss, " ")
}
