package tui

// Layout: the frame is header / conversation (+ scrollbar) / status bar /
// input box. Heights are measured from what is actually rendered, so the
// frame is always exactly the terminal height — a frame taller than the
// terminal makes Bubble Tea leave stale lines behind (duplicated status bar
// and input box).

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"

	"github.com/clitorhea/sagittarius-astar.git/internal/agent"
	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
)

// minViewportHeight keeps some conversation visible on tiny terminals.
const minViewportHeight = 3

// View renders the full TUI.
func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "Loading…"
	}
	header := m.headerView()
	body := lipgloss.JoinHorizontal(lipgloss.Top, m.viewport.View(), scrollbarView(m.viewport))
	frame := lipgloss.JoinVertical(lipgloss.Left, header, body, m.statusBar(), m.inputView())
	return clampFrame(frame, m.height, lipgloss.Height(header))
}

// clampFrame guarantees the frame fits the terminal by dropping conversation
// lines (just below the header) if anything is still too tall.
func clampFrame(frame string, height, headerLines int) string {
	lines := strings.Split(frame, "\n")
	if len(lines) <= height {
		return frame
	}
	excess := len(lines) - height
	if headerLines+excess > len(lines) {
		return strings.Join(lines[len(lines)-height:], "\n")
	}
	lines = append(lines[:headerLines], lines[headerLines+excess:]...)
	return strings.Join(lines, "\n")
}

func (m Model) fit(s string) string {
	return lipgloss.NewStyle().MaxWidth(m.width).Render(s)
}

// convDivider is a rule as wide as the conversation area (the terminal minus
// the scrollbar column), so it never wraps.
func (m Model) convDivider() string {
	if m.viewport.Width > 0 {
		return divider(m.viewport.Width)
	}
	return divider(max(m.width-1, 10))
}

func (m Model) headerView() string {
	return m.fit(strings.TrimRight(headerBanner(m.appVersion), "\n")) + "\n" + divider(m.width)
}

// activeBox / idleBox draw the input area at exactly the terminal width, so
// long content wraps inside the box instead of overflowing.
func (m Model) activeBox(s string) string {
	return inputActiveStyle.Width(max(m.width-2, 10)).Render(s)
}

func (m Model) idleBox(s string) string {
	return inputStyle.Width(max(m.width-2, 10)).Render(s)
}

// inputView renders the area under the status bar for the current state.
func (m Model) inputView() string {
	switch m.appState {
	case stateInput:
		if line := m.mentionLine(); line != "" {
			return m.activeBox(m.textarea.View() + "\n" + line)
		}
		return m.activeBox(m.textarea.View())
	case stateStreaming:
		label := "streaming…"
		if m.cancelling {
			label = "cancelling…"
		} else if m.isDeliberating {
			label = "thinking…"
		}
		return m.idleBox(m.spinner.View() + " " + lipgloss.NewStyle().Foreground(colorSubtext).Render(label+"  (Ctrl+C to cancel)"))
	case stateConfirmingCmd, stateConfirmingWrite, stateConfirmingTool, stateInputSudoPassword,
		stateExecutingTool, stateRuntimeInput, stateAnswering:
		return m.viewTurnInput()
	case stateSelecting:
		return m.selectionView()
	}
	return ""
}

func (m Model) selectionView() string {
	items := []string{confirmStyle.Render(fmt.Sprintf("Select %s (↑/↓ to move, Enter to select, Esc to cancel):", m.selectionType))}
	start, end := 0, len(m.selectionItems)
	if len(m.selectionItems) > 4 {
		start = max(m.selectionIdx-1, 0)
		end = start + 4
		if end > len(m.selectionItems) {
			end = len(m.selectionItems)
			start = end - 4
		}
	}
	maxLabel := max(m.width-10, 10)
	for i := start; i < end; i++ {
		label := m.selectionItems[i].Label
		if lipgloss.Width(label) > maxLabel {
			label = lipgloss.NewStyle().MaxWidth(maxLabel-1).Render(label) + "…"
		}
		marker, style := "  ", lipgloss.NewStyle().Foreground(colorText)
		if i == m.selectionIdx {
			marker, style = "▶ ", lipgloss.NewStyle().Foreground(colorGreen).Bold(true)
		}
		items = append(items, style.Render(marker+label))
	}
	return m.activeBox(lipgloss.JoinVertical(lipgloss.Left, items...))
}

