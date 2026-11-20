package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/clitorhea/sagittarius-astar.git/internal/config"
	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
)

// modelListMsg delivers the result of an async model-list fetch.
type modelListMsg struct {
	provider string
	filter   string
	live     []string // model IDs from the endpoint (NIM); nil for static providers
	err      error
}

// endpointFingerprint identifies the exact client configuration for a
// provider/model, so the client is rebuilt only when something changed.
// It returns "" when the provider has no usable credentials.
func endpointFingerprint(provider, model string) string {
	ep, err := config.ResolveEndpoint(config.NormalizeProvider(provider))
	if err != nil {
		return ""
	}
	return strings.Join([]string{provider, model, ep.BaseURL, ep.APIKey}, "|")
}

// buildProvider constructs a client for provider/model using the current
// config + environment (API key, base URL) and records its fingerprint.
func (m *Model) buildProvider(provider, model string) (llm.Provider, error) {
	name := config.NormalizeProvider(provider)
	ep, err := config.ResolveEndpoint(name)
	if err != nil {
		return nil, err
	}
	p, err := llm.NewProvider(string(name), ep.APIKey, model, ep.BaseURL)
	if err != nil {
		return nil, err
	}
	m.providerFingerprint = strings.Join([]string{string(name), model, ep.BaseURL, ep.APIKey}, "|")
	return p, nil
}

// openModelPicker starts building the /model picker. For NIM it fetches the
// live catalog from the endpoint (hosted or self-hosted) in the background;
// other providers use the built-in list immediately.
func (m *Model) openModelPicker(filter string) tea.Cmd {
	prov := config.NormalizeProvider(m.activeProvider)
	if prov != config.ProviderNIM {
		return func() tea.Msg { return modelListMsg{provider: string(prov), filter: filter} }
	}

	ep, err := config.ResolveEndpoint(prov)
	if err != nil {
		return func() tea.Msg { return modelListMsg{provider: string(prov), filter: filter, err: err} }
	}
	base := ep.BaseURL
	if base == "" {
		base = llm.NIMBaseURL
	}
	m.appendOutput(lipgloss.NewStyle().Foreground(colorSubtext).Render("⏳ Fetching models from " + base + " …"))
	m.refreshViewport()

	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		ids, err := llm.ListOpenAICompatModels(ctx, ep.APIKey, base)
		return modelListMsg{provider: string(prov), filter: filter, live: ids, err: err}
	}
}

// buildModelItems merges the built-in models with a live list (deduplicated,
// built-ins first) and applies a case-insensitive substring filter.
func buildModelItems(known [][2]string, live []string, filter, active string) []selectionItem {
	filter = strings.ToLower(filter)
	match := func(s string) bool { return filter == "" || strings.Contains(strings.ToLower(s), filter) }

	seen := map[string]bool{}
	var items []selectionItem
	add := func(id, label string) {
		if seen[id] || !match(id+" "+label) {
			return
		}
		seen[id] = true
		if id == active {
			label += " (active)"
		}
		items = append(items, selectionItem{ID: id, Label: label})
	}
	for _, pair := range known {
		add(pair[0], fmt.Sprintf("%s (%s)", pair[1], pair[0]))
	}
	for _, id := range live {
		add(id, id)
	}
	return items
}

// showModelPicker opens the selection list once the model list is ready.
func (m Model) showModelPicker(msg modelListMsg) Model {
	// Ignore stale results (provider switched, or the user moved on).
	if msg.provider != string(config.NormalizeProvider(m.activeProvider)) || m.appState != stateInput {
		return m
	}
	if msg.err != nil {
		m.appendOutput(errorStyle.Render("⚠ Could not fetch the live model list: " + msg.err.Error()))
		m.appendOutput(lipgloss.NewStyle().Foreground(colorSubtext).Render(
			"  Showing built-in models. You can still switch to any model with /model <id>."))
	}

	items := buildModelItems(config.KnownModels[config.ProviderName(msg.provider)], msg.live, msg.filter, m.activeModel)
	if len(items) == 0 {
		if msg.filter != "" {
			m.appendOutput(lipgloss.NewStyle().Foreground(colorSubtext).Render(
				fmt.Sprintf("  (no models match %q)", msg.filter)))
		} else {
			m.appendOutput(lipgloss.NewStyle().Foreground(colorSubtext).Render("  (no known models for this provider)"))
		}
		m.refreshViewport()
		return m
	}
	if len(msg.live) > 0 {
		m.appendOutput(lipgloss.NewStyle().Foreground(colorSubtext).Render(
			fmt.Sprintf("  %d models available — narrow with /models <filter>, e.g. /models llama", len(items))))
	}

	m.selectionItems = items
	m.selectionIdx = 0
	for i, it := range items {
		if it.ID == m.activeModel {
			m.selectionIdx = i
			break
		}
	}
	m.selectionType = "model"
	m.appState = stateSelecting
	m.textarea.Blur()
	m.refreshViewport()
	return m
}
