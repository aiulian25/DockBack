package store

import (
	"database/sql"
	"errors"
)

// Passkey is one stored WebAuthn credential. CredentialJSON is go-webauthn's
// credential record (public key, sign counter, flags, AAGUID), never a secret.
type Passkey struct {
	ID             string
	UserID         int64
	RPID           string
	CredentialID   string // base64url, the lookup key during sign-in
	CredentialJSON string
	Name           string
	Synced         bool
	CreatedAt      int64
	LastUsedAt     int64
}

const passkeyColumns = `id, user_id, rp_id, credential_id, credential_json, name, synced, created_at, last_used_at`

func scanPasskey(row interface{ Scan(...any) error }) (*Passkey, error) {
	p := &Passkey{}
	err := row.Scan(&p.ID, &p.UserID, &p.RPID, &p.CredentialID, &p.CredentialJSON, &p.Name, &p.Synced, &p.CreatedAt, &p.LastUsedAt)
	return p, err
}

// CreatePasskey stores a new passkey. A credential id already stored is refused
// by the UNIQUE constraint.
func (s *Store) CreatePasskey(p *Passkey) error {
	_, err := s.db.Exec(`INSERT INTO passkeys(`+passkeyColumns+`) VALUES(?,?,?,?,?,?,?,?,0)`,
		p.ID, p.UserID, p.RPID, p.CredentialID, p.CredentialJSON, p.Name, p.Synced, p.CreatedAt)
	return err
}

// ListPasskeys returns a user's passkeys, oldest first.
func (s *Store) ListPasskeys(userID int64) ([]*Passkey, error) {
	rows, err := s.db.Query(`SELECT `+passkeyColumns+` FROM passkeys WHERE user_id=? ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Passkey
	for rows.Next() {
		p, err := scanPasskey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PasskeyByCredentialID finds the passkey a sign-in answer names, for one host.
func (s *Store) PasskeyByCredentialID(rpID, credentialID string) (*Passkey, error) {
	p, err := scanPasskey(s.db.QueryRow(`SELECT `+passkeyColumns+` FROM passkeys WHERE rp_id=? AND credential_id=?`, rpID, credentialID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// CountPasskeysForRP says whether the sign-in page should offer a passkey here.
func (s *Store) CountPasskeysForRP(rpID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM passkeys WHERE rp_id=?`, rpID).Scan(&n)
	return n, err
}

// RecordPasskeyUse stores the credential record a sign-in updated (its sign
// counter and flags) and when it was used.
func (s *Store) RecordPasskeyUse(id, credentialJSON string, at int64) error {
	_, err := s.db.Exec(`UPDATE passkeys SET credential_json=?, last_used_at=? WHERE id=?`, credentialJSON, at, id)
	return err
}

// RenamePasskey renames one of a user's own passkeys.
func (s *Store) RenamePasskey(userID int64, id, name string) error {
	return expectOneRow(s.db.Exec(`UPDATE passkeys SET name=? WHERE id=? AND user_id=?`, name, id, userID))
}

// DeletePasskey removes one of a user's own passkeys.
func (s *Store) DeletePasskey(userID int64, id string) error {
	return expectOneRow(s.db.Exec(`DELETE FROM passkeys WHERE id=? AND user_id=?`, id, userID))
}

// expectOneRow turns a write that matched nothing into ErrNotFound, so asking
// for someone else's passkey reads the same as asking for one that doesn't exist.
func expectOneRow(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
