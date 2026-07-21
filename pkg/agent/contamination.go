package agent

// KnownContamination returns the context-leak residuals that remain for an
// agent after clean-context materialization. All entries were established by
// probing the CLI (asking the model to enumerate its own context) on
// 2026-07-20/21 — config inspection alone misses these.
//
// Clean-context residuals:
//   - claude: --safe-mode (probed clean 2026-07-21 on 2.1.216) leaves only
//     the CLI's default bundled skills, which ship with the binary.
//   - cursor: 4+ account-level user rules sync SERVER-SIDE into every
//     session; not strippable locally. Present in every mode, not just clean.
//   - grok: 4 built-in CLI skills (check-work, create-skill, help, imagine)
//     ship with the binary and survive the fake-HOME strip.
//   - codex: built-in skills (imagegen, openai-docs, plugin-creator,
//     skill-creator, skill-installer) ship with codex 0.144; the clean
//     CODEX_HOME probe showed no user MCP servers and no global AGENTS.md.
//
// Without clean context, local-runtime sessions inherit whatever the host
// environment provides; that is reported as "host-config" rather than
// enumerated.
func KnownContamination(agentName string, cleanContext bool) []string {
	if !cleanContext {
		residual := []string{"host-config"}
		if agentName == "cursor" {
			residual = append(residual, "cursor-account-rules")
		}
		return residual
	}

	switch agentName {
	case "claude":
		return []string{"claude-bundled-skills"}
	case "codex":
		return []string{"codex-builtin-skills"}
	case "cursor":
		return []string{"cursor-account-rules"}
	case "grok":
		return []string{"grok-builtin-skills"}
	default:
		return nil
	}
}
