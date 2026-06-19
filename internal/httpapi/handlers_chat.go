package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/RenanQueiroz/hina-agent/internal/agent"
	"github.com/RenanQueiroz/hina-agent/internal/auth"
	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/id"
	"github.com/RenanQueiroz/hina-agent/internal/llm"
	"github.com/RenanQueiroz/hina-agent/internal/store"
	"github.com/RenanQueiroz/hina-agent/internal/wire"
)

func (s *Server) handleListConversations(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	convs, err := s.store.ListConversationsByOwner(r.Context(), u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]wire.Conversation, 0, len(convs))
	for _, c := range convs {
		out = append(out, conversationView(c))
	}
	writeJSON(w, http.StatusOK, wire.ConversationList{Conversations: out})
}

func (s *Server) handleCreateConversation(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	var body struct {
		Title string `json:"title"`
	}
	_ = decodeJSON(w, r, &body) // title optional

	conv := store.Conversation{ID: id.New("cnv"), OwnerUserID: u.ID, Title: body.Title}
	// Persist the conversation and its SessionCreated event atomically, so the
	// API never returns a created conversation whose creation event is missing
	// from the replayed event log.
	evt := s.newEvent(events.SourceServer, events.TypeSessionCreated, conv.ID, u.ID, "", map[string]string{"title": conv.Title})
	if _, err := s.bus.PublishConversation(r.Context(), conv, evt); err != nil {
		s.log.Error("create conversation", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	conv, _ = s.store.GetConversation(r.Context(), conv.ID)
	writeJSON(w, http.StatusCreated, conversationView(conv))
}

func (s *Server) handleGetConversation(w http.ResponseWriter, r *http.Request) {
	conv, ok := s.loadOwnedConversation(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, conversationView(conv))
}

func (s *Server) handleListTurns(w http.ResponseWriter, r *http.Request) {
	conv, ok := s.loadOwnedConversation(w, r)
	if !ok {
		return
	}
	turns, err := s.store.ListTurns(r.Context(), conv.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]wire.Turn, 0, len(turns))
	for _, t := range turns {
		out = append(out, wire.Turn{ID: t.ID, Role: t.Role, Mode: t.Mode, Text: t.CanonicalText, CreatedAt: t.CreatedAt})
	}
	writeJSON(w, http.StatusOK, wire.TurnList{Turns: out})
}

// handlePostMessage persists the user's text turn, then streams an assistant
// reply from the configured LLM provider. Streaming is tied to the request
// context: if the client aborts (stop/navigate away), the upstream LLM call is
// cancelled and the partial reply is persisted with an interrupted marker. Text
// deltas are ephemeral (live-only); the persisted turn + AgentTextCompleted
// event carry the canonical text.
func (s *Server) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	conv, ok := s.loadOwnedConversation(w, r)
	if !ok {
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := decodeJSON(w, r, &body); err != nil || strings.TrimSpace(body.Text) == "" {
		writeErr(w, http.StatusBadRequest, "text is required")
		return
	}

	// Serialize turns per conversation: reject a second concurrent POST so
	// interleaved turns can't corrupt the durable order or the context the model
	// sees. The composer disables Send while pending; this also guards other
	// tabs, API clients, and retries.
	if !s.beginTurn(conv.ID) {
		writeErr(w, http.StatusConflict, "a turn is already in progress for this conversation")
		return
	}
	defer s.endTurn(conv.ID)

	ctx := r.Context()

	// 1. Persist the user turn together with its UserTextSubmitted event
	//    atomically, on a non-cancelled context: once the message is accepted it
	//    must be durable, and the turn must never outlive its event (the timeline
	//    replays from events, so a turn with no event would vanish on reload).
	userTurn := store.Turn{ID: id.New("trn"), ConversationID: conv.ID, Role: "user", Mode: "text", CanonicalText: body.Text}
	userEvt := s.newEvent(events.SourceClient, events.TypeUserTextSubmitted, conv.ID, u.ID, userTurn.ID,
		map[string]string{"text": body.Text})
	if _, err := s.bus.PublishTurn(context.Background(), userTurn, userEvt); err != nil {
		s.log.Error("persist user turn", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	// 2. Build model context from the full canonical history (includes this turn).
	turns, err := s.store.ListTurns(ctx, conv.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	msgs := agent.BuildContext(s.cfg.LLM.SystemPrompt, turns)

	// 3. Stream the assistant reply.
	asTurnID := id.New("trn")
	s.publish(ctx, events.SourceServer, events.TypeTurnStarted, conv.ID, u.ID, asTurnID, nil)

	stream, err := s.provider.Stream(ctx, llm.Request{Messages: msgs})
	if err != nil {
		s.publish(context.Background(), events.SourceServer, events.TypeError, conv.ID, u.ID, asTurnID,
			map[string]string{"error": err.Error()})
		writeErr(w, http.StatusBadGateway, "llm backend error")
		return
	}

	var sb strings.Builder
	var streamErr error
	for d := range stream {
		if d.Err != nil {
			streamErr = d.Err
			break
		}
		if d.Done {
			break
		}
		sb.WriteString(d.Text)
		s.publishEphemeral(events.SourceServer, events.TypeAgentTextDelta, conv.ID, u.ID, asTurnID,
			map[string]string{"delta": d.Text})
	}
	interrupted := ctx.Err() != nil

	// 4. Finalize on a non-cancelled context so a client disconnect still records
	//    what was generated. The assistant turn and its durable event(s) are
	//    persisted atomically (PublishTurn), so the turn never outlives its event.
	text := sb.String()

	// A real mid-stream failure (not a client-initiated interrupt) is NOT a
	// successful turn: record the partial as errored, emit ErrorEvent, close the
	// turn, and return 502 — never AgentTextCompleted.
	if streamErr != nil && !interrupted {
		assistantTurn := store.Turn{
			ID: asTurnID, ConversationID: conv.ID, Role: "assistant", Mode: "text",
			CanonicalText: text, Metadata: `{"error":true}`,
		}
		// Carry the partial canonical text in the durable ErrorEvent so a reload
		// replays exactly what BuildContext will feed the model (the live-only
		// AgentTextDelta events aren't replayed). Keeps UI history and model
		// context in parity for failed turns.
		errEvt := s.newEvent(events.SourceServer, events.TypeError, conv.ID, u.ID, asTurnID,
			map[string]any{"error": streamErr.Error(), "text": text})
		commitEvt := s.newEvent(events.SourceServer, events.TypeTurnCommitted, conv.ID, u.ID, asTurnID, nil)
		if _, err := s.bus.PublishTurn(context.Background(), assistantTurn, errEvt, commitEvt); err != nil {
			s.log.Error("persist failed assistant turn", "err", err)
		}
		writeErr(w, http.StatusBadGateway, "llm stream error")
		return
	}

	meta := "{}"
	if interrupted {
		meta = `{"interrupted":true}`
	}
	assistantTurn := store.Turn{
		ID: asTurnID, ConversationID: conv.ID, Role: "assistant", Mode: "text",
		CanonicalText: text, Metadata: meta,
	}
	completedEvt := s.newEvent(events.SourceServer, events.TypeAgentTextCompleted, conv.ID, u.ID, asTurnID,
		map[string]any{"text": text, "interrupted": interrupted})
	commitEvt := s.newEvent(events.SourceServer, events.TypeTurnCommitted, conv.ID, u.ID, asTurnID, nil)
	if _, err := s.bus.PublishTurn(context.Background(), assistantTurn, completedEvt, commitEvt); err != nil {
		// Finalization is part of the request's commit. If the assistant turn
		// isn't durable, don't acknowledge success: the client would believe a
		// turn persisted that replay and BuildContext lack. (If the client is
		// already gone, there's no response to send — the log is the record.)
		s.log.Error("persist assistant turn", "err", err)
		if !interrupted {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	if interrupted {
		return // client is gone; response write would fail
	}
	writeJSON(w, http.StatusOK, wire.PostMessageResponse{
		UserTurnID:      userTurn.ID,
		AssistantTurnID: asTurnID,
		Text:            text,
		Interrupted:     false,
	})
}

// handleEvents streams a conversation's events over SSE: subscribe first (to
// avoid gaps), replay everything after ?since, then stream live, deduping by seq.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	conv, ok := s.loadOwnedConversation(w, r)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	// On reconnect, EventSource sends Last-Event-ID (the last persisted seq);
	// honor it so the stream resumes without re-replaying. Fall back to ?since.
	var since int64
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		since, _ = strconv.ParseInt(v, 10, 64)
	} else if v := r.URL.Query().Get("since"); v != "" {
		since, _ = strconv.ParseInt(v, 10, 64)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, cancel := s.bus.Subscribe(conv.ID)
	defer cancel()

	// Initial catch-up replay. A failure here is a stream setup failure: return
	// without emitting anything so we never advance the client past unsent
	// durable events; EventSource reconnects (Last-Event-ID=since) and retries.
	lastSeq := since
	missed, err := s.bus.Replay(r.Context(), conv.ID, since)
	if err != nil {
		s.log.Error("sse initial replay", "conversation", conv.ID, "err", err)
		return
	}
	for _, e := range missed {
		writeSSE(w, e)
		lastSeq = e.Seq
	}
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case e, open := <-ch:
			if !open {
				// The bus poisoned this subscriber (its buffer overflowed). Flush
				// every persisted event we missed from the store, then return so
				// the browser's EventSource reconnects (Last-Event-ID=lastSeq) and
				// resumes — no persisted event is lost. If even this replay fails we
				// still return without advancing, so reconnect retries the range.
				if recovered, err := s.bus.Replay(ctx, conv.ID, lastSeq); err == nil {
					for _, m := range recovered {
						if m.Seq <= lastSeq {
							continue
						}
						writeSSE(w, m)
						lastSeq = m.Seq
					}
					flusher.Flush()
				} else {
					s.log.Error("sse replay after poison", "conversation", conv.ID, "err", err)
				}
				return
			}
			next, ok := s.deliverEvent(ctx, w, conv.ID, e, lastSeq)
			if !ok {
				// Gap replay failed; terminate without advancing so the browser
				// reconnects (Last-Event-ID still before the gap) and recovers it.
				return
			}
			lastSeq = next
			flusher.Flush()
		}
	}
}

// deliverEvent writes a single event to the SSE stream and returns the updated
// lastSeq plus an ok flag. Ephemeral deltas (seq==0) are always delivered live
// and never advance lastSeq. For persisted events it dedups against what was
// already sent and, on detecting a gap (e.Seq > lastSeq+1 — i.e. earlier
// persisted events were dropped from this subscriber's full buffer), replays the
// missing range from the store before sending. If that replay fails it returns
// ok=false WITHOUT writing the later event or advancing lastSeq, so the caller
// terminates the stream and the browser reconnects (Last-Event-ID still points
// before the gap) and recovers it — a transient store error never lets the
// client skip durable events. The bus's non-blocking fan-out stays safe for the
// publisher.
func (s *Server) deliverEvent(ctx context.Context, w http.ResponseWriter, convID string, e events.Event, lastSeq int64) (int64, bool) {
	if e.Seq == 0 { // ephemeral delta: live-only
		writeSSE(w, e)
		return lastSeq, true
	}
	if e.Seq <= lastSeq { // already delivered via replay/dedup
		return lastSeq, true
	}
	if e.Seq > lastSeq+1 { // gap: persisted events were dropped — replay them
		missed, err := s.bus.Replay(ctx, convID, lastSeq)
		if err != nil {
			s.log.Error("sse replay during gap", "conversation", convID, "err", err)
			return lastSeq, false
		}
		for _, m := range missed {
			if m.Seq <= lastSeq {
				continue
			}
			writeSSE(w, m)
			lastSeq = m.Seq
		}
		if e.Seq <= lastSeq { // replay already covered e
			return lastSeq, true
		}
	}
	writeSSE(w, e)
	if e.Seq > lastSeq {
		lastSeq = e.Seq
	}
	return lastSeq, true
}

func writeSSE(w http.ResponseWriter, e events.Event) {
	data, err := json.Marshal(e)
	if err != nil {
		return
	}
	// No `event:` line: all events use the default message event so the browser
	// EventSource delivers them through one onmessage handler; the client reads
	// the type from the JSON. id: (persisted seq only) drives reconnect resume.
	if e.Seq > 0 {
		fmt.Fprintf(w, "id: %d\n", e.Seq)
	}
	fmt.Fprintf(w, "data: %s\n\n", data)
}

func (s *Server) publishEphemeral(source, typ, convID, userID, turnID string, payload any) {
	e, err := events.New(source, typ, convID, userID, turnID, payload)
	if err != nil {
		s.log.Error("build ephemeral event", "type", typ, "err", err)
		return
	}
	s.bus.PublishEphemeral(e)
}

// loadOwnedConversation loads the conversation in the path and verifies the
// authenticated user owns it.
func (s *Server) loadOwnedConversation(w http.ResponseWriter, r *http.Request) (store.Conversation, bool) {
	u, _ := auth.UserFrom(r.Context())
	conv, err := s.store.GetConversation(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not found")
		} else {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return store.Conversation{}, false
	}
	if conv.OwnerUserID != u.ID {
		writeErr(w, http.StatusForbidden, "forbidden")
		return store.Conversation{}, false
	}
	return conv, true
}

func (s *Server) publish(ctx context.Context, source, typ, convID, userID, turnID string, payload any) {
	e, err := events.New(source, typ, convID, userID, turnID, payload)
	if err != nil {
		s.log.Error("build event", "type", typ, "err", err)
		return
	}
	if _, err := s.bus.Publish(ctx, e); err != nil {
		s.log.Error("publish event", "type", typ, "err", err)
	}
}

// newEvent builds an event for atomic turn+event persistence. If payload
// marshaling somehow fails it logs and falls back to an empty payload so the
// event still carries its correct type and ids (never a malformed/empty event).
func (s *Server) newEvent(source, typ, convID, userID, turnID string, payload any) events.Event {
	e, err := events.New(source, typ, convID, userID, turnID, payload)
	if err != nil {
		s.log.Error("build event payload", "type", typ, "err", err)
		e, _ = events.New(source, typ, convID, userID, turnID, nil)
	}
	return e
}

func conversationView(c store.Conversation) wire.Conversation {
	return wire.Conversation{ID: c.ID, Title: c.Title, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
}
