package agent

import (
	"io"
	"sync"

	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
	"github.com/clitorhea/sagittarius-astar.git/internal/tools"
)

// Event is something that happened during a turn. Front-ends (the TUI, a
// future headless mode) consume events from Agent.Run in order; the channel
// is closed after the final DoneEvent.
type Event interface{ agentEvent() }

// StreamStartEvent marks the start of a model response (one per round).
type StreamStartEvent struct{}

// TokenEvent is a streamed chunk. Reasoning chunks come from the provider's
// native reasoning channel; inline <think> tags arrive as normal text.
type TokenEvent struct {
	Text      string
	Reasoning bool
}

// MessageEvent carries a message that was appended to the conversation
// (assistant turns, tool results, system notes). Appending these, in order,
// to the front-end's history keeps it identical to the agent's.
type MessageEvent struct {
	Msg llm.Message
}

// ToolStartEvent is sent right before a tool executes (after approval).
type ToolStartEvent struct {
	Call llm.ToolCall
	// Command is the command actually run (possibly edited by the user).
	Command string
	// Stdin, when non-nil, feeds the running command's stdin. It is closed by
	// the agent when the command finishes.
	Stdin io.Writer
	// Auto is true when the call ran without asking (auto-approve or a
	// read-only tool inside the workspace).
	Auto bool
}

// ToolResultEvent reports a finished tool call (for display; the history
// message follows as a MessageEvent).
type ToolResultEvent struct {
	Call   llm.ToolCall
	Result string
	Err    error
	Denied bool // the user rejected the call
}

// ApprovalEvent asks the front-end to approve something. The agent blocks
// until the request is answered or the turn is cancelled.
type ApprovalEvent struct {
	Req *ApprovalRequest
}

// NoticeLevel is the severity of a NoticeEvent.
type NoticeLevel int

const (
	NoticeInfo NoticeLevel = iota
	NoticeWarn
	NoticeError
)

// NoticeEvent is a message for the user (loop breaker fired, sudo cache
// cleared, …). It is not part of the conversation.
type NoticeEvent struct {
	Level NoticeLevel
	Text  string
}

// HistoryEvent replaces the conversation history: the agent compacted it.
// Front-ends must adopt History as their history (it already contains every
// MessageEvent of the turn so far).
type HistoryEvent struct {
	History []llm.Message
	Reason  string
}

// UsageEvent reports a request's token usage against the context window.
type UsageEvent struct {
	Usage    llm.Usage
	Reported bool // false: InputTokens is aig's estimate, not the API's count
	Window   int
}

// DoneEvent is always the last event of a turn.
type DoneEvent struct {
	Err     error // provider/stream failure
	Aborted bool  // cancelled by the user
}

func (StreamStartEvent) agentEvent() {}
func (HistoryEvent) agentEvent()     {}
func (UsageEvent) agentEvent()       {}
func (TokenEvent) agentEvent()       {}
func (MessageEvent) agentEvent()     {}
func (ToolStartEvent) agentEvent()   {}
func (ToolResultEvent) agentEvent()  {}
func (ApprovalEvent) agentEvent()    {}
func (NoticeEvent) agentEvent()      {}
func (DoneEvent) agentEvent()        {}

// ApprovalKind says what is being approved.
type ApprovalKind int

const (
	// ApproveCommand: run_command. The reply may carry an edited command.
	ApproveCommand ApprovalKind = iota
	// ApproveWrite: a file change; Write holds the diff.
	ApproveWrite
	// ApproveTool: an MCP tool, web_fetch, or a read outside the workspace.
	ApproveTool
	// ApproveSudoPassword: the command needs sudo and no graphical askpass
	// helper is available; the reply carries the password.
	ApproveSudoPassword
)

// ApprovalRequest is answered exactly once via Approve, ProvidePassword or Deny.
type ApprovalRequest struct {
	Kind    ApprovalKind
	Call    llm.ToolCall
	Title   string
	Command string              // ApproveCommand / ApproveSudoPassword
	Write   *tools.PendingWrite // ApproveWrite
	Risk    tools.Risk

	reply chan ApprovalReply
	once  sync.Once
}

// ApprovalReply is the front-end's answer.
type ApprovalReply struct {
	Approved bool
	Command  string // edited command (ApproveCommand); "" keeps the original
	Password string // ApproveSudoPassword
}

func newRequest(r *ApprovalRequest) *ApprovalRequest {
	r.reply = make(chan ApprovalReply, 1)
	return r
}

func (r *ApprovalRequest) send(rep ApprovalReply) {
	r.once.Do(func() { r.reply <- rep })
}

// Approve approves the request. For commands, command may be an edited
// version ("" keeps the original).
func (r *ApprovalRequest) Approve(command string) {
	r.send(ApprovalReply{Approved: true, Command: command})
}

// ProvidePassword answers an ApproveSudoPassword request.
func (r *ApprovalRequest) ProvidePassword(password string) {
	r.send(ApprovalReply{Approved: true, Password: password})
}

// Deny rejects the request.
func (r *ApprovalRequest) Deny() {
	r.send(ApprovalReply{Approved: false})
}
