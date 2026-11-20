package llm

import (
	"strings"
	"testing"
)

func big(n int) string { return strings.Repeat("x", n*4) } // ≈ n tokens

func TestPruneHistoryUnderBudgetIsNoop(t *testing.T) {
	msgs := []Message{{Role: RoleSystem, Content: "sys"}, {Role: RoleUser, Content: "hi"}}
	got := PruneHistory(msgs, 1000)
	if len(got) != 2 {
		t.Fatalf("expected no pruning, got %d messages", len(got))
	}
}

func TestPruneHistoryCutsAtUserBoundaries(t *testing.T) {
	msgs := []Message{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: big(100)},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "a", Name: "read_file", Args: map[string]any{"path": "x"}}}},
		{Role: RoleTool, Content: big(100), ToolCallID: "a"},
		{Role: RoleAssistant, Content: "done"},
		{Role: RoleUser, Content: "second question"},
		{Role: RoleAssistant, Content: "answer"},
	}
	got := PruneHistory(msgs, 50)

	if got[0].Role != RoleSystem {
		t.Fatalf("system prompt must be kept, got %q first", got[0].Role)
	}
	if got[1].Role != RoleUser || got[1].Content != "second question" {
		t.Fatalf("history after system prompt must start at a user turn, got %+v", got[1])
	}
	for _, m := range got {
		if m.Role == RoleTool {
			t.Fatal("orphaned tool message survived pruning")
		}
	}
}

func TestPruneHistoryKeepsLatestUserTurnEvenIfHuge(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Content: "old"},
		{Role: RoleAssistant, Content: "old answer"},
		{Role: RoleUser, Content: big(500)},
	}
	got := PruneHistory(msgs, 10)
	if len(got) == 0 || got[len(got)-1].Content != msgs[2].Content {
		t.Fatalf("latest user turn must never be dropped: %+v", got)
	}
}

func TestEstimateCountsToolArgs(t *testing.T) {
	m := Message{Role: RoleAssistant, ToolCalls: []ToolCall{{Name: "write_file", Args: map[string]any{"content": big(200)}}}}
	if EstimateMessageTokens(m) < 200 {
		t.Fatalf("tool-call args not counted: %d", EstimateMessageTokens(m))
	}
}
