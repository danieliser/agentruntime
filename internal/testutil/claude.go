package testutil

// ClaudeQualificationScript simulates control-only readbacks. It is not evidence
// of confinement; only the opt-in installed-binary test measures the interface.
func ClaudeQualificationScript(fallback string) string {
	return `#!/bin/sh
case "$1" in
 --version) printf '2.1.fixture (Claude Code)\n'; exit 0;;
 --help) printf '%s\n' '--restricted confines the file tools --permission-prompts --strict-mcp-config --tools'; exit 0;;
esac
case "$CLAUDE_CONFIG_DIR" in
 */.restricted-claude-probe-*/*)
  for arg do settings="$arg"; done
  mode=default
  case "$*" in *dontAsk*) mode=dontAsk;; esac
  while IFS= read -r line; do
   case "$line" in
    *'"subtype":"initialize"'*) printf '{"type":"control_response","response":{"subtype":"success","request_id":"init","response":{"claude_code_version":"2.1.fixture","current_permission_mode":"%s","commands":[]}}}\n' "$mode";;
    *'"subtype":"get_settings"'*) printf '{"type":"control_response","response":{"subtype":"success","request_id":"settings","response":{"effective":'; /usr/bin/tr -d '\n' < "$settings"; printf '}}}\n';;
    *'"subtype":"list_permission_rules"'*) printf '%s\n' '{"type":"control_response","response":{"subtype":"success","request_id":"rules","response":{"state":{"rules":[{"behavior":"ask","rule":"Edit"},{"behavior":"ask","rule":"Write"},{"behavior":"ask","rule":"Bash"},{"behavior":"deny","rule":"Bash"},{"behavior":"deny","rule":"Agent"},{"behavior":"deny","rule":"WebFetch"},{"behavior":"deny","rule":"WebSearch"},{"behavior":"deny","rule":"NotebookEdit"}]}}}}';;
    *'"subtype":"set_permission_mode"'*) printf '%s\n' '{"type":"control_response","response":{"subtype":"error","request_id":"bypass","error_code":"bypass_restricted"}}';;
    *) exit 80;;
   esac
  done
  exit 0;;
esac
` + fallback + "\n"
}
