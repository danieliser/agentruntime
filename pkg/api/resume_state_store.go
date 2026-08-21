package api

import (
	"archive/tar"
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	portableResumeSchemaVersion = "1.0"
	portableManifestName        = "manifest.json"
	portableProviderStateName   = "provider-state.tar"
	maxPortableManifestBytes    = 64 << 10
	maxPortableProviderBytes    = 1 << 30
	maxPortableStateFiles       = 100_000
	maxPortableBundleBytes      = maxPortableProviderBytes + maxPortableManifestBytes + (1 << 20)
)

type portableResumeManifest struct {
	SchemaVersion       string    `json:"schema_version"`
	Agent               string    `json:"agent"`
	ProviderSessionID   string    `json:"provider_session_id"`
	SourceSessionID     string    `json:"source_session_id"`
	ProviderStateTarget string    `json:"provider_state_target"`
	ProviderStateSHA256 string    `json:"provider_state_sha256"`
	ImageReference      string    `json:"image_reference"`
	ImageDigest         string    `json:"image_digest"`
	CreatedAt           time.Time `json:"created_at"`
}

type resumeStateStore struct {
	dir string
}

func newResumeStateStore(dir string) *resumeStateStore {
	return &resumeStateStore{dir: dir}
}

func (store *resumeStateStore) Export(
	ctx context.Context,
	manifest portableResumeManifest,
	exporter func(context.Context, io.Writer) error,
) (string, portableResumeManifest, error) {
	if store == nil || store.dir == "" || exporter == nil {
		return "", portableResumeManifest{}, fmt.Errorf("portable resume store and exporter are required")
	}
	if manifest.SchemaVersion == "" {
		manifest.SchemaVersion = portableResumeSchemaVersion
	}
	if err := os.MkdirAll(store.dir, 0o700); err != nil {
		return "", portableResumeManifest{}, fmt.Errorf("create portable resume directory: %w", err)
	}
	if err := os.Chmod(store.dir, 0o700); err != nil {
		return "", portableResumeManifest{}, fmt.Errorf("secure portable resume directory: %w", err)
	}
	file, err := os.CreateTemp(store.dir, ".resume-state-*")
	if err != nil {
		return "", portableResumeManifest{}, fmt.Errorf("create portable resume bundle: %w", err)
	}
	tempPath := file.Name()
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(tempPath)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return "", portableResumeManifest{}, fmt.Errorf("secure portable resume bundle: %w", err)
	}

	archive := zip.NewWriter(file)
	stateHeader := &zip.FileHeader{Name: portableProviderStateName, Method: zip.Store}
	stateHeader.SetMode(0o600)
	stateEntry, err := archive.CreateHeader(stateHeader)
	if err != nil {
		return "", portableResumeManifest{}, fmt.Errorf("create provider-state archive entry: %w", err)
	}
	stateDigest := sha256.New()
	limitedState := &boundedWriter{writer: io.MultiWriter(stateEntry, stateDigest), remaining: maxPortableProviderBytes}
	if err := exporter(ctx, limitedState); err != nil {
		return "", portableResumeManifest{}, fmt.Errorf("export provider state: %w", err)
	}
	manifest.ProviderStateSHA256 = "sha256:" + hex.EncodeToString(stateDigest.Sum(nil))
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return "", portableResumeManifest{}, fmt.Errorf("encode portable resume manifest: %w", err)
	}
	if len(manifestBytes) > maxPortableManifestBytes {
		return "", portableResumeManifest{}, fmt.Errorf("portable resume manifest exceeds %d bytes", maxPortableManifestBytes)
	}
	manifestHeader := &zip.FileHeader{Name: portableManifestName, Method: zip.Store}
	manifestHeader.SetMode(0o600)
	manifestEntry, err := archive.CreateHeader(manifestHeader)
	if err != nil {
		return "", portableResumeManifest{}, fmt.Errorf("create portable manifest entry: %w", err)
	}
	if _, err := manifestEntry.Write(manifestBytes); err != nil {
		return "", portableResumeManifest{}, fmt.Errorf("write portable manifest: %w", err)
	}
	if err := archive.Close(); err != nil {
		return "", portableResumeManifest{}, fmt.Errorf("close portable resume archive: %w", err)
	}
	if err := file.Sync(); err != nil {
		return "", portableResumeManifest{}, fmt.Errorf("sync portable resume archive: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", portableResumeManifest{}, fmt.Errorf("close portable resume bundle: %w", err)
	}
	validated, err := validateResumeStateBundle(ctx, tempPath)
	if err != nil {
		return "", portableResumeManifest{}, err
	}
	manifest = validated
	id, err := fileSHA256(tempPath)
	if err != nil {
		return "", portableResumeManifest{}, err
	}
	finalPath := store.path(id)
	if _, err := os.Stat(finalPath); err == nil {
		_ = store.recordLatest(manifest.SourceSessionID, id)
		return id, manifest, nil
	} else if !os.IsNotExist(err) {
		return "", portableResumeManifest{}, fmt.Errorf("inspect portable resume bundle: %w", err)
	}
	if err := os.Rename(tempPath, finalPath); err != nil {
		return "", portableResumeManifest{}, fmt.Errorf("commit portable resume bundle: %w", err)
	}
	keep = true
	if err := store.recordLatest(manifest.SourceSessionID, id); err != nil {
		return "", portableResumeManifest{}, err
	}
	return id, manifest, nil
}

