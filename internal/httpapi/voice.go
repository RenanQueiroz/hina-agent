package httpapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/agent"
	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/id"
	"github.com/RenanQueiroz/hina-agent/internal/sandbox"
	"github.com/RenanQueiroz/hina-agent/internal/store"
)

// ErrTurnInProgress is returned by RunTurn when a turn is already running for the
// conversation (text or voice) — turns are serialized per conversation.
var ErrTurnInProgress = errors.New("a turn is already in progress for this conversation")

// ErrPersistFailed is returned by RunTurn when the assistant turn could not be
// persisted durably; the caller must NOT speak the reply (it would be audible state
// the next model context lacks).
var ErrPersistFailed = errors.New("voice turn finalization failed")

// liveTurnLockWait bounds how long a live voice turn waits for the per-conversation
// turn lock (held by a just-cancelled previous reply until its provider observes the
// cancellation). Generous enough for a provider's cancellation latency, bounded so a
// genuinely stuck turn doesn't pin the new one forever.
const liveTurnLockWait = 3 * time.Second

// waitBeginTurn claims the per-conversation turn lock, retrying briefly so a live
// voice turn following a just-cancelled reply waits for that reply to release the lock
// instead of failing immediately. Returns false if the lock isn't free within timeout
// or ctx ends.
func (s *Server) waitBeginTurn(ctx context.Context, convID string, timeout time.Duration) bool {
	if s.beginTurn(convID) {
		return true
	}
	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(25 * time.Millisecond):
		}
		if s.beginTurn(convID) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// RunTurn implements rtc.AgentService for the live-voice loop: it persists the
// spoken request as a canonical "voice" user turn, runs the shared agent loop over
// the conversation's full history (so a text↔live switch preserves context with no
// audio rehydration), streams the reply via onDelta, and persists the assistant
// turn — all with the same durable/atomic semantics as the text path
// (handlePostMessage). It mirrors that handler deliberately rather than refactoring
// it, keeping the well-tested HTTP turn intact while sharing the agent.Loop, the
// context builder, and finalizeTurn. The returned text is what the caller speaks.
func (s *Server) RunTurn(ctx context.Context, convID, userID, transcript string, onDelta func(string), onCommitted func(turnID string)) (string, string, error) {
	transcript = strings.TrimSpace(transcript)
	if transcript == "" {
		return "", "", nil
	}
	// Serialize turns per conversation, like the text path. But unlike the text POST
	// (which 409s immediately on a busy conversation), a live voice turn WAITS briefly
	// for the lock: after a barge-in cancels the previous reply, the provider observes
	// the cancellation and releases the lock shortly after, and the barge-in utterance's
	// turn must be persisted + answered, not dropped with an error.
	if !s.waitBeginTurn(ctx, convID, liveTurnLockWait) {
		return "", "", ErrTurnInProgress
	}
	defer s.endTurn(convID)
	// Wait for any in-flight interrupt mark on this conversation to commit before
	// building context (below), so this turn never reads a pre-interrupt full reply. If
	// ctx ends before the fence clears, abort rather than persist/answer with stale state.
	if !s.awaitInterrupts(ctx, convID) {
		return "", "", ctx.Err()
	}

	// 1. Persist the user voice turn + its event atomically on a non-cancelled
	//    context: once recognized it must be durable, and never outlive its event.
	userTurn := store.Turn{ID: id.New("trn"), ConversationID: convID, Role: "user", Mode: "voice", CanonicalText: transcript}
	userEvt := s.newEvent(events.SourceClient, events.TypeUserTextSubmitted, convID, userID, userTurn.ID,
		map[string]string{"text": transcript})
	if _, err := s.bus.PublishTurn(context.Background(), userTurn, userEvt); err != nil {
		s.log.Error("persist voice user turn", "err", err)
		return "", "", err
	}

	// 2. Build context from the full canonical history (text + voice turns alike).
	turns, err := s.store.ListTurns(context.Background(), convID)
	if err != nil {
		return "", "", err
	}
	msgs := agent.BuildContext(s.cfg.LLM.SystemPrompt, turns)

	// 3. Stream the assistant reply through the shared agent loop, scoping any tool
	//    call to this user + conversation (Phase 7), exactly as the text path does.
	asTurnID := id.New("trn")
	s.publish(ctx, events.SourceServer, events.TypeTurnStarted, convID, userID, asTurnID, nil)
	turnCtx := withToolScope(ctx, sandbox.Scope{UserID: userID, ConversationID: convID})
	res := s.loop.Run(turnCtx, msgs, onDelta)
	text := res.Text

	// 4. Finalize durably (non-cancelled context), classifying error vs interrupt
	//    exactly as the text path does.
	if res.Err != nil {
		assistantTurn := store.Turn{
			ID: asTurnID, ConversationID: convID, Role: "assistant", Mode: "voice",
			CanonicalText: text, Metadata: `{"error":true}`,
		}
		s.finalizeTurn(convID, userID, asTurnID, text, assistantTurn,
			s.newEvent(events.SourceServer, events.TypeError, convID, userID, asTurnID,
				map[string]any{"error": res.Err.Error(), "text": text}),
			s.newEvent(events.SourceServer, events.TypeTurnCommitted, convID, userID, asTurnID, nil),
		)
		return text, asTurnID, res.Err
	}

	meta := "{}"
	if res.Interrupted {
		meta = `{"interrupted":true}`
	}
	assistantTurn := store.Turn{
		ID: asTurnID, ConversationID: convID, Role: "assistant", Mode: "voice",
		CanonicalText: text, Metadata: meta,
	}
	ok := s.finalizeTurn(convID, userID, asTurnID, text, assistantTurn,
		s.newEvent(events.SourceServer, events.TypeAgentTextCompleted, convID, userID, asTurnID,
			map[string]any{"text": text, "interrupted": res.Interrupted}),
		s.newEvent(events.SourceServer, events.TypeTurnCommitted, convID, userID, asTurnID, nil),
	)
	if !ok {
		// The assistant turn isn't durable. Like the text path, don't acknowledge
		// success — return an error so the caller does NOT speak a reply the next
		// model context can't see.
		return "", "", ErrPersistFailed
	}
	// The assistant turn is now durably committed. Invoke onCommitted while STILL holding
	// the per-conversation turn lock (endTurn is deferred), so the live loop can record
	// the turn id and reserve the interrupt fence under that lock — a concurrent text POST
	// (which must claim the same lock) then can't read this just-committed reply before
	// the live loop has fenced/marked it.
	if onCommitted != nil {
		onCommitted(asTurnID)
	}
	return text, asTurnID, nil
}

