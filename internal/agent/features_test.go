package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
)

// ── update_plan ──────────────────────────────────────────────────────────────

func planCall(id string, steps ...string) llm.ToolCall {
	var items []any
	for _, s := range steps {
		step, status, _ := strings.Cut(s, "=")
		items = append(items, map[string]any{"step": step, "status": status})
	}
	return llm.ToolCall{ID: id, Name: "update_plan", Args: map[string]any{"items": items}}
}

func TestUpdatePlanEmitsEventAndSurvivesInHistory(t *testing.T) {
	p := &fakeProvider{replies: []reply{
		{calls: []llm.ToolCall{planCall("1", "read code=done", "fix bug=in_progress", "run tests=pending")}},
		{tokens: []string{"ok"}},
	}}
	a := New(Options{Provider: p})
	evs, msgs := collect(t, a.Run(context.Background(), user("fix it")), nil)
	done(t, evs)
	var plan *PlanEvent
	for _, ev := range evs {
		if e, ok := ev.(PlanEvent); ok {
			plan = &e
		}
	}
	if plan == nil || len(plan.Items) != 3 || plan.Items[1].Status != PlanInProgress {
		t.Fatalf("plan event = %+v", plan)
	}
	if !strings.Contains(msgs[1].Content, "Plan updated (1/3 done)") {
		t.Errorf("tool result = %q", msgs[1].Content)
	}
	// A new turn on the same history rebuilds the plan.
	hist := append(user("fix it"), msgs...)
	if got := PlanFromHistory(hist); len(got) != 3 || got[0].Status != PlanDone {
		t.Fatalf("PlanFromHistory = %+v", got)
	}
	if len(PlanFromHistory(user("new session"))) != 0 {
		t.Error("a fresh history has no plan")
	}
}

func TestUpdatePlanValidation(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"two in progress": planCall("x", "a=in_progress", "b=in_progress").Args,
		"bad status":      planCall("x", "a=maybe").Args,
		"empty step":      planCall("x", "=pending").Args,
		"not an array":    {"items": "do stuff"},
	} {
		if _, err := parsePlan(args); err == nil {
			t.Errorf("%s: should be rejected", name)
		}
	}
	items, err := parsePlan(planCall("x", "a=completed", "b=todo", "c=").Args)
	if err != nil || items[0].Status != PlanDone || items[1].Status != PlanPending || items[2].Status != PlanPending {
		t.Fatalf("aliases: %+v %v", items, err)
	}
	// A failed update_plan call doesn't count when rebuilding the plan.
	h := []llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{planCall("1", "good=pending")}},
		{Role: llm.RoleTool, ToolCallID: "1", ToolName: "update_plan", Content: "Plan updated (0/1 done)."},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{planCall("2", "x=in_progress", "y=in_progress")}},
		{Role: llm.RoleTool, ToolCallID: "2", ToolName: "update_plan", Content: "Error: update_plan: 2 steps are in_progress"},
	}
	if got := PlanFromHistory(h); len(got) != 1 || got[0].Step != "good" {
		t.Fatalf("PlanFromHistory = %+v", got)
	}
}

func TestPlanToolsNotOfferedToExploreSubagents(t *testing.T) {
	a := New(Options{})
	for _, d := range a.exploreDefs() {
		switch d.Name {
		case "update_plan", "ask_user", "read_process_output", "kill_process":
			t.Errorf("explore subagents must not get %s", d.Name)
		}
	}
}

// ── ask_user ─────────────────────────────────────────────────────────────────

