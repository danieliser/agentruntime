package observer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/danieliser/agentruntime/pkg/durable"
)

type HealthState string

const (
	HealthHealthy  HealthState = "healthy"
	HealthDegraded HealthState = "degraded"
	HealthDown     HealthState = "down"
)

var ErrPluginUnavailable = errors.New("observer plugin unavailable")

type PluginStatus struct {
	Name           string      `json:"name"`
	Version        string      `json:"version,omitempty"`
	Policy         Policy      `json:"policy"`
	State          HealthState `json:"state"`
	LastError      string      `json:"last_error,omitempty"`
	Unacknowledged int64       `json:"unacknowledged_events"`
	CheckedAt      time.Time   `json:"checked_at,omitempty"`
}

type TraceLink struct {
	Plugin               string `json:"plugin"`
	SessionID            string `json:"session_id"`
	TraceID              string `json:"trace_id"`
	AcknowledgedSequence int64  `json:"acknowledged_sequence"`
}

type Manager struct {
	store              durable.Store
	dataDir            string
	agentdVersion      string
	eventSchemaVersion string
	workers            []*pluginWorker
	byName             map[string]*pluginWorker
	startOnce          sync.Once
	cancel             context.CancelFunc
	wg                 sync.WaitGroup
}

type pluginWorker struct {
	config      PluginConfig
	checkpoints *CheckpointStore
	process     *Process
	wake        chan struct{}
	syncMu      sync.Mutex
	mu          sync.RWMutex
	status      PluginStatus
	links       map[string]TraceLink
}

func NewManager(dataDir string, store durable.Store, config Config, agentdVersion, eventSchemaVersion string) (*Manager, error) {
	if dataDir == "" || store == nil || eventSchemaVersion == "" {
		return nil, fmt.Errorf("observer: data directory, durable store, and event schema are required")
	}
	manager := &Manager{
		store: store, dataDir: dataDir, agentdVersion: agentdVersion,
		eventSchemaVersion: eventSchemaVersion, byName: make(map[string]*pluginWorker),
	}
	for _, plugin := range config.Plugins {
		if !plugin.Enabled {
			continue
		}
		if _, exists := manager.byName[plugin.Name]; exists {
			return nil, fmt.Errorf("observer: duplicate plugin %q", plugin.Name)
		}
		checkpoints, err := NewCheckpointStore(dataDir, plugin.Name)
		if err != nil {
			return nil, err
		}
		worker := &pluginWorker{
			config: plugin, checkpoints: checkpoints, wake: make(chan struct{}, 1), links: make(map[string]TraceLink),
			status: PluginStatus{Name: plugin.Name, Policy: plugin.Policy, State: HealthDown, LastError: "not started"},
		}
		manager.workers = append(manager.workers, worker)
		manager.byName[plugin.Name] = worker
	}
	return manager, nil
}

func (manager *Manager) Start(parent context.Context) {
	manager.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(parent)
		manager.cancel = cancel
		for _, worker := range manager.workers {
			manager.wg.Add(1)
			go manager.runWorker(ctx, worker)
		}
	})
}

func (manager *Manager) runWorker(ctx context.Context, worker *pluginWorker) {
	defer manager.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		_ = manager.syncWorker(ctx, worker)
		select {
		case <-ctx.Done():
			return
		case <-worker.wake:
		case <-ticker.C:
		}
	}
}

// Notify is intentionally non-blocking. The ledger remains authoritative, so
// coalesced wakeups or daemon restarts are recovered by the next full scan.
func (manager *Manager) Notify(_ durable.Event) {
	for _, worker := range manager.workers {
		select {
		case worker.wake <- struct{}{}:
		default:
		}
	}
}

