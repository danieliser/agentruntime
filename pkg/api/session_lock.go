package api

import "sync"

// sessionLocks serializes generation changes and input for one logical session.
// Entries include waiters and are removed after the last holder leaves.
type sessionLocks struct {
	mu      sync.Mutex
	entries map[string]*sessionLock
}
type sessionLock struct {
	mu   sync.Mutex
	refs int
}

func (locks *sessionLocks) lock(id string) func() {
	locks.mu.Lock()
	if locks.entries == nil {
		locks.entries = make(map[string]*sessionLock)
	}
	entry := locks.entries[id]
	if entry == nil {
		entry = &sessionLock{}
		locks.entries[id] = entry
	}
	entry.refs++
	locks.mu.Unlock()
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		locks.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(locks.entries, id)
		}
		locks.mu.Unlock()
	}
}
