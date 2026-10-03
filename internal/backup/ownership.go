package backup

import (
	"fmt"
	"strconv"
	"strings"

	"dockback/internal/dockercli"
)

// Per-container restore ownership (F184).
//
// F117 aligns restored data to the ids the TARGET container declares, read from
// its environment — PUID/PGID, USERMAP_UID/USERMAP_GID and the other spellings
// images use to say which user they drop to. That covers the images that
// announce it and, by construction, nothing else: an image running as a fixed
// baked-in user (www-data, or a numeric USER in its Dockerfile) declares nothing,
// so there is nothing to detect and the restored files keep whatever ownership
// the archive recorded.
//
// Usually that is right — a uid baked into an image is the same uid everywhere.
// It stops being right the moment the data came from a machine that numbered its
// users differently, which is exactly what moving off a NAS is: a Synology share
// owned by 1026:100 restored onto a host where the application runs as 1000:1000,
// or as 33.
//
// DockBack cannot infer the answer in that case, and guessing at ownership is a
// good way to make an application unable to read its own files. So this is the
// operator saying it outright, per container: restore this one's data owned by
// these ids. It beats the detected value when set, and it is the only thing that
// applies at all when nothing is detected.
//
// Stored per container, so a stack restore picks it up for each service on its
// own — a database that runs as 999 and an application that runs as 1000 need
// different answers, and one setting for a whole stack would be wrong for at
// least one of them.

const restoreOwnershipKey = "restore.ownership"

// maxID bounds an accepted id. Linux allows more, but a value beyond this is a
// typo far more often than a real account, and chowning a restore to a
// mistyped id is not something the operator finds out about quickly.
const maxID = 1 << 22

func restoreOwnershipKeyFor(nodeID, name string) string {
	return restoreOwnershipKey + "." + nodeID + "." + name
}

// RestoreOwnership returns the uid/gid an operator pinned for this container's
// restores, and whether one is set.
//
// A stored value that cannot be parsed reports "not set" rather than a partial
// guess: half of a pair is not an owner, and falling back to detection is the
// behaviour the operator had before they typed anything.
func (e *Engine) RestoreOwnership(nodeID, name string) (uid, gid int, ok bool) {
	v, _ := e.Store.GetSetting(restoreOwnershipKeyFor(nodeID, name), "")
	return ParseOwnership(v)
}

// SetRestoreOwnership pins the ids, or clears the pin when spec is empty.
func (e *Engine) SetRestoreOwnership(nodeID, name, spec string) error {
	key := restoreOwnershipKeyFor(nodeID, name)
	if strings.TrimSpace(spec) == "" {
		return e.Store.SetSetting(key, "")
	}
	uid, gid, ok := ParseOwnership(spec)
	if !ok {
		return fmt.Errorf("ownership must be written as uid:gid, two numbers between 0 and %d — for example 1000:1000", maxID)
	}
	return e.Store.SetSetting(key, fmt.Sprintf("%d:%d", uid, gid))
}

// RestoreOwnershipSpec renders the stored pin for the API, empty when none is
// set.
func (e *Engine) RestoreOwnershipSpec(nodeID, name string) string {
	uid, gid, ok := e.RestoreOwnership(nodeID, name)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d", uid, gid)
}

// ParseOwnership reads a "uid:gid" pair. Exported so the API rejects a bad value
// at the boundary with the same rule that stores it, rather than a second
// implementation that can drift from this one.
//
// Numeric only, and deliberately: names are resolved against /etc/passwd INSIDE
// a container, which is a different file on every image and does not exist at
// all on a distroless one. A number means the same thing everywhere, which is
// the entire reason restores are numeric in the first place.
func ParseOwnership(spec string) (uid, gid int, ok bool) {
	u, g, found := strings.Cut(strings.TrimSpace(spec), ":")
	if !found {
		return 0, 0, false
	}
	uid, uerr := strconv.Atoi(strings.TrimSpace(u))
	gid, gerr := strconv.Atoi(strings.TrimSpace(g))
	if uerr != nil || gerr != nil || uid < 0 || gid < 0 || uid > maxID || gid > maxID {
		return 0, 0, false
	}
	return uid, gid, true
}

// HostFileOwner is the "uid:gid" a reconstructed stack folder should belong to
// when DockBack has just created its parent (F230).
//
// Preference order, most explicit first: the operator's pinned restore
// ownership for this container (F184), then the ids the container's own
// user-mapping variables declare (PUID/PGID and friends), then nothing — in
// which case the writer keeps matching the parent, whatever it is.
//
// Never guesses a non-root id out of thin air: "" means "no better answer than
// the parent", and the caller treats it as such. Pure, so the precedence is
// unit-testable.
func HostFileOwner(pinnedUID, pinnedGID int, pinned bool, env []string) string {
	if pinned {
		return fmt.Sprintf("%d:%d", pinnedUID, pinnedGID)
	}
	if uid, gid, _, ok := dockercli.RunAsIDs(env); ok && (uid != 0 || gid != 0) {
		return fmt.Sprintf("%d:%d", uid, gid)
	}
	return ""
}