func TestAskUserAnswerAndDecline(t *testing.T) {
	ask := llm.ToolCall{ID: "1", Name: "ask_user", Args: map[string]any{"question": "Postgres or SQLite?", "options": []any{"Postgres", "SQLite"}}}
	p := &fakeProvider{replies: []reply{{calls: []llm.ToolCall{ask}}, {tokens: []string{"ok"}}}}
	a := New(Options{Provider: p})
	a.SetMode(ModeAuto) // a question is asked even in auto mode
	var got *ApprovalRequest
	evs, msgs := collect(t, a.Run(context.Background(), user("set up a db")), func(r *ApprovalRequest) {
		got = r
		r.Answer("SQLite, it's a CLI tool")
	})
	done(t, evs)
	if got == nil || got.Kind != ApproveQuestion || got.Question != "Postgres or SQLite?" || len(got.Options) != 2 {
		t.Fatalf("question request = %+v", got)
	}
	if msgs[1].Content != "The user answered: SQLite, it's a CLI tool" {
		t.Errorf("result = %q", msgs[1].Content)
	}

	p = &fakeProvider{replies: []reply{{calls: []llm.ToolCall{ask}}, {tokens: []string{"ok"}}}}
	a = New(Options{Provider: p})
	evs, msgs = collect(t, a.Run(context.Background(), user("set up a db")), func(r *ApprovalRequest) { r.Deny() })
	done(t, evs)
	if !strings.Contains(msgs[1].Content, "did not answer") {
		t.Errorf("declined result = %q", msgs[1].Content)
	}
}

// ── parallel read-only calls ─────────────────────────────────────────────────

// barrierProvider answers explore subagent requests only once n of them are
// in flight at the same time (or after a timeout), so a test can tell
// whether they ran concurrently.
type barrierProvider struct {
	fakeProvider
	n        int32
	inFlight atomic.Int32
	reached  atomic.Bool
	release  chan struct{}
	once     sync.Once
}

func (b *barrierProvider) StreamChat(ctx context.Context, msgs []llm.Message, defs []llm.Tool, ch chan<- string) ([]llm.ToolCall, string, error) {
	if len(msgs) > 0 && msgs[0].Content == explorePrompt {
		if b.inFlight.Add(1) == b.n {
			b.reached.Store(true)
			b.once.Do(func() { close(b.release) })
		}
		select {
		case <-b.release:
		case <-time.After(2 * time.Second):
		}
		ch <- "answer for " + msgs[1].Content
		close(ch)
		return nil, "", nil
	}
	return b.fakeProvider.StreamChat(ctx, msgs, defs, ch)
}

func TestReadOnlyCallsRunInParallelAndKeepOrder(t *testing.T) {
	q := func(id string) llm.ToolCall {
		return llm.ToolCall{ID: id, Name: "explore", Args: map[string]any{"question": "q" + id}}
	}
	p := &barrierProvider{n: 3, release: make(chan struct{}), fakeProvider: fakeProvider{replies: []reply{
		{calls: []llm.ToolCall{q("1"), q("2"), q("3")}},
		{tokens: []string{"done"}},
	}}}
	a := New(Options{Provider: p, WorkspaceRoot: t.TempDir()})
	start := time.Now()
	evs, msgs := collect(t, a.Run(context.Background(), user("look around")), nil)
	done(t, evs)
	if !p.reached.Load() {
		t.Fatalf("the 3 explore calls did not run concurrently (took %v)", time.Since(start))
	}
	assertAllCallsAnswered(t, msgs)
	for i, id := range []string{"1", "2", "3"} {
		m := msgs[1+i]
		if m.ToolCallID != id || !strings.HasPrefix(m.Content, "answer for q"+id) {
			t.Errorf("result %d = %s %q; want results in call order", i, m.ToolCallID, m.Content)
		}
	}
}

