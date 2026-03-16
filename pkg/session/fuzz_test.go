package session

import (
	"testing"
)

// FuzzReplayBuffer feeds random data and offsets to the replay buffer.
// Must never panic regardless of input.
func FuzzReplayBuffer(f *testing.F) {
	f.Add(64, []byte("hello"), int64(0))
	f.Add(1, []byte("x"), int64(0))
	f.Add(8, []byte("12345678ABCDEFGH"), int64(4))
	f.Add(1024, []byte(""), int64(-1))
	f.Add(16, []byte("data"), int64(9999999))

	f.Fuzz(func(t *testing.T, size int, data []byte, offset int64) {
		if size <= 0 {
			size = 1 // NewReplayBuffer handles 0 → default, but we test small sizes
		}
		if size > 1<<20 {
			size = 1 << 20 // cap at 1MB to avoid OOM in fuzzing
		}

		rb := NewReplayBuffer(size)

		// Write must not panic.
		n, err := rb.Write(data)
		if err != nil {
			t.Errorf("Write returned error: %v", err)
		}
		if n != len(data) {
			t.Errorf("Write returned %d, want %d", n, len(data))
		}

		// ReadFrom must not panic for any offset.
		got, nextOffset := rb.ReadFrom(offset)

		// Invariant: nextOffset must equal Total after read.
		if nextOffset != rb.Total {
			t.Errorf("nextOffset=%d, Total=%d — must be equal", nextOffset, rb.Total)
		}

		// Invariant: returned data length must not exceed buffer size.
		if len(got) > size {
			t.Errorf("got %d bytes from buffer of size %d", len(got), size)
		}

		// Invariant: if offset >= Total, got must be nil.
		if offset >= rb.Total && got != nil {
			t.Errorf("offset >= Total but got non-nil data")
		}
	})
}
