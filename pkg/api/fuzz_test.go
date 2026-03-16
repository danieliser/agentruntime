package api

import (
	"testing"
	"time"
)

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
