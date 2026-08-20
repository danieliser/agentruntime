package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
)

const (
	policyNetworkPrefix = "agentruntime-policy-"
	policyProxyPrefix   = "agentruntime-proxy-"
)

const (
	policySessionLabelKey = "agentruntime.policy_session"
	policyHashLabelKey    = "agentruntime.policy_hash"
)

type policyEnsure struct {
	once sync.Once
	err  error
}

var sha256PolicyHashPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// PolicyNetworkSpec identifies one session-private, default-deny egress
// network and its exact-host proxy policy.
type PolicyNetworkSpec struct {
	PolicyHash   string
	SessionID    string
	AllowedHosts []string
}

func (s PolicyNetworkSpec) Validate() error {
	if !sha256PolicyHashPattern.MatchString(s.PolicyHash) {
		return fmt.Errorf("policy hash must be a canonical SHA-256 digest")
	}
	if !safeRuntimeSessionID(s.SessionID) {
		return fmt.Errorf("policy network session ID is unsafe")
	}
	previous := ""
	for _, host := range s.AllowedHosts {
		if host == "" || host != strings.ToLower(host) || strings.ContainsAny(host, "*:/") || strings.HasPrefix(host, ".") || net.ParseIP(host) != nil {
			return fmt.Errorf("egress host %q is not an exact lowercase DNS host", host)
		}
		if previous != "" && host <= previous {
			return fmt.Errorf("egress hosts must be unique and sorted")
		}
		previous = host
	}
	return nil
}

func (s PolicyNetworkSpec) suffix() string {
	hash := strings.TrimPrefix(s.PolicyHash, "sha256:")
	return hash[:8] + "-" + sessionIDPrefix(s.SessionID)
}

func (s PolicyNetworkSpec) NetworkName() string {
	return policyNetworkPrefix + s.suffix()
}

func (s PolicyNetworkSpec) ProxyContainerName() string {
	return policyProxyPrefix + s.suffix()
}

func policyNetworkSpec(cfg SpawnConfig) (PolicyNetworkSpec, error) {
	if cfg.Request == nil || cfg.Request.ExecutionPolicy == nil {
		return PolicyNetworkSpec{}, fmt.Errorf("execution policy is required")
	}
	hash := cfg.ExecutionPolicyHash
	if hash == "" {
		raw, err := json.Marshal(cfg.Request.ExecutionPolicy)
		if err != nil {
			return PolicyNetworkSpec{}, fmt.Errorf("encode execution policy: %w", err)
		}
		digest := sha256.Sum256(raw)
		hash = "sha256:" + hex.EncodeToString(digest[:])
	}
	hosts := append([]string(nil), cfg.Request.ExecutionPolicy.EgressAllowlist...)
	sort.Strings(hosts)
	spec := PolicyNetworkSpec{PolicyHash: hash, SessionID: cfg.SessionID, AllowedHosts: hosts}
	if err := spec.Validate(); err != nil {
		return PolicyNetworkSpec{}, err
	}
	return spec, nil
}

// RenderPolicyProxyConfig builds a non-logging CONNECT-only Squid policy.
// Exact host tokens deliberately omit Squid's leading-dot subdomain syntax.
func RenderPolicyProxyConfig(hosts []string) (string, error) {
	canonical := append([]string(nil), hosts...)
	sort.Strings(canonical)
	spec := PolicyNetworkSpec{
		PolicyHash:   "sha256:" + strings.Repeat("0", 64),
		SessionID:    "config-render",
		AllowedHosts: canonical,
	}
	if err := spec.Validate(); err != nil {
		return "", err
	}
	var builder strings.Builder
	builder.WriteString("http_port 3128\n")
	builder.WriteString("access_log none\ncache_log /dev/null\ncache_store_log none\nlogfile_rotate 0\n")
	builder.WriteString("cache deny all\ncache_dir null /tmp\n")
	builder.WriteString("acl SSL_ports port 443\nacl CONNECT method CONNECT\n")
	builder.WriteString("http_access deny !CONNECT\nhttp_access deny CONNECT !SSL_ports\n")
	for index, host := range canonical {
		fmt.Fprintf(&builder, "acl allowed_host_%d dstdomain %s\n", index, host)
		fmt.Fprintf(&builder, "http_access allow CONNECT allowed_host_%d\n", index)
	}
	builder.WriteString("http_access deny all\n")
	return builder.String(), nil
}

