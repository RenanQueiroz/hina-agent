package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/RenanQueiroz/hina-agent/internal/auth"
	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/id"
	"github.com/RenanQueiroz/hina-agent/internal/store"
)

func (s *Server) handleListConversations(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	convs, err := s.store.ListConversationsByOwner(r.Context(), u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]map[string]any, 0, len(convs))
	for _, c := range convs {
		out = append(out, conversationView(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversations": out})
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
	s.publish(r.Context(), events.SourceServer, events.TypeSessionCreated, conv.ID, u.ID, map[string]string{"title": conv.Title})
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
	out := make([]map[string]any, 0, len(turns))
	for _, t := range turns {
		out = append(out, map[string]any{
			"id": t.ID, "role": t.Role, "mode": t.Mode,
			"text": t.CanonicalText, "created_at": t.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"turns": out})
}

// handlePostMessage persists a user text turn and drives the event flow. The
// LLM is wired in Phase 2; for now the assistant reply is a fixed placeholder so
// the end-to-end persistence + event + SSE path is exercised.
func (s *Server) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	conv, ok := s.loadOwnedConversation(w, r)
	if !ok {
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := decodeJSON(w, r, &body); err != nil || body.Text == "" {
		writeErr(w, http.StatusBadRequest, "text is required")
		return
	}

	ctx := r.Context()
	userTurn := store.Turn{ID: id.New("trn"), ConversationID: conv.ID, Role: "user", Mode: "text", CanonicalText: body.Text}
	if err := s.store.AppendTurn(ctx, userTurn); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.publish(ctx, events.SourceClient, events.TypeUserTextSubmitted, conv.ID, u.ID,
		map[string]string{"turn_id": userTurn.ID, "text": body.Text})
	s.publish(ctx, events.SourceServer, events.TypeTurnStarted, conv.ID, u.ID, nil)

	const reply = "Text chat is wired end-to-end (persistence + events + SSE). The LLM lands in Phase 2."
	s.publish(ctx, events.SourceServer, events.TypeAgentTextDelta, conv.ID, u.ID, map[string]string{"delta": reply})
	asTurn := store.Turn{ID: id.New("trn"), ConversationID: conv.ID, Role: "assistant", Mode: "text", CanonicalText: reply}
	if err := s.store.AppendTurn(ctx, asTurn); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.publish(ctx, events.SourceServer, events.TypeAgentTextCompleted, conv.ID, u.ID, map[string]string{"turn_id": asTurn.ID})
	s.publish(ctx, events.SourceServer, events.TypeTurnCommitted, conv.ID, u.ID, nil)

	writeJSON(w, http.StatusOK, map[string]any{
		"user_turn_id":      userTurn.ID,
		"assistant_turn_id": asTurn.ID,
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
	var since int64
	if v := r.URL.Query().Get("since"); v != "" {
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
			if e.Seq <= lastSeq {
				continue // already delivered via replay
			}
			writeSSE(w, e)
			lastSeq = e.Seq
			flusher.Flush()
		}
	}
}

func writeSSE(w http.ResponseWriter, e events.Event) {
	data, err := json.Marshal(e)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.Type, data)
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

func (s *Server) publish(ctx context.Context, source, typ, convID, userID string, payload any) {
	e, err := events.New(source, typ, convID, userID, payload)
	if err != nil {
		s.log.Error("build event", "type", typ, "err", err)
		return
	}
	if _, err := s.bus.Publish(ctx, e); err != nil {
		s.log.Error("publish event", "type", typ, "err", err)
	}
}

func conversationView(c store.Conversation) map[string]any {
	return map[string]any{
		"id": c.ID, "title": c.Title,
		"created_at": c.CreatedAt, "updated_at": c.UpdatedAt,
	}
}
