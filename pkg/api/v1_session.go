package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

type v1SessionData struct {
	SessionID      string               `json:"session_id"`
	IdempotencyKey string               `json:"idempotency_key"`
	Agent          string               `json:"agent"`
	Runtime        string               `json:"runtime"`
	State          durable.SessionState `json:"state"`
	Generation     int64                `json:"generation"`
	LastSequence   int64                `json:"last_sequence"`
	EventsURL      string               `json:"events_url"`
	EventStreamURL string               `json:"event_stream_url"`
}

type v1TerminalReceiptData struct {
	SessionID    string               `json:"session_id"`
	Generation   int64                `json:"generation"`
	State        durable.SessionState `json:"state"`
	ExitCode     *int                 `json:"exit_code,omitempty"`
	Signal       string               `json:"signal,omitempty"`
	StartedAt    time.Time            `json:"started_at"`
	EndedAt      time.Time            `json:"ended_at"`
	OutputHash   string               `json:"output_hash"`
	ArtifactHash string               `json:"artifact_hash,omitempty"`
	LastSequence int64                `json:"last_sequence"`
}

func (s *Server) handleV1CreateSession(c *gin.Context) {
	var request SessionRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": apiErrorEnvelope{Code: durable.CodeInvalidArgument, Message: err.Error()}})
		return
	}
	s.createSession(c, request, true)
}

func (s *Server) handleV1GetSession(c *gin.Context) {
	if s.durableStore == nil {
		writeDurableError(c, durable.NewError(durable.CodeIndeterminate, "get_v1_session", "durable session store unavailable", nil))
		return
	}
	stored, err := s.durableStore.GetSession(c.Request.Context(), c.Param("id"))
	if err != nil {
		writeDurableError(c, err)
		return
	}
	s.writeV1Session(c, http.StatusOK, stored)
}

func (s *Server) handleV1GetTerminalReceipt(c *gin.Context) {
	if s.durableStore == nil {
		writeDurableError(c, durable.NewError(durable.CodeIndeterminate, "get_v1_receipt", "durable session store unavailable", nil))
		return
	}
	receipt, err := s.durableStore.GetTerminalReceipt(c.Request.Context(), c.Param("id"))
	if err != nil {
		writeDurableError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"api_version": "v1", "data": v1TerminalReceiptData{
		SessionID: receipt.SessionID, Generation: receipt.Generation, State: receipt.State,
		ExitCode: receipt.ExitCode, Signal: receipt.Signal, StartedAt: receipt.StartedAt, EndedAt: receipt.EndedAt,
		OutputHash: receipt.OutputHash, ArtifactHash: receipt.ArtifactHash, LastSequence: receipt.LastSequence,
	}})
}

func generationDockerLogDriver(runtimeName string, native bool) string {
	if runtimeName == "docker" && native {
		return "json-file"
	}
	return ""
}

func nativeV1Agent(agentName string) bool {
	return agentName == string(nativeprotocol.ProviderClaude) || agentName == string(nativeprotocol.ProviderCodex)
}

func runtimeSandboxProfile(runtimeName string, native bool) string {
	transport := "compat"
	if native {
		transport = "native"
	}
	return runtimeName + "-" + transport + "-v1"
}

func (s *Server) admitV1Session(ctx context.Context, request SessionRequest, runtimeName string) (durable.CreateSessionResult, error) {
	const op = "admit_v1_session"
	if s.durableStore == nil {
		return durable.CreateSessionResult{}, durable.NewError(durable.CodeIndeterminate, op, "durable session store unavailable", nil)
	}
	if request.IdempotencyKey == "" {
		return durable.CreateSessionResult{}, durable.NewError(durable.CodeInvalidArgument, op, "idempotency_key is required", nil)
	}
	manifest, grants, requestHash, err := durableRequestManifest(request, runtimeName)
	if err != nil {
		return durable.CreateSessionResult{}, err
	}
	sessionID := request.SessionID
	if sessionID == "" {
		sessionID = uuid.NewString()
	}
	return s.durableStore.CreateSession(ctx, durable.CreateSessionParams{
		SessionID: sessionID, IdempotencyKey: request.IdempotencyKey,
		RequestHash: requestHash, RequestManifest: manifest, SecretGrants: grants,
		Agent: request.Agent, Runtime: runtimeName, CreatedAt: time.Now().UTC(),
	})
}

