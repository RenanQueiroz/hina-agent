package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// AgentProfile is the per-user, per-provider record of a configured callable agent
// (Phase 8). It is metadata only: the credential material (a browser/subscription
// credential store, or an API key/OAuth token) lives envelope-encrypted in the vault
// — this row never holds a token, URL, or device code. Label is a coarse,
// non-sensitive UI string.
type AgentProfile struct {
	ID        string
	UserID    string
	Provider  string
	AuthType  string
	Status    string
	Label     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// UpsertAgentProfile writes a user's profile for a provider, replacing any existing
// one (a user reconfigures at most one profile per provider). id is used only on
// first insert; the unique (user_id, provider) index makes the upsert idempotent.
func (s *Store) UpsertAgentProfile(ctx context.Context, p AgentProfile) error {
	now := nowUTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO agent_profiles(id, user_id, provider, auth_type, status, label, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?)
		 ON CONFLICT(user_id, provider) DO UPDATE SET
		   auth_type=excluded.auth_type, status=excluded.status, label=excluded.label, updated_at=excluded.updated_at`,
		p.ID, p.UserID, p.Provider, p.AuthType, p.Status, p.Label,
		formatTime(p.CreatedAt), formatTime(now),
	)
	return err
}

// GetAgentProfile returns a user's profile for a provider, or ErrNotFound.
func (s *Store) GetAgentProfile(ctx context.Context, userID, provider string) (AgentProfile, error) {
	return s.scanAgentProfile(s.db.QueryRowContext(ctx,
		agentProfileSelect+` WHERE user_id=? AND provider=?`, userID, provider))
}

// ListAgentProfilesByUser returns a user's configured agent profiles, newest first.
func (s *Store) ListAgentProfilesByUser(ctx context.Context, userID string) ([]AgentProfile, error) {
	return s.queryAgentProfiles(ctx, agentProfileSelect+` WHERE user_id=? ORDER BY created_at DESC`, userID)
}

// ListAllAgentProfiles returns every user's profiles (admin coarse-status view).
func (s *Store) ListAllAgentProfiles(ctx context.Context) ([]AgentProfile, error) {
	return s.queryAgentProfiles(ctx, agentProfileSelect+` ORDER BY user_id, provider`)
}

// DeleteAgentProfile removes a user's profile for a provider (logout). Scoped to
// the owner; returns ErrNotFound when nothing was deleted.
func (s *Store) DeleteAgentProfile(ctx context.Context, userID, provider string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM agent_profiles WHERE user_id=? AND provider=?`, userID, provider)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

const agentProfileSelect = `SELECT id, user_id, provider, auth_type, status, label, created_at, updated_at FROM agent_profiles`

func (s *Store) queryAgentProfiles(ctx context.Context, query string, args ...any) ([]AgentProfile, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentProfile
	for rows.Next() {
		var (
			p                  AgentProfile
			createdAt, updated string
		)
		if err := rows.Scan(&p.ID, &p.UserID, &p.Provider, &p.AuthType, &p.Status, &p.Label, &createdAt, &updated); err != nil {
			return nil, err
		}
		p.CreatedAt, _ = parseTime(createdAt)
		p.UpdatedAt, _ = parseTime(updated)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) scanAgentProfile(row *sql.Row) (AgentProfile, error) {
	var (
		p                  AgentProfile
		createdAt, updated string
	)
	err := row.Scan(&p.ID, &p.UserID, &p.Provider, &p.AuthType, &p.Status, &p.Label, &createdAt, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentProfile{}, ErrNotFound
	}
	if err != nil {
		return AgentProfile{}, err
	}
	p.CreatedAt, _ = parseTime(createdAt)
	p.UpdatedAt, _ = parseTime(updated)
	return p, nil
}
