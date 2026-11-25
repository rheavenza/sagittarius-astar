package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/clitorhea/sagittarius-astar.git/internal/agent"
	"github.com/clitorhea/sagittarius-astar.git/internal/checkpoint"
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
	cp := checkpoint.New(root, "test-session")
	ag := agent.New(agent.Options{Provider: p, WorkspaceRoot: root, Checkpoints: cp})
	sess := &session.Session{ID: "test-session", Name: "Untitled", History: []llm.Message{{Role: llm.RoleSystem, Content: "sys"}}}
	mp, err := NewModel(ag, sess, "sys", "test", "gemini", "test-model", "coder")
	if err != nil {
		t.Fatal(err)
	}
	mp.SetCheckpoints(cp)
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

// ── safety net ───────────────────────────────────────────────────────────────

func TestTUIUndoRedoAgentWrite(t *testing.T) {
	d := newDriver(t, nil)
	target := filepath.Join(d.m.agentRootForTest(), "made-by-agent.txt")
	d.m.agent.SetProvider(&fakeProvider{replies: []scripted{
		{calls: []llm.ToolCall{{ID: "w1", Name: "write_file", Args: map[string]any{"path": target, "content": "hello"}}}},
		{tokens: []string{"written"}},
	}})
	d.submit("create the file")
	d.runUntil("write approval", func(m Model) bool { return m.appState == stateConfirmingWrite })
	d.key(tea.KeyEnter) // prefilled "y"
	d.runUntil("turn done", idle)
	if read, _ := os.ReadFile(target); string(read) != "hello" {
		t.Fatal("approved write not committed")
	}
	if !strings.Contains(output(d.m), "Checkpoint saved") {
		t.Errorf("checkpoint notice missing:\n%s", output(d.m))
	}

	d.submit("/undo")
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("/undo did not remove the file the agent created")
	}
	last := d.m.history[len(d.m.history)-1]
	if last.Role != llm.RoleSystem || !strings.Contains(last.Content, "/undo") {
		t.Errorf("model not told about the undo: %+v", last)
	}

	d.submit("/redo")
	if read, _ := os.ReadFile(target); string(read) != "hello" {
		t.Fatal("/redo did not restore the file")
	}

	d.submit("/checkpoints")
	if !strings.Contains(output(d.m), "create the file") {
		t.Errorf("checkpoint list missing the turn:\n%s", output(d.m))
	}
	d.submit("/diff")
	if !strings.Contains(output(d.m), "made-by-agent.txt") {
		t.Errorf("diff missing the file:\n%s", output(d.m))
	}
}

func TestTUIPlanModeThenGo(t *testing.T) {
	d := newDriver(t, nil)
	target := filepath.Join(d.m.agentRootForTest(), "x.txt")
	write := llm.ToolCall{ID: "w", Name: "write_file", Args: map[string]any{"path": target, "content": "x"}}
	d.m.agent.SetProvider(&fakeProvider{replies: []scripted{
		{calls: []llm.ToolCall{write}},
		{tokens: []string{"1. create x.txt"}},
		{calls: []llm.ToolCall{func() llm.ToolCall { w := write; w.ID = "w2"; return w }()}},
		{tokens: []string{"done"}},
	}})

	d.submit("/plan add x.txt")
	if !d.m.agent.PlanMode() {
		t.Fatal("plan mode not enabled")
	}
	d.runUntil("plan turn done", idle)
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("plan mode allowed a write")
	}
	if !strings.Contains(output(d.m), "/go to implement") {
		t.Errorf("plan hint missing:\n%s", output(d.m))
	}

	d.submit("/go")
	if d.m.agent.PlanMode() {
		t.Fatal("/go must leave plan mode")
	}
	d.runUntil("write approval after /go", func(m Model) bool { return m.appState == stateConfirmingWrite })
	d.key(tea.KeyEnter)
	d.runUntil("implementation done", idle)
	if _, err := os.Stat(target); err != nil {
		t.Fatal("approved implementation did not write the file")
	}
	found := false
	for _, m := range d.m.history {
		if m.Role == llm.RoleUser && m.Content == planApprovedPrompt {
			found = true
		}
	}
	if !found {
		t.Error("approval prompt not sent")
	}
}

