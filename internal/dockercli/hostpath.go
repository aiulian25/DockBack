package dockercli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/mount"
)

// Host-path remap for cross-host restores (F81), mirroring the shipped host-IP
// remap (hostip.go): rewrite the SOURCE machine's base directory to the TARGET
// machine's in the bits that would otherwise materialize the old layout on the
// new host — recreated bind-mount sources and the reconstructed compose file.
// Exact prefix matches only; everything else is left byte-identical.

// normalizePathBases vets a from/to base pair: both must be absolute, cleaned,
// different, and neither may be "/" (a root base would rewrite every absolute
// path). Returns the cleaned pair.
func normalizePathBases(fromBase, toBase string) (string, string, bool) {
	f := path.Clean(strings.TrimSpace(fromBase))
	t := path.Clean(strings.TrimSpace(toBase))
	if !path.IsAbs(f) || !path.IsAbs(t) || f == "/" || t == "/" || f == t {
		return "", "", false
	}
	return f, t, true
}

// pathTokenStart reports whether a match starting at data[i] begins a path
// token. Deliberately narrow: only start-of-input, whitespace, and quotes
// qualify — NOT ':' (the text after a bind spec's colon is the CONTAINER path,
// which must never be rewritten) and NOT '}' or alphanumerics (so
// "${HOME}/opt/app" or "/mnt/opt/app" never match a base of "/opt/app").
func pathTokenStart(data []byte, i int) bool {
	if i == 0 {
		return true
	}
	switch data[i-1] {
	case ' ', '\t', '\n', '\r', '"', '\'', '=':
		return true
	}
	return false
}

// pathTokenEnd reports whether the character after the matched base ends the
// base component: a deeper path ('/'), a bind-spec separator (':'), a closing
// quote, whitespace, or end of input — so a base of /opt/app never rewrites
// /opt/app2.
func pathTokenEnd(data []byte, i int) bool {
	if i >= len(data) {
		return true
	}
	switch data[i] {
	case '/', ':', '"', '\'', ' ', '\t', '\n', '\r':
		return true
	}
	return false
}

// RemapTextHostPath replaces occurrences of fromBase with toBase in a text blob
// (the reconstructed compose file) — only where the occurrence is a whole base
// component at a path-token start, so container-side paths after a bind colon,
// variable-suffixed paths, and longer sibling directories are never touched.
// Returns the new bytes and the substitution count; (data, 0) when the bases
// are invalid, equal, or absent.
func RemapTextHostPath(data []byte, fromBase, toBase string) ([]byte, int) {
	from, to, ok := normalizePathBases(fromBase, toBase)
	if !ok {
		return data, 0
	}
	var out []byte
	n, i := 0, 0
	for {
		j := bytes.Index(data[i:], []byte(from))
		if j < 0 {
			break
		}
		j += i
		if pathTokenStart(data, j) && pathTokenEnd(data, j+len(from)) {
			out = append(out, data[i:j]...)
			out = append(out, to...)
			n++
			i = j + len(from)
		} else {
			out = append(out, data[i:j+1]...)
			i = j + 1
		}
	}
	if n == 0 {
		return data, 0
	}
	out = append(out, data[i:]...)
	return out, n
}

// remapPathPrefix rewrites p when it equals fromBase or lives under it;
// returns the (possibly rewritten) path and whether it changed.
func remapPathPrefix(p, from, to string) (string, bool) {
	if p == from {
		return to, true
	}
	if strings.HasPrefix(p, from+"/") {
		return to + p[len(from):], true
	}
	return p, false
}

// RemapHostPath applies the F81 base rewrite to ONE host path — the same rule
// RemapContainerHostPath applies across a whole inspect document.
//
// It exists because a bind whose contents travel outside the volume archive is
// written to the host by path, not by rewriting a document: the restore holds
// the source machine's path and has to land it on the target's layout. Doing
// that with the shared rule rather than a second string replacement is what
// keeps the file beside the directories it belongs with.
//
// Returns p unchanged when either base is missing, invalid, or does not match.
func RemapHostPath(p, fromBase, toBase string) string {
	from, to, ok := normalizePathBases(fromBase, toBase)
	if !ok {
		return p
	}
	out, _ := remapPathPrefix(p, from, to)
	return out
}

// RemapContainerHostPath rewrites a container's saved `docker inspect` JSON so
// bind-mount HOST sources under the source machine's base directory land under
// the target machine's instead:
//
//   - HostConfig.Binds entries' source segment (the text before the first ':',
//     handling both "src:dst" and "src:dst:opts" forms; named-volume sources —
//     no leading '/' — are never touched);
//   - HostConfig.Mounts[].Source for Type=="bind" — compose long-syntax and
//     `--mount` land here, and this is the field ContainerCreate actually reads;
//   - Mounts[].Source for Type=="bind".
//
// All three, because a container's binds are not all in one place and missing
// one is silent. HostConfig.Mounts was the one missed: the top-level Mounts list
// is read-only OUTPUT from inspect and changing it affects nothing, so a restore
// could report every path remapped, create them all at the new location, and
// still be refused by the daemon asking for the old one — which is exactly what
// a single `--mount`-declared secret did.
//
// Exact prefix matches only (fromBase itself or fromBase/...). Returns the
// (possibly rewritten) JSON and the substitution count. On any problem it
// returns the original bytes with 0 changes, so a restore is never blocked by
// a best-effort remap.
func RemapContainerHostPath(inspectJSON []byte, fromBase, toBase string) ([]byte, int, error) {
	from, to, ok := normalizePathBases(fromBase, toBase)
	if !ok {
		return inspectJSON, 0, fmt.Errorf("path remap needs two different absolute base directories (never /)")
	}
	var insp types.ContainerJSON
	if err := json.Unmarshal(inspectJSON, &insp); err != nil {
		return inspectJSON, 0, err
	}
	changes := 0
	if insp.HostConfig != nil {
		for i, bind := range insp.HostConfig.Binds {
			src, rest, found := strings.Cut(bind, ":")
			if !found || !strings.HasPrefix(src, "/") {
				continue // malformed or a named-volume source — never remapped
			}
			if ns, changed := remapPathPrefix(src, from, to); changed {
				insp.HostConfig.Binds[i] = ns + ":" + rest
				changes++
			}
		}
		// The authoritative one: ContainerCreate reads HostConfig, and this is
		// where compose's long-syntax volumes and `--mount` put a bind.
		for i := range insp.HostConfig.Mounts {
			if insp.HostConfig.Mounts[i].Type != mount.TypeBind {
				continue
			}
			if ns, changed := remapPathPrefix(insp.HostConfig.Mounts[i].Source, from, to); changed {
				insp.HostConfig.Mounts[i].Source = ns
				changes++
			}
		}
	}
	for i := range insp.Mounts {
		if insp.Mounts[i].Type != "bind" {
			continue
		}
		if ns, changed := remapPathPrefix(insp.Mounts[i].Source, from, to); changed {
			insp.Mounts[i].Source = ns
			changes++
		}
	}
	if changes == 0 {
		return inspectJSON, 0, nil
	}
	out, err := json.MarshalIndent(&insp, "", "  ")
	if err != nil {
		return inspectJSON, 0, err
	}
	return out, changes, nil
}
