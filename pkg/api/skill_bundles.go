package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/danieliser/agentruntime/pkg/durable"
)

const SkillBundleContentType = "application/x-tar"

func (s *Server) handleV1PutSkillBundle(c *gin.Context) {
	const op = "put_skill_bundle"
	if s.skillBundles == nil {
		writeDurableError(c, durable.NewError(durable.CodeIndeterminate, op, "skill bundle store unavailable", nil))
		return
	}
	if c.ContentType() != SkillBundleContentType {
		writeDurableError(c, durable.NewError(durable.CodeInvalidArgument, op, "skill bundle upload requires application/x-tar content type", nil))
		return
	}
	created, err := s.skillBundles.Put(c.Param("digest"), c.Request.Body)
	if err != nil {
		writeDurableError(c, durable.NewError(durable.CodeInvalidArgument, op, "invalid skill bundle", err))
		return
	}
	size, err := s.skillBundles.Size(c.Param("digest"))
	if err != nil {
		writeDurableError(c, durable.NewError(durable.CodeIndeterminate, op, "stat stored skill bundle", err))
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	c.JSON(status, gin.H{"api_version": "v1", "data": gin.H{
		"bundle_sha256": c.Param("digest"),
		"size_bytes":    size,
		"idempotent":    !created,
	}})
}
