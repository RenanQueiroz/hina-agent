package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("not found")

// CreateUser inserts a new user. CreatedAt/UpdatedAt are set if zero.
func (s *Store) CreateUser(ctx context.Context, u User) error {
	if u.CreatedAt.IsZero() {
		u.CreatedAt = nowUTC()
	}
	if u.UpdatedAt.IsZero() {
		u.UpdatedAt = u.CreatedAt
	}
	if u.Status == "" {
		u.Status = "active"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users(id, username, role, password_hash, status, must_change_password, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		u.ID, u.Username, u.Role, u.PasswordHash, u.Status, boolToInt(u.MustChangePassword),
		formatTime(u.CreatedAt), formatTime(u.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	return nil
}

// GetUserByUsername looks up a user by username.
func (s *Store) GetUserByUsername(ctx context.Context, username string) (User, error) {
	return s.scanUser(s.db.QueryRowContext(ctx, userSelect+` WHERE username=?`, username))
}

// GetUserByID looks up a user by id.
func (s *Store) GetUserByID(ctx context.Context, id string) (User, error) {
	return s.scanUser(s.db.QueryRowContext(ctx, userSelect+` WHERE id=?`, id))
}

// ListUsers returns all users ordered by creation time.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, userSelect+` ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var (
			u                  User
			mustChange         int
			createdAt, updated string
		)
		if err := rows.Scan(&u.ID, &u.Username, &u.Role, &u.PasswordHash, &u.Status, &mustChange, &createdAt, &updated); err != nil {
			return nil, err
		}
		u.MustChangePassword = mustChange != 0
		u.CreatedAt, _ = parseTime(createdAt)
		u.UpdatedAt, _ = parseTime(updated)
		out = append(out, u)
	}
	return out, rows.Err()
}

// CountByRole returns the number of users with the given role.
func (s *Store) CountByRole(ctx context.Context, role string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM users WHERE role=?`, role).Scan(&n)
	return n, err
}

// CountAdminsRequiringPasswordChange returns how many admins still have the
// bootstrap must_change_password flag set. Used to gate LAN binding.
func (s *Store) CountAdminsRequiringPasswordChange(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM users WHERE role='admin' AND must_change_password=1`,
	).Scan(&n)
	return n, err
}

// UpdateUserPassword sets a new password hash and clears must_change_password.
func (s *Store) UpdateUserPassword(ctx context.Context, id, passwordHash string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET password_hash=?, must_change_password=0, updated_at=? WHERE id=?`,
		passwordHash, formatTime(nowUTC()), id,
	)
	return err
}

const userSelect = `SELECT id, username, role, password_hash, status, must_change_password, created_at, updated_at FROM users`

func (s *Store) scanUser(row *sql.Row) (User, error) {
	var (
		u                  User
		mustChange         int
		createdAt, updated string
	)
	err := row.Scan(&u.ID, &u.Username, &u.Role, &u.PasswordHash, &u.Status, &mustChange, &createdAt, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	u.MustChangePassword = mustChange != 0
	u.CreatedAt, _ = parseTime(createdAt)
	u.UpdatedAt, _ = parseTime(updated)
	return u, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
