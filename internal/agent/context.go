package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
	"github.com/clitorhea/sagittarius-astar.git/internal/logger"
	"github.com/clitorhea/sagittarius-astar.git/internal/tools"
)

// Context management: token budgeting against the model's window,
// calibration of the byte-based estimate with the API's real counts, and
// compaction (summarizing older turns) instead of silently dropping them.

const (
	// compactAt is the share of the window at which history is compacted.
	compactAt = 0.80
	// keepShare of the window is kept verbatim (the most recent turns).
	keepShare = 0.35
	// maxOutputReserve caps the room left for the model's answer.
	maxOutputReserve = 16_384
	// maxSummaryInputShare of the window may be used by the transcript sent
	// to the summarizer.
	maxSummaryInputShare = 0.6
)

// CompactedPrefix starts the user message that carries a compaction summary.
const CompactedPrefix = "[Earlier conversation compacted]"

const compactAck = "Understood. I'll continue from this summary, and re-read files when I need their exact current contents."

const compactSystem = "You compress a coding agent's conversation so that the agent can continue the same task with much less context. You are precise and keep exact identifiers."

const compactInstr = `Summarize the conversation transcript below for the agent that will continue it. It will see only your summary and the most recent turns.

Include, as markdown bullets under short headings:
1. The user's goals, explicit requests and constraints (quote short key instructions verbatim).
2. What was done: files read and changed (exact paths), commands run and their outcomes, tools used for what.
3. Decisions made and why.
4. Current state: what works, what is broken, open errors (exact messages).
5. Next steps that were planned or are obviously needed.
Keep exact paths, function names, commands, flags and error messages. Drop pleasantries and dead ends unless they matter later. If the transcript starts with an earlier summary, merge it in. At most ~700 words.

<transcript>
%s
</transcript>`

// budget tracks the context window and the estimate calibration.
type budget struct {
	mu       sync.Mutex
	window   int     // model context window in tokens; 0 = unknown
	factor   float64 // real tokens / estimated tokens, smoothed
	override int     // Options.MaxContextTokens (tests, explicit config)
}

func (b *budget) get() (window int, factor float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	w := b.window
	if b.override > 0 {
		w = b.override
	}
	if w <= 0 {
		w = DefaultMaxContextTokens
	}
	f := b.factor
	if f <= 0 {
		f = 1
	}
	return w, f
}

// calibrate folds one request's real input token count into the factor.
func (b *budget) calibrate(estimated, actual int) {
	if estimated < 500 || actual <= 0 {
		return // too small to say anything
	}
	r := float64(actual) / float64(estimated)
	r = min(max(r, 0.5), 3)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.factor <= 0 {
		b.factor = r
	} else {
		b.factor = 0.7*b.factor + 0.3*r
	}
}

// SetContextWindow sets the model's context window in tokens (the TUI calls
// it when the model changes).
func (a *Agent) SetContextWindow(tokens int) {
	a.budget.mu.Lock()
	defer a.budget.mu.Unlock()
	a.budget.window = tokens
}

// ContextWindow returns the effective context window in tokens.
func (a *Agent) ContextWindow() int {
	w, _ := a.budget.get()
	return w
}

// limits returns, in calibrated tokens: the hard limit for a request and
// the point at which to compact.
func limits(window int) (hard, compact, keep int) {
	reserve := min(window/8, maxOutputReserve)
	hard = window - reserve
	compact = min(int(float64(window)*compactAt), hard)
	keep = int(float64(window) * keepShare)
	return hard, compact, keep
}

// estimate is the calibrated token estimate of a request.
func (t *turn) estimate(msgs []llm.Message, defs []llm.Tool) int {
	_, factor := t.a.budget.get()
	return int(float64(llm.EstimateHistoryTokens(msgs)+defsTokens(defs)) * factor)
}

func defsTokens(defs []llm.Tool) int {
	if len(defs) == 0 {
		return 0
	}
	b, err := json.Marshal(defs)
	if err != nil {
		return 0
	}
	return len(b) / 4
}

