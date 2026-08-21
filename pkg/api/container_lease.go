package api

import (
	"fmt"
	"sync"
	"time"

	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
)

const maxContainerLeaseIdleTTL = 24 * time.Hour

type resolvedContainerLease struct {
	Maintain       bool
	IdleTTL        time.Duration
	PortableResume bool
}

func resolveContainerLease(request SessionRequest, runtimeName string, native bool) (resolvedContainerLease, error) {
	if request.ContainerLease == nil {
		return resolvedContainerLease{}, nil
	}
	mode := request.ContainerLease.Mode
	if mode == "" {
		mode = "delete"
	}
	if mode != "delete" && mode != "maintain" {
		return resolvedContainerLease{}, fmt.Errorf("container_lease.mode must be delete or maintain")
	}
	requestedLifecycle := mode == "maintain" || request.ContainerLease.PortableResume
	if requestedLifecycle && (runtimeName != "docker" || !native) {
		return resolvedContainerLease{}, fmt.Errorf("container leases require a provider-native Docker session")
	}
	if requestedLifecycle && request.ExecutionPolicy != nil {
		return resolvedContainerLease{}, fmt.Errorf("container leases are not available for restricted execution-policy sessions")
	}
	if mode == "delete" {
		if request.ContainerLease.IdleTTL != "" {
			return resolvedContainerLease{}, fmt.Errorf("container_lease.idle_ttl requires maintain mode")
		}
		return resolvedContainerLease{PortableResume: request.ContainerLease.PortableResume}, nil
	}
	if request.StructuredOutput != nil {
		return resolvedContainerLease{}, fmt.Errorf("maintained container leases do not yet support structured_output")
	}
	idleTTL, err := time.ParseDuration(request.ContainerLease.IdleTTL)
	if err != nil || idleTTL <= 0 {
		return resolvedContainerLease{}, fmt.Errorf("container_lease.idle_ttl must be a positive duration")
	}
	if idleTTL > maxContainerLeaseIdleTTL {
		return resolvedContainerLease{}, fmt.Errorf("container_lease.idle_ttl must not exceed %s", maxContainerLeaseIdleTTL)
	}
	return resolvedContainerLease{Maintain: true, IdleTTL: idleTTL, PortableResume: request.ContainerLease.PortableResume}, nil
}

// nativeContainerLease owns the idle-only timer for one maintained transport.
// A prompt atomically claims an idle transport and cancels its cleanup timer;
// the next provider turn completion rearms it.
type nativeContainerLease struct {
	mu        sync.Mutex
	idleTTL   time.Duration
	expire    func()
	timer     *time.Timer
	turnTTL   time.Duration
	turnEnd   func()
	turnTimer *time.Timer
	idle      bool
	closed    bool
	expiring  bool
	snapshot  bool
}

func newNativeContainerLease(idleTTL time.Duration, expire func()) *nativeContainerLease {
	return &nativeContainerLease{idleTTL: idleTTL, expire: expire}
}

