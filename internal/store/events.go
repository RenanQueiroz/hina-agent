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
	if e.ServerTS.IsZero() {
		e.ServerTS = nowUTC()
	}
	if e.Payload == "" {
		e.Payload = "{}"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var next int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq),0)+1 FROM events WHERE conversation_id IS ?`,
		nullStr(e.ConversationID),
	).Scan(&next); err != nil {
		return err
	}
	e.Seq = next

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events(event_id, conversation_id, user_id, turn_id, seq, source, type, payload, server_ts)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		e.EventID, nullStr(e.ConversationID), nullStr(e.UserID), nullStr(e.TurnID),
		e.Seq, e.Source, e.Type, e.Payload, formatTime(e.ServerTS),
	); err != nil {
		return err
	}
	return tx.Commit()
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
