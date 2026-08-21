package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/runtime"
)

const PortableResumeContentType = "application/vnd.agentruntime.resume-state+zip"

type v1ResumeStateData struct {
	ResumeStateID       string    `json:"resume_state_id"`
	SchemaVersion       string    `json:"schema_version"`
	SourceSessionID     string    `json:"source_session_id"`
	Agent               string    `json:"agent"`
	ProviderSessionID   string    `json:"provider_session_id"`
	ProviderStateTarget string    `json:"provider_state_target"`
	ProviderStateSHA256 string    `json:"provider_state_sha256"`
	ImageReference      string    `json:"image_reference"`
	ImageDigest         string    `json:"image_digest"`
	CreatedAt           time.Time `json:"created_at"`
	DownloadURL         string    `json:"download_url"`
}

func (s *Server) handleV1CreateResumeState(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Minute)
	defer cancel()
	id, manifest, err := s.createPortableResumeState(ctx, c.Param("id"))
	if err != nil {
		writeDurableError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"api_version": "v1", "data": resumeStateView(id, manifest)})
}

func (s *Server) handleV1GetLatestResumeState(c *gin.Context) {
	if s.resumeStates == nil {
		writeDurableError(c, durable.NewError(durable.CodeIndeterminate, "get_latest_resume_state", "portable resume store unavailable", nil))
		return
	}
	id, manifest, err := s.resumeStates.Latest(c.Param("id"))
	if err != nil {
		code := durable.CodeIndeterminate
		if errors.Is(err, os.ErrNotExist) {
			code = durable.CodeNotFound
		}
		writeDurableError(c, durable.NewError(code, "get_latest_resume_state", "portable resume state not found", err))
		return
	}
	c.JSON(http.StatusOK, gin.H{"api_version": "v1", "data": resumeStateView(id, manifest)})
}

func (s *Server) handleV1UploadResumeState(c *gin.Context) {
	if s.resumeStates == nil {
		writeDurableError(c, durable.NewError(durable.CodeIndeterminate, "upload_resume_state", "portable resume store unavailable", nil))
		return
	}
	if c.ContentType() != PortableResumeContentType && c.ContentType() != "application/zip" {
		writeDurableError(c, durable.NewError(durable.CodeInvalidArgument, "upload_resume_state", "portable resume upload requires the AgentD resume-state or application/zip content type", nil))
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxPortableBundleBytes+1)
	id, manifest, err := s.resumeStates.Upload(c.Request.Context(), c.Request.Body)
	if err != nil {
		writeDurableError(c, durable.NewError(durable.CodeInvalidArgument, "upload_resume_state", "portable resume upload is invalid", err))
		return
	}
	c.JSON(http.StatusCreated, gin.H{"api_version": "v1", "data": resumeStateView(id, manifest)})
}

func (s *Server) handleV1DownloadResumeState(c *gin.Context) {
	if s.resumeStates == nil {
		writeDurableError(c, durable.NewError(durable.CodeIndeterminate, "download_resume_state", "portable resume store unavailable", nil))
		return
	}
	id := c.Param("id")
	file, err := s.resumeStates.Open(id)
	if err != nil {
		code := durable.CodeInvalidArgument
		if errors.Is(err, os.ErrNotExist) {
			code = durable.CodeNotFound
		}
		writeDurableError(c, durable.NewError(code, "download_resume_state", "portable resume state is unavailable", err))
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		writeDurableError(c, durable.NewError(durable.CodeIndeterminate, "download_resume_state", "inspect portable resume state", err))
		return
	}
	c.DataFromReader(http.StatusOK, info.Size(), PortableResumeContentType, file, map[string]string{
		"Content-Disposition":    fmt.Sprintf(`attachment; filename="%s.agentstate"`, id),
		"X-Content-Type-Options": "nosniff",
	})
}