// recalculateDimensions sizes the components from the measured heights of
// everything around the conversation.
func (m *Model) recalculateDimensions() {
	if m.width == 0 || m.height == 0 {
		return
	}
	m.viewport.Width = max(m.width-1, 10) // one column for the scrollbar
	m.textarea.SetWidth(max(m.width-4, 10))
	m.cmdTextarea.SetWidth(max(m.width-4, 10))
	m.sudoTextInput.Width = max(m.width-6, 10)
	m.runtimeInput.Width = max(m.width-6, 10)

	used := lipgloss.Height(m.headerView()) + lipgloss.Height(m.statusBar()) + lipgloss.Height(m.inputView())
	h := max(m.height-used, minViewportHeight)
	if h != m.viewport.Height {
		atBottom := m.viewport.AtBottom()
		m.viewport.Height = h
		if atBottom {
			m.viewport.GotoBottom()
		}
	}

	if m.renderer != nil && m.width != m.lastRendererWidth {
		if r, err := newRenderer(m.viewport.Width - 4); err == nil {
			m.renderer = r
		}
		m.lastRendererWidth = m.width
	}
}

// scrollbarView draws a one-column scrollbar for the conversation.
func scrollbarView(vp viewport.Model) string {
	h := vp.Height
	if h <= 0 {
		return ""
	}
	total := vp.TotalLineCount()
	track := lipgloss.NewStyle().Foreground(colorSurface)
	thumbStyle := lipgloss.NewStyle().Foreground(colorAccent)
	lines := make([]string, h)
	if total <= h {
		for i := range lines {
			lines[i] = " "
		}
		return strings.Join(lines, "\n")
	}
	thumb := max(h*h/total, 1)
	maxOff := total - h
	pos := 0
	if maxOff > 0 {
		pos = (vp.YOffset*(h-thumb) + maxOff/2) / maxOff
	}
	for i := range lines {
		if i >= pos && i < pos+thumb {
			lines[i] = thumbStyle.Render("┃")
		} else {
			lines[i] = track.Render("│")
		}
	}
	return strings.Join(lines, "\n")
}

// modeBadge shows the permission mode in the status bar.
func (m Model) modeBadge() string {
	if m.agent.PlanMode() {
		return lipgloss.NewStyle().Foreground(colorYellow).Bold(true).Render("📝 plan")
	}
	switch m.agent.Mode() {
	case agent.ModeAcceptEdits:
		return lipgloss.NewStyle().Foreground(colorGreen).Bold(true).Render("✎ accept edits")
	case agent.ModeAuto:
		return lipgloss.NewStyle().Foreground(colorYellow).Bold(true).Render("⚡ auto")
	}
	return lipgloss.NewStyle().Foreground(colorSubtext).Render("ask")
}

