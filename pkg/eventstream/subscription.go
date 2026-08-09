package eventstream

import (
	"context"
	"io"
	"sync"

	"github.com/danieliser/agentruntime/pkg/durable"
)

// Subscription replays through the handshake tail and then consumes the same
// ordered live stream without changing cursor domains.
type Subscription struct {
	store       durable.Store
	sessionID   string
	replayUntil int64
	earliest    int64
	cursor      int64
	replay      []durable.Event

	state      *sessionState
	id         uint64
	live       <-chan durable.Event
	subscriber *liveSubscriber
	cancel     context.CancelFunc
	closed     chan struct{}
	closeOnce  sync.Once
	nextMu     sync.Mutex
}

// Subscribe atomically snapshots the durable tail and joins live publication.
// Events strictly after afterSequence are returned by Subscription.Next.
func (broker *Broker) Subscribe(ctx context.Context, sessionID string, afterSequence int64, liveBuffer int) (*Subscription, error) {
	const op = "subscribe_events"
	if broker == nil || broker.store == nil || sessionID == "" || afterSequence < 0 {
		return nil, durable.NewError(durable.CodeInvalidArgument, op, "store, session ID, and nonnegative cursor are required", nil)
	}
	if liveBuffer <= 0 {
		liveBuffer = defaultLiveBuffer
	}
	if liveBuffer > maxLiveBuffer {
		liveBuffer = maxLiveBuffer
	}
	state := broker.session(sessionID)
	state.mu.Lock()
	defer state.mu.Unlock()
	session, err := broker.store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if afterSequence > session.LastSequence {
		return nil, durable.NewError(durable.CodeInvalidCursor, op, "cursor is beyond the durable tail", nil)
	}
	if afterSequence < session.LastSequence {
		if _, err := broker.store.ListEvents(ctx, durable.EventQuery{SessionID: sessionID, AfterSequence: afterSequence, Limit: 1}); err != nil {
			return nil, err
		}
	}
	subscriptionCtx, cancel := context.WithCancel(ctx)
	state.nextSubscriber++
	id := state.nextSubscriber
	live := make(chan durable.Event, liveBuffer)
	closed := make(chan struct{})
	state.subscribers[id] = &liveSubscriber{events: live, done: subscriptionCtx.Done()}
	subscription := &Subscription{
		store: broker.store, sessionID: sessionID, replayUntil: session.LastSequence,
		cursor: afterSequence, state: state, id: id, live: live, subscriber: state.subscribers[id], cancel: cancel, closed: closed,
	}
	if session.LastSequence > 0 {
		subscription.earliest = 1
	}
	go func() {
		select {
		case <-subscriptionCtx.Done():
			_ = subscription.Close()
		case <-closed:
		}
	}()
	return subscription, nil
}

// Next returns exactly the next contiguous durable event.
func (subscription *Subscription) Next(ctx context.Context) (durable.Event, error) {
	const op = "next_event"
	if subscription == nil {
		return durable.Event{}, durable.NewError(durable.CodeInvalidState, op, "subscription is nil", nil)
	}
	subscription.nextMu.Lock()
	defer subscription.nextMu.Unlock()
	if subscription.cursor < subscription.replayUntil {
		if len(subscription.replay) == 0 {
			page, err := subscription.store.ListEvents(ctx, durable.EventQuery{
				SessionID: subscription.sessionID, AfterSequence: subscription.cursor, Limit: 1000,
			})
			if err != nil {
				return durable.Event{}, err
			}
			for _, event := range page.Events {
				if event.Sequence > subscription.replayUntil {
					break
				}
				subscription.replay = append(subscription.replay, event)
			}
			if len(subscription.replay) == 0 {
				return durable.Event{}, durable.NewError(durable.CodeEventGap, op, "durable replay ended before handshake tail", nil)
			}
		}
		event := subscription.replay[0]
		subscription.replay = subscription.replay[1:]
		if event.Sequence != subscription.cursor+1 {
			return durable.Event{}, durable.NewError(durable.CodeEventGap, op, "durable replay is not contiguous", nil)
		}
		subscription.cursor = event.Sequence
		return event, nil
	}
	select {
	case <-ctx.Done():
		return durable.Event{}, ctx.Err()
	case <-subscription.closed:
		return durable.Event{}, io.EOF
	case event, ok := <-subscription.live:
		if !ok {
			if err := subscription.subscriber.failure(); err != nil {
				return durable.Event{}, err
			}
			return durable.Event{}, io.EOF
		}
		if event.Sequence != subscription.cursor+1 {
			return durable.Event{}, durable.NewError(durable.CodeEventGap, op, "live event sequence is not contiguous", nil)
		}
		subscription.cursor = event.Sequence
		return event, nil
	}
}

// Cursor returns the last contiguous sequence delivered by Next.
func (subscription *Subscription) Cursor() int64 {
	subscription.nextMu.Lock()
	defer subscription.nextMu.Unlock()
	return subscription.cursor
}

// ReplayUntil is the durable tail captured by the stored/live handshake.
func (subscription *Subscription) ReplayUntil() int64 {
	if subscription == nil {
		return 0
	}
	return subscription.replayUntil
}

// EarliestSequence is the oldest cursor boundary available for replay.
func (subscription *Subscription) EarliestSequence() int64 {
	if subscription == nil {
		return 0
	}
	return subscription.earliest
}

// Close idempotently leaves live publication.
func (subscription *Subscription) Close() error {
	if subscription == nil {
		return nil
	}
	subscription.closeOnce.Do(func() {
		subscription.cancel()
		subscription.state.mu.Lock()
		if subscriber, exists := subscription.state.subscribers[subscription.id]; exists {
			delete(subscription.state.subscribers, subscription.id)
			close(subscriber.events)
		}
		subscription.state.mu.Unlock()
		close(subscription.closed)
	})
	return nil
}
