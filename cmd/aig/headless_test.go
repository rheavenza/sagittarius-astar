package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The tests re-run the test binary as aig (AIG_TEST_MAIN=1), so flags,
// stdin, stdout and exit codes behave exactly as in the real command.
func TestMain(m *testing.M) {
	if os.Getenv("AIG_TEST_MAIN") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeModel is an OpenAI-compatible streaming endpoint that plays scripted
// replies and records the requests.
type fakeModel struct {
	mu       sync.Mutex
	replies  []string // one SSE body per request
	requests []map[string]any
}

func (f *fakeModel) handler(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.requests = append(f.requests, body)
	reply := "data: " + textChunk("(no more replies)") + "\n\n" + finishChunk("stop")
	if len(f.replies) > 0 {
		reply, f.replies = f.replies[0], f.replies[1:]
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, reply)
	fmt.Fprint(w, "data: "+`{"id":"x","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":7,"total_tokens":107}}`+"\n\n")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func textChunk(s string) string {
	b, _ := json.Marshal(s)
	return `{"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":` + string(b) + `}}]}`
}

func finishChunk(reason string) string {
	return "data: " + `{"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"` + reason + `"}]}` + "\n\n"
}

func textReply(s string) string { return "data: " + textChunk(s) + "\n\n" + finishChunk("stop") }

func toolReply(id, name string, args map[string]any) string {
	a, _ := json.Marshal(args)
	as, _ := json.Marshal(string(a))
	return "data: " + `{"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"` + id +
		`","type":"function","function":{"name":"` + name + `","arguments":` + string(as) + `}}]}}]}` + "\n\n" + finishChunk("tool_calls")
}

type runResult struct {
	stdout, stderr string
	code           int
}

// runAig runs aig with args in a scratch home and workspace.
func runAig(t *testing.T, f *fakeModel, home, dir, stdin string, args ...string) runResult {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()
	cmd := exec.Command(os.Args[0], append([]string{"--provider", "nim", "--model", "test-model"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"AIG_TEST_MAIN=1",
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"XDG_CACHE_HOME="+filepath.Join(home, ".cache"),
		"XDG_DATA_HOME="+filepath.Join(home, ".local", "share"),
		"NVIDIA_API_KEY=test",
		"NIM_BASE_URL="+srv.URL+"/v1",
		"NO_COLOR=1",
	)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return runResult{out.String(), errb.String(), code}
}

func messages(req map[string]any) []map[string]any {
	var out []map[string]any
	for _, m := range req["messages"].([]any) {
		out = append(out, m.(map[string]any))
	}
	return out
}

func TestHeadlessToolThenAnswer(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("the answer is 42\n"), 0o644)
	f := &fakeModel{replies: []string{
		toolReply("c1", "read_file", map[string]any{"path": "notes.txt"}),
		textReply("It is 42."),
	}}
	r := runAig(t, f, home, dir, "", "-p", "what", "is", "the", "answer?")
	if r.code != 0 {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	if r.stdout != "It is 42.\n" {
		t.Fatalf("stdout = %q, want only the final answer", r.stdout)
	}
	if len(f.requests) != 2 {
		t.Fatalf("%d requests, want 2", len(f.requests))
	}
	msgs := messages(f.requests[1])
	if got := msgs[len(msgs)-1]["content"].(string); !strings.Contains(got, "the answer is 42") {
		t.Fatalf("read_file result not sent back: %q", got)
	}
	var user string
	for _, m := range msgs {
		if m["role"] == "user" {
			user = m["content"].(string)
		}
	}
	if user != "what is the answer?" {
		t.Fatalf("user message = %q", user)
	}

	// The run was saved as a session that -r continues.
	f.replies = []string{textReply("Still 42.")}
	r = runAig(t, f, home, dir, "", "-r", "-p", "sure?")
	if r.code != 0 || r.stdout != "Still 42.\n" {
		t.Fatalf("resume: exit %d stdout %q stderr %s", r.code, r.stdout, r.stderr)
	}
	msgs = messages(f.requests[2])
	if len(msgs) < 5 || !strings.Contains(fmt.Sprint(msgs), "It is 42.") {
		t.Fatalf("resumed request lacks the earlier conversation: %v", msgs)
	}
}

func TestHeadlessStdinAndDeniedWrite(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	f := &fakeModel{replies: []string{
		toolReply("c1", "write_file", map[string]any{"path": "out.txt", "content": "x"}),
		textReply("Could not write."),
	}}
	r := runAig(t, f, home, dir, "diff --git a/x b/x\n+hello\n", "-p", "--output-format", "json", "commit message")
	if r.code != 0 {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); err == nil {
		t.Fatal("write ran without approval in default mode")
	}
	var res headlessResult
	if err := json.Unmarshal([]byte(r.stdout), &res); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, r.stdout)
	}
	if res.Result != "Could not write." || res.IsError || len(res.Denied) != 1 || res.Requests != 2 ||
		res.Usage.InputTokens != 200 || res.Usage.OutputTokens != 14 || res.SessionID == "" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if !strings.Contains(r.stderr, "denied") {
		t.Fatalf("denial not reported on stderr: %q", r.stderr)
	}
	msgs := messages(f.requests[0])
	user := msgs[len(msgs)-1]["content"].(string)
	if !strings.HasPrefix(user, "commit message\n\n<stdin>\ndiff --git") || !strings.HasSuffix(user, "+hello\n</stdin>") {
		t.Fatalf("stdin not attached: %q", user)
	}
	msgs = messages(f.requests[1])
	if got := msgs[len(msgs)-1]["content"].(string); !strings.Contains(got, "headless") {
		t.Fatalf("model not told why the write was denied: %q", got)
	}

	// accept-edits lets the write through.
	f.replies = []string{
		toolReply("c1", "write_file", map[string]any{"path": "out.txt", "content": "x"}),
		textReply("Done."),
	}
	r = runAig(t, f, home, dir, "", "-p", "--permission-mode", "accept-edits", "write it")
	if r.code != 0 || r.stdout != "Done.\n" {
		t.Fatalf("exit %d stdout %q stderr %s", r.code, r.stdout, r.stderr)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "out.txt")); err != nil || string(b) != "x" {
		t.Fatalf("accept-edits write: %q %v", b, err)
	}
}

func TestHeadlessStreamJSONAndErrors(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	f := &fakeModel{replies: []string{textReply("hi")}}
	r := runAig(t, f, home, dir, "", "-p", "--output-format", "stream-json", "hello")
	if r.code != 0 {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	var types []string
	for _, l := range lines {
		var ev map[string]any
		if err := json.Unmarshal([]byte(l), &ev); err != nil {
			t.Fatalf("bad line %q: %v", l, err)
		}
		types = append(types, ev["type"].(string))
	}
	if got := strings.Join(types, ","); got != "init,text,usage,result" {
		t.Fatalf("event types = %s", got)
	}

	// No prompt at all.
	if r := runAig(t, f, home, dir, "", "-p"); r.code != 1 || !strings.Contains(r.stderr, "needs a prompt") {
		t.Fatalf("empty prompt: exit %d stderr %q", r.code, r.stderr)
	}
	// Stray arguments without -p don't start the TUI.
	if r := runAig(t, f, home, dir, "", "hello"); r.code != 1 || !strings.Contains(r.stderr, "aig -p") {
		t.Fatalf("stray args: exit %d stderr %q", r.code, r.stderr)
	}
	// A provider error exits 1 with the message on stderr.
	r = runAig(t, &fakeModel{replies: []string{"data: {not json\n\n"}}, home, dir, "", "-p", "x")
	if r.code != 1 || r.stdout != "" || r.stderr == "" {
		t.Fatalf("provider error: exit %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
}

func TestHeadlessMentionAttachesFile(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("the answer is 42\n"), 0o644)
	f := &fakeModel{replies: []string{textReply("42")}}
	r := runAig(t, f, home, dir, "", "-p", "-q", "read @notes.txt and @nope/missing.go")
	if r.code != 0 || r.stdout != "42\n" {
		t.Fatalf("exit %d stdout %q stderr %s", r.code, r.stdout, r.stderr)
	}
	msgs := messages(f.requests[0])
	if got := msgs[len(msgs)-1]["content"].(string); !strings.HasSuffix(got, "<file path=\"notes.txt\">\nthe answer is 42\n</file>") {
		t.Fatalf("@notes.txt not attached: %q", got)
	}
	if !strings.Contains(r.stderr, "@nope/missing.go: no such file") {
		t.Fatalf("missing mention not reported (even with -q): %q", r.stderr)
	}
}
