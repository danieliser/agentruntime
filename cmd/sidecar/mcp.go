package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	mcpProtocolVersion = "2025-11-25"
	mcpIdeName         = "agentruntime"
	mcpPingInterval    = 30 * time.Second
)

type MCPServerConfig struct {
	WorkspaceFolders []string
	IDEName          string
}

type MCPServer struct {
	workspaceFolders []string
	ideName          string
	authToken        string

	mu              sync.RWMutex
	port            int
	lockFile        string
	listener        net.Listener
	httpServer      *http.Server
	conn            *websocket.Conn
	latestSelection map[string]any
	openEditors     []map[string]any
}

type mcpRPCMessage struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpRPCError    `json:"error,omitempty"`
}

type mcpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func NewMCPServer(cfg MCPServerConfig) (*MCPServer, error) {
	workspace := append([]string(nil), cfg.WorkspaceFolders...)
	if len(workspace) == 0 {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		workspace = []string{cwd}
	}

	ideName := cfg.IDEName
	if ideName == "" {
		ideName = mcpIdeName
	}

	return &MCPServer{
		workspaceFolders: workspace,
		ideName:          ideName,
		authToken:        uuid.NewString(),
		openEditors:      make([]map[string]any, 0),
	}, nil
}

func (s *MCPServer) Start() error {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		_ = listener.Close()
		return errors.New("unexpected listener address type")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleWebSocket)
	mux.HandleFunc("/ws", s.handleWebSocket)

	server := &http.Server{
		Handler: mux,
	}

	s.mu.Lock()
	s.listener = listener
	s.port = addr.Port
	s.httpServer = server
	s.lockFile = filepath.Join(mcpLockDir(), fmt.Sprintf("%d.lock", s.port))
	s.mu.Unlock()

	if err := s.writeLockFile(); err != nil {
		_ = listener.Close()
		return err
	}

	go func() {
		_ = server.Serve(listener)
	}()

	return nil
}

func (s *MCPServer) Stop() error {
	s.mu.Lock()
	conn := s.conn
	server := s.httpServer
	lockFile := s.lockFile
	s.conn = nil
	s.httpServer = nil
	s.listener = nil
	s.port = 0
	s.lockFile = ""
	s.mu.Unlock()

	if conn != nil {
		_ = conn.Close()
	}
	if server != nil {
		_ = server.Close()
	}
	if lockFile != "" {
		_ = os.Remove(lockFile)
	}
	return nil
}

func (s *MCPServer) EnvVars() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.port == 0 {
		return nil
	}
	return []string{
		fmt.Sprintf("CLAUDE_CODE_SSE_PORT=%d", s.port),
		"ENABLE_IDE_INTEGRATION=true",
		"CLAUDE_CODE_EXIT_AFTER_STOP_DELAY=0",
	}
}

func (s *MCPServer) Port() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.port
}

func (s *MCPServer) AuthToken() string {
	return s.authToken
}

func (s *MCPServer) LockFile() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lockFile
}

func (s *MCPServer) SendSelection(text, filePath string, lineStart, lineEnd int) error {
	lastLine := ""
	if text != "" {
		parts := strings.Split(text, "\n")
		lastLine = parts[len(parts)-1]
	}
	params := map[string]any{
		"text":     text,
		"filePath": filePath,
		"fileUrl":  fileURL(filePath),
		"selection": map[string]any{
			"start": map[string]any{
				"line":      lineStart,
				"character": 0,
			},
			"end": map[string]any{
				"line":      chooseLineEnd(lineStart, lineEnd),
				"character": len(lastLine),
			},
			"isEmpty": text == "",
		},
	}

	s.mu.Lock()
	s.latestSelection = params
	s.mu.Unlock()

	return s.sendNotification("selection_changed", params)
}

func (s *MCPServer) SendAtMention(filePath string, lineStart, lineEnd int) error {
	return s.sendNotification("at_mentioned", map[string]any{
		"filePath":  filePath,
		"lineStart": lineStart,
		"lineEnd":   lineEnd,
	})
}

func (s *MCPServer) sendNotification(method string, params map[string]any) error {
	payload := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	}

	s.mu.RLock()
	conn := s.conn
	s.mu.RUnlock()
	if conn == nil {
		return errors.New("no claude ide connection")
	}

	return conn.WriteJSON(payload)
}

