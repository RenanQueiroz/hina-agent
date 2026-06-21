package httpapi

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/auth"
	"github.com/RenanQueiroz/hina-agent/internal/config"
	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/id"
	"github.com/RenanQueiroz/hina-agent/internal/llm"
	"github.com/RenanQueiroz/hina-agent/internal/logbuf"
	"github.com/RenanQueiroz/hina-agent/internal/store"
)

func voiceTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	srv := New(config.Default(), st, events.NewBus(st), auth.NewManager(st, false),
		llm.NewMockProvider(), logbuf.New(50), slog.New(slog.NewTextHandler(io.Discard, nil)))
	return srv, st
}

func seedConversation(t *testing.T, st *store.Store) string {
	t.Helper()
	if err := st.CreateUser(context.Background(), store.User{ID: "usr_1", Username: "u", Role: "user", PasswordHash: "x"}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	conv := store.Conversation{ID: id.New("cnv"), OwnerUserID: "usr_1"}
	if err := st.CreateConversation(context.Background(), conv); err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	return conv.ID
}

func TestRunTurnPersistsVoiceTurnsAndReturnsReply(t *testing.T) {
	srv, st := voiceTestServer(t)
	convID := seedConversation(t, st)

	var streamed string
	reply, turnID, err := srv.RunTurn(context.Background(), convID, "usr_1", "hina what time is it",
		func(d string) { streamed += d }, nil)
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if reply == "" || reply != streamed {
		t.Fatalf("reply=%q streamed=%q — must be non-empty and match", reply, streamed)
	}
	if turnID == "" {
		t.Fatal("RunTurn should return the durable assistant turn id")
	}
	// The returned turn id can be durably marked interrupted (barge-in truncation).
	if err := srv.MarkTurnInterrupted(context.Background(), convID, "usr_1", turnID, 1200); err != nil {
		t.Fatalf("MarkTurnInterrupted: %v", err)
	}
	// Both a "voice" user turn and a "voice" assistant turn are durable.
	turns, err := st.ListTurns(context.Background(), convID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2 (user + assistant)", len(turns))
	}
	for _, tn := range turns {
		if tn.Mode != "voice" {
			t.Fatalf("turn %s mode = %q, want voice", tn.Role, tn.Mode)
		}
	}
	if turns[0].Role != "user" || turns[0].CanonicalText != "hina what time is it" {
		t.Fatalf("user turn = %+v", turns[0])
	}
}

func TestMarkTurnInterruptedAtomicAndChecksTurn(t *testing.T) {
	srv, st := voiceTestServer(t)
	convID := seedConversation(t, st)
	if _, _, err := srv.RunTurn(context.Background(), convID, "usr_1", "hi", nil, nil); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	// Find the assistant turn.
	turns, _ := st.ListTurns(context.Background(), convID)
	var asTurnID string
	for _, tn := range turns {
		if tn.Role == "assistant" {
			asTurnID = tn.ID
		}
	}
	// Marking a MISSING turn must fail (no event published for a non-existent turn).
	if err := srv.MarkTurnInterrupted(context.Background(), convID, "usr_1", "trn_missing", 500); err == nil {
		t.Fatal("MarkTurnInterrupted on a missing turn should error")
	}
	// Marking the real turn persists BOTH the metadata and a durable event.
	if err := srv.MarkTurnInterrupted(context.Background(), convID, "usr_1", asTurnID, 900); err != nil {
		t.Fatalf("MarkTurnInterrupted: %v", err)
	}
	turns, _ = st.ListTurns(context.Background(), convID)
	var marked bool
	for _, tn := range turns {
		if tn.ID == asTurnID && strings.Contains(tn.Metadata, `"interrupted":true`) {
			marked = true
		}
	}
	if !marked {
		t.Fatal("assistant turn metadata should be marked interrupted")
	}
	evs, err := st.ListEventsSince(context.Background(), convID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var truncated bool
	for _, e := range evs {
		if e.Type == events.TypeConversationTruncated && e.TurnID == asTurnID {
			truncated = true
		}
	}
	if !truncated {
		t.Fatal("a durable ConversationTruncated event should be persisted atomically with the metadata update")
	}
}

func TestRunTurnRejectsConcurrentTurn(t *testing.T) {
	srv, st := voiceTestServer(t)
	convID := seedConversation(t, st)

	// A turn is already in flight for this conversation (text or voice) and never
	// releases. A short ctx makes the bounded wait give up quickly with ErrTurnInProgress.
	if !srv.beginTurn(convID) {
		t.Fatal("beginTurn should succeed first")
	}
	defer srv.endTurn(convID)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _, err := srv.RunTurn(ctx, convID, "usr_1", "hello", nil, nil)
	if err != ErrTurnInProgress {
		t.Fatalf("RunTurn during an active turn = %v, want ErrTurnInProgress", err)
	}
}

func TestRunTurnWaitsForBusyTurnLock(t *testing.T) {
	srv, st := voiceTestServer(t)
	convID := seedConversation(t, st)
	// Simulate a just-cancelled prior reply still holding the per-conversation turn
	// lock; it releases shortly after (the provider observes the cancellation).
	if !srv.beginTurn(convID) {
		t.Fatal("precondition: claim the lock")
	}
	go func() {
		time.Sleep(60 * time.Millisecond)
		srv.endTurn(convID)
	}()
	// The barge-in turn must WAIT for the lock and then persist + answer — not fail
	// immediately with ErrTurnInProgress and drop the user's utterance.
	reply, turnID, err := srv.RunTurn(context.Background(), convID, "usr_1", "after the barge-in", nil, nil)
	if err != nil {
		t.Fatalf("RunTurn should wait for the busy lock and succeed, got %v", err)
	}
	if reply == "" || turnID == "" {
		t.Fatal("RunTurn should have produced + persisted a reply once the lock freed")
	}
	turns, _ := st.ListTurns(context.Background(), convID)
	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2 (the waited barge-in turn persisted)", len(turns))
	}
}

// TestRunTurnWaitsForInterruptFence is the round-25 regression: a reserved interrupt
// fence (a barge-in / stop / truncation mark in flight) blocks a turn entry point from
// building context until the mark commits (release) — the happens-before edge that keeps
// a next turn (voice OR a concurrent text POST) from reading the pre-interrupt full reply.
func TestRunTurnWaitsForInterruptFence(t *testing.T) {
	srv, st := voiceTestServer(t)
	convID := seedConversation(t, st)

	release := srv.BeginInterrupt(convID) // an interrupt mark is in flight
	done := make(chan struct{})
	go func() {
		_, _, _ = srv.RunTurn(context.Background(), convID, "usr_1", "next turn", nil, nil)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("RunTurn built context before the interrupt fence released")
	case <-time.After(60 * time.Millisecond):
	}
	release()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunTurn did not proceed after the interrupt fence released")
	}
}

