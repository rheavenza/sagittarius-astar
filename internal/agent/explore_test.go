package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
	"github.com/clitorhea/sagittarius-astar.git/internal/summary"
)

func exploreCall(q string) llm.ToolCall {
	return llm.ToolCall{ID: "e1", Name: "explore", Args: map[string]any{"question": q}}
}

func TestExploreReturnsSubagentAnswer(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "auth.go")
	os.WriteFile(f, []byte("package x\n\nfunc Login() {}\n"), 0o644)

	p := &fakeProvider{replies: []reply{
		{calls: []llm.ToolCall{exploreCall("where is login?")}},                                 // main
		{calls: []llm.ToolCall{{ID: "s1", Name: "read_file", Args: map[string]any{"path": f}}}}, // sub
		{tokens: []string{"Login is at auth.go:3."}},                                            // sub answer
		{tokens: []string{"It's in auth.go."}},                                                  // main answer
	}}
	a := New(Options{Provider: p, WorkspaceRoot: dir, ContextBlock: "# Environment\n- x"})
	evs, msgs := collect(t, a.Run(context.Background(), user("find login")), func(r *ApprovalRequest) {
		t.Fatalf("explore must not ask: %+v", r)
	})
	if d := done(t, evs); d.Err != nil {
		t.Fatal(d.Err)
	}
	assertAllCallsAnswered(t, msgs)

	// Main history: explore call, its result, final answer. The subagent's
	// own read_file call and output stay out of it.
	if len(msgs) != 3 || msgs[1].ToolCallID != "e1" {
		t.Fatalf("main history = %+v", msgs)
	}
	if !strings.Contains(msgs[1].Content, "Login is at auth.go:3.") || !strings.Contains(msgs[1].Content, "1 tool calls") {
		t.Errorf("explore result = %q", msgs[1].Content)
	}
	if strings.Contains(msgs[1].Content, "func Login") {
		t.Error("raw tool output leaked into the main context")
	}

	// The subagent got a fresh context: its own system prompt (+ context
	// block) and the question, nothing from the main conversation.
	sub := p.requests[1]
	if len(sub) != 2 || !strings.Contains(sub[0].Content, "exploration subagent") ||
		!strings.Contains(sub[0].Content, "# Environment") || sub[1].Content != "where is login?" {
		t.Errorf("subagent request = %+v", sub)
	}
	// And only read-only tools, without explore.
	for _, name := range p.toolSets[1] {
		if name == "explore" || name == "write_file" || name == "run_command" || name == "web_fetch" {
			t.Errorf("subagent was offered %s", name)
		}
	}
	if !slices.Contains(p.toolSets[1], "read_file") || !slices.Contains(p.toolSets[0], "explore") {
		t.Errorf("tool sets = %v", p.toolSets)
	}

	// Progress is reported as notices.
	var notes []string
	for _, ev := range evs {
		if n, ok := ev.(NoticeEvent); ok {
			notes = append(notes, n.Text)
		}
	}
	if !slices.ContainsFunc(notes, func(s string) bool { return strings.Contains(s, "explore: read_file "+f) }) {
		t.Errorf("notices = %v", notes)
	}
}

func TestExploreSubagentCannotWriteOrAsk(t *testing.T) {
	dir := t.TempDir()
	p := &fakeProvider{replies: []reply{
		{calls: []llm.ToolCall{exploreCall("q")}},
		{calls: []llm.ToolCall{
			{ID: "s1", Name: "write_file", Args: map[string]any{"path": filepath.Join(dir, "x"), "content": "y"}},
			{ID: "s2", Name: "read_file", Args: map[string]any{"path": "/etc/hostname"}},
			{ID: "s3", Name: "explore", Args: map[string]any{"question": "recurse"}},
		}},
		{tokens: []string{"nothing"}},
		{tokens: []string{"done"}},
	}}
	a := New(Options{Provider: p, WorkspaceRoot: dir})
	a.SetMode(ModeAuto) // even auto mode must not let the subagent write
	evs, _ := collect(t, a.Run(context.Background(), user("go")), func(r *ApprovalRequest) {
		t.Fatalf("subagent reached the user: %+v", r)
	})
	done(t, evs)

	if _, err := os.Stat(filepath.Join(dir, "x")); err == nil {
		t.Fatal("subagent wrote a file")
	}
	results := p.requests[2] // sub's second request carries the three results
	var got []string
	for _, m := range results {
		if m.Role == llm.RoleTool {
			got = append(got, m.Content)
		}
	}
	if len(got) != 3 ||
		!strings.Contains(got[0], "only use local read-only tools") ||
		!strings.Contains(got[1], "cannot ask the user") ||
		!strings.Contains(got[2], "only use local read-only tools") {
		t.Errorf("sub tool results = %q", got)
	}
}

