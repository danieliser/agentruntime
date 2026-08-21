package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/session"
	"github.com/danieliser/agentruntime/pkg/session/agentsessions"
)

type resolvedResumeSession struct {
	ProviderID      string
	VolumeName      string
	SourceSessionID string
	PortableStateID string
}

type providerVolumePlan struct {
	// Name is persisted on the logical AgentD session for later continuation.
	Name string
	// ExistingName is passed to the runtime only when the volume must already
	// exist. An empty value tells Docker to create the first-generation volume.
	ExistingName string
}

func planProviderVolume(sessionID string, persist bool, existingNames ...string) providerVolumePlan {
	if !persist {
		return providerVolumePlan{}
	}
	for _, existingName := range existingNames {
		if existingName != "" {
			return providerVolumePlan{Name: existingName, ExistingName: existingName}
		}
	}
	return providerVolumePlan{Name: "agentruntime-vol-" + sessionID}
}

func validateResolvedResumeState(runtimeName, requested string, resolved resolvedResumeSession) error {
	if runtimeName == "docker" && requested != "" && resolved.VolumeName == "" {
		return fmt.Errorf("Docker resume requires a logical AgentD session with persistent provider state")
	}
	return nil
}

func configureDockerProviderPersistence(request *SessionRequest, runtimeName string) {
	if request == nil || runtimeName != "docker" || !nativeV1Agent(request.Agent) || request.ExecutionPolicy != nil {
		return
	}
	request.PersistSession = true
}

func (s *Server) resolvePortableResumeState(request SessionRequest) (resolvedResumeSession, error) {
	if request.ResumeStateID == "" {
		return resolvedResumeSession{}, nil
	}
	if request.ResumeSession != "" {
		return resolvedResumeSession{}, fmt.Errorf("resume_state_id and resume_session are mutually exclusive")
	}
	if request.Runtime != "docker" || !nativeV1Agent(request.Agent) || request.ExecutionPolicy != nil {
		return resolvedResumeSession{}, fmt.Errorf("portable resume state requires an unrestricted native Docker session")
	}
	if s.resumeStates == nil {
		return resolvedResumeSession{}, fmt.Errorf("portable resume state store is unavailable")
	}
	manifest, err := s.resumeStates.Manifest(request.ResumeStateID)
	if err != nil {
		return resolvedResumeSession{}, err
	}
	if manifest.Agent != request.Agent {
		return resolvedResumeSession{}, fmt.Errorf("portable resume agent %q does not match %q", manifest.Agent, request.Agent)
	}
	return resolvedResumeSession{
		ProviderID: manifest.ProviderSessionID, SourceSessionID: manifest.SourceSessionID,
		PortableStateID: request.ResumeStateID,
	}, nil
}

// resolveResumeSession is the canonical provider-continuation resolver. A
// logical AgentD session ID is resolved from the durable generation ledger, so
// provider identity and Docker volume lineage survive daemon restarts.
func (s *Server) resolveResumeSession(ctx context.Context, agentName, sessionID string, original *session.Session) (resolvedResumeSession, error) {
	if sessionID == "" {
		return resolvedResumeSession{}, nil
	}
	if s.durableStore != nil {
		stored, err := s.durableStore.GetSession(ctx, sessionID)
		if err == nil {
			if stored.Agent != agentName {
				return resolvedResumeSession{}, fmt.Errorf("resume session agent %q does not match %q", stored.Agent, agentName)
			}
			if stored.ActiveGeneration < 1 {
				return resolvedResumeSession{}, fmt.Errorf("resume session %q has no provider generation", sessionID)
			}
			generation, err := s.durableStore.GetGeneration(ctx, stored.ID, stored.ActiveGeneration)
			if err != nil {
				return resolvedResumeSession{}, err
			}
			if generation.ProviderID == "" {
				return resolvedResumeSession{}, fmt.Errorf("resume session %q has no provider identity", sessionID)
			}
			resolved := resolvedResumeSession{ProviderID: generation.ProviderID, SourceSessionID: stored.ID}
			if stored.Runtime == "docker" {
				rootID, persistent, err := s.durableProviderStateRoot(ctx, stored)
				if err != nil {
					return resolvedResumeSession{}, err
				}
				if !persistent {
					return resolvedResumeSession{}, fmt.Errorf("Docker resume session %q has no persistent provider state", sessionID)
				}
				resolved.VolumeName = "agentruntime-vol-" + rootID
			}
			return resolved, nil
		}
		if !durable.IsCode(err, durable.CodeNotFound) {
			return resolvedResumeSession{}, err
		}
	}

	providerID, err := s.resolveLegacyResumeSessionID(agentName, sessionID, original)
	if err != nil {
		return resolvedResumeSession{}, err
	}
	resolved := resolvedResumeSession{ProviderID: providerID, SourceSessionID: sessionID}
	if original != nil {
		resolved.VolumeName = original.VolumeName
	}
	return resolved, nil
}

func (s *Server) durableProviderStateRoot(ctx context.Context, stored durable.Session) (string, bool, error) {
	visited := make(map[string]struct{})
	current := stored
	for depth := 0; depth < 64; depth++ {
		if _, duplicate := visited[current.ID]; duplicate {
			return "", false, fmt.Errorf("resume session lineage contains a cycle at %q", current.ID)
		}
		visited[current.ID] = struct{}{}
		var manifest struct {
			ResumeSession  string `json:"resume_session"`
			PersistSession bool   `json:"persist_session"`
		}
		if err := json.Unmarshal(current.RequestManifest, &manifest); err != nil {
			return "", false, fmt.Errorf("decode resume session %q manifest: %w", current.ID, err)
		}
		if manifest.ResumeSession == "" {
			return current.ID, manifest.PersistSession, nil
		}
		parent, err := s.durableStore.GetSession(ctx, manifest.ResumeSession)
		if durable.IsCode(err, durable.CodeNotFound) {
			return current.ID, manifest.PersistSession, nil
		}
		if err != nil {
			return "", false, err
		}
		if parent.Agent != stored.Agent || parent.Runtime != stored.Runtime {
			return "", false, fmt.Errorf("resume session lineage changes agent or runtime at %q", parent.ID)
		}
		current = parent
	}
	return "", false, fmt.Errorf("resume session lineage exceeds 64 generations")
}

func (s *Server) resolveLegacyResumeSessionID(agentName, sessionID string, original *session.Session) (string, error) {
	if original != nil {
		snapshot := original.Snapshot()
		if providerID := snapshot.Tags["claude_session_id"]; providerID != "" {
			return providerID, nil
		}
	}
	var (
		args []string
		err  error
	)
	switch agentName {
	case "claude":
		args, err = agentsessions.ClaudeResumeArgs(s.dataDir, sessionID)
	case "codex":
		args, err = agentsessions.CodexResumeArgs(s.dataDir, sessionID)
	default:
		return "", fmt.Errorf("resume_session is not supported for agent: %s", agentName)
	}
	if err != nil {
		return "", err
	}
	resolved, err := resumeSessionIDFromArgs(args)
	if err != nil {
		return "", err
	}
	if resolved == "" && original == nil {
		return sessionID, nil
	}
	return resolved, nil
}