// ConfigureTurnTimeout arms the initial provider turn and supplies the timer
// reused for every subsequent prompt admitted by this maintained transport.
func (lease *nativeContainerLease) ConfigureTurnTimeout(timeout time.Duration, startedAt time.Time, expire func()) {
	if lease == nil {
		return
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.closed || lease.expiring || timeout <= 0 {
		return
	}
	lease.turnTTL = timeout
	lease.turnEnd = expire
	lease.armTurnTimerLocked(time.Until(startedAt.Add(timeout)))
}

func (lease *nativeContainerLease) TurnCompleted() {
	if lease == nil {
		return
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.closed || lease.expiring {
		return
	}
	lease.stopTurnTimerLocked()
	lease.idle = true
	if lease.timer != nil {
		lease.timer.Stop()
	}
	lease.timer = time.AfterFunc(lease.idleTTL, lease.expireIdle)
}

func (lease *nativeContainerLease) BeginInput(kind string) error {
	if lease == nil {
		return nil
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.closed || lease.expiring || lease.snapshot {
		if lease.snapshot {
			return fmt.Errorf("portable resume snapshot is in progress")
		}
		return fmt.Errorf("maintained container lease is expiring")
	}
	switch nativeprotocol.InputKind(kind) {
	case nativeprotocol.InputPrompt:
		if !lease.idle {
			return fmt.Errorf("provider turn is still active; use steer instead of prompt")
		}
		lease.idle = false
		if lease.timer != nil {
			lease.timer.Stop()
			lease.timer = nil
		}
		lease.armTurnTimerLocked(lease.turnTTL)
	case nativeprotocol.InputSteer:
		if lease.idle {
			return fmt.Errorf("provider has no active turn; use prompt instead of steer")
		}
	}
	return nil
}

// BeginSnapshot atomically pins an idle maintained container while its
// provider volume is exported. The returned function restores the idle lease.
func (lease *nativeContainerLease) BeginSnapshot() (func(), error) {
	if lease == nil {
		return nil, fmt.Errorf("maintained container lease is unavailable")
	}
	lease.mu.Lock()
	if lease.closed || lease.expiring {
		lease.mu.Unlock()
		return nil, fmt.Errorf("maintained container lease is expiring")
	}
	if !lease.idle {
		lease.mu.Unlock()
		return nil, fmt.Errorf("provider turn is still active")
	}
	if lease.snapshot {
		lease.mu.Unlock()
		return nil, fmt.Errorf("portable resume snapshot is already in progress")
	}
	lease.snapshot = true
	if lease.timer != nil {
		lease.timer.Stop()
		lease.timer = nil
	}
	lease.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			lease.mu.Lock()
			defer lease.mu.Unlock()
			lease.snapshot = false
			if lease.closed || lease.expiring || !lease.idle {
				return
			}
			lease.timer = time.AfterFunc(lease.idleTTL, lease.expireIdle)
		})
	}, nil
}

func (lease *nativeContainerLease) IsIdle() bool {
	if lease == nil {
		return false
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	return lease.idle && !lease.closed && !lease.expiring && !lease.snapshot
}

func (lease *nativeContainerLease) AbortPrompt() {
	if lease == nil {
		return
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.closed || lease.expiring || lease.idle {
		return
	}
	lease.stopTurnTimerLocked()
	lease.idle = true
	lease.timer = time.AfterFunc(lease.idleTTL, lease.expireIdle)
}

func (lease *nativeContainerLease) armTurnTimerLocked(wait time.Duration) {
	lease.stopTurnTimerLocked()
	if lease.turnTTL <= 0 || lease.turnEnd == nil {
		return
	}
	if wait < 0 {
		wait = 0
	}
	lease.turnTimer = time.AfterFunc(wait, lease.expireTurn)
}

func (lease *nativeContainerLease) stopTurnTimerLocked() {
	if lease.turnTimer != nil {
		lease.turnTimer.Stop()
		lease.turnTimer = nil
	}
}

func (lease *nativeContainerLease) expireTurn() {
	lease.mu.Lock()
	if lease.closed || lease.expiring || lease.idle {
		lease.mu.Unlock()
		return
	}
	lease.expiring = true
	expire := lease.turnEnd
	lease.mu.Unlock()
	if expire != nil {
		expire()
	}
}

func (lease *nativeContainerLease) expireIdle() {
	lease.mu.Lock()
	if lease.closed || lease.expiring || !lease.idle || lease.snapshot {
		lease.mu.Unlock()
		return
	}
	lease.expiring = true
	expire := lease.expire
	lease.mu.Unlock()
	if expire != nil {
		expire()
	}
}

func (lease *nativeContainerLease) Close() {
	if lease == nil {
		return
	}
	lease.mu.Lock()
	lease.closed = true
	if lease.timer != nil {
		lease.timer.Stop()
		lease.timer = nil
	}
	lease.stopTurnTimerLocked()
	lease.mu.Unlock()
}
