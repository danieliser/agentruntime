package api

import "testing"

func TestCodingEnvironmentIsDerivedOnlyFromCodingPermissions(t *testing.T) {
	tests := []struct {
		name    string
		request SessionRequest
		want    bool
	}{
		{"restricted coding", SessionRequest{ProviderPermissions: &ProviderPermissions{Mode: "restricted", ApprovalPolicy: "on-request"}}, true},
		{"web workspace write", SessionRequest{ExecutionPolicy: &ExecutionPolicy{Filesystem: "workspace_write", ApprovalPolicy: "never", Network: "public_https"}}, false},
		{"web read only", SessionRequest{ExecutionPolicy: &ExecutionPolicy{Filesystem: "read_only", ApprovalPolicy: "never"}}, false},
		{"conflicting policy never enables", SessionRequest{ProviderPermissions: &ProviderPermissions{Mode: "restricted", ApprovalPolicy: "on-request"}, ExecutionPolicy: &ExecutionPolicy{Filesystem: "read_only", ApprovalPolicy: "never"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := nativePolicy(tt.request)
			if p.AllowLocalEnvironment != tt.want {
				t.Fatalf("derived policy %+v", p)
			}
			if !p.Enforced {
				t.Fatal("policy lost enforcement")
			}
			if tt.want && (p.ApprovalPolicy != "on-request" || p.Filesystem != "workspace_write") {
				t.Fatalf("coding policy widened %+v", p)
			}
		})
	}
}
