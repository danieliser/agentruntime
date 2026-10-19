package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
)

func probeRestrictedLocalCodex(ctx context.Context, binary, dataDir string) (string, error) {
	return measureRestrictedLocalCodex(ctx, binary, dataDir, nil)
}

func measureRestrictedLocalCodex(ctx context.Context, binary, dataDir string, passed func(string)) (version string, err error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return "", err
	}
	versionHome, err := os.MkdirTemp(dataDir, ".restricted-local-version-")
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(versionHome)) }()
	versionCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := restrictedLocalProbeCommand(versionCtx, binary, "--version")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + versionHome, "CODEX_HOME=" + versionHome, "TMPDIR=" + versionHome}
	output, err := cmd.Output()
	version = strings.TrimSpace(string(output))
	if err != nil || !strings.HasPrefix(version, "codex-cli ") {
		return version, errors.New("cannot measure Codex version")
	}
	return version, runRestrictedLocalCodexProbes(ctx, binary, dataDir, passed)
}

// Synthetic-only fixtures: neither credentials nor prompts go to a provider.
// Every denied file has a readable host baseline. A live ephemeral loopback
// listener establishes network availability before testing the sandbox boundary.
func runRestrictedLocalCodexProbes(ctx context.Context, binary, dataDir string, passed func(string)) (err error) {
	if err = os.MkdirAll(dataDir, 0700); err != nil {
		return err
	}
	root, err := os.MkdirTemp(dataDir, ".restricted-local-probe-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(root)) }()
	repo := filepath.Join(root, "project.repo")
	work := filepath.Join(repo, ".claude", "worktrees", "my.app")
	storage := filepath.Join(root, "agentd")
	outside := filepath.Join(root, "host-home")
	for _, dir := range []string{work, filepath.Join(outside, ".ssh"), filepath.Join(storage, "credentials"), filepath.Join(storage, "codex-sessions", "other")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	git := restrictedLocalProbeCommand(ctx, "/usr/bin/git", "-c", "init.templateDir=", "init", "-q", repo)
	git.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + outside, "GIT_CONFIG_NOSYSTEM=1"}
	if err := git.Run(); err != nil {
		return errors.New("cannot create probe repository")
	}
	doc := filepath.Join(repo, "AGENTS.md")
	inside := filepath.Join(work, "allow-read")
	other := filepath.Join(outside, "deny-outside")
	master := filepath.Join(storage, "credentials", "codex-auth.json")
	sibling := filepath.Join(storage, "codex-sessions", "other", "auth.json")
	ssh := filepath.Join(outside, ".ssh", "deny-key")
	for _, path := range []string{doc, inside, other, sibling, ssh} {
		if err := os.WriteFile(path, []byte("synthetic-only\n"), 0600); err != nil {
			return err
		}
	}
	if err := os.WriteFile(master, []byte(`{"synthetic_provider_login":"fixture"}`), 0600); err != nil {
		return err
	}
	if err := os.Symlink(other, filepath.Join(work, "deny-symlink")); err != nil {
		return err
	}
	req := &apischema.SessionRequest{Agent: "codex", Runtime: "local", WorkDir: work, ProviderPermissions: &apischema.ProviderPermissions{Mode: "restricted", ApprovalPolicy: "on-request"}}
	_, env, cleanup, err := materializeRestrictedLocal(SpawnConfig{SessionID: "probe", DataDir: storage, WorkDir: work, Cmd: []string{binary}, Request: req})
	if cleanup != nil {
		defer func() { err = errors.Join(err, cleanup()) }()
	}
	if err != nil {
		return err
	}
	values := map[string]string{}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	for _, path := range []string{inside, other, master, sibling, ssh, filepath.Join(values["CODEX_HOME"], "auth.json")} {
		if _, err := os.ReadFile(path); err != nil {
			return errors.New("unreadable probe baseline")
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	response, err := client.Get(server.URL)
	if err != nil {
		return errors.New("network baseline unavailable")
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("network baseline failed")
	}
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer tlsServer.Close()
	tlsClient := tlsServer.Client()
	tlsClient.Timeout = time.Second
	defer tlsClient.CloseIdleConnections()
	tlsResponse, err := tlsClient.Get(tlsServer.URL)
	if err != nil {
		return errors.New("HTTPS baseline unavailable")
	}
	tlsResponse.Body.Close()
	if tlsResponse.StatusCode != http.StatusOK {
		return errors.New("HTTPS baseline failed")
	}
	type probe struct {
		name    string
		allow   bool
		command []string
	}
	// Establish the helper can write the synthetic outside fixture without a
	// sandbox; its intentionally terse write-denial diagnostic omits errno.
	baselinePatch := filepath.Join(outside, "deny-baseline-patch")
	baselineCmd := restrictedLocalProbeCommand(ctx, binary, "--codex-run-as-apply-patch", "*** Begin Patch\n*** Add File: "+baselinePatch+"\n+synthetic-only\n*** End Patch")
	baselineCmd.Env = env
	baselineCmd.Dir = work
	if err := baselineCmd.Run(); err != nil {
		return errors.New("apply_patch host baseline failed")
	}
	if err := os.Remove(baselinePatch); err != nil {
		return errors.New("apply_patch host baseline did not create file")
	}
	patch := func(path string) []string {
		return []string{binary, "--codex-run-as-apply-patch", "*** Begin Patch\n*** Add File: " + path + "\n+synthetic-only\n*** End Patch"}
	}
	tests := []probe{
		{"read-work", true, []string{"/bin/cat", inside}},
		{"read-ancestor-instructions", true, []string{"/bin/cat", doc}},
		{"write-work", true, []string{"/usr/bin/touch", filepath.Join(work, "allow-write")}},
		{"write-own-home", true, []string{"/usr/bin/touch", filepath.Join(values["HOME"], "allow-write")}},
		{"write-private-tmp", true, []string{"/usr/bin/touch", filepath.Join(values["TMPDIR"], "allow-write")}},
		{"apply-patch-work", true, patch(filepath.Join(work, "allow-patch"))},
		{"apply-patch-outside", false, patch(filepath.Join(outside, "deny-patch"))},
		{"apply-patch-provider-home", false, patch(filepath.Join(values["CODEX_HOME"], "deny-patch"))},
		{"read-outside", false, []string{"/bin/cat", other}},
		{"read-master", false, []string{"/bin/cat", master}},
		{"read-other-session", false, []string{"/bin/cat", sibling}},
		{"read-own-auth", false, []string{"/bin/cat", filepath.Join(values["CODEX_HOME"], "auth.json")}},
		{"read-ssh", false, []string{"/bin/cat", ssh}},
		{"read-symlink-escape", false, []string{"/bin/cat", filepath.Join(work, "deny-symlink")}},
		{"write-outside", false, []string{"/usr/bin/touch", filepath.Join(outside, "deny-write")}},
		{"network-loopback", false, []string{"/usr/bin/curl", "--max-time", "2", "--noproxy", "*", "-sS", "-o", "/dev/null", server.URL}},
		{"network-localhost", false, []string{"/usr/bin/curl", "--max-time", "2", "--noproxy", "*", "-sS", "-o", "/dev/null", strings.Replace(server.URL, "127.0.0.1", "localhost", 1)}},
		{"network-https-ip", false, []string{"/usr/bin/curl", "--max-time", "2", "--noproxy", "*", "-ksS", "-o", "/dev/null", tlsServer.URL}},
		{"network-https-name", false, []string{"/usr/bin/curl", "--max-time", "2", "--noproxy", "*", "-ksS", "-o", "/dev/null", strings.Replace(tlsServer.URL, "127.0.0.1", "localhost", 1)}},
	}
	// Probe both physical temporary roots and all public aliases. These scratch
	// files contain only synthetic bytes and are always removed, including errors.
	for _, base := range []string{"/private/tmp", "/private/var/tmp"} {
		tmp, createErr := os.MkdirTemp(base, "agentd-deny-probe-")
		if createErr != nil {
			return errors.New("cannot create temporary-root probe")
		}
		defer func() { err = errors.Join(err, os.RemoveAll(tmp)) }()
		path := filepath.Join(tmp, "deny-read")
		if err := os.WriteFile(path, []byte("synthetic-only"), 0600); err != nil {
			return err
		}
		for _, alias := range []string{base, strings.TrimPrefix(base, "/private")} {
			aliasDir := filepath.Join(alias, filepath.Base(tmp))
			tests = append(tests, probe{"apply-patch-" + alias, false, patch(filepath.Join(aliasDir, "deny-patch"))})
			tests = append(tests, probe{"read-" + alias, false, []string{"/bin/cat", filepath.Join(aliasDir, "deny-read")}}, probe{"write-" + alias, false, []string{"/usr/bin/touch", filepath.Join(aliasDir, "deny-write")}})
		}
	}
	for _, test := range tests {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		args := append([]string{"sandbox", "--permission-profile", apischema.RestrictedLocalCodexProfile, "-C", work, "--"}, test.command...)
		cmd := restrictedLocalProbeCommand(probeCtx, binary, args...)
		cmd.Env = env
		cmd.Dir = work
		output, runErr := cmd.CombinedOutput()
		timedOut := probeCtx.Err() != nil
		cancel()
		if timedOut {
			return fmt.Errorf("probe %s timed out", test.name)
		}
		if test.allow && runErr != nil {
			return fmt.Errorf("probe %s denied required access: %s", test.name, output)
		}
		if !test.allow {
			var exit *exec.ExitError
			if !errors.As(runErr, &exit) {
				return fmt.Errorf("probe %s did not deny access", test.name)
			}
			// Non-network denials must actually be permission failures, not missing
			// commands/files, bad config or another unrelated nonzero process exit.
			text := strings.ToLower(string(output))
			if !strings.Contains(text, "operation not permitted") && !strings.Contains(text, "permission denied") && !(strings.HasPrefix(test.name, "network-") && strings.Contains(text, "curl: (7)")) && !(strings.HasPrefix(test.name, "apply-patch-") && strings.HasPrefix(text, "failed to write file ")) {
				return fmt.Errorf("probe %s failed without a permission denial: %s", test.name, output)
			}
		}
		if test.name == "apply-patch-work" {
			if _, err := os.Stat(filepath.Join(work, "allow-patch")); err != nil {
				return errors.New("apply_patch did not create its allowed file")
			}
		}
		if passed != nil {
			passed(test.name)
		}
	}
	bootstrapCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := probeCodexBootstrap(bootstrapCtx, binary, work, env); err != nil {
		return fmt.Errorf("no-turn bootstrap: %w", err)
	}
	if passed != nil {
		passed("dotted-repo-subdirectory-no-turn-bootstrap-active-profile")
	}
	return nil
}

// Kill descendants as well as the provider on timeout, and bound waiting for
// copied output pipes even if a descendant has retained a descriptor.
func restrictedLocalProbeCommand(ctx context.Context, binary string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, binary, args...)
	configureLocalProcessGroup(cmd)
	cmd.Cancel = func() error { return killLocalProcessGroup(cmd) }
	cmd.WaitDelay = time.Second
	return cmd
}
