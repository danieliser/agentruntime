package materialize

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
	"github.com/danieliser/agentruntime/pkg/credentials"
	"github.com/danieliser/agentruntime/pkg/session/agentsessions"
	"github.com/danieliser/agentruntime/pkg/skills"
	"github.com/pelletier/go-toml/v2"
)

// Result is the output of materializing agent config into an agent home directory.
type Result struct {
	SessionDir string
	Mounts     []apischema.Mount
	CleanupFn  func()
}

// Materialize writes agent config files into an agent home directory and returns mounts.
// When dataDir is empty, it falls back to the legacy tempdir behavior.
func Materialize(req *apischema.SessionRequest, sessionID, dataDir string) (*Result, error) {
	if req == nil {
		req = &apischema.SessionRequest{}
	}

	result := &Result{
		Mounts:    nil,
		CleanupFn: func() {},
	}

	var tmpDir string
	if dataDir == "" {
		var err error
		tmpDir, err = os.MkdirTemp("", "agentruntime-"+sessionIDPrefix(sessionID))
		if err != nil {
			return nil, err
		}
		result.CleanupFn = func() {
			_ = os.RemoveAll(tmpDir)
		}
	}

	cleanup := result.CleanupFn
	skillSessionDir := ""
	providerRoot := ""

	if req.Claude != nil || req.Agent == "claude" {
		if req.Claude == nil {
			req.Claude = &apischema.ClaudeConfig{}
		}
		sessionDir, err := materializeClaude(tmpDir, dataDir, sessionID, req, &result.Mounts)
		if err != nil {
			cleanup()
			return nil, err
		}
		result.SessionDir = sessionDir
		if req.Agent == "claude" {
			skillSessionDir = sessionDir
			providerRoot = "/home/agent/.claude"
		}
	}

	if req.Codex != nil || req.Agent == "codex" {
		if req.Codex == nil {
			req.Codex = &apischema.CodexConfig{}
		}
		sessionDir, err := materializeCodex(tmpDir, dataDir, sessionID, req, &result.Mounts)
		if err != nil {
			cleanup()
			return nil, err
		}
		result.SessionDir = sessionDir
		if req.Agent == "codex" {
			skillSessionDir = sessionDir
			providerRoot = "/home/agent/.codex"
		}
	}

	if skillSessionDir != "" {
		skillRoot := filepath.Join(skillSessionDir, "skills")
		if err := os.RemoveAll(skillRoot); err != nil {
			cleanup()
			return nil, fmt.Errorf("reset provider skill root: %w", err)
		}
		if err := os.Mkdir(skillRoot, 0o700); err != nil {
			cleanup()
			return nil, fmt.Errorf("create provider skill root: %w", err)
		}
		if req.Skills != nil && len(req.Skills.Entries) > 0 {
			if dataDir == "" {
				cleanup()
				return nil, errors.New("skills require a durable data directory")
			}
			store := skills.NewStore(filepath.Join(dataDir, "skill-bundles"))
			for _, entry := range req.Skills.Entries {
				destination := filepath.Join(skillRoot, entry.Name)
				if err := store.Extract(entry.BundleSHA256, entry.Name, destination); err != nil {
					cleanup()
					return nil, fmt.Errorf("materialize skill %q: %w", entry.Name, err)
				}
			}
		}
		result.Mounts = append(result.Mounts, apischema.Mount{Host: skillRoot, Container: providerRoot + "/skills", Mode: "ro"})
	} else if req.Skills != nil {
		cleanup()
		return nil, fmt.Errorf("skills are unsupported for agent %q", req.Agent)
	}

	return result, nil
}

