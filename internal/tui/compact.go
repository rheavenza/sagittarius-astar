package tui

// /compact: summarize the conversation so far (all but the last exchange)
// to free context, like the automatic compaction at ~80% of the window.

import (
	"context"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/clitorhea/sagittarius-astar.git/internal/config"
	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
)

type compactDoneMsg struct {
	history []llm.Message
	before  int // messages before
	err     error
}

func (m Model) handleCompactCommand(input string) (Model, tea.Cmd, bool) {
	if input != "/compact" {
		return m, nil, false
	}
	defer m.refreshViewport()
	if m.events != nil {
		m.appendOutput(errorStyle.Render("✗ Wait for the current turn to finish before /compact."))
		return m, nil, true
	}
	if m.compacting {
		m.appendOutput(errorStyle.Render("✗ /compact is already running."))
		return m, nil, true
	}
	m.agent.SetContextWindow(llm.ContextWindow(m.activeModel, config.LoadFileConfig().ContextWindows))
	m.compacting = true
	m.appendOutput(lipgloss.NewStyle().Foreground(colorSubtext).Render("📦 Compacting the conversation…"))
	hist := append([]llm.Message(nil), m.history...)
	ag := m.agent
	return m, func() tea.Msg {
		out, err := ag.Compact(context.Background(), hist)
		return compactDoneMsg{history: out, before: len(hist), err: err}
	}, true
}

func (m Model) handleCompactDone(msg compactDoneMsg) (Model, tea.Cmd) {
	m.compacting = false
	defer m.refreshViewport()
	if msg.err != nil {
		m.appendOutput(errorStyle.Render("✗ /compact: " + msg.err.Error()))
		return m, nil
	}
	if m.events != nil || len(m.history) != msg.before {
		// The user started a turn meanwhile; its history is newer.
		m.appendOutput(errorStyle.Render("✗ /compact result discarded: the conversation changed while it ran."))
		return m, nil
	}
	before := llm.EstimateHistoryTokens(m.history)
	m.loadHistory(msg.history, false)
	m.saveSession()
	after := llm.EstimateHistoryTokens(m.history)
	m.ctxUsed = 0 // unknown until the next request
	m.appendOutput(lipgloss.NewStyle().Foreground(colorGreen).Render(fmt.Sprintf("✓ Compacted: ~%s → ~%s tokens of history.", kTok(before), kTok(after))))
	return m, nil
}