func (s *MCPServer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("x-claude-code-ide-authorization")
	if token == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if token != s.authToken {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	upgrader := websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return true },
		Subprotocols: []string{
			"mcp",
		},
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	s.replaceConnection(conn)
	defer s.clearConnection(conn)

	_ = conn.SetReadDeadline(time.Now().Add(2 * mcpPingInterval))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(2 * mcpPingInterval))
	})

	stopPing := make(chan struct{})
	go s.pingLoop(conn, stopPing)
	defer close(stopPing)

	for {
		var msg mcpRPCMessage
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(2 * mcpPingInterval))
		s.handleRPC(conn, msg)
	}
}

func (s *MCPServer) replaceConnection(conn *websocket.Conn) {
	s.mu.Lock()
	old := s.conn
	s.conn = conn
	s.mu.Unlock()

	if old != nil {
		_ = old.Close()
	}
}

func (s *MCPServer) clearConnection(conn *websocket.Conn) {
	s.mu.Lock()
	if s.conn == conn {
		s.conn = nil
	}
	s.mu.Unlock()
}

func (s *MCPServer) pingLoop(conn *websocket.Conn, stop <-chan struct{}) {
	ticker := time.NewTicker(mcpPingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeTimeout))
		}
	}
}

func (s *MCPServer) handleRPC(conn *websocket.Conn, msg mcpRPCMessage) {
	switch msg.Method {
	case "initialize":
		s.writeRPCResult(conn, msg.ID, map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities": map[string]any{
				"tools": map[string]any{"listChanged": true},
				"resources": map[string]any{
					"subscribe":   false,
					"listChanged": false,
				},
			},
			"serverInfo": map[string]any{
				"name":    s.ideName,
				"version": "1.0.0",
			},
		})
	case "ping":
		s.writeRPCResult(conn, msg.ID, map[string]any{})
	case "tools/list":
		s.writeRPCResult(conn, msg.ID, map[string]any{"tools": s.toolList()})
	case "tools/call":
		s.handleToolCall(conn, msg)
	case "resources/list":
		s.writeRPCResult(conn, msg.ID, map[string]any{"resources": []any{}})
	case "resources/read":
		s.writeRPCResult(conn, msg.ID, map[string]any{"contents": []any{}})
	case "resources/templates/list":
		s.writeRPCResult(conn, msg.ID, map[string]any{"resourceTemplates": []any{}})
	case "prompts/list":
		s.writeRPCResult(conn, msg.ID, map[string]any{"prompts": []any{}})
	case "notifications/initialized", "ide_connected":
		// Claude sends these as notifications; no response required.
	default:
		if msg.ID != nil {
			s.writeRPCResult(conn, msg.ID, map[string]any{})
		}
	}
}

func (s *MCPServer) handleToolCall(conn *websocket.Conn, msg mcpRPCMessage) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		s.writeRPCError(conn, msg.ID, -32602, "invalid tool params")
		return
	}

	result, isError := s.toolResult(params.Name, params.Arguments)
	payload := map[string]any{
		"content": []map[string]any{
			{
				"type": "text",
				"text": result,
			},
		},
	}
	if isError {
		payload["isError"] = true
	}
	s.writeRPCResult(conn, msg.ID, payload)
}

func (s *MCPServer) toolResult(name string, args map[string]any) (string, bool) {
	encode := func(v any) string {
		data, _ := json.Marshal(v)
		return string(data)
	}

	switch name {
	case "openFile":
		path, _ := args["filePath"].(string)
		return encode(map[string]any{
			"success":  true,
			"filePath": path,
			"preview":  truthy(args["preview"]),
		}), false
	case "openDiff":
		return encode("DIFF_REJECTED"), false
	case "getCurrentSelection", "getLatestSelection":
		s.mu.RLock()
		selection := s.latestSelection
		s.mu.RUnlock()
		if selection == nil {
			return encode(map[string]any{"success": false, "message": "No active editor"}), false
		}
		return encode(selection), false
	case "getOpenEditors":
		s.mu.RLock()
		tabs := append([]map[string]any(nil), s.openEditors...)
		s.mu.RUnlock()
		return encode(map[string]any{"tabs": tabs}), false
	case "getWorkspaceFolders":
		folders := make([]map[string]any, 0, len(s.workspaceFolders))
		for _, folder := range s.workspaceFolders {
			folders = append(folders, map[string]any{
				"name": filepath.Base(folder),
				"path": folder,
				"uri":  fileURL(folder),
			})
		}
		rootPath := ""
		if len(s.workspaceFolders) > 0 {
			rootPath = s.workspaceFolders[0]
		}
		return encode(map[string]any{
			"success":  true,
			"folders":  folders,
			"rootPath": rootPath,
		}), false
	case "getDiagnostics":
		return encode([]any{}), false
	case "checkDocumentDirty":
		return encode(map[string]any{
			"success":    true,
			"isDirty":    false,
			"isUntitled": false,
		}), false
	case "saveDocument":
		return encode(map[string]any{"success": true, "saved": true}), false
	case "close_tab":
		return encode("TAB_CLOSED"), false
	case "closeAllDiffTabs":
		return encode("CLOSED_0_DIFF_TABS"), false
	case "executeCode":
		return encode(map[string]any{
			"success": false,
			"message": "Jupyter execution is unavailable in the sidecar",
		}), false
	default:
		return encode(map[string]any{
			"success": false,
			"message": fmt.Sprintf("Unknown tool: %s", name),
		}), true
	}
}

