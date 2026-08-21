package api

import (
	"sync"
	"time"
)

type activationProgressFrame struct {
	FrameType string    `json:"frame_type"`
	SessionID string    `json:"session_id"`
	Stage     string    `json:"stage"`
	Message   string    `json:"message"`
	Timestamp time.Time `json:"timestamp"`
}

type activationProgressBroker struct {
	mu          sync.Mutex
	latest      map[string]activationProgressFrame
	subscribers map[string]map[uint64]chan activationProgressFrame
	nextID      uint64
}

func newActivationProgressBroker() *activationProgressBroker {
	return &activationProgressBroker{
		latest: make(map[string]activationProgressFrame), subscribers: make(map[string]map[uint64]chan activationProgressFrame),
	}
}

func (broker *activationProgressBroker) publish(sessionID, stage, message string, final bool) {
	if broker == nil || sessionID == "" {
		return
	}
	frame := activationProgressFrame{
		FrameType: "session.progress", SessionID: sessionID, Stage: stage,
		Message: message, Timestamp: time.Now().UTC(),
	}
	broker.mu.Lock()
	if final {
		delete(broker.latest, sessionID)
	} else {
		broker.latest[sessionID] = frame
	}
	for _, subscriber := range broker.subscribers[sessionID] {
		select {
		case subscriber <- frame:
		default:
		}
	}
	broker.mu.Unlock()
}

func (broker *activationProgressBroker) subscribe(sessionID string) (<-chan activationProgressFrame, func()) {
	updates := make(chan activationProgressFrame, 8)
	broker.mu.Lock()
	broker.nextID++
	id := broker.nextID
	if broker.subscribers[sessionID] == nil {
		broker.subscribers[sessionID] = make(map[uint64]chan activationProgressFrame)
	}
	broker.subscribers[sessionID][id] = updates
	if latest, ok := broker.latest[sessionID]; ok {
		updates <- latest
	}
	broker.mu.Unlock()
	var once sync.Once
	return updates, func() {
		once.Do(func() {
			broker.mu.Lock()
			delete(broker.subscribers[sessionID], id)
			if len(broker.subscribers[sessionID]) == 0 {
				delete(broker.subscribers, sessionID)
			}
			broker.mu.Unlock()
		})
	}
}
