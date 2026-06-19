package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// CreateAuthSession inserts a login session (token stored hashed by the caller).
func (s *Store) CreateAuthSession(ctx context.Context, sess AuthSession) error {
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = nowUTC()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO auth_sessions(id, user_id, token_hash, created_at, expires_at) VALUES(?,?,?,?,?)`,
		sess.ID, sess.UserID, sess.TokenHash, formatTime(sess.CreatedAt), formatTime(sess.ExpiresAt),
	)
	return err
}

// GetAuthSessionByTokenHash returns a non-expired session for the token hash.
func (s *Store) GetAuthSessionByTokenHash(ctx context.Context, tokenHash string) (AuthSession, error) {
	var (
		sess               AuthSession
		createdAt, expires string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, token_hash, created_at, expires_at FROM auth_sessions WHERE token_hash=?`,
		tokenHash,
	).Scan(&sess.ID, &sess.UserID, &sess.TokenHash, &createdAt, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return AuthSession{}, ErrNotFound
	}
	if err != nil {
		return AuthSession{}, err
	}
	sess.CreatedAt, _ = parseTime(createdAt)
	sess.ExpiresAt, _ = parseTime(expires)
	if time.Now().UTC().After(sess.ExpiresAt) {
		return AuthSession{}, ErrNotFound
	}
	return sess, nil
}

// DeleteAuthSession removes a session by id.
func (s *Store) DeleteAuthSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE id=?`, id)
	return err
}

// DeleteExpiredAuthSessions purges sessions whose expiry has passed.
func (s *Store) DeleteExpiredAuthSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE expires_at < ?`, formatTime(nowUTC()))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
