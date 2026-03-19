package api

import (
	"net/http"
	"testing"
)

func TestDashboardRedirect(t *testing.T) {
	ts, _ := newTestServer(t)

	// Test redirect from / to /dashboard/
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // Don't follow redirects
		},
	}

	resp, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET / failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMovedPermanently {
		t.Errorf("Expected 301 redirect, got %d", resp.StatusCode)
	}

	location := resp.Header.Get("Location")
	if location != "/dashboard/" {
		t.Errorf("Expected redirect to /dashboard/, got %s", location)
	}
}

func TestAPIPrioritizesOverDashboard(t *testing.T) {
	ts, _ := newTestServer(t)

	// API routes should still work (health endpoint), even if dashboard files don't exist
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected 200 OK for /health, got %d", resp.StatusCode)
	}
}
