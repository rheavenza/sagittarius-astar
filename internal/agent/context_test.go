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
)

// longHistory builds sys + n user/assistant exchanges of about size bytes
// each, plus a final short question.
func longHistory(n, size int) []llm.Message {
	h := []llm.Message{{Role: llm.RoleSystem, Content: "sys"}}
	for i := 0; i < n; i++ {
		h = append(h,
			llm.Message{Role: llm.RoleUser, Content: fmt.Sprintf("question %d: %s", i, strings.Repeat("q", size/2))},
			llm.Message{Role: llm.RoleAssistant, Content: fmt.Sprintf("answer %d: %s", i, strings.Repeat("a", size/2))},
		)
	}
	return append(h, llm.Message{Role: llm.RoleUser, Content: "the new question"})
}

func TestAutoCompactionSummarizesOldTurns(t *testing.T) {
	p := &fakeProvider{replies: []reply{
		{tokens: []string{"SUMMARY: user asked 4 questions"}}, // compaction request
		{tokens: []string{"final answer"}},
	}}
	a := New(Options{Provider: p, MaxContextTokens: 2000})
	evs, msgs := collect(t, a.Run(context.Background(), longHistory(4, 1200)), nil)
	if d := done(t, evs); d.Err != nil {
		t.Fatal(d.Err)
	}

	// The first request was the summary, without tools, over a transcript.
	if len(p.toolSets[0]) != 0 || !strings.Contains(p.requests[0][1].Content, "<transcript>") ||
		!strings.Contains(p.requests[0][1].Content, "USER: question 0") {
		t.Fatalf("summary request = %+v", p.requests[0])
	}
	// The real request: system, summary + ack, recent turns, new question.
	main := p.requests[1]
	if !strings.HasPrefix(main[1].Content, CompactedPrefix) || !strings.Contains(main[1].Content, "SUMMARY: user asked") || main[2].Role != llm.RoleAssistant {
		t.Fatalf("compacted request starts with %+v", main[:3])
	}
	if main[len(main)-1].Content != "the new question" || strings.Contains(main[3].Content, "question 0") {
		t.Errorf("kept tail wrong: %v", main[3:])
	}

	// The front-end got the new history (and later the answer as usual).
	var hist *HistoryEvent
	var notes []string
	for _, ev := range evs {
		switch e := ev.(type) {
		case HistoryEvent:
			hist = &e
		case NoticeEvent:
			notes = append(notes, e.Text)
		}
	}
	if hist == nil || !strings.HasPrefix(hist.History[1].Content, CompactedPrefix) || hist.History[len(hist.History)-1].Content != "the new question" {
		t.Fatalf("history event = %+v", hist)
	}
	if len(msgs) != 1 || msgs[0].Content != "final answer" {
		t.Errorf("messages = %+v", msgs)
	}
	if !slices.ContainsFunc(notes, func(s string) bool { return strings.Contains(s, "Context compacted") }) {
		t.Errorf("notices = %v", notes)
	}
}

func TestCompactionFailureFallsBackToPruning(t *testing.T) {
	p := &fakeProvider{replies: []reply{
		{err: fmt.Errorf("rate limited")},
		{tokens: []string{"answer anyway"}},
	}}
	a := New(Options{Provider: p, MaxContextTokens: 2000})
	evs, msgs := collect(t, a.Run(context.Background(), longHistory(8, 1200)), nil)
	if d := done(t, evs); d.Err != nil {
		t.Fatal(d.Err)
	}
	if len(msgs) != 1 || msgs[0].Content != "answer anyway" {
		t.Fatalf("msgs = %+v", msgs)
	}
	// Pruned to fit: far fewer messages than the 18 in the history.
	if n := len(p.requests[1]); n >= 18 || p.requests[1][n-1].Content != "the new question" {
		t.Errorf("fallback request has %d messages", n)
	}
	warned := false
	for _, ev := range evs {
		if n, ok := ev.(NoticeEvent); ok && strings.Contains(n.Text, "compaction failed (rate limited)") {
			warned = true
		}
	}
	if !warned {
		t.Error("no warning about the failed compaction")
	}
}

