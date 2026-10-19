package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	if !slices.Contains(QualifiedRestrictedLocalAgents(), cfg.Request.Agent) {
		return nil, nil, nil, durable.NewError(durable.CodeExecutionPolicyUnsupported, "restricted_local", "provider has no qualified read-confined local sandbox", nil)
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
	stateDir := filepath.Join(dataDir, "codex-sessions", cfg.SessionID)
	home := filepath.Join(stateDir, "local-home")
	cleanup = func() error {
		authErr := os.Remove(filepath.Join(stateDir, "auth.json"))
		if os.IsNotExist(authErr) {
			authErr = nil
		}
		return errors.Join(os.RemoveAll(home), authErr)
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
	providerHome := filepath.Join(home, ".codex")
	if err = os.MkdirAll(providerHome, 0700); err != nil {
		return
	}
	entries, readErr := os.ReadDir(result.SessionDir)
	if readErr != nil {
		err = readErr
		return
	}
	for _, entry := range entries {
		if entry.Name() == "local-home" || entry.Name() == "sessions" || strings.HasPrefix(entry.Name(), "state_") {
			continue
		}
		if err = os.Rename(filepath.Join(result.SessionDir, entry.Name()), filepath.Join(providerHome, entry.Name())); err != nil {
			return
		}
	}
	if err = os.Symlink(filepath.Join(result.SessionDir, "sessions"), filepath.Join(providerHome, "sessions")); err != nil {
		return
	}
	config, configErr := materialize.RestrictedLocalCodexConfig(req, home, providerHome, result.SessionDir)
	if configErr != nil {
		err = configErr
		return
	}
	if err = os.WriteFile(filepath.Join(providerHome, "config.toml"), config, 0600); err != nil {
		return
	}
	values := map[string]string{"HOME": home, "CODEX_HOME": providerHome, "PATH": os.Getenv("PATH"), "SHELL": "/bin/sh"}
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
	return
}
