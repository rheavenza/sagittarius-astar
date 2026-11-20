package llm

import (
	"strings"
	"testing"
)

func TestSplitReasoningBothTags(t *testing.T) {
	content, reasoning, open := SplitReasoning("a<think>r1</think>b<thought>r2</thought>c")
	if content != "abc" || reasoning != "r1r2" || open {
		t.Fatalf("got content=%q reasoning=%q open=%v", content, reasoning, open)
	}
	content, reasoning, open = SplitReasoning("x<thought>partial")
	if content != "x" || reasoning != "partial" || !open {
		t.Fatalf("unclosed block: content=%q reasoning=%q open=%v", content, reasoning, open)
	}
}

func TestThinkSplitterHandlesTagsSplitAcrossChunks(t *testing.T) {
	input := "Hello <think>step one, step two</think> world <thought>more</thought>!"
	// Feed one byte at a time: every tag is split across chunks.
	var s ThinkSplitter
	for i := 0; i < len(input); i++ {
		s.Write(input[i : i+1])
		if strings.Contains(s.Content(), "<") {
			t.Fatalf("partial tag leaked into content at byte %d: %q", i, s.Content())
		}
	}
	s.Flush()
	if s.Content() != "Hello  world !" || s.Reasoning() != "step one, step twomore" {
		t.Fatalf("content=%q reasoning=%q", s.Content(), s.Reasoning())
	}
}

func TestThinkSplitterFlushesNonTagLessThan(t *testing.T) {
	var s ThinkSplitter
	s.Write("if a <")
	if s.Content() != "if a " {
		t.Fatalf("'<' should be held back, got %q", s.Content())
	}
	s.Write(" b")
	s.Flush()
	if s.Content() != "if a < b" {
		t.Fatalf("got %q", s.Content())
	}
}
