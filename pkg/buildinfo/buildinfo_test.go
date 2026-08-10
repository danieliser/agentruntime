package buildinfo

import (
	"strings"
	"testing"
)

func TestVerifyExactBuildIdentity(t *testing.T) {
	identity := Identity{Version: "2.1.0", Commit: "0123456789abcdef0123456789abcdef01234567"}
	if err := identity.Verify("2.1.0@0123456789abcdef0123456789abcdef01234567"); err != nil {
		t.Fatalf("verify exact identity: %v", err)
	}
}

func TestVerifyRejectsArtifactDriftAndUnverifiableBuilds(t *testing.T) {
	tests := []struct {
		name     string
		identity Identity
		required string
		contains string
	}{
		{name: "version drift", identity: Identity{Version: "2.1.0", Commit: "0123456789abcdef0123456789abcdef01234567"}, required: "2.0.0@0123456789abcdef0123456789abcdef01234567", contains: "version mismatch"},
		{name: "commit drift", identity: Identity{Version: "2.1.0", Commit: "0123456789abcdef0123456789abcdef01234567"}, required: "2.1.0@1123456789abcdef0123456789abcdef01234567", contains: "commit mismatch"},
		{name: "unknown commit", identity: Identity{Version: "2.1.0", Commit: "unknown"}, required: "2.1.0@0123456789abcdef0123456789abcdef01234567", contains: "not verifiable"},
		{name: "malformed", identity: Identity{Version: "2.1.0", Commit: "0123456789abcdef0123456789abcdef01234567"}, required: "2.1.0", contains: "version@commit"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.identity.Verify(test.required)
			if err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("Verify(%q) error = %v, want containing %q", test.required, err, test.contains)
			}
		})
	}
}