func (s *Server) createPortableResumeState(ctx context.Context, sessionID string) (string, portableResumeManifest, error) {
	const op = "create_resume_state"
	if s.durableStore == nil || s.resumeStates == nil {
		return "", portableResumeManifest{}, durable.NewError(durable.CodeIndeterminate, op, "portable resume services unavailable", nil)
	}
	stored, err := s.durableStore.GetSession(ctx, sessionID)
	if err != nil {
		return "", portableResumeManifest{}, err
	}
	if stored.Runtime != "docker" || (stored.Agent != "codex" && stored.Agent != "claude") {
		return "", portableResumeManifest{}, durable.NewError(durable.CodeInvalidState, op, "portable resume requires a provider-native Docker session", nil)
	}
	if stored.ActiveGeneration < 1 {
		return "", portableResumeManifest{}, durable.NewError(durable.CodeInvalidState, op, "session has no runtime generation", nil)
	}
	generation, err := s.durableStore.GetGeneration(ctx, stored.ID, stored.ActiveGeneration)
	if err != nil {
		return "", portableResumeManifest{}, err
	}
	if generation.ProviderID == "" || generation.ImageReference == "" || generation.ImageDigest == "" {
		return "", portableResumeManifest{}, durable.NewError(durable.CodeInvalidState, op, "session is missing provider or image provenance", nil)
	}
	rootID, persistent, err := s.durableProviderStateRoot(ctx, stored)
	if err != nil {
		return "", portableResumeManifest{}, durable.NewError(durable.CodeIndeterminate, op, "resolve provider state volume", err)
	}
	if !persistent {
		return "", portableResumeManifest{}, durable.NewError(durable.CodeInvalidState, op, "session did not retain provider state", nil)
	}

	var releaseSnapshot func()
	if !stored.State.Terminal() {
		if stored.State != durable.StateRunning {
			return "", portableResumeManifest{}, durable.NewError(durable.CodeInvalidState, op, "session is not snapshot-ready", nil)
		}
		active := s.nativeSession(stored.ID)
		if active == nil || active.lease == nil {
			return "", portableResumeManifest{}, durable.NewError(durable.CodeInvalidState, op, "running session requires an idle maintained container lease", nil)
		}
		releaseSnapshot, err = active.lease.BeginSnapshot()
		if err != nil {
			return "", portableResumeManifest{}, durable.NewError(durable.CodeInvalidState, op, err.Error(), nil)
		}
		defer releaseSnapshot()
	}

	driver, ok := s.RuntimeFor(stored.Runtime).(runtime.PortableProviderState)
	if !ok {
		return "", portableResumeManifest{}, durable.NewError(durable.CodeInvalidState, op, "Docker runtime cannot export portable provider state", nil)
	}
	target := portableProviderTarget(stored.Agent)
	manifest := portableResumeManifest{
		SchemaVersion: portableResumeSchemaVersion, Agent: stored.Agent,
		ProviderSessionID: generation.ProviderID, SourceSessionID: stored.ID,
		ProviderStateTarget: target, ImageReference: generation.ImageReference,
		ImageDigest: generation.ImageDigest, CreatedAt: time.Now().UTC(),
	}
	id, manifest, err := s.resumeStates.Export(ctx, manifest, func(exportCtx context.Context, writer io.Writer) error {
		return driver.ExportProviderState(exportCtx, stored.Agent, "agentruntime-vol-"+rootID, writer)
	})
	if err != nil {
		return "", portableResumeManifest{}, durable.NewError(durable.CodeIndeterminate, op, "export portable provider state", err)
	}
	return id, manifest, nil
}

func portableProviderTarget(agentName string) string {
	if agentName == "claude" {
		return "/home/agent/.claude/projects"
	}
	return "/home/agent/.codex/sessions"
}

func resumeStateView(id string, manifest portableResumeManifest) v1ResumeStateData {
	return v1ResumeStateData{
		ResumeStateID: id, SchemaVersion: manifest.SchemaVersion,
		SourceSessionID: manifest.SourceSessionID, Agent: manifest.Agent,
		ProviderSessionID: manifest.ProviderSessionID, ProviderStateTarget: manifest.ProviderStateTarget,
		ProviderStateSHA256: manifest.ProviderStateSHA256, ImageReference: manifest.ImageReference,
		ImageDigest: manifest.ImageDigest, CreatedAt: manifest.CreatedAt,
		DownloadURL: "/api/v1/resume-states/" + id,
	}
}

func manifestRequestsPortableResume(raw json.RawMessage) bool {
	var manifest struct {
		ContainerLease *struct {
			PortableResume bool `json:"portable_resume"`
		} `json:"container_lease"`
	}
	return json.Unmarshal(raw, &manifest) == nil && manifest.ContainerLease != nil && manifest.ContainerLease.PortableResume
}
