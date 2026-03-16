package runtime

import (
	"errors"
	"fmt"
	"sort"
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

func buildSpawnEnv(extra map[string]string) ([]string, error) {
	if len(extra) == 0 {
		return []string{}, nil
	}

	keys := make([]string, 0, len(extra))
	for key := range extra {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	env := make([]string, 0, len(keys))
	for _, key := range keys {
		if err := validateEnvKey(key); err != nil {
			return nil, fmt.Errorf("invalid env key %q: %w", key, err)
		}
		if _, reserved := reservedEnvKeys[key]; reserved {
			return nil, fmt.Errorf("env key %q is reserved", key)
		}
		env = append(env, key+"="+extra[key])
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
