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

	e, _ := New(SourceServer, TypeSessionCreated, conv.ID, u.ID, "", map[string]string{"title": "hi"})
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

	e2, _ := New(SourceServer, TypeUserTextSubmitted, conv.ID, u.ID, "trn_x", nil)
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

// TestPublishPoisonsOverflowedSubscriber proves a persisted event is never
// silently dropped: a subscriber that doesn't drain has its channel closed
// (poisoned) once the buffer overflows, rather than losing events. Every
// published event stays replayable from the store, so the SSE handler recovers
// on the close + reconnect.
func TestPublishPoisonsOverflowedSubscriber(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "ev.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
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

	// Publish well past the 64-deep buffer WITHOUT draining, so a send fails and
	// poisons the subscriber.
	const n = 80
	for i := 0; i < n; i++ {
		e, _ := New(SourceServer, TypeTurnStarted, conv.ID, u.ID, "", nil)
		if _, err := bus.Publish(ctx, e); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}

	// The channel must now be closed; draining it must terminate (not hang).
	done := make(chan int, 1)
	go func() {
		c := 0
		for range ch {
			c++
		}
		done <- c
	}()
	select {
	case got := <-done:
		if got == 0 {
			t.Fatal("no buffered events were delivered before close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber channel never closed — overflow did not poison it")
	}

	// Nothing was lost: all n events remain replayable from the durable log.
	replay, err := bus.Replay(ctx, conv.ID, 0)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(replay) != n {
		t.Fatalf("replay = %d events, want %d (persisted events must not be lost)", len(replay), n)
	}
}