func TestSingleLongTurnElidesOldToolOutputs(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "big.txt")
	os.WriteFile(f, []byte(strings.Repeat("line of text\n", 500)), 0o644) // ~6.5 KB
	var replies []reply
	for i := 0; i < 8; i++ {
		replies = append(replies, reply{calls: []llm.ToolCall{{ID: fmt.Sprint(i), Name: "read_file", Args: map[string]any{"path": f, "start_line": float64(i + 1)}}}})
	}
	replies = append(replies, reply{tokens: []string{"done"}})
	p := &fakeProvider{replies: replies}
	a := New(Options{Provider: p, WorkspaceRoot: dir, MaxContextTokens: 8000})
	evs, _ := collect(t, a.Run(context.Background(), user("read it many times")), nil)
	done(t, evs)

	last := p.requests[len(p.requests)-1]
	elided, full := 0, 0
	for _, m := range last {
		if m.Role == llm.RoleTool {
			if strings.HasPrefix(m.Content, elidedPrefix) {
				elided++
			} else {
				full++
			}
		}
	}
	if elided == 0 || full < 2 {
		t.Errorf("elided=%d full=%d; want old outputs elided and the 2 most recent kept", elided, full)
	}
	// Every request stayed within the window (8000 tokens minus the reserve).
	for i, r := range p.requests {
		if est := llm.EstimateHistoryTokens(r); est > 7000 {
			t.Errorf("request %d is ~%d tokens: over the window", i, est)
		}
	}
	// No summary request was made: nothing before the turn to summarize.
	for _, ts := range p.toolSets {
		if len(ts) == 0 {
			t.Error("unexpected tool-less (summary) request")
		}
	}
}

func TestManualCompactKeepsLastExchange(t *testing.T) {
	p := &fakeProvider{replies: []reply{{tokens: []string{"the summary"}}}}
	a := New(Options{Provider: p})
	h := longHistory(3, 100)
	h = append(h, llm.Message{Role: llm.RoleAssistant, Content: "latest answer"})
	out, err := a.Compact(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 5 || out[0].Role != llm.RoleSystem || !strings.Contains(out[1].Content, "the summary") ||
		out[3].Content != "the new question" || out[4].Content != "latest answer" {
		t.Fatalf("compacted = %+v", out)
	}
	// Compacting again right away has nothing to gain.
	if _, err := a.Compact(context.Background(), out); err == nil {
		t.Error("re-compacting a fresh summary should be refused")
	}
}

func TestUsageEventAndCalibration(t *testing.T) {
	p := &fakeProvider{replies: []reply{
		{tokens: []string{"one"}, usage: llm.Usage{InputTokens: 9000, OutputTokens: 10, CachedTokens: 8000}},
		{tokens: []string{"two"}},
	}}
	a := New(Options{Provider: p, MaxContextTokens: 100_000})
	hist := longHistory(2, 4000) // ~2k estimated tokens; the API says 9000
	evs, _ := collect(t, a.Run(context.Background(), hist), nil)
	var u *UsageEvent
	for _, ev := range evs {
		if e, ok := ev.(UsageEvent); ok {
			u = &e
		}
	}
	if u == nil || !u.Reported || u.Usage.InputTokens != 9000 || u.Usage.CachedTokens != 8000 || u.Window != 100_000 {
		t.Fatalf("usage event = %+v", u)
	}
	if _, f := a.budget.get(); f <= 1.5 {
		t.Errorf("calibration factor = %v, want it raised by the real count", f)
	}

	// Without reported usage the event carries a calibrated estimate.
	evs, _ = collect(t, a.Run(context.Background(), hist), nil)
	for _, ev := range evs {
		if e, ok := ev.(UsageEvent); ok {
			if e.Reported || e.Usage.InputTokens < 5000 {
				t.Errorf("estimated usage event = %+v", e)
			}
		}
	}
}

func TestLargeOutputStoredAndReadBack(t *testing.T) {
	p := &fakeProvider{replies: []reply{
		{calls: []llm.ToolCall{{ID: "1", Name: "run_command", Args: map[string]any{"command": "seq 1 20000"}}}},
		{calls: []llm.ToolCall{
			{ID: "2", Name: "read_output", Args: map[string]any{"id": "out-1", "start_line": float64(100), "end_line": float64(101)}},
			{ID: "3", Name: "read_output", Args: map[string]any{"id": "out-1", "pattern": "^1999[0-9]$"}},
			{ID: "4", Name: "read_output", Args: map[string]any{"id": "out-9"}},
		}},
		{tokens: []string{"ok"}},
	}}
	a := New(Options{Provider: p})
	a.SetMode(ModeAuto)
	evs, msgs := collect(t, a.Run(context.Background(), user("count")), nil)
	done(t, evs)
	assertAllCallsAnswered(t, msgs)

	stored := msgs[1].Content
	if len(stored) > 10*1024 || !strings.Contains(stored, `stored as "out-1"`) || !strings.Contains(stored, "20000") {
		t.Fatalf("stored preview (%d bytes) = %q…", len(stored), stored[:200])
	}
	if !slices.Contains(p.toolSets[1], "read_output") || slices.Contains(p.toolSets[0], "read_output") {
		t.Errorf("read_output offered: %v / %v", p.toolSets[0], p.toolSets[1])
	}
	if r := msgs[3].Content; !strings.Contains(r, "lines 100-101") {
		t.Errorf("range read = %q", r)
	}
	if r := msgs[4].Content; !strings.Contains(r, "10 matching lines") || !strings.Contains(r, "19995") {
		t.Errorf("pattern read = %q", r)
	}
	if r := msgs[5].Content; !strings.Contains(r, "no stored output") {
		t.Errorf("unknown id = %q", r)
	}
}
