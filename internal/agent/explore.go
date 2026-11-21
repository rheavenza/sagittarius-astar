package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
	"github.com/clitorhea/sagittarius-astar.git/internal/tools"
)

// exploreMaxRounds bounds an explore subagent's tool rounds; after that it
// must answer with what it found.
const exploreMaxRounds = 20

// explorePrompt is the subagent's system prompt. Its answer goes to the main
// agent, not to the user.
const explorePrompt = `You are an exploration subagent inside aig, a coding agent. The main agent sent you one question about the codebase. Investigate it with your read-only tools and answer it.

# Rules
- You cannot modify files, run commands, browse the web, or ask anyone anything. Only local read-only tools work, inside the workspace.
- Be efficient: orient with repo_map, search_regex and find_files, then read_file only the relevant line ranges. Don't read whole large files.
- Stop as soon as you can answer well. Don't explore beyond the question.

# Answer
Your answer is read by the main agent, which has not seen your tool output, so make it self-contained:
- Lead with the direct answer.
- Cite evidence as path:line, with function and type names, and explain how the pieces connect (call flow, data flow).
- Quote code only when the exact text matters (a signature, a condition), and keep quotes short.
- Say what you did not verify or could not find.
- No preamble and no offers of further help. Aim for under 400 words unless the question needs more.`

// exploreLimitMsg is sent when the subagent runs out of rounds.
const exploreLimitMsg = "You have reached the exploration limit. Do not call more tools: answer the question now from what you have found, and say what remains unverified."

// exploreDefs are the tools an explore subagent may use: local read-only
// tools, without explore itself (no recursion).
func exploreDefs() []llm.Tool {
	var out []llm.Tool
	for _, d := range tools.Definitions() {
		if r, ok := tools.RiskOf(d.Name); ok && r == tools.RiskRead && d.Name != "explore" {
			out = append(out, d)
		}
	}
	return out
}

// explore runs a subagent with a fresh context on the question and returns
// its final answer. Its tool calls are reported as progress notices.
func (t *turn) explore(ctx context.Context, call llm.ToolCall) outcome {
	question, _ := call.Args["question"].(string)
	question = strings.TrimSpace(question)
	if question == "" {
		return outcome{err: fmt.Errorf("explore: missing 'question' argument")}
	}
	if hints, _ := call.Args["hints"].(string); strings.TrimSpace(hints) != "" {
		question += "\n\nHints from the main agent: " + strings.TrimSpace(hints)
	}

	t.emit(ToolStartEvent{Call: call, Auto: true})

	ch := make(chan Event, 64)
	sub := &turn{
		a:            t.a,
		provider:     t.provider,
		contextBlock: t.a.contextBlockSnapshot(),
		history: []llm.Message{
			{Role: llm.RoleSystem, Content: explorePrompt},
			{Role: llm.RoleUser, Content: question},
		},
		ch:       ch,
		sub:      true,
		toolDefs: exploreDefs(),
	}

	type result struct {
		answer       string
		rounds, uses int
		err          error
	}
	done := make(chan result, 1)
	go func() {
		defer close(ch)
		ans, rounds, uses, err := sub.runSub(ctx)
		done <- result{ans, rounds, uses, err}
	}()
	for ev := range ch {
		switch e := ev.(type) {
		case ToolStartEvent:
			t.emit(NoticeEvent{Level: NoticeInfo, Text: "  ↳ explore: " + describeCall(e.Call)})
		case ApprovalEvent:
			e.Req.Deny() // unreachable: sub turns never ask (see turn.ask)
		}
	}
	res := <-done

	if res.err != nil {
		if ctx.Err() != nil {
			return outcome{err: fmt.Errorf("explore: aborted by user")}
		}
		return outcome{err: fmt.Errorf("explore: %w", res.err)}
	}
	answer := strings.TrimSpace(res.answer)
	if answer == "" {
		answer = "(the explore subagent returned no answer)"
	}
	return outcome{result: fmt.Sprintf("%s\n\n(explore subagent: %d tool calls in %d rounds; its tool output is not in your context — read files yourself before editing them)", answer, res.uses, res.rounds)}
}

// runSub is the subagent loop: like run, but it returns the final text
// instead of emitting a DoneEvent, and forces an answer at the round limit.
func (t *turn) runSub(ctx context.Context) (answer string, rounds, uses int, err error) {
	for round := 0; ; round++ {
		final := round >= exploreMaxRounds
		if final {
			t.toolDefs = []llm.Tool{} // no tools: the model must answer
			t.history = append(t.history, llm.Message{Role: llm.RoleUser, Content: exploreLimitMsg})
		}
		text, _, calls, err := t.stream(ctx)
		if err != nil {
			return "", round, uses, err
		}
		if len(calls) == 0 || final {
			return text, round, uses, nil
		}
		t.appendMsg(llm.Message{Role: llm.RoleAssistant, Content: text, ToolCalls: calls})
		uses += len(calls)
		if stop := t.runCalls(ctx, calls); stop {
			return "", round + 1, uses, ctx.Err()
		}
	}
}

// describeCall is a one-line summary of a tool call for progress notices.
func describeCall(c llm.ToolCall) string {
	arg := ""
	for _, k := range []string{"path", "pattern", "glob", "dir", "question"} {
		if v, ok := c.Args[k].(string); ok && v != "" {
			arg = v
			break
		}
	}
	if len(arg) > 80 {
		arg = tools.TruncateUTF8(arg, 77) + "…"
	}
	if arg == "" {
		return c.Name
	}
	return c.Name + " " + arg
}
