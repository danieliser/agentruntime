package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientSendsTokenOnlyAsAuthorizationHeaderAndRedactsFailure(t *testing.T) {
	const token = "do-not-leak-this-token"
	var gotAuth, gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.RawQuery
		http.Error(w, "upstream reflected "+token, http.StatusInternalServerError)
	}))
	defer ts.Close()
	c := NewAPIClient(Config{URL: ts.URL, Token: token})
	_, err := c.GetSession(context.Background(), "sensitive-id")
	if err == nil {
		t.Fatal("expected request error")
	}
	if gotAuth != "Bearer "+token {
		t.Fatalf("Authorization header missing: %q", gotAuth)
	}
	if strings.Contains(gotQuery, token) || strings.Contains(err.Error(), token) {
		t.Fatal("token leaked to URL or error")
	}
}

func TestHTTPFailureCodeComesFromStatusNotBody(t *testing.T) {
	c := NewAPIClient(Config{})
	err := c.safeError(fmt.Errorf("request failed with status 500 Internal Server Error: upstream session 404 failed"))
	if !errors.Is(err, ErrAPI) {
		t.Fatalf("got %v", err)
	}
	err = c.safeError(context.DeadlineExceeded)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v", err)
	}
}
