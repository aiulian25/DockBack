package backup

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"dockback/internal/store"
)

// Canonical archive layout: <node>/<stack>/<container>/<timestamp>_<id>.dback
// (no stack level for standalone containers). backupKey (capture time) and
// CanonicalKey (migration of pre-existing archives, F77) both go through
// layoutKey, so there is exactly ONE definition of the on-storage layout.

// layoutKey joins the layout segments, each sanitized SEPARATELY (never a
// combined string) so no segment can escape its directory — the storage
// backend's safePath still confines the whole key to the root.
func layoutKey(nodeName, stack, name, file string) string {
	node := sanitize(nodeName)
	if node == "" {
		node = "unknown-node"
	}
	if stack != "" {
		return filepath.Join(node, sanitize(stack), sanitize(name), file)
	}
	return filepath.Join(node, sanitize(name), file)
}

// CanonicalKey returns the storage key an EXISTING backup should live at under
// the per-stack layout (F77). The original basename is preserved when it looks
// like a sane archive filename (keeping uniqueness and the capture timestamp);
// otherwise one is derived from the row's CreatedAt + short id. Pure.
func CanonicalKey(b *store.Backup, man *Manifest) string {
	node := man.NodeName
	if node == "" {
		node = b.NodeID
	}
	stack := b.Stack
	if stack == "" {
		stack = man.Stack
	}
	name := b.TargetName
	if name == "" {
		name = man.TargetName
	}
	base := ""
	if b.StorageKey != "" {
		base = sanitize(filepath.Base(b.StorageKey))
	}
	if base == "" || base == "unnamed" || !strings.HasSuffix(base, ".dback") {
		id := b.ID
		if len(id) > 8 {
			id = id[:8]
		}
		base = fmt.Sprintf("%s_%s.dback", time.Unix(b.CreatedAt, 0).UTC().Format("2006-01-02_15-04-05"), id)
	}
	return layoutKey(node, stack, name, base)
}

// IsCanonicalKey reports whether a backup already lives at its canonical key —
// such rows are skipped by the layout migration (F77).
func IsCanonicalKey(b *store.Backup, man *Manifest) bool {
	return b.StorageKey != "" && b.StorageKey == CanonicalKey(b, man)
}
