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
	if err := s.store.CreateConversation(r.Context(), conv); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	conv, _ = s.store.GetConversation(r.Context(), conv.ID)
	s.publish(r.Context(), events.SourceServer, events.TypeSessionCreated, conv.ID, u.ID, "", map[string]string{"title": conv.Title})
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
	ctx := r.Context()

	// 1. Persist the user turn + checkpoint events.
	userTurn := store.Turn{ID: id.New("trn"), ConversationID: conv.ID, Role: "user", Mode: "text", CanonicalText: body.Text}
	if err := s.store.AppendTurn(ctx, userTurn); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.publish(ctx, events.SourceClient, events.TypeUserTextSubmitted, conv.ID, u.ID, userTurn.ID,
		map[string]string{"text": body.Text})

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

	// 4. Finalize, using a fresh context so a client disconnect still records
	//    what was generated.
	pctx := context.Background()
	text := sb.String()

	// A real mid-stream failure (not a client-initiated interrupt) is NOT a
	// successful turn: record the partial as errored, emit ErrorEvent, close the
	// turn, and return 502 — never AgentTextCompleted.
	if streamErr != nil && !interrupted {
		if err := s.store.AppendTurn(pctx, store.Turn{
			ID: asTurnID, ConversationID: conv.ID, Role: "assistant", Mode: "text",
			CanonicalText: text, Metadata: `{"error":true}`,
		}); err != nil {
			s.log.Error("persist failed assistant turn", "err", err)
		}
		s.publish(pctx, events.SourceServer, events.TypeError, conv.ID, u.ID, asTurnID,
			map[string]string{"error": streamErr.Error()})
		s.publish(pctx, events.SourceServer, events.TypeTurnCommitted, conv.ID, u.ID, asTurnID, nil)
		writeErr(w, http.StatusBadGateway, "llm stream error")
		return
	}

	meta := "{}"
	if interrupted {
		meta = `{"interrupted":true}`
	}
	if err := s.store.AppendTurn(pctx, store.Turn{
		ID: asTurnID, ConversationID: conv.ID, Role: "assistant", Mode: "text",
		CanonicalText: text, Metadata: meta,
	}); err != nil {
		s.log.Error("persist assistant turn", "err", err)
	}
	s.publish(pctx, events.SourceServer, events.TypeAgentTextCompleted, conv.ID, u.ID, asTurnID,
		map[string]any{"text": text, "interrupted": interrupted})
	s.publish(pctx, events.SourceServer, events.TypeTurnCommitted, conv.ID, u.ID, asTurnID, nil)

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

	lastSeq := since
	if missed, err := s.bus.Replay(r.Context(), conv.ID, since); err == nil {
		for _, e := range missed {
			writeSSE(w, e)
			lastSeq = e.Seq
		}
	}
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case e, open := <-ch:
			if !open {
				return
			}
			// Persisted events (seq>0) dedup against replay; ephemeral deltas
			// (seq==0) are always delivered live and never advance lastSeq.
			if e.Seq > 0 && e.Seq <= lastSeq {
				continue
			}
			writeSSE(w, e)
			if e.Seq > lastSeq {
				lastSeq = e.Seq
			}
			flusher.Flush()
		}
	}
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

func conversationView(c store.Conversation) wire.Conversation {
	return wire.Conversation{ID: c.ID, Title: c.Title, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
}
