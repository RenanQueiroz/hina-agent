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
// sends — a lagging subscriber simply replays from the store on reconnect.
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

	for _, ch := range b.subs[e.ConversationID] {
		select {
		case ch <- e:
		default: // subscriber lagging; it catches up via replay
		}
	}
	return e, nil
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