func materializeClaude(tmpDir, dataDir, sessionID string, req *apischema.SessionRequest, mounts *[]apischema.Mount) (string, error) {
	claudeDir, err := claudeMountSource(tmpDir, dataDir, sessionID, req)
	if err != nil {
		return "", err
	}

	// Parse auto_discover configuration
	var discoverOpts *DiscoverOptions
	if req.AutoDiscover != nil {
		discoverOpts = ParseAutoDiscover(req.AutoDiscover)
	}
	if discoverOpts == nil {
		// Platform default: enable discovery for both local and docker
		discoverOpts = &DiscoverOptions{
			ClaudeMD:   true,
			Settings:   true,
			MCP:        true,
			Rules:      true,
			Agents:     true,
			AgentsMD:   true,
			ConfigTOML: true,
		}
	}

	// Discover files if enabled
	var discovered *ClaudeDiscovery
	if req.WorkDir != "" && (discoverOpts.ClaudeMD || discoverOpts.Settings || discoverOpts.MCP || discoverOpts.Rules || discoverOpts.Agents) {
		discovered, _ = DiscoverClaudeFiles(req.WorkDir, *discoverOpts)
	}

	// Merge CLAUDE.md: discovered first, then explicit
	claudeMD := req.Claude.ClaudeMD
	if claudeMD == "" && discovered != nil && discoverOpts.ClaudeMD && len(discovered.ClaudeMDFiles) > 0 {
		claudeMD = mergeClaudeMD(discovered.ClaudeMDFiles)
	}

	// Merge settings: discovered as base, explicit wins
	settings := req.Claude.SettingsJSON
	if discovered != nil && discoverOpts.Settings && len(discovered.SettingsJSON) > 0 {
		if settings == nil {
			settings = discovered.SettingsJSON
		} else {
			// Deep merge: discovered fills gaps, explicit wins on conflicts
			settings = deepMerge(discovered.SettingsJSON, settings)
		}
	}
	if settings == nil {
		settings = map[string]any{}
	}
	// Pre-accept the dangerous mode permission prompt so Claude doesn't
	// show a TUI dialog when --dangerously-skip-permissions is used.
	if _, exists := settings["skipDangerousModePermissionPrompt"]; !exists {
		settings["skipDangerousModePermissionPrompt"] = true
	}
	if err := writeJSONFile(filepath.Join(claudeDir, "settings.json"), settings); err != nil {
		return "", err
	}

	if err := writeTextFile(filepath.Join(claudeDir, "CLAUDE.md"), claudeMD); err != nil {
		return "", err
	}

	// Merge MCP JSON: discovered as base, explicit wins
	mcpJSONBase := req.Claude.McpJSON
	if discovered != nil && discoverOpts.MCP && len(discovered.McpJSON) > 0 {
		if mcpJSONBase == nil {
			mcpJSONBase = discovered.McpJSON
		} else {
			// Deep merge: discovered fills gaps, explicit wins on conflicts
			mcpJSONBase = deepMerge(discovered.McpJSON, mcpJSONBase)
		}
	}

	mcpJSON, err := buildClaudeMCPJSON(mcpJSONBase, req)
	if err != nil {
		return "", err
	}
	if err := writeJSONFile(filepath.Join(claudeDir, ".mcp.json"), mcpJSON); err != nil {
		return "", err
	}

	// Write a .claude.json that pre-trusts /workspace so Claude skips the
	// trust dialog in interactive mode. The file is placed in the session dir
	// and mounted rw so Claude can update it during the session.
	claudeState := map[string]any{
		"numStartups":            100,
		"autoUpdates":            false,
		"hasCompletedOnboarding": true,
		"lastOnboardingVersion":  "1.0.53",
		"hasSeenTasksHint":       true,
		"hasSeenStashHint":       true,
		"lastReleaseNotesSeen":   "2.1.76",
		"projects": map[string]any{
			"/workspace": map[string]any{
				"hasTrustDialogAccepted":        true,
				"hasCompletedProjectOnboarding": true,
				"hasTrustDialogHooksAccepted":   true,
				"allowedTools":                  []any{},
			},
		},
	}
	// Session dir mount FIRST — tests and consumers expect Mounts[0] to be the .claude dir.
	*mounts = append(*mounts, apischema.Mount{
		Host:      claudeDir,
		Container: "/home/agent/.claude",
		Mode:      "rw",
	})

	// Write .claude.json inside the per-session dir and bind-mount it as a
	// single file at /home/agent/.claude.json. The Dockerfile pre-creates this
	// target with `touch` so Docker mounts it as a file, not a directory.
	// Previously this was written to the parent (shared) dir, causing race
	// conditions when multiple sessions spawn concurrently.
	claudeStateBytes, _ := json.MarshalIndent(claudeState, "", "  ")
	claudeStatePath := filepath.Join(claudeDir, ".claude.json")
	if err := os.WriteFile(claudeStatePath, claudeStateBytes, 0o644); err == nil {
		*mounts = append(*mounts, apischema.Mount{
			Host:      claudeStatePath,
			Container: "/home/agent/.claude.json",
			Mode:      "rw",
		})
	}

	if req.Claude.MemoryPath != "" {
		hostPath, err := expandPath(req.Claude.MemoryPath)
		if err != nil {
			return "", err
		}
		hash := sha256.Sum256([]byte(hostPath))
		*mounts = append(*mounts, apischema.Mount{
			Host:      hostPath,
			Container: "/home/agent/.claude/projects/" + hex.EncodeToString(hash[:])[:16],
			Mode:      "ro",
		})
	}
	if req.ExecutionPolicy != nil || restrictedLocal(req) {
		if err := hardenMaterializedTree(claudeDir); err != nil {
			return "", err
		}
	}

	return claudeDir, nil
}