func durableRequestManifest(request SessionRequest, runtimeName string) (json.RawMessage, []string, string, error) {
	const op = "build_durable_request_manifest"
	raw, err := json.Marshal(request)
	if err != nil {
		return nil, nil, "", durable.NewError(durable.CodeInvalidArgument, op, "encode request", err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, nil, "", durable.NewError(durable.CodeInvalidArgument, op, "decode request manifest", err)
	}
	delete(manifest, "idempotency_key")
	delete(manifest, "secret_grants")
	manifest["runtime"] = runtimeName

	grants := append([]string(nil), request.SecretGrants...)
	sort.Strings(grants)
	grantSet := make(map[string]struct{}, len(grants))
	for index, grant := range grants {
		if grant == "" {
			return nil, nil, "", durable.NewError(durable.CodeInvalidArgument, op, "secret grant names must not be empty", nil)
		}
		if index > 0 && grants[index-1] == grant {
			return nil, nil, "", durable.NewError(durable.CodeInvalidArgument, op, "secret grant names must be unique", nil)
		}
		if _, exists := request.Env[grant]; !exists {
			return nil, nil, "", durable.NewError(durable.CodeInvalidArgument, op, fmt.Sprintf("secret grant %q is not present in env", grant), nil)
		}
		grantSet[grant] = struct{}{}
	}
	for name := range request.Env {
		if _, granted := grantSet[name]; !granted && sensitiveEnvironmentName(name) {
			return nil, nil, "", durable.NewError(durable.CodeInvalidArgument, op, fmt.Sprintf("environment variable %q must be declared in secret_grants", name), nil)
		}
	}
	if env, ok := manifest["env"].(map[string]any); ok {
		for _, grant := range grants {
			delete(env, grant)
		}
		if len(env) == 0 {
			delete(manifest, "env")
		}
	}
	for key, value := range manifest {
		if key == "env" {
			continue
		}
		scrubManifestSecrets(value, key, &grants)
	}
	sort.Strings(grants)
	manifestRaw, err := json.Marshal(manifest)
	if err != nil {
		return nil, nil, "", durable.NewError(durable.CodeInvalidArgument, op, "encode canonical manifest", err)
	}
	grantsRaw, err := json.Marshal(grants)
	if err != nil {
		return nil, nil, "", durable.NewError(durable.CodeInvalidArgument, op, "encode canonical grants", err)
	}
	digest := sha256.New()
	_, _ = digest.Write(manifestRaw)
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(grantsRaw)
	return manifestRaw, grants, "sha256:" + hex.EncodeToString(digest.Sum(nil)), nil
}

func sensitiveEnvironmentName(name string) bool {
	upper := strings.ToUpper(strings.TrimSpace(name))
	for _, exact := range []string{"TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "CREDENTIALS", "PRIVATE_KEY", "API_KEY"} {
		if upper == exact {
			return true
		}
	}
	for _, suffix := range []string{"_TOKEN", "_SECRET", "_PASSWORD", "_CREDENTIAL", "_CREDENTIALS", "_PRIVATE_KEY", "_API_KEY", "_KEY"} {
		if strings.HasSuffix(upper, suffix) {
			return true
		}
	}
	return upper == "KEY"
}

func scrubManifestSecrets(value any, path string, grants *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			childPath := path + "." + key
			if sensitiveEnvironmentName(key) {
				if secret := typed[key]; secret != nil && secret != "" {
					delete(typed, key)
					*grants = append(*grants, "request:"+childPath)
				}
				continue
			}
			scrubManifestSecrets(typed[key], childPath, grants)
		}
	case []any:
		for index, item := range typed {
			scrubManifestSecrets(item, fmt.Sprintf("%s[%d]", path, index), grants)
		}
	}
}

func (s *Server) writeV1Session(c *gin.Context, status int, stored durable.Session) {
	c.JSON(status, gin.H{
		"api_version": "v1",
		"data": v1SessionData{
			SessionID: stored.ID, IdempotencyKey: stored.IdempotencyKey,
			Agent: stored.Agent, Runtime: stored.Runtime, State: stored.State,
			Generation: stored.ActiveGeneration, LastSequence: stored.LastSequence,
			EventsURL:      sessionEventsURL(c, stored.ID),
			EventStreamURL: sessionEventStreamURL(c, stored.ID),
		},
	})
}

func sessionEventsURL(c *gin.Context, sessionID string) string {
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/api/v1/sessions/%s/events", scheme, c.Request.Host, sessionID)
}