func TestWritesAndCommandsNeverRunInParallel(t *testing.T) {
	tr := &turn{a: New(Options{WorkspaceRoot: "/ws"})}
	for _, c := range []llm.ToolCall{
		{Name: "write_file", Args: map[string]any{"path": "/ws/a"}},
		{Name: "run_command", Args: map[string]any{"command": "ls"}},
		{Name: "web_fetch", Args: map[string]any{"url": "https://x"}},
		{Name: "read_file", Args: map[string]any{"path": "/etc/passwd"}}, // asks: outside the workspace
		{Name: "ask_user", Args: map[string]any{"question": "?"}},
		{Name: "update_plan"},
		{Name: "mcp__fs__read"},
	} {
		if tr.parallelSafe(c) {
			t.Errorf("%s %v must run alone", c.Name, c.Args)
		}
	}
	for _, c := range []llm.ToolCall{
		{Name: "read_file", Args: map[string]any{"path": "/ws/a.go"}},
		{Name: "search_regex", Args: map[string]any{"pattern": "x", "dir": "/ws"}},
		{Name: "web_search", Args: map[string]any{"query": "go"}},
		{Name: "explore", Args: map[string]any{"question": "?"}},
	} {
		if !tr.parallelSafe(c) {
			t.Errorf("%s %v should be parallel-safe", c.Name, c.Args)
		}
	}
}

// ── background processes ─────────────────────────────────────────────────────

func TestBackgroundProcessLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	cmd := fmt.Sprintf("echo $$ > %s; echo starting; sleep 2.5; echo compiling; sleep 0.3; echo server ready on :8080; sleep 60", pidFile)
	p := &fakeProvider{replies: []reply{
		{calls: []llm.ToolCall{{ID: "1", Name: "run_command", Args: map[string]any{"command": cmd, "background": true}}}},
		{calls: []llm.ToolCall{{ID: "2", Name: "read_process_output", Args: map[string]any{"id": "proc-1", "wait_seconds": float64(5), "pattern": "ready"}}}},
		{calls: []llm.ToolCall{{ID: "3", Name: "read_process_output", Args: map[string]any{}}}},
		{calls: []llm.ToolCall{{ID: "4", Name: "kill_process", Args: map[string]any{"id": "proc-1"}}}},
		{tokens: []string{"done"}},
	}}
	a := New(Options{Provider: p, WorkspaceRoot: dir})
	a.SetMode(ModeAuto)
	start := time.Now()
	evs, msgs := collect(t, a.Run(context.Background(), user("start the server")), nil)
	done(t, evs)
	if time.Since(start) > 15*time.Second {
		t.Fatalf("background command blocked the turn (%v)", time.Since(start))
	}
	results := map[string]string{}
	for _, m := range msgs {
		if m.Role == llm.RoleTool {
			results[m.ToolCallID] = m.Content
		}
	}
	if !strings.Contains(results["1"], "Started proc-1") || !strings.Contains(results["1"], "starting") {
		t.Errorf("start result = %q", results["1"])
	}
	if !strings.Contains(results["2"], "server ready on :8080") || strings.Contains(results["2"], "starting") {
		t.Errorf("read result = %q; want only the new matching line", results["2"])
	}
	if !strings.Contains(results["3"], "proc-1") || !strings.Contains(results["3"], "running") {
		t.Errorf("list result = %q", results["3"])
	}
	if !strings.Contains(results["4"], "Stopped proc-1") {
		t.Errorf("kill result = %q", results["4"])
	}
	if a.RunningProcesses() != 0 {
		t.Error("process still running after kill_process")
	}
	// The tools appear once a process exists.
	last := p.toolSets[len(p.toolSets)-1]
	if !strings.Contains(strings.Join(last, ","), "read_process_output") {
		t.Errorf("process tools not offered: %v", last)
	}
}

func TestCloseStopsBackgroundProcesses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell")
	}
	p := &fakeProvider{replies: []reply{
		{calls: []llm.ToolCall{{ID: "1", Name: "run_command", Args: map[string]any{"command": "sleep 60", "background": true}}}},
		{tokens: []string{"ok"}},
	}}
	a := New(Options{Provider: p, WorkspaceRoot: t.TempDir()})
	a.SetMode(ModeAuto)
	evs, _ := collect(t, a.Run(context.Background(), user("go")), nil)
	done(t, evs)
	if a.RunningProcesses() != 1 {
		t.Fatalf("running = %d, want 1", a.RunningProcesses())
	}
	a.Close()
	if a.RunningProcesses() != 0 {
		t.Fatal("Close left a process running")
	}
}

