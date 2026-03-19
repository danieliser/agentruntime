package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrInvalidWorkDir indicates that a working directory path failed validation.
type ErrInvalidWorkDir struct {
	message string
}

func (e ErrInvalidWorkDir) Error() string {
	return e.message
}

// ValidateWorkDir validates that a working directory path is safe to use for
// agent execution. It checks that:
//   - Path is absolute
//   - Path exists and is a directory
//   - Symlinks are resolved to detect traversal attempts
//   - Path does not contain ".." components
//   - Path is not the filesystem root
//   - Path does not contain sensitive directories
func ValidateWorkDir(path string) error {
	if path == "" {
		return ErrInvalidWorkDir{message: "working directory cannot be empty"}
	}

	// Check if path is absolute
	if !filepath.IsAbs(path) {
		return ErrInvalidWorkDir{message: fmt.Sprintf("working directory must be absolute: %s", path)}
	}

	// Check for ".." components
	if containsDotDot(path) {
		return ErrInvalidWorkDir{message: fmt.Sprintf("working directory cannot contain '..': %s", path)}
	}

	// Check if path exists and is a directory first (before resolving symlinks)
	// This provides a clearer error message for missing paths
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrInvalidWorkDir{message: fmt.Sprintf("working directory does not exist: %s", path)}
		}
		return ErrInvalidWorkDir{message: fmt.Sprintf("cannot stat working directory: %s: %v", path, err)}
	}

	// Check if it's a directory
	if !info.IsDir() {
		return ErrInvalidWorkDir{message: fmt.Sprintf("working directory is not a directory: %s", path)}
	}

	// Resolve symlinks to detect traversal attempts
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ErrInvalidWorkDir{message: fmt.Sprintf("failed to resolve symlinks in %s: %v", path, err)}
	}

	// Reject filesystem root
	if resolved == "/" {
		return ErrInvalidWorkDir{message: "working directory cannot be the filesystem root"}
	}

	// Check for sensitive directories
	if err := checkSensitiveDirectories(resolved); err != nil {
		return err
	}

	return nil
}

// containsDotDot checks if a path string contains ".." components.
func containsDotDot(path string) bool {
	// Check the raw path string for ".." pattern
	// We do this before cleaning to catch literal ".." in paths
	parts := strings.Split(path, string(filepath.Separator))
	for _, part := range parts {
		if part == ".." {
			return true
		}
	}
	return false
}

// checkSensitiveDirectories checks if the given path is a sensitive directory
// or a parent of sensitive directories relative to $HOME.
func checkSensitiveDirectories(path string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		// If we can't determine home dir, skip this check
		return nil
	}

	sensitiveNames := []string{
		".ssh",
		".gnupg",
		".aws",
		".kube",
		".docker",
		".config/gcloud",
		"Library/Keychains",
	}

	for _, sensitiveDir := range sensitiveNames {
		sensitivePath := filepath.Join(home, sensitiveDir)
		// Normalize paths for comparison
		sensitivePath = filepath.Clean(sensitivePath)

		// Check if the provided path is the sensitive directory or a parent/child
		// We need to check both if path is trying to access the sensitive dir
		// and if the path itself is under a sensitive dir
		if isSensitivePath(path, sensitivePath) {
			return ErrInvalidWorkDir{message: fmt.Sprintf("working directory cannot be or contain sensitive directory: %s", sensitiveDir)}
		}
	}

	return nil
}

// isSensitivePath checks if targetPath matches or is related to sensitivePath.
// It checks both if targetPath is the sensitive directory and if targetPath
// is under a sensitive directory.
func isSensitivePath(targetPath, sensitivePath string) bool {
	// Normalize both paths
	targetPath = filepath.Clean(targetPath)
	sensitivePath = filepath.Clean(sensitivePath)

	// Check if target is the sensitive directory
	if targetPath == sensitivePath {
		return true
	}

	// Check if target is under the sensitive directory
	rel, err := filepath.Rel(sensitivePath, targetPath)
	if err == nil && !filepath.IsAbs(rel) && rel != ".." && !startsWith(rel, "..") {
		// targetPath is under sensitivePath
		return true
	}

	// Check if sensitivePath is under targetPath (parent of sensitive dir)
	rel, err = filepath.Rel(targetPath, sensitivePath)
	if err == nil && !filepath.IsAbs(rel) && rel != ".." && !startsWith(rel, "..") {
		// sensitivePath is under targetPath
		return true
	}

	return false
}

// startsWith checks if path starts with prefix accounting for path separators.
func startsWith(path, prefix string) bool {
	if len(prefix) == 0 {
		return true
	}
	if len(path) < len(prefix) {
		return false
	}
	return path[:len(prefix)] == prefix
}
