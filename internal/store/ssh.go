package store

import (
	"database/sql"
	"errors"
	"time"
)

// SSHAccess is one account's desired SSH state as the brain holds it. The brain
// is the durable side of this pair and the host is reconstructible, so this is
// what a reconcile re-applies after a host rebuild (CLAUDE.md # Brain commits
// first, host is reconstructible).
type SSHAccess struct {
	UserID          string
	Enabled         bool
	RequirePassword bool
	UpdatedAt       time.Time
}

// SSHKey is one public key belonging to an account. Several per account is the
// normal case — a laptop and a desktop — which is why keys are their own table
// rather than a column on ssh_access.
//
// PublicKey is the authorized_keys line as the host will receive it. Fingerprint
// is the SHA-256 fingerprint, stored so the same key cannot be added twice and so
// the UI can show the user something short and comparable.
type SSHKey struct {
	ID          string
	UserID      string
	Label       string
	PublicKey   string
	Fingerprint string
	AddedAt     time.Time
}

// ErrDuplicateSSHKey is returned when an account already holds the same key. The
// API maps it to 409 rather than silently succeeding, so the user learns the key
// they just pasted was already there instead of wondering which one is live.
var ErrDuplicateSSHKey = errors.New("ssh key already added")

// SSHAccessFor returns the account's SSH state. A user who has never touched the
// setting has no row, which reads as the default: disabled, no second factor.
// That is not an error — the absent row *is* the off state.
func (s *Store) SSHAccessFor(userID string) (SSHAccess, error) {
	var (
		enabled, requirePassword int
		updated                  int64
	)
	err := s.db.QueryRow(
		`SELECT enabled, require_password, updated_at FROM ssh_access WHERE user_id=?`, userID,
	).Scan(&enabled, &requirePassword, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return SSHAccess{UserID: userID}, nil
	}
	if err != nil {
		return SSHAccess{}, err
	}
	return SSHAccess{
		UserID:          userID,
		Enabled:         enabled == 1,
		RequirePassword: requirePassword == 1,
		UpdatedAt:       time.Unix(updated, 0).UTC(),
	}, nil
}

// SetSSHAccess upserts the account's SSH state.
func (s *Store) SetSSHAccess(userID string, enabled, requirePassword bool) error {
	_, err := s.db.Exec(`
		INSERT INTO ssh_access (user_id, enabled, require_password, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			enabled=excluded.enabled,
			require_password=excluded.require_password,
			updated_at=excluded.updated_at`,
		userID, boolToInt(enabled), boolToInt(requirePassword), time.Now().Unix())
	return err
}

// ListSSHKeys returns the account's keys, oldest first.
func (s *Store) ListSSHKeys(userID string) ([]SSHKey, error) {
	rows, err := s.db.Query(
		`SELECT id, user_id, label, public_key, fingerprint, added_at
		 FROM ssh_keys WHERE user_id=? ORDER BY added_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SSHKey
	for rows.Next() {
		var (
			k     SSHKey
			added int64
		)
		if err := rows.Scan(&k.ID, &k.UserID, &k.Label, &k.PublicKey, &k.Fingerprint, &added); err != nil {
			return nil, err
		}
		k.AddedAt = time.Unix(added, 0).UTC()
		out = append(out, k)
	}
	return out, rows.Err()
}

// AddSSHKey stores a key. The caller has already parsed and fingerprinted it —
// the store does not validate key material, the same way it does not validate a
// time zone.
//
// A key the account already holds returns ErrDuplicateSSHKey, enforced by the
// UNIQUE (user_id, fingerprint) index rather than a read-then-write, so two
// concurrent adds cannot both win.
func (s *Store) AddSSHKey(k SSHKey) error {
	_, err := s.db.Exec(
		`INSERT INTO ssh_keys (id, user_id, label, public_key, fingerprint, added_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		k.ID, k.UserID, k.Label, k.PublicKey, k.Fingerprint, time.Now().Unix())
	if err != nil && isUniqueErr(err) {
		return ErrDuplicateSSHKey
	}
	return err
}

// DeleteSSHKey removes one of the account's keys. Scoped by user_id as well as
// id so a crafted request cannot delete another account's key: the ownership
// check is the WHERE clause, not a prior read.
func (s *Store) DeleteSSHKey(userID, keyID string) error {
	res, err := s.db.Exec(`DELETE FROM ssh_keys WHERE id=? AND user_id=?`, keyID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
