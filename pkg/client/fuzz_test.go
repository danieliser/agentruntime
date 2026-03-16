package client

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/danieliser/agentruntime/pkg/api"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func FuzzClientDispatch_ResponseParsing(f *testing.F) {
	f.Add([]byte(`{"session_id":"sess-123","agent":"claude","runtime":"local","status":"running","ws_url":"ws://example.test/ws","log_url":"http://example.test/logs"}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`{"session_id":`))
	f.Add([]byte(""))
	f.Add([]byte{0xff, 0xfe, 0xfd})

	f.Fuzz(func(t *testing.T, body []byte) {
		status := http.StatusCreated
		if len(body) > 0 && body[0]%5 == 0 {
			status = http.StatusInternalServerError
		}

		client := &Client{
			BaseURL: "http://example.test",
			HTTPClient: &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.Method != http.MethodPost {
						t.Fatalf("expected POST, got %s", req.Method)
					}

					if req.Body != nil {
						_, _ = io.Copy(io.Discard, req.Body)
						_ = req.Body.Close()
					}

					return &http.Response{
						StatusCode: status,
						Status:     http.StatusText(status),
						Header:     make(http.Header),
						Body:       io.NopCloser(bytes.NewReader(body)),
						Request:    req,
					}, nil
				}),
			},
		}

		resp, err := client.Dispatch(context.Background(), api.SessionRequest{
			Agent:  "claude",
			Prompt: "fuzz dispatch",
		})
		if err == nil && resp == nil {
			t.Fatal("expected non-nil response when Dispatch succeeds")
		}
	})
}
