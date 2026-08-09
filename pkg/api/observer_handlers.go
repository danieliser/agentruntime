package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/danieliser/agentruntime/pkg/observer"
)

func (s *Server) handleV1Plugins(c *gin.Context) {
	statuses := []observer.PluginStatus{}
	if s.observers != nil {
		statuses = s.observers.Status()
	}
	c.JSON(http.StatusOK, gin.H{"api_version": "v1", "plugin_api_version": observer.APIVersion, "data": statuses})
}

func (s *Server) handleV1SessionTraces(c *gin.Context) {
	links := []observer.TraceLink{}
	if s.observers != nil {
		for _, status := range s.observers.Status() {
			if link, ok := s.observers.TraceLink(status.Name, c.Param("id")); ok {
				links = append(links, link)
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{"api_version": "v1", "data": links})
}
