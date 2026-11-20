package tui

// Turn handling: the TUI side of an agent turn. The agent (internal/agent)
// runs the loop; this file turns its events into display updates and
// collects the user's answers to approval requests.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/clitorhea/sagittarius-astar.git/internal/agent"
	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
	"github.com/clitorhea/sagittarius-astar.git/internal/logger"
	"github.com/clitorhea/sagittarius-astar.git/internal/sandbox"
	"github.com/clitorhea/sagittarius-astar.git/internal/tools"
)

// renderInterval caps how often the streaming preview is re-rendered, so
// cost per second is bounded regardless of token rate.
const renderInterval = 33 * time.Millisecond

// agentEventMsg delivers one event of the turn identified by turnID.
type agentEventMsg struct {
	turnID int
	ev     agent.Event
}

// agentClosedMsg reports that a turn's event channel closed.
type agentClosedMsg struct{ turnID int }

// renderTickMsg triggers a deferred streaming re-render.
type renderTickMsg struct{}

// streamState holds per-response streaming buffers. It lives behind a
// pointer because Bubble Tea copies the Model by value and strings.Builder
// panics when copied after use.
type streamState struct {
	split     llm.ThinkSplitter
	native    strings.Builder // provider reasoning channel
	baseLen   int             // len(outputLines) when baseCache was built
	baseCache string
}

func newStreamState() *streamState { return &streamState{baseLen: -1} }

func (s *streamState) reasoning() string { return s.native.String() + s.split.Reasoning() }

// waitForEvent relays the next agent event into the Bubble Tea loop.
func waitForEvent(turnID int, ch <-chan agent.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return agentClosedMsg{turnID: turnID}
		}
		return agentEventMsg{turnID: turnID, ev: ev}
	}
}

