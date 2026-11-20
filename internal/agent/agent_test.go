package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
)

// ── fake provider ────────────────────────────────────────────────────────────

type reply struct {
	tokens    []string
	calls     []llm.ToolCall
	reasoning string
	err       error
	block     bool // wait for ctx cancellation
}

type fakeProvider struct {
	mu       sync.Mutex
	replies  []reply
	requests [][]llm.Message
}

func (f *fakeProvider) StreamChat(ctx context.Context, msgs []llm.Message, _ []llm.Tool, ch chan<- string) ([]llm.ToolCall, string, error) {
	defer close(ch)
	f.mu.Lock()
	f.requests = append(f.requests, append([]llm.Message(nil), msgs...))
	if len(f.replies) == 0 {
		f.mu.Unlock()
		return nil, "", nil
	}
	r := f.replies[0]
	f.replies = f.replies[1:]
	f.mu.Unlock()

	if r.block {
		<-ctx.Done()
		return nil, "", ctx.Err()
	}
	for _, tok := range r.tokens {
		ch <- tok
	}
	return r.calls, r.reasoning, r.err
}

// ── harness ──────────────────────────────────────────────────────────────────

// collect drains events, answering approvals with answer (nil = approve).
func collect(t *testing.T, events <-chan Event, answer func(*ApprovalRequest)) ([]Event, []llm.Message) {
	t.Helper()
	var all []Event
	var msgs []llm.Message
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return all, msgs
			}
			all = append(all, ev)
			switch e := ev.(type) {
			case MessageEvent:
				msgs = append(msgs, e.Msg)
			case ApprovalEvent:
				if answer != nil {
					answer(e.Req)
				} else {
					e.Req.Approve("")
				}
			}
		case <-timeout:
			t.Fatal("turn did not finish")
		}
	}
}

func done(t *testing.T, evs []Event) DoneEvent {
	t.Helper()
	d, ok := evs[len(evs)-1].(DoneEvent)
	if !ok {
		t.Fatalf("last event is %T, want DoneEvent", evs[len(evs)-1])
	}
	return d
}

// assertAllCallsAnswered checks the invariant providers rely on.
func assertAllCallsAnswered(t *testing.T, msgs []llm.Message) {
	t.Helper()
	answered := map[string]bool{}
	for _, m := range msgs {
		if m.Role == llm.RoleTool {
			answered[m.ToolCallID] = true
		}
	}
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			if !answered[c.ID] {
				t.Errorf("tool call %s (%s) has no result", c.ID, c.Name)
			}
		}
	}
}

