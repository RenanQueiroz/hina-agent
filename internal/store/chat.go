package store

import (
	"context"
	"database/sql"
	"errors"
)

// CreateConversation inserts a new conversation.
func (s *Store) CreateConversation(ctx context.Context, c Conversation) error {
	if c.CreatedAt.IsZero() {
		c.CreatedAt = nowUTC()
	}
	if c.UpdatedAt.IsZero() {
		c.UpdatedAt = c.CreatedAt
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO conversations(id, owner_user_id, title, created_at, updated_at) VALUES(?,?,?,?,?)`,
		c.ID, c.OwnerUserID, c.Title, formatTime(c.CreatedAt), formatTime(c.UpdatedAt),
	)
	return err
}

// GetConversation returns a conversation by id.
func (s *Store) GetConversation(ctx context.Context, id string) (Conversation, error) {
	var (
		c                  Conversation
		createdAt, updated string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, owner_user_id, title, created_at, updated_at FROM conversations WHERE id=?`, id,
	).Scan(&c.ID, &c.OwnerUserID, &c.Title, &createdAt, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Conversation{}, ErrNotFound
	}
	if err != nil {
		return Conversation{}, err
	}
	c.CreatedAt, _ = parseTime(createdAt)
	c.UpdatedAt, _ = parseTime(updated)
	return c, nil
}

// ListConversationsByOwner returns a user's conversations, newest first.
func (s *Store) ListConversationsByOwner(ctx context.Context, ownerID string) ([]Conversation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, owner_user_id, title, created_at, updated_at FROM conversations
		 WHERE owner_user_id=? ORDER BY updated_at DESC`, ownerID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Conversation
	for rows.Next() {
		var (
			c                  Conversation
			createdAt, updated string
		)
		if err := rows.Scan(&c.ID, &c.OwnerUserID, &c.Title, &createdAt, &updated); err != nil {
			return nil, err
		}
		c.CreatedAt, _ = parseTime(createdAt)
		c.UpdatedAt, _ = parseTime(updated)
		out = append(out, c)
	}
	return out, rows.Err()
}

// AppendTurn inserts a turn and bumps the conversation's updated_at.
func (s *Store) AppendTurn(ctx context.Context, t Turn) error {
	if t.CreatedAt.IsZero() {
		t.CreatedAt = nowUTC()
	}
	if t.Mode == "" {
		t.Mode = "text"
	}
	if t.Metadata == "" {
		t.Metadata = "{}"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO turns(id, conversation_id, role, mode, canonical_text, metadata, created_at)
		 VALUES(?,?,?,?,?,?,?)`,
		t.ID, t.ConversationID, t.Role, t.Mode, t.CanonicalText, t.Metadata, formatTime(t.CreatedAt),
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE conversations SET updated_at=? WHERE id=?`, formatTime(t.CreatedAt), t.ConversationID,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// ListTurns returns a conversation's turns in chronological order.
func (s *Store) ListTurns(ctx context.Context, conversationID string) ([]Turn, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, conversation_id, role, mode, canonical_text, metadata, created_at
		 FROM turns WHERE conversation_id=? ORDER BY created_at ASC`, conversationID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Turn
	for rows.Next() {
		var (
			t         Turn
			createdAt string
		)
		if err := rows.Scan(&t.ID, &t.ConversationID, &t.Role, &t.Mode, &t.CanonicalText, &t.Metadata, &createdAt); err != nil {
			return nil, err
		}
		t.CreatedAt, _ = parseTime(createdAt)
		out = append(out, t)
	}
	return out, rows.Err()
}
