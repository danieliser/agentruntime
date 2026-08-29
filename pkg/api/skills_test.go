package api

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
	"github.com/danieliser/agentruntime/pkg/skills"
)

func TestValidateSkillAdmissionRequiresCanonicalAvailableBundles(t *testing.T) {
	store := skills.NewStore(t.TempDir())
	bundle := apiSkillBundle(t, "persist-comms")
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(bundle))
	if _, err := store.Put(digest, bytes.NewReader(bundle)); err != nil {
		t.Fatal(err)
	}
	server := &Server{skillBundles: store}
	valid := SessionRequest{Agent: "claude", Skills: &SkillSet{
		SchemaVersion: "1.0", Mode: "replace",
		Entries: []SkillRef{{Name: "persist-comms", BundleSHA256: digest}},
	}}
	if err := server.validateSkillAdmission(valid, "docker"); err != nil {
		t.Fatalf("valid admission: %v", err)
	}

	cases := []struct {
		name    string
		mutate  func(*SessionRequest)
		runtime string
	}{
		{"unsupported mode", func(r *SessionRequest) { r.Skills.Mode = "inherit" }, "docker"},
		{"unsupported local", func(*SessionRequest) {}, "local"},
		{"unsorted", func(r *SessionRequest) {
			r.Skills.Entries = []SkillRef{{Name: "z", BundleSHA256: digest}, {Name: "a", BundleSHA256: digest}}
		}, "docker"},
		{"missing bundle", func(r *SessionRequest) {
			r.Skills.Entries[0].BundleSHA256 = "sha256:" + fmt.Sprintf("%064d", 0)
		}, "docker"},
		{"mismatched bundle root", func(r *SessionRequest) { r.Skills.Entries[0].Name = "other" }, "docker"},
		{"unsafe name", func(r *SessionRequest) { r.Skills.Entries[0].Name = "../escape" }, "docker"},
		{"autojack", func(r *SessionRequest) { r.Agent = "autojack" }, "docker"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := valid
			set := *valid.Skills
			set.Entries = append([]SkillRef(nil), valid.Skills.Entries...)
			request.Skills = &set
			tc.mutate(&request)
			if err := server.validateSkillAdmission(request, tc.runtime); !durable.IsCode(err, durable.CodeInvalidArgument) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestPutSkillBundleEndpointAllowsAdvertisedArchiveSizeWithAuth(t *testing.T) {
	const authToken = "agentd-skill-upload-auth-token"
	dataDir := t.TempDir()
	server := NewServer(session.NewManager(), runtime.NewLocalRuntime(), agent.NewRegistry(), ServerConfig{
		DataDir:   dataDir,
		AuthToken: authToken,
	})
	bundle := apiSkillBundleWithBody(t, "large-skill", bytes.Repeat([]byte("x"), 2<<20))
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(bundle))
	request := httptest.NewRequest(http.MethodPut, "/api/v1/skill-bundles/"+digest, bytes.NewReader(bundle))
	request.Header.Set("Authorization", "Bearer "+authToken)
	request.Header.Set("Content-Type", "application/x-tar")
	response := httptest.NewRecorder()

	server.router.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestPutSkillBundleEndpointIsContentAddressedAndIdempotent(t *testing.T) {
	dataDir := t.TempDir()
	server := NewServer(session.NewManager(), runtime.NewLocalRuntime(), agent.NewRegistry(), ServerConfig{DataDir: dataDir})
	bundle := apiSkillBundle(t, "persist-comms")
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(bundle))
	for index, expectedStatus := range []int{http.StatusCreated, http.StatusOK} {
		request := httptest.NewRequest(http.MethodPut, "/api/v1/skill-bundles/"+digest, bytes.NewReader(bundle))
		request.Header.Set("Content-Type", "application/x-tar")
		response := httptest.NewRecorder()
		server.router.ServeHTTP(response, request)
		if response.Code != expectedStatus {
			t.Fatalf("attempt %d status = %d body=%s", index, response.Code, response.Body.String())
		}
		var envelope struct {
			Data struct {
				BundleSHA256 string `json:"bundle_sha256"`
				SizeBytes    int    `json:"size_bytes"`
				Idempotent   bool   `json:"idempotent"`
			} `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Data.BundleSHA256 != digest || envelope.Data.SizeBytes != len(bundle) || envelope.Data.Idempotent != (index == 1) {
			t.Fatalf("attempt %d response = %+v", index, envelope.Data)
		}
	}
}

func TestPutSkillBundleEndpointRejectsNonTarContentType(t *testing.T) {
	server := NewServer(session.NewManager(), runtime.NewLocalRuntime(), agent.NewRegistry(), ServerConfig{DataDir: t.TempDir()})
	bundle := apiSkillBundle(t, "persist-comms")
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(bundle))
	request := httptest.NewRequest(http.MethodPut, "/api/v1/skill-bundles/"+digest, bytes.NewReader(bundle))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	server.router.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
}

func apiSkillBundle(t *testing.T, name string) []byte {
	return apiSkillBundleWithBody(t, name, []byte("---\nname: "+name+"\n---\n"))
}

func apiSkillBundleWithBody(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)
	if err := tw.WriteHeader(&tar.Header{Name: name + "/SKILL.md", Mode: 0o444, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
