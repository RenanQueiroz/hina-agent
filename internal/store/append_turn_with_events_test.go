package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/RenanQueiroz/hina-agent/internal/id"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func seedConversation(t *testing.T, st *Store) (string, string) {
	t.Helper()
	ctx := context.Background()
	uid := id.New("usr")
	if err := st.CreateUser(ctx, User{ID: uid, Username: uid, Role: "user", PasswordHash: "x"}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	cid := id.New("cnv")
	if err := st.CreateConversation(ctx, Conversation{ID: cid, OwnerUserID: uid}); err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	return uid, cid
}

// TestAppendTurnWithEventsCommitsTogether proves the turn and its events land
// together with monotonic seqs.
func TestAppendTurnWithEventsCommitsTogether(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	uid, cid := seedConversation(t, st)

	turn := Turn{ID: id.New("trn"), ConversationID: cid, Role: "user", CanonicalText: "hi"}
	evs := []*Event{
		{EventID: id.New("evt"), ConversationID: cid, UserID: uid, TurnID: turn.ID, Source: "client", Type: "UserTextSubmitted"},
		{EventID: id.New("evt"), ConversationID: cid, UserID: uid, TurnID: turn.ID, Source: "server", Type: "TurnCommitted"},
	}
	if err := st.AppendTurnWithEvents(ctx, turn, evs); err != nil {
		t.Fatalf("append turn with events: %v", err)
	}
	if evs[0].Seq != 1 || evs[1].Seq != 2 {
		t.Fatalf("seqs = %d,%d want 1,2", evs[0].Seq, evs[1].Seq)
	}
	if turns, _ := st.ListTurns(ctx, cid); len(turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(turns))
	}
	if got, _ := st.ListEventsSince(ctx, cid, 0); len(got) != 2 {
		t.Fatalf("events = %d, want 2", len(got))
	}
}

// TestAppendTurnWithEventsRollsBack proves atomicity: if an event insert fails
// (here a duplicate event_id PK), the turn is NOT left orphaned in the store.
func TestAppendTurnWithEventsRollsBack(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	uid, cid := seedConversation(t, st)

	// Pre-existing event whose id we will collide with.
	dup := &Event{EventID: id.New("evt"), ConversationID: cid, UserID: uid, Source: "server", Type: "Seed"}
	if err := st.AppendEvent(ctx, dup); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	turn := Turn{ID: id.New("trn"), ConversationID: cid, Role: "user", CanonicalText: "doomed"}
	evs := []*Event{
		{EventID: dup.EventID, ConversationID: cid, UserID: uid, TurnID: turn.ID, Source: "client", Type: "UserTextSubmitted"},
	}
	if err := st.AppendTurnWithEvents(ctx, turn, evs); err == nil {
		t.Fatal("expected duplicate event_id to fail the transaction")
	}
	if turns, _ := st.ListTurns(ctx, cid); len(turns) != 0 {
		t.Fatalf("turn must not persist when the event insert fails; got %d turns", len(turns))
	}
}

// TestCreateConversationWithEvent proves the conversation and its SessionCreated
// event commit together, and that a failing event insert rolls back the
// conversation (never an eventless conversation).
func TestCreateConversationWithEvent(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	uid, cid := seedConversation(t, st)

	// Happy path: a fresh conversation + its event commit together.
	c2 := Conversation{ID: id.New("cnv"), OwnerUserID: uid}
	evt := &Event{EventID: id.New("evt"), ConversationID: c2.ID, UserID: uid, Source: "server", Type: "SessionCreated"}
	if err := st.CreateConversationWithEvent(ctx, c2, evt); err != nil {
		t.Fatalf("create conversation with event: %v", err)
	}
	if _, err := st.GetConversation(ctx, c2.ID); err != nil {
		t.Fatalf("conversation not persisted: %v", err)
	}
	if evs, _ := st.ListEventsSince(ctx, c2.ID, 0); len(evs) != 1 || evs[0].Seq != 1 {
		t.Fatalf("events = %+v, want one with seq 1", evs)
	}

	// Rollback: a duplicate event_id fails the insert, so the conversation must
	// not be left behind.
	dup := &Event{EventID: id.New("evt"), ConversationID: cid, UserID: uid, Source: "server", Type: "Seed"}
	if err := st.AppendEvent(ctx, dup); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	c3 := Conversation{ID: id.New("cnv"), OwnerUserID: uid}
	bad := &Event{EventID: dup.EventID, ConversationID: c3.ID, UserID: uid, Source: "server", Type: "SessionCreated"}
	if err := st.CreateConversationWithEvent(ctx, c3, bad); err == nil {
		t.Fatal("expected duplicate event_id to fail the transaction")
	}
	if _, err := st.GetConversation(ctx, c3.ID); err == nil {
		t.Fatal("conversation must not persist when its event insert fails")
	}
}
