package session

import (
	"bytes"
	"sync"
	"testing"
)

func TestReplayBuffer_WriteAndRead(t *testing.T) {
	rb := NewReplayBuffer(64)
	data := []byte("hello world\n")
	rb.Write(data)

	got, next := rb.ReadFrom(0)
	if !bytes.Equal(got, data) {
		t.Fatalf("expected %q, got %q", data, got)
	}
	if next != int64(len(data)) {
		t.Fatalf("expected next=%d, got %d", len(data), next)
	}
}

func TestReplayBuffer_CircularEviction(t *testing.T) {
	size := 16
	rb := NewReplayBuffer(size)

	// Write 20 bytes — 4 bytes will be evicted.
	first := []byte("AAAA")               // will be evicted
	second := []byte("BBBBBBBBBBBBBBBB") // 16 bytes fills the buffer
	rb.Write(first)
	rb.Write(second)

	// total == 20, oldest == 4
	got, next := rb.ReadFrom(0) // offset 0 is too old, should be clamped to oldest
	if next != 20 {
		t.Fatalf("expected next=20, got %d", next)
	}
	if len(got) != size {
		t.Fatalf("expected %d bytes, got %d", size, len(got))
	}
	if !bytes.Equal(got, second) {
		t.Fatalf("expected %q, got %q", second, got)
	}
}

func TestReplayBuffer_OffsetCaughtUp(t *testing.T) {
	rb := NewReplayBuffer(64)
	rb.Write([]byte("some output\n"))

	total := rb.Total
	got, next := rb.ReadFrom(total)
	if got != nil {
		t.Fatalf("expected nil for caught-up offset, got %q", got)
	}
	if next != total {
		t.Fatalf("expected next=%d, got %d", total, next)
	}
}

func TestReplayBuffer_LargeGap(t *testing.T) {
	size := 8
	rb := NewReplayBuffer(size)

	// Write 16 bytes total — first 8 are evicted.
	rb.Write([]byte("12345678")) // bytes 0-7
	rb.Write([]byte("ABCDEFGH")) // bytes 8-15

	// Request from offset 0 (evicted) — should return from oldest available (offset 8).
	got, next := rb.ReadFrom(0)
	if next != 16 {
		t.Fatalf("expected next=16, got %d", next)
	}
	if !bytes.Equal(got, []byte("ABCDEFGH")) {
		t.Fatalf("expected oldest available data, got %q", got)
	}
}

func TestReplayBuffer_Concurrent(t *testing.T) {
	rb := NewReplayBuffer(1024)
	chunk := []byte("x")

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				rb.Write(chunk)
			}
		}()
	}

	// Concurrent reader.
	wg.Add(1)
	go func() {
		defer wg.Done()
		var off int64
		for i := 0; i < 50; i++ {
			_, off = rb.ReadFrom(off)
		}
	}()

	wg.Wait()

	if rb.Total != 1000 {
		t.Fatalf("expected total=1000, got %d", rb.Total)
	}
}

func TestReplayBuffer_ImplementsWriter(t *testing.T) {
	rb := NewReplayBuffer(64)
	n, err := rb.Write([]byte("test"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 4 {
		t.Fatalf("expected n=4, got %d", n)
	}
}
