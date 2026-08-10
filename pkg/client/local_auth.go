package client

import (
	"errors"
	"net/http"
	"os"

	"github.com/danieliser/agentruntime/pkg/api"
	"github.com/danieliser/agentruntime/pkg/config"
)

// LoadLocalBearerToken reads the private credential shared with the locally
// supervised daemon. A missing file preserves compatibility with explicitly
// unauthenticated test/development servers; unsafe existing files fail closed.
func LoadLocalBearerToken() (string, error) {
	token, err := api.ReadAuthToken(config.DataDir())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return token, err
}

// NewLocal creates a client authenticated from the local private token file.
func NewLocal(baseURL string) (*Client, error) {
	token, err := LoadLocalBearerToken()
	if err != nil {
		return nil, err
	}
	return NewAuthenticated(baseURL, token), nil
}

// AuthorizeLocalRequest adds the local token to an HTTP request in memory.
func AuthorizeLocalRequest(request *http.Request) error {
	token, err := LoadLocalBearerToken()
	if err != nil {
		return err
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return nil
}

// LocalWebSocketHeaders returns authentication headers for a first-party CLI
// WebSocket handshake. The credential is never encoded into the URL.
func LocalWebSocketHeaders() (http.Header, error) {
	headers := make(http.Header)
	token, err := LoadLocalBearerToken()
	if err != nil {
		return nil, err
	}
	if token != "" {
		headers.Set("Authorization", "Bearer "+token)
	}
	return headers, nil
}
