package skills

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	MaxArchiveBytes  = 32 << 20
	MaxExpandedBytes = 32 << 20
	MaxFiles         = 512
	MaxDepth         = 16
	MaxEntries       = 50
)

var (
	digestPattern = regexp.MustCompile(`^sha256:([0-9a-f]{64})$`)
	namePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
)

type Store struct{ root string }

func NewStore(root string) *Store { return &Store{root: root} }

func ValidName(name string) bool { return namePattern.MatchString(name) }

func (s *Store) Put(digest string, reader io.Reader) (bool, error) {
	hexDigest, err := parseDigest(digest)
	if err != nil {
		return false, err
	}
	data, err := io.ReadAll(io.LimitReader(reader, MaxArchiveBytes+1))
	if err != nil {
		return false, fmt.Errorf("read skill bundle: %w", err)
	}
	if len(data) > MaxArchiveBytes {
		return false, fmt.Errorf("skill bundle exceeds %d bytes", MaxArchiveBytes)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != hexDigest {
		return false, errors.New("skill bundle digest mismatch")
	}
	if _, err := validateBundle(data); err != nil {
		return false, err
	}
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return false, fmt.Errorf("create skill store: %w", err)
	}
	finalPath := filepath.Join(s.root, hexDigest+".tar")
	if existing, err := s.readStoredHex(hexDigest); err == nil {
		if !bytes.Equal(existing, data) {
			return false, errors.New("stored skill bundle bytes conflict with digest")
		}
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("read stored skill bundle: %w", err)
	}
	tmp, err := os.CreateTemp(s.root, ".upload-*")
	if err != nil {
		return false, fmt.Errorf("create skill bundle temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Link(tmpName, finalPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			existing, readErr := s.readStoredHex(hexDigest)
			if readErr == nil && bytes.Equal(existing, data) {
				return false, nil
			}
			if readErr != nil {
				return false, fmt.Errorf("verify concurrently stored skill bundle: %w", readErr)
			}
			return false, errors.New("stored skill bundle bytes conflict with digest")
		}
		return false, fmt.Errorf("publish skill bundle: %w", err)
	}
	return true, nil
}

func (s *Store) Ensure(digest string) error {
	return s.ensure(digest, "")
}

func (s *Store) EnsureNamed(digest, expectedName string) error {
	if !ValidName(expectedName) {
		return errors.New("unsafe skill name")
	}
	return s.ensure(digest, expectedName)
}

func (s *Store) ensure(digest, expectedName string) error {
	_, err := s.verifiedBundle(digest, expectedName)
	return err
}

func (s *Store) verifiedBundle(digest, expectedName string) ([]byte, error) {
	hexDigest, err := parseDigest(digest)
	if err != nil {
		return nil, err
	}
	data, err := s.readStoredHex(hexDigest)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != hexDigest {
		return nil, errors.New("stored skill bundle is corrupt")
	}
	root, err := validateBundle(data)
	if err != nil {
		return nil, err
	}
	if expectedName != "" && root != expectedName {
		return nil, fmt.Errorf("skill bundle root %q does not match %q", root, expectedName)
	}
	return data, nil
}

func (s *Store) Size(digest string) (int64, error) {
	hexDigest, err := parseDigest(digest)
	if err != nil {
		return 0, err
	}
	info, err := os.Lstat(filepath.Join(s.root, hexDigest+".tar"))
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return 0, errors.New("stored skill bundle is not a regular file")
	}
	if info.Size() > MaxArchiveBytes {
		return 0, fmt.Errorf("stored skill bundle exceeds %d bytes", MaxArchiveBytes)
	}
	return info.Size(), nil
}

func (s *Store) Extract(digest, expectedName, destination string) error {
	if !namePattern.MatchString(expectedName) {
		return errors.New("unsafe skill name")
	}
	data, err := s.verifiedBundle(digest, expectedName)
	if err != nil {
		return err
	}
	root := expectedName
	parent := filepath.Dir(destination)
	if filepath.Base(destination) != expectedName {
		return errors.New("skill destination must end in the canonical skill name")
	}
	if err := os.Mkdir(parent, 0o700); err != nil && !os.IsExist(err) {
		return fmt.Errorf("create skill destination parent: %w", err)
	}
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return fmt.Errorf("inspect skill destination parent: %w", err)
	}
	if !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("skill destination parent must be a real directory")
	}
	rootHandle, err := os.OpenRoot(parent)
	if err != nil {
		return fmt.Errorf("open skill destination parent: %w", err)
	}
	defer rootHandle.Close()
	tmp, err := makeExtractTempDir(rootHandle)
	if err != nil {
		return err
	}
	defer func() { _ = removeAllInRoot(rootHandle, tmp) }()
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, readErr := tr.Next()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		rel := strings.TrimPrefix(path.Clean(hdr.Name), root+"/")
		target := filepath.Join(tmp, filepath.FromSlash(rel))
		if err := mkdirAllInRoot(rootHandle, filepath.Dir(target), 0o700); err != nil {
			return err
		}
		mode := os.FileMode(0o444 | (hdr.Mode & 0o111))
		file, err := rootHandle.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(file, tr)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if err := removeAllInRoot(rootHandle, expectedName); err != nil {
		return err
	}
	return os.Rename(filepath.Join(parent, tmp), destination)
}

