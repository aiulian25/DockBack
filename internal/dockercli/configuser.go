package dockercli

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types"
)

// The third UID model: a `user:` override in the compose file (#24).
//
// R3 §Issue 24: Nextcloud-REDIS declared `user: 1026:100` — "Synology's uid/gid,
// hardcoded in compose. On the target, 1026 does not exist. Redis would run as a
// numeric uid with no passwd entry, against a /data owned by someone else."
//
// PLAYBOOK §7.2 puts it beside the other two: env-configurable (PUID/PGID),
// image-baked (USER in the Dockerfile), and this one — "the easiest to miss
// because it lives in the stack file rather than in the image or environment."
// F117's data alignment fixes the FILES; only this fixes the PROCESS, and a
// restore that corrects one without the other has just moved the mismatch.

// numericUserPattern matches the only form of `user:` that is host-specific:
// bare numbers. A NAME is resolved against /etc/passwd inside the image, which
// travels with the image — `user: postgres` means the same thing on every host
// and must never be rewritten.
var numericUserPattern = regexp.MustCompile(`^\d+(:\d+)?$`)

// NumericUser reads a `user:` override that is expressed as ids.
//
// Returns ok=false for an empty value, for a name, and for the name:group and
// uid:name mixtures — anything the image itself has to resolve is the image's
// business and stays exactly as recorded. A value with no group half reports
// gid = -1, meaning "not stated", which is different from group 0.
func NumericUser(user string) (uid, gid int, ok bool) {
	u := strings.TrimSpace(user)
	if !numericUserPattern.MatchString(u) {
		return 0, -1, false
	}
	uidPart, gidPart, hasGID := strings.Cut(u, ":")
	uid, err := strconv.Atoi(uidPart)
	if err != nil {
		return 0, -1, false
	}
	if !hasGID {
		return uid, -1, true
	}
	gid, err = strconv.Atoi(gidPart)
	if err != nil {
		return 0, -1, false
	}
	return uid, gid, true
}

// ConfigUser returns the `user:` a recorded inspect carries, if any.
func ConfigUser(inspectJSON []byte) string {
	var insp types.ContainerJSON
	if json.Unmarshal(inspectJSON, &insp) != nil || insp.Config == nil {
		return ""
	}
	return strings.TrimSpace(insp.Config.User)
}

// RewriteConfigUser points a NUMERIC `user:` override at the ids pinned for this
// container's restores.
//
// Three things it will not do, each for its own reason:
//
//   - A named user (`user: postgres`, `user: redis`) is left exactly as
//     recorded. The name resolves inside the image, so it is already portable,
//     and replacing it with a number would discard the group memberships the
//     image set up for that account.
//   - A container with no `user:` at all gets none. Adding one would be
//     introducing configuration the source does not have (#31), and it would
//     override the image's own USER — the model that is already correct
//     everywhere.
//   - An override that already names the pinned ids comes back byte-identical,
//     so re-running a restore cannot churn the document.
func RewriteConfigUser(inspectJSON []byte, uid, gid int) ([]byte, bool, error) {
	current := ConfigUser(inspectJSON)
	if _, _, numeric := NumericUser(current); !numeric {
		return inspectJSON, false, nil
	}
	want := strconv.Itoa(uid) + ":" + strconv.Itoa(gid)
	if current == want {
		return inspectJSON, false, nil
	}
	var insp types.ContainerJSON
	if err := json.Unmarshal(inspectJSON, &insp); err != nil {
		return inspectJSON, false, err
	}
	if insp.Config == nil {
		return inspectJSON, false, nil
	}
	insp.Config.User = want
	out, err := json.MarshalIndent(&insp, "", "  ")
	if err != nil {
		return inspectJSON, false, err
	}
	return out, true, nil
}
