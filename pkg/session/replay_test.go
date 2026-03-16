package session

import (
	"bytes"
	"math"
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
	first := []byte("AAAA")              // will be evicted
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

func TestReplayBuffer_ZeroSizeFallsBackToDefault(t *testing.T) {
	rb := NewReplayBuffer(0)
	if rb.size != defaultReplayBufSize {
		t.Fatalf("expected default size %d, got %d", defaultReplayBufSize, rb.size)
	}
	if len(rb.buf) != defaultReplayBufSize {
		t.Fatalf("expected buffer length %d, got %d", defaultReplayBufSize, len(rb.buf))
	}

	data := []byte("default-sized buffer still writes")
	if _, err := rb.Write(data); err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}

	got, next := rb.ReadFrom(0)
	if !bytes.Equal(got, data) {
		t.Fatalf("expected %q, got %q", data, got)
	}
	if next != int64(len(data)) {
		t.Fatalf("expected next=%d, got %d", len(data), next)
	}
}

func TestReplayBuffer_SingleByteBufferWraparound(t *testing.T) {
	rb := NewReplayBuffer(1)
	if _, err := rb.Write([]byte("abc")); err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}

	got, next := rb.ReadFrom(0)
	if !bytes.Equal(got, []byte("c")) {
		t.Fatalf("expected last byte only, got %q", got)
	}
	if next != 3 {
		t.Fatalf("expected next=3, got %d", next)
	}
}

func TestReplayBuffer_WriteLargerThanCapacitySingleCall(t *testing.T) {
	rb := NewReplayBuffer(8)
	data := []byte("abcdefghijklmnopqrst")

	n, err := rb.Write(data)
	if err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}
	if n != len(data) {
		t.Fatalf("expected n=%d, got %d", len(data), n)
	}

	got, next := rb.ReadFrom(0)
	want := data[len(data)-rb.size:]
	if !bytes.Equal(got, want) {
		t.Fatalf("expected %q, got %q", want, got)
	}
	if next != int64(len(data)) {
		t.Fatalf("expected next=%d, got %d", len(data), next)
	}
}

func TestReplayBuffer_ReadFromNegativeOffsetClampsToOldest(t *testing.T) {
	rb := NewReplayBuffer(16)
	data := []byte("hello")
	rb.Write(data)

	got, next := rb.ReadFrom(-123)
	if !bytes.Equal(got, data) {
		t.Fatalf("expected %q, got %q", data, got)
	}
	if next != int64(len(data)) {
		t.Fatalf("expected next=%d, got %d", len(data), next)
	}
}

func TestReplayBuffer_ReadFromMaxInt64Offset(t *testing.T) {
	rb := NewReplayBuffer(16)
	rb.Write([]byte("hello"))

	got, next := rb.ReadFrom(math.MaxInt64)
	if got != nil {
		t.Fatalf("expected nil for future offset, got %q", got)
	}
	if next != rb.Total {
		t.Fatalf("expected next=%d, got %d", rb.Total, next)
	}
}

func TestReplayBuffer_ConcurrentFullBufferWriters(t *testing.T) {
	const (
		size    = 32
		writers = 12
	)

	rb := NewReplayBuffer(size)
	payloads := make([][]byte, writers)
	start := make(chan struct{})

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		payload := bytes.Repeat([]byte{byte('A' + i)}, size)
		payloads[i] = payload

		wg.Add(1)
		go func(p []byte) {
			defer wg.Done()
			<-start
			if _, err := rb.Write(p); err != nil {
				t.Errorf("unexpected write error: %v", err)
			}
		}(payload)
	}

	close(start)
	wg.Wait()

	if rb.Total != int64(writers*size) {
		t.Fatalf("expected total=%d, got %d", writers*size, rb.Total)
	}

	got, next := rb.ReadFrom(0)
	if len(got) != size {
		t.Fatalf("expected %d bytes, got %d", size, len(got))
	}
	if next != rb.Total {
		t.Fatalf("expected next=%d, got %d", rb.Total, next)
	}

	for _, payload := range payloads {
		if bytes.Equal(got, payload) {
			return
		}
	}
	t.Fatalf("expected final buffer to match one writer payload, got %q", got)
}

func TestReplayBuffer_RapidAlternatingReadWriteManyGoroutines(t *testing.T) {
	const (
		size         = 64
		writers      = 64
		readers      = 64
		writeIters   = 200
		readIters    = 200
		totalWrites  = writers * writeIters
		totalWorkers = writers + readers
	)

	rb := NewReplayBuffer(size)
	start := make(chan struct{})
	errCh := make(chan string, totalWorkers)

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(b byte) {
			defer wg.Done()
			<-start
			for j := 0; j < writeIters; j++ {
				n, err := rb.Write([]byte{b})
				if err != nil {
					errCh <- "write returned error"
					return
				}
				if n != 1 {
					errCh <- "write returned short count"
					return
				}
			}
		}(byte('a' + (i % 26)))
	}

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			var off int64
			for j := 0; j < readIters; j++ {
				got, next := rb.ReadFrom(off)
				if next < off {
					errCh <- "next offset moved backwards"
					return
				}
				if len(got) > size {
					errCh <- "read exceeded buffer size"
					return
				}
				off = next
			}
		}()
	}

	close(start)
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatal(err)
	}

	if rb.Total != int64(totalWrites) {
		t.Fatalf("expected total=%d, got %d", totalWrites, rb.Total)
	}
}

func TestReplayBuffer_WriteEmptySliceNoOp(t *testing.T) {
	rb := NewReplayBuffer(16)
	headBefore := rb.head
	totalBefore := rb.Total

	n, err := rb.Write(nil)
	if err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected n=0, got %d", n)
	}
	if rb.head != headBefore {
		t.Fatalf("expected head=%d, got %d", headBefore, rb.head)
	}
	if rb.Total != totalBefore {
		t.Fatalf("expected total=%d, got %d", totalBefore, rb.Total)
	}

	got, next := rb.ReadFrom(0)
	if got != nil {
		t.Fatalf("expected nil after empty write, got %q", got)
	}
	if next != 0 {
		t.Fatalf("expected next=0, got %d", next)
	}
}

func TestReplayBuffer_NearInt64Boundary(t *testing.T) {
	rb := NewReplayBuffer(4)
	rb.Total = math.MaxInt64 - 2
	rb.head = int(rb.Total % int64(rb.size))

	n, err := rb.Write([]byte("YZ"))
	if err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected n=2, got %d", n)
	}
	if rb.Total != math.MaxInt64 {
		t.Fatalf("expected total=%d, got %d", int64(math.MaxInt64), rb.Total)
	}

	got, next := rb.ReadFrom(math.MaxInt64 - 2)
	if !bytes.Equal(got, []byte("YZ")) {
		t.Fatalf("expected %q, got %q", []byte("YZ"), got)
	}
	if next != math.MaxInt64 {
		t.Fatalf("expected next=%d, got %d", int64(math.MaxInt64), next)
	}
}

func TestReplayBuffer_ReadFromImmediatelyAfterConstruction(t *testing.T) {
	rb := NewReplayBuffer(16)

	got, next := rb.ReadFrom(0)
	if got != nil {
		t.Fatalf("expected nil with no writes, got %q", got)
	}
	if next != 0 {
		t.Fatalf("expected next=0, got %d", next)
	}
}