func TestExploreForcesAnswerAtRoundLimit(t *testing.T) {
	dir := t.TempDir()
	replies := []reply{{calls: []llm.ToolCall{exploreCall("q")}}}
	for i := 0; i < exploreMaxRounds; i++ {
		replies = append(replies, reply{calls: []llm.ToolCall{{
			ID: fmt.Sprintf("s%d", i), Name: "find_files", Args: map[string]any{"pattern": fmt.Sprintf("*.x%d", i), "dir": dir},
		}}})
	}
	replies = append(replies, reply{tokens: []string{"partial answer"}}, reply{tokens: []string{"ok"}})
	p := &fakeProvider{replies: replies}
	a := New(Options{Provider: p, WorkspaceRoot: dir})
	evs, msgs := collect(t, a.Run(context.Background(), user("go")), nil)
	done(t, evs)

	last := p.requests[exploreMaxRounds+1]
	if n := len(p.toolSets[exploreMaxRounds+1]); n != 0 {
		t.Errorf("final sub request still offered %d tools", n)
	}
	if !strings.Contains(last[len(last)-1].Content, "exploration limit") {
		t.Errorf("limit message missing: %+v", last[len(last)-1])
	}
	if !strings.Contains(msgs[1].Content, "partial answer") || !strings.Contains(msgs[1].Content, fmt.Sprintf("%d tool calls in %d rounds", exploreMaxRounds, exploreMaxRounds)) {
		t.Errorf("explore result = %q", msgs[1].Content)
	}
}

func TestExploreAllowedInPlanMode(t *testing.T) {
	p := &fakeProvider{replies: []reply{
		{calls: []llm.ToolCall{exploreCall("q")}},
		{tokens: []string{"sub answer"}},
		{tokens: []string{"plan"}},
	}}
	a := New(Options{Provider: p})
	a.SetPlanMode(true)
	evs, msgs := collect(t, a.Run(context.Background(), user("plan it")), nil)
	done(t, evs)
	if !slices.Contains(p.toolSets[0], "explore") || !strings.Contains(msgs[1].Content, "sub answer") {
		t.Errorf("tools=%v result=%q", p.toolSets[0], msgs[1].Content)
	}
	// The plan directive is for the main agent, not the subagent.
	if strings.Contains(p.requests[1][0].Content, "Plan mode") {
		t.Error("plan directive leaked into the subagent prompt")
	}
}

func TestExploreCancel(t *testing.T) {
	p := &fakeProvider{replies: []reply{
		{calls: []llm.ToolCall{exploreCall("q")}},
		{block: true}, // sub blocks until cancelled
	}}
	a := New(Options{Provider: p})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := a.Run(ctx, user("go"))
	var msgs []llm.Message
	var last Event
	for ev := range events {
		last = ev
		switch e := ev.(type) {
		case ToolStartEvent:
			if e.Call.Name == "explore" {
				cancel()
			}
		case MessageEvent:
			msgs = append(msgs, e.Msg)
		}
	}
	if d, ok := last.(DoneEvent); !ok || !d.Aborted {
		t.Fatalf("last event = %#v", last)
	}
	assertAllCallsAnswered(t, msgs)
}

func TestProjectOverviewOfferedOnlyWithIndex(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n"), 0o644)

	offered := func() bool {
		p := &fakeProvider{replies: []reply{{tokens: []string{"ok"}}}}
		a := New(Options{Provider: p, WorkspaceRoot: root})
		collect(t, a.Run(context.Background(), user("hi")), nil)
		return slices.Contains(p.toolSets[0], "project_overview")
	}
	if offered() {
		t.Fatal("offered without an index")
	}
	plan, err := summary.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Run(context.Background(), &fakeProvider{replies: []reply{{tokens: []string{"file"}}, {tokens: []string{"project"}}}}, "m", nil); err != nil {
		t.Fatal(err)
	}
	if !offered() {
		t.Error("not offered once the index exists")
	}
}
