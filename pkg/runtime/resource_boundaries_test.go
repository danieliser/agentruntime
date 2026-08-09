package runtime_test

import (
	"bytes"
	"fmt"
	"testing"

	sessionpkg "github.com/danieliser/agentruntime/pkg/session"
)

func TestReplayBufferResourceBoundaryStaysCapped(t *testing.T) {
	const capacity = 1 << 20
	replay := sessionpkg.NewReplayBuffer(capacity)
	chunk := bytes.Repeat([]byte("z"), capacity)
	for index := 0; index < 100; index++ {
		if written, err := replay.Write(chunk); err != nil || written != len(chunk) {
			t.Fatalf("write %d = %d, %v", index, written, err)
		}
	}
	data, next := replay.ReadFrom(0)
	if next != int64(100*capacity) || len(data) != capacity || !bytes.Equal(data, chunk) {
		t.Fatalf("bounded replay next=%d retained=%d", next, len(data))
	}
}

func TestManagerResourceBoundaryHandlesOneThousandSessions(t *testing.T) {
	manager := sessionpkg.NewManager()
	for index := 0; index < 1000; index++ {
		sess := sessionpkg.NewSession(fmt.Sprintf("task-%d", index), "claude", "local")
		if err := manager.Add(sess); err != nil {
			t.Fatalf("add session %d: %v", index, err)
		}
	}
	if got := len(manager.List()); got != 1000 {
		t.Fatalf("session count = %d", got)
	}
}
