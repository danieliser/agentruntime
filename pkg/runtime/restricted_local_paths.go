package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/danieliser/agentruntime/pkg/durable"
)

func pathWithin(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}

// Both directions of workspace/data overlap are unsafe: a workspace grant must
// never expose master credentials or sibling session homes through an ancestor.
func validateRestrictedLocalPaths(workDir, dataDir string) error {
	reject := func() error {
		return durable.NewError(durable.CodeExecutionPolicyUnsupported, "restricted_local", "workspace must be a project outside provider/system roots and AgentD storage", nil)
	}
	work, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		return reject()
	}
	data, err := filepath.Abs(dataDir)
	if err != nil || dataDir == "" {
		return reject()
	}
	if resolved, resolveErr := filepath.EvalSymlinks(data); resolveErr == nil {
		data = resolved
	}
	if pathWithin(work, data) || pathWithin(data, work) {
		return reject()
	}
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		if resolved, resolveErr := filepath.EvalSymlinks(home); resolveErr == nil {
			home = resolved
		}
		if pathWithin(work, home) {
			return reject()
		}
	}
	for _, system := range []string{"/usr", "/bin", "/sbin", "/System", "/Library", "/opt/homebrew"} {
		if pathWithin(system, work) || pathWithin(work, system) || pathWithin(system, data) {
			return reject()
		}
	}
	return nil
}

func (*LocalRuntime) CheckRequestAdmission(_ context.Context, cfg SpawnConfig) error {
	if !restrictedLocalRequest(cfg) {
		return nil
	}
	return validateRestrictedLocalPaths(cfg.WorkDir, cfg.DataDir)
}