// fitContext makes t.history fit the window before a request: compact when
// past the threshold, and as a last resort drop the oldest turns from what
// is sent (PruneHistory). It returns the messages to send.
func (t *turn) fitContext(ctx context.Context, defs []llm.Tool) []llm.Message {
	window, factor := t.a.budget.get()
	hard, compactLimit, keep := limits(window)
	withCtx := WithContextBlock(t.history, t.contextBlock)

	if est := t.estimate(withCtx, defs); est > compactLimit && !t.compactFailed {
		before := est
		if err := t.compact(ctx, keep); err != nil {
			switch {
			case ctx.Err() != nil:
			case errors.Is(err, errNothingToCompact):
				// Cheap to re-check next round; warn once per turn.
				if !t.compactWarned {
					t.compactWarned = true
					t.emit(NoticeEvent{Level: NoticeWarn, Text: "The context is nearly full and nothing more can be compacted; the oldest turns will be dropped."})
				}
			default:
				t.compactFailed = true // a failed summary request: don't retry every round
				logger.L.Warn("agent: compaction failed", "error", err)
				t.emit(NoticeEvent{Level: NoticeWarn, Text: "Context compaction failed (" + err.Error() + "); dropping the oldest turns instead."})
			}
		} else {
			withCtx = WithContextBlock(t.history, t.contextBlock)
			after := t.estimate(withCtx, defs)
			t.emit(NoticeEvent{Level: NoticeInfo, Text: fmt.Sprintf("📦 Context compacted: ~%s → ~%s tokens of %s", kTokens(before), kTokens(after), kTokens(window))})
		}
	}

	// Hard limit: PruneHistory works on raw estimates.
	raw := int(float64(hard)/factor) - defsTokens(defs) - llm.EstimateTokens(t.contextBlock)
	return WithContextBlock(llm.PruneHistory(t.history, max(raw, 1000)), t.contextBlock)
}

func kTokens(n int) string {
	if n >= 10_000 {
		return fmt.Sprintf("%dk", (n+500)/1000)
	}
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprint(n)
}

// compact replaces older turns of t.history with a summary, keeping the
// most recent turns that fit in keep tokens. When the history is a single
// long turn there is nothing to summarize, so old tool outputs of that turn
// are elided instead. The front-end is told with a HistoryEvent.
func (t *turn) compact(ctx context.Context, keep int) error {
	newHist, err := t.a.compactHistory(ctx, t.provider, t.history, keep)
	if err != nil {
		return err
	}
	t.history = newHist
	t.emit(HistoryEvent{History: append([]llm.Message(nil), newHist...), Reason: "compacted"})
	return nil
}

// Compact summarizes all but the last user turn of history (the /compact
// command). It returns the new history.
func (a *Agent) Compact(ctx context.Context, history []llm.Message) ([]llm.Message, error) {
	return a.compactHistory(ctx, a.Provider(), history, 0)
}

// compactHistory does the work for compact and Compact. keep (calibrated
// tokens) bounds the verbatim tail; 0 keeps only the last user turn.
func (a *Agent) compactHistory(ctx context.Context, provider llm.Provider, history []llm.Message, keep int) ([]llm.Message, error) {
	if provider == nil {
		return nil, fmt.Errorf("no model provider configured")
	}
	window, factor := a.budget.get()
	head := 0
	if len(history) > 0 && history[0].Role == llm.RoleSystem {
		head = 1
	}

	// Candidate cut points: user messages after the head. Keep the earliest
	// one whose tail fits in keep; never cut after the last user message.
	lastUser := -1
	for i := len(history) - 1; i >= head; i-- {
		if history[i].Role == llm.RoleUser {
			lastUser = i
			break
		}
	}
	if lastUser < 0 {
		return nil, fmt.Errorf("nothing to compact")
	}
	cut := lastUser
	if keep > 0 {
		for i := head; i < lastUser; i++ {
			if history[i].Role != llm.RoleUser {
				continue
			}
			if int(float64(llm.EstimateHistoryTokens(history[i:]))*factor) <= keep {
				cut = i
				break
			}
		}
	}

	// Only a summary of an earlier compaction (and its ack) before the cut:
	// summarizing it again gains nothing. Same if nothing precedes the cut.
	if cut <= head || (cut-head <= 2 && strings.HasPrefix(history[head].Content, CompactedPrefix)) {
		if keep == 0 { // manual /compact
			return nil, fmt.Errorf("%w: the conversation is already compacted up to the last exchange", errNothingToCompact)
		}
		out, n := elideToolOutputs(history, cut, int(float64(window)/2/factor))
		if n == 0 {
			return nil, fmt.Errorf("%w: the current turn alone fills the context", errNothingToCompact)
		}
		return out, nil
	}

	transcript := renderTranscript(history[head:cut], int(float64(window)*maxSummaryInputShare/factor)*4)
	summary, err := completeText(ctx, provider, compactSystem, fmt.Sprintf(compactInstr, transcript))
	if err != nil {
		return nil, err
	}

	out := make([]llm.Message, 0, head+2+len(history)-cut)
	out = append(out, history[:head]...)
	out = append(out,
		llm.Message{Role: llm.RoleUser, Content: CompactedPrefix + "\nThe earlier part of this conversation was summarized to save context:\n\n" + summary},
		llm.Message{Role: llm.RoleAssistant, Content: compactAck},
	)
	out = append(out, history[cut:]...)
	return out, nil
}

