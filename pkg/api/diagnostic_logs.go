package api

import (
	"encoding/json"
	"sort"
	"strings"
)

// diagnosticRedactions returns request-owned values that must never appear in
// the best-effort diagnostic mirror. It does not affect canonical events,
// replay bytes, results, or receipts.
func diagnosticRedactions(request SessionRequest) []string {
	values := make(map[string]struct{})
	add := func(value string) {
		if value != "" {
			values[value] = struct{}{}
		}
	}
	add(request.Prompt)
	for _, line := range strings.Split(request.Prompt, "\n") {
		add(strings.TrimSpace(line))
	}
	grants := make(map[string]struct{}, len(request.SecretGrants))
	for _, name := range request.SecretGrants {
		grants[name] = struct{}{}
	}
	for name, value := range request.Env {
		_, granted := grants[name]
		if !granted && !diagnosticSensitiveEnvName(name) {
			continue
		}
		add(value)
		var decoded any
		if json.Unmarshal([]byte(value), &decoded) == nil {
			collectDiagnosticJSONStrings(decoded, add)
		}
	}
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func diagnosticSensitiveEnvName(name string) bool {
	upper := strings.ToUpper(name)
	for _, marker := range []string{"TOKEN", "SECRET", "PASSWORD", "AUTH", "COOKIE", "CREDENTIAL", "API_KEY", "ACCESS_KEY", "PRIVATE_KEY"} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

func collectDiagnosticJSONStrings(value any, add func(string)) {
	switch typed := value.(type) {
	case string:
		add(typed)
	case []any:
		for _, item := range typed {
			collectDiagnosticJSONStrings(item, add)
		}
	case map[string]any:
		for _, item := range typed {
			collectDiagnosticJSONStrings(item, add)
		}
	}
}
