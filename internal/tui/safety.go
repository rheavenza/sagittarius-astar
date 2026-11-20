package tui

// Safety-net commands: /undo, /redo, /diff, /checkpoints (workspace
// checkpoints) and /plan, /go (plan mode).

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/clitorhea/sagittarius-astar.git/internal/checkpoint"
	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
)

// maxDiffLines caps how much of /diff is shown in the viewport.
const maxDiffLines = 400

// planApprovedPrompt is sent on /go.
const planApprovedPrompt = "The plan is approved. Implement it now, following the plan. Verify your changes as you go (build/tests) and tell me what you verified."

// SetCheckpoints wires the checkpoint manager used by /undo, /redo, /diff.
func (m *Model) SetCheckpoints(cp *checkpoint.Manager) {
	m.checkpoints = cp
	m.syncCheckpointSession()
}

// syncCheckpointSession points the checkpoint manager at the active session.
func (m *Model) syncCheckpointSession() {
	if m.checkpoints != nil && m.activeSession != nil {
		m.checkpoints.SetSession(m.activeSession.ID)
	}
}

// handleSafetyCommand handles the safety-net slash commands. ok is false if
// input isn't one of them.
func (m Model) handleSafetyCommand(input string) (Model, tea.Cmd, bool) {
	cmd, arg, _ := strings.Cut(input, " ")
	arg = strings.TrimSpace(arg)
	ok := lipgloss.NewStyle().Foreground(colorGreen)
	sub := lipgloss.NewStyle().Foreground(colorSubtext)

	switch cmd {
	case "/undo", "/redo":
		if m.checkpoints == nil {
			m.appendOutput(errorStyle.Render("✗ Checkpoints are not available."))
			break
		}
		var res *checkpoint.Result
		var err error
		if cmd == "/undo" {
			res, err = m.checkpoints.Undo()
		} else {
			res, err = m.checkpoints.Redo()
		}
		if err != nil {
			m.appendOutput(errorStyle.Render("✗ " + err.Error()))
			break
		}
		verb := "Undid"
		if cmd == "/redo" {
			verb = "Redid"
		}
		m.appendOutput(ok.Render(fmt.Sprintf("↩ %s changes of turn: %q", verb, res.Checkpoint.Label)))
		if !res.Changed() {
			m.appendOutput(sub.Render("  (no files differed)"))
		}
		for _, f := range res.Restored {
			m.appendOutput(sub.Render("  restored " + f))
		}
		for _, f := range res.Deleted {
			m.appendOutput(sub.Render("  deleted  " + f))
		}
		if res.Changed() {
			// Keep the model's picture of the workspace accurate.
			m.history = append(m.history, llm.Message{Role: llm.RoleSystem, Content: undoNote(cmd, res)})
			m.saveSession()
		}
		if cmd == "/undo" && m.checkpoints.Mode() == "journal" {
			m.appendOutput(sub.Render("  note: not a git repository — only files written by aig's edit tools were restored, not changes made by shell commands"))
		}

	case "/diff":
		if m.checkpoints == nil {
			m.appendOutput(errorStyle.Render("✗ Checkpoints are not available."))
			break
		}
		diff, err := m.checkpoints.Diff()
		if err != nil {
			m.appendOutput(errorStyle.Render("✗ " + err.Error()))
			break
		}
		m.appendOutput(lipgloss.NewStyle().Foreground(colorAccent).Bold(true).Render("Changes since this session's first checkpoint:"))
		m.appendOutput(renderDiff(diff))

	case "/checkpoints":
		if m.checkpoints == nil {
			m.appendOutput(errorStyle.Render("✗ Checkpoints are not available."))
			break
		}
		list := m.checkpoints.List()
		if len(list) == 0 {
			m.appendOutput(sub.Render("No checkpoints in this session yet (one is taken before the agent's first change in each turn)."))
			break
		}
		m.appendOutput(lipgloss.NewStyle().Foreground(colorAccent).Bold(true).Render(
			fmt.Sprintf("Checkpoints (%s mode, newest first — /undo restores the top one):", m.checkpoints.Mode())))
		for _, c := range list {
			when := ""
			if !c.Time.IsZero() {
				when = c.Time.Format("15:04:05") + "  "
			}
			m.appendOutput(sub.Render(fmt.Sprintf("  #%-3d %s%s", c.Seq, when, c.Label)))
		}
		if m.checkpoints.CanRedo() {
			m.appendOutput(sub.Render("  (/redo available)"))
		}

	case "/plan":
		switch arg {
		case "off", "0", "no":
			m.agent.SetPlanMode(false)
			m.appendOutput(ok.Render("✓ Plan mode off — the agent can make changes again."))
		case "", "on":
			m.agent.SetPlanMode(true)
			m.appendOutput(lipgloss.NewStyle().Foreground(colorYellow).Bold(true).Render(
				"📝 Plan mode on — the agent will investigate with read-only tools and propose a plan. /go to implement, /plan off to exit."))
		default:
			// "/plan <task>": enable and send the task right away.
			m.agent.SetPlanMode(true)
			m.appendOutput(lipgloss.NewStyle().Foreground(colorYellow).Bold(true).Render("📝 Plan mode on"))
			m = m.submitPrompt(arg)
			return m, m.startTurn(), true
		}

	case "/go", "/approve-plan":
		if !m.agent.PlanMode() {
			m.appendOutput(errorStyle.Render("✗ Not in plan mode. Use /plan to have the agent propose a plan first."))
			break
		}
		m.agent.SetPlanMode(false)
		m.appendOutput(ok.Render("✓ Plan approved — implementing."))
		m = m.submitPrompt(planApprovedPrompt)
		return m, m.startTurn(), true

	default:
		return m, nil, false
	}

	m.refreshViewport()
	return m, nil, true
}

