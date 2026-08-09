package observer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

type Policy string

const (
	PolicyBestEffort Policy = "best_effort"
	PolicyRequired   Policy = "required"
)

const defaultTimeout = 5 * time.Second

var safeEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type Config struct {
	Version string
	Plugins []PluginConfig
}

type PluginConfig struct {
	Name        string
	Enabled     bool
	Command     string
	Args        []string
	Environment map[string]string
	Policy      Policy
	Timeout     time.Duration
}

type diskConfig struct {
	Version string             `json:"version"`
	Plugins []diskPluginConfig `json:"plugins"`
}

type diskPluginConfig struct {
	Name        string            `json:"name"`
	Enabled     bool              `json:"enabled"`
	Command     string            `json:"command"`
	Args        []string          `json:"args,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
	Policy      Policy            `json:"policy,omitempty"`
	Timeout     string            `json:"timeout,omitempty"`
}

func LoadOptionalConfig(path string) (Config, error) {
	config, err := LoadConfig(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{Version: "1"}, nil
	}
	return config, err
}

func LoadConfig(path string) (Config, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Config{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return Config{}, fmt.Errorf("observer: plugin config must be a private regular file (mode 0600 or stricter)")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var disk diskConfig
	if err := decoder.Decode(&disk); err != nil {
		return Config{}, fmt.Errorf("observer: decode plugin config: %w", err)
	}
	if disk.Version != "1" {
		return Config{}, fmt.Errorf("observer: unsupported config version %q", disk.Version)
	}
	config := Config{Version: disk.Version, Plugins: make([]PluginConfig, 0, len(disk.Plugins))}
	seen := make(map[string]struct{}, len(disk.Plugins))
	for _, candidate := range disk.Plugins {
		plugin, err := normalizePluginConfig(candidate)
		if err != nil {
			return Config{}, err
		}
		if _, exists := seen[plugin.Name]; exists {
			return Config{}, fmt.Errorf("observer: duplicate plugin %q", plugin.Name)
		}
		seen[plugin.Name] = struct{}{}
		config.Plugins = append(config.Plugins, plugin)
	}
	return config, nil
}

func normalizePluginConfig(candidate diskPluginConfig) (PluginConfig, error) {
	if !safeName.MatchString(candidate.Name) {
		return PluginConfig{}, fmt.Errorf("observer: invalid plugin name %q", candidate.Name)
	}
	if candidate.Enabled && (!filepath.IsAbs(candidate.Command) || filepath.Clean(candidate.Command) != candidate.Command) {
		return PluginConfig{}, fmt.Errorf("observer: plugin %q command must be an explicit absolute path", candidate.Name)
	}
	if candidate.Policy == "" {
		candidate.Policy = PolicyBestEffort
	}
	if candidate.Policy != PolicyBestEffort && candidate.Policy != PolicyRequired {
		return PluginConfig{}, fmt.Errorf("observer: plugin %q has invalid policy %q", candidate.Name, candidate.Policy)
	}
	timeout := defaultTimeout
	if candidate.Timeout != "" {
		parsed, err := time.ParseDuration(candidate.Timeout)
		if err != nil || parsed <= 0 {
			return PluginConfig{}, fmt.Errorf("observer: plugin %q has invalid timeout", candidate.Name)
		}
		timeout = parsed
	}
	for key := range candidate.Environment {
		if !safeEnvName.MatchString(key) {
			return PluginConfig{}, fmt.Errorf("observer: plugin %q has invalid environment key %q", candidate.Name, key)
		}
	}
	return PluginConfig{
		Name: candidate.Name, Enabled: candidate.Enabled, Command: candidate.Command,
		Args: append([]string(nil), candidate.Args...), Environment: cloneStrings(candidate.Environment),
		Policy: candidate.Policy, Timeout: timeout,
	}, nil
}

func cloneStrings(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