func user(s string) []llm.Message {
	return []llm.Message{{Role: llm.RoleSystem, Content: "sys"}, {Role: llm.RoleUser, Content: s}}
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestTextAnswerWithInlineReasoning(t *testing.T) {
	p := &fakeProvider{replies: []reply{{tokens: []string{"<thi", "nk>hmm</think>Hel", "lo"}}}}
	a := New(Options{Provider: p, ContextBlock: "# Environment\n- x"})
	evs, msgs := collect(t, a.Run(context.Background(), user("hi")), nil)

	if d := done(t, evs); d.Err != nil || d.Aborted {
		t.Fatalf("unexpected done: %+v", d)
	}
	if len(msgs) != 1 || msgs[0].Content != "Hello" || msgs[0].ReasoningContent != "hmm" {
		t.Fatalf("assistant message = %+v", msgs)
	}
	// Context block goes to the provider but not into history.
	if !strings.Contains(p.requests[0][0].Content, "# Environment") {
		t.Error("context block not sent")
	}
}

func TestToolLoopReadThenAnswer(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	os.WriteFile(f, []byte("secret-value\n"), 0o644)

	p := &fakeProvider{replies: []reply{
		{calls: []llm.ToolCall{{ID: "1", Name: "read_file", Args: map[string]any{"path": f}}}},
		{tokens: []string{"done"}},
	}}
	a := New(Options{Provider: p, WorkspaceRoot: dir})
	evs, msgs := collect(t, a.Run(context.Background(), user("read it")), func(r *ApprovalRequest) {
		t.Fatalf("read inside workspace must not ask: %+v", r)
	})
	done(t, evs)

	if len(msgs) != 3 || msgs[1].Role != llm.RoleTool || !strings.Contains(msgs[1].Content, "secret-value") {
		t.Fatalf("history = %+v", msgs)
	}
	// Second request includes the tool result.
	last := p.requests[1][len(p.requests[1])-1]
	if last.Role != llm.RoleTool || last.ToolCallID != "1" {
		t.Fatalf("tool result not sent back: %+v", last)
	}
}

func TestCommandApprovalDenyAndEdit(t *testing.T) {
	p := &fakeProvider{replies: []reply{
		{calls: []llm.ToolCall{
			{ID: "1", Name: "run_command", Args: map[string]any{"command": "echo original"}},
			{ID: "2", Name: "run_command", Args: map[string]any{"command": "echo second"}},
		}},
		{tokens: []string{"ok"}},
	}}
	a := New(Options{Provider: p})
	n := 0
	evs, msgs := collect(t, a.Run(context.Background(), user("go")), func(r *ApprovalRequest) {
		if r.Kind != ApproveCommand {
			t.Fatalf("unexpected approval kind %v", r.Kind)
		}
		n++
		if n == 1 {
			r.Approve("echo edited")
		} else {
			r.Deny()
		}
	})
	done(t, evs)
	assertAllCallsAnswered(t, msgs)

	if !strings.Contains(msgs[1].Content, "edited") || !strings.Contains(msgs[1].Content, "user edited the command") {
		t.Errorf("edited command not run/reported: %q", msgs[1].Content)
	}
	if msgs[0].ToolCalls[0].Args["command"] != "echo original" {
		t.Error("history must keep the model's original call")
	}
	if !strings.Contains(msgs[2].Content, "skipped by user") {
		t.Errorf("denied command result = %q", msgs[2].Content)
	}
}

func TestCancelDuringApprovalAnswersEverything(t *testing.T) {
	p := &fakeProvider{replies: []reply{{calls: []llm.ToolCall{
		{ID: "1", Name: "run_command", Args: map[string]any{"command": "echo a"}},
		{ID: "2", Name: "read_file", Args: map[string]any{"path": "x"}},
	}}}}
	a := New(Options{Provider: p})
	ctx, cancel := context.WithCancel(context.Background())
	evs, msgs := collect(t, a.Run(ctx, user("go")), func(r *ApprovalRequest) { cancel() })

	if d := done(t, evs); !d.Aborted {
		t.Fatalf("expected aborted, got %+v", d)
	}
	assertAllCallsAnswered(t, msgs)
	if len(p.requests) != 1 {
		t.Error("model must not be called again after abort")
	}
}

func TestCancelWhileStreaming(t *testing.T) {
	p := &fakeProvider{replies: []reply{{block: true}}}
	a := New(Options{Provider: p})
	ctx, cancel := context.WithCancel(context.Background())
	ch := a.Run(ctx, user("go"))
	time.AfterFunc(50*time.Millisecond, cancel)
	evs, msgs := collect(t, ch, nil)
	if d := done(t, evs); !d.Aborted || len(msgs) != 0 {
		t.Fatalf("done=%+v msgs=%v", d, msgs)
	}
}

func TestSameCallLoopBreaker(t *testing.T) {
	same := llm.ToolCall{Name: "list_directory", Args: map[string]any{"path": "."}}
	call := func(id string) llm.ToolCall { c := same; c.ID = id; return c }
	p := &fakeProvider{replies: []reply{
		{calls: []llm.ToolCall{call("1")}},
		{calls: []llm.ToolCall{call("2")}},
		{calls: []llm.ToolCall{call("3"), call("4")}},
		{tokens: []string{"giving up"}},
	}}
	dir := t.TempDir()
	a := New(Options{Provider: p, WorkspaceRoot: dir})
	t.Chdir(dir)
	evs, msgs := collect(t, a.Run(context.Background(), user("go")), nil)
	done(t, evs)
	assertAllCallsAnswered(t, msgs)

	var notices, notes int
	for _, e := range evs {
		if _, ok := e.(NoticeEvent); ok {
			notices++
		}
	}
	for _, m := range msgs {
		if m.Role == llm.RoleSystem && strings.Contains(m.Content, "Loop breaker") {
			notes++
		}
	}
	if notices != 1 || notes != 1 {
		t.Fatalf("expected one breaker notice and note, got %d/%d", notices, notes)
	}
}

func TestErrorLoopBreaker(t *testing.T) {
	bad := func(id string) llm.ToolCall {
		return llm.ToolCall{ID: id, Name: "read_file", Args: map[string]any{"path": "missing-" + id}}
	}
	p := &fakeProvider{replies: []reply{
		{calls: []llm.ToolCall{bad("1"), bad("2"), bad("3"), bad("4")}},
		{tokens: []string{"ok"}},
	}}
	dir := t.TempDir()
	t.Chdir(dir)
	a := New(Options{Provider: p, WorkspaceRoot: dir})
	evs, msgs := collect(t, a.Run(context.Background(), user("go")), nil)
	done(t, evs)
	assertAllCallsAnswered(t, msgs)
	if !strings.Contains(msgs[4].Content, "loop breaker") {
		t.Fatalf("4th call should be skipped by the breaker: %q", msgs[4].Content)
	}
}

func TestWriteAlwaysAsksEvenWithAutoApprove(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "x.json")
	p := &fakeProvider{replies: []reply{
		{calls: []llm.ToolCall{{ID: "1", Name: "write_file", Args: map[string]any{"path": f, "content": `{"a":1}`}}}},
		{tokens: []string{"written"}},
	}}
	a := New(Options{Provider: p, WorkspaceRoot: dir})
	a.SetAutoApprove(true)
	asked := false
	evs, msgs := collect(t, a.Run(context.Background(), user("write")), func(r *ApprovalRequest) {
		asked = true
		if r.Kind != ApproveWrite || r.Write == nil || r.Write.Path != f {
			t.Fatalf("bad write approval: %+v", r)
		}
		r.Approve("")
	})
	done(t, evs)
	if !asked {
		t.Fatal("writes must always ask")
	}
	if b, _ := os.ReadFile(f); string(b) != `{"a":1}` {
		t.Fatalf("file not written: %q", b)
	}
	if !strings.Contains(msgs[1].Content, "Post-edit check (JSON): OK") {
		t.Errorf("post-edit check missing: %q", msgs[1].Content)
	}
}

