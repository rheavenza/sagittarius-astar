// Package agent runs aig's agent loop independently of any UI: it streams a
// model response, executes the requested tools (asking for approval where the
// policy requires it), feeds results back, and repeats until the model gives
// a final answer, the user cancels, or a safety limit trips.
//
// Front-ends drive it with Run and consume the returned Event stream.
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
	"github.com/clitorhea/sagittarius-astar.git/internal/logger"
	"github.com/clitorhea/sagittarius-astar.git/internal/mcp"
	"github.com/clitorhea/sagittarius-astar.git/internal/tools"
)

const (
	// DefaultMaxContextTokens is the default history budget per request.
	DefaultMaxContextTokens = 100_000
	// MaxToolRounds bounds model↔tool round trips in a single turn.
	MaxToolRounds = 50
	// breakerLimit is how many identical calls / consecutive errors trip a
	// loop breaker.
	breakerLimit = 3
)

// Options configures an Agent.
type Options struct {
	Provider         llm.Provider
	MCP              *mcp.Registry // may be nil
	WorkspaceRoot    string        // reads inside it run without approval
	ContextBlock     string        // appended to the system prompt of each request
	PostEditChecks   map[string]string
	MaxContextTokens int
}

// Agent holds session-level state shared by turns. It is safe for concurrent
// use, but only one turn should run at a time.
type Agent struct {
	mu             sync.Mutex
	provider       llm.Provider
	contextBlock   string
	sudoPassword   string
	mcp            *mcp.Registry
	workspaceRoot  string
	postEditChecks map[string]string
	maxTokens      int

	autoApprove atomic.Bool
}

// New creates an Agent.
func New(o Options) *Agent {
	if o.MaxContextTokens <= 0 {
		o.MaxContextTokens = DefaultMaxContextTokens
	}
	return &Agent{
		provider:       o.Provider,
		contextBlock:   strings.TrimSpace(o.ContextBlock),
		mcp:            o.MCP,
		workspaceRoot:  o.WorkspaceRoot,
		postEditChecks: o.PostEditChecks,
		maxTokens:      o.MaxContextTokens,
	}
}

// SetProvider swaps the model client (takes effect on the next turn).
func (a *Agent) SetProvider(p llm.Provider) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.provider = p
}

// Provider returns the current model client.
func (a *Agent) Provider() llm.Provider {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.provider
}

// SetContextBlock sets the environment/instructions block appended to the
// system prompt of every request.
func (a *Agent) SetContextBlock(s string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.contextBlock = strings.TrimSpace(s)
}

// SetAutoApprove toggles running commands and sensitive tools without asking.
// File writes always ask.
func (a *Agent) SetAutoApprove(on bool) { a.autoApprove.Store(on) }

// AutoApprove reports the auto-approve setting.
func (a *Agent) AutoApprove() bool { return a.autoApprove.Load() }

func (a *Agent) cachedSudoPassword() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sudoPassword
}

func (a *Agent) setSudoPassword(p string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sudoPassword = p
}

// Run starts one turn. history must end with the new user message. Events are
// delivered in order on the returned channel, which is closed after the
// final DoneEvent. Cancel ctx to abort the turn; every pending tool call is
// still answered so the conversation stays valid.
func (a *Agent) Run(ctx context.Context, history []llm.Message) <-chan Event {
	ch := make(chan Event, 64)
	a.mu.Lock()
	t := &turn{
		a:            a,
		provider:     a.provider,
		contextBlock: a.contextBlock,
		history:      append([]llm.Message(nil), history...),
		ch:           ch,
	}
	a.mu.Unlock()

	go func() {
		defer close(ch)
		if t.provider == nil {
			t.emit(DoneEvent{Err: fmt.Errorf("no model provider configured")})
			return
		}
		t.run(ctx)
	}()
	return ch
}

// turn is the state of one Run.
type turn struct {
	a            *Agent
	provider     llm.Provider
	contextBlock string
	history      []llm.Message
	ch           chan<- Event

	lastName, lastHash string
	sameCount          int
	errCount           int
}

func (t *turn) emit(e Event) { t.ch <- e }

func (t *turn) appendMsg(m llm.Message) {
	t.history = append(t.history, m)
	t.emit(MessageEvent{Msg: m})
}

func (t *turn) appendToolResult(call llm.ToolCall, content string) {
	t.appendMsg(llm.Message{Role: llm.RoleTool, Content: content, ToolCallID: call.ID, ToolName: call.Name})
}

// answer gives every call a synthetic result, so no tool_call is left without
// a tool message (OpenAI-compatible APIs reject that with HTTP 400).
func (t *turn) answer(calls []llm.ToolCall, reason string) {
	for _, m := range SkippedToolResults(calls, reason) {
		t.appendMsg(m)
	}
}

func (t *turn) run(ctx context.Context) {
	for round := 0; ; round++ {
		if round >= MaxToolRounds {
			t.emit(NoticeEvent{Level: NoticeWarn, Text: fmt.Sprintf(
				"Stopped after %d tool rounds in one turn. Send a message to continue.", MaxToolRounds)})
			t.emit(DoneEvent{})
			return
		}

		text, reasoning, calls, err := t.stream(ctx)
		if err != nil {
			if ctx.Err() != nil {
				t.emit(DoneEvent{Aborted: true})
				return
			}
			t.emit(DoneEvent{Err: err})
			return
		}

		if len(calls) == 0 {
			t.appendMsg(llm.Message{Role: llm.RoleAssistant, Content: text, ReasoningContent: reasoning})
			t.emit(DoneEvent{})
			return
		}

		t.appendMsg(llm.Message{Role: llm.RoleAssistant, Content: text, ReasoningContent: reasoning, ToolCalls: calls})
		if stop := t.runCalls(ctx, calls); stop {
			return
		}
	}
}