func materializeCodex(tmpDir, dataDir, sessionID string, req *apischema.SessionRequest, mounts *[]apischema.Mount) (string, error) {
	restricted := req.ExecutionPolicy != nil
	explicitAuth, err := apischema.ExplicitCodexAuthJSON(req)
	if err != nil {
		return "", fmt.Errorf("validate explicit Codex credential grant: %w", err)
	}
	codexDir, err := codexMountSource(tmpDir, dataDir, sessionID, restricted)
	if err != nil {
		return "", err
	}

	// Parse auto_discover configuration
	var discoverOpts *DiscoverOptions
	if req.AutoDiscover != nil {
		discoverOpts = ParseAutoDiscover(req.AutoDiscover)
	}
	if discoverOpts == nil {
		// Platform default: enable discovery for both local and docker
		discoverOpts = &DiscoverOptions{
			ClaudeMD:   true,
			Settings:   true,
			MCP:        true,
			Rules:      true,
			Agents:     true,
			AgentsMD:   true,
			ConfigTOML: true,
		}
	}

	// Discover files if enabled
	var discovered *CodexDiscovery
	if req.WorkDir != "" && (discoverOpts.AgentsMD || discoverOpts.ConfigTOML) {
		discovered, _ = DiscoverCodexFiles(req.WorkDir, *discoverOpts)
	}

	// Merge config.toml: discovered as base, explicit wins
	config := cloneMap(req.Codex.ConfigTOML)
	if discovered != nil && discoverOpts.ConfigTOML && len(discovered.ConfigTOML) > 0 {
		if config == nil {
			config = discovered.ConfigTOML
		} else {
			// Deep merge: discovered fills gaps, explicit wins on conflicts
			config = deepMerge(discovered.ConfigTOML, config)
		}
	}
	if config == nil {
		config = map[string]any{}
	}
	// ACT-1001: the top-level resolved controls are the canonical provider
	// values. Materialization must never replace them with a wider default.
	if req.Model != "" {
		config["model"] = req.Model
	}
	if req.Effort != "" {
		config["model_reasoning_effort"] = req.Effort
	}
	if req.Fast {
		config["service_tier"] = "priority"
	}
	mergeCodexMCPServers(req, config)

	// Merge AGENTS.md: discovered first, then explicit
	agentsMD := req.Codex.Instructions
	if agentsMD == "" && discovered != nil && discoverOpts.AgentsMD && discovered.AgentsMD != "" {
		agentsMD = discovered.AgentsMD
	}
	tomlData, err := marshalSimpleTOML(config)
	if err != nil {
		return "", err
	}
	// Append workspace trust that the flat TOML marshaler cannot represent.
	tomlData = append(tomlData, []byte("\n"+
		"[projects.\"/workspace\"]\n"+
		"trust_level = \"trusted\"\n",
	)...)
	if err := os.WriteFile(filepath.Join(codexDir, "config.toml"), tomlData, 0o644); err != nil {
		return "", err
	}

	if err := writeTextFile(filepath.Join(codexDir, "instructions.md"), agentsMD); err != nil {
		return "", err
	}

	// Copy Codex auth.json if not already placed by codexMountSource (persistent mode).
	authDest := filepath.Join(codexDir, "auth.json")
	if len(explicitAuth) != 0 {
		if err := os.WriteFile(authDest, explicitAuth, 0o600); err != nil {
			return "", fmt.Errorf("write explicit Codex credential grant: %w", err)
		}
		delete(req.Env, apischema.CodexAuthJSONEnv)
	} else if !restricted {
		if _, err := os.Stat(authDest); os.IsNotExist(err) {
			if authData := discoverCodexAuth(dataDir); authData != nil {
				_ = os.WriteFile(authDest, authData, 0o600)
			}
		}
	}

	*mounts = append(*mounts, apischema.Mount{
		Host:      codexDir,
		Container: "/home/agent/.codex",
		Mode:      "rw",
	})
	if restricted || restrictedLocal(req) {
		if err := hardenMaterializedTree(codexDir); err != nil {
			return "", err
		}
	}

	return codexDir, nil
}

