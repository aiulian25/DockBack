package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Cluster registry (F104).
//
// A cluster was previously nothing but a free-text string typed into the node
// form: two nodes were "in the same cluster" only because someone spelled the
// word identically twice. That made `prod` and `Prod` two different fleets,
// renaming a cluster a per-node chore, and hanging anything (policy, limits,
// scoping) off a cluster impossible.
//
// This registry makes the cluster a real record while KEEPING the node's own
// `cluster` column as the source of membership, storing the cluster NAME rather
// than an opaque id. That choice matters: every existing node row, every API
// response, every export and every stored `cluster:` policy scope stays valid
// with no data migration and no dual-write.
//
// The name is the primary key under NOCASE collation, so `prod` and `Prod`
// cannot both exist, and node writes canonicalize to the registered spelling —
// which is what actually stops a fleet silently splitting in two.
//
// DELETING A CLUSTER NEVER DELETES DATA. A cluster is a grouping over nodes; it
// owns no nodes, no containers, no backups and no archives. Removing one drops
// the registry row and its policy override, and (optionally, in the same
// transaction) reassigns its nodes to another cluster. Nodes that still belong
// to it block the delete rather than being silently orphaned.

// Cluster is one registered cluster. Nodes is a derived count, not stored.
type Cluster struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Color       string `json:"color"`
	CreatedAt   int64  `json:"created_at"`
	Nodes       int    `json:"nodes"`
}

// DefaultCluster is the cluster a node lands in when none is chosen. It matches
// the `nodes.cluster` column default, so the registry and the schema agree.
const DefaultCluster = "default"

// maxClusterNameLen bounds a name so it stays renderable in a chip and safe as a
// URL path segment and a policy-scope key.
const maxClusterNameLen = 64

// ErrClusterExists / ErrClusterInUse are the two conditions callers must be able
// to tell apart from a generic failure (they map to 409, not 500).
var (
	ErrClusterExists = errors.New("a cluster with that name already exists")
	ErrClusterInUse  = errors.New("cluster still has nodes")
)

// ClusterScope builds the policy_overrides key for a cluster. It sits between
// the global policy and a node override in the precedence chain, using the same
// opaque-scope mechanism the node and container scopes already use — no new
// table and no new resolution model.
func ClusterScope(name string) string { return "cluster:" + name }

// ValidateClusterName reports why a name is unusable, or nil if it is fine.
//
// It is deliberately permissive about content (a cluster is a human label, and
// non-ASCII names must work) and strict about the two things that would break
// something downstream: length, and characters that are structural elsewhere —
// "/" would split a URL path segment and blur the `container:<node>/<name>`
// scope shape, and control characters would corrupt a log line or a Prometheus
// label.
func ValidateClusterName(name string) error {
	if name == "" {
		return errors.New("a cluster name is required")
	}
	if name != strings.TrimSpace(name) {
		return errors.New("a cluster name can't start or end with a space")
	}
	if len([]rune(name)) > maxClusterNameLen {
		return fmt.Errorf("a cluster name can be at most %d characters", maxClusterNameLen)
	}
	if strings.ContainsAny(name, "/\\") {
		return errors.New(`a cluster name can't contain "/" or "\"`)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return errors.New("a cluster name can't contain control characters")
		}
	}
	return nil
}

// ListClusters returns every registered cluster with its live node count
// (excluding soft-deleted nodes), ordered by name.
//
// The count is a correlated subquery rather than a join+group so a cluster with
// no nodes still appears — an empty cluster is exactly the one a user is about
// to assign nodes to, or delete.
func (s *Store) ListClusters() ([]*Cluster, error) {
	rows, err := s.db.Query(`
		SELECT c.name, c.description, c.color, c.created_at,
		       (SELECT COUNT(*) FROM nodes n WHERE n.deleted_at=0 AND n.cluster = c.name COLLATE NOCASE)
		FROM clusters c ORDER BY c.name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Cluster{}
	for rows.Next() {
		c := &Cluster{}
		if err := rows.Scan(&c.Name, &c.Description, &c.Color, &c.CreatedAt, &c.Nodes); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetCluster returns one cluster (with its node count). Lookup is case-
// insensitive, matching the primary key's collation.
func (s *Store) GetCluster(name string) (*Cluster, error) {
	c := &Cluster{}
	err := s.db.QueryRow(`
		SELECT c.name, c.description, c.color, c.created_at,
		       (SELECT COUNT(*) FROM nodes n WHERE n.deleted_at=0 AND n.cluster = c.name COLLATE NOCASE)
		FROM clusters c WHERE c.name = ? COLLATE NOCASE`, name).
		Scan(&c.Name, &c.Description, &c.Color, &c.CreatedAt, &c.Nodes)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// CreateCluster registers a new cluster, failing with ErrClusterExists if the
// name is already taken under case-insensitive comparison.
func (s *Store) CreateCluster(name, description, color string) error {
	if err := ValidateClusterName(name); err != nil {
		return err
	}
	var existing string
	err := s.db.QueryRow(`SELECT name FROM clusters WHERE name = ? COLLATE NOCASE`, name).Scan(&existing)
	if err == nil {
		return fmt.Errorf("%w: %q", ErrClusterExists, existing)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO clusters(name, description, color, created_at) VALUES(?,?,?,?)`,
		name, description, color, now())
	return err
}

