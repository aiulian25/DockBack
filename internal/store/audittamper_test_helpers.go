//go:build testhooks

// Built only under the `testhooks` tag, so the shipped binary contains no method
// that can rewrite or truncate the audit trail.
//
// It cannot simply be a _test.go file: internal/api/auditbeacon_test.go is in
// another package and needs these, and Go does not export a package's test
// helpers across a package boundary. A tag is the next-narrowest thing — the
// production build cannot see them at all, and `go test -tags testhooks` can.
//
// The claim this protects is "the audit trail is append-only by construction".
// With these compiled in, that claim was false for the binary operators run.

package store

import "fmt"

// Test-only helpers that simulate an attacker with write access to the database
// (F200). They exist so the beacon's whole premise can be PROVEN rather than
// asserted: that a rewritten trail passes the built-in chain walk and is still
// caught by a checkpoint kept outside the machine.
//
// Deliberately not part of the Store's real surface — nothing in the app writes
// to the audit table except Audit(), which is append-only by construction.

// TestOnlyRewriteAudit overwrites one column of one audit row.
func (s *Store) TestOnlyRewriteAudit(id int64, column, value string) error {
	switch column {
	case "detail", "actor", "action", "target", "chain":
	default:
		return fmt.Errorf("refusing to rewrite unknown audit column %q", column)
	}
	_, err := s.db.Exec(`UPDATE audit SET `+column+`=? WHERE id=?`, value, id)
	return err
}

// TestOnlyDeleteAuditFrom removes every audit row from id onwards (tail
// truncation — the one tamper a self-contained chain cannot see).
func (s *Store) TestOnlyDeleteAuditFrom(id int64) error {
	_, err := s.db.Exec(`DELETE FROM audit WHERE id >= ?`, id)
	return err
}
