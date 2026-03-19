package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateWorkDir(t *testing.T) {
	tempDir := t.TempDir()

	tests := []struct {
		name    string
		path    string
		wantErr bool
		errMsg  string
	}{
		{
			name:    "valid absolute path",
			path:    tempDir,
			wantErr: false,
		},
		{
			name:    "valid current working directory",
			path:    "/tmp",
			wantErr: false,
		},
		{
			name:    "relative path",
			path:    "./relative",
			wantErr: true,
			errMsg:  "must be absolute",
		},
		{
			name:    "relative path without prefix",
			path:    "relative",
			wantErr: true,
			errMsg:  "must be absolute",
		},
		{
			name:    "path with double dot",
			path:    "/tmp/../etc",
			wantErr: true,
			errMsg:  "cannot contain '..'",
		},
		{
			name:    "filesystem root",
			path:    "/",
			wantErr: true,
			errMsg:  "cannot be the filesystem root",
		},
		{
			name:    "empty path",
			path:    "",
			wantErr: true,
			errMsg:  "cannot be empty",
		},
		{
			name:    "nonexistent path",
			path:    "/nonexistent/directory/that/does/not/exist",
			wantErr: true,
			errMsg:  "does not exist",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateWorkDir(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateWorkDir(%q) error = %v, wantErr %v", tt.path, err, tt.wantErr)
				return
			}
			if tt.wantErr && err != nil {
				var eivwd ErrInvalidWorkDir
				if !isErrInvalidWorkDir(err) {
					t.Errorf("ValidateWorkDir(%q) returned wrong error type: %T", tt.path, err)
				}
				errMsg := err.Error()
				if tt.errMsg != "" && !contains(errMsg, tt.errMsg) {
					t.Errorf("ValidateWorkDir(%q) error = %q, want to contain %q", tt.path, errMsg, tt.errMsg)
				}
				_ = eivwd // suppress unused variable warning
			}
		})
	}
}

func TestValidateWorkDir_SensitiveDirectories(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home directory")
	}

	tests := []struct {
		name    string
		mkPath  string
		wantErr bool
		reason  string
	}{
		{
			name:    "reject .ssh",
			mkPath:  filepath.Join(home, ".ssh"),
			wantErr: true,
			reason:  "sensitive SSH directory",
		},
		{
			name:    "reject .gnupg",
			mkPath:  filepath.Join(home, ".gnupg"),
			wantErr: true,
			reason:  "sensitive GPG directory",
		},
		{
			name:    "reject .aws",
			mkPath:  filepath.Join(home, ".aws"),
			wantErr: true,
			reason:  "sensitive AWS directory",
		},
		{
			name:    "reject .kube",
			mkPath:  filepath.Join(home, ".kube"),
			wantErr: true,
			reason:  "sensitive Kubernetes directory",
		},
		{
			name:    "reject .docker",
			mkPath:  filepath.Join(home, ".docker"),
			wantErr: true,
			reason:  "sensitive Docker directory",
		},
		{
			name:    "reject Library/Keychains",
			mkPath:  filepath.Join(home, "Library/Keychains"),
			wantErr: true,
			reason:  "sensitive Keychain directory (macOS)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create the directory if it doesn't exist
			if err := os.MkdirAll(tt.mkPath, 0700); err != nil {
				t.Skipf("cannot create test directory %s: %v", tt.mkPath, err)
			}

			err := ValidateWorkDir(tt.mkPath)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateWorkDir(%q) error = %v, wantErr %v; reason: %s", tt.mkPath, err, tt.wantErr, tt.reason)
				return
			}
			if tt.wantErr && err != nil {
				if !isErrInvalidWorkDir(err) {
					t.Errorf("ValidateWorkDir(%q) returned wrong error type: %T", tt.mkPath, err)
				}
			}
		})
	}
}

func TestValidateWorkDir_NotADirectory(t *testing.T) {
	tempDir := t.TempDir()

	// Create a file
	filePath := filepath.Join(tempDir, "testfile.txt")
	if err := os.WriteFile(filePath, []byte("test"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	err := ValidateWorkDir(filePath)
	if err == nil {
		t.Errorf("ValidateWorkDir(%q) expected error for non-directory file, got nil", filePath)
		return
	}
	if !isErrInvalidWorkDir(err) {
		t.Errorf("ValidateWorkDir(%q) returned wrong error type: %T", filePath, err)
	}
	if !contains(err.Error(), "not a directory") {
		t.Errorf("ValidateWorkDir(%q) error = %q, want to contain 'not a directory'", filePath, err.Error())
	}
}

func TestValidateWorkDir_Symlink(t *testing.T) {
	tempDir := t.TempDir()

	// Create a real directory
	realDir := filepath.Join(tempDir, "real")
	if err := os.Mkdir(realDir, 0755); err != nil {
		t.Fatalf("failed to create real directory: %v", err)
	}

	// Create a symlink to it
	linkDir := filepath.Join(tempDir, "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	// Should succeed - symlinks are valid as long as they resolve
	err := ValidateWorkDir(linkDir)
	if err != nil {
		t.Errorf("ValidateWorkDir(%q) should accept symlinks that resolve, got error: %v", linkDir, err)
	}
}

func TestContainsDotDot(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/tmp/test", false},
		{"/tmp/../etc", true},
		{"/tmp/../../etc", true},
		{"/tmp/test/..", true},
		{"/tmp/..", true},
		{"/tmp/./test", false},
		{"/home/user/.ssh", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := containsDotDot(tt.path); got != tt.want {
				t.Errorf("containsDotDot(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestIsSensitivePath(t *testing.T) {
	tests := []struct {
		name         string
		targetPath   string
		sensitivePath string
		want         bool
	}{
		{
			name:         "exact match",
			targetPath:   "/home/user/.ssh",
			sensitivePath: "/home/user/.ssh",
			want:         true,
		},
		{
			name:         "target under sensitive",
			targetPath:   "/home/user/.ssh/id_rsa",
			sensitivePath: "/home/user/.ssh",
			want:         true,
		},
		{
			name:         "sensitive under target",
			targetPath:   "/home/user",
			sensitivePath: "/home/user/.ssh",
			want:         true,
		},
		{
			name:         "unrelated paths",
			targetPath:   "/home/user/projects",
			sensitivePath: "/home/user/.ssh",
			want:         false,
		},
		{
			name:         "similar but different",
			targetPath:   "/home/user/.ssh2",
			sensitivePath: "/home/user/.ssh",
			want:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSensitivePath(tt.targetPath, tt.sensitivePath); got != tt.want {
				t.Errorf("isSensitivePath(%q, %q) = %v, want %v", tt.targetPath, tt.sensitivePath, got, tt.want)
			}
		})
	}
}

// Helper functions
func isErrInvalidWorkDir(err error) bool {
	_, ok := err.(ErrInvalidWorkDir)
	return ok
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