func (s *MCPServer) toolList() []map[string]any {
	return []map[string]any{
		{
			"name":        "openFile",
			"description": "Open a file in the editor and optionally select a range of text",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"filePath":          map[string]any{"type": "string"},
					"preview":           map[string]any{"type": "boolean", "default": false},
					"startText":         map[string]any{"type": "string"},
					"endText":           map[string]any{"type": "string"},
					"selectToEndOfLine": map[string]any{"type": "boolean", "default": false},
					"makeFrontmost":     map[string]any{"type": "boolean", "default": true},
				},
				"required": []string{"filePath"},
			},
		},
		{
			"name":        "openDiff",
			"description": "Open a git diff for the file",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"old_file_path":     map[string]any{"type": "string"},
					"new_file_path":     map[string]any{"type": "string"},
					"new_file_contents": map[string]any{"type": "string"},
					"tab_name":          map[string]any{"type": "string"},
				},
				"required": []string{"old_file_path", "new_file_path", "new_file_contents"},
			},
		},
		{
			"name":        "getCurrentSelection",
			"description": "Get the current text selection in the active editor",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "getLatestSelection",
			"description": "Get the most recent text selection",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "getOpenEditors",
			"description": "Get information about currently open editors",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "getWorkspaceFolders",
			"description": "Get all workspace folders currently open in the IDE",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "getDiagnostics",
			"description": "Get language diagnostics",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"uri": map[string]any{"type": "string"},
				},
			},
		},
		{
			"name":        "checkDocumentDirty",
			"description": "Check if a document has unsaved changes",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"filePath": map[string]any{"type": "string"},
				},
				"required": []string{"filePath"},
			},
		},
		{
			"name":        "saveDocument",
			"description": "Save a document with unsaved changes",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"filePath": map[string]any{"type": "string"},
				},
				"required": []string{"filePath"},
			},
		},
		{
			"name":        "close_tab",
			"description": "Close a tab by name",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tab_name": map[string]any{"type": "string"},
				},
				"required": []string{"tab_name"},
			},
		},
		{
			"name":        "closeAllDiffTabs",
			"description": "Close all diff tabs in the editor",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "executeCode",
			"description": "Execute Python in the active Jupyter kernel",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"code": map[string]any{"type": "string"},
				},
				"required": []string{"code"},
			},
		},
	}
}

func (s *MCPServer) writeRPCResult(conn *websocket.Conn, id any, result any) {
	if id == nil {
		return
	}
	_ = conn.WriteJSON(mcpRPCMessage{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	})
}

func (s *MCPServer) writeRPCError(conn *websocket.Conn, id any, code int, message string) {
	if id == nil {
		return
	}
	_ = conn.WriteJSON(mcpRPCMessage{
		JSONRPC: "2.0",
		ID:      id,
		Error: &mcpRPCError{
			Code:    code,
			Message: message,
		},
	})
}

func (s *MCPServer) writeLockFile() error {
	lockDir := mcpLockDir()
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return err
	}

	s.mu.RLock()
	lockFile := s.lockFile
	port := s.port
	s.mu.RUnlock()

	payload := map[string]any{
		"pid":              os.Getpid(),
		"workspaceFolders": s.workspaceFolders,
		"ideName":          s.ideName,
		"transport":        "ws",
		"authToken":        s.authToken,
		"port":             port,
	}

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(lockFile, data, 0o600)
}

func mcpLockDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".claude", "ide")
	}
	return filepath.Join(home, ".claude", "ide")
}

func chooseLineEnd(lineStart, lineEnd int) int {
	if lineEnd > 0 {
		return lineEnd
	}
	return lineStart
}

func fileURL(path string) string {
	if path == "" {
		return ""
	}
	return (&url.URL{Scheme: "file", Path: path}).String()
}

func truthy(v any) bool {
	switch value := v.(type) {
	case bool:
		return value
	case string:
		return value == "true"
	default:
		return false
	}
}
