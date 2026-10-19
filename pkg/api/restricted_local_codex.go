package api

import (
	"github.com/danieliser/agentruntime/pkg/materialize"

	"github.com/danieliser/agentruntime/pkg/durable"
)

// CLI overrides have highest precedence. Mark every possible project ancestor
// untrusted so Codex cannot load project-local config, hook or rule layers,
// even when a work_dir is nested in a repository or reached through a symlink.
func restrictedLocalCodexArgs(workDir string) ([]string, error) {
	args, err := materialize.RestrictedLocalCodexArgs(workDir)
	if err != nil {
		return nil, durable.NewError(durable.CodeExecutionPolicyUnsupported, "restricted_local_codex", "cannot resolve project work_dir", err)
	}
	return args, nil
}
