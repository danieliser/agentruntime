package materialize

import (
	"encoding/json"
	"os"
	"path/filepath"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
)

// RestrictedClaudeSettings is shared by launch and no-turn qualification.
// Bash remains absent from --tools until its production sandbox can be measured.
type RestrictedClaudeSettings struct {
	DisableAllHooks bool                        `json:"disableAllHooks"`
	FastMode        bool                        `json:"fastMode,omitempty"`
	Permissions     RestrictedClaudePermissions `json:"permissions"`
	Sandbox         RestrictedClaudeSandbox     `json:"sandbox"`
}
type RestrictedClaudePermissions struct {
	Allow                               []string `json:"allow"`
	Ask                                 []string `json:"ask"`
	Deny                                []string `json:"deny"`
	BlockReadsOutsideWorkingDirectories bool     `json:"blockReadsOutsideWorkingDirectories"`
}
type RestrictedClaudeSandbox struct {
	Enabled                  bool                       `json:"enabled"`
	FailIfUnavailable        bool                       `json:"failIfUnavailable"`
	AutoAllowBashIfSandboxed bool                       `json:"autoAllowBashIfSandboxed"`
	AllowUnsandboxedCommands bool                       `json:"allowUnsandboxedCommands"`
	ExcludedCommands         []string                   `json:"excludedCommands"`
	Filesystem               RestrictedClaudeFilesystem `json:"filesystem"`
	Network                  RestrictedClaudeNetwork    `json:"network"`
}
type RestrictedClaudeFilesystem struct {
	AllowRead  []string `json:"allowRead"`
	AllowWrite []string `json:"allowWrite"`
	DenyRead   []string `json:"denyRead"`
	DenyWrite  []string `json:"denyWrite"`
}
type RestrictedClaudeNetwork struct {
	AllowedDomains      []string `json:"allowedDomains"`
	DeniedDomains       []string `json:"deniedDomains"`
	AllowLocalBinding   bool     `json:"allowLocalBinding"`
	AllowAllUnixSockets bool     `json:"allowAllUnixSockets"`
	AllowUnixSockets    []string `json:"allowUnixSockets"`
}

func RestrictedLocalClaudeSettings(req *apischema.SessionRequest, home, providerHome string) ([]byte, error) {
	shared := []string{"/tmp", "/private/tmp", "/var/tmp", "/private/var/tmp"}
	reads := []string{req.WorkDir, home, "/bin", "/sbin", "/usr", "/System", "/Library/Apple", "/opt/homebrew", "/dev/null", "/dev/urandom"}
	reads = append(reads, restrictedLocalInstructionFiles(req.WorkDir, "AGENTS.md", "AGENTS.override.md", "CLAUDE.md")...)
	denies := append([]string{"/", providerHome}, shared...)
	if owner, err := os.UserHomeDir(); err == nil && owner != home {
		denies = append(denies, owner)
	}
	settings := RestrictedClaudeSettings{
		DisableAllHooks: true, FastMode: req.Fast,
		// Read/Edit are Claude's effective path rules, including Write and searches.
		// Keep the explicit Write entry too; current Claude accepts but ignores Write(path).
		Permissions: RestrictedClaudePermissions{Allow: []string{}, Ask: []string{"Edit", "Write", "Bash"}, Deny: []string{"Read(/" + providerHome + "/**)", "Edit(/" + providerHome + "/**)", "Write(/" + providerHome + "/**)"}, BlockReadsOutsideWorkingDirectories: true},
		Sandbox: RestrictedClaudeSandbox{Enabled: true, FailIfUnavailable: true, ExcludedCommands: []string{},
			Filesystem: RestrictedClaudeFilesystem{AllowRead: reads, AllowWrite: []string{req.WorkDir, home}, DenyRead: denies, DenyWrite: append(append([]string{}, shared...), providerHome, filepath.Join(req.WorkDir, ".git"), filepath.Join(req.WorkDir, ".claude"))},
			Network:    RestrictedClaudeNetwork{AllowedDomains: []string{}, DeniedDomains: []string{"*"}, AllowUnixSockets: []string{}},
		},
	}
	return json.MarshalIndent(settings, "", "  ")
}
