package api

import (
	"log"
	"sync"

	"github.com/danieliser/agentruntime/pkg/session"
)

// AttachSessionIO starts stdout/stderr drain goroutines and an exit watcher for
// a session handle, mirroring the normal create-session lifecycle.
func AttachSessionIO(sess *session.Session, logDir string) {
	if sess == nil || sess.Handle == nil {
		return
	}

	logw, err := session.NewLogWriter(logDir, sess.ID)
	if err != nil {
		log.Printf("[session %s] warning: log file creation failed: %v", sess.ID, err)
	}
	drainTarget := session.DrainWriter(sess.Replay, logw)

	handle := sess.Handle
	stdout := handle.Stdout()
	stderr := handle.Stderr()

	var drainWg sync.WaitGroup
	if stdout != nil {
		drainWg.Add(1)
		go func() {
			defer drainWg.Done()
			drainTo(sess.ID, "stdout", stdout, drainTarget)
		}()
	}
	if stderr != nil {
		drainWg.Add(1)
		go func() {
			defer drainWg.Done()
			drainTo(sess.ID, "stderr", stderr, drainTarget)
		}()
	}

	go func() {
		result := <-handle.Wait()
		drainWg.Wait()
		sess.Replay.Close()
		if logw != nil {
			if err := logw.Close(); err != nil {
				log.Printf("[session %s] warning: close log failed: %v", sess.ID, err)
			} else {
				log.Printf("[session %s] log saved: %s", sess.ID, logw.Path())
			}
		}
		log.Printf("[session %s] exited: code=%d err=%v replay_bytes=%d", sess.ID, result.Code, result.Err, sess.Replay.TotalBytes())
		sess.SetCompleted(result.Code)
	}()
}