func TestBackgroundCommandStillNeedsApproval(t *testing.T) {
	p := &fakeProvider{replies: []reply{
		{calls: []llm.ToolCall{{ID: "1", Name: "run_command", Args: map[string]any{"command": "sleep 60", "background": true}}}},
		{tokens: []string{"ok"}},
	}}
	a := New(Options{Provider: p, WorkspaceRoot: t.TempDir()})
	var asked *ApprovalRequest
	evs, _ := collect(t, a.Run(context.Background(), user("go")), func(r *ApprovalRequest) { asked = r; r.Deny() })
	done(t, evs)
	if asked == nil || asked.Kind != ApproveCommand || !strings.Contains(asked.Title, "background") {
		t.Fatalf("approval = %+v", asked)
	}
	if a.RunningProcesses() != 0 {
		t.Error("a denied background command must not start")
	}
}

// ── compaction ───────────────────────────────────────────────────────────────

func TestLimitsCapOnLargeWindows(t *testing.T) {
	_, compact, keep := limits(1_000_000, 200_000)
	if compact != 200_000 || keep != 87_500 {
		t.Errorf("1M window, 200k cap: compact=%d keep=%d", compact, keep)
	}
	_, compact, keep = limits(128_000, 200_000)
	if compact != 102_400 || keep != 44_800 {
		t.Errorf("128k window (cap above it): compact=%d keep=%d", compact, keep)
	}
	_, compact, _ = limits(1_000_000, 0)
	if compact != 800_000 {
		t.Errorf("no cap: compact=%d", compact)
	}
}

// turnSummaryProvider answers tool-less requests with a summary.
type turnSummaryProvider struct {
	fakeProvider
	summaryPrompts []string
}

func (s *turnSummaryProvider) StreamChat(ctx context.Context, msgs []llm.Message, defs []llm.Tool, ch chan<- string) ([]llm.ToolCall, string, error) {
	if len(defs) == 0 {
		s.mu.Lock()
		s.summaryPrompts = append(s.summaryPrompts, msgs[len(msgs)-1].Content)
		s.mu.Unlock()
		ch <- "PROGRESS: read big.txt several times; it is plain text."
		close(ch)
		return nil, "", nil
	}
	return s.fakeProvider.StreamChat(ctx, msgs, defs, ch)
}

func TestSingleLongTurnIsSummarizedWithExactRecordAndPlan(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell")
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "big.txt")
	os.WriteFile(f, []byte(strings.Repeat("line of text\n", 500)), 0o644) // ~6.5 KB
	replies := []reply{
		{calls: []llm.ToolCall{planCall("p", "read the file=in_progress", "report=pending")}},
		{calls: []llm.ToolCall{{ID: "c", Name: "run_command", Args: map[string]any{"command": "echo hello"}}}},
		{calls: []llm.ToolCall{{ID: "w", Name: "write_file", Args: map[string]any{"path": filepath.Join(dir, "notes.md"), "content": "x"}}}},
	}
	for i := 0; i < 8; i++ {
		replies = append(replies, reply{calls: []llm.ToolCall{{ID: fmt.Sprint(i), Name: "read_file", Args: map[string]any{"path": f, "start_line": float64(i + 1)}}}})
	}
	replies = append(replies, reply{tokens: []string{"done"}})
	p := &turnSummaryProvider{fakeProvider: fakeProvider{replies: replies}}
	a := New(Options{Provider: p, WorkspaceRoot: dir, MaxContextTokens: 8000})
	a.SetMode(ModeAuto)
	evs, _ := collect(t, a.Run(context.Background(), user("read it many times")), nil)
	if d := done(t, evs); d.Err != nil {
		t.Fatal(d.Err)
	}
	if len(p.summaryPrompts) == 0 || !strings.Contains(p.summaryPrompts[0], "<request>\nread it many times") {
		t.Fatalf("no in-turn summary request: %v", p.summaryPrompts)
	}

	var hist *HistoryEvent
	for _, ev := range evs {
		if e, ok := ev.(HistoryEvent); ok {
			hist = &e
		}
	}
	if hist == nil {
		t.Fatal("no HistoryEvent")
	}
	h := hist.History
	if h[1].Role != llm.RoleUser || h[1].Content != "read it many times" {
		t.Fatalf("the user's request must stay verbatim: %+v", h[1])
	}
	note := h[2]
	if note.Role != llm.RoleSystem || !strings.HasPrefix(note.Content, TurnCompactedPrefix) || !strings.Contains(note.Content, "PROGRESS:") {
		t.Fatalf("progress note = %+v", note)
	}
	for _, want := range []string{recordHeading, "- edited: " + filepath.Join(dir, "notes.md"), "- ran: `echo hello` → exit 0", "## Current plan", "- [>] read the file"} {
		if !strings.Contains(note.Content, want) {
			t.Errorf("progress note lacks %q:\n%s", want, note.Content)
		}
	}
	if h[3].Role != llm.RoleAssistant {
		t.Errorf("the kept rounds must start with an assistant message, got %s", h[3].Role)
	}
	// The plan is rebuilt from the summary once its update_plan call is gone.
	if got := PlanFromHistory(h); len(got) != 2 || got[0].Status != PlanInProgress {
		t.Errorf("plan from compacted history = %+v", got)
	}
	for i, r := range p.requests {
		if est := llm.EstimateHistoryTokens(r); est > 7000 {
			t.Errorf("request %d is ~%d tokens: over the window", i, est)
		}
	}
}