func TestTUIGoOutsidePlanMode(t *testing.T) {
	d := newDriver(t, &fakeProvider{})
	d.submit("/go")
	if d.m.appState != stateInput || !strings.Contains(output(d.m), "Not in plan mode") {
		t.Fatalf("unexpected: state=%d\n%s", d.m.appState, output(d.m))
	}
}

// ── layout ───────────────────────────────────────────────────────────────────

func frameLines(m Model) []string { return strings.Split(m.View(), "\n") }

func TestFrameAlwaysFitsTerminal(t *testing.T) {
	d := newDriver(t, &fakeProvider{replies: []scripted{
		{calls: []llm.ToolCall{{ID: "c", Name: "run_command", Args: map[string]any{"command": "echo hi"}}}},
		{tokens: []string{"ok"}},
	}})
	d.m.activeProvider = "nim"
	d.m.activeModel = "mistralai/mistral-large-4-1t-a52b-instruct-very-long-name"
	d.m.activeSession.Name = "A really long session title that goes on"
	for i := 0; i < 80; i++ {
		d.m.appendOutput(fmt.Sprintf("line %d", i))
	}
	d.m.refreshViewport()

	check := func(label string) {
		t.Helper()
		for _, size := range [][2]int{{120, 40}, {80, 24}, {50, 20}, {40, 12}} {
			d.send(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			lines := frameLines(d.m)
			if len(lines) != size[1] {
				t.Errorf("%s @%dx%d: frame has %d lines", label, size[0], size[1], len(lines))
			}
			for i, l := range lines {
				if w := lipgloss.Width(l); w > size[0] {
					t.Errorf("%s @%dx%d: line %d is %d wide", label, size[0], size[1], i, w)
					break
				}
			}
			if h := lipgloss.Height(d.m.statusBar()); h != 1 {
				t.Errorf("%s @%dx%d: status bar is %d lines", label, size[0], size[1], h)
			}
		}
	}
	check("input")
	d.submit("run it")
	d.runUntil("approval", func(m Model) bool { return m.appState == stateConfirmingCmd })
	check("approval")
	d.key(tea.KeyEnter)
	d.runUntil("done", idle)
	d.submit("/history")
	check("selecting")
}

func TestScrollbarAndStickyScroll(t *testing.T) {
	d := newDriver(t, &fakeProvider{})
	for i := 0; i < 200; i++ {
		d.m.appendOutput(fmt.Sprintf("line %d", i))
	}
	d.m.refreshViewport()
	if !d.m.viewport.AtBottom() {
		t.Fatal("should start at the bottom")
	}
	bar := scrollbarView(d.m.viewport)
	if !strings.Contains(bar, "┃") || !strings.Contains(bar, "│") {
		t.Fatalf("scrollbar missing thumb/track:\n%s", bar)
	}

	d.key(tea.KeyPgUp)
	off := d.m.viewport.YOffset
	d.m.appendOutput("new output while scrolled up")
	d.m.refreshViewport()
	if d.m.viewport.YOffset != off {
		t.Errorf("view jumped while the user was reading (offset %d -> %d)", off, d.m.viewport.YOffset)
	}
	if !strings.Contains(d.m.statusBar(), "more below") {
		t.Error("status bar should hint at more content below")
	}
	d.send(tea.KeyMsg{Type: tea.KeyCtrlEnd})
	if !d.m.viewport.AtBottom() {
		t.Error("Ctrl+End should jump to the bottom")
	}
}

func TestShiftTabCyclesModes(t *testing.T) {
	d := newDriver(t, &fakeProvider{})
	want := []string{"accept edits", "auto", "plan", "ask"}
	for _, w := range want {
		d.key(tea.KeyShiftTab)
		if !strings.Contains(reANSI.ReplaceAllString(d.m.modeBadge(), ""), w) {
			t.Fatalf("after Shift+Tab badge = %q, want %q", d.m.modeBadge(), w)
		}
	}
	d.submit("/mode edits")
	if d.m.agent.Mode() != agent.ModeAcceptEdits || d.m.agent.PlanMode() {
		t.Fatal("/mode edits not applied")
	}
}

func TestTUISummarizePreviewRunAndShow(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	d := newDriver(t, &fakeProvider{})
	cwd, _ := os.Getwd()
	os.WriteFile(filepath.Join(cwd, "a.go"), []byte("package a\n"), 0o644)
	os.WriteFile(filepath.Join(cwd, "b.go"), []byte("package a\n"), 0o644)

	d.submit("/summarize")
	out := output(d.m)
	if !strings.Contains(out, "2 source files: 0 cached, 2 to summarize") || !strings.Contains(out, "/summarize go") {
		t.Fatalf("preview:\n%s", out)
	}
	if d.m.summary != nil {
		t.Fatal("preview must not start a run")
	}

	d.submit("/summarize go")
	if d.m.summary == nil {
		t.Fatal("run not started")
	}
	d.runUntil("summary done", func(m Model) bool { return strings.Contains(output(m), "Project summary written") })
	if d.m.summary != nil {
		t.Error("job not cleared")
	}

	d.submit("/summarize")
	if !strings.Contains(output(d.m), "Project summary is up to date (2 files)") {
		t.Errorf("second preview:\n%s", output(d.m))
	}
	d.submit("/summarize show")
	if !strings.Contains(output(d.m), "PROJECT_SUMMARY.md") {
		t.Errorf("show:\n%s", output(d.m))
	}
}

func TestTUICompactCommand(t *testing.T) {
	d := newDriver(t, &fakeProvider{})
	for i := 0; i < 3; i++ {
		d.m.history = append(d.m.history,
			llm.Message{Role: llm.RoleUser, Content: fmt.Sprintf("question %d", i)},
			llm.Message{Role: llm.RoleAssistant, Content: fmt.Sprintf("answer %d", i)})
	}
	d.submit("/compact")
	d.runUntil("compacted", func(m Model) bool { return strings.Contains(output(m), "✓ Compacted") })

	h := d.m.history
	if len(h) != 5 || !strings.HasPrefix(h[1].Content, agent.CompactedPrefix) || h[3].Content != "question 2" || h[4].Content != "answer 2" {
		t.Fatalf("history after /compact = %+v", h)
	}
	if !strings.Contains(output(d.m), "📦 Earlier conversation (compacted)") {
		t.Errorf("compacted summary not rendered:\n%s", output(d.m))
	}
	saved, err := session.Load("test-session")
	if err != nil || len(saved.History) != 5 {
		t.Errorf("compacted history not saved: %v", err)
	}
}

func TestStatusBarShowsContextUse(t *testing.T) {
	d := newDriver(t, &fakeProvider{})
	d.m.ctxUsed, d.m.ctxWindow, d.m.ctxReported = 92_000, 131_072, true
	bar := reANSI.ReplaceAllString(d.m.statusBar(), "")
	if !strings.Contains(bar, "ctx 70% (92k/131k)") {
		t.Errorf("status bar = %q", bar)
	}
	d.m.ctxReported = false
	if bar := reANSI.ReplaceAllString(d.m.statusBar(), ""); !strings.Contains(bar, "ctx ~70%") {
		t.Errorf("estimated usage not marked: %q", bar)
	}
}

func TestTUIMentionCompletionAndAttach(t *testing.T) {
	d := newDriver(t, &fakeProvider{replies: []scripted{{tokens: []string{"ok"}}}})
	os.MkdirAll("internal/agent", 0o755)
	os.WriteFile("internal/agent/agent.go", []byte("package agent\n"), 0o644)
	os.WriteFile("internal/agent/explore.go", []byte("package agent\n"), 0o644)
	os.WriteFile("notes.txt", []byte("remember the milk\n"), 0o644)

	typeText := func(s string) {
		for _, r := range s {
			d.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		}
	}
	typeText("explain @int")
	frame := reANSI.ReplaceAllString(d.m.View(), "")
	if !strings.Contains(frame, "@ Tab:") || !strings.Contains(frame, "internal/") {
		t.Fatalf("no suggestions shown:\n%s", frame)
	}
	if len(frameLines(d.m)) > 40 {
		t.Fatal("suggestion line pushed the frame past the terminal height")
	}
	d.key(tea.KeyTab)
	if got := d.m.textarea.Value(); got != "explain @internal/" {
		t.Fatalf("after Tab: %q", got)
	}
	typeText("e")
	d.key(tea.KeyTab)
	first := d.m.textarea.Value()
	d.key(tea.KeyTab)
	second := d.m.textarea.Value()
	if first == second || !strings.HasPrefix(first, "explain @internal/") || !strings.HasPrefix(second, "explain @internal/") {
		t.Fatalf("Tab should cycle: %q then %q", first, second)
	}

	d.m.textarea.SetValue("what is in @notes.txt?")
	d.key(tea.KeyEnter)
	d.runUntil("turn done", idle)
	var user string
	for _, m := range d.m.history {
		if m.Role == llm.RoleUser {
			user = m.Content
		}
	}
	if !strings.HasPrefix(user, "what is in @notes.txt?\n\n<file path=\"notes.txt\">\nremember the milk\n</file>") {
		t.Fatalf("attachment missing from the user message: %q", user)
	}
	if out := output(d.m); !strings.Contains(out, "📎 notes.txt (1 line)") || strings.Contains(out, "remember the milk") {
		t.Fatalf("transcript should list the attachment, not its content:\n%s", out)
	}
	if d.m.mentions.sugg != nil {
		t.Fatal("suggestions not cleared on submit")
	}

	// A reloaded session shows the attachment as a 📎 line too.
	d.m.loadHistory(d.m.history, false)
	if out := output(d.m); !strings.Contains(out, "📎 notes.txt") || strings.Contains(out, "remember the milk") {
		t.Fatalf("reloaded transcript:\n%s", out)
	}
}

func TestTUIAskUserPickOptionAndShowPlan(t *testing.T) {
	d := newDriver(t, &fakeProvider{replies: []scripted{
		{calls: []llm.ToolCall{{ID: "p1", Name: "update_plan", Args: map[string]any{"items": []any{
			map[string]any{"step": "pick a database", "status": "in_progress"},
			map[string]any{"step": "write the schema", "status": "pending"},
		}}}}},
		{calls: []llm.ToolCall{{ID: "q1", Name: "ask_user", Args: map[string]any{"question": "Which database?", "options": []any{"Postgres", "SQLite"}}}}},
		{tokens: []string{"Going with SQLite."}},
	}})
	d.submit("set up storage")
	d.runUntil("question", func(m Model) bool { return m.appState == stateAnswering })

	out := output(d.m)
	for _, want := range []string{"📋 Plan (0/2)", "▸ pick a database", "○ write the schema", "❓ Which database?", "2. SQLite"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Tool: update_plan") || strings.Contains(out, "Tool update_plan done") {
		t.Errorf("update_plan should show the plan, not tool lines:\n%s", out)
	}
	if !strings.Contains(reANSI.ReplaceAllString(d.m.statusBar(), ""), "📋 0/2") {
		t.Errorf("status bar lacks the plan progress: %q", d.m.statusBar())
	}

	d.m.cmdTextarea.SetValue("2")
	d.key(tea.KeyEnter)
	d.runUntil("turn done", idle)
	h := d.m.history
	assertAnswered(t, h)
	var answer string
	for _, m := range h {
		if m.Role == llm.RoleTool && m.ToolName == "ask_user" {
			answer = m.Content
		}
	}
	if answer != "The user answered: SQLite" {
		t.Fatalf("ask_user result = %q", answer)
	}

	// Reloading the session shows the plan again and restores the counter.
	d.m.plan = nil
	d.m.loadHistory(h, false)
	if len(d.m.plan) != 2 || !strings.Contains(output(d.m), "📋 Plan (0/2)") {
		t.Errorf("reloaded plan = %+v\n%s", d.m.plan, output(d.m))
	}
}
