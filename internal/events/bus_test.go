package events

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/id"
	"github.com/RenanQueiroz/hina-agent/internal/store"
)

func TestBusPublishSubscribeReplay(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "ev.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// FK constraints require a user + conversation.
	u := store.User{ID: id.New("usr"), Username: "u", Role: "user", PasswordHash: "x"}
	if err := st.CreateUser(ctx, u); err != nil {
		t.Fatalf("user: %v", err)
	}
	conv := store.Conversation{ID: id.New("cnv"), OwnerUserID: u.ID}
	if err := st.CreateConversation(ctx, conv); err != nil {
		t.Fatalf("conversation: %v", err)
	}

	bus := NewBus(st)
	ch, cancel := bus.Subscribe(conv.ID)
	defer cancel()

	e, _ := New(SourceServer, TypeSessionCreated, conv.ID, u.ID, map[string]string{"title": "hi"})
	pub, err := bus.Publish(ctx, e)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if pub.Seq != 1 {
		t.Fatalf("seq = %d, want 1", pub.Seq)
	}

	select {
	case got := <-ch:
		if got.EventID != pub.EventID || got.Seq != 1 {
			t.Fatalf("subscriber got %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber timed out")
	}

	e2, _ := New(SourceServer, TypeUserTextSubmitted, conv.ID, u.ID, nil)
	if _, err := bus.Publish(ctx, e2); err != nil {
		t.Fatalf("publish 2: %v", err)
	}
	<-ch // drain the live delivery

	replay, err := bus.Replay(ctx, conv.ID, 1) // everything after seq 1
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(replay) != 1 || replay[0].Seq != 2 {
		t.Fatalf("replay = %+v, want one event with seq 2", replay)
	}
}
