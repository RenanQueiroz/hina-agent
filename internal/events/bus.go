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
// AgentTextCompleted. A non-persisted terminal failure (PublishLiveFailure) also
// poisons a full subscriber, but records the failure on the subscription so the
// SSE handler can emit it to the client before the stream ends (it can't be
// replayed).
type Bus struct {
	mu      sync.Mutex
	store   *store.Store
	subs    map[string]map[int]*subscriber // conversationID -> subID -> subscriber
	nextSub int
}

// subscriber is one live event consumer. failure, set under the bus mutex when a
// non-replayable terminal failure poisons the subscriber, is the SSE handler's
// cue to emit that failure to the client before the closed channel ends the
// stream (it cannot be recovered via store replay).
type subscriber struct {
	ch      chan Event
	failure *Event
}

// Subscription is a live event subscription handle.
type Subscription struct {
	Events <-chan Event
	cancel func()
	sub    *subscriber
}

// Cancel unsubscribes and closes the event channel.
func (s *Subscription) Cancel() { s.cancel() }

// Failure returns the non-persisted terminal failure event that poisoned this
// subscription, or nil. Read it only after Events has been observed closed; the
// close establishes the happens-before edge with the bus's write.
func (s *Subscription) Failure() *Event { return s.sub.failure }

// NewBus constructs a bus backed by the given store.
func NewBus(st *store.Store) *Bus {
	return &Bus{store: st, subs: make(map[string]map[int]*subscriber)}
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

// PublishTurnMetadata updates an existing turn's metadata AND appends its durable
// event(s) atomically (one store transaction), then fans the events out. Like
// PublishTurn it keeps the metadata change and the event that announces it from
// ever diverging, and returns store.ErrNotFound (without publishing anything) if the
// turn doesn't exist. Seq assignment is serialized by the bus mutex. Returns the
// events with Seq/ServerTS populated.
func (b *Bus) PublishTurnMetadata(ctx context.Context, turnID, metadata string, evs ...Event) ([]Event, error) {
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
	if err := b.store.UpdateTurnMetadataWithEvents(ctx, turnID, metadata, ses); err != nil {
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

// PublishConversation creates a conversation and its SessionCreated event
// atomically (one store transaction), then fans the event out. Like PublishTurn,
// this keeps a conversation from ever existing without the event that announces
// it. Seq assignment is serialized by the bus mutex. Returns the event with
// Seq/ServerTS populated.
func (b *Bus) PublishConversation(ctx context.Context, c store.Conversation, e Event) (Event, error) {
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
	if err := b.store.CreateConversationWithEvent(ctx, c, se); err != nil {
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

// PublishLiveFailure fans out a NON-persisted terminal failure event (e.g. an
// assistant turn whose durable finalization failed, so there is nothing to
// persist or replay). Unlike PublishEphemeral it must not be silently dropped:
// it uses the same poison-on-full semantics as durable events, so a lagging
// subscriber is force-disconnected (to reconnect/re-sync) rather than missing
// the only signal that the turn failed. Subscribers with buffer room receive it
// live (seq==0). Used only on the rare finalization-failure path.
func (b *Bus) PublishLiveFailure(e Event) {
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
	failure := e // stable copy to hand to poisoned subscribers
	m := b.subs[e.ConversationID]
	for subID, sb := range m {
		select {
		case sb.ch <- e:
		default:
			// Full buffer: this event can't be replayed (it's non-persisted), so
			// record it as the poison reason — the SSE handler writes it to the
			// client before the closed channel ends the stream — then close.
			sb.failure = &failure
			close(sb.ch)
			delete(m, subID)
		}
	}
	if m != nil && len(m) == 0 {
		delete(b.subs, e.ConversationID)
	}
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
	for subID, sb := range m {
		select {
		case sb.ch <- e:
		default:
			close(sb.ch)
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
	for _, sb := range b.subs[e.ConversationID] {
		select {
		case sb.ch <- e:
		default:
		}
	}
}

// Subscribe registers a live subscriber for a conversation and returns a
// Subscription. Callers typically replay missed events from the store first,
// then stream live from sub.Events; on close they may read sub.Failure().
func (b *Bus) Subscribe(conversationID string) *Subscription {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.subs[conversationID] == nil {
		b.subs[conversationID] = make(map[int]*subscriber)
	}
	subID := b.nextSub
	b.nextSub++
	sb := &subscriber{ch: make(chan Event, 64)}
	b.subs[conversationID][subID] = sb

	cancel := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if m := b.subs[conversationID]; m != nil {
			if cur, ok := m[subID]; ok {
				delete(m, subID)
				close(cur.ch)
			}
			if len(m) == 0 {
				delete(b.subs, conversationID)
			}
		}
	}
	return &Subscription{Events: sb.ch, cancel: cancel, sub: sb}
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
