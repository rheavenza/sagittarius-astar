package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
	"github.com/clitorhea/sagittarius-astar.git/internal/tools"
)

// ask_user lets the model ask the user a question in the middle of a task
// and continue with the answer, instead of ending its turn to ask.

const maxAskOptions = 6

func askUserDefinition() llm.Tool {
	return llm.Tool{
		Name: "ask_user",
		Description: "Ask the user a question and wait for the answer, then continue the task. " +
			"Use it only when the decision is genuinely the user's and a wrong guess would waste real work " +
			"(which of two designs, whether a destructive step is wanted, a missing value you cannot find). " +
			"Do not ask what you can find out by reading the code or running a command; otherwise make a sensible assumption and state it. " +
			"Ask one question at a time. Offer options when the likely answers are few; the user can always type their own.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"question": map[string]any{"type": "string", "description": "The question, self-contained and short."},
				"options": map[string]any{
					"type":        "array",
					"description": fmt.Sprintf("Optional: up to %d suggested answers, best first.", maxAskOptions),
					"items":       map[string]any{"type": "string"},
				},
			},
			"required": []string{"question"},
		},
	}
}

// askUser runs an ask_user call.
func (t *turn) askUser(ctx context.Context, call llm.ToolCall) outcome {
	q, _ := call.Args["question"].(string)
	q = strings.TrimSpace(q)
	if q == "" {
		return outcome{err: fmt.Errorf("ask_user: missing 'question'")}
	}
	var opts []string
	if raw, ok := call.Args["options"].([]any); ok {
		for _, o := range raw {
			if s, ok := o.(string); ok && strings.TrimSpace(s) != "" && len(opts) < maxAskOptions {
				opts = append(opts, strings.TrimSpace(s))
			}
		}
	}
	if t.sub {
		return deniedOutcome("Subagents cannot ask the user. Answer with what you found and say what is unclear.")
	}
	req := newRequest(&ApprovalRequest{Kind: ApproveQuestion, Call: call, Title: "Question from the agent", Question: q, Options: opts, Risk: tools.RiskRead})
	rep, ok := t.ask(ctx, req)
	if !ok {
		return deniedBy(rep, "The user did not answer. Continue with your best judgment and state the assumption you made.")
	}
	t.emit(ToolStartEvent{Call: call, Auto: false})
	answer := strings.TrimSpace(rep.Answer)
	if answer == "" {
		answer = "(empty answer)"
	}
	return outcome{result: "The user answered: " + answer}
}