func (s *Store) readStoredHex(hexDigest string) ([]byte, error) {
	path := filepath.Join(s.root, hexDigest+".tar")
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("skill bundle unavailable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("stored skill bundle is not a regular file")
	}
	if info.Size() > MaxArchiveBytes {
		return nil, fmt.Errorf("stored skill bundle exceeds %d bytes", MaxArchiveBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open stored skill bundle: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaxArchiveBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read stored skill bundle: %w", err)
	}
	if len(data) > MaxArchiveBytes {
		return nil, fmt.Errorf("stored skill bundle exceeds %d bytes", MaxArchiveBytes)
	}
	return data, nil
}

func makeExtractTempDir(root *os.Root) (string, error) {
	for range 100 {
		random := make([]byte, 12)
		if _, err := rand.Read(random); err != nil {
			return "", fmt.Errorf("generate skill extraction directory: %w", err)
		}
		name := ".skill-" + hex.EncodeToString(random)
		if err := root.Mkdir(name, 0o700); err == nil {
			return name, nil
		} else if !os.IsExist(err) {
			return "", fmt.Errorf("create skill extraction directory: %w", err)
		}
	}
	return "", errors.New("create unique skill extraction directory")
}

func mkdirAllInRoot(root *os.Root, name string, mode os.FileMode) error {
	clean := filepath.Clean(name)
	if clean == "." {
		return nil
	}
	current := ""
	for _, part := range strings.Split(clean, string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		if err := root.Mkdir(current, mode); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err := root.Stat(current)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("skill extraction path %q is not a directory", current)
		}
	}
	return nil
}

func removeAllInRoot(root *os.Root, name string) error {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return root.Remove(name)
	}
	directory, err := root.Open(name)
	if err != nil {
		return err
	}
	entries, readErr := directory.ReadDir(-1)
	closeErr := directory.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	for _, entry := range entries {
		if err := removeAllInRoot(root, filepath.Join(name, entry.Name())); err != nil {
			return err
		}
	}
	return root.Remove(name)
}

func validateBundle(data []byte) (string, error) {
	tr := tar.NewReader(bytes.NewReader(data))
	root := ""
	seen := map[string]bool{}
	files, expanded := 0, int64(0)
	hasSkill := false
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("invalid skill tar: %w", err)
		}
		clean := path.Clean(hdr.Name)
		if clean == "." || path.IsAbs(hdr.Name) || clean == ".." || strings.HasPrefix(clean, "../") || strings.ContainsRune(hdr.Name, 0) {
			return "", errors.New("unsafe skill bundle path")
		}
		parts := strings.Split(clean, "/")
		if len(parts) > MaxDepth+1 {
			return "", errors.New("skill bundle path depth exceeded")
		}
		if root == "" {
			root = parts[0]
			if !namePattern.MatchString(root) {
				return "", errors.New("unsafe skill bundle root")
			}
		}
		if parts[0] != root {
			return "", errors.New("skill bundle must contain exactly one root directory")
		}
		if _, ok := seen[clean]; ok {
			return "", errors.New("duplicate skill bundle path")
		}
		isDir := hdr.Typeflag == tar.TypeDir
		if len(parts) == 1 && !isDir {
			return "", errors.New("skill bundle root must be a directory")
		}
		for index := 1; index < len(parts); index++ {
			ancestor := strings.Join(parts[:index], "/")
			if ancestorIsDir, exists := seen[ancestor]; exists && !ancestorIsDir {
				return "", errors.New("skill bundle path descends through a regular file")
			}
		}
		if !isDir {
			prefix := clean + "/"
			for existing := range seen {
				if strings.HasPrefix(existing, prefix) {
					return "", errors.New("skill bundle regular file conflicts with an existing child path")
				}
			}
		}
		seen[clean] = isDir
		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			files++
			expanded += hdr.Size
			if clean == root+"/SKILL.md" {
				hasSkill = true
			}
		case tar.TypeDir:
		default:
			return "", errors.New("skill bundle contains unsupported entry type")
		}
		if files > MaxFiles || expanded > MaxExpandedBytes {
			return "", errors.New("skill bundle expansion limits exceeded")
		}
	}
	if root == "" || !hasSkill {
		return "", errors.New("skill bundle must contain SKILL.md")
	}
	return root, nil
}

func parseDigest(digest string) (string, error) {
	match := digestPattern.FindStringSubmatch(digest)
	if match == nil {
		return "", errors.New("bundle digest must be sha256:<64 lowercase hex>")
	}
	return match[1], nil
}
