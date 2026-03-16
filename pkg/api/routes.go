package api

import "github.com/gin-gonic/gin"

// RegisterRoutes wires up all HTTP and WebSocket routes.
func RegisterRoutes(r *gin.Engine, s *Server) {
	r.GET("/health", s.handleHealth)

	sessions := r.Group("/sessions")
	{
		sessions.POST("", s.handleCreateSession)
		sessions.GET("/:id", s.handleGetSession)
		sessions.DELETE("/:id", s.handleDeleteSession)
	}

	r.GET("/ws/sessions/:id", s.handleSessionWS)
}