func hardenMaterializedTree(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if entry.IsDir() {
			mode = 0o700
		}
		if err := os.Chmod(path, mode); err != nil {
			return fmt.Errorf("set private mode on %s: %w", path, err)
		}
		return nil
	})
}

func claudeMountSource(tmpDir, dataDir, sessionID string, req *apischema.SessionRequest) (string, error) {
	if dataDir == "" {
		claudeDir := filepath.Join(tmpDir, ".claude")
		if err := os.MkdirAll(claudeDir, 0o755); err != nil {
			return "", err
		}
		return claudeDir, nil
	}

	restricted := req.ExecutionPolicy != nil
	credentialsPath := ""
	if req.Claude.CredentialsPath != "" {
		expanded, err := expandPath(req.Claude.CredentialsPath)
		if err != nil {
			return "", err
		}
		credentialsPath = expanded
	}

	// Auto-discover credentials from credential sync cache when not explicitly provided.
	// The daemon's --credential-sync flag populates this cache from Keychain/file sources.
	if !restricted && credentialsPath == "" && dataDir != "" {
		syncCache := filepath.Join(dataDir, "credentials", "claude-credentials.json")
		if _, err := os.Stat(syncCache); err == nil {
			credentialsPath = syncCache
		}
		// Also check the host's default Claude credentials location.
		if credentialsPath == "" {
			if home, err := os.UserHomeDir(); err == nil {
				for _, name := range []string{".credentials.json", "credentials.json"} {
					hostCreds := filepath.Join(home, ".claude", name)
					if _, err := os.Stat(hostCreds); err == nil {
						credentialsPath = hostCreds
						break
					}
				}
			}
		}
	}

	if credentialsPath != "" {
		if err := credentials.ValidateClaudeCredentialsFile(credentialsPath); err != nil {
			return "", err
		}
	}
	return agentsessions.InitClaudeSessionDir(dataDir, sessionID, claudeProjectPath(), credentialsPath)
}

