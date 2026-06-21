package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// SandboxState is one per-user sandbox configuration blob. kind partitions the
// blobs (kind='environment' holds the Sandbox Environment policy as JSON); the
// runtime upserts on (user_id, kind).
type SandboxState struct {
	ID        string
	UserID    string
	Kind      string
	Data      string // JSON
	UpdatedAt time.Time
}

// GetSandboxState returns a user's sandbox blob of the given kind, or ErrNotFound.
func (s *Store) GetSandboxState(ctx context.Context, userID, kind string) (SandboxState, error) {
	var (
		st      SandboxState
		updated string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, kind, data, updated_at FROM sandbox_state WHERE user_id=? AND kind=?`,
		userID, kind,
	).Scan(&st.ID, &st.UserID, &st.Kind, &st.Data, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return SandboxState{}, ErrNotFound
	}
	if err != nil {
		return SandboxState{}, err
	}
	st.UpdatedAt, _ = parseTime(updated)
	return st, nil
}

// UpsertSandboxState writes a user's sandbox blob, replacing any existing row of
// the same kind. id is used only on first insert (the unique (user_id, kind)
// index makes the upsert idempotent).
func (s *Store) UpsertSandboxState(ctx context.Context, st SandboxState) error {
	if st.Data == "" {
		st.Data = "{}"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sandbox_state(id, user_id, kind, data, updated_at)
		 VALUES(?,?,?,?,?)
		 ON CONFLICT(user_id, kind) DO UPDATE SET data=excluded.data, updated_at=excluded.updated_at`,
		st.ID, st.UserID, st.Kind, st.Data, formatTime(nowUTC()),
	)
	return err
}

// SandboxRun is one audit-log entry for a sandbox tool invocation. It records
// metadata only — never secret values, and `Command` is a redacted argv summary.
type SandboxRun struct {
	ID             string
	UserID         string
	ConversationID string
	Tool           string
	SandboxID      string
	Command        string // redacted summary, never the raw secret-bearing argv
	Decision       string // approved | denied | auto
	ExitCode       int
	DurationMs     int64
	Error          string
	StdoutPath     string
	StderrPath     string
	CreatedAt      time.Time
}

// InsertSandboxRun appends an audit-log row for a sandbox tool invocation.
func (s *Store) InsertSandboxRun(ctx context.Context, r SandboxRun) error {
	if r.CreatedAt.IsZero() {
		r.CreatedAt = nowUTC()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sandbox_runs(id, user_id, conversation_id, tool, sandbox_id, command,
		 decision, exit_code, duration_ms, error, stdout_path, stderr_path, created_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.UserID, nullIfEmpty(r.ConversationID), r.Tool, r.SandboxID, r.Command,
		r.Decision, r.ExitCode, r.DurationMs, r.Error, r.StdoutPath, r.StderrPath,
		formatTime(r.CreatedAt),
	)
	return err
}

// UpdateSandboxRun finalizes a previously-inserted (pending) audit row with the
// run's outcome. Returns ErrNotFound if the id no longer exists.
func (s *Store) UpdateSandboxRun(ctx context.Context, r SandboxRun) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE sandbox_runs SET sandbox_id=?, exit_code=?, duration_ms=?, error=?, stdout_path=?, stderr_path=? WHERE id=?`,
		r.SandboxID, r.ExitCode, r.DurationMs, r.Error, r.StdoutPath, r.StderrPath, r.ID,
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

// ListSandboxRuns returns the most recent sandbox-run audit rows, newest first,
// capped at limit. userID=="" returns runs across all users (admin visibility);
// a non-empty userID scopes to that user.
func (s *Store) ListSandboxRuns(ctx context.Context, userID string, limit int) ([]SandboxRun, error) {
	if limit <= 0 {
		limit = 100
	}
	var (
		rows *sql.Rows
		err  error
	)
	if userID == "" {
		rows, err = s.db.QueryContext(ctx, sandboxRunSelect+` ORDER BY created_at DESC LIMIT ?`, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, sandboxRunSelect+` WHERE user_id=? ORDER BY created_at DESC LIMIT ?`, userID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SandboxRun
	for rows.Next() {
		var (
			r         SandboxRun
			convID    sql.NullString
			createdAt string
		)
		if err := rows.Scan(&r.ID, &r.UserID, &convID, &r.Tool, &r.SandboxID, &r.Command,
			&r.Decision, &r.ExitCode, &r.DurationMs, &r.Error, &r.StdoutPath, &r.StderrPath, &createdAt); err != nil {
			return nil, err
		}
		r.ConversationID = convID.String
		r.CreatedAt, _ = parseTime(createdAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountSandboxRunsByUser returns how many audit rows a user has accumulated.
func (s *Store) CountSandboxRunsByUser(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM sandbox_runs WHERE user_id=?`, userID).Scan(&n)
	return n, err
}

const sandboxRunSelect = `SELECT id, user_id, conversation_id, tool, sandbox_id, command,
	decision, exit_code, duration_ms, error, stdout_path, stderr_path, created_at FROM sandbox_runs`

// nullIfEmpty maps "" to a SQL NULL so the optional conversation_id column stays
// NULL rather than an empty string (cleaner for the nullable FK semantics).
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
