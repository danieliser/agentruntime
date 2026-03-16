package materialize

import (
	"strings"
	"testing"
)

func TestResolveHostGateway(t *testing.T) {
	if got := ResolveHostGateway(); got == "" {
		t.Fatal("expected non-empty host gateway")
	}
}

func TestResolveVars_Substitutes(t *testing.T) {
	got := ResolveVars("http://${HOST_GATEWAY}:8080")
	if got == "http://${HOST_GATEWAY}:8080" {
		t.Fatal("expected HOST_GATEWAY placeholder to be replaced")
	}
	if !strings.Contains(got, ResolveHostGateway()) {
		t.Fatalf("expected resolved gateway in %q", got)
	}
}

func TestResolveVars_NoMatch(t *testing.T) {
	want := "http://example.com:8080"
	if got := ResolveVars(want); got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}
