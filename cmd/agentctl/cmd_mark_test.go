package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMarkThenWaitOnlySeesLaterEvents(t *testing.T) {
	f := &fakeSession{state: "completed"}
	f.add("turn.completed")
	f.add("session.completed")
	srv := f.server(t)
	env := cliEnv(t, srv.URL)
	var out, log bytes.Buffer
	if code := run([]string{"mark", "s1", "--as", "wrapper", "--json"}, &out, &log, env); code != 0 {
		t.Fatalf("mark code=%d log=%s", code, &log)
	}
	var value map[string]any
	if err := json.Unmarshal(out.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if len(value) != 4 || value["session_id"] != "s1" || value["caller"] != "wrapper" || value["sequence"] != float64(2) || value["previous"] != nil {
		t.Fatalf("shape: %s", &out)
	}
	f.add("lifecycle.turn.started")
	f.add("turn.completed")
	out.Reset()
	log.Reset()
	if code := run([]string{"wait", "s1", "--as", "wrapper", "--cursor-dir", env["AGENTCTL_CURSOR_DIR"], "--until", "turn_end,done", "--timeout", "2s", "--json"}, &out, &log, env); code != ExitWaitTurnEnd {
		t.Fatalf("wait code=%d log=%s out=%s", code, &log, &out)
	}
	if len(f.wsAfter) != 1 || f.wsAfter[0] != "2" || !strings.Contains(out.String(), `"sequence":4`) {
		t.Fatalf("after=%v out=%s", f.wsAfter, &out)
	}
}

func TestMarkRestoreRollback(t *testing.T) {
	f := &fakeSession{state: "completed"}
	f.add("session.completed")
	srv := f.server(t)
	env := cliEnv(t, srv.URL)
	store := CursorStore{Dir: env["AGENTCTL_CURSOR_DIR"]}
	for _, restore := range []string{"0", "none"} {
		var out, log bytes.Buffer
		if code := run([]string{"mark", "s1", "--as", "wrapper", "--json"}, &out, &log, env); code != 0 {
			t.Fatalf("mark code=%d log=%s", code, &log)
		}
		out.Reset()
		log.Reset()
		if code := run([]string{"mark", "s1", "--as", "wrapper", "--restore", restore, "--json"}, &out, &log, env); code != 0 {
			t.Fatalf("restore code=%d log=%s", code, &log)
		}
		seq, found, err := store.Lookup("wrapper", controlKey("s1"))
		if err != nil || seq != 0 || found != (restore != "none") {
			t.Fatalf("restore %s: %d %v %v", restore, seq, found, err)
		}
		var got map[string]any
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got["previous"] != float64(1) || (restore == "none" && got["sequence"] != nil) || (restore == "0" && got["sequence"] != float64(0)) {
			t.Fatalf("restore shape=%s", &out)
		}
	}
	var out, log bytes.Buffer
	if code := run([]string{"wait", "s1", "--as", "wrapper", "--cursor-dir", env["AGENTCTL_CURSOR_DIR"], "--until", "done", "--timeout", "2s"}, &out, &log, env); code != ExitWaitDone {
		t.Fatalf("rollback hid completed session: %d %s", code, &log)
	}
}

func TestMarkValidationAndPreviousSequence(t *testing.T) {
	f := &fakeSession{state: "running"}
	f.add("turn.completed")
	srv := f.server(t)
	env := cliEnv(t, srv.URL)
	store := CursorStore{Dir: env["AGENTCTL_CURSOR_DIR"]}
	if err := store.Save("wrapper", controlKey("s1"), 0); err != nil {
		t.Fatal(err)
	}
	var out, log bytes.Buffer
	if code := run([]string{"mark", "s1", "--as", "wrapper", "--json"}, &out, &log, env); code != 0 {
		t.Fatalf("mark code=%d log=%s", code, &log)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || got["previous"] != float64(0) {
		t.Fatalf("previous: %s err=%v", &out, err)
	}
	for _, args := range [][]string{{"mark"}, {"mark", "s1", "--as", "wrapper", "--restore", "-1"}, {"mark", "s1", "--as", "wrapper", "--restore", "bad"}, {"mark", "s1", "--as", "wrapper", "--restore", ""}, {"mark", "s1", "--as", ""}, {"mark", "s1", "--as", "wrapper", "--restore"}} {
		out.Reset()
		log.Reset()
		if code := run(args, &out, &log, env); code != ExitUsage {
			t.Fatalf("%v code=%d log=%s", args, code, &log)
		}
	}
	if seq, err := store.Load("wrapper", controlKey("s1")); err != nil || seq != 1 {
		t.Fatalf("invalid mark changed store: %d %v", seq, err)
	}
}

func TestMarkFailurePreservesCursorsAndNoneRestoreWorksOffline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	env := cliEnv(t, srv.URL)
	store := CursorStore{Dir: env["AGENTCTL_CURSOR_DIR"]}
	for key, seq := range map[string]int64{controlKey("s"): 7, waitKey("s"): 8, "s": 9} {
		if err := store.Save("wrapper", key, seq); err != nil {
			t.Fatal(err)
		}
	}
	var out, log bytes.Buffer
	if code := run([]string{"mark", "s", "--as", "wrapper", "--json"}, &out, &log, env); code != ExitAPI || out.Len() != 0 {
		t.Fatalf("code=%d out=%s log=%s", code, &out, &log)
	}
	if seq, err := store.Load("wrapper", controlKey("s")); err != nil || seq != 7 {
		t.Fatalf("failed mark changed previous: %d %v", seq, err)
	}
	srv.Close()
	out.Reset()
	log.Reset()
	if code := run([]string{"mark", "s", "--as", "wrapper", "--restore", "3", "--json"}, &out, &log, env); code != ExitAPI {
		t.Fatalf("integer restore must validate the head: code=%d log=%s", code, &log)
	}
	if seq, err := store.Load("wrapper", controlKey("s")); err != nil || seq != 7 {
		t.Fatalf("failed restore changed mark: %d %v", seq, err)
	}
	out.Reset()
	log.Reset()
	if code := run([]string{"mark", "s", "--as", "wrapper", "--restore", "none", "--json"}, &out, &log, env); code != 0 {
		t.Fatalf("offline none restore code=%d log=%s", code, &log)
	}
	if _, found, err := store.Lookup("wrapper", controlKey("s")); err != nil || found {
		t.Fatalf("none restore did not remove mark: %v %v", found, err)
	}

	for key, want := range map[string]int64{waitKey("s"): 8, "s": 9} {
		if seq, err := store.Load("wrapper", key); err != nil || seq != want {
			t.Fatalf("key=%q seq=%d want=%d err=%v", key, seq, want, err)
		}
	}
}

func TestMarkRequiresExplicitCaller(t *testing.T) {
	f := &fakeSession{state: "running"}
	f.add("turn.completed")
	srv := f.server(t)
	env := cliEnv(t, srv.URL)
	for _, caller := range []string{"", "inherited"} {
		env["AGENTD_CALLER"] = caller
		for _, args := range [][]string{{"mark", "s1"}, {"mark", "s1", "--restore", "none"}} {
			var out, log bytes.Buffer
			if code := run(args, &out, &log, env); code != ExitUsage || out.Len() != 0 {
				t.Fatalf("env caller=%q args=%v code=%d out=%s log=%s", caller, args, code, &out, &log)
			}
		}
	}
	for _, args := range [][]string{{"--as", "wrapper", "mark", "s1"}, {"mark", "s1", "--as=wrapper"}, {"mark", "s1", "--as", "agentctl"}} {
		var out, log bytes.Buffer
		if code := run(args, &out, &log, env); code != 0 {
			t.Fatalf("explicit caller args=%v code=%d log=%s", args, code, &log)
		}
	}
}

func TestMarkCursorDirectoryMatchesWait(t *testing.T) {
	f := &fakeSession{state: "running"}
	f.add("turn.completed")
	srv := f.server(t)
	env := cliEnv(t, srv.URL)
	dir := t.TempDir()
	store := CursorStore{Dir: dir}
	var out, log bytes.Buffer
	if code := run([]string{"mark", "s1", "--as", "wrapper", "--cursor-dir", dir, "--json"}, &out, &log, env); code != 0 {
		t.Fatalf("mark code=%d log=%s", code, &log)
	}
	if seq, found, err := store.Lookup("wrapper", controlKey("s1")); err != nil || !found || seq != 1 {
		t.Fatalf("override mark: %d %v %v", seq, found, err)
	}
	defaults := CursorStore{Dir: env["AGENTCTL_CURSOR_DIR"]}
	if _, found, err := defaults.Lookup("wrapper", controlKey("s1")); err != nil || found {
		t.Fatalf("default store changed: %v %v", found, err)
	}
	f.add("turn.completed")
	out.Reset()
	log.Reset()
	if code := run([]string{"wait", "s1", "--as", "wrapper", "--cursor-dir", dir, "--until", "turn_end", "--timeout", "2s"}, &out, &log, env); code != ExitWaitTurnEnd {
		t.Fatalf("wait code=%d out=%s log=%s", code, &out, &log)
	}
	if len(f.wsAfter) != 1 || f.wsAfter[0] != "1" {
		t.Fatalf("wait did not use marked store: %v", f.wsAfter)
	}
	out.Reset()
	log.Reset()
	if code := run([]string{"mark", "s1", "--as", "wrapper", "--cursor-dir=" + dir, "--restore", "none"}, &out, &log, env); code != 0 {
		t.Fatalf("restore code=%d log=%s", code, &log)
	}
	if _, found, err := store.Lookup("wrapper", controlKey("s1")); err != nil || found {
		t.Fatalf("override restore: %v %v", found, err)
	}
}

func TestMarkRestoreRejectsFutureSequence(t *testing.T) {
	f := &fakeSession{state: "running"}
	for i := 0; i < 5; i++ {
		f.add("content.delta")
	}
	srv := f.server(t)
	env := cliEnv(t, srv.URL)
	store := CursorStore{Dir: env["AGENTCTL_CURSOR_DIR"]}
	if err := store.Save("wrapper", controlKey("s1"), 2); err != nil {
		t.Fatal(err)
	}
	var out, log bytes.Buffer
	if code := run([]string{"mark", "s1", "--as", "wrapper", "--restore", "6", "--json"}, &out, &log, env); code != ExitUsage || out.Len() != 0 {
		t.Fatalf("future restore code=%d out=%s log=%s", code, &out, &log)
	}
	if seq, err := store.Load("wrapper", controlKey("s1")); err != nil || seq != 2 {
		t.Fatalf("rejection changed mark: %d %v", seq, err)
	}
	out.Reset()
	log.Reset()
	if code := run([]string{"mark", "s1", "--as", "wrapper", "--restore", "5", "--json"}, &out, &log, env); code != 0 {
		t.Fatalf("restore at head code=%d log=%s", code, &log)
	}
	if seq, err := store.Load("wrapper", controlKey("s1")); err != nil || seq != 5 {
		t.Fatalf("restore at head: %d %v", seq, err)
	}
}

func TestMarkAndWaitShareDefaultCursorDirectory(t *testing.T) {
	// Keep the process fallback private while exercising run's injected environment.
	t.Setenv("AGENTCTL_CURSOR_DIR", t.TempDir())
	f := &fakeSession{state: "running"}
	f.add("turn.completed")
	srv := f.server(t)
	env := cliEnv(t, srv.URL)
	var out, log bytes.Buffer
	if code := run([]string{"mark", "s1", "--as", "wrapper"}, &out, &log, env); code != 0 {
		t.Fatalf("mark code=%d log=%s", code, &log)
	}
	f.add("turn.completed")
	out.Reset()
	log.Reset()
	if code := run([]string{"wait", "s1", "--as", "wrapper", "--until", "turn_end", "--timeout", "2s"}, &out, &log, env); code != ExitWaitTurnEnd {
		t.Fatalf("default stores differ: code=%d out=%s log=%s", code, &out, &log)
	}
}