// errNothingToCompact: compaction found nothing it could shrink.
var errNothingToCompact = errors.New("nothing to compact")

// elideToolOutputs shortens large tool results in history[from:], oldest
// first, until the history's raw estimate is at most target tokens; the two
// most recent results are always kept. It is used when a single turn fills
// the context. It returns the new history and how many results it shortened.
func elideToolOutputs(history []llm.Message, from, target int) ([]llm.Message, int) {
	const keepRecent = 2
	const minSize = 2048
	out := append([]llm.Message(nil), history...)
	var idx []int
	for i := max(from, 0); i < len(out); i++ {
		if out[i].Role == llm.RoleTool {
			idx = append(idx, i)
		}
	}
	n := 0
	total := llm.EstimateHistoryTokens(out)
	for k, i := range idx {
		if total <= target {
			break
		}
		if k >= len(idx)-keepRecent || len(out[i].Content) < minSize || strings.HasPrefix(out[i].Content, elidedPrefix) {
			continue
		}
		total -= llm.EstimateMessageTokens(out[i])
		out[i].Content = fmt.Sprintf("%s %d bytes of %s output removed to free context. Re-run the tool if you need it again.]\n%s",
			elidedPrefix, len(out[i].Content), out[i].ToolName, tools.TruncateUTF8(out[i].Content, 400))
		total += llm.EstimateMessageTokens(out[i])
		n++
	}
	return out, n
}

const elidedPrefix = "[elided:"

// renderTranscript renders messages as plain text for the summarizer,
// truncating long parts, within maxBytes (keeping the end, which matters
// most, when it must cut).
func renderTranscript(msgs []llm.Message, maxBytes int) string {
	var b strings.Builder
	clip := func(s string, n int) string {
		s = strings.TrimSpace(s)
		if len(s) <= n {
			return s
		}
		return tools.TruncateUTF8(s, n) + fmt.Sprintf(" …(%d more bytes)", len(s)-n)
	}
	for _, m := range msgs {
		switch m.Role {
		case llm.RoleUser:
			b.WriteString("USER: " + clip(m.Content, 6000) + "\n\n")
		case llm.RoleAssistant:
			if c := strings.TrimSpace(m.Content); c != "" {
				b.WriteString("ASSISTANT: " + clip(c, 4000) + "\n")
			}
			for _, c := range m.ToolCalls {
				args, _ := json.Marshal(c.Args)
				b.WriteString("ASSISTANT → " + c.Name + " " + clip(string(args), 400) + "\n")
			}
			b.WriteString("\n")
		case llm.RoleTool:
			b.WriteString("RESULT (" + m.ToolName + "): " + clip(m.Content, 1500) + "\n\n")
		case llm.RoleSystem:
			b.WriteString("NOTE: " + clip(m.Content, 800) + "\n\n")
		}
	}
	s := b.String()
	if maxBytes > 0 && len(s) > maxBytes {
		headLen := maxBytes / 4
		h := tools.TruncateUTF8(s, headLen)
		t := s[len(s)-(maxBytes-headLen):]
		s = h + "\n…(middle of the transcript omitted)…\n" + strings.ToValidUTF8(t, "")
	}
	return s
}

// completeText sends one tool-less request and returns the answer text.
func completeText(ctx context.Context, provider llm.Provider, system, user string) (string, error) {
	ch := make(chan string, 64)
	errc := make(chan error, 1)
	go func() {
		_, _, err := provider.StreamChat(ctx, []llm.Message{
			{Role: llm.RoleSystem, Content: system},
			{Role: llm.RoleUser, Content: user},
		}, nil, ch)
		errc <- err
	}()
	var split llm.ThinkSplitter
	for tok := range ch {
		if !strings.HasPrefix(tok, "\x00") {
			split.Write(tok)
		}
	}
	err := <-errc
	split.Flush()
	if err != nil {
		return "", err
	}
	out := strings.TrimSpace(split.Content())
	if out == "" {
		return "", fmt.Errorf("empty summary")
	}
	return out, nil
}
