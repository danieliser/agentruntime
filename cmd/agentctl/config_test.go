package main

import (
	"bytes"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestParseArgsGlobalFlagsBeforeAndAfterCommand(t *testing.T) {
	got, err := ParseArgs([]string{"--url", "http://localhost:9", "status", "abc", "--as", "worker", "--json", "--provider", "claude"}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Config.URL != "http://localhost:9" || got.Config.Caller != "worker" || !got.Config.JSON || got.Command != "status" || !reflect.DeepEqual(got.Args, []string{"abc", "--provider", "claude"}) {
		t.Fatalf("unexpected parse: %#v", got)
	}
}

func TestParseArgsDefaultsAndRegistry(t *testing.T) {
	got, err := ParseArgs([]string{"ls"}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Config.URL != defaultURL || got.Config.Caller == "" || got.Config.JSON {
		t.Fatalf("unexpected defaults: %#v", got.Config)
	}
	if _, err := ParseArgs([]string{"unknown"}, map[string]string{}); err == nil {
		t.Fatal("expected unknown command error")
	}
}

func TestParsePersistOptionsAndSessionReference(t *testing.T) {
	got, err := ParseArgs([]string{"--persist-url", "http://persist.test:9", "status", "plan:p-1/node-a"}, map[string]string{"PERSIST_URL": "http://env.test"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Config.PersistURL != "http://persist.test:9" {
		t.Fatalf("persist URL = %q", got.Config.PersistURL)
	}
	ref, err := ParseSessionReference("plan:p-1/node-a")
	if err != nil || ref.Kind != "plan_node" || ref.ID != "p-1" || ref.Node != "node-a" {
		t.Fatalf("reference = %#v, %v", ref, err)
	}
	ref, err = ParseSessionReference("task:t-1")
	if err != nil || ref.Kind != "task" || ref.ID != "t-1" {
		t.Fatalf("reference = %#v, %v", ref, err)
	}
}

func TestHelpMentionsPersistDispatch(t *testing.T) {
	if !strings.Contains(helpText(), "persist plan auto") {
		t.Fatal("help does not point to persist CLI dispatch")
	}
}

func TestLoadTokenFromConfiguredFileAndRedactsErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	token := "secret-bearer-value"
	path := t.TempDir() + "/token"
	if err := os.WriteFile(path, []byte("  "+token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadToken(map[string]string{"AGENTD_TOKEN_FILE": path})
	if err != nil || got != token {
		t.Fatalf("LoadToken() = %q, %v", got, err)
	}
	err = redactError(strings.NewReader(token), token)
	if strings.Contains(err.Error(), token) {
		t.Fatal("token leaked in error")
	}
	var out bytes.Buffer
	ctx := &Context{Config: Config{JSON: true, Token: token}, Out: &out, Command: "status"}
	if err := skeletonHandler(ctx, []string{token}); err != nil {
		t.Fatal(err)
	}
	argv := []string{"status", "session-1"}
	if strings.Contains(out.String()+strings.Join(argv, " "), token) {
		t.Fatal("token leaked in output or argv")
	}
}
