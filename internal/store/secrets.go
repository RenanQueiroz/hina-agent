package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ErrConflict is returned when an insert violates a uniqueness constraint
// (e.g. a duplicate secret name for a user).
var ErrConflict = errors.New("conflict")

// SecretMeta is the non-sensitive metadata for a vaulted secret. The encrypted
// value never lives in the database — it is stored as an owner-private file by
// internal/vault; this row only records the name/description so the admin UI and
// the user's secret list can show "what exists" without the value.
type SecretMeta struct {
	ID          string
	UserID      string
	Name        string
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// CreateSecretMeta inserts a secret's metadata row. A duplicate (user_id, name)
// returns ErrConflict so the handler can report a clean 409.
func (s *Store) CreateSecretMeta(ctx context.Context, m SecretMeta) error {
	if m.CreatedAt.IsZero() {
		m.CreatedAt = nowUTC()
	}
	if m.UpdatedAt.IsZero() {
		m.UpdatedAt = m.CreatedAt
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO secrets_meta(id, user_id, name, description, created_at, updated_at)
		 VALUES(?,?,?,?,?,?)`,
		m.ID, m.UserID, m.Name, m.Description, formatTime(m.CreatedAt), formatTime(m.UpdatedAt),
	)
	if err != nil && isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

// ListSecretsByUser returns a user's secret metadata, newest first.
func (s *Store) ListSecretsByUser(ctx context.Context, userID string) ([]SecretMeta, error) {
	rows, err := s.db.QueryContext(ctx, secretSelect+` WHERE user_id=? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SecretMeta
	for rows.Next() {
		m, err := scanSecretRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetSecretMeta looks up a secret by id, scoped to its owner so one user can
// never address another user's secret.
func (s *Store) GetSecretMeta(ctx context.Context, userID, id string) (SecretMeta, error) {
	return s.scanSecret(s.db.QueryRowContext(ctx, secretSelect+` WHERE id=? AND user_id=?`, id, userID))
}

// UpdateSecretMeta updates the mutable metadata (description) and bumps updated_at.
func (s *Store) UpdateSecretMeta(ctx context.Context, userID, id, description string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE secrets_meta SET description=?, updated_at=? WHERE id=? AND user_id=?`,
		description, formatTime(nowUTC()), id, userID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteSecretMeta removes a secret's metadata row (the encrypted blob is removed
// by internal/vault). Scoped to the owner.
func (s *Store) DeleteSecretMeta(ctx context.Context, userID, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM secrets_meta WHERE id=? AND user_id=?`, id, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

const secretSelect = `SELECT id, user_id, name, description, created_at, updated_at FROM secrets_meta`

func (s *Store) scanSecret(row *sql.Row) (SecretMeta, error) {
	var (
		m                  SecretMeta
		createdAt, updated string
	)
	err := row.Scan(&m.ID, &m.UserID, &m.Name, &m.Description, &createdAt, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return SecretMeta{}, ErrNotFound
	}
	if err != nil {
		return SecretMeta{}, err
	}
	m.CreatedAt, _ = parseTime(createdAt)
	m.UpdatedAt, _ = parseTime(updated)
	return m, nil
}

func scanSecretRows(rows *sql.Rows) (SecretMeta, error) {
	var (
		m                  SecretMeta
		createdAt, updated string
	)
	if err := rows.Scan(&m.ID, &m.UserID, &m.Name, &m.Description, &createdAt, &updated); err != nil {
		return SecretMeta{}, err
	}
	m.CreatedAt, _ = parseTime(createdAt)
	m.UpdatedAt, _ = parseTime(updated)
	return m, nil
}

// isUniqueViolation reports whether err is a SQLite UNIQUE constraint failure.
// modernc.org/sqlite surfaces these in the error text ("UNIQUE constraint
// failed"); matching on it keeps the store driver-portable without importing the
// driver's error type.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
