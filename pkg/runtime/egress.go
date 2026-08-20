package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type EgressErrorCode string

const (
	EgressPreflightFailed EgressErrorCode = "egress_preflight_failed"
	EgressDenied          EgressErrorCode = "egress_denied"
)

// EgressError is a stable runtime-boundary failure that always attributes the
// exact CONNECT host without retaining request data or headers.
type EgressError struct {
	Code EgressErrorCode
	Host string
	Stage string
	Err  error
}

func (err *EgressError) Error() string {
	if err == nil {
		return "<nil>"
	}
	if err.Host != "" {
		return fmt.Sprintf("%s: CONNECT host %q", err.Code, err.Host)
	}
	return fmt.Sprintf("%s: %s", err.Code, err.Stage)
}

func (err *EgressError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Err
}

func (m *NetworkManager) preflightPolicyEgress(ctx context.Context, spec PolicyNetworkSpec, image string) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	for _, host := range spec.AllowedHosts {
		var probeErr error
		for attempt := 0; attempt < 2; attempt++ {
			probeCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
			_, probeErr = dockerOutputHost(probeCtx, m.dockerHost(),
				"run", "--rm", "--read-only", "--cap-drop", "ALL",
				"--security-opt", "no-new-privileges:true", "--tmpfs", "/tmp:rw,nosuid,nodev,size=1m",
				"--network", spec.NetworkName(),
				"--entrypoint", "curl",
				image,
				"--head", "--silent", "--show-error", "--output", "/dev/null",
				"--connect-timeout", "5", "--max-time", "10",
				"--proxy", m.proxyURL(),
				"https://"+host+"/",
			)
			cancel()
			if probeErr == nil {
				break
			}
		}
		if probeErr != nil {
			return &EgressError{Code: EgressPreflightFailed, Host: host, Err: probeErr}
		}
	}
	return nil
}

func (m *NetworkManager) inspectPolicyEgressFailure(ctx context.Context, spec PolicyNetworkSpec) error {
	if !spec.Diagnostics {
		return nil
	}
	raw, err := m.policyProxyLogs(ctx, spec.ProxyContainerName())
	if err != nil {
		return err
	}
	allowed := make(map[string]struct{}, len(spec.AllowedHosts))
	for _, host := range spec.AllowedHosts {
		allowed[host] = struct{}{}
	}
	for _, record := range parsePolicyEgressDiagnosticRecords(raw) {
		if _, ok := allowed[record.ConnectHost]; !ok {
			return &EgressError{Code: EgressDenied, Host: record.ConnectHost}
		}
	}
	return nil
}

func (m *NetworkManager) policyProxyLogs(ctx context.Context, proxy string) (string, error) {
	// Squid's stdio access-log module buffers writes. A rotate signal flushes
	// the timestamp/host-only records before AgentD inspects or retains them.
	_, _ = dockerOutputHost(ctx, m.dockerHost(), "exec", proxy, "squid", "-k", "rotate")
	return dockerOutputHost(ctx, m.dockerHost(), "exec", proxy, "sh", "-c",
		"cat /var/log/squid/agentd-egress.log.0 /var/log/squid/agentd-egress.log 2>/dev/null || true")
}

func parsePolicyEgressDiagnosticRecords(raw string) []policyEgressDiagnosticRecord {
	records := make([]policyEgressDiagnosticRecord, 0)
	scanner := bufio.NewScanner(strings.NewReader(raw))
	for scanner.Scan() {
		fields := strings.Fields(strings.ReplaceAll(scanner.Text(), `\t`, "\t"))
		if len(fields) != 2 || !canonicalEgressHost(fields[1]) {
			continue
		}
		timestamp, ok := parseSquidDiagnosticTimestamp(fields[0])
		if !ok {
			continue
		}
		records = append(records, policyEgressDiagnosticRecord{Timestamp: timestamp, ConnectHost: fields[1]})
	}
	return records
}

func encodePolicyEgressDiagnosticRecords(encoder *json.Encoder, records []policyEgressDiagnosticRecord) error {
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			return err
		}
	}
	return nil
}
