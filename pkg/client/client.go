package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/danieliser/agentruntime/pkg/api"
)

type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

func New(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
	}
}

func (c *Client) Dispatch(ctx context.Context, req api.SessionRequest) (*api.SessionResponse, error) {
	httpReq, err := c.newJSONRequest(ctx, http.MethodPost, "/sessions", req)
	if err != nil {
		return nil, err
	}

	var resp api.SessionResponse
	if err := c.doJSON(httpReq, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) GetSession(ctx context.Context, id string) (*api.SessionSummary, error) {
	httpReq, err := c.newRequest(ctx, http.MethodGet, "/sessions/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}

	var wire struct {
		SessionID string            `json:"session_id"`
		TaskID    string            `json:"task_id,omitempty"`
		Agent     string            `json:"agent"`
		Runtime   string            `json:"runtime"`
		Status    string            `json:"status"`
		CreatedAt time.Time         `json:"created_at"`
		Tags      map[string]string `json:"tags,omitempty"`

		ID          string `json:"id"`
		AgentName   string `json:"agent_name"`
		RuntimeName string `json:"runtime_name"`
		State       string `json:"state"`
	}
	if err := c.doJSON(httpReq, &wire); err != nil {
		return nil, err
	}

	summary := &api.SessionSummary{
		SessionID: firstNonEmpty(wire.SessionID, wire.ID),
		TaskID:    wire.TaskID,
		Agent:     firstNonEmpty(wire.Agent, wire.AgentName),
		Runtime:   firstNonEmpty(wire.Runtime, wire.RuntimeName),
		Status:    firstNonEmpty(wire.Status, wire.State),
		CreatedAt: wire.CreatedAt,
		Tags:      wire.Tags,
	}
	if summary.SessionID == "" {
		return nil, fmt.Errorf("decode session: missing session id")
	}

	return summary, nil
}

func (c *Client) ListSessions(ctx context.Context) ([]api.SessionSummary, error) {
	httpReq, err := c.newRequest(ctx, http.MethodGet, "/sessions", nil)
	if err != nil {
		return nil, err
	}

	var sessions []api.SessionSummary
	if err := c.doJSON(httpReq, &sessions); err != nil {
		return nil, err
	}
	return sessions, nil
}

func (c *Client) Kill(ctx context.Context, id string) error {
	httpReq, err := c.newRequest(ctx, http.MethodDelete, "/sessions/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}

	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

func (c *Client) GetLogs(ctx context.Context, id string, cursor int64) (data []byte, nextCursor int64, err error) {
	values := url.Values{}
	values.Set("cursor", strconv.FormatInt(cursor, 10))

	httpReq, err := c.newRequest(ctx, http.MethodGet, "/sessions/"+url.PathEscape(id)+"/logs?"+values.Encode(), nil)
	if err != nil {
		return nil, 0, err
	}

	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return nil, 0, err
	}

	data, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}

	nextCursor = cursor
	cursorHeader := resp.Header.Get("Agentruntime-Log-Cursor")
	if cursorHeader != "" {
		nextCursor, err = strconv.ParseInt(cursorHeader, 10, 64)
		if err != nil {
			return nil, 0, fmt.Errorf("parse Agentruntime-Log-Cursor: %w", err)
		}
	}

	return data, nextCursor, nil
}

func (c *Client) StreamLogs(ctx context.Context, id string) (io.ReadCloser, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()

	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		var cursor int64
		for {
			data, nextCursor, err := c.GetLogs(streamCtx, id, cursor)
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			cursor = nextCursor

			if len(data) > 0 {
				if _, err := pw.Write(data); err != nil {
					_ = pw.CloseWithError(err)
					return
				}
			}

			sess, err := c.GetSession(streamCtx, id)
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			if isTerminalStatus(sess.Status) {
				_ = pw.Close()
				return
			}

			select {
			case <-streamCtx.Done():
				_ = pw.CloseWithError(streamCtx.Err())
				return
			case <-ticker.C:
			}
		}
	}()

	return &streamReadCloser{ReadCloser: pr, cancel: cancel}, nil
}

type streamReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (s *streamReadCloser) Close() error {
	s.cancel()
	return s.ReadCloser.Close()
}

func (c *Client) newJSONRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := c.newRequest(ctx, method, path, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func (c *Client) doJSON(req *http.Request, out any) error {
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return err
	}

	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func checkResponse(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return fmt.Errorf("request failed with status %s", resp.Status)
	}
	if len(body) == 0 {
		return fmt.Errorf("request failed with status %s", resp.Status)
	}
	return fmt.Errorf("request failed with status %s: %s", resp.Status, strings.TrimSpace(string(body)))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func isTerminalStatus(status string) bool {
	return status == "completed" || status == "failed"
}