func TestWorkRecordCarriesEarlierRecord(t *testing.T) {
	prev := CompactedPrefix + "\nsummary\n\n" + recordHeading + "\n- edited: a.go\n- ran: `make` → exit 2\n\n## Current plan (update_plan, 0/1 done)\n- [ ] x\n"
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: prev},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "1", Name: "edit_file", Args: map[string]any{"path": "b.go"}},
			{ID: "2", Name: "edit_file", Args: map[string]any{"path": "c.go"}},
			{ID: "3", Name: "run_command", Args: map[string]any{"command": "go test ./...\n"}},
			{ID: "4", Name: "run_command", Args: map[string]any{"command": "rm x"}},
		}},
		{Role: llm.RoleTool, ToolCallID: "1", Content: "Successfully wrote b.go"},
		{Role: llm.RoleTool, ToolCallID: "2", Content: "The user rejected this file change."},
		{Role: llm.RoleTool, ToolCallID: "3", Content: "exit_code: 0\n--- stdout ---\nok"},
		{Role: llm.RoleTool, ToolCallID: "4", Content: "Command skipped by user."},
	}
	rec := workRecord(msgs)
	for _, want := range []string{"- edited: a.go", "- edited: b.go", "- ran: `make` → exit 2", "- ran: `go test ./...` → exit 0", "- ran: `rm x` → not run"} {
		if !strings.Contains(rec, want) {
			t.Errorf("record lacks %q:\n%s", want, rec)
		}
	}
	if strings.Contains(rec, "c.go") {
		t.Errorf("a rejected edit is not a change:\n%s", rec)
	}
}

func TestCompactionUsesSummaryModel(t *testing.T) {
	main := &fakeProvider{replies: []reply{{tokens: []string{"final answer"}}}}
	cheap := &fakeProvider{replies: []reply{{tokens: []string{"SUMMARY from the cheap model"}}}}
	a := New(Options{Provider: main, MaxContextTokens: 2000})
	a.SetCompactProvider(cheap)
	evs, _ := collect(t, a.Run(context.Background(), longHistory(4, 1200)), nil)
	if d := done(t, evs); d.Err != nil {
		t.Fatal(d.Err)
	}
	if len(cheap.requests) != 1 || len(main.requests) != 1 {
		t.Fatalf("requests: cheap=%d main=%d; want the summary on the cheap model", len(cheap.requests), len(main.requests))
	}
	if !strings.Contains(main.requests[0][1].Content, "SUMMARY from the cheap model") {
		t.Errorf("main request = %+v", main.requests[0][:2])
	}
}
