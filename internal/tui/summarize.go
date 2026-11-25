package tui

// /summarize: build the cached project summary (internal/summary) in the
// background, with a cost preview first.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/clitorhea/sagittarius-astar.git/internal/config"
	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
	"github.com/clitorhea/sagittarius-astar.git/internal/summary"
)

// summaryJob is a running /summarize. It lives behind a pointer because the
// Model is copied by value.
type summaryJob struct {
	cancel context.CancelFunc
	ch     chan tea.Msg
}

type summaryProgressMsg struct{ p summary.Progress }

type summaryDoneMsg struct {
	res *summary.Result
	err error
}

func waitSummary(ch <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

// summaryModel resolves the provider and model /summarize uses: the
// summary_model setting ("provider:model" or a model of the session's
// provider), else the session's model.
func (m *Model) summaryModel() (llm.Provider, string, error) {
	spec := strings.TrimSpace(config.LoadFileConfig().SummaryModel)
	if spec == "" {
		return m.agent.Provider(), m.activeProvider + ":" + m.activeModel, nil
	}
	name, model, ep, err := config.ResolveModelSpec(spec, config.ProviderName(m.activeProvider))
	if err != nil {
		return nil, "", err
	}
	p, err := llm.NewProvider(string(name), ep.APIKey, model, ep.BaseURL)
	if err != nil {
		return nil, "", err
	}
	return p, string(name) + ":" + model, nil
}

// handleSummarizeCommand handles /summarize [go|stop|show]. ok is false if
// input isn't /summarize.
func (m Model) handleSummarizeCommand(input string) (Model, tea.Cmd, bool) {
	cmd, arg, _ := strings.Cut(input, " ")
	if cmd != "/summarize" {
		return m, nil, false
	}
	arg = strings.TrimSpace(arg)
	sub := lipgloss.NewStyle().Foreground(colorSubtext)
	okStyle := lipgloss.NewStyle().Foreground(colorGreen)
	defer m.refreshViewport()

	cwd, err := os.Getwd()
	if err != nil {
		m.appendOutput(errorStyle.Render("✗ " + err.Error()))
		return m, nil, true
	}
	root := summary.RootFor(cwd)

	switch arg {
	case "stop":
		if m.summary == nil {
			m.appendOutput(sub.Render("No /summarize run in progress."))
		} else {
			m.summary.cancel()
			m.appendOutput(sub.Render("Stopping /summarize… (finished summaries are kept)"))
		}
		return m, nil, true

	case "show":
		ix, err := summary.Load(root)
		if err != nil || ix == nil || ix.Overview() == "" {
			m.appendOutput(sub.Render("No project summary yet. Run /summarize to create one."))
			return m, nil, true
		}
		m.appendOutput(okStyle.Render(fmt.Sprintf("📚 %s (%d files, %d directories, %s)", summary.MarkdownPath(root), len(ix.Files), len(ix.Dirs), ix.Updated.Format("2006-01-02 15:04"))))
		m.appendOutput(ix.Overview())
		return m, nil, true

	case "", "go":
		if m.summary != nil {
			m.appendOutput(errorStyle.Render("✗ /summarize is already running (/summarize stop to cancel)."))
			return m, nil, true
		}
		plan, err := summary.Scan(root)
		if err != nil {
			m.appendOutput(errorStyle.Render("✗ /summarize: " + err.Error()))
			return m, nil, true
		}
		provider, model, err := m.summaryModel()
		if err != nil {
			m.appendOutput(errorStyle.Render("✗ /summarize: summary_model: " + err.Error()))
			return m, nil, true
		}
		if plan.Files == 0 {
			m.appendOutput(sub.Render("No source files to summarize in " + root + "."))
			return m, nil, true
		}
		if plan.UpToDate {
			m.appendOutput(okStyle.Render(fmt.Sprintf("✓ Project summary is up to date (%d files). /summarize show to view it.", plan.Files)))
			return m, nil, true
		}
		if arg == "" {
			// Preview only: a run costs real tokens.
			tokens := plan.TodoBytes/4 + int64(plan.Todo)*250 // content + prompt and answer
			m.appendOutput(lipgloss.NewStyle().Foreground(colorAccent).Bold(true).Render("📚 /summarize " + root))
			m.appendOutput(fmt.Sprintf("  %d source files: %d cached, %d to summarize (≈%s tokens) with %s",
				plan.Files, plan.Cached, plan.Todo, humanTokens(tokens), model))
			if plan.Removed > 0 {
				m.appendOutput(fmt.Sprintf("  %d indexed files were removed and will be dropped", plan.Removed))
			}
			if plan.Truncated {
				m.appendOutput(sub.Render("  (only the first 5000 source files are indexed)"))
			}
			m.appendOutput(sub.Render("  Tests, generated and vendored files are skipped. Directory and project summaries add a few more requests."))
			if !strings.Contains(strings.ToLower(model), "flash") && config.LoadFileConfig().SummaryModel == "" {
				m.appendOutput(sub.Render(`  Tip: set "summary_model" in config.json (e.g. "deepseek:deepseek-v4-flash") to use a cheaper model.`))
			}
			m.appendOutput("  Run /summarize go to start · /summarize stop cancels a run")
			return m, nil, true
		}

		ctx, cancel := context.WithCancel(context.Background())
		job := &summaryJob{cancel: cancel, ch: make(chan tea.Msg, 16)}
		m.summary = job
		m.appendOutput(okStyle.Render(fmt.Sprintf("📚 Summarizing %d files with %s in the background — keep working; /summarize stop cancels.", plan.Todo, model)))
		go func() {
			defer cancel()
			last := ""
			res, err := plan.Run(ctx, provider, model, func(p summary.Progress) {
				// Report phase changes and every 25 files; drop if the UI lags.
				key := p.Phase
				if p.Phase == "files" {
					key += fmt.Sprint(p.Done / 25)
				}
				if key == last || p.Phase == "done" {
					return
				}
				last = key
				select {
				case job.ch <- summaryProgressMsg{p}:
				default:
				}
			})
			job.ch <- summaryDoneMsg{res: res, err: err}
		}()
		return m, waitSummary(job.ch), true

	default:
		m.appendOutput(errorStyle.Render("✗ Usage: /summarize [go|stop|show]"))
		return m, nil, true
	}
}

// handleSummaryMsg renders /summarize progress and completion.
func (m Model) handleSummaryMsg(msg tea.Msg) (Model, tea.Cmd) {
	sub := lipgloss.NewStyle().Foreground(colorSubtext)
	switch msg := msg.(type) {
	case summaryProgressMsg:
		p := msg.p
		switch p.Phase {
		case "files":
			line := fmt.Sprintf("  📚 files %d/%d", p.Done, p.Total)
			if p.Failed > 0 {
				line += fmt.Sprintf(" (%d failed)", p.Failed)
			}
			m.appendOutput(sub.Render(line))
		case "dirs":
			m.appendOutput(sub.Render("  📚 summarizing directories…"))
		case "project":
			m.appendOutput(sub.Render("  📚 writing the project overview…"))
		}
		m.refreshViewport()
		if m.summary != nil {
			return m, waitSummary(m.summary.ch)
		}
		return m, nil

	case summaryDoneMsg:
		m.summary = nil
		switch {
		case errors.Is(msg.err, context.Canceled):
			m.appendOutput(sub.Render(fmt.Sprintf("📚 /summarize stopped; %d file summaries saved. /summarize go resumes.", msg.res.Files)))
		case msg.err != nil:
			done := 0
			if msg.res != nil {
				done = msg.res.Files
			}
			m.appendOutput(errorStyle.Render(fmt.Sprintf("✗ /summarize failed: %v (%d file summaries saved; /summarize go resumes)", msg.err, done)))
		default:
			line := fmt.Sprintf("✓ Project summary written: %d files and %d directories summarized → %s", msg.res.Files, msg.res.Dirs, msg.res.Path)
			if msg.res.Failed > 0 {
				line += fmt.Sprintf(" (%d files failed; /summarize go retries them)", msg.res.Failed)
			}
			m.appendOutput(lipgloss.NewStyle().Foreground(colorGreen).Render(line))
			m.appendOutput(sub.Render("  The agent can use project_overview now; the overview joins the system prompt from the next session."))
		}
		m.refreshViewport()
		return m, nil
	}
	return m, nil
}

func humanTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%dk", n/1000)
	default:
		return fmt.Sprint(n)
	}
}
