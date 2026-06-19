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