// UpdateCluster edits a cluster's description and colour. The name is not
// touched here — renaming has to move node membership and the policy scope too,
// so it is RenameCluster's job.
func (s *Store) UpdateCluster(name, description, color string) error {
	res, err := s.db.Exec(`UPDATE clusters SET description=?, color=? WHERE name = ? COLLATE NOCASE`,
		description, color, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// EnsureCluster registers name if it is not known yet and returns the CANONICAL
// spelling — the one already stored when the cluster exists under a different
// case. Node writes run every submitted cluster through this, which is what
// keeps `prod` and `Prod` from becoming two fleets.
//
// An empty name resolves to DefaultCluster, matching the column default, so a
// node is never left pointing at a cluster that does not exist.
func (s *Store) EnsureCluster(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = DefaultCluster
	}
	if err := ValidateClusterName(name); err != nil {
		return "", err
	}
	var canonical string
	err := s.db.QueryRow(`SELECT name FROM clusters WHERE name = ? COLLATE NOCASE`, name).Scan(&canonical)
	if err == nil {
		return canonical, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if _, err := s.db.Exec(`INSERT INTO clusters(name, description, color, created_at) VALUES(?,'','',?)`,
		name, now()); err != nil {
		return "", err
	}
	return name, nil
}

// RenameCluster renames a cluster and carries its membership and policy with it,
// atomically: the registry row, every node pointing at the old name, and the
// `cluster:<name>` policy-override key all move together.
//
// Renaming to a different spelling of the SAME name (prod -> Prod) is allowed —
// it is a re-casing, not a collision.
func (s *Store) RenameCluster(oldName, newName string) error {
	if err := ValidateClusterName(newName); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var current string
	if err := tx.QueryRow(`SELECT name FROM clusters WHERE name = ? COLLATE NOCASE`, oldName).Scan(&current); err != nil {
		return err
	}
	if current == newName {
		return nil // nothing to do
	}
	// A different cluster already holding the target name is a real collision;
	// the same row under another case is just a re-casing of itself.
	var clash string
	err = tx.QueryRow(`SELECT name FROM clusters WHERE name = ? COLLATE NOCASE AND name <> ?`, newName, current).Scan(&clash)
	if err == nil {
		return fmt.Errorf("%w: %q", ErrClusterExists, clash)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	if _, err := tx.Exec(`UPDATE clusters SET name=? WHERE name=?`, newName, current); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE nodes SET cluster=? WHERE cluster = ? COLLATE NOCASE`, newName, current); err != nil {
		return err
	}
	// Move the policy override. A row already sitting on the target scope would
	// belong to a cluster that cannot exist (the collision check above ruled it
	// out), so it is stale — replace it rather than failing the rename.
	if _, err := tx.Exec(`DELETE FROM policy_overrides WHERE scope=?`, ClusterScope(newName)); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE policy_overrides SET scope=? WHERE scope=?`,
		ClusterScope(newName), ClusterScope(current)); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteCluster removes a cluster from the registry. It deletes NO node, NO
// container, NO backup and NO archive — a cluster is a grouping over nodes and
// owns none of them.
//
// What it does remove is the registry row and the cluster's policy override, so
// any node that was inheriting from it falls back to the global policy (or to
// its own node override, which is untouched).
//
// A cluster that still has nodes is refused with ErrClusterInUse unless
// reassignTo names another cluster, in which case those nodes are moved first —
// in the same transaction, so there is no window where a node points at a
// cluster that no longer exists.
func (s *Store) DeleteCluster(name, reassignTo string) (moved int, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var current string
	if err := tx.QueryRow(`SELECT name FROM clusters WHERE name = ? COLLATE NOCASE`, name).Scan(&current); err != nil {
		return 0, err
	}

	var members int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM nodes WHERE deleted_at=0 AND cluster = ? COLLATE NOCASE`, current).
		Scan(&members); err != nil {
		return 0, err
	}
	if members > 0 {
		if reassignTo == "" {
			return 0, fmt.Errorf("%w: %d", ErrClusterInUse, members)
		}
		var target string
		if err := tx.QueryRow(`SELECT name FROM clusters WHERE name = ? COLLATE NOCASE`, reassignTo).Scan(&target); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return 0, fmt.Errorf("cluster %q does not exist", reassignTo)
			}
			return 0, err
		}
		if target == current {
			return 0, errors.New("can't reassign a cluster's nodes to itself")
		}
		res, err := tx.Exec(`UPDATE nodes SET cluster=? WHERE cluster = ? COLLATE NOCASE`, target, current)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		moved = int(n)
	}

	// Soft-deleted nodes still inside their undo window would be restored into a
	// cluster that no longer exists; move them with the rest (or to the default)
	// so an undo can never resurrect a dangling membership.
	fallback := reassignTo
	if fallback == "" {
		fallback = DefaultCluster
	}
	if _, err := tx.Exec(`UPDATE nodes SET cluster=? WHERE deleted_at<>0 AND cluster = ? COLLATE NOCASE`,
		fallback, current); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM policy_overrides WHERE scope=?`, ClusterScope(current)); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM clusters WHERE name=?`, current); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	// Hold the invariant "every cluster a node references is registered". The
	// fallback above can itself be the cluster being deleted (removing "default"
	// while a soft-deleted node still points at it), which would leave that node
	// referencing a cluster absent from the picker and the settings list.
	// Re-seeding is idempotent and adopts whatever is genuinely still in use.
	s.seedClusters()
	return moved, nil
}

// SetNodeCluster moves ONE node into a cluster, touching nothing else on the
// node row. This is deliberately not a general node update: reassigning a server
// from the cluster screen must never go anywhere near its transport, address or
// sealed credential blob.
//
// Soft-deleted nodes are excluded — a node inside its undo window is on its way
// out and must not be pulled into a cluster behind the user's back.
func (s *Store) SetNodeCluster(nodeID, cluster string) error {
	res, err := s.db.Exec(`UPDATE nodes SET cluster=? WHERE id=? AND deleted_at=0`, cluster, nodeID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// NodeCluster returns a node's cluster name, or "" when the node is unknown.
// Used to resolve the cluster tier of the policy chain.
func (s *Store) NodeCluster(nodeID string) string {
	var c string
	if err := s.db.QueryRow(`SELECT cluster FROM nodes WHERE id=?`, nodeID).Scan(&c); err != nil {
		return ""
	}
	return c
}

// NodeClusterScope returns the policy scope key for a node's cluster, or "" when
// the node is unknown or unclustered — callers skip the tier on "".
func (s *Store) NodeClusterScope(nodeID string) string {
	c := s.NodeCluster(nodeID)
	if c == "" {
		return ""
	}
	return ClusterScope(c)
}

// seedClusters backfills the registry from the cluster names already typed into
// node rows, so an upgrade lands with exactly the clusters the fleet was already
// using — not an empty list that makes every node look unassigned.
//
// It is idempotent (INSERT OR IGNORE) and never rejects a legacy name: whatever
// was stored is adopted as-is, even if ValidateClusterName would now refuse it.
// Validation applies to what a user creates or renames from here on, not
// retroactively to data that already exists.
func (s *Store) seedClusters() {
	s.db.Exec(`INSERT OR IGNORE INTO clusters(name, description, color, created_at)
		SELECT DISTINCT cluster, '', '', ? FROM nodes WHERE cluster <> ''`, now())
	// A brand-new install has no nodes yet, so seeding finds nothing. Guarantee
	// the default exists, because the nodes.cluster column defaults to it.
	var n int
	if s.db.QueryRow(`SELECT COUNT(*) FROM clusters`).Scan(&n) == nil && n == 0 {
		s.db.Exec(`INSERT OR IGNORE INTO clusters(name, description, color, created_at) VALUES(?, '', '', ?)`,
			DefaultCluster, now())
	}
}
