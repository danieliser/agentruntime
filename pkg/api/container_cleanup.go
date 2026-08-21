package api

import (
	"context"
	"log"
	"time"

	"github.com/danieliser/agentruntime/pkg/runtime"
)

const (
	defaultStoppedContainerGrace    = 10 * time.Minute
	defaultStoppedContainerInterval = time.Minute
	stoppedContainerPruneTimeout    = 20 * time.Second
)

func (s *Server) startStoppedContainerCleanup() {
	s.cleanupOnce.Do(func() {
		s.cleanupWG.Add(1)
		go func() {
			defer s.cleanupWG.Done()
			s.pruneStoppedContainers(s.activationCtx, time.Now().UTC())
			ticker := time.NewTicker(defaultStoppedContainerInterval)
			defer ticker.Stop()
			for {
				select {
				case <-s.activationCtx.Done():
					return
				case now := <-ticker.C:
					s.pruneStoppedContainers(s.activationCtx, now.UTC())
				}
			}
		}()
	})
}

func (s *Server) pruneStoppedContainers(parent context.Context, now time.Time) int {
	removed := 0
	for name, rt := range s.runtimes {
		pruner, ok := rt.(runtime.StoppedContainerPruner)
		if !ok {
			continue
		}
		ctx, cancel := context.WithTimeout(parent, stoppedContainerPruneTimeout)
		count, err := pruner.PruneStoppedContainers(ctx, now.Add(-defaultStoppedContainerGrace))
		cancel()
		removed += count
		if err != nil {
			log.Printf("runtime %s stopped-container cleanup degraded: %v", name, err)
		}
	}
	if removed > 0 {
		log.Printf("removed %d expired stopped AgentD containers", removed)
	}
	return removed
}

func (s *Server) waitForStoppedContainerCleanup(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		s.cleanupWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}