func (store *resumeStateStore) Open(id string) (*os.File, error) {
	if !validResumeStateID(id) {
		return nil, fmt.Errorf("invalid portable resume state ID")
	}
	file, err := os.Open(store.path(id))
	if err != nil {
		return nil, fmt.Errorf("open portable resume state: %w", err)
	}
	return file, nil
}

func (store *resumeStateStore) Upload(ctx context.Context, reader io.Reader) (string, portableResumeManifest, error) {
	if store == nil || store.dir == "" || reader == nil {
		return "", portableResumeManifest{}, fmt.Errorf("portable resume store and upload reader are required")
	}
	if err := os.MkdirAll(store.dir, 0o700); err != nil {
		return "", portableResumeManifest{}, fmt.Errorf("create portable resume directory: %w", err)
	}
	file, err := os.CreateTemp(store.dir, ".resume-upload-*")
	if err != nil {
		return "", portableResumeManifest{}, fmt.Errorf("create portable resume upload: %w", err)
	}
	tempPath := file.Name()
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(tempPath)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return "", portableResumeManifest{}, fmt.Errorf("secure portable resume upload: %w", err)
	}
	written, err := io.Copy(file, io.LimitReader(reader, maxPortableBundleBytes+1))
	if err != nil {
		return "", portableResumeManifest{}, fmt.Errorf("write portable resume upload: %w", err)
	}
	if written > maxPortableBundleBytes {
		return "", portableResumeManifest{}, fmt.Errorf("portable resume upload exceeds %d bytes", maxPortableBundleBytes)
	}
	if err := file.Sync(); err != nil {
		return "", portableResumeManifest{}, fmt.Errorf("sync portable resume upload: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", portableResumeManifest{}, fmt.Errorf("close portable resume upload: %w", err)
	}
	manifest, err := validateResumeStateBundle(ctx, tempPath)
	if err != nil {
		return "", portableResumeManifest{}, err
	}
	id, err := fileSHA256(tempPath)
	if err != nil {
		return "", portableResumeManifest{}, err
	}
	finalPath := store.path(id)
	if _, err := os.Stat(finalPath); err == nil {
		_ = store.recordLatest(manifest.SourceSessionID, id)
		return id, manifest, nil
	} else if !os.IsNotExist(err) {
		return "", portableResumeManifest{}, fmt.Errorf("inspect portable resume upload: %w", err)
	}
	if err := os.Rename(tempPath, finalPath); err != nil {
		return "", portableResumeManifest{}, fmt.Errorf("commit portable resume upload: %w", err)
	}
	keep = true
	if err := store.recordLatest(manifest.SourceSessionID, id); err != nil {
		return "", portableResumeManifest{}, err
	}
	return id, manifest, nil
}

func (store *resumeStateStore) Manifest(id string) (portableResumeManifest, error) {
	if !validResumeStateID(id) {
		return portableResumeManifest{}, fmt.Errorf("invalid portable resume state ID")
	}
	return validateResumeStateBundle(context.Background(), store.path(id))
}

