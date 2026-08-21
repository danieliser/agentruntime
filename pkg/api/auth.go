package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	AuthTokenFilename   = "auth.token"
	maxRequestBodyBytes = 1 << 20
)

// ReadAuthToken reads an existing AgentD bearer credential from a private data
// directory without creating or changing it.
func ReadAuthToken(dataDir string) (string, error) {
	if dataDir == "" {
		return "", fmt.Errorf("read_auth_token: data directory is required")
	}
	return readPrivateAuthToken(filepath.Join(dataDir, AuthTokenFilename))
}

// LoadOrCreateAuthToken returns the stable user-private bearer credential for
// AgentD. The token is never returned in an error or logged by this package.
func LoadOrCreateAuthToken(dataDir string) (string, error) {
	const op = "load_or_create_auth_token"
	if dataDir == "" {
		return "", fmt.Errorf("%s: data directory is required", op)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", fmt.Errorf("%s: create private data directory: %w", op, err)
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return "", fmt.Errorf("%s: secure data directory: %w", op, err)
	}
	path := filepath.Join(dataDir, AuthTokenFilename)
	if token, err := readPrivateAuthToken(path); err == nil {
		return token, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("%s: generate credential: %w", op, err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	file, err := os.CreateTemp(dataDir, ".auth-token-*")
	if err != nil {
		return "", fmt.Errorf("%s: create temporary credential file: %w", op, err)
	}
	temporaryPath := file.Name()
	defer os.Remove(temporaryPath)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("%s: secure temporary credential file: %w", op, err)
	}
	if _, err := file.WriteString(token + "\n"); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("%s: write credential file: %w", op, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("%s: sync credential file: %w", op, err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("%s: close credential file: %w", op, err)
	}
	// A hard link makes publication atomic and refuses to replace an existing
	// credential. Concurrent daemon starts therefore all observe one complete
	// token rather than an empty or partially-written file.
	if err := os.Link(temporaryPath, path); err != nil {
		if os.IsExist(err) {
			return readPrivateAuthToken(path)
		}
		return "", fmt.Errorf("%s: publish credential file: %w", op, err)
	}
	return token, nil
}

func readPrivateAuthToken(path string) (string, error) {
	const op = "read_auth_token"
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%s: credential path is not a regular file", op)
	}
	if info.Mode().Perm() != 0o600 {
		return "", fmt.Errorf("%s: credential file must have mode 0600", op)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s: read credential file: %w", op, err)
	}
	token := strings.TrimSpace(string(contents))
	if len(token) < 40 || len(token) > 256 {
		return "", fmt.Errorf("%s: credential length is invalid", op)
	}
	return token, nil
}

func (s *Server) requireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.authEnabled {
			c.Next()
			return
		}
		token := requestAuthToken(c.Request)
		if token == "" {
			writeUnauthorized(c)
			return
		}
		provided := sha256.Sum256([]byte(token))
		if subtle.ConstantTimeCompare(provided[:], s.authTokenHash[:]) != 1 {
			writeUnauthorized(c)
			return
		}
		if c.Request.Body != nil {
			limit := int64(maxRequestBodyBytes)
			if c.Request.Method == http.MethodPost && c.Request.URL.Path == "/api/v1/resume-states" {
				limit = maxPortableBundleBytes + 1
			}
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
		}
		c.Next()
	}
}

func requestAuthToken(request *http.Request) string {
	const bearerPrefix = "Bearer "
	header := request.Header.Get("Authorization")
	if strings.HasPrefix(header, bearerPrefix) && len(header) > len(bearerPrefix) {
		return header[len(bearerPrefix):]
	}
	if !strings.EqualFold(request.Header.Get("Upgrade"), "websocket") {
		return ""
	}
	const protocolPrefix = "agentd.auth."
	for _, protocol := range strings.Split(request.Header.Get("Sec-WebSocket-Protocol"), ",") {
		protocol = strings.TrimSpace(protocol)
		if strings.HasPrefix(protocol, protocolPrefix) && len(protocol) > len(protocolPrefix) {
			return protocol[len(protocolPrefix):]
		}
	}
	return ""
}

func writeUnauthorized(c *gin.Context) {
	c.Header("WWW-Authenticate", `Bearer realm="agentd"`)
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
		"api_version": "v1",
		"error":       apiErrorEnvelope{Code: "unauthenticated", Message: "authentication required"},
	})
}
