package materialize

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
	"github.com/danieliser/agentruntime/pkg/session/agentsessions"
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

	if req.Claude != nil {
		sessionDir, err := materializeClaude(tmpDir, dataDir, sessionID, req, &result.Mounts)
		if err != nil {
			cleanup()
			return nil, err
		}
		result.SessionDir = sessionDir
	}

	if req.Codex != nil {
		sessionDir, err := materializeCodex(tmpDir, dataDir, sessionID, req, &result.Mounts)
		if err != nil {
			cleanup()
			return nil, err
		}
		result.SessionDir = sessionDir
	}

	return result, nil
}

func materializeClaude(tmpDir, dataDir, sessionID string, req *apischema.SessionRequest, mounts *[]apischema.Mount) (string, error) {
	claudeDir, err := claudeMountSource(tmpDir, dataDir, sessionID, req)
	if err != nil {
		return "", err
	}

	settings := req.Claude.SettingsJSON
	if settings == nil {
		settings = map[string]any{}
	}
	if err := writeJSONFile(filepath.Join(claudeDir, "settings.json"), settings); err != nil {
		return "", err
	}

	if err := writeTextFile(filepath.Join(claudeDir, "CLAUDE.md"), req.Claude.ClaudeMD); err != nil {
		return "", err
	}

	mcpJSON, err := buildClaudeMCPJSON(req.Claude.McpJSON, req.MCPServers)
	if err != nil {
		return "", err
	}
	if err := writeJSONFile(filepath.Join(claudeDir, ".mcp.json"), mcpJSON); err != nil {
		return "", err
	}

	*mounts = append(*mounts, apischema.Mount{
		Host:      claudeDir,
		Container: "/home/agent/.claude",
		Mode:      "rw",
	})

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

	return claudeDir, nil
}

func materializeCodex(tmpDir, dataDir, sessionID string, req *apischema.SessionRequest, mounts *[]apischema.Mount) (string, error) {
	codexDir, err := codexMountSource(tmpDir, dataDir, sessionID)
	if err != nil {
		return "", err
	}

	config := req.Codex.ConfigTOML
	if config == nil {
		config = map[string]any{}
	}
	tomlData, err := marshalSimpleTOML(config)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(codexDir, "config.toml"), tomlData, 0o644); err != nil {
		return "", err
	}

	if err := writeTextFile(filepath.Join(codexDir, "instructions.md"), req.Codex.Instructions); err != nil {
		return "", err
	}

	*mounts = append(*mounts, apischema.Mount{
		Host:      codexDir,
		Container: "/home/agent/.codex",
		Mode:      "rw",
	})

	return codexDir, nil
}

func claudeMountSource(tmpDir, dataDir, sessionID string, req *apischema.SessionRequest) (string, error) {
	if dataDir == "" {
		claudeDir := filepath.Join(tmpDir, ".claude")
		if err := os.MkdirAll(claudeDir, 0o755); err != nil {
			return "", err
		}
		return claudeDir, nil
	}

	credentialsPath := ""
	if req.Claude.CredentialsPath != "" {
		expanded, err := expandPath(req.Claude.CredentialsPath)
		if err != nil {
			return "", err
		}
		credentialsPath = expanded
	}

	return agentsessions.InitClaudeSessionDir(dataDir, sessionID, claudeProjectPath(), credentialsPath)
}

func codexMountSource(tmpDir, dataDir, sessionID string) (string, error) {
	if dataDir == "" {
		codexDir := filepath.Join(tmpDir, ".codex")
		if err := os.MkdirAll(codexDir, 0o755); err != nil {
			return "", err
		}
		return codexDir, nil
	}

	return agentsessions.InitCodexSessionDir(dataDir, sessionID)
}

func claudeProjectPath() string {
	return "/workspace"
}

func buildClaudeMCPJSON(base map[string]any, servers []apischema.MCPServer) (map[string]any, error) {
	merged, ok := cloneValue(base).(map[string]any)
	if !ok || merged == nil {
		merged = map[string]any{}
	}

	serverMap, ok := merged["mcpServers"].(map[string]any)
	if !ok || serverMap == nil {
		serverMap = map[string]any{}
	}

	for _, server := range servers {
		serverMap[server.Name] = mcpServerToMap(server)
	}

	merged["mcpServers"] = serverMap
	resolved, ok := sanitizeMCPConfigValue("", merged).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("resolved MCP JSON was not an object")
	}

	return resolved, nil
}

func mcpServerToMap(server apischema.MCPServer) map[string]any {
	out := map[string]any{
		"type": server.Type,
	}
	if server.URL != "" {
		out["url"] = server.URL
	}
	if len(server.Cmd) > 0 {
		cmd := make([]any, 0, len(server.Cmd))
		for _, part := range server.Cmd {
			cmd = append(cmd, part)
		}
		out["cmd"] = cmd
	}
	if len(server.Env) > 0 {
		env := make(map[string]any, len(server.Env))
		for k, v := range server.Env {
			env[k] = v
		}
		out["env"] = env
	}
	if server.Token != "" {
		out["token"] = server.Token
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
	var b strings.Builder
	if err := writeTOMLTable(&b, "", values); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

func writeTOMLTable(b *strings.Builder, prefix string, values map[string]any) error {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	wroteTableHeader := false
	if prefix != "" {
		b.WriteString("[")
		b.WriteString(prefix)
		b.WriteString("]\n")
		wroteTableHeader = true
	}

	var tables []string
	for _, key := range keys {
		value := values[key]
		if _, ok := value.(map[string]any); ok {
			tables = append(tables, key)
			continue
		}

		encoded, err := encodeTOMLValue(value)
		if err != nil {
			return fmt.Errorf("encode %q: %w", key, err)
		}
		b.WriteString(key)
		b.WriteString(" = ")
		b.WriteString(encoded)
		b.WriteString("\n")
	}

	for i, key := range tables {
		if (prefix != "" || len(keys) > len(tables)) && (i == 0 || wroteTableHeader || b.Len() > 0) {
			if !strings.HasSuffix(b.String(), "\n\n") {
				b.WriteString("\n")
			}
		}
		nextPrefix := key
		if prefix != "" {
			nextPrefix = prefix + "." + key
		}
		nested := values[key].(map[string]any)
		if err := writeTOMLTable(b, nextPrefix, nested); err != nil {
			return err
		}
		if i < len(tables)-1 {
			b.WriteString("\n")
		}
	}

	return nil
}

func encodeTOMLValue(value any) (string, error) {
	switch v := value.(type) {
	case nil:
		return "\"\"", nil
	case string:
		return strconv.Quote(v), nil
	case bool:
		if v {
			return "true", nil
		}
		return "false", nil
	case int:
		return strconv.Itoa(v), nil
	case int8, int16, int32, int64:
		return strconv.FormatInt(reflect.ValueOf(v).Int(), 10), nil
	case uint, uint8, uint16, uint32, uint64:
		return strconv.FormatUint(reflect.ValueOf(v).Uint(), 10), nil
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32), nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	}

	rv := reflect.ValueOf(value)
	if rv.IsValid() && rv.Kind() == reflect.Slice {
		items := make([]string, 0, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			item, err := encodeTOMLValue(rv.Index(i).Interface())
			if err != nil {
				return "", err
			}
			items = append(items, item)
		}
		return "[" + strings.Join(items, ", ") + "]", nil
	}

	return "", fmt.Errorf("unsupported TOML value type %T", value)
}