func codexMountSource(tmpDir, dataDir, sessionID string, restricted bool) (string, error) {
	if dataDir == "" {
		codexDir := filepath.Join(tmpDir, ".codex")
		if err := os.MkdirAll(codexDir, 0o755); err != nil {
			return "", err
		}
		return codexDir, nil
	}

	codexDir, err := agentsessions.InitCodexSessionDir(dataDir, sessionID)
	if err != nil {
		return "", err
	}

	// Auto-discover Codex auth.json for legacy persistent session dirs. An
	// explicit execution policy requires credentials to arrive only through
	// declared secret environment grants.
	// Priority: 1) credential sync cache, 2) host ~/.codex/auth.json.
	if !restricted {
		authData := discoverCodexAuth(dataDir)
		if authData != nil {
			_ = os.WriteFile(filepath.Join(codexDir, "auth.json"), authData, 0o600)
		}
	}

	return codexDir, nil
}

// discoverCodexAuth returns the contents of the best available Codex auth.json,
// checking the credential sync cache first, then the host's default location.
func discoverCodexAuth(dataDir string) []byte {
	// 1. Credential sync cache (populated by --credential-sync).
	if dataDir != "" {
		syncCache := filepath.Join(dataDir, "credentials", "codex-auth.json")
		if data, err := os.ReadFile(syncCache); err == nil {
			return data
		}
	}
	// 2. Host ~/.codex/auth.json.
	if home, err := os.UserHomeDir(); err == nil {
		if data, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json")); err == nil {
			return data
		}
	}
	return nil
}

func claudeProjectPath() string {
	return "/workspace"
}

func buildClaudeMCPJSON(base map[string]any, req *apischema.SessionRequest) (map[string]any, error) {
	merged, ok := cloneValue(base).(map[string]any)
	if !ok || merged == nil {
		merged = map[string]any{}
	}

	serverMap, ok := merged["mcpServers"].(map[string]any)
	if !ok || serverMap == nil {
		serverMap = map[string]any{}
	}

	for _, server := range req.MCPServers {
		serverMap[server.Name] = mcpServerToMap(req, server)
	}

	merged["mcpServers"] = serverMap
	resolved, ok := sanitizeMCPConfigValue("", merged).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("resolved MCP JSON was not an object")
	}

	return resolved, nil
}

func mcpServerToMap(req *apischema.SessionRequest, server apischema.MCPServer) map[string]any {
	out := map[string]any{
		"type": server.Type,
	}
	if server.URL != "" {
		out["url"] = server.URL
	}
	// Claude Code .mcp.json expects "command" + "args", not a flat "cmd" array.
	if len(server.Cmd) > 0 {
		out["command"] = server.Cmd[0]
		if len(server.Cmd) > 1 {
			args := make([]any, 0, len(server.Cmd)-1)
			for _, part := range server.Cmd[1:] {
				args = append(args, part)
			}
			out["args"] = args
		}
	}
	if len(server.Env) > 0 {
		env := make(map[string]any, len(server.Env))
		for k, v := range server.Env {
			env[k] = v
		}
		out["env"] = env
	}
	if server.Token != "" {
		envName := injectMCPTokenEnv(req, server)
		out["headers"] = map[string]any{
			"Authorization": "Bearer ${" + envName + "}",
		}
	}
	return out
}

func writeJSONFile(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func writeTextFile(path, contents string) error {
	return os.WriteFile(path, []byte(contents), 0o644)
}

func expandPath(path string) (string, error) {
	expanded := os.ExpandEnv(path)
	if strings.HasPrefix(expanded, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		switch expanded {
		case "~":
			expanded = home
		default:
			if len(expanded) > 1 && os.IsPathSeparator(expanded[1]) {
				expanded = filepath.Join(home, expanded[2:])
			}
		}
	}
	if filepath.IsAbs(expanded) {
		return filepath.Clean(expanded), nil
	}

	base, err := os.Getwd()
	if err != nil {
		return "", err
	}

	return filepath.Join(base, stripRelativeTraversal(expanded)), nil
}

func sessionIDPrefix(sessionID string) string {
	safe := sanitizeSessionID(sessionID)
	if len(safe) == 0 {
		return ""
	}
	if len(safe) < 8 {
		return safe
	}
	return safe[:8]
}

func cloneValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		cloned := make(map[string]any, len(v))
		for key, item := range v {
			cloned[key] = cloneValue(item)
		}
		return cloned
	case []any:
		cloned := make([]any, len(v))
		for i, item := range v {
			cloned[i] = cloneValue(item)
		}
		return cloned
	default:
		rv := reflect.ValueOf(value)
		if !rv.IsValid() {
			return nil
		}
		switch rv.Kind() {
		case reflect.Map:
			if rv.Type().Key().Kind() != reflect.String {
				return value
			}
			cloned := make(map[string]any, rv.Len())
			iter := rv.MapRange()
			for iter.Next() {
				cloned[iter.Key().String()] = cloneValue(iter.Value().Interface())
			}
			return cloned
		case reflect.Slice, reflect.Array:
			cloned := make([]any, rv.Len())
			for i := 0; i < rv.Len(); i++ {
				cloned[i] = cloneValue(rv.Index(i).Interface())
			}
			return cloned
		default:
			return value
		}
	}
}