// TestInterruptFenceReleases asserts awaitInterrupts blocks while a fence is reserved
// and returns once released (and that nested reserves are balanced).
func TestInterruptFenceReleases(t *testing.T) {
	srv, st := voiceTestServer(t)
	convID := seedConversation(t, st)

	r1 := srv.BeginInterrupt(convID)
	r2 := srv.BeginInterrupt(convID) // two in-flight marks
	got := make(chan struct{})
	go func() { srv.awaitInterrupts(context.Background(), convID); close(got) }()
	r1()
	select {
	case <-got:
		t.Fatal("awaitInterrupts returned with a fence still reserved")
	case <-time.After(40 * time.Millisecond):
	}
	r2()
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("awaitInterrupts did not return after all fences released")
	}
}

// TestRunTurnAbortsOnFenceCtxCancel is the round-26 regression: if the interrupt fence
// never clears within the request's deadline (a stuck mark / a disconnect), the turn
// entry point must ABORT — not build context from stale state or persist a turn after
// its deadline.
func TestRunTurnAbortsOnFenceCtxCancel(t *testing.T) {
	srv, st := voiceTestServer(t)
	convID := seedConversation(t, st)
	release := srv.BeginInterrupt(convID) // an interrupt mark that never releases here
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, _, err := srv.RunTurn(ctx, convID, "usr_1", "next turn", nil, nil); err == nil {
		t.Fatal("RunTurn must abort when the interrupt fence never clears within ctx")
	}
	turns, _ := st.ListTurns(context.Background(), convID)
	if len(turns) != 0 {
		t.Fatalf("an aborted RunTurn must not persist a turn (got %d)", len(turns))
	}
}

// TestRunTurnOnCommittedFenceNoSelfDeadlock is the round-29 regression on the REAL
// Server: the live loop reserves the interrupt/playback fence inside onCommitted (which
// RunTurn invokes at the durable commit). Because RunTurn's awaitInterrupts runs at its
// START — long before onCommitted reserves — RunTurn must NOT wait on a fence its own
// reply reserved. A prior bug reserved the fence BEFORE RunTurn and self-deadlocked.
func TestRunTurnOnCommittedFenceNoSelfDeadlock(t *testing.T) {
	srv, st := voiceTestServer(t)
	convID := seedConversation(t, st)

	var release func()
	done := make(chan struct{})
	go func() {
		_, _, _ = srv.RunTurn(context.Background(), convID, "usr_1", "hi", nil, func(turnID string) {
			release = srv.BeginInterrupt(convID) // reserve at commit, exactly as the live worker does
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunTurn self-deadlocked on a fence its own onCommitted reserved")
	}
	if release != nil {
		release()
	}
}

func TestRunTurnReturnsErrorWhenNotDurable(t *testing.T) {
	srv, st := voiceTestServer(t)
	convID := seedConversation(t, st)
	// Force persistence to fail by closing the store: RunTurn must return a non-nil
	// error so the live loop never speaks a reply that isn't in conversation history.
	_ = st.Close()

	reply, _, err := srv.RunTurn(context.Background(), convID, "usr_1", "hello", nil, nil)
	if err == nil {
		t.Fatal("RunTurn must return an error when the turn can't be persisted")
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty on a non-durable turn", reply)
	}
}
