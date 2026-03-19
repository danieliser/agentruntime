package api

import "github.com/gin-gonic/gin"

// RegisterRoutes wires up all HTTP and WebSocket routes.
func RegisterRoutes(r *gin.Engine, s *Server) {
	r.GET("/health", s.handleHealth)

	sessions := r.Group("/sessions")
	{
		sessions.POST("", s.handleCreateSession)
		sessions.GET("", s.handleListSessions)
		sessions.GET("/history", s.handleSessionHistory)
		sessions.GET("/:id", s.handleGetSession)
		sessions.GET("/:id/info", s.handleGetSessionInfo)
		sessions.GET("/:id/logs", s.handleGetLogs)
		sessions.GET("/:id/log", s.handleGetLogFile) // full NDJSON log file
		sessions.DELETE("/:id", s.handleDeleteSession)
	}

	r.GET("/ws/sessions/:id", s.handleSessionWS)

	chats := r.Group("/chats")
	{
		chats.POST("", s.handleCreateChat)
		chats.GET("", s.handleListChats)
		chats.GET("/:name", s.handleGetChat)
		chats.POST("/:name/messages", s.handleSendMessage)
		chats.POST("/:name/attach", s.handleChatAttach)
		chats.GET("/:name/messages", s.handleGetChatMessages)
		chats.PATCH("/:name/config", s.handleUpdateChatConfig)
		chats.DELETE("/:name", s.handleDeleteChat)
	}

	r.GET("/ws/chats/:name", s.handleChatWS)

	// Embedded dashboard — baked into the binary, works from any working directory.
	r.StaticFS("/dashboard", DashboardHandler())

	// Redirect root and unmatched routes to dashboard.
	r.NoRoute(func(c *gin.Context) {
		if c.Request.URL.Path == "/" {
			c.Redirect(301, "/dashboard/")
			return
		}
		c.JSON(404, gin.H{"error": "not found"})
	})
}
