package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/RenanQueiroz/hina-agent/internal/id"
)

func TestMigrateAndRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	n, err := st.Migrate(ctx)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if n < 1 {
		t.Fatalf("expected >=1 migration applied, got %d", n)
	}
	if n2, err := st.Migrate(ctx); err != nil || n2 != 0 {
		t.Fatalf("re-migrate should apply 0 (got %d, err %v)", n2, err)
	}

	u := User{ID: id.New("usr"), Username: "admin", Role: "admin", PasswordHash: "x"}
	if err := st.CreateUser(ctx, u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	got, err := st.GetUserByUsername(ctx, "admin")
	if err != nil || got.ID != u.ID || !got.IsAdmin() {
		t.Fatalf("get user: %+v err=%v", got, err)
	}
	if c, _ := st.CountByRole(ctx, "admin"); c != 1 {
		t.Fatalf("admin count = %d, want 1", c)
	}

	conv := Conversation{ID: id.New("cnv"), OwnerUserID: u.ID, Title: "t"}
	if err := st.CreateConversation(ctx, conv); err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	if err := st.AppendTurn(ctx, Turn{ID: id.New("trn"), ConversationID: conv.ID, Role: "user", CanonicalText: "hi"}); err != nil {
		t.Fatalf("append turn: %v", err)
	}
	if turns, _ := st.ListTurns(ctx, conv.ID); len(turns) != 1 || turns[0].CanonicalText != "hi" {
		t.Fatalf("turns: %+v", turns)
	}

	// Event seq must be monotonic per conversation.
	for i := 0; i < 3; i++ {
		e := &Event{EventID: id.New("evt"), ConversationID: conv.ID, UserID: u.ID, Source: "server", Type: "TestEvent"}
		if err := st.AppendEvent(ctx, e); err != nil {
			t.Fatalf("append event %d: %v", i, err)
		}
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d seq = %d, want %d", i, e.Seq, i+1)
		}
	}
	if evs, _ := st.ListEventsSince(ctx, conv.ID, 1); len(evs) != 2 {
		t.Fatalf("events since seq 1 = %d, want 2", len(evs))
	}
}
