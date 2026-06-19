package events

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/id"
	"github.com/RenanQueiroz/hina-agent/internal/store"
)

// Bus persists events (assigning a per-conversation seq) and fans them out to
// in-process subscribers. A single mutex serializes publishes so the store's
// seq assignment never races; fan-out uses buffered channels with non-blocking
// sends so one slow subscriber can never stall the publisher (which is on the
// LLM streaming path). Persisted (seq>0) events are never silently dropped: if a
// subscriber's buffer is full it is poisoned (channel closed + removed) so its
// SSE handler replays the remainder from the store and the browser reconnects —
// see fanoutDurable and the SSE handler. Only *ephemeral* deltas (seq==0) are
// disposable on a full buffer; their text is carried durably by
// AgentTextCompleted.
type Bus struct {
	mu      sync.Mutex
	store   *store.Store
	subs    map[string]map[int]chan Event // conversationID -> subID -> chan
	nextSub int
}

// NewBus constructs a bus backed by the given store.
func NewBus(st *store.Store) *Bus {
	return &Bus{store: st, subs: make(map[string]map[int]chan Event)}
}

// Publish persists the event (filling in EventID/Seq/ServerTS) and fans it out.
func (b *Bus) Publish(ctx context.Context, e Event) (Event, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if e.EventID == "" {
		e.EventID = id.New("evt")
	}
	payload := string(e.Payload)
	if payload == "" {
		payload = "{}"
	}
	se := &store.Event{
		EventID:        e.EventID,
		ConversationID: e.ConversationID,
		UserID:         e.UserID,
		TurnID:         e.TurnID,
		Source:         e.Source,
		Type:           e.Type,
		Payload:        payload,
	}
	if err := b.store.AppendEvent(ctx, se); err != nil {
		return Event{}, err
	}
	e.Seq = se.Seq
	e.ServerTS = se.ServerTS
	if len(e.Payload) == 0 {
		e.Payload = []byte("{}")
	}

	b.fanoutDurable(e)
	return e, nil
}

// PublishTurn persists a turn and its durable event(s) atomically (via the
// store's single transaction), then fans the events out. This is the path text
// turns take so a turn and the event that announces it are always both-or-
// neither — never a persisted turn with no event (which would vanish from the
// replayed timeline). Seq assignment is serialized by the bus mutex, matching
// Publish. Returns the events with Seq/ServerTS populated.
func (b *Bus) PublishTurn(ctx context.Context, t store.Turn, evs ...Event) ([]Event, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	ses := make([]*store.Event, len(evs))
	for i := range evs {
		if evs[i].EventID == "" {
			evs[i].EventID = id.New("evt")
		}
		payload := string(evs[i].Payload)
		if payload == "" {
			payload = "{}"
		}
		ses[i] = &store.Event{
			EventID:        evs[i].EventID,
			ConversationID: evs[i].ConversationID,
			UserID:         evs[i].UserID,
			TurnID:         evs[i].TurnID,
			Source:         evs[i].Source,
			Type:           evs[i].Type,
			Payload:        payload,
		}
	}
	if err := b.store.AppendTurnWithEvents(ctx, t, ses); err != nil {
		return nil, err
	}
	for i := range evs {
		evs[i].Seq = ses[i].Seq
		evs[i].ServerTS = ses[i].ServerTS
		if len(evs[i].Payload) == 0 {
			evs[i].Payload = []byte("{}")
		}
		b.fanoutDurable(evs[i])
	}
	return evs, nil
}

// fanoutDurable delivers a persisted (seq>0) event to live subscribers. A
// subscriber whose buffer is full is *poisoned* — its channel is closed and
// removed — rather than having the event silently dropped: the SSE handler sees
// the close, replays everything it missed from the store, and the browser
// reconnects. This guarantees a persisted event (e.g. the final
// AgentTextCompleted/TurnCommitted) is never lost to a slow client. Must be
// called with b.mu held.
func (b *Bus) fanoutDurable(e Event) {
	m := b.subs[e.ConversationID]
	for subID, ch := range m {
		select {
		case ch <- e:
		default:
			close(ch)
			delete(m, subID)
		}
	}
	if m != nil && len(m) == 0 {
		delete(b.subs, e.ConversationID)
	}
}

// PublishEphemeral fans an event out to live subscribers WITHOUT persisting it
// or assigning a seq (it keeps Seq=0). Used for high-frequency, replaceable
// updates like streaming text deltas: the durable checkpoint (e.g. the
// AgentTextCompleted event + the persisted turn) carries the canonical text, so
// per-token rows are not written. Ephemeral events are not part of replay.
func (b *Bus) PublishEphemeral(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e.EventID == "" {
		e.EventID = id.New("evt")
	}
	if e.ServerTS.IsZero() {
		e.ServerTS = time.Now().UTC()
	}
	if len(e.Payload) == 0 {
		e.Payload = []byte("{}")
	}
	// Ephemeral deltas are replaceable, so a full buffer simply drops them; the
	// durable AgentTextCompleted carries the canonical text.
	for _, ch := range b.subs[e.ConversationID] {
		select {
		case ch <- e:
		default:
		}
	}
}

// Subscribe returns a channel of events for a conversation plus a cancel func.
// Callers typically replay missed events from the store first, then stream live
// from this channel.
func (b *Bus) Subscribe(conversationID string) (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.subs[conversationID] == nil {
		b.subs[conversationID] = make(map[int]chan Event)
	}
	subID := b.nextSub
	b.nextSub++
	ch := make(chan Event, 64)
	b.subs[conversationID][subID] = ch

	cancel := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if m := b.subs[conversationID]; m != nil {
			if c, ok := m[subID]; ok {
				delete(m, subID)
				close(c)
			}
			if len(m) == 0 {
				delete(b.subs, conversationID)
			}
		}
	}
	return ch, cancel
}

// Replay returns persisted events for a conversation with seq > sinceSeq.
func (b *Bus) Replay(ctx context.Context, conversationID string, sinceSeq int64) ([]Event, error) {
	rows, err := b.store.ListEventsSince(ctx, conversationID, sinceSeq)
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(rows))
	for _, r := range rows {
		out = append(out, Event{
			EventID:        r.EventID,
			ConversationID: r.ConversationID,
			UserID:         r.UserID,
			TurnID:         r.TurnID,
			Seq:            r.Seq,
			ServerTS:       r.ServerTS,
			Source:         r.Source,
			Type:           r.Type,
			Payload:        json.RawMessage(r.Payload),
		})
	}
	return out, nil
}