// stream sends the conversation to the model and relays tokens.
func (t *turn) stream(ctx context.Context) (text, reasoning string, calls []llm.ToolCall, err error) {
	pruned := llm.PruneHistory(t.history, t.a.maxTokens)
	outgoing := WithContextBlock(pruned, t.contextBlock)
	defs := tools.Definitions()
	if t.a.mcp != nil {
		defs = append(defs, t.a.mcp.LLMTools()...)
	}

	type result struct {
		calls     []llm.ToolCall
		reasoning string
		err       error
	}
	tokenCh := make(chan string, 64)
	done := make(chan result, 1)
	go func() {
		c, r, e := t.provider.StreamChat(ctx, outgoing, defs, tokenCh)
		done <- result{c, r, e}
	}()

	t.emit(StreamStartEvent{})
	var split llm.ThinkSplitter
	var native strings.Builder
	for tok := range tokenCh {
		if strings.HasPrefix(tok, "\x00") {
			native.WriteString(tok[1:])
			t.emit(TokenEvent{Text: tok[1:], Reasoning: true})
			continue
		}
		split.Write(tok)
		t.emit(TokenEvent{Text: tok})
	}
	res := <-done
	split.Flush()
	if res.err != nil {
		return "", "", nil, res.err
	}

	// Reasoning the provider returned must be stored verbatim (DeepSeek
	// requires it echoed back); otherwise keep what we saw streamed.
	reasoning = res.reasoning
	if reasoning == "" {
		reasoning = native.String() + split.Reasoning()
	}
	return split.Content(), reasoning, res.calls, nil
}

// runCalls executes a batch of tool calls. It returns true when the turn is
// over (aborted); false means "stream again".
func (t *turn) runCalls(ctx context.Context, calls []llm.ToolCall) bool {
	for i, call := range calls {
		h := hashArguments(call.Args)
		if call.Name == t.lastName && h == t.lastHash {
			t.sameCount++
		} else {
			t.sameCount, t.lastName, t.lastHash = 1, call.Name, h
		}
		if t.sameCount >= breakerLimit {
			t.answer(calls[i:], "Skipped: loop breaker blocked a repeated identical call.")
			t.appendMsg(llm.Message{Role: llm.RoleSystem, Content: fmt.Sprintf(
				"Loop breaker: you called %q with the same arguments %d times in a row, so the call was blocked. "+
					"Stop repeating it; re-check your assumptions (paths, arguments, earlier output) and choose a different approach.",
				call.Name, breakerLimit)})
			t.emit(NoticeEvent{Level: NoticeWarn, Text: fmt.Sprintf("Loop breaker: blocked %d identical calls to %s", breakerLimit, call.Name)})
			t.sameCount, t.lastName, t.lastHash = 0, "", ""
			return false
		}

		out := t.execute(ctx, call)

		if ctx.Err() != nil {
			content := out.content()
			if content == "" {
				content = "Aborted by user."
			}
			t.emit(ToolResultEvent{Call: call, Result: out.result, Err: out.err, Denied: out.denied})
			t.appendToolResult(call, content)
			t.answer(calls[i+1:], "Aborted by user before execution.")
			t.emit(DoneEvent{Aborted: true})
			return true
		}

		t.emit(ToolResultEvent{Call: call, Result: out.result, Err: out.err, Denied: out.denied})
		t.appendToolResult(call, out.content())

		if out.err != nil {
			t.errCount++
		} else {
			t.errCount = 0
		}
		if t.errCount >= breakerLimit {
			t.answer(calls[i+1:], "Skipped: loop breaker halted execution after consecutive errors.")
			t.appendMsg(llm.Message{Role: llm.RoleSystem, Content: fmt.Sprintf(
				"Loop breaker: the last %d tool calls failed, so the remaining calls in this batch were skipped. "+
					"Read the errors, fix the cause (syntax, paths, arguments) and reconsider your approach before trying again.",
				breakerLimit)})
			t.emit(NoticeEvent{Level: NoticeWarn, Text: fmt.Sprintf("Loop breaker: %d consecutive tool errors", breakerLimit)})
			t.errCount = 0
			return false
		}
	}
	return false
}

// SkippedToolResults builds a tool-result message for each call.
func SkippedToolResults(calls []llm.ToolCall, reason string) []llm.Message {
	out := make([]llm.Message, 0, len(calls))
	for _, c := range calls {
		out = append(out, llm.Message{Role: llm.RoleTool, Content: reason, ToolCallID: c.ID, ToolName: c.Name})
	}
	return out
}

// WithContextBlock returns a copy of history whose first system message has
// block appended (or a new leading system message if there is none). The
// original history is not modified, so saved sessions stay clean.
func WithContextBlock(history []llm.Message, block string) []llm.Message {
	if block == "" {
		return history
	}
	out := make([]llm.Message, len(history))
	copy(out, history)
	if len(out) > 0 && out[0].Role == llm.RoleSystem {
		out[0].Content = strings.TrimSpace(out[0].Content) + "\n\n" + block
		return out
	}
	return append([]llm.Message{{Role: llm.RoleSystem, Content: block}}, out...)
}

// hashArguments returns a stable hash of tool arguments (encoding/json sorts
// map keys).
func hashArguments(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	b, err := json.Marshal(args)
	if err != nil {
		logger.L.Debug("agent: cannot hash tool args", "error", err)
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
