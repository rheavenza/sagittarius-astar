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
	"github.com/clitorhea/sagittarius-astar.git/internal/summary"
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
	Provider       llm.Provider
	MCP            *mcp.Registry // may be nil
	WorkspaceRoot  string        // reads inside it run without approval
	ContextBlock   string        // appended to the system prompt of each request
	PostEditChecks map[string]string
	// ProjectChecks is the project's definition of done (.aig/config.json);
	// when it has checks, the run_checks tool is offered.
	ProjectChecks    tools.ProjectChecks
	MaxContextTokens int
	// Checkpoints, if set, snapshots the workspace before the first change
	// of each turn (write, command, or MCP call) so it can be undone.
	Checkpoints Checkpointer
}

// Checkpointer saves restorable workspace state (see internal/checkpoint).
type Checkpointer interface {
	// Checkpoint records the current workspace before a turn changes it.
	Checkpoint(label string) error
	// RecordWrite remembers a file's content before an agent write.
	RecordWrite(path, original string, existed bool)
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
	projectChecks  tools.ProjectChecks
	summaryRoot    string // repo root whose /summarize index project_overview reads
	maxTokens      int
	checkpoints    Checkpointer

	mode     atomic.Int32 // Mode
	planMode atomic.Bool
}

// Mode is the permission mode: how much the agent may do without asking.
type Mode int32

const (
	// ModeDefault asks before file writes, commands, MCP tools, web_fetch
	// and reads outside the workspace.
	ModeDefault Mode = iota
	// ModeAcceptEdits applies file edits inside the workspace without
	// asking; everything else behaves like ModeDefault.
	ModeAcceptEdits
	// ModeAuto runs everything without asking, except writes outside the
	// workspace and destructive commands (tools.DangerousCommand).
	ModeAuto
)

func (m Mode) String() string {
	switch m {
	case ModeAcceptEdits:
		return "accept-edits"
	case ModeAuto:
		return "auto"
	default:
		return "default"
	}
}

// ParseMode accepts "default"/"ask", "accept-edits"/"edits", "auto"/"yolo".
func ParseMode(s string) (Mode, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "default", "ask", "normal", "off":
		return ModeDefault, true
	case "accept-edits", "acceptedits", "accept_edits", "edits":
		return ModeAcceptEdits, true
	case "auto", "yolo", "on":
		return ModeAuto, true
	}
	return ModeDefault, false
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
		projectChecks:  o.ProjectChecks,
		summaryRoot:    summaryRootFor(o.WorkspaceRoot),
		maxTokens:      o.MaxContextTokens,
		checkpoints:    o.Checkpoints,
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

func summaryRootFor(workspaceRoot string) string {
	if workspaceRoot == "" {
		return ""
	}
	return summary.RootFor(workspaceRoot)
}

// indexDefs returns project_overview while the project has a /summarize
// index (checked per request, so a /summarize run mid-session enables it).
func (a *Agent) indexDefs() []llm.Tool {
	if a.summaryRoot != "" && summary.Exists(a.summaryRoot) {
		return []llm.Tool{tools.ProjectOverviewDefinition()}
	}
	return nil
}

// contextBlockSnapshot returns the context block (without plan directive).
func (a *Agent) contextBlockSnapshot() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.contextBlock
}

// SetMode sets the permission mode (takes effect on the next tool call).
func (a *Agent) SetMode(m Mode) { a.mode.Store(int32(m)) }

// Mode returns the permission mode.
func (a *Agent) Mode() Mode { return Mode(a.mode.Load()) }

// SetAutoApprove switches between ModeAuto (on) and ModeDefault (off).
func (a *Agent) SetAutoApprove(on bool) {
	if on {
		a.SetMode(ModeAuto)
	} else {
		a.SetMode(ModeDefault)
	}
}

// AutoApprove reports whether the agent is in ModeAuto.
func (a *Agent) AutoApprove() bool { return a.Mode() == ModeAuto }

// SetPlanMode toggles plan mode: the model may only use read-only tools and
// is asked to produce a plan instead of making changes. Applies from the
// next turn.
func (a *Agent) SetPlanMode(on bool) { a.planMode.Store(on) }

// PlanMode reports whether plan mode is on.
func (a *Agent) PlanMode() bool { return a.planMode.Load() }

// planDirective is appended to the system prompt in plan mode.
const planDirective = `# Plan mode (active)
The user enabled plan mode. Investigate as needed with read-only tools (read_file, find_files, search_regex, repo_map, explore, list_directory, map_workspace, web_search, web_fetch), but do NOT modify files, run commands, or call MCP tools — those calls are blocked.
Then reply with a concise, numbered implementation plan:
1. the files you will change and what changes in each,
2. how you will verify the result (build, tests, manual checks),
3. risks, assumptions and open questions.
The user will review the plan and approve it with /go before anything is changed.`

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
		planMode:     a.planMode.Load(),
	}
	if t.planMode {
		t.contextBlock = strings.TrimSpace(t.contextBlock + "\n\n" + planDirective)
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

	planMode     bool
	checkpointed bool // a checkpoint was taken this turn

	// sub marks an explore subagent turn: it never asks for approval and
	// only gets toolDefs.
	sub bool
	// toolDefs, if non-nil, replaces the normal tool set.
	toolDefs []llm.Tool
}

// ensureCheckpoint snapshots the workspace before the first change of the
// turn. Failure is reported but doesn't block the change.
func (t *turn) ensureCheckpoint() {
	cp := t.a.checkpoints
	if cp == nil || t.checkpointed {
		return
	}
	t.checkpointed = true
	label := "agent turn"
	for i := len(t.history) - 1; i >= 0; i-- {
		if t.history[i].Role == llm.RoleUser {
			label = t.history[i].Content
			break
		}
	}
	if err := cp.Checkpoint(label); err != nil {
		logger.L.Warn("agent: checkpoint failed", "error", err)
		t.emit(NoticeEvent{Level: NoticeWarn, Text: "Checkpoint failed (/undo won't cover this turn): " + err.Error()})
		return
	}
	t.emit(NoticeEvent{Level: NoticeInfo, Text: "📌 Checkpoint saved — /undo reverts this turn's changes"})
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
	var defs []llm.Tool
	switch {
	case t.toolDefs != nil:
		defs = t.toolDefs
	default:
		defs = append(tools.Definitions(), t.a.indexDefs()...)
		if len(t.a.projectChecks.Checks) > 0 {
			defs = append(defs, tools.RunChecksDefinition(t.a.projectChecks))
		}
		if t.planMode {
			defs = readOnlyDefs(defs)
		} else if t.a.mcp != nil {
			defs = append(defs, t.a.mcp.LLMTools()...)
		}
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

// readOnlyDefs keeps only tools that cannot change anything (plan mode).
func readOnlyDefs(defs []llm.Tool) []llm.Tool {
	out := defs[:0:0]
	for _, d := range defs {
		if r, ok := tools.RiskOf(d.Name); ok && (r == tools.RiskRead || r == tools.RiskNetwork) {
			out = append(out, d)
		}
	}
	return out
}
