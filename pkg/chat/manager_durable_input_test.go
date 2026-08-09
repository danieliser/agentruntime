package chat

import (
	"context"
	"testing"
)

func TestSendMessage_RunningChatUsesDurableNativeInputSender(t *testing.T) {
	handle := newFakeHandle()
	sess := makeSession("durable-running", nil)
	sess.SetRunning(handle)
	spawner := &nativeInputTestSpawner{fakeSpawner: newFakeSpawner(sess)}
	mgr, sessMgr := newTestManager(t, newFakeVolumeManager(), spawner.fakeSpawner)
	mgr.spawner = spawner
	sessMgr.Add(sess)
	mgr.CreateChat("durable-input", ChatConfig{Agent: "claude"})
	rec, _ := mgr.GetChat("durable-input")
	rec.State = ChatStateRunning
	rec.CurrentSession = sess.ID
	rec.SessionChain = []string{sess.ID}
	mgr.registry.Save(rec)

	if _, err := mgr.SendMessage("durable-input", "follow-up"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if spawner.sessionID != sess.ID || spawner.kind != "prompt" || spawner.text != "follow-up" || spawner.idempotencyKey == "" {
		t.Fatalf("durable input = %+v", spawner)
	}
	if got := handle.stdinBuf.String(); got != "" {
		t.Fatalf("durable native input leaked to raw stdin: %q", got)
	}
}

type nativeInputTestSpawner struct {
	*fakeSpawner
	sessionID      string
	idempotencyKey string
	kind           string
	text           string
}

func (spawner *nativeInputTestSpawner) SendSessionInput(_ context.Context, sessionID, idempotencyKey, kind, text string) error {
	spawner.sessionID, spawner.idempotencyKey = sessionID, idempotencyKey
	spawner.kind, spawner.text = kind, text
	return nil
}
