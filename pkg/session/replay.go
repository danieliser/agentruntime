package session

import (
	"fmt"
	"io"
	"os"
	"sync"
)

const defaultReplayBufSize = 1 << 20 // 1 MiB

// ReplayBuffer is a bounded circular output buffer that allows reconnecting
// clients to replay missed output from a given byte offset. Thread-safe.
//
// Subscribers can call WaitFor(offset) to block until data at that offset
// is available, enabling push-based streaming without polling.
type ReplayBuffer struct {
	mu    sync.Mutex
	cond  *sync.Cond // broadcast on every Write — wakes WaitFor callers
	buf   []byte     // circular storage
	size  int        // capacity
	head  int        // next write position (wraps mod size)
	Total int64      // total bytes ever written (monotonic offset)
	done  bool       // set when the producer is finished (EOF)
}

// NewReplayBuffer creates a replay buffer with the given capacity in bytes.
func NewReplayBuffer(size int) *ReplayBuffer {
	return newReplayBuffer(size, true)
}

func newLazyReplayBuffer(size int) *ReplayBuffer {
	return newReplayBuffer(size, false)
}

func newReplayBuffer(size int, eager bool) *ReplayBuffer {
	if size <= 0 {
		size = defaultReplayBufSize
	}
	rb := &ReplayBuffer{size: size}
	rb.cond = sync.NewCond(&rb.mu)
	if eager {
		rb.buf = make([]byte, size)
	}
	return rb
}

func (r *ReplayBuffer) ensureBufferLocked() {
	if r.buf == nil {
		r.buf = make([]byte, r.size)
	}
}

// Write appends p to the ring buffer, overwriting oldest bytes if full.
// Wakes any goroutines blocked in WaitFor.
func (r *ReplayBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureBufferLocked()
	for _, b := range p {
		r.buf[r.head] = b
		r.head = (r.head + 1) % r.size
		r.Total++
	}
	if len(p) > 0 {
		r.cond.Broadcast()
	}
	return len(p), nil
}

// WriteOffset appends p and returns the new total byte offset atomically.
func (r *ReplayBuffer) WriteOffset(p []byte) (n int, offset int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureBufferLocked()
	for _, b := range p {
		r.buf[r.head] = b
		r.head = (r.head + 1) % r.size
		r.Total++
	}
	if len(p) > 0 {
		r.cond.Broadcast()
	}
	return len(p), r.Total
}

// TotalBytes returns the total number of bytes ever written to the buffer.
func (r *ReplayBuffer) TotalBytes() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Total
}

// Close marks the buffer as done — no more writes will come.
// Wakes all WaitFor callers so they can return.
func (r *ReplayBuffer) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.done = true
	r.cond.Broadcast()
}

// IsDone returns true if Close has been called.
func (r *ReplayBuffer) IsDone() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.done
}

// WaitFor blocks until the buffer has data beyond the given offset,
// or until the buffer is closed (no more data coming).
// Returns (data, nextOffset, done). If done is true, no more writes will come.
func (r *ReplayBuffer) WaitFor(offset int64) ([]byte, int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Wait until new data is available or buffer is closed.
	for r.Total <= offset && !r.done {
		r.cond.Wait()
	}

	if r.Total <= offset {
		return nil, r.Total, r.done
	}

	// Read from offset (same logic as ReadFrom but already under lock).
	oldest := r.Total - int64(r.size)
	if oldest < 0 {
		oldest = 0
	}
	if offset < oldest {
		offset = oldest
	}
	n := int(r.Total - offset)
	out := make([]byte, n)
	start := int(offset % int64(r.size))
	for i := 0; i < n; i++ {
		out[i] = r.buf[(start+i)%r.size]
	}
	return out, r.Total, r.done
}

// ReadFrom returns all bytes from offset onward (up to buffer capacity).
// Returns (data, nextOffset).
//   - If offset >= total, returns (nil, total) — client is caught up.
//   - If offset is too old (evicted), reads from the oldest available byte.
func (r *ReplayBuffer) ReadFrom(offset int64) ([]byte, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	oldest := r.Total - int64(r.size)
	if oldest < 0 {
		oldest = 0
	}
	if offset >= r.Total {
		return nil, r.Total // nothing new
	}
	if offset < oldest {
		offset = oldest // truncated — client missed some output
	}

	n := int(r.Total - offset)
	out := make([]byte, n)
	start := int(offset % int64(r.size))
	for i := 0; i < n; i++ {
		out[i] = r.buf[(start+i)%r.size]
	}
	return out, r.Total
}

// LoadFromFile streams file contents into the replay buffer.
func (r *ReplayBuffer) LoadFromFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open replay file: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(r, f); err != nil {
		return fmt.Errorf("load replay file: %w", err)
	}
	return nil
}
