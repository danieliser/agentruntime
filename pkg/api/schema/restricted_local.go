package schema

const RestrictedLocalCodexProfile = "agentd-local-v1"

// RestrictedLocalEnvAllowed is the complete caller environment grant for a
// provider-isolated local worker. Unknown variables cannot silently become
// provider configuration or process-injection controls in a later CLI version.
func RestrictedLocalEnvAllowed(name string) bool {
	switch name {
	case "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "OPENAI_API_KEY",
		"LANG", "LC_ALL", "LC_CTYPE", "LC_MESSAGES", "LANGUAGE",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "no_proxy", "all_proxy":
		return true
	default:
		return false
	}
}