func (manager *Manager) Sync(ctx context.Context) error {
	var failures []error
	for _, worker := range manager.workers {
		if err := manager.syncWorker(ctx, worker); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (manager *Manager) syncWorker(ctx context.Context, worker *pluginWorker) error {
	worker.syncMu.Lock()
	defer worker.syncMu.Unlock()
	if worker.process == nil || !worker.process.Running() {
		process, err := StartProcess(ctx, worker.config, manager.agentdVersion, manager.eventSchemaVersion)
		if err != nil {
			manager.setFailure(worker, HealthDown, err, manager.lag(ctx, worker))
			return err
		}
		worker.process = process
		worker.mu.Lock()
		worker.status.Version = process.Identity().Version
		worker.mu.Unlock()
	}
	if err := worker.process.Health(ctx); err != nil {
		manager.setFailure(worker, HealthDegraded, err, manager.lag(ctx, worker))
		return err
	}
	sessions, err := manager.store.ListSessions(ctx)
	if err != nil {
		manager.setFailure(worker, HealthDegraded, err, 0)
		return err
	}
	for _, session := range sessions {
		if err := manager.syncSession(ctx, worker, session); err != nil {
			state := HealthDegraded
			if worker.process == nil || !worker.process.Running() {
				state = HealthDown
			}
			manager.setFailure(worker, state, err, manager.lag(ctx, worker))
			return err
		}
	}
	worker.mu.Lock()
	worker.status.State = HealthHealthy
	worker.status.LastError = ""
	worker.status.Unacknowledged = manager.lag(ctx, worker)
	worker.status.CheckedAt = time.Now().UTC()
	worker.mu.Unlock()
	return nil
}

func (manager *Manager) syncSession(ctx context.Context, worker *pluginWorker, session durable.Session) error {
	checkpoint, err := worker.checkpoints.Load(session.ID)
	if errors.Is(err, ErrCheckpointNotFound) {
		checkpoint = Checkpoint{SessionID: session.ID}
	} else if err != nil {
		return err
	}
	if checkpoint.Sequence > 0 {
		if checkpoint.Sequence > session.LastSequence {
			return fmt.Errorf("observer: checkpoint %d is beyond session %q tail %d", checkpoint.Sequence, session.ID, session.LastSequence)
		}
		event, err := manager.store.GetEventByID(ctx, checkpoint.EventID)
		if err != nil || event.SessionID != session.ID || event.Sequence != checkpoint.Sequence {
			return fmt.Errorf("observer: checkpoint identity is not present in the durable ledger: %w", err)
		}
		if checkpoint.TraceID != "" {
			worker.mu.Lock()
			worker.links[session.ID] = TraceLink{Plugin: worker.config.Name, SessionID: session.ID, TraceID: checkpoint.TraceID, AcknowledgedSequence: checkpoint.Sequence}
			worker.mu.Unlock()
		}
	}
	for {
		page, err := manager.store.ListEvents(ctx, durable.EventQuery{SessionID: session.ID, AfterSequence: checkpoint.Sequence, Limit: 256})
		if err != nil {
			return err
		}
		for _, event := range page.Events {
			generation, err := manager.store.GetGeneration(ctx, session.ID, event.Generation)
			if err != nil {
				return err
			}
			eventContext := EventContext{
				JobID: session.IdempotencyKey, Agent: session.Agent, Runtime: session.Runtime,
				RequestManifest: append([]byte(nil), session.RequestManifest...), SecretGrants: append([]string(nil), session.SecretGrants...),
				ProviderSessionID: generation.ProviderID, ImageReference: generation.ImageReference,
				ImageDigest: generation.ImageDigest, SandboxProfile: generation.SandboxProfile,
			}
			ack, err := worker.process.DeliverWithContext(ctx, event, eventContext)
			if err != nil {
				return err
			}
			traceID, err := stableTraceID(checkpoint.TraceID, ack.TraceID)
			if err != nil {
				return err
			}
			checkpoint = Checkpoint{SessionID: session.ID, Sequence: event.Sequence, EventID: event.EventID, TraceID: traceID}
			if err := worker.checkpoints.Advance(checkpoint); err != nil {
				return err
			}
			worker.mu.Lock()
			worker.links[session.ID] = TraceLink{Plugin: worker.config.Name, SessionID: session.ID, TraceID: traceID, AcknowledgedSequence: event.Sequence}
			worker.mu.Unlock()
		}
		if !page.HasMore {
			return nil
		}
	}
}

func stableTraceID(current, acknowledged string) (string, error) {
	if acknowledged == "" {
		if current == "" {
			return "", fmt.Errorf("observer: trace-linked acknowledgement omitted trace ID")
		}
		return current, nil
	}
	if _, err := uuid.Parse(acknowledged); err != nil {
		return "", fmt.Errorf("observer: invalid trace ID: %w", err)
	}
	if current != "" && current != acknowledged {
		return "", fmt.Errorf("observer: trace linkage changed from %q to %q", current, acknowledged)
	}
	return acknowledged, nil
}

func (manager *Manager) lag(ctx context.Context, worker *pluginWorker) int64 {
	sessions, err := manager.store.ListSessions(ctx)
	if err != nil {
		return 0
	}
	var lag int64
	for _, session := range sessions {
		checkpoint, err := worker.checkpoints.Load(session.ID)
		if errors.Is(err, ErrCheckpointNotFound) {
			lag += session.LastSequence
			continue
		}
		if err == nil && session.LastSequence > checkpoint.Sequence {
			lag += session.LastSequence - checkpoint.Sequence
		}
	}
	return lag
}

func (manager *Manager) setFailure(worker *pluginWorker, state HealthState, err error, lag int64) {
	worker.mu.Lock()
	defer worker.mu.Unlock()
	worker.status.State = state
	worker.status.LastError = err.Error()
	worker.status.Unacknowledged = lag
	worker.status.CheckedAt = time.Now().UTC()
}

func (manager *Manager) Status() []PluginStatus {
	statuses := make([]PluginStatus, 0, len(manager.workers))
	for _, worker := range manager.workers {
		worker.mu.RLock()
		statuses = append(statuses, worker.status)
		worker.mu.RUnlock()
	}
	return statuses
}

func (manager *Manager) TraceLink(pluginName, sessionID string) (TraceLink, bool) {
	worker := manager.byName[pluginName]
	if worker == nil {
		return TraceLink{}, false
	}
	worker.mu.RLock()
	defer worker.mu.RUnlock()
	link, ok := worker.links[sessionID]
	return link, ok
}

func (manager *Manager) RequireHealthy(pluginName string) error {
	worker := manager.byName[pluginName]
	if worker == nil {
		return fmt.Errorf("%w: %s", ErrPluginUnavailable, pluginName)
	}
	worker.mu.RLock()
	defer worker.mu.RUnlock()
	if worker.status.State != HealthHealthy {
		return fmt.Errorf("%w: %s is %s", ErrPluginUnavailable, pluginName, worker.status.State)
	}
	return nil
}

func (manager *Manager) Policy(pluginName string) (Policy, bool) {
	worker := manager.byName[pluginName]
	if worker == nil {
		return "", false
	}
	return worker.config.Policy, true
}

func (manager *Manager) Close(ctx context.Context) error {
	if manager.cancel != nil {
		manager.cancel()
	}
	manager.wg.Wait()
	var failures []error
	for _, worker := range manager.workers {
		worker.syncMu.Lock()
		if worker.process != nil {
			if err := worker.process.Flush(ctx); err != nil {
				failures = append(failures, err)
			}
			if err := worker.process.Close(ctx); err != nil {
				failures = append(failures, err)
			}
		}
		worker.syncMu.Unlock()
	}
	return errors.Join(failures...)
}
