package session

import "sync"

const defaultReplayBufSize = 1 << 20 // 1 MiB

// ReplayBuffer is a bounded circular output buffer that allows reconnecting
// clients to replay missed output from a given byte offset. Thread-safe.
type ReplayBuffer struct {
	mu    sync.Mutex
	buf   []byte // circular storage
	size  int    // capacity
	head  int    // next write position (wraps mod size)
	Total int64  // total bytes ever written (monotonic offset)
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
func (r *ReplayBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureBufferLocked()
	for _, b := range p {
		r.buf[r.head] = b
		r.head = (r.head + 1) % r.size
		r.Total++
	}
	return len(p), nil
}

// WriteOffset appends p and returns the new total byte offset atomically.
// Use this instead of Write + Total when the offset is needed, to avoid
// a race between the write and the read of Total.
func (r *ReplayBuffer) WriteOffset(p []byte) (n int, offset int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureBufferLocked()
	for _, b := range p {
		r.buf[r.head] = b
		r.head = (r.head + 1) % r.size
		r.Total++
	}
	return len(p), r.Total
}

// TotalBytes returns the total number of bytes ever written to the buffer.
func (r *ReplayBuffer) TotalBytes() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Total
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