func sessionEventStreamURL(c *gin.Context, sessionID string) string {
	scheme := "ws"
	if c.Request.TLS != nil {
		scheme = "wss"
	}
	return fmt.Sprintf("%s://%s/api/v1/ws/sessions/%s/events", scheme, c.Request.Host, sessionID)
}

func runtimeGenerationIdentity(handle runtime.ProcessHandle, runtimeName, sessionID string, generation int64) string {
	identity := ""
	if identified, ok := handle.(runtime.RuntimeIdentifiedHandle); ok {
		identity = identified.RuntimeID()
	}
	if identity == "" {
		if pid := handle.PID(); pid > 0 {
			identity = fmt.Sprintf("pid:%d", pid)
		}
	}
	if identity == "" {
		return ""
	}
	if runtimeName == "docker" {
		return identity
	}
	return fmt.Sprintf("%s:%s:g%d:%s", runtimeName, sessionID, generation, identity)
}

func runtimeGenerationImageDigest(handle runtime.ProcessHandle) string {
	if identified, ok := handle.(runtime.RuntimeImageIdentifiedHandle); ok {
		return identified.RuntimeImageDigest()
	}
	return ""
}

func resolvedImageReference(request SessionRequest, runtimeName string) string {
	if runtimeName != "docker" {
		return "local-process"
	}
	if request.Container != nil && request.Container.Image != "" {
		return request.Container.Image
	}
	return runtime.DefaultDockerImage
}

func outputHash(path string) string {
	digest := sha256.New()
	if file, err := os.Open(path); err == nil {
		_, _ = io.Copy(digest, file)
		_ = file.Close()
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil))
}

func (s *Server) finalizeV1Session(sessionID string, result runtime.ExitResult, streamErrors ...error) {
	s.finalizeV1SessionAs(sessionID, result, "", streamErrors...)
}

func runtimeTerminalState(result runtime.ExitResult) durable.SessionState {
	if result.OOMKilled || result.Signal != "" || result.ErrorDetail != "" || result.Err != nil {
		return durable.StateCrashed
	}
	if result.Code != 0 {
		return durable.StateFailed
	}
	return durable.StateCompleted
}

func (s *Server) finalizeV1SessionAs(sessionID string, result runtime.ExitResult, override durable.SessionState, streamErrors ...error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stored, err := s.durableStore.GetSession(ctx, sessionID)
	if err != nil {
		log.Printf("[session %s] durable finalization lookup failed: %v", sessionID, err)
		return
	}
	if stored.State.Terminal() || stored.ActiveGeneration < 1 {
		return
	}
	generation, err := s.durableStore.GetGeneration(ctx, sessionID, stored.ActiveGeneration)
	if err != nil {
		log.Printf("[session %s] durable generation lookup failed: %v", sessionID, err)
		return
	}
	state := runtimeTerminalState(result)
	generationTo := durable.GenerationExited
	streamErr := errors.Join(streamErrors...)
	if streamErr != nil {
		state = durable.StateIndeterminate
		generationTo = durable.GenerationIndeterminate
	}
	if override != "" {
		state = override
		generationTo = durable.GenerationExited
		if override == durable.StateIndeterminate {
			generationTo = durable.GenerationIndeterminate
		}
	}
	exitCode := result.Code
	startedAt := result.StartedAt
	if startedAt.IsZero() {
		startedAt = generation.CreatedAt
	}
	endedAt := result.EndedAt
	if endedAt.IsZero() {
		endedAt = time.Now().UTC()
	}
	if endedAt.Before(stored.UpdatedAt) {
		endedAt = stored.UpdatedAt
	}
	if endedAt.Before(generation.UpdatedAt) {
		endedAt = generation.UpdatedAt
	}
	_, err = s.durableStore.FinalizeSession(ctx, durable.FinalizeSessionParams{
		From: stored.State, GenerationFrom: generation.State, GenerationTo: generationTo,
		Receipt: durable.TerminalReceipt{
			SessionID: sessionID, Generation: generation.Number, State: state,
			ExitCode: &exitCode, Signal: result.Signal, StartedAt: startedAt, EndedAt: endedAt,
			OutputHash: outputHash(session.LogFilePath(s.logDir, sessionID)), LastSequence: stored.LastSequence,
		},
	})
	if err != nil {
		log.Printf("[session %s] durable finalization failed: %v", sessionID, err)
	}
}
