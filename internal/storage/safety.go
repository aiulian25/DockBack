package storage

import (
	"errors"
	"strings"
)

// ErrUnsafeKey is returned for keys that are empty or attempt to escape the
// configured backup sub-path.
var ErrUnsafeKey = errors.New("unsafe storage key")

// validateKey enforces that every storage operation stays strictly inside the
// destination's configured sub-path: no empty keys (which could target the
// folder root) and no "." / ".." segments (path traversal). This is the
// guarantee that DockBack can never read, overwrite, or delete a user's other
// files on a shared destination (e.g. a Nextcloud account) — it only ever
// touches objects under the backup folder it was given.
func validateKey(key string) error {
	k := strings.Trim(strings.ReplaceAll(key, "\\", "/"), "/")
	if k == "" {
		return ErrUnsafeKey
	}
	for _, seg := range strings.Split(k, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return ErrUnsafeKey
		}
	}
	return nil
}
