package runtime

import (
	"crypto/sha256"
	_ "embed"
	"fmt"
	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
	"os"
)

const codexSeccompSHA256 = "4d9767cdcdc79338c0ce172baab5042d1c5a7d847a0d73a10e308f54c44a286d"

//go:embed codex-sandbox/seccomp.json
var codexSeccompProfile []byte

func validateCodexSeccompProfile(raw []byte) error {
	if fmt.Sprintf("%x", sha256.Sum256(raw)) != codexSeccompSHA256 {
		return fmt.Errorf("restricted Codex sandbox profile failed integrity validation")
	}
	return nil
}

// prepareCodexSeccomp is host-owned and never accepts a caller profile path.
// Docker reads the private file at container creation; the compiled filter
// survives host reconnects and the temporary file is cleaned with run resources.
func prepareCodexSeccomp(request *apischema.SessionRequest) (string, func(), error) {
	noop := func() {}
	if request == nil || request.Agent != "codex" || request.ExecutionPolicy != nil || request.ProviderPermissions == nil || request.ProviderPermissions.Mode != "restricted" {
		return "", noop, nil
	}
	if request.Container != nil && len(request.Container.SecurityOpt) > 0 {
		return "", noop, fmt.Errorf("restricted Codex coding cannot override container security controls")
	}
	if err := validateCodexSeccompProfile(codexSeccompProfile); err != nil {
		return "", noop, err
	}
	f, err := os.CreateTemp("", "agentruntime-codex-seccomp-*.json")
	if err != nil {
		return "", noop, fmt.Errorf("create restricted Codex sandbox profile: %w", err)
	}
	cleanup := func() { _ = os.Remove(f.Name()) }
	if _, err = f.Write(codexSeccompProfile); err != nil {
		f.Close()
		cleanup()
		return "", noop, fmt.Errorf("write restricted Codex sandbox profile: %w", err)
	}
	if err = f.Close(); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("close restricted Codex sandbox profile: %w", err)
	}
	return f.Name(), cleanup, nil
}
