package tui

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/clitorhea/sagittarius-astar.git/internal/agent"
	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
	"github.com/clitorhea/sagittarius-astar.git/internal/session"
)

// ── fake provider ────────────────────────────────────────────────────────────

type scripted struct {
	tokens []string
	calls  []llm.ToolCall
}

type fakeProvider struct {
	mu      sync.Mutex
	replies []scripted
}

func (f *fakeProvider) StreamChat(ctx context.Context, msgs []llm.Message, defs []llm.Tool, ch chan<- string) ([]llm.ToolCall, string, error) {
	defer close(ch)
	// Session-title requests (no tools) get a fixed answer.
	if len(defs) == 0 {
		ch <- "Test Title"
		return nil, "", nil
	}
	f.mu.Lock()
	if len(f.replies) == 0 {
		f.mu.Unlock()
		ch <- "(no more replies)"
		return nil, "", nil
	}
	r := f.replies[0]
	f.replies = f.replies[1:]
	f.mu.Unlock()
	for _, t := range r.tokens {
		select {
		case ch <- t:
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}
	return r.calls, "", nil
}

// ── driver ───────────────────────────────────────────────────────────────────

// driver runs a Model like Bubble Tea does: it executes commands and feeds
// the resulting messages back into Update. Spinner ticks are dropped.
type driver struct {
	t     *testing.T
	m     Model
	queue []tea.Cmd
}

func newDriver(t *testing.T, p llm.Provider) *driver {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for _, k := range []string{"GEMINI_API_KEY", "DEEPSEEK_API_KEY", "NVIDIA_API_KEY", "NIM_BASE_URL"} {
		t.Setenv(k, "")
	}
	root := t.TempDir()
	t.Chdir(root)
	ag := agent.New(agent.Options{Provider: p, WorkspaceRoot: root})
	sess := &session.Session{ID: "test-session", Name: "Untitled", History: []llm.Message{{Role: llm.RoleSystem, Content: "sys"}}}
	mp, err := NewModel(ag, sess, "sys", "test", "gemini", "test-model", "coder")
	if err != nil {
		t.Fatal(err)
	}
	d := &driver{t: t, m: *mp}
	d.send(tea.WindowSizeMsg{Width: 100, Height: 40})
	return d
}

func (d *driver) send(msg tea.Msg) {
	nm, cmd := d.m.Update(msg)
	d.m = nm.(Model)
	d.queue = append(d.queue, cmd)
}

func (d *driver) key(k tea.KeyType) { d.send(tea.KeyMsg{Type: k}) }

func (d *driver) submit(text string) {
	d.m.textarea.SetValue(text)
	d.key(tea.KeyEnter)
}

// runUntil processes queued commands until pred holds.
func (d *driver) runUntil(what string, pred func(Model) bool) {
	d.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !pred(d.m) {
		if time.Now().After(deadline) || len(d.queue) == 0 {
			d.t.Fatalf("never reached: %s (state=%d, queue=%d)", what, d.m.appState, len(d.queue))
		}
		c := d.queue[0]
		d.queue = d.queue[1:]
		if c == nil {
			continue
		}
		switch msg := c().(type) {
		case nil, spinner.TickMsg:
		case tea.BatchMsg:
			d.queue = append(d.queue, msg...)
		default:
			d.send(msg)
		}
	}
}

func idle(m Model) bool { return m.appState == stateInput && m.events == nil }

var reANSI = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// output returns the rendered transcript without ANSI styling.
func output(m Model) string { return reANSI.ReplaceAllString(strings.Join(m.outputLines, "\n"), "") }

func assertAnswered(t *testing.T, h []llm.Message) {
	t.Helper()
	answered := map[string]bool{}
	for _, m := range h {
		if m.Role == llm.RoleTool {
			answered[m.ToolCallID] = true
		}
	}
	for _, m := range h {
		for _, c := range m.ToolCalls {
			if !answered[c.ID] {
				t.Errorf("tool call %s has no result", c.ID)
			}
		}
	}
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestTUITextTurn(t *testing.T) {
	d := newDriver(t, &fakeProvider{replies: []scripted{{tokens: []string{"Hel", "lo ", "there"}}}})
	d.submit("hi")
	if d.m.appState != stateStreaming {
		t.Fatalf("state after submit = %d", d.m.appState)
	}
	d.runUntil("turn done", idle)

	h := d.m.history
	if len(h) != 3 || h[2].Role != llm.RoleAssistant || h[2].Content != "Hello there" {
		t.Fatalf("history = %+v", h)
	}
	if !strings.Contains(output(d.m), "Hello there") {
		t.Errorf("answer not rendered:\n%s", output(d.m))
	}
	// Session saved with the turn.
	saved, err := session.Load("test-session")
	if err != nil || len(saved.History) != 3 {
		t.Fatalf("session not saved: %v %+v", err, saved)
	}
}

func TestTUICommandApprovalWithEdit(t *testing.T) {
	d := newDriver(t, &fakeProvider{replies: []scripted{
		{calls: []llm.ToolCall{{ID: "c1", Name: "run_command", Args: map[string]any{"command": "echo one"}}}},
		{tokens: []string{"ran it"}},
	}})
	d.submit("run something")
	d.runUntil("command approval", func(m Model) bool { return m.appState == stateConfirmingCmd })

	if d.m.cmdTextarea.Value() != "echo one" {
		t.Fatalf("command not prefilled: %q", d.m.cmdTextarea.Value())
	}
	d.m.cmdTextarea.SetValue("echo edited")
	d.key(tea.KeyEnter)
	d.runUntil("turn done", idle)

	h := d.m.history
	assertAnswered(t, h)
	if !strings.Contains(h[3].Content, "edited") {
		t.Errorf("edited command not run: %q", h[3].Content)
	}
	if !strings.Contains(output(d.m), "✓ Tool run_command done") {
		t.Errorf("result not shown:\n%s", output(d.m))
	}
}

func TestTUIRejectWriteWithEsc(t *testing.T) {
	d := newDriver(t, nil)
	target := filepath.Join(d.m.agentRootForTest(), "new.txt")
	d.m.agent.SetProvider(&fakeProvider{replies: []scripted{
		{calls: []llm.ToolCall{{ID: "w1", Name: "write_file", Args: map[string]any{"path": target, "content": "data"}}}},
		{tokens: []string{"ok, not writing"}},
	}})
	d.submit("write a file")
	d.runUntil("write approval", func(m Model) bool { return m.appState == stateConfirmingWrite })
	if !strings.Contains(output(d.m), "write_file: "+target) {
		t.Errorf("write preview missing:\n%s", output(d.m))
	}
	d.key(tea.KeyEsc)
	d.runUntil("turn done", idle)

	if _, err := os.Stat(target); err == nil {
		t.Fatal("rejected write was committed")
	}
	assertAnswered(t, d.m.history)
	if !strings.Contains(d.m.history[3].Content, "rejected") {
		t.Errorf("rejection not reported to model: %q", d.m.history[3].Content)
	}
}

func TestTUICtrlCDuringApprovalCancelsTurn(t *testing.T) {
	d := newDriver(t, &fakeProvider{replies: []scripted{
		{calls: []llm.ToolCall{
			{ID: "a", Name: "run_command", Args: map[string]any{"command": "echo a"}},
			{ID: "b", Name: "run_command", Args: map[string]any{"command": "echo b"}},
		}},
	}})
	d.submit("go")
	d.runUntil("command approval", func(m Model) bool { return m.appState == stateConfirmingCmd })

	// A resize while waiting must keep the approval prompt visible.
	d.send(tea.WindowSizeMsg{Width: 80, Height: 30})
	if !strings.Contains(output(d.m), "run_command: echo a") {
		t.Errorf("approval prompt lost on resize:\n%s", output(d.m))
	}

	d.key(tea.KeyCtrlC)
	d.runUntil("turn done", idle)
	assertAnswered(t, d.m.history)
	if !strings.Contains(output(d.m), "Turn cancelled") {
		t.Errorf("cancellation not shown:\n%s", output(d.m))
	}

	// The app is usable again: another prompt starts a new turn.
	d.m.agent.SetProvider(&fakeProvider{replies: []scripted{{tokens: []string{"back"}}}})
	d.submit("again")
	d.runUntil("second turn done", func(m Model) bool {
		return idle(m) && m.history[len(m.history)-1].Content == "back"
	})
}

func TestTUIGeneratesTitleAfterFirstTurn(t *testing.T) {
	d := newDriver(t, &fakeProvider{replies: []scripted{{tokens: []string{"answer"}}}})
	d.submit("hello")
	d.runUntil("title", func(m Model) bool { return m.activeSession.Name == "Test Title" })
}
func TestBuildModelItems(t *testing.T) {
	known := [][2]string{{"meta/llama-3.3-70b-instruct", "Llama 3.3 70B"}}
	live := []string{"meta/llama-3.3-70b-instruct", "moonshotai/kimi-k3", "google/gemma-4-31b-it"}

	all := buildModelItems(known, live, "", "moonshotai/kimi-k3")
	if len(all) != 3 || all[0].ID != "meta/llama-3.3-70b-instruct" {
		t.Fatalf("expected known first and deduplicated: %+v", all)
	}
	if !strings.Contains(all[1].Label, "(active)") {
		t.Errorf("active model not marked: %+v", all[1])
	}

	filtered := buildModelItems(known, live, "GEMMA", "")
	if len(filtered) != 1 || filtered[0].ID != "google/gemma-4-31b-it" {
		t.Fatalf("filter failed: %+v", filtered)
	}
}

func TestCleanTitle(t *testing.T) {
	cases := []struct{ raw, query, want string }{
		{`"Debugging Make Test"`, "q", "Debugging Make Test"},
		{"<think>hmm</think>Fix Login Bug\nextra line", "q", "Fix Login Bug"},
		{"", "why does make test fail on my machine today", "why does make test fail on …"},
		{"   ", "", "Untitled"},
	}
	for _, c := range cases {
		if got := cleanTitle(c.raw, c.query); got != c.want {
			t.Errorf("cleanTitle(%q, %q) = %q, want %q", c.raw, c.query, got, c.want)
		}
	}
}

// slowTitleProvider fails title requests (like a timeout) but answers turns.
type slowTitleProvider struct{ fakeProvider }

func (s *slowTitleProvider) StreamChat(ctx context.Context, msgs []llm.Message, defs []llm.Tool, ch chan<- string) ([]llm.ToolCall, string, error) {
	if len(defs) == 0 {
		close(ch)
		return nil, "", context.DeadlineExceeded
	}
	return s.fakeProvider.StreamChat(ctx, msgs, defs, ch)
}

func TestTUITitleFallsBackWhenGenerationFails(t *testing.T) {
	p := &slowTitleProvider{fakeProvider{replies: []scripted{{tokens: []string{"answer"}}}}}
	d := newDriver(t, p)
	d.submit("explain the agent package please")
	d.runUntil("fallback title", func(m Model) bool { return m.activeSession.Name == "explain the agent package please" })
}