// startTurn runs the agent on the current history (which ends with the
// user's message).
func (m *Model) startTurn() tea.Cmd {
	// Pick up a changed API key / endpoint, rebuilding only when it changed.
	if fp := endpointFingerprint(m.activeProvider, m.activeModel); fp != "" && fp != m.providerFingerprint {
		if p, err := m.buildProvider(m.activeProvider, m.activeModel); err == nil {
			m.agent.SetProvider(p)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.cancelTurn = cancel
	m.cancelling = false
	m.turnID++
	m.events = m.agent.Run(ctx, m.history)
	m.stream = newStreamState()
	m.appState = stateStreaming
	m.textarea.Blur()
	m.refreshViewportStreaming()
	return tea.Batch(waitForEvent(m.turnID, m.events), m.spinner.Tick)
}

// cancelRunningTurn asks the agent to stop. The turn ends with a DoneEvent.
func (m *Model) cancelRunningTurn() {
	if m.cancelTurn == nil || m.cancelling {
		return
	}
	m.cancelling = true
	m.cancelTurn()
	if m.approval != nil {
		m.approval = nil
	}
	m.cmdTextarea.Blur()
	m.sudoTextInput.Blur()
	m.appendOutput(errorStyle.Render("✗ Cancelling…"))
	m.refreshViewport()
}

// handleAgentEvent applies one agent event and re-arms the relay.
func (m Model) handleAgentEvent(msg agentEventMsg) (Model, tea.Cmd) {
	if msg.turnID != m.turnID || m.events == nil {
		return m, nil // stale turn
	}
	next := waitForEvent(m.turnID, m.events)

	switch ev := msg.ev.(type) {
	case agent.StreamStartEvent:
		m.stream = newStreamState()
		m.isDeliberating = false
		m.appState = stateStreaming
		m.currentTool = nil
		m.refreshViewportStreaming()
		return m, tea.Batch(next, m.spinner.Tick)

	case agent.TokenEvent:
		if ev.Reasoning {
			m.stream.native.WriteString(ev.Text)
		} else {
			m.stream.split.Write(ev.Text)
		}
		m.isDeliberating = m.stream.split.InThink() || (m.stream.native.Len() > 0 && m.stream.split.Content() == "")
		if time.Since(m.lastRender) >= renderInterval {
			m.refreshViewportStreaming()
			return m, next
		}
		if !m.renderPending {
			m.renderPending = true
			return m, tea.Batch(next, tea.Tick(renderInterval, func(time.Time) tea.Msg { return renderTickMsg{} }))
		}
		return m, next

	case agent.MessageEvent:
		m.history = append(m.history, ev.Msg)
		m.saveSession()
		if ev.Msg.Role == llm.RoleAssistant {
			m.renderAssistant(ev.Msg)
			m.stream = newStreamState()
			m.refreshViewport()
		}
		return m, next

	case agent.ToolStartEvent:
		e := ev
		m.currentTool = &e
		m.stdin = ev.Stdin
		m.appState = stateExecutingTool
		m.execStart = time.Now()
		m.execTimeout = 60 * time.Second
		if v, ok := ev.Call.Args["timeout_seconds"].(float64); ok && v > 0 {
			m.execTimeout = time.Duration(v) * time.Second
		} else if ev.Call.Name == "run_command" {
			m.execTimeout = sandbox.DefaultTimeout
		}
		m.appendOutput(toolStartLine(ev))
		m.refreshViewport()
		return m, tea.Batch(next, m.spinner.Tick)

	case agent.ToolResultEvent:
		m.stdin = nil
		m.currentTool = nil
		if m.appState == stateRuntimeInput {
			m.runtimeInput.Blur()
			m.appState = stateExecutingTool
		}
		m.renderToolResult(ev)
		m.refreshViewport()
		return m, next

	case agent.ApprovalEvent:
		if m.cancelling {
			ev.Req.Deny()
			return m, next
		}
		m.approval = ev.Req
		m.currentTool = nil
		m.renderApproval(ev.Req)
		m.textarea.Blur()
		switch ev.Req.Kind {
		case agent.ApproveCommand:
			m.appState = stateConfirmingCmd
			m.cmdTextarea.SetValue(ev.Req.Command)
			m.cmdTextarea.Focus()
		case agent.ApproveSudoPassword:
			m.appState = stateInputSudoPassword
			m.sudoTextInput.Reset()
			m.sudoTextInput.Focus()
		case agent.ApproveWrite:
			m.appState = stateConfirmingWrite
			m.cmdTextarea.SetValue("y")
			m.cmdTextarea.Focus()
		default:
			m.appState = stateConfirmingTool
			m.cmdTextarea.SetValue("y")
			m.cmdTextarea.Focus()
		}
		m.refreshViewport()
		return m, next

	case agent.NoticeEvent:
		style := lipgloss.NewStyle().Foreground(colorSubtext)
		switch ev.Level {
		case agent.NoticeWarn:
			style = lipgloss.NewStyle().Foreground(colorYellow)
		case agent.NoticeError:
			style = errorStyle
		}
		m.appendOutput(style.Render("⚠ " + ev.Text))
		m.refreshViewport()
		return m, next

	case agent.DoneEvent:
		return m.finishTurn(ev)
	}
	return m, next
}

// finishTurn handles the end of a turn and returns to input.
func (m Model) finishTurn(ev agent.DoneEvent) (Model, tea.Cmd) {
	m.events = nil
	m.cancelTurn = nil
	m.cancelling = false
	m.approval = nil
	m.stdin = nil
	m.currentTool = nil
	m.stream = newStreamState()
	m.isDeliberating = false

	switch {
	case ev.Err != nil:
		m.lastErr = ev.Err.Error()
		logger.L.Error("turn failed", "error", ev.Err)
		m.appendOutput(errorStyle.Render("✗ Error: " + m.lastErr))
	case ev.Aborted:
		m.appendOutput(errorStyle.Render("✗ Turn cancelled."))
	}

	var titleCmd tea.Cmd
	if ev.Err == nil && !ev.Aborted && m.activeSession != nil && m.activeSession.Name == "Untitled" && !m.titleGenerated {
		m.titleGenerated = true
		titleCmd = m.generateSessionTitleCmd()
	}
	m.resetToInput()
	return m, titleCmd
}

// resetToInput returns to the prompt.
func (m *Model) resetToInput() {
	m.appState = stateInput
	m.cmdTextarea.Blur()
	m.sudoTextInput.Blur()
	m.runtimeInput.Blur()
	m.textarea.Focus()
	m.refreshViewport()
}

// ── rendering ────────────────────────────────────────────────────────────────

func (m *Model) renderAssistant(msg llm.Message) {
	if msg.Content == "" && msg.ReasoningContent == "" {
		return
	}
	m.outputLines = append(m.outputLines, assistantLabelStyle.Render("aig"))
	if r := strings.TrimSpace(msg.ReasoningContent); r != "" {
		m.outputLines = append(m.outputLines, thinkingStyle.Render(r))
	}
	if msg.Content != "" {
		rendered, err := m.renderer.Render(msg.Content)
		if err != nil {
			rendered = msg.Content
		}
		m.outputLines = append(m.outputLines, strings.TrimRight(rendered, "\n"))
	}
	m.outputLines = append(m.outputLines, divider(m.width))
}

func toolIcon(name string) string {
	switch {
	case name == "run_command":
		return "⚙"
	case tools.IsWriteTool(name):
		if name == "write_file" {
			return "💾"
		}
		return "✏️ "
	case strings.HasPrefix(name, "mcp__"):
		return "🔌"
	default:
		return "⚙"
	}
}

func toolStartLine(ev agent.ToolStartEvent) string {
	style := lipgloss.NewStyle().Foreground(colorYellow)
	switch {
	case ev.Call.Name == "run_command":
		prefix := "⚡ run_command"
		if ev.Auto {
			prefix += " (auto)"
		}
		return style.Render(fmt.Sprintf("%s: %s", prefix, ev.Command))
	case tools.IsWriteTool(ev.Call.Name):
		path, _ := ev.Call.Args["path"].(string)
		return style.Render("✍  Writing " + path + "…")
	default:
		return style.Render(fmt.Sprintf("%s Tool: %s", toolIcon(ev.Call.Name), ev.Call.Name))
	}
}

func (m *Model) renderToolResult(ev agent.ToolResultEvent) {
	name := ev.Call.Name
	switch {
	case ev.Denied:
		m.appendOutput(confirmStyle.Render(fmt.Sprintf("✗ %s skipped: %s", name, ev.Result)))
	case ev.Err != nil:
		m.appendOutput(errorStyle.Render(fmt.Sprintf("✗ Tool %s failed", name)))
		m.appendOutput(errorStyle.Render("  " + tools.TruncateUTF8(ev.Err.Error(), 2000)))
	default:
		m.appendOutput(lipgloss.NewStyle().Foreground(colorGreen).Render(fmt.Sprintf("✓ Tool %s done", name)))
		if name == "run_command" {
			m.appendOutput(execResultStyle.Render(ev.Result))
		}
		if i := strings.Index(ev.Result, "Post-edit check"); i >= 0 {
			style := lipgloss.NewStyle().Foreground(colorSubtext)
			if strings.Contains(ev.Result[i:], "FAILED") {
				style = errorStyle
			}
			m.appendOutput(style.Render("  " + strings.ReplaceAll(ev.Result[i:], "\n", "\n  ")))
		}
	}
}

// renderApproval prints the context the user needs to decide.
func (m *Model) renderApproval(req *agent.ApprovalRequest) {
	hint := lipgloss.NewStyle().Foreground(colorSubtext)
	switch req.Kind {
	case agent.ApproveCommand:
		m.appendOutput(confirmStyle.Render("🔧 run_command: " + req.Command))
		m.appendOutput(hint.Render("Enter to execute · edit the command if needed · Esc to skip · Ctrl+C to cancel the turn"))
	case agent.ApproveSudoPassword:
		m.appendOutput(confirmStyle.Render("🔒 Sudo password required for: " + req.Command))
	case agent.ApproveWrite:
		pw := req.Write
		m.appendOutput(lipgloss.NewStyle().Foreground(colorYellow).Bold(true).Render(
			fmt.Sprintf("%s %s: %s", toolIcon(req.Call.Name), req.Call.Name, pw.Path)))
		for _, note := range pw.Notes {
			m.appendOutput(hint.Render("  ℹ " + note))
		}
		if pw.Diff != "" && pw.Diff != "(no changes)" {
			m.appendOutput(hint.Render("Diff preview:"))
			m.appendOutput(codeBlockStyle.Render(pw.Diff))
		} else {
			preview := pw.Content
			if len(preview) > 400 {
				preview = tools.TruncateUTF8(preview, 400) + "\n…(truncated)"
			}
			m.appendOutput(hint.Render(fmt.Sprintf("Content preview (%d bytes):", len(pw.Content))))
			m.appendOutput(codeBlockStyle.Render(preview))
		}
	default:
		m.appendOutput(confirmStyle.Render("⚠ " + req.Title))
		args, _ := json.MarshalIndent(req.Call.Args, "", "  ")
		argStr := tools.TruncateUTF8(string(args), 1500)
		if len(argStr) < len(args) {
			argStr += "\n…(truncated)"
		}
		m.appendOutput(codeBlockStyle.Render(argStr))
		m.appendOutput(hint.Render("Enter to allow · Esc to deny · /approve-tools on to stop asking"))
	}
}

// restoreTurnOutput re-adds in-flight turn state after a history re-render
// (window resize). It returns true when it already refreshed the viewport.
func (m *Model) restoreTurnOutput() bool {
	switch m.appState {
	case stateStreaming:
		m.stream.baseLen = -1
		m.refreshViewportStreaming()
		return true
	case stateConfirmingCmd, stateConfirmingWrite, stateConfirmingTool, stateInputSudoPassword:
		if m.approval != nil {
			m.renderApproval(m.approval)
		}
	case stateExecutingTool, stateRuntimeInput:
		if m.currentTool != nil {
			m.appendOutput(toolStartLine(*m.currentTool))
		}
	}
	return false
}

// refreshViewportStreaming shows the committed output plus the live response.
// The committed part is cached while it doesn't change, and the live part is
// maintained incrementally, so each refresh is cheap.
func (m *Model) refreshViewportStreaming() {
	m.lastRender = time.Now()
	m.renderPending = false
	s := m.stream
	if s == nil {
		s = newStreamState()
		m.stream = s
	}
	if s.baseLen != len(m.outputLines) {
		s.baseCache = strings.Join(m.outputLines, "\n")
		s.baseLen = len(m.outputLines)
	}

	var b strings.Builder
	b.WriteString(s.baseCache)
	if s.baseCache != "" {
		b.WriteString("\n")
	}
	b.WriteString(assistantLabelStyle.Render("aig"))
	if r := strings.TrimRight(s.reasoning(), "\n"); r != "" {
		b.WriteString("\n" + thinkingStyle.Render(r))
	}
	if c := strings.TrimRight(s.split.Content(), "\n"); c != "" {
		b.WriteString("\n\n" + c)
	} else if s.native.Len() == 0 && s.split.Reasoning() == "" {
		b.WriteString(" " + spinnerStyle.Render("…"))
	}
	m.viewport.SetContent(b.String())
	m.viewport.GotoBottom()
}

// ── keys ─────────────────────────────────────────────────────────────────────

// handleTurnKey handles keys while a turn is running.
func (m Model) handleTurnKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	if msg.Type == tea.KeyCtrlC {
		m.cancelRunningTurn()
		return m, nil
	}
	if m.appState != stateRuntimeInput {
		if handled, cmd := m.handleScrollKeys(msg); handled {
			return m, cmd
		}
	}

	switch m.appState {
	case stateStreaming:
		return m, nil

	case stateExecutingTool:
		if msg.Type == tea.KeyCtrlI && m.stdin != nil {
			m.appState = stateRuntimeInput
			m.runtimeInput.Reset()
			m.runtimeInput.Focus()
		}
		return m, nil

	case stateRuntimeInput:
		switch msg.Type {
		case tea.KeyEsc:
			m.runtimeInput.Blur()
			m.appState = stateExecutingTool
			return m, nil
		case tea.KeyEnter:
			line := m.runtimeInput.Value()
			m.runtimeInput.Reset()
			m.runtimeInput.Blur()
			m.appState = stateExecutingTool
			if w := m.stdin; w != nil {
				m.appendOutput(lipgloss.NewStyle().Foreground(colorSubtext).Render("> " + line))
				m.refreshViewport()
				return m, func() tea.Msg {
					_, _ = fmt.Fprintln(w, line)
					return nil
				}
			}
			return m, nil
		}
		var cmd tea.Cmd
		m.runtimeInput, cmd = m.runtimeInput.Update(msg)
		return m, cmd

	case stateConfirmingCmd:
		switch msg.Type {
		case tea.KeyEsc:
			m.answer(func(r *agent.ApprovalRequest) { r.Deny() }, "✗ Command skipped.")
			return m, nil
		case tea.KeyEnter:
			cmdText := strings.TrimSpace(m.cmdTextarea.Value())
			if cmdText == "" {
				return m, nil
			}
			m.answer(func(r *agent.ApprovalRequest) { r.Approve(cmdText) }, "⚡ Executing…")
			return m, nil
		}
		var cmd tea.Cmd
		m.cmdTextarea, cmd = m.cmdTextarea.Update(msg)
		return m, cmd

	case stateConfirmingWrite, stateConfirmingTool:
		switch msg.Type {
		case tea.KeyEsc:
			m.answer(func(r *agent.ApprovalRequest) { r.Deny() }, "✗ Rejected.")
			return m, nil
		case tea.KeyEnter:
			v := strings.ToLower(strings.TrimSpace(m.cmdTextarea.Value()))
			if v == "y" || v == "yes" {
				m.answer(func(r *agent.ApprovalRequest) { r.Approve("") }, "")
			} else {
				m.answer(func(r *agent.ApprovalRequest) { r.Deny() }, "✗ Rejected.")
			}
			return m, nil
		}
		var cmd tea.Cmd
		m.cmdTextarea, cmd = m.cmdTextarea.Update(msg)
		return m, cmd

	case stateInputSudoPassword:
		switch msg.Type {
		case tea.KeyEsc:
			m.sudoTextInput.Reset()
			m.answer(func(r *agent.ApprovalRequest) { r.Deny() }, "✗ Command cancelled (sudo password required).")
			return m, nil
		case tea.KeyEnter:
			pw := m.sudoTextInput.Value()
			m.sudoTextInput.Reset()
			m.answer(func(r *agent.ApprovalRequest) { r.ProvidePassword(pw) }, "")
			return m, nil
		}
		var cmd tea.Cmd
		m.sudoTextInput, cmd = m.sudoTextInput.Update(msg)
		return m, cmd
	}
	return m, nil
}

// answer replies to the pending approval and waits for the agent's next event.
func (m *Model) answer(reply func(*agent.ApprovalRequest), note string) {
	if m.approval != nil {
		reply(m.approval)
		m.approval = nil
	}
	m.cmdTextarea.Blur()
	m.sudoTextInput.Blur()
	m.appState = stateExecutingTool
	m.execStart = time.Now()
	if note != "" {
		m.appendOutput(confirmStyle.Render(note))
	}
	m.refreshViewport()
}

// handleScrollKeys applies viewport scrolling shortcuts. It reports whether
// the key was consumed.
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

// viewTurnInput renders the input area while a turn is running.
func (m Model) viewTurnInput() string {
	hint := lipgloss.NewStyle().Foreground(colorSubtext)
	switch m.appState {
	case stateConfirmingCmd:
		return inputActiveStyle.Render(lipgloss.JoinVertical(lipgloss.Left,
			confirmStyle.Render("Review command (Enter to run, Esc to skip, Shift+Enter for newline, Ctrl+C to cancel the turn):"),
			m.cmdTextarea.View()))
	case stateConfirmingWrite:
		path := ""
		if m.approval != nil && m.approval.Write != nil {
			path = m.approval.Write.Path
		}
		return inputActiveStyle.Render(lipgloss.JoinVertical(lipgloss.Left,
			confirmStyle.Render(fmt.Sprintf("💾 Write to %s? Type 'y' and Enter to confirm, Esc to reject:", path)),
			m.cmdTextarea.View()))
	case stateConfirmingTool:
		return inputActiveStyle.Render(lipgloss.JoinVertical(lipgloss.Left,
			confirmStyle.Render("Allow this tool call? Type 'y' and Enter to allow, Esc to deny:"),
			m.cmdTextarea.View()))
	case stateInputSudoPassword:
		return inputActiveStyle.Render(lipgloss.JoinVertical(lipgloss.Left,
			confirmStyle.Render("🔒 This command requires sudo. Enter password (Esc to cancel):"),
			m.sudoTextInput.View()))
	case stateRuntimeInput:
		return inputActiveStyle.Render(lipgloss.JoinVertical(lipgloss.Left,
			hint.Render("⌨️  Type a line to send to the running process (Enter to send · Esc to return)"),
			m.runtimeInput.View()))
	case stateExecutingTool:
		elapsed := time.Since(m.execStart).Seconds()
		if m.cancelling {
			return inputStyle.Render(m.spinner.View() + " " + hint.Render("cancelling…"))
		}
		if m.currentTool == nil {
			return inputStyle.Render(m.spinner.View() + " " + hint.Render("working…"))
		}
		icon, desc := "⚙", "executing tool: "+m.currentTool.Call.Name+"…"
		if m.currentTool.Call.Name == "run_command" {
			icon, desc = m.getExecutionIndicator(m.currentTool.Command)
			c := m.currentTool.Command
			if len(c) > 50 {
				c = tools.TruncateUTF8(c, 47) + "..."
			}
			desc += " " + c
		}
		keys := "Ctrl+C to cancel"
		if m.stdin != nil {
			keys = "Ctrl+I to send stdin · " + keys
		}
		return inputStyle.Render(lipgloss.JoinVertical(lipgloss.Left,
			fmt.Sprintf("%s %s %s (elapsed: %.1fs)", m.spinner.View(), icon, desc, elapsed),
			hint.Render(keys)))
	}
	return ""
}

// agentRootForTest exposes the working directory tests run in.
func (m Model) agentRootForTest() string {
	wd, _ := os.Getwd()
	return wd
}