func (store *resumeStateStore) Import(ctx context.Context, id string, importer func(context.Context, io.Reader) error) error {
	if importer == nil {
		return fmt.Errorf("portable resume importer is required")
	}
	if _, err := store.Manifest(id); err != nil {
		return err
	}
	archive, err := zip.OpenReader(store.path(id))
	if err != nil {
		return fmt.Errorf("open portable resume bundle: %w", err)
	}
	defer archive.Close()
	for _, entry := range archive.File {
		if entry.Name != portableProviderStateName {
			continue
		}
		reader, err := entry.Open()
		if err != nil {
			return fmt.Errorf("open provider-state archive: %w", err)
		}
		err = importer(ctx, reader)
		closeErr := reader.Close()
		if err != nil {
			return fmt.Errorf("import provider state: %w", err)
		}
		if closeErr != nil {
			return fmt.Errorf("close provider-state archive: %w", closeErr)
		}
		return nil
	}
	return fmt.Errorf("portable resume bundle is missing %s", portableProviderStateName)
}

func (store *resumeStateStore) path(id string) string {
	return filepath.Join(store.dir, id+".agentstate")
}

func (store *resumeStateStore) recordLatest(sourceSessionID, id string) error {
	if !safePortableSourceID(sourceSessionID) || !validResumeStateID(id) {
		return fmt.Errorf("portable resume source identity is unsafe")
	}
	indexDir := filepath.Join(store.dir, "by-session")
	if err := os.MkdirAll(indexDir, 0o700); err != nil {
		return fmt.Errorf("create portable resume index: %w", err)
	}
	file, err := os.CreateTemp(indexDir, ".latest-*")
	if err != nil {
		return fmt.Errorf("create portable resume index entry: %w", err)
	}
	tempPath := file.Name()
	defer func() {
		_ = file.Close()
		_ = os.Remove(tempPath)
	}()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if _, err := io.WriteString(file, id+"\n"); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, filepath.Join(indexDir, sourceSessionID)); err != nil {
		return fmt.Errorf("commit portable resume index: %w", err)
	}
	return nil
}

func (store *resumeStateStore) Latest(sourceSessionID string) (string, portableResumeManifest, error) {
	if !safePortableSourceID(sourceSessionID) {
		return "", portableResumeManifest{}, fmt.Errorf("portable resume source identity is unsafe")
	}
	raw, err := os.ReadFile(filepath.Join(store.dir, "by-session", sourceSessionID))
	if err != nil {
		return "", portableResumeManifest{}, fmt.Errorf("read portable resume index: %w", err)
	}
	id := strings.TrimSpace(string(raw))
	manifest, err := store.Manifest(id)
	return id, manifest, err
}

func safePortableSourceID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_' {
			continue
		}
		return false
	}
	return true
}

type boundedWriter struct {
	writer    io.Writer
	remaining int64
}

func (writer *boundedWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > writer.remaining {
		return 0, fmt.Errorf("portable provider state exceeds %d bytes", maxPortableProviderBytes)
	}
	written, err := writer.writer.Write(data)
	writer.remaining -= int64(written)
	return written, err
}

