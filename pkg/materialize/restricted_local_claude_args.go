package materialize

// RestrictedLocalClaudeArgs is the canonical file-tools-only launch, shared by
// the driver and no-model qualification. No caller may add executable tools.
func RestrictedLocalClaudeArgs(permissionMode string) []string {
	prompts := "host"
	if permissionMode == "dontAsk" {
		prompts = "none"
	} else {
		permissionMode = "default"
	}
	return []string{"--permission-mode", permissionMode, "--restricted", "--print", "--tools", "Read,Glob,Grep,Edit,Write", "--permission-prompts", prompts,
		"--setting-sources", "", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--safe-mode", "--disable-slash-commands",
		"--output-format", "stream-json", "--input-format", "stream-json", "--permission-prompt-tool", "stdio", "--verbose", "--include-partial-messages", "--forward-subagent-text"}
}