func (m *NetworkManager) writePolicyProxyConfig(spec PolicyNetworkSpec) (string, error) {
	config, err := RenderPolicyProxyConfig(spec.AllowedHosts)
	if err != nil {
		return "", err
	}
	root := m.DataDir
	if root == "" {
		root = os.TempDir()
	}
	dir := filepath.Join(root, "network-policies", strings.TrimPrefix(spec.PolicyHash, "sha256:"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create policy proxy config directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("harden policy proxy config directory: %w", err)
	}
	path := filepath.Join(dir, "squid.conf")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		return "", fmt.Errorf("write policy proxy config: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", fmt.Errorf("harden policy proxy config: %w", err)
	}
	return path, nil
}

func executionPolicyHosts(request *apischema.SessionRequest) []string {
	if request == nil || request.ExecutionPolicy == nil {
		return nil
	}
	return append([]string(nil), request.ExecutionPolicy.EgressAllowlist...)
}

func (m *NetworkManager) EnsurePolicyProxy(ctx context.Context, spec PolicyNetworkSpec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	key := spec.ProxyContainerName()
	value, _ := m.policyEnsures.LoadOrStore(key, &policyEnsure{})
	ensure := value.(*policyEnsure)
	ensure.once.Do(func() {
		ensure.err = m.ensurePolicyProxyOnce(ctx, spec)
	})
	if ensure.err != nil {
		m.policyEnsures.Delete(key)
	}
	return ensure.err
}

func (m *NetworkManager) ensurePolicyProxyOnce(ctx context.Context, spec PolicyNetworkSpec) error {
	if err := m.EnsureNetwork(ctx); err != nil {
		return err
	}
	if !dockerNetworkExists(ctx, m.dockerHost(), spec.NetworkName()) {
		if _, err := dockerOutputHost(ctx, m.dockerHost(),
			"network", "create", "--driver", "bridge", "--internal",
			"--label", policySessionLabelKey+"="+spec.SessionID,
			"--label", policyHashLabelKey+"="+spec.PolicyHash,
			spec.NetworkName(),
		); err != nil && !strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("create policy network %q: %w", spec.NetworkName(), err)
		}
	}

	configPath, err := m.writePolicyProxyConfig(spec)
	if err != nil {
		return err
	}
	proxyName := spec.ProxyContainerName()
	running, inspectErr := dockerInspectRunningHost(ctx, m.dockerHost(), proxyName)
	if inspectErr == nil && running {
		return m.preparePolicyProxy(ctx, spec)
	}
	if inspectErr == nil {
		if err := dockerRemoveContainerHost(ctx, m.dockerHost(), proxyName); err != nil {
			return err
		}
	} else if !dockerObjectMissing(inspectErr) {
		return fmt.Errorf("inspect policy proxy %q: %w", proxyName, inspectErr)
	}
	if _, err := dockerOutputHost(ctx, m.dockerHost(),
		"run", "-d", "--name", proxyName,
		"--network", m.networkName(),
		"--read-only",
		"--tmpfs", "/run:rw,nosuid,nodev,size=1m",
		"--tmpfs", "/var/log/squid:rw,nosuid,nodev,size=1m",
		"--tmpfs", "/var/spool/squid:rw,nosuid,nodev,size=8m",
		"--log-driver", "none",
		"--label", policySessionLabelKey+"="+spec.SessionID,
		"--label", policyHashLabelKey+"="+spec.PolicyHash,
		"--volume", configPath+":"+"/etc/squid/squid.conf:ro",
		m.proxyImage(),
	); err != nil {
		if !strings.Contains(err.Error(), "already in use") {
			return fmt.Errorf("start policy proxy %q: %w", proxyName, err)
		}
	}
	return m.preparePolicyProxy(ctx, spec)
}

func (m *NetworkManager) preparePolicyProxy(ctx context.Context, spec PolicyNetworkSpec) error {
	_, err := dockerOutputHost(ctx, m.dockerHost(), "network", "connect", "--alias", dockerProxyContainerName, spec.NetworkName(), spec.ProxyContainerName())
	if err != nil && !strings.Contains(err.Error(), "already exists") && !strings.Contains(err.Error(), "already connected") {
		return fmt.Errorf("connect policy proxy to session network: %w", err)
	}
	return m.waitForNamedProxyReady(ctx, spec.ProxyContainerName())
}

func (m *NetworkManager) ReleasePolicySession(ctx context.Context, sessionID string) error {
	if !safeRuntimeSessionID(sessionID) {
		return fmt.Errorf("release policy session: unsafe session ID")
	}
	return m.cleanupPolicyNetworks(ctx, sessionID)
}

func (m *NetworkManager) cleanupPolicyNetworks(ctx context.Context, sessionID string) error {
	filter := "label=" + policySessionLabelKey
	if sessionID != "" {
		filter += "=" + sessionID
	}
	var errs []error
	containers, err := dockerOutputHost(ctx, m.dockerHost(), "ps", "-aq", "--no-trunc", "--filter", filter)
	if err != nil && !dockerObjectMissing(err) {
		errs = append(errs, fmt.Errorf("list policy proxies: %w", err))
	} else {
		for _, containerID := range strings.Fields(containers) {
			if _, removeErr := dockerOutputHost(ctx, m.dockerHost(), "rm", "-f", containerID); removeErr != nil && !dockerObjectMissing(removeErr) {
				errs = append(errs, fmt.Errorf("remove policy proxy %q: %w", containerID, removeErr))
			}
		}
	}
	networks, err := dockerOutputHost(ctx, m.dockerHost(), "network", "ls", "-q", "--filter", filter)
	if err != nil && !dockerObjectMissing(err) {
		errs = append(errs, fmt.Errorf("list policy networks: %w", err))
	} else {
		for _, networkID := range strings.Fields(networks) {
			if _, removeErr := dockerOutputHost(ctx, m.dockerHost(), "network", "rm", networkID); removeErr != nil && !dockerObjectMissing(removeErr) {
				errs = append(errs, fmt.Errorf("remove policy network %q: %w", networkID, removeErr))
			}
		}
	}
	return errors.Join(errs...)
}
