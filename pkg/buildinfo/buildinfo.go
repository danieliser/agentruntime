// Package buildinfo owns the canonical identity embedded in an AgentD binary.
package buildinfo

import (
	"fmt"
	"runtime/debug"
	"strings"
)

const ReleaseVersion = "2.3.3"

// Version and Commit are set by release builds with -ldflags. Development
// builds fall back to Go's VCS metadata for the commit when it is available.
var (
	Version = "dev"
	Commit  = "unknown"
)

// Identity is the exact source identity of one AgentD artifact.
type Identity struct {
	Version string `json:"agentd_version"`
	Commit  string `json:"commit_hash"`
}

// Current returns the identity embedded in the running process.
func Current() Identity {
	commit := Commit
	if commit == "" || commit == "unknown" {
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, setting := range info.Settings {
				if setting.Key == "vcs.revision" && setting.Value != "" {
					commit = setting.Value
					break
				}
			}
		}
	}
	if commit == "" {
		commit = "unknown"
	}
	return Identity{Version: Version, Commit: commit}
}

// Verify rejects an installed binary unless both its version and commit match
// the caller's exact version@commit requirement.
func (identity Identity) Verify(required string) error {
	version, commit, ok := strings.Cut(required, "@")
	if !ok || version == "" || commit == "" || strings.Contains(commit, "@") {
		return fmt.Errorf("build requirement must use exact version@commit form")
	}
	if identity.Version == "" || identity.Commit == "" || identity.Commit == "unknown" {
		return fmt.Errorf("installed AgentD build is not verifiable: version=%q commit=%q", identity.Version, identity.Commit)
	}
	if identity.Version != version {
		return fmt.Errorf("AgentD version mismatch: installed %q, required %q", identity.Version, version)
	}
	if identity.Commit != commit {
		return fmt.Errorf("AgentD commit mismatch: installed %q, required %q", identity.Commit, commit)
	}
	return nil
}