func validateResumeStateBundle(ctx context.Context, path string) (portableResumeManifest, error) {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return portableResumeManifest{}, fmt.Errorf("open portable resume bundle: %w", err)
	}
	defer archive.Close()
	var manifestEntry, stateEntry *zip.File
	for _, entry := range archive.File {
		switch entry.Name {
		case portableManifestName:
			if manifestEntry != nil {
				return portableResumeManifest{}, fmt.Errorf("portable resume bundle has duplicate manifest")
			}
			manifestEntry = entry
		case portableProviderStateName:
			if stateEntry != nil {
				return portableResumeManifest{}, fmt.Errorf("portable resume bundle has duplicate provider state")
			}
			stateEntry = entry
		default:
			return portableResumeManifest{}, fmt.Errorf("portable resume bundle contains unexpected entry %q", entry.Name)
		}
	}
	if manifestEntry == nil || stateEntry == nil || len(archive.File) != 2 {
		return portableResumeManifest{}, fmt.Errorf("portable resume bundle requires manifest and provider state")
	}
	if manifestEntry.UncompressedSize64 > maxPortableManifestBytes || stateEntry.UncompressedSize64 > maxPortableProviderBytes {
		return portableResumeManifest{}, fmt.Errorf("portable resume bundle exceeds size limits")
	}
	manifestReader, err := manifestEntry.Open()
	if err != nil {
		return portableResumeManifest{}, fmt.Errorf("open portable resume manifest: %w", err)
	}
	manifestBytes, err := io.ReadAll(io.LimitReader(manifestReader, maxPortableManifestBytes+1))
	closeErr := manifestReader.Close()
	if err != nil || closeErr != nil {
		return portableResumeManifest{}, fmt.Errorf("read portable resume manifest: %w", errors.Join(err, closeErr))
	}
	var manifest portableResumeManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return portableResumeManifest{}, fmt.Errorf("decode portable resume manifest: %w", err)
	}
	if err := validatePortableResumeManifest(manifest); err != nil {
		return portableResumeManifest{}, err
	}
	stateReader, err := stateEntry.Open()
	if err != nil {
		return portableResumeManifest{}, fmt.Errorf("open provider-state archive: %w", err)
	}
	hash := sha256.New()
	tee := io.TeeReader(stateReader, hash)
	if err := validateProviderStateTar(ctx, tee); err != nil {
		_ = stateReader.Close()
		return portableResumeManifest{}, err
	}
	if err := stateReader.Close(); err != nil {
		return portableResumeManifest{}, fmt.Errorf("close provider-state archive: %w", err)
	}
	gotHash := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if gotHash != manifest.ProviderStateSHA256 {
		return portableResumeManifest{}, fmt.Errorf("provider-state hash %q does not match manifest %q", gotHash, manifest.ProviderStateSHA256)
	}
	return manifest, nil
}

func validatePortableResumeManifest(manifest portableResumeManifest) error {
	if manifest.SchemaVersion != portableResumeSchemaVersion {
		return fmt.Errorf("unsupported portable resume schema %q", manifest.SchemaVersion)
	}
	wantTarget := ""
	switch manifest.Agent {
	case "codex":
		wantTarget = "/home/agent/.codex/sessions"
	case "claude":
		wantTarget = "/home/agent/.claude/projects"
	default:
		return fmt.Errorf("portable resume agent %q is unsupported", manifest.Agent)
	}
	if manifest.ProviderSessionID == "" || manifest.SourceSessionID == "" {
		return fmt.Errorf("portable resume provider and source session identities are required")
	}
	if manifest.ProviderStateTarget != wantTarget {
		return fmt.Errorf("portable resume state target %q does not match agent %q", manifest.ProviderStateTarget, manifest.Agent)
	}
	if !validSHA256Digest(manifest.ProviderStateSHA256) || !validSHA256Digest(manifest.ImageDigest) || manifest.ImageReference == "" || manifest.CreatedAt.IsZero() {
		return fmt.Errorf("portable resume manifest is missing provenance")
	}
	return nil
}

func validSHA256Digest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") {
		return false
	}
	digest := strings.TrimPrefix(value, "sha256:")
	decoded, err := hex.DecodeString(digest)
	return err == nil && len(decoded) == sha256.Size
}

func validateProviderStateTar(ctx context.Context, reader io.Reader) error {
	archive := tar.NewReader(io.LimitReader(reader, maxPortableProviderBytes+1))
	files := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		header, err := archive.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read provider-state tar: %w", err)
		}
		files++
		if files > maxPortableStateFiles {
			return fmt.Errorf("provider-state tar exceeds %d entries", maxPortableStateFiles)
		}
		clean := filepath.ToSlash(filepath.Clean(header.Name))
		if filepath.IsAbs(header.Name) || clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("provider-state tar contains unsafe path %q", header.Name)
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA && header.Typeflag != tar.TypeDir {
			return fmt.Errorf("provider-state tar contains unsupported entry type for %q", header.Name)
		}
		if header.Size < 0 || header.Size > maxPortableProviderBytes {
			return fmt.Errorf("provider-state tar entry %q exceeds size limit", header.Name)
		}
		if _, err := io.Copy(io.Discard, archive); err != nil {
			return fmt.Errorf("read provider-state tar entry %q: %w", header.Name, err)
		}
	}
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open portable resume bundle for hashing: %w", err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash portable resume bundle: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func validResumeStateID(id string) bool {
	if len(id) != sha256.Size*2 || strings.ToLower(id) != id {
		return false
	}
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == sha256.Size
}