// statusBar renders a single line; lower-priority items are dropped when
// the terminal is too narrow, so it can never wrap.
func (m Model) statusBar() string {
	var statusText string
	switch m.appState {
	case stateStreaming:
		statusText = "● streaming"
		if m.isDeliberating {
			statusText = "● deliberating"
		}
	case stateConfirmingCmd, stateConfirmingTool:
		statusText = "⚠ awaiting approval"
	case stateExecutingTool, stateRuntimeInput:
		statusText = "⚙ running tool"
	case stateConfirmingWrite:
		statusText = "💾 write pending"
	case stateInputSudoPassword:
		statusText = "🔒 sudo password"
	case stateAnswering:
		statusText = "❓ question for you"
	default:
		statusText = "● ready"
	}
	sub := lipgloss.NewStyle().Foreground(colorSubtext)
	if !m.viewport.AtBottom() {
		statusText += sub.Render(" · ↓ more below (PgDn, Ctrl+End)")
	}

	session := ""
	if m.activeSession != nil {
		name := m.activeSession.Name
		if name == "" {
			name = "Untitled"
		}
		if len([]rune(name)) > 28 {
			name = string([]rune(name)[:27]) + "…"
		}
		session = "[" + name + "]"
	}

	tokens := fmt.Sprintf("~%d tokens", m.tokenEstimate)
	if m.tokenEstimate >= 1000 {
		tokens = fmt.Sprintf("~%dk tokens", m.tokenEstimate/1000)
	}
	if m.ctxWindow > 0 && m.ctxUsed > 0 {
		pct := m.ctxUsed * 100 / m.ctxWindow
		approx := ""
		if !m.ctxReported {
			approx = "~"
		}
		tokens = fmt.Sprintf("ctx %s%d%% (%s/%s)", approx, pct, kTok(m.ctxUsed), kTok(m.ctxWindow))
		if pct >= 70 {
			tokens = lipgloss.NewStyle().Foreground(colorYellow).Render(tokens)
		}
	}

	if len(m.plan) > 0 {
		done, total := agent.PlanProgress(m.plan)
		statusText += sub.Render(fmt.Sprintf(" · 📋 %d/%d", done, total))
	}
	if n := m.agent.RunningProcesses(); n > 0 {
		statusText += sub.Render(fmt.Sprintf(" · ⚙ %d bg", n))
	}

	// Right side, most important first; items are dropped from the end.
	right := []string{
		m.modeBadge() + sub.Render(" (⇧⇥)"),
		sub.Render(tokens),
		lipgloss.NewStyle().Foreground(colorOverlay).Render(m.activeProvider + "/" + m.activeModel),
		lipgloss.NewStyle().Foreground(colorAccent).Render("🎨 " + m.activePersona),
		sub.Render("? help"),
	}
	priority := []int{0, 4, 1, 2, 3} // drop order is the reverse of this
	avail := m.width - 4             // status bar padding
	build := func(keep int, withSession bool) (string, string) {
		left := statusText
		if withSession && session != "" {
			left += " · " + session
		}
		var parts []string
		for i := range right {
			for _, p := range priority[:keep] {
				if p == i {
					parts = append(parts, right[i])
				}
			}
		}
		return left, strings.Join(parts, " · ")
	}

	left, rightText := build(len(priority), true)
	for keep := len(priority); keep >= 1 && lipgloss.Width(left)+lipgloss.Width(rightText)+1 > avail; keep-- {
		left, rightText = build(keep, true)
	}
	if lipgloss.Width(left)+lipgloss.Width(rightText)+1 > avail {
		left, rightText = build(1, false)
	}
	padding := max(avail-lipgloss.Width(left)-lipgloss.Width(rightText), 1)
	line := left + strings.Repeat(" ", padding) + rightText
	return statusBarStyle.Width(m.width).MaxWidth(m.width).MaxHeight(1).Render(m.fitWidth(line, avail))
}

// fitWidth truncates s (ANSI-aware) to w cells.
func (m Model) fitWidth(s string, w int) string {
	if lipgloss.Width(s) <= w {
		return s
	}
	return lipgloss.NewStyle().MaxWidth(w).Render(s)
}

// cycleMode advances the permission mode: ask → accept edits → auto → plan → ask.
func (m *Model) cycleMode() {
	switch {
	case m.agent.PlanMode():
		m.agent.SetPlanMode(false)
		m.agent.SetMode(agent.ModeDefault)
	case m.agent.Mode() == agent.ModeDefault:
		m.agent.SetMode(agent.ModeAcceptEdits)
	case m.agent.Mode() == agent.ModeAcceptEdits:
		m.agent.SetMode(agent.ModeAuto)
	default:
		m.agent.SetMode(agent.ModeDefault)
		m.agent.SetPlanMode(true)
	}
}

// modeDescription explains a mode in one line.
func modeDescription(planMode bool, mode agent.Mode) string {
	if planMode {
		return "📝 plan — read-only investigation, then a plan; /go to implement"
	}
	switch mode {
	case agent.ModeAcceptEdits:
		return "✎ accept edits — file edits inside the workspace apply without asking; commands still ask"
	case agent.ModeAuto:
		return "⚡ auto — everything runs without asking, except writes outside the workspace and destructive commands (checkpoints still let you /undo)"
	}
	return "ask — approve every write, command and sensitive tool"
}

// updateTokenEstimate refreshes the cached history size shown in the status bar.
func (m *Model) updateTokenEstimate() {
	m.tokenEstimate = llm.EstimateHistoryTokens(m.history)
}

// kTok formats a token count compactly: 950, 45k, 1.0M.
func kTok(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%dk", (n+500)/1000)
	default:
		return fmt.Sprint(n)
	}
}
