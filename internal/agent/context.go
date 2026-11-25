package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

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
	// DefaultCompactAtTokens caps the compaction point on large windows:
	// requests near a 1M-token window are slow and costly, and models get
	// worse with very long contexts well before the window is full.
	DefaultCompactAtTokens = 200_000
	// minTurnCompactBytes: a part of the current turn smaller than this is
	// not worth a summary request.
	minTurnCompactBytes = 8 * 1024
	// maxRecordCommands is how many commands the exact record keeps.
	maxRecordCommands = 15
)

// TurnCompactedPrefix starts the system note that replaces the earlier tool
// rounds of the current request (compaction within one long turn).
const TurnCompactedPrefix = "[Progress on this request compacted]"

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

const compactTurnInstr = `The agent below is in the middle of one long request from the user. Summarize its progress so far for the agent that continues it: it will see the user's request, your summary and its most recent tool rounds, but not the transcript below.

Include, as markdown bullets under short headings:
1. Findings: what the agent learned about the code and the problem (exact paths, function names, line numbers, behaviour).
2. Changes made so far (exact paths and what changed) and commands run with their outcomes.
3. What failed or was ruled out, and why (exact error messages).
4. Where it is now and what it was about to do next.
Keep exact identifiers. Drop raw file contents and long outputs; keep only the facts the agent will need. If the transcript contains an earlier progress summary, merge it in. At most ~600 words.

<request>
%s
</request>

<transcript>
%s
</transcript>`

// budget tracks the context window and the estimate calibration.
type budget struct {
	mu       sync.Mutex
	window   int     // model context window in tokens; 0 = unknown
	factor   float64 // real tokens / estimated tokens, smoothed
	override int     // Options.MaxContextTokens (tests, explicit config)
	// compactCap caps the compaction point in tokens: 0 = DefaultCompactAtTokens,
	// negative = no cap (compact at compactAt of the window).
	compactCap int
}

// SetCompactAt sets the token count at which history is compacted on large
// windows (0 = DefaultCompactAtTokens, negative = only the window share).
func (a *Agent) SetCompactAt(tokens int) {
	a.budget.mu.Lock()
	defer a.budget.mu.Unlock()
	a.budget.compactCap = tokens
}

func (b *budget) capTokens() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case b.compactCap < 0:
		return 0
	case b.compactCap == 0:
		return DefaultCompactAtTokens
	}
	return b.compactCap
}

// SetCompactProvider sets the model that writes compaction summaries (nil =
// the session's model). A cheaper, fast model works well here.
func (a *Agent) SetCompactProvider(p llm.Provider) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.compactProvider = p
}

