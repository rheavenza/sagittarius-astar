package llm

import "encoding/json"

// EstimateTokens provides a rough estimate of the number of tokens in a given text.
// It uses the common heuristic of 4 characters per token.
func EstimateTokens(text string) int {
	return len(text) / 4
}

// EstimateMessageTokens estimates the tokens of a single message, including
// tool-call arguments (which can be large, e.g. write_file content).
func EstimateMessageTokens(msg Message) int {
	total := EstimateTokens(msg.Content) + EstimateTokens(msg.ReasoningContent)
	for _, tc := range msg.ToolCalls {
		total += EstimateTokens(tc.Name)
		if b, err := json.Marshal(tc.Args); err == nil {
			total += len(b) / 4
		}
	}
	return total
}

// EstimateHistoryTokens estimates the total number of tokens in a conversation history.
func EstimateHistoryTokens(messages []Message) int {
	var total int
	for _, msg := range messages {
		total += EstimateMessageTokens(msg)
	}
	return total
}

// PruneHistory keeps the context window within maxTokens by dropping the
// oldest turns.
//
// Invariants:
//   - A leading system prompt is always kept.
//   - Cuts happen only at user-message boundaries, so the remaining history
//     always starts with a user turn and assistant tool_calls are never
//     separated from their tool results.
//   - The most recent user turn (and everything after it) is never dropped,
//     even if it alone exceeds the budget.
func PruneHistory(messages []Message, maxTokens int) []Message {
	if EstimateHistoryTokens(messages) <= maxTokens {
		return messages
	}

	var head []Message
	rest := messages
	if len(rest) > 0 && rest[0].Role == RoleSystem {
		head = rest[:1]
		rest = rest[1:]
	}
	budget := maxTokens - EstimateHistoryTokens(head)

	lastUser := -1
	for i := len(rest) - 1; i >= 0; i-- {
		if rest[i].Role == RoleUser {
			lastUser = i
			break
		}
	}
	if lastUser <= 0 {
		// Nothing we can safely cut.
		return messages
	}

	start := 0
	for start < lastUser && EstimateHistoryTokens(rest[start:]) > budget {
		next := start + 1
		for next < lastUser && rest[next].Role != RoleUser {
			next++
		}
		start = next
	}
	// If the first kept message isn't a user turn (can only happen at start==0
	// when the history began without one), advance to the first user message.
	for start < lastUser && rest[start].Role != RoleUser {
		start++
	}

	out := make([]Message, 0, len(head)+len(rest)-start)
	out = append(out, head...)
	out = append(out, rest[start:]...)
	return out
}