func sanitizeMCPConfigValue(parentKey string, value any) any {
	switch v := value.(type) {
	case map[string]any:
		resolved := make(map[string]any, len(v))
		for key, item := range v {
			next := sanitizeMCPConfigValue(key, item)
			if key == "url" {
				if urlValue, ok := next.(string); !ok || urlValue == "" {
					continue
				}
			}
			resolved[key] = next
		}
		return resolved
	case []any:
		resolved := make([]any, len(v))
		for i, item := range v {
			resolved[i] = sanitizeMCPConfigValue(parentKey, item)
		}
		return resolved
	case string:
		switch parentKey {
		case "url":
			return sanitizeMCPURL(v)
		case "token":
			return sanitizeMCPToken(v)
		default:
			return v
		}
	default:
		rv := reflect.ValueOf(value)
		if !rv.IsValid() {
			return nil
		}
		switch rv.Kind() {
		case reflect.Map:
			if rv.Type().Key().Kind() != reflect.String {
				return value
			}
			resolved := make(map[string]any, rv.Len())
			iter := rv.MapRange()
			for iter.Next() {
				key := iter.Key().String()
				next := sanitizeMCPConfigValue(key, iter.Value().Interface())
				if key == "url" {
					if urlValue, ok := next.(string); !ok || urlValue == "" {
						continue
					}
				}
				resolved[key] = next
			}
			return resolved
		case reflect.Slice, reflect.Array:
			resolved := make([]any, rv.Len())
			for i := 0; i < rv.Len(); i++ {
				resolved[i] = sanitizeMCPConfigValue(parentKey, rv.Index(i).Interface())
			}
			return resolved
		default:
			return value
		}
	}
}

func sanitizeMCPURL(raw string) string {
	resolved := ResolveVars(raw)
	parsed, err := url.Parse(resolved)
	if err != nil {
		return ""
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "ws", "wss":
		return resolved
	default:
		return ""
	}
}

func sanitizeMCPToken(raw string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, raw)
}

func stripRelativeTraversal(path string) string {
	cleaned := filepath.Clean(path)
	if cleaned == "." || cleaned == "" {
		return ""
	}

	sep := string(os.PathSeparator)
	for cleaned == ".." || strings.HasPrefix(cleaned, ".."+sep) {
		cleaned = strings.TrimPrefix(cleaned, "..")
		cleaned = strings.TrimPrefix(cleaned, sep)
		if cleaned == "" {
			return ""
		}
	}

	return cleaned
}

func sanitizeSessionID(sessionID string) string {
	var b strings.Builder
	b.Grow(len(sessionID))

	for _, r := range sessionID {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_':
			b.WriteRune(r)
		default:
			// Replace path separators, dots, and other unsafe chars with '-'.
			// Dots are excluded to prevent ".." path traversal in temp dir names.
			b.WriteByte('-')
		}
	}

	return strings.Trim(b.String(), "-")
}

func marshalSimpleTOML(values map[string]any) ([]byte, error) {
	return toml.Marshal(values)
}
