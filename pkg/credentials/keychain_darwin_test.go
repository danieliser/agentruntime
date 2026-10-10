//go:build darwin

package credentials

import (
	"os"
	"path/filepath"
	"testing"
)

func TestKeychainExtractorSelectsExactNativeAccount(t *testing.T) {
	bin := t.TempDir()
	// Reproduce an obsolete account selected by a service-only lookup. No real
	// keychain is accessed and the returned fixture contains no credentials.
	script := "#!/bin/sh\nif [ \"$*\" = \"find-generic-password -a native-user -s Claude Code-credentials -w\" ]; then printf current-account; else printf obsolete-account; fi\n"
	if err := os.WriteFile(filepath.Join(bin, "security"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("USER", "native-user")
	got, err := (&keychainExtractor{}).Extract("Claude Code-credentials")
	if err != nil {
		t.Fatal(err)
	}
	if got != "current-account" {
		t.Fatalf("selected %q instead of exact native account", got)
	}
}
