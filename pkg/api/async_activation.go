package api

import (
	"context"
	"log"
	"time"

	"github.com/danieliser/agentruntime/pkg/durable"
)

func (s *Server) launchAdmittedNativeSession(request SessionRequest) {
	sessionID := request.AdmittedSessionID
	if sessionID == "" {
		sessionID = request.SessionID
	}
	s.progress.publish(sessionID, "admitted", "durable session admitted", false)
	s.activationMu.Lock()
	s.activations++
	s.activationMu.Unlock()
	go func() {
		defer func() {
			s.activationMu.Lock()
			s.activations--
			s.activationMu.Unlock()
		}()
		_, stored, err := s.spawnDurableSession(s.activationCtx, request)
		if err == nil {
			return
		}
		log.Printf("[session %s] asynchronous activation failed: %v", sessionID, err)
		s.progress.publish(sessionID, "failed", "runtime activation failed", true)
		if stored.ID != "" && stored.State.Terminal() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, settleErr := s.settleAdmittedSession(ctx, sessionID, durable.StateFailed, "failed", err); settleErr != nil {
			log.Printf("[session %s] asynchronous activation settlement failed: %v", sessionID, settleErr)
		}
	}()
}

func (s *Server) waitForActivationDrain(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		s.activationMu.Lock()
		active := s.activations
		s.activationMu.Unlock()
		if active == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