// MarkTurnInterrupted durably marks an already-committed assistant voice turn as
// interrupted by a barge-in, recording the played boundary (playedMs). The turn's
// generated text is kept (word-level audio→text truncation needs TTS timestamps,
// a later refinement), but the durable interrupted marker ensures a reload / the
// next model context reflects that the user heard only a prefix rather than the
// whole answer. A durable ConversationTruncated event is published so replay shows
// the truncation too.
func (s *Server) MarkTurnInterrupted(ctx context.Context, convID, userID, turnID string, playedMs int64) error {
	// Ordering with respect to next turns is provided by the interrupt FENCE
	// (BeginInterrupt/awaitInterrupts), reserved by the caller before this write and held
	// until it commits, so this method is just the durable write — it must NOT take the
	// per-conversation turn lock (a turn entry point awaits the fence while holding that
	// lock, so taking it here would deadlock).
	meta := fmt.Sprintf(`{"interrupted":true,"played_ms":%d}`, playedMs)
	// The metadata update (durable, read by BuildContext) and the ConversationTruncated
	// event (durable, replayed by the timeline) are written in ONE transaction so they
	// can never diverge; a missing turn returns ErrNotFound without publishing anything.
	evt := s.newEvent(events.SourceServer, events.TypeConversationTruncated, convID, userID, turnID,
		map[string]any{"played_ms": playedMs, "interrupted": true})
	if _, err := s.bus.PublishTurnMetadata(ctx, turnID, meta, evt); err != nil {
		s.log.Error("mark voice turn interrupted", "turn", turnID, "err", err)
		return err
	}
	return nil
}
