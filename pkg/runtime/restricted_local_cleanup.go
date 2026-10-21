package runtime

import (
	"fmt"
	"os"
	"path/filepath"
)

// CleanupRestrictedLocalHomes removes disposable homes left after a daemon crash.
// Local processes are not recovered. Durable conversations and Docker-only
// session credentials are retained. Never traverse symlinked provider/session roots.
func CleanupRestrictedLocalHomes(dataDir string) error {
	if dataDir == "" {
		return nil
	}
	for _, provider := range []string{"claude", "codex"} {
		root := filepath.Join(dataDir, provider+"-sessions")
		info, err := os.Lstat(root)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			continue
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			home := filepath.Join(root, entry.Name(), "local-home")
			if err := os.RemoveAll(home); err != nil {
				return fmt.Errorf("remove orphaned %s launch home: %w", provider, err)
			}
		}
	}
	return nil
}
