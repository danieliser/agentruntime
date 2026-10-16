package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	agentclient "github.com/danieliser/agentruntime/pkg/client"
)

type APIClient struct {
	client *agentclient.Client
	token  string
}

func NewAPIClient(cfg Config) *APIClient {
	c := agentclient.NewAuthenticated(cfg.URL, cfg.Token)
	c.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	return &APIClient{client: c, token: cfg.Token}
}

func (c *APIClient) GetSession(ctx context.Context, id string) (*agentclient.DurableSession, error) {
	v, err := c.client.GetDurableSession(ctx, id)
	return v, c.safeError(err)
}
func (c *APIClient) ListSessions(ctx context.Context) ([]agentclient.DurableSession, error) {
	v, e := c.client.ListDurableSessions(ctx)
	return v, c.safeError(e)
}
func (c *APIClient) Events(ctx context.Context, id string, after int64, limit int) (*agentclient.EventPage, error) {
	v, e := c.client.GetEvents(ctx, id, after, limit)
	return v, c.safeError(e)
}
func (c *APIClient) safeError(err error) error {
	if err == nil {
		return nil
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return errors.Join(ErrTimeout, redactError(strings.NewReader(err.Error()), c.token))
	}
	message := err.Error()
	err = redactError(strings.NewReader(message), c.token)
	var status int
	if _, scanErr := fmt.Sscanf(message, "request failed with status %d", &status); scanErr == nil {
		switch status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return errors.Join(ErrAuth, err)
		case http.StatusNotFound:
			return errors.Join(ErrNotFound, err)
		}
	}
	return errors.Join(ErrAPI, err)
}

func redactError(reader io.Reader, token string) error {
	b, _ := io.ReadAll(reader)
	message := string(b)
	if token != "" {
		message = strings.ReplaceAll(message, token, "[REDACTED]")
	}
	return errors.New(message)
}

func redactedMessage(err error, token string) string {
	return redactError(strings.NewReader(err.Error()), token).Error()
}