func (a *Agent) summarizer(fallback llm.Provider) llm.Provider {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.compactProvider != nil {
		return a.compactProvider
	}
	return fallback
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

// limits returns, in calibrated tokens: the hard limit for a request, the
// point at which to compact, and how much to keep verbatim. capTokens > 0
// lowers the compaction point on large windows; the verbatim share scales
// with it, so compaction still frees most of the context.
func limits(window, capTokens int) (hard, compact, keep int) {
	reserve := min(window/8, maxOutputReserve)
	hard = window - reserve
	eff := float64(window)
	if capTokens > 0 && float64(capTokens) < eff*compactAt {
		eff = float64(capTokens) / compactAt
	}
	compact = min(int(eff*compactAt), hard)
	keep = int(eff * keepShare)
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
	hard, compactLimit, keep := limits(window, t.a.budget.capTokens())
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
	newHist, turnErr, err := t.a.compactHistory(ctx, t.provider, t.history, keep, t.turnSummaryFailed)
	if turnErr != nil && !t.turnSummaryFailed {
		t.turnSummaryFailed = true
		t.emit(NoticeEvent{Level: NoticeWarn, Text: "Summarizing this turn's earlier tool rounds failed (" + shortError(turnErr) + "); shortening old tool outputs instead."})
	}
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
	out, _, err := a.compactHistory(ctx, a.Provider(), history, 0, false)
	return out, err
}

// summarize sends one summary request with the compaction model, retrying
// transient errors like the main loop does.
func (a *Agent) summarize(ctx context.Context, fallback llm.Provider, prompt string) (string, error) {
	p := a.summarizer(fallback)
	for attempt := 0; ; attempt++ {
		out, err := completeText(ctx, p, compactSystem, prompt)
		if err == nil || ctx.Err() != nil || attempt >= maxStreamRetries || !llm.IsRetryable(err) {
			return out, err
		}
		logger.L.Warn("agent: summary request failed, retrying", "attempt", attempt+1, "error", err)
		select {
		case <-time.After(retryDelay(attempt)):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// compactHistory does the work for compact and Compact. keep (calibrated
// tokens) bounds the verbatim tail; 0 keeps only the last user turn. When
// one turn fills the context it summarizes that turn's earlier rounds
// (unless skipTurnSummary) and falls back to shortening tool outputs;
// turnErr reports a failed turn summary.
func (a *Agent) compactHistory(ctx context.Context, provider llm.Provider, history []llm.Message, keep int, skipTurnSummary bool) (out []llm.Message, turnErr, err error) {
	if provider == nil {
		return nil, nil, fmt.Errorf("no model provider configured")
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
		return nil, nil, fmt.Errorf("nothing to compact")
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
			return nil, nil, fmt.Errorf("%w: the conversation is already compacted up to the last exchange", errNothingToCompact)
		}
		// One long turn fills the context: summarize its earlier tool rounds,
		// or, if that isn't possible, shorten its old tool outputs.
		if !skipTurnSummary {
			out, err := a.compactTurn(ctx, provider, history, lastUser, keep)
			if err == nil {
				return out, nil, nil
			}
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			if !errors.Is(err, errNothingToCompact) {
				logger.L.Warn("agent: in-turn compaction failed; eliding tool outputs", "error", err)
				turnErr = err
			}
		}
		out, n := a.elideToolOutputs(history, cut, int(float64(window)/2/factor))
		if n == 0 {
			return nil, turnErr, fmt.Errorf("%w: the current turn alone fills the context", errNothingToCompact)
		}
		return out, turnErr, nil
	}

	transcript := renderTranscript(history[head:cut], int(float64(window)*maxSummaryInputShare/factor)*4)
	summary, err := a.summarize(ctx, provider, fmt.Sprintf(compactInstr, transcript))
	if err != nil {
		return nil, nil, err
	}
	summary += a.summaryAppendix(history[head:cut])

	out = make([]llm.Message, 0, head+2+len(history)-cut)
	out = append(out, history[:head]...)
	out = append(out,
		llm.Message{Role: llm.RoleUser, Content: CompactedPrefix + "\nThe earlier part of this conversation was summarized to save context:\n\n" + summary},
		llm.Message{Role: llm.RoleAssistant, Content: compactAck},
	)
	out = append(out, history[cut:]...)
	return out, nil, nil
}

// errNothingToCompact: compaction found nothing it could shrink.
var errNothingToCompact = errors.New("nothing to compact")

// compactTurn summarizes the earlier tool rounds of the current request
// (history[lastUser+1:cut]) into one system note, keeping the user's request
// and the most recent rounds that fit in keep tokens verbatim. Cuts are made
// before an assistant message, so tool calls and their results stay together.
func (a *Agent) compactTurn(ctx context.Context, provider llm.Provider, history []llm.Message, lastUser, keep int) ([]llm.Message, error) {
	window, factor := a.budget.get()
	from := lastUser + 1
	cut, lastAssistant := -1, -1
	for i := from + 1; i < len(history); i++ {
		if history[i].Role != llm.RoleAssistant {
			continue
		}
		lastAssistant = i
		if cut < 0 && int(float64(llm.EstimateHistoryTokens(history[i:]))*factor) <= keep {
			cut = i
		}
	}
	if cut < 0 {
		cut = lastAssistant // keep at least the latest round
	}
	if cut <= from {
		return nil, fmt.Errorf("%w: no earlier tool rounds in this turn", errNothingToCompact)
	}
	segment := history[from:cut]
	size := 0
	for _, m := range segment {
		size += len(m.Content)
		for _, c := range m.ToolCalls {
			b, _ := json.Marshal(c.Args)
			size += len(b)
		}
	}
	if size < minTurnCompactBytes {
		return nil, fmt.Errorf("%w: the earlier rounds of this turn are small", errNothingToCompact)
	}

	request := tools.TruncateUTF8(strings.TrimSpace(history[lastUser].Content), 6000)
	transcript := renderTranscript(segment, int(float64(window)*maxSummaryInputShare/factor)*4)
	summary, err := a.summarize(ctx, provider, fmt.Sprintf(compactTurnInstr, request, transcript))
	if err != nil {
		return nil, err
	}
	summary += a.summaryAppendix(segment)

	out := make([]llm.Message, 0, from+1+len(history)-cut)
	out = append(out, history[:from]...)
	out = append(out, llm.Message{Role: llm.RoleSystem, Content: TurnCompactedPrefix +
		"\nThe earlier tool rounds of this request were summarized to save context. Re-read files before editing them.\n\n" + summary})
	out = append(out, history[cut:]...)
	return out, nil
}

// summaryAppendix is what aig adds to a summary itself, exactly: the record
// of file changes and commands taken from the summarized tool calls (merged
// with the record of an earlier summary), and the current plan.
func (a *Agent) summaryAppendix(summarized []llm.Message) string {
	var b strings.Builder
	if rec := workRecord(summarized); rec != "" {
		b.WriteString("\n\n" + rec)
	}
	if pb := planBlock(a.plan.get()); pb != "" {
		b.WriteString("\n\n" + pb)
	}
	return b.String()
}

const (
	recordHeading = "## Exact record (from tool calls)"
	recordEdited  = "- edited: "
	recordRan     = "- ran: "
)

// workRecord lists the files changed and the commands run in msgs, from the
// tool calls and their results, so the summary doesn't depend on the model
// copying them. Lines of an earlier record inside msgs are carried over.
func workRecord(msgs []llm.Message) string {
	results := map[string]string{}
	for _, m := range msgs {
		if m.Role == llm.RoleTool {
			results[m.ToolCallID] = m.Content
		}
	}
	var edited []string
	seen := map[string]bool{}
	addEdited := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			edited = append(edited, p)
		}
	}
	var ran []string
	for _, m := range msgs {
		// Carry over an earlier record (in a previous summary).
		prevEdited, prevRan := earlierRecord(m.Content)
		for _, p := range prevEdited {
			addEdited(p)
		}
		ran = append(ran, prevRan...)
		for _, c := range m.ToolCalls {
			res, answered := results[c.ID]
			switch c.Name {
			case "write_file", "edit_file", "multi_edit":
				if p, _ := c.Args["path"].(string); answered && strings.HasPrefix(res, "Successfully wrote") {
					addEdited(p)
				}
			case "run_command":
				cmd, _ := c.Args["command"].(string)
				if !answered || cmd == "" {
					continue
				}
				ran = append(ran, recordRan+"`"+oneLineClip(cmd, 160)+"` → "+commandOutcome(res))
			}
		}
	}
	if len(ran) > maxRecordCommands {
		ran = append([]string{fmt.Sprintf("- (%d earlier commands omitted)", len(ran)-maxRecordCommands)}, ran[len(ran)-maxRecordCommands:]...)
	}
	if len(edited) == 0 && len(ran) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(recordHeading + "\n")
	for _, p := range edited {
		b.WriteString(recordEdited + p + "\n")
	}
	for _, r := range ran {
		b.WriteString(r + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// earlierRecord returns the edited paths and command lines of the exact
// record in a compaction summary (none for other messages).
func earlierRecord(content string) (edited, ran []string) {
	if !strings.HasPrefix(content, CompactedPrefix) && !strings.HasPrefix(content, TurnCompactedPrefix) {
		return nil, nil
	}
	i := strings.Index(content, recordHeading)
	if i < 0 {
		return nil, nil
	}
	for _, line := range strings.Split(content[i:], "\n")[1:] {
		switch {
		case strings.HasPrefix(line, recordEdited):
			edited = append(edited, strings.TrimPrefix(line, recordEdited))
		case strings.HasPrefix(line, recordRan), strings.HasPrefix(line, "- ("):
			ran = append(ran, line)
		case strings.TrimSpace(line) == "":
		default:
			return edited, ran // the record ended
		}
	}
	return edited, ran
}

// commandOutcome summarizes a run_command result: "exit 0", "exit 1",
// "killed: timed out after 30s", "background proc-2", "not run", or "error".
func commandOutcome(res string) string {
	switch {
	case strings.HasPrefix(res, "exit_code: "):
		line, rest, _ := strings.Cut(res, "\n")
		out := "exit " + strings.TrimPrefix(line, "exit_code: ")
		if strings.HasPrefix(rest, "[killed:") {
			if note, _, ok := strings.Cut(strings.TrimPrefix(rest, "["), "]"); ok {
				out = note
			}
		}
		return out
	case strings.HasPrefix(res, "Started proc-"):
		id, _, _ := strings.Cut(strings.TrimPrefix(res, "Started "), " ")
		return "background " + id
	case strings.HasPrefix(res, "Note: the user edited the command"):
		if i := strings.Index(res, "exit_code: "); i >= 0 {
			return commandOutcome(res[i:]) + " (edited by the user)"
		}
		return "run (edited by the user)"
	case strings.HasPrefix(res, "Error:"):
		return "error"
	}
	return "not run"
}

func oneLineClip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return tools.TruncateUTF8(s, n) + "…"
	}
	return s
}

// elideToolOutputs shortens large tool results in history[from:], oldest
// first, until the history's raw estimate is at most target tokens; the two
// most recent results are always kept. The full results move to the output
// store, so the model can read them back with read_output instead of
// re-running a tool (which, for a command, could repeat its side effects).
// It returns the new history and how many results it shortened.
func (a *Agent) elideToolOutputs(history []llm.Message, from, target int) ([]llm.Message, int) {
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
		id := a.outputs.put(out[i].Content)
		out[i].Content = fmt.Sprintf("%s %d bytes of %s output moved out of the context as %s; read_output(%q) returns any part of it.]\n%s",
			elidedPrefix, len(out[i].Content), out[i].ToolName, id, id, tools.TruncateUTF8(out[i].Content, 400))
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
			if strings.HasPrefix(m.Content, TurnCompactedPrefix) {
				b.WriteString("EARLIER PROGRESS SUMMARY: " + clip(m.Content, 8000) + "\n\n")
				continue
			}
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
