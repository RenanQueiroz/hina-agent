package store

import (
	"context"
	"database/sql"
)

// AppendEvent persists an event, assigning a per-conversation monotonic seq
// (max+1) inside a transaction. The event's Seq and ServerTS are filled in on
// the passed pointer. Callers should serialize appends per conversation (the
// event bus does) so the seq assignment does not race.
func (s *Store) AppendEvent(ctx context.Context, e *Event) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertEventTx(ctx, tx, e); err != nil {
		return err
	}
	return tx.Commit()
}

// AppendTurnWithEvents persists a turn and its durable event(s) in a single
// transaction (bumping the conversation's updated_at), assigning each event a
// per-conversation monotonic seq. Either everything commits or nothing does, so
// a turn can never end up persisted without the event that announces it — the
// event log is what the timeline replays from, so a turn with no event would
// silently vanish on reload while still feeding model context. Callers must
// serialize appends per conversation (the bus does, under its mutex) so the seq
// assignment cannot race. Each event's Seq/ServerTS are filled in on the passed
// pointers.
func (s *Store) AppendTurnWithEvents(ctx context.Context, t Turn, evs []*Event) error {
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

	for _, e := range evs {
		if err := insertEventTx(ctx, tx, e); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpdateTurnMetadataWithEvents replaces an existing turn's metadata AND appends its
// durable event(s) in a SINGLE transaction, so the metadata change (e.g. marking a
// voice turn interrupted after a barge-in) and the event that announces it (e.g.
// ConversationTruncated) can never diverge — either both commit or neither does.
// Returns ErrNotFound if the turn doesn't exist (so a truncation event is never
// published for a missing turn). Each event's Seq/ServerTS are filled in. Callers
// must serialize per conversation (the bus does, under its mutex).
func (s *Store) UpdateTurnMetadataWithEvents(ctx context.Context, turnID, metadata string, evs []*Event) error {
	if metadata == "" {
		metadata = "{}"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `UPDATE turns SET metadata=? WHERE id=?`, metadata, turnID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound // no such turn — don't publish a truncation event for it
	}
	for _, e := range evs {
		if err := insertEventTx(ctx, tx, e); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CreateConversationWithEvent inserts a conversation and its first durable event
// (SessionCreated) in a single transaction, so the API can never return a
// created conversation whose creation event is missing from the replayed log
// (which the timeline and reconnect contract depend on). The event's
// Seq/ServerTS are filled in on the passed pointer.
func (s *Store) CreateConversationWithEvent(ctx context.Context, c Conversation, e *Event) error {
	if c.CreatedAt.IsZero() {
		c.CreatedAt = nowUTC()
	}
	if c.UpdatedAt.IsZero() {
		c.UpdatedAt = c.CreatedAt
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO conversations(id, owner_user_id, title, created_at, updated_at) VALUES(?,?,?,?,?)`,
		c.ID, c.OwnerUserID, c.Title, formatTime(c.CreatedAt), formatTime(c.UpdatedAt),
	); err != nil {
		return err
	}
	if err := insertEventTx(ctx, tx, e); err != nil {
		return err
	}
	return tx.Commit()
}

// insertEventTx assigns the next per-conversation monotonic seq and inserts the
// event within tx, filling e.Seq/e.ServerTS. Callers must serialize per
// conversation (the bus does, under its mutex) so the seq assignment can't race.
func insertEventTx(ctx context.Context, tx *sql.Tx, e *Event) error {
	if e.ServerTS.IsZero() {
		e.ServerTS = nowUTC()
	}
	if e.Payload == "" {
		e.Payload = "{}"
	}
	var next int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq),0)+1 FROM events WHERE conversation_id IS ?`,
		nullStr(e.ConversationID),
	).Scan(&next); err != nil {
		return err
	}
	e.Seq = next
	_, err := tx.ExecContext(ctx,
		`INSERT INTO events(event_id, conversation_id, user_id, turn_id, seq, source, type, payload, server_ts)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		e.EventID, nullStr(e.ConversationID), nullStr(e.UserID), nullStr(e.TurnID),
		e.Seq, e.Source, e.Type, e.Payload, formatTime(e.ServerTS),
	)
	return err
}

// ListEventsSince returns events for a conversation with seq > sinceSeq, in
// order. Used for reconnect/replay.
func (s *Store) ListEventsSince(ctx context.Context, conversationID string, sinceSeq int64) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT event_id, conversation_id, user_id, turn_id, seq, source, type, payload, server_ts
		 FROM events WHERE conversation_id IS ? AND seq > ? ORDER BY seq ASC`,
		nullStr(conversationID), sinceSeq,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var (
			e                          Event
			conv, user, turn, serverTS sql.NullString
		)
		if err := rows.Scan(&e.EventID, &conv, &user, &turn, &e.Seq, &e.Source, &e.Type, &e.Payload, &serverTS); err != nil {
			return nil, err
		}
		e.ConversationID = conv.String
		e.UserID = user.String
		e.TurnID = turn.String
		e.ServerTS, _ = parseTime(serverTS.String)
		out = append(out, e)
	}
	return out, rows.Err()
}

// nullStr maps "" to SQL NULL so nullable FKs stay null rather than empty text.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