func undoNote(cmd string, res *checkpoint.Result) string {
	var b strings.Builder
	if cmd == "/undo" {
		fmt.Fprintf(&b, "The user ran /undo: the workspace was restored to its state before the turn %q.", res.Checkpoint.Label)
	} else {
		fmt.Fprintf(&b, "The user ran /redo: the changes of the turn %q were re-applied.", res.Checkpoint.Label)
	}
	if len(res.Restored) > 0 {
		b.WriteString(" Restored: " + strings.Join(res.Restored, ", ") + ".")
	}
	if len(res.Deleted) > 0 {
		b.WriteString(" Deleted: " + strings.Join(res.Deleted, ", ") + ".")
	}
	b.WriteString(" File contents you saw earlier may be outdated — re-read files before editing them.")
	return b.String()
}

// renderDiff colors a unified diff and caps its length.
func renderDiff(diff string) string {
	lines := strings.Split(diff, "\n")
	truncated := 0
	if len(lines) > maxDiffLines {
		truncated = len(lines) - maxDiffLines
		lines = lines[:maxDiffLines]
	}
	add := lipgloss.NewStyle().Foreground(colorGreen)
	del := lipgloss.NewStyle().Foreground(colorRed)
	hdr := lipgloss.NewStyle().Foreground(colorAccent)
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "+++") || strings.HasPrefix(l, "---") || strings.HasPrefix(l, "diff "):
			lines[i] = hdr.Render(l)
		case strings.HasPrefix(l, "@@"):
			lines[i] = hdr.Render(l)
		case strings.HasPrefix(l, "+"):
			lines[i] = add.Render(l)
		case strings.HasPrefix(l, "-"):
			lines[i] = del.Render(l)
		}
	}
	out := strings.Join(lines, "\n")
	if truncated > 0 {
		out += "\n" + lipgloss.NewStyle().Foreground(colorSubtext).Render(
			fmt.Sprintf("… %d more lines (run `git diff refs/aig/checkpoints/<session>/base` for the full diff)", truncated))
	}
	return out
}