func TestOutsideWorkspaceReadAsksAndCanBeDenied(t *testing.T) {
	p := &fakeProvider{replies: []reply{
		{calls: []llm.ToolCall{{ID: "1", Name: "read_file", Args: map[string]any{"path": "/etc/hostname"}}}},
		{tokens: []string{"ok"}},
	}}
	a := New(Options{Provider: p, WorkspaceRoot: t.TempDir()})
	evs, msgs := collect(t, a.Run(context.Background(), user("go")), func(r *ApprovalRequest) {
		if r.Kind != ApproveTool {
			t.Fatalf("kind = %v", r.Kind)
		}
		r.Deny()
	})
	done(t, evs)
	if !strings.Contains(msgs[1].Content, "denied") {
		t.Fatalf("denied read result = %q", msgs[1].Content)
	}
}

func TestStreamErrorReported(t *testing.T) {
	p := &fakeProvider{replies: []reply{{err: context.DeadlineExceeded}}}
	evs, _ := collect(t, New(Options{Provider: p}).Run(context.Background(), user("x")), nil)
	if d := done(t, evs); d.Err == nil {
		t.Fatal("expected error")
	}
}

func TestWithContextBlock(t *testing.T) {
	h := []llm.Message{{Role: llm.RoleSystem, Content: "persona"}, {Role: llm.RoleUser, Content: "hi"}}
	out := WithContextBlock(h, "# Environment")
	if out[0].Content != "persona\n\n# Environment" || h[0].Content != "persona" {
		t.Fatalf("got %q (original %q)", out[0].Content, h[0].Content)
	}
	if out = WithContextBlock(h[1:], "# Env"); out[0].Role != llm.RoleSystem || len(out) != 2 {
		t.Fatalf("expected a new system message: %+v", out)
	}
}
