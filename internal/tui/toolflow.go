package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
	"github.com/clitorhea/sagittarius-astar.git/internal/logger"
	"github.com/clitorhea/sagittarius-astar.git/internal/sandbox"
)

// Helpers for the tool-call lifecycle: launching commands, approvals,
// rejections, and keeping every tool_call answered.

// skippedToolResults builds a tool-result message for each call, so an
// assistant turn's tool_calls are always fully answered even when execution
// stops early (loop breaker, user abort). OpenAI-compatible APIs reject a
// history where any tool_call lacks a matching tool message.
func skippedToolResults(calls []llm.ToolCall, reason string) []llm.Message {
	out := make([]llm.Message, 0, len(calls))
	for _, c := range calls {
		out = append(out, llm.Message{
			Role:       llm.RoleTool,
			Content:    reason,
			ToolCallID: c.ID,
			ToolName:   c.Name,
		})
	}
	return out
}

// answerRemainingToolCalls appends a synthetic result for pendingToolCalls[from:].
func (m *Model) answerRemainingToolCalls(from int, reason string) {
	if from < 0 {
		from = 0
	}
	if from >= len(m.pendingToolCalls) {
		return
	}
	m.history = append(m.history, skippedToolResults(m.pendingToolCalls[from:], reason)...)
}

// fallbackSudoPassword returns the cached in-app sudo password, or "" when a
// graphical askpass helper handles sudo (then aig never pipes a password).
func (m *Model) fallbackSudoPassword() string {
	if sandbox.AskpassHelper() != "" {
		return ""
	}
	return m.sudoPassword
}

// seedStdin writes the sudo password as the first stdin line without blocking
// the caller (io.Pipe writes block until the child reads; the write is
// released with an error once the read end is closed).
func seedStdin(w *io.PipeWriter, password string) {
	if password == "" {
		return
	}
	go func() { _, _ = fmt.Fprintln(w, password) }()
}

// startRunCommand launches an approved run_command tool call with the
// (possibly user-edited) command and returns the tea.Cmd that delivers its
// toolCallResultMsg. All three launch sites (auto-approve, confirm, after sudo
// prompt) go through here.
func (m *Model) startRunCommand(call llm.ToolCall, command string) tea.Cmd {
	m.appState = stateExecutingTool
	m.execStart = time.Now()
	m.execTimeout = sandbox.DefaultTimeout
	if v, ok := call.Args["timeout_seconds"].(float64); ok && v > 0 {
		m.execTimeout = time.Duration(v) * time.Second
		if m.execTimeout > sandbox.MaxTimeout {
			m.execTimeout = sandbox.MaxTimeout
		}
	}
	m.refreshViewport()

	// Copy args so the edited command doesn't silently rewrite the assistant's
	// tool call stored in history; tell the model about edits instead.
	original, _ := call.Args["command"].(string)
	callCopy := call
	callCopy.Args = make(map[string]any, len(call.Args))
	for k, v := range call.Args {
		callCopy.Args[k] = v
	}
	callCopy.Args["command"] = command
	edited := original != command

	ctx, cancel := context.WithCancel(context.Background())
	m.cancelExec = cancel
	pipeR, pipeW := io.Pipe()
	m.stdinPipe = pipeW
	password := m.fallbackSudoPassword()
	seedStdin(pipeW, password)

	return tea.Batch(func() tea.Msg {
		out, err := ExecuteRunCommand(ctx, callCopy, password, pipeR)
		cancel()
		_ = pipeR.Close()
		if err != nil {
			return toolCallResultMsg{CallID: callCopy.ID, Name: callCopy.Name, Error: err}
		}
		if edited {
			out = fmt.Sprintf("Note: the user edited the command before running it. Executed:\n%s\n\n%s", command, out)
		}
		return toolCallResultMsg{CallID: callCopy.ID, Name: callCopy.Name, Result: out}
	}, m.spinner.Tick)
}

// rejectPendingWrite cancels a proposed write_file/edit_file and still answers
// the tool call so the conversation stays valid.
func (m *Model) rejectPendingWrite() tea.Cmd {
	callID, name := m.pendingWriteCallID, m.pendingWriteName
	m.pendingWrite = nil
	m.pendingWriteCallID = ""
	m.pendingWriteName = ""
	m.cmdTextarea.Blur()
	m.appendOutput(confirmStyle.Render("✗ Write cancelled."))

	if m.pendingToolIdx >= len(m.pendingToolCalls) {
		m.resetToInput()
		return nil
	}
	m.appState = stateExecutingTool
	m.refreshViewport()
	return func() tea.Msg {
		return toolCallResultMsg{CallID: callID, Name: name,
			Result: "The user rejected this file change. Do not retry it without asking the user."}
	}
}

// askToolApproval switches to stateConfirmingTool for call.
func (m *Model) askToolApproval(call llm.ToolCall, title string) tea.Cmd {
	c := call
	m.pendingConfirmCall = &c
	m.appState = stateConfirmingTool

	args, _ := json.MarshalIndent(call.Args, "", "  ")
	argStr := truncateUTF8(string(args), 1500)
	if len(argStr) < len(args) {
		argStr += "\n…(truncated)"
	}
	m.appendOutput(confirmStyle.Render(title))
	m.appendOutput(codeBlockStyle.Render(argStr))
	m.appendOutput(lipgloss.NewStyle().Foreground(colorSubtext).Render(
		"Enter to allow · Esc to deny · /approve-tools on to stop asking"))

	m.cmdTextarea.SetValue("y")
	m.textarea.Blur()
	m.cmdTextarea.Focus()
	m.refreshViewport()
	return nil
}

// runApprovedTool executes a call the user just approved in stateConfirmingTool.
func (m *Model) runApprovedTool(call llm.ToolCall) tea.Cmd {
	if m.mcpRegistry != nil && m.mcpRegistry.IsMCPTool(call.Name) {
		return m.mcpToolCallCmd(call)
	}
	return m.runNativeTool(call)
}

// denyTool answers a denied tool call so the batch continues.
func (m *Model) denyTool(call llm.ToolCall) tea.Cmd {
	logger.L.Info("tool: user denied", "name", call.Name)
	m.appendOutput(confirmStyle.Render("✗ Tool call denied: " + call.Name))
	m.appState = stateExecutingTool
	m.refreshViewport()
	return func() tea.Msg {
		return toolCallResultMsg{CallID: call.ID, Name: call.Name,
			Result: "The user denied this tool call. Do not retry it without asking the user."}
	}
}

// handleScrollKeys applies viewport scrolling shortcuts shared by the
// confirmation states. It reports whether the key was consumed.
func (m *Model) handleScrollKeys(msg tea.KeyMsg) (bool, tea.Cmd) {
	switch {
	case msg.Type == tea.KeyPgUp:
		m.viewport.LineUp(m.viewport.Height)
	case msg.Type == tea.KeyCtrlU:
		m.viewport.LineUp(m.viewport.Height / 2)
	case msg.Type == tea.KeyPgDown:
		m.viewport.LineDown(m.viewport.Height)
	case msg.Type == tea.KeyCtrlD:
		m.viewport.LineDown(m.viewport.Height / 2)
	case msg.String() == "alt+up":
		m.viewport.LineUp(1)
	case msg.String() == "alt+down":
		m.viewport.LineDown(1)
	default:
		return false, nil
	}
	return true, nil
}
