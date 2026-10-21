package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/materialize"
)

func restrictedLocalRequest(cfg SpawnConfig) bool {
	return cfg.Request != nil && cfg.Request.Runtime == "local" && cfg.Request.ProviderPermissions != nil && cfg.Request.ProviderPermissions.Mode == "restricted"
}

func prepareRestrictedLocal(cfg SpawnConfig) (command, env []string, cleanup func() error, err error) {
	if err := CheckRestrictedLocalProviderPermissions(cfg.Request.Agent, cfg.Request.ProviderPermissions.Tools); err != nil {
		return nil, nil, nil, err
	}
	return materializeRestrictedLocal(cfg)
}

// The qualification suite uses the production materializer without recursively
// asking the qualification cache to approve its synthetic fixture.
func materializeRestrictedLocal(cfg SpawnConfig) (command, env []string, cleanup func() error, err error) {
	reject := func(message string) ([]string, []string, func() error, error) {
		return nil, nil, nil, durable.NewError(durable.CodeExecutionPolicyUnsupported, "restricted_local", message, nil)
	}
	req := cfg.Request
	if cfg.DataDir == "" || !safeRuntimeSessionID(cfg.SessionID) {
		return reject("restricted local requires durable provider storage")
	}
	if req.WorkDir == "" || !filepath.IsAbs(req.WorkDir) || cfg.WorkDir != req.WorkDir {
		return reject("restricted local requires the explicit absolute project work_dir")
	}
	if len(req.Mounts) > 0 || len(req.Volumes) > 0 {
		return reject("restricted local does not grant mounts or volumes")
	}
	if pathErr := validateRestrictedLocalPaths(cfg.WorkDir, cfg.DataDir); pathErr != nil {
		return nil, nil, nil, pathErr
	}
	for key := range req.Env {
		if !apischema.RestrictedLocalEnvAllowed(key) {
			return reject(fmt.Sprintf("environment variable %q is unsupported", key))
		}
	}
	req.Context = "clean"
	req.AutoDiscover = false
	dataDir, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return reject("cannot resolve durable provider storage")
	}
	stateDir := filepath.Join(dataDir, req.Agent+"-sessions", cfg.SessionID)
	for _, path := range []string{filepath.Dir(stateDir), stateDir} {
		info, statErr := os.Lstat(path)
		if statErr != nil && !os.IsNotExist(statErr) {
			return nil, nil, nil, statErr
		}
		if statErr == nil && !info.IsDir() {
			return reject("restricted local provider/session storage must be real directories")
		}
	}
	home := filepath.Join(stateDir, "local-home")
	cleanup = func() error {
		var cleanupErr error
		for _, name := range []string{"auth.json", "credentials.json", ".credentials.json"} {
			if err := os.Remove(filepath.Join(stateDir, name)); err != nil && !os.IsNotExist(err) {
				cleanupErr = errors.Join(cleanupErr, err)
			}
		}
		return errors.Join(os.RemoveAll(home), cleanupErr)
	}
	// A cancelled supervisor may leave a prior launch home, including symlinks.
	// Remove only that disposable home before materializing fresh credentials.
	if err = os.RemoveAll(home); err != nil {
		return nil, nil, cleanup, err
	}
	result, err := materialize.Materialize(req, cfg.SessionID, dataDir)
	if err != nil {
		return nil, nil, cleanup, err
	}
	if cfg.SessionDir != nil {
		*cfg.SessionDir = result.SessionDir
	}
	// Return the cleanup even on preparation failures. The caller owns its use.
	defer func() {
		if err != nil {
			command = nil
			env = nil
		}
	}()
	providerHome := filepath.Join(home, "."+req.Agent)
	if err = os.MkdirAll(providerHome, 0700); err != nil {
		return
	}
	entries, readErr := os.ReadDir(result.SessionDir)
	if readErr != nil {
		err = readErr
		return
	}
	for _, entry := range entries {
		if entry.Name() == "local-home" || entry.Name() == "sessions" || (req.Agent == "claude" && entry.Name() == "projects") || strings.HasPrefix(entry.Name(), "state_") {
			continue
		}
		if err = os.Rename(filepath.Join(result.SessionDir, entry.Name()), filepath.Join(providerHome, entry.Name())); err != nil {
			return
		}
	}
	if err = os.Symlink(filepath.Join(result.SessionDir, "sessions"), filepath.Join(providerHome, "sessions")); err != nil {
		return
	}
	if req.Agent == "claude" {
		if err = os.Symlink(filepath.Join(result.SessionDir, "projects"), filepath.Join(providerHome, "projects")); err != nil {
			return
		}
	}
	configName := "config.toml"
	var config []byte
	var configErr error
	if req.Agent == "claude" {
		configName = "restricted-settings.json"
		config, configErr = materialize.RestrictedLocalClaudeSettings(req, home, providerHome)
	} else {
		config, configErr = materialize.RestrictedLocalCodexConfig(req, home, providerHome, result.SessionDir)
	}
	if configErr != nil {
		err = configErr
		return
	}
	if err = os.WriteFile(filepath.Join(providerHome, configName), config, 0600); err != nil {
		return
	}
	values := map[string]string{"HOME": home, "PATH": os.Getenv("PATH"), "SHELL": "/bin/sh"}
	if req.Agent == "claude" {
		values["CLAUDE_CONFIG_DIR"] = providerHome
		values["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] = "1"
		values["DISABLE_AUTOUPDATER"] = "1"
	} else {
		values["CODEX_HOME"] = providerHome
	}
	for key, value := range req.Env {
		values[key] = value
	}
	for _, key := range []string{"LANG", "LC_ALL", "LC_CTYPE", "LC_MESSAGES", "LANGUAGE"} {
		if _, present := values[key]; !present {
			if value, ok := os.LookupEnv(key); ok {
				values[key] = value
			}
		}
	}
	for key, subdir := range map[string]string{"XDG_CONFIG_HOME": "config", "XDG_DATA_HOME": "data", "XDG_CACHE_HOME": "cache", "XDG_STATE_HOME": "state", "TMPDIR": "tmp", "TMP": "tmp", "TEMP": "tmp"} {
		path := filepath.Join(home, subdir)
		if err = os.MkdirAll(path, 0700); err != nil {
			return
		}
		values[key] = path
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err = validateEnvKey(key); err != nil {
			return
		}
		if strings.ContainsRune(values[key], 0) {
			err = fmt.Errorf("invalid environment value for %s", key)
			return
		}
		env = append(env, key+"="+values[key])
	}
	command = append([]string(nil), cfg.Cmd...)
	if req.Agent == "claude" {
		command = append(command, "--settings", filepath.Join(providerHome, configName))
	}
	return
}
