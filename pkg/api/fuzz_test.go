package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const fuzzMaxBytes = 8 << 10

func clampFuzzBytes(data []byte) []byte {
	if len(data) > fuzzMaxBytes {
		return data[:fuzzMaxBytes]
	}
	return data
}

func FuzzSessionRequest_JSON(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte(`{}`),
		[]byte(`{"agent":"claude","prompt":"hello"}`),
		[]byte(`{"agent":"codex","prompt":"build it","runtime":"docker","timeout":"5m","pty":true,"interactive":false}`),
		[]byte(`{"agent":"claude","prompt":"go","work_dir":"/tmp","mounts":[{"host":"/a","container":"/b","mode":"rw"}],"claude":{"claude_md":"# hi","credentials_path":"~/.claude/credentials.json","memory_path":"~/.claude/projects","settings_json":{"key":"val"},"mcp_json":{"mcpServers":{}},"max_turns":10,"allowed_tools":["Read"]}}`),
		[]byte(`{"agent":"codex","prompt":"test","codex":{"config_toml":{"model":"o3"},"instructions":"use rg","approval_mode":"suggest"},"mcp_servers":[{"name":"s","type":"http","url":"http://${HOST_GATEWAY}:8080"}],"env":{"FOO":"bar"},"container":{"image":"ubuntu:22.04","memory":"4g","cpus":2.0,"network":"bridge","security_opt":["no-new-privileges:true"]}}`),
		[]byte(`{"agent":"","prompt":"","timeout":"not-a-duration","mounts":[{},{}]}`),
		[]byte(`[1,2,3]`),
		[]byte(`{"agent":"claude","prompt":"x","tags":{"":""},"env":{"":""}}`),
		[]byte(`{"agent":"` + strings.Repeat("a", 4096) + `"}`),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		data = clampFuzzBytes(data)
		var req SessionRequest
		if err := json.Unmarshal(data, &req); err != nil {
			return
		}

		// Exercise methods that process fuzzed fields.
		_ = req.EffectiveMounts()
		_ = req.EffectiveTimeout()

		// Re-marshal should not panic.
		_, _ = json.Marshal(req)
	})
}

func FuzzSessionRequest_EffectiveMounts(f *testing.F) {
	f.Add("", "", "", "", "", "", "")
	f.Add("/tmp/workspace", "", "", "", "", "", "")
	f.Add("", "/host", "/container", "rw", "", "", "")
	f.Add("/tmp/workspace", "/host", "/container", "rw", "/host-two", "/container-two", "ro")
	f.Add("relative/work", "C:\\host", "/workspace/windows", "rw", "${HOME}/project", "/workspace/home", "")

	f.Fuzz(func(t *testing.T, workDir, host1, container1, mode1, host2, container2, mode2 string) {
		req := SessionRequest{
			WorkDir: workDir,
			Mounts: []Mount{
				{Host: host1, Container: container1, Mode: mode1},
				{Host: host2, Container: container2, Mode: mode2},
			},
		}
		original := append([]Mount(nil), req.Mounts...)

		got := req.EffectiveMounts()

		if len(req.Mounts) != len(original) {
			t.Fatalf("expected original mounts length %d, got %d", len(original), len(req.Mounts))
		}
		for i := range original {
			if req.Mounts[i] != original[i] {
				t.Fatalf("expected original mounts to remain unchanged at %d", i)
			}
		}

		if workDir != "" {
			if len(got) == 0 {
				t.Fatal("expected workdir mount to be prepended")
			}
			if got[0] != (Mount{Host: workDir, Container: "/workspace", Mode: "rw"}) {
				t.Fatalf("expected workdir mount first, got %#v", got[0])
			}
			if len(got) != len(req.Mounts)+1 {
				t.Fatalf("expected %d mounts, got %d", len(req.Mounts)+1, len(got))
			}
			for i, mount := range req.Mounts {
				if got[i+1] != mount {
					t.Fatalf("expected mount %d to preserve order", i)
				}
			}
			return
		}

		if len(got) != len(req.Mounts) {
			t.Fatalf("expected %d mounts, got %d", len(req.Mounts), len(got))
		}
		for i, mount := range req.Mounts {
			if got[i] != mount {
				t.Fatalf("expected mount %d to match original", i)
			}
		}
	})
}

func FuzzSessionRequest_EffectiveTimeout(f *testing.F) {
	f.Add("")
	f.Add("5m")
	f.Add("1h30m")
	f.Add("0")
	f.Add("-5s")
	f.Add("not-a-duration")
	f.Add("2562047h47m16.854775807s")

	f.Fuzz(func(t *testing.T, timeout string) {
		req := SessionRequest{Timeout: timeout}
		got := req.EffectiveTimeout()

		if parsed, err := time.ParseDuration(timeout); err == nil {
			if got != parsed {
				t.Fatalf("expected parsed duration %v, got %v", parsed, got)
			}
			return
		}

		if got != 5*time.Minute {
			t.Fatalf("expected default timeout %v, got %v", 5*time.Minute, got)
		}
	})
}
