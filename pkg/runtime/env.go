package runtime

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

var reservedEnvKeys = map[string]struct{}{
	"DYLD_FRAMEWORK_PATH":   {},
	"DYLD_INSERT_LIBRARIES": {},
	"DYLD_LIBRARY_PATH":     {},
	"LD_LIBRARY_PATH":       {},
	"LD_PRELOAD":            {},
	"PATH":                  {},
}

// buildSpawnEnv builds the environment for a spawned process.
// When extra is empty, returns nil — Go's exec.Cmd inherits the parent env.
// When extra has entries, they are merged ON TOP of the parent env (os.Environ).
// Reserved keys (PATH, LD_PRELOAD, etc.) cannot be overridden via extra.
func buildSpawnEnv(extra map[string]string) ([]string, error) {
	if len(extra) == 0 {
		// nil tells exec.Cmd to inherit parent environment.
		return nil, nil
	}

	// Validate extra keys first.
	for key := range extra {
		if err := validateEnvKey(key); err != nil {
			return nil, fmt.Errorf("invalid env key %q: %w", key, err)
		}
		if _, reserved := reservedEnvKeys[key]; reserved {
			return nil, fmt.Errorf("env key %q is reserved", key)
		}
	}

	// Start with parent env, then overlay extra vars.
	env := os.Environ()
	for key, val := range extra {
		env = append(env, key+"="+val)
	}

	return env, nil
}

func validateEnvKey(key string) error {
	if key == "" {
		return errors.New("must not be empty")
	}
	if strings.Contains(key, "=") {
		return errors.New("must not contain '='")
	}
	if strings.ContainsAny(key, " \t\r\n") {
		return errors.New("must not contain whitespace")
	}
	if strings.IndexByte(key, 0) >= 0 {
		return errors.New("must not contain NUL")
	}
	return nil
}
