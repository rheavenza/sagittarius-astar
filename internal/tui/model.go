// Package tui is aig's Bubble Tea front-end. The agent loop itself lives in
// internal/agent; this package renders its events and collects approvals.
//
//	INPUT ──submit──► STREAMING ⇄ EXECUTING_TOOL ──done──► INPUT
//	                       │              ▲
//	              approval needed      answered
//	                       ▼              │
//	        CONFIRMING_CMD / _WRITE / _TOOL / SUDO_PASSWORD
package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"

	"github.com/clitorhea/sagittarius-astar.git/internal/agent"
	"github.com/clitorhea/sagittarius-astar.git/internal/checkpoint"
	"github.com/clitorhea/sagittarius-astar.git/internal/config"
	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
	"github.com/clitorhea/sagittarius-astar.git/internal/logger"
	"github.com/clitorhea/sagittarius-astar.git/internal/mention"
	"github.com/clitorhea/sagittarius-astar.git/internal/sandbox"
	"github.com/clitorhea/sagittarius-astar.git/internal/session"
	"github.com/clitorhea/sagittarius-astar.git/internal/tools"
	"github.com/clitorhea/sagittarius-astar.git/internal/workspace"
)

// state represents the TUI's current operational mode.
type state int

const (
	stateInput             state = iota // Waiting for user to type and submit.
	stateStreaming                      // Model is streaming a response.
	stateConfirmingCmd                  // Awaiting approval (and optional edit) of run_command.
	stateExecutingTool                  // A tool is running, or waiting for the agent.
	stateConfirmingWrite                // Awaiting approval of a file change.
	stateSelecting                      // Selecting options (models, history, providers, personas).
	stateInputSudoPassword              // Awaiting a sudo password (no graphical askpass).
	stateRuntimeInput                   // Sending a stdin line to a running command.
	stateConfirmingTool                 // Awaiting approval of an MCP / web_fetch / out-of-workspace tool call.
	stateAnswering                      // Answering an ask_user question from the agent.
)

type selectionItem struct {
	ID    string
	Label string
}

// Model is the Bubble Tea application model for aig.
type Model struct {
	// Core state
	appState state
	history  []llm.Message // full conversation history (mirrors the agent's)
	agent    *agent.Agent

	// Running turn
	turnID      int
	events      <-chan agent.Event
	cancelTurn  context.CancelFunc
	cancelling  bool
	approval    *agent.ApprovalRequest // pending approval, if any
	currentTool *agent.ToolStartEvent  // tool currently executing
	plan        []agent.PlanItem       // the agent's task plan (update_plan)
	stdin       io.Writer              // stdin of the running command (Ctrl+I)

	// Streaming display
	stream         *streamState
	isDeliberating bool
	lastRender     time.Time
	renderPending  bool

	// Session management
	activeSession       *session.Session
	defaultSystemPrompt string
	titleGenerated      bool

	// Selection state
	selectionItems []selectionItem
	selectionIdx   int
	selectionType  string // "model", "history", "provider", "persona"

	// Provider / model identity (for status bar)
	activeProvider string
	activeModel    string
	activePersona  string

	// fileConfig is the parsed config, for persona resolution.
	fileConfig *config.FileConfig

	// checkpoints backs /undo, /redo, /diff (nil if unavailable).
	checkpoints *checkpoint.Manager
	summary     *summaryJob // running /summarize, or nil
	compacting  bool        // /compact in progress

	// providerFingerprint is provider|model|baseURL|apiKey of the live
	// provider, so the client is only rebuilt when one of those changes.
	providerFingerprint string

	// UI components
	textarea           textarea.Model
	cmdTextarea        textarea.Model
	sudoTextInput      textinput.Model
	runtimeInput       textinput.Model
	viewport           viewport.Model
	spinner            spinner.Model
	progressBar        progress.Model
	execStart          time.Time
	execTimeout        time.Duration
	autocompletePrefix string
	autocompleteIdx    int
	mentions           *mentionState

	// Layout
	width             int
	height            int
	lastRendererWidth int

	// Rendered display content
	outputLines []string

	// stickBottom forces the next viewport refresh to scroll to the end
	// (e.g. after the user sends a message). Otherwise the view only follows
	// new output when it is already at the bottom.
	stickBottom bool

	// tokenEstimate caches the history size shown in the status bar.
	tokenEstimate int
	// ctxUsed / ctxWindow: context use of the last request (UsageEvent);
	// ctxReported is false when the provider gave no counts (estimate).
	ctxUsed, ctxWindow int
	ctxReported        bool

	// Glamour renderer
	renderer *glamour.TermRenderer

	// Error message to display
	lastErr string

	// App version string
	appVersion string

	// showHelp displays the keybind/command overlay.
	showHelp bool
}

// NewModel constructs a new TUI model wired to the given LLM provider and session.
// registry may be nil when no MCP servers are configured; all features work identically.
// NewModel constructs the TUI around an agent and the active session.
func NewModel(ag *agent.Agent, activeSession *session.Session, defaultSystemPrompt string, appVersion string, providerName string, modelName string, personaName string) (*Model, error) {
	// Text area
	ta := textarea.New()
	ta.Placeholder = "Ask anything… (Enter to send, Shift+Enter for newline, /help for commands)"
	ta.Focus()
	ta.CharLimit = 8000
	ta.SetHeight(3)
	ta.ShowLineNumbers = false
	ta.KeyMap.InsertNewline.SetKeys("shift+enter")

	cta := textarea.New()
	cta.Placeholder = "Edit command before running... (Enter to execute, Esc to cancel)"
	cta.CharLimit = 8000
	cta.SetHeight(5)
	cta.ShowLineNumbers = true
	cta.KeyMap.InsertNewline.SetKeys("shift+enter")

	// Viewport
	vp := viewport.New(80, 20)

	// Spinner
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = spinnerStyle

	// Progress Bar
	pg := progress.New(progress.WithDefaultGradient())
	pg.Width = 40

	// Glamour renderer
	renderer, err := newRenderer(0)
	if err != nil {
		return nil, fmt.Errorf("tui: failed to create glamour renderer: %w", err)
	}

	ti := textinput.New()
	ti.Placeholder = "Enter sudo password..."
	ti.EchoMode = textinput.EchoPassword
	ti.EchoCharacter = '*'
	ti.CharLimit = 128

	ri := textinput.New()
	ri.Placeholder = "Type input for the running process and press Enter to send..."
	ri.CharLimit = 1024

	m := &Model{
		providerFingerprint: endpointFingerprint(providerName, modelName),
		appState:            stateInput,
		mentions:            &mentionState{},
		history:             activeSession.History,
		agent:               ag,
		stream:              newStreamState(),
		textarea:            ta,
		cmdTextarea:         cta,
		sudoTextInput:       ti,
		runtimeInput:        ri,
		viewport:            vp,
		spinner:             sp,
		progressBar:         pg,
		renderer:            renderer,
		activeSession:       activeSession,
		defaultSystemPrompt: defaultSystemPrompt,
		appVersion:          appVersion,
		activeProvider:      providerName,
		activeModel:         modelName,
		activePersona:       personaName,
		fileConfig:          config.LoadFileConfig(),
	}
	m.updateTokenEstimate()

	return m, nil
}

// ── Bubble Tea lifecycle ──────────────────────────────────────────────────────

// Init satisfies tea.Model.
func (m Model) Init() tea.Cmd {
	enableWindowsQuickEdit()
	return nil
}

// Update is the central message dispatcher.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	concreteModel, cmd := m.update(msg)
	// Heights depend on what is rendered (state, wrapped labels, status
	// bar), so re-measure after every update.
	concreteModel.recalculateDimensions()
	return concreteModel, cmd
}

func (m Model) update(msg tea.Msg) (Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {

	// ── Window Resized ─────────────────────────────────────────────────────
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.recalculateDimensions()
		m.loadHistory(m.history, true) // preserve scroll position on resize

	// ── Spinner tick ───────────────────────────────────────────────────────
	case spinner.TickMsg:
		if m.appState == stateStreaming || m.appState == stateExecutingTool {
			var spCmd tea.Cmd
			m.spinner, spCmd = m.spinner.Update(msg)
			return m, spCmd
		}

	// ── Keyboard input ─────────────────────────────────────────────────────
	case tea.KeyMsg:
		switch m.appState {
		case stateInput:
			if msg.Type != tea.KeyTab {
				m.autocompletePrefix = ""
				m.resetMentions()
			}
			switch msg.Type {
			case tea.KeyShiftTab:
				m.cycleMode()
				return m, nil
			case tea.KeyCtrlEnd:
				m.viewport.GotoBottom()
				return m, nil
			case tea.KeyCtrlHome:
				m.viewport.GotoTop()
				return m, nil
			case tea.KeyTab:
				if m.completeMention() {
					m.recalculateDimensions()
					return m, nil
				}
				currentText := m.textarea.Value()
				if strings.HasPrefix(currentText, "/") {
					var slashCommands = []string{
						"/approve-tools",
						"/checkpoints",
						"/clear",
						"/compact",
						"/diff",
						"/go",
						"/delete",
						"/exit",
						"/help",
						"/history",
						"/load",
						"/map",
						"/mode",
						"/model",
						"/models",
						"/new",
						"/persona",
						"/plan",
						"/provider",
						"/quit",
						"/redo",
						"/summarize",
						"/undo",
					}

					// Start fresh or filter based on typed prefix
					if m.autocompletePrefix == "" || !strings.HasPrefix(currentText, m.autocompletePrefix) {
						m.autocompletePrefix = currentText
						m.autocompleteIdx = 0
					} else {
						m.autocompleteIdx++
					}

					var matches []string
					for _, cmd := range slashCommands {
						if strings.HasPrefix(cmd, m.autocompletePrefix) {
							matches = append(matches, cmd)
						}
					}

					if len(matches) > 0 {
						idx := m.autocompleteIdx % len(matches)
						m.textarea.SetValue(matches[idx] + " ")
						m.textarea.CursorEnd()
					}
					return m, nil
				}
			case tea.KeyEnter:
				input := strings.TrimSpace(m.textarea.Value())
				if input == "" {
					break
				}

				if m.mentions != nil && len(m.mentions.sugg) > 0 {
					m.mentions.sugg = nil
					defer m.recalculateDimensions()
				}

				// Slash Commands (/read(file) is the old mention syntax)
				if strings.HasPrefix(input, "/") && !strings.HasPrefix(input, "/read(") {
					m.textarea.Reset()

					// Exit commands
					if input == "/quit" || input == "/exit" || input == "/q" {
						return m, tea.Quit
					}

					// Clear display: trim history to system-prompt-only, avoiding
					// orphaned tool messages that would cause API errors on next call.
					if input == "/clear" {
						m.outputLines = nil
						if len(m.history) > 0 && m.history[0].Role == llm.RoleSystem {
							m.history = m.history[:1]
						} else {
							m.history = nil
						}
						m.titleGenerated = false
						m.saveSession()
						m.refreshViewport()
						break
					}

					// Help manual
					if input == "/help" || input == "/?" {
						m.appendOutput(lipgloss.NewStyle().Foreground(colorAccent).Bold(true).Render("Commands:"))
						m.appendOutput("  /history           List all saved conversations")
						m.appendOutput("  /load <id>         Resume a conversation by its ID")
						m.appendOutput("  /save <name>       Give the current conversation a friendly name")
						m.appendOutput("  /delete <id>       Permanently delete a saved conversation")
						m.appendOutput("  /map [dir]         Inject workspace tree into context")
						m.appendOutput("  /new               Start a fresh conversation thread")
						m.appendOutput("  /clear             Clear display output (keeps system prompt)")
						m.appendOutput("  /model             List available models for current provider")
						m.appendOutput("  /model <id>        Switch active model (keeps history)")
						m.appendOutput("  /models [filter]   Pick from the provider's live model list (NIM), filtered")
						m.appendOutput("  /provider          List available providers")
						m.appendOutput("  /provider <name>   Switch active provider (keeps history)")
						m.appendOutput("  /persona           List available personas")
						m.appendOutput("  /persona <name>    Switch system prompt persona")
						m.appendOutput("  /mode [ask|edits|auto|plan]  Permission mode (Shift+Tab cycles)")
						m.appendOutput("  /approve-tools on|off        Shortcut for /mode auto | /mode ask")
						m.appendOutput("  /undo, /redo       Revert / re-apply the agent's last turn of file changes")
						m.appendOutput("  /diff              Show all changes since the session's first checkpoint")
						m.appendOutput("  /checkpoints       List workspace checkpoints")
						m.appendOutput("  /plan [task]       Plan mode: read-only investigation, then a plan (/plan off to exit)")
						m.appendOutput("  /go                Approve the plan and implement it")
						m.appendOutput("  /summarize [go|stop|show]  Cached project summary (preview cost, run, cancel, view)")
						m.appendOutput("  /compact           Summarize the conversation so far to free context (automatic at ~80%)")
						m.appendOutput("  /quit, /q          Exit the application")
						m.appendOutput(lipgloss.NewStyle().Foreground(colorAccent).Bold(true).Render("Keyboard shortcuts:"))
						m.appendOutput("  Enter              Send message")
						m.appendOutput("  Shift+Enter        Insert newline")
						m.appendOutput("  Ctrl+C             Cancel the running turn / quit")
						m.appendOutput("  Shift+Tab          Cycle permission mode: ask → accept edits → auto → plan")
						m.appendOutput("  PgUp/PgDn, wheel   Scroll the conversation (Ctrl+Home / Ctrl+End: top / bottom)")
						m.appendOutput("  Ctrl+L             Clear viewport")
						m.appendOutput("  ?                  Toggle this help overlay")
						m.appendOutput("  @path, @path:10-40  Attach a file, line range or directory (Tab completes)")
						m.refreshViewport()
						break
					}

					// Session history list
					if input == "/history" {
						sessions, err := session.List()
						if err != nil {
							m.appendOutput(errorStyle.Render("✗ Failed to list sessions: " + err.Error()))
							m.refreshViewport()
							break
						}
						if len(sessions) == 0 {
							m.appendOutput(lipgloss.NewStyle().Foreground(colorYellow).Render("No saved conversations found."))
							m.refreshViewport()
							break
						}
						m.selectionItems = nil
						m.selectionIdx = 0
						m.selectionType = "history"
						for _, s := range sessions {
							name := s.Name
							if name == "" {
								name = "Untitled"
							}
							label := fmt.Sprintf("%s · %s (%s)", s.ID, name, s.UpdatedAt.Format("2006-01-02 15:04"))
							if s.ID == m.activeSession.ID {
								label += " (active)"
							}
							m.selectionItems = append(m.selectionItems, selectionItem{
								ID:    s.ID,
								Label: label,
							})
						}
						m.appState = stateSelecting
						m.textarea.Blur()
						break
					}

					// Delete session
					if strings.HasPrefix(input, "/delete ") {
						id := strings.TrimSpace(strings.TrimPrefix(input, "/delete "))
						if id == "" {
							m.appendOutput(errorStyle.Render("✗ Please specify a session ID (e.g. /delete 20260521-120000)"))
							m.refreshViewport()
							break
						}
						if m.activeSession != nil && id == m.activeSession.ID {
							m.appendOutput(errorStyle.Render("✗ Cannot delete the active session. Start a /new session first."))
							m.refreshViewport()
							break
						}
						if err := session.Delete(id); err != nil {
							m.appendOutput(errorStyle.Render("✗ Failed to delete session: " + err.Error()))
						} else {
							m.appendOutput(lipgloss.NewStyle().Foreground(colorGreen).Render("🗑 Deleted session: " + id))
						}
						m.refreshViewport()
						break
					}

					// Create new conversation
					if input == "/new" {
						m.activeSession = &session.Session{
							ID:        session.GenerateID(),
							Name:      "Untitled",
							CreatedAt: time.Now(),
							UpdatedAt: time.Now(),
							Provider:  m.activeSession.Provider,
							Model:     m.activeSession.Model,
						}
						if m.defaultSystemPrompt != "" {
							m.activeSession.History = []llm.Message{{
								Role:    llm.RoleSystem,
								Content: m.defaultSystemPrompt,
							}}
						}
						m.history = m.activeSession.History
						m.plan = nil
						m.titleGenerated = false
						m.outputLines = nil
						m.syncCheckpointSession()
						m.saveSession()
						m.appendOutput(lipgloss.NewStyle().Foreground(colorGreen).Render("Started fresh session: " + m.activeSession.ID))
						m.refreshViewport()
						break
					}

					// Rename/Save session
					if strings.HasPrefix(input, "/save ") || strings.HasPrefix(input, "/rename ") {
						name := strings.TrimSpace(strings.TrimPrefix(input, "/save "))
						if strings.HasPrefix(input, "/rename ") {
							name = strings.TrimSpace(strings.TrimPrefix(input, "/rename "))
						}
						if name == "" {
							m.appendOutput(errorStyle.Render("✗ Please provide a name (e.g. /save my-topic)"))
							m.refreshViewport()
							break
						}
						m.activeSession.Name = name
						if err := session.Save(m.activeSession); err != nil {
							m.appendOutput(errorStyle.Render("✗ Failed to save: " + err.Error()))
						} else {
							m.appendOutput(lipgloss.NewStyle().Foreground(colorGreen).Render("Conversation renamed to: " + name))
						}
						m.refreshViewport()
						break
					}

					// Workspace map
					if strings.HasPrefix(input, "/map ") || input == "/map" {
						dir := strings.TrimSpace(strings.TrimPrefix(input, "/map"))
						if dir == "" {
							dir = "."
						}
						tree, err := workspace.MapDirectory(dir)
						if err != nil {
							m.appendOutput(errorStyle.Render("✗ Failed to map directory: " + err.Error()))
						} else {
							m.history = append(m.history, llm.Message{
								Role:    llm.RoleSystem,
								Content: fmt.Sprintf("Workspace Map for '%s':\n```\n%s\n```", dir, tree),
							})
							m.saveSession()
							m.appendOutput(lipgloss.NewStyle().Foreground(colorSubtext).Render(fmt.Sprintf("🗺️  Mapped workspace %s", dir)))
						}
						m.refreshViewport()
						break
					}

					// Load session
					if strings.HasPrefix(input, "/load") {
						id := strings.TrimSpace(strings.TrimPrefix(input, "/load"))
						if id == "" {
							sessions, err := session.List()
							if err != nil {
								m.appendOutput(errorStyle.Render("✗ Failed to list sessions: " + err.Error()))
								m.refreshViewport()
								break
							}
							if len(sessions) == 0 {
								m.appendOutput(lipgloss.NewStyle().Foreground(colorYellow).Render("No saved conversations found."))
								m.refreshViewport()
								break
							}
							m.selectionItems = nil
							m.selectionIdx = 0
							m.selectionType = "history"
							for _, s := range sessions {
								name := s.Name
								if name == "" {
									name = "Untitled"
								}
								label := fmt.Sprintf("%s · %s (%s)", s.ID, name, s.UpdatedAt.Format("2006-01-02 15:04"))
								if s.ID == m.activeSession.ID {
									label += " (active)"
								}
								m.selectionItems = append(m.selectionItems, selectionItem{
									ID:    s.ID,
									Label: label,
								})
							}
							m.appState = stateSelecting
							m.textarea.Blur()
							break
						}
						s, err := session.Load(id)
						if err != nil {
							m.appendOutput(errorStyle.Render("✗ Failed to load session: " + err.Error()))
						} else {
							m.activeSession = s
							m.syncCheckpointSession()
							m.loadHistory(s.History, false)
							m.titleGenerated = false
							m.appendOutput(lipgloss.NewStyle().Foreground(colorGreen).Render("Loaded session: " + s.ID))
						}
						m.refreshViewport()
						break
					}

					// ── /models [filter] — live model picker ───────────────────
					if input == "/models" || strings.HasPrefix(input, "/models ") {
						filter := strings.TrimSpace(strings.TrimPrefix(input, "/models"))
						cmds = append(cmds, m.openModelPicker(filter))
						break
					}

					// ── /model [name] ──────────────────────────────────────────
					if strings.HasPrefix(input, "/model") {
						arg := strings.TrimSpace(strings.TrimPrefix(input, "/model"))

						if arg == "" {
							cmds = append(cmds, m.openModelPicker(""))
							break
						}

						// Switch to the named model (keep provider, keep history)
						newProvider, err := m.buildProvider(m.activeProvider, arg)
						if err != nil {
							m.appendOutput(errorStyle.Render("✗ Failed to switch model: " + err.Error()))
						} else {
							m.agent.SetProvider(newProvider)
							m.activeModel = arg
							if m.activeSession != nil {
								m.activeSession.Model = arg
								m.saveSession()
							}
							m.appendOutput(lipgloss.NewStyle().Foreground(colorGreen).Render(
								fmt.Sprintf("✓ Switched to model: %s", arg)))
						}
						m.refreshViewport()
						break
					}

					// ── /provider [name] ───────────────────────────────────────
					if strings.HasPrefix(input, "/provider") {
						arg := strings.TrimSpace(strings.TrimPrefix(input, "/provider"))

						if arg == "" {
							m.selectionItems = nil
							m.selectionIdx = 0
							m.selectionType = "provider"
							for _, pn := range config.Providers() {
								p := string(pn)
								label := p
								if pn == config.ProviderNIM {
									label += " — NVIDIA NIM"
								}
								if p == m.activeProvider {
									label += " (active)"
								}
								m.selectionItems = append(m.selectionItems, selectionItem{
									ID:    p,
									Label: label,
								})
							}
							m.appState = stateSelecting
							m.textarea.Blur()
							break
						}

						arg = string(config.NormalizeProvider(arg))
						defaultModel := string(config.DefaultModel(config.ProviderName(arg)))
						newProvider, err := m.buildProvider(arg, defaultModel)
						if err != nil {
							m.appendOutput(errorStyle.Render("✗ Failed to switch provider: " + err.Error()))
						} else {
							m.agent.SetProvider(newProvider)
							m.activeProvider = arg
							m.activeModel = defaultModel
							if m.activeSession != nil {
								m.activeSession.Provider = arg
								m.activeSession.Model = defaultModel
								m.saveSession()
							}
							m.appendOutput(lipgloss.NewStyle().Foreground(colorGreen).Render(
								fmt.Sprintf("✓ Switched to provider: %s  model: %s", arg, defaultModel)))
						}
						m.refreshViewport()
						break
					}

					// ── /persona [name] ────────────────────────────────────────
					if strings.HasPrefix(input, "/persona") {
						arg := strings.TrimSpace(strings.TrimPrefix(input, "/persona"))

						if arg == "" {
							names := config.PersonaList(m.fileConfig)
							m.selectionItems = nil
							m.selectionIdx = 0
							m.selectionType = "persona"
							for _, name := range names {
								label := name
								if name == m.activePersona {
									label += " (active)"
								}
								m.selectionItems = append(m.selectionItems, selectionItem{
									ID:    name,
									Label: label,
								})
							}
							m.appState = stateSelecting
							m.textarea.Blur()
							break
						}

						prompt, ok := config.ResolvePersona(arg, m.fileConfig)
						if !ok {
							m.appendOutput(errorStyle.Render(fmt.Sprintf("✗ Unknown persona %q. Type /persona to list available.", arg)))
							m.refreshViewport()
							break
						}

						m.defaultSystemPrompt = prompt
						m.activePersona = arg
						// Replace or prepend the leading system message in history
						if len(m.history) > 0 && m.history[0].Role == llm.RoleSystem {
							m.history[0].Content = prompt
						} else {
							m.history = append([]llm.Message{{Role: llm.RoleSystem, Content: prompt}}, m.history...)
						}
						m.saveSession()
						m.appendOutput(lipgloss.NewStyle().Foreground(colorGreen).Render(
							fmt.Sprintf("✓ Persona switched to %q (effective next message)", arg)))
						m.refreshViewport()
						break
					}

					// ── /approve-tools on|off ──────────────────────────────────────
					if strings.HasPrefix(input, "/approve-tools") {
						arg := strings.TrimSpace(strings.TrimPrefix(input, "/approve-tools"))
						switch arg {
						case "on", "1", "yes":
							m.agent.SetAutoApprove(true)
							m.appendOutput(lipgloss.NewStyle().Foreground(colorYellow).Bold(true).Render(
								"Mode: " + modeDescription(false, agent.ModeAuto)))
						case "off", "0", "no":
							m.agent.SetAutoApprove(false)
							m.appendOutput(lipgloss.NewStyle().Foreground(colorGreen).Render(
								"Mode: " + modeDescription(false, agent.ModeDefault)))
						default:
							status := "off"
							if m.agent.AutoApprove() {
								status = "on"
							}
							m.appendOutput(lipgloss.NewStyle().Foreground(colorSubtext).Render(
								fmt.Sprintf("Auto-approve is currently: %s. Use /approve-tools on|off", status)))
						}
						m.refreshViewport()
						break
					}

					if nm, cmd, ok := m.handleCompactCommand(input); ok {
						return nm, cmd
					}
					if nm, cmd, ok := m.handleSummarizeCommand(input); ok {
						return nm, cmd
					}

					// Safety net: /undo /redo /diff /checkpoints /plan /go
					if nm, cmd, ok := m.handleSafetyCommand(input); ok {
						return nm, cmd
					}

					// Unknown command fallback
					m.appendOutput(errorStyle.Render("✗ Unknown command. Type /help to see available commands."))
					m.refreshViewport()
					break
				}

				// Standard user prompt submission
				m.textarea.Reset()
				logger.L.Info("user prompt submitted",
					"prompt_len", len(input),
					"history_turns", len(m.history),
				)
				m = m.submitPrompt(input)
				return m, m.startTurn()

			case tea.KeyCtrlC:
				return m, tea.Quit

			case tea.KeyCtrlL:
				// Clear viewport (same as /clear) without wiping history
				m.outputLines = nil
				if len(m.history) > 0 && m.history[0].Role == llm.RoleSystem {
					m.history = m.history[:1]
				} else {
					m.history = nil
				}
				m.titleGenerated = false
				m.saveSession()
				m.refreshViewport()
				return m, nil
			}

			// Viewport scroll while in input mode (explicit bindings so that
			// typing normal characters like f/b/j/k/d/u doesn't scroll).
			if msg.Type == tea.KeyPgUp {
				m.viewport.LineUp(m.viewport.Height)
				return m, nil
			}
			if msg.Type == tea.KeyPgDown {
				m.viewport.LineDown(m.viewport.Height)
				return m, nil
			}
			if msg.Type == tea.KeyCtrlU {
				m.viewport.LineUp(m.viewport.Height / 2)
				return m, nil
			}
			if msg.Type == tea.KeyCtrlD {
				m.viewport.LineDown(m.viewport.Height / 2)
				return m, nil
			}
			if msg.String() == "alt+up" {
				m.viewport.LineUp(1)
				return m, nil
			}
			if msg.String() == "alt+down" {
				m.viewport.LineDown(1)
				return m, nil
			}

			// '?' toggles the inline help overlay
			if msg.String() == "?" && m.textarea.Value() == "" {
				m.showHelp = !m.showHelp
				if m.showHelp {
					m.appendOutput(helpOverlay())
				} else {
					// Remove the last help block (trim until we hit a divider)
					for len(m.outputLines) > 0 && !strings.HasPrefix(m.outputLines[len(m.outputLines)-1], "─") {
						m.outputLines = m.outputLines[:len(m.outputLines)-1]
					}
				}
				m.refreshViewport()
				return m, nil
			}

		case stateStreaming, stateExecutingTool, stateRuntimeInput, stateConfirmingCmd,
			stateConfirmingWrite, stateConfirmingTool, stateInputSudoPassword, stateAnswering:
			return m.handleTurnKey(msg)

		case stateSelecting:
			switch msg.Type {
			case tea.KeyUp, tea.KeyPgUp:
				m.selectionIdx--
				if m.selectionIdx < 0 {
					m.selectionIdx = len(m.selectionItems) - 1
				}
				return m, nil
			case tea.KeyDown, tea.KeyPgDown:
				m.selectionIdx++
				if m.selectionIdx >= len(m.selectionItems) {
					m.selectionIdx = 0
				}
				return m, nil
			case tea.KeyEsc:
				m.appState = stateInput
				m.textarea.Focus()
				return m, nil
			case tea.KeyDelete:
				if m.selectionType == "history" && len(m.selectionItems) > 0 {
					selected := m.selectionItems[m.selectionIdx]
					if m.activeSession != nil && selected.ID == m.activeSession.ID {
						m.appendOutput(errorStyle.Render("✗ Cannot delete the active session."))
						m.refreshViewport()
						return m, nil
					}
					if err := session.Delete(selected.ID); err != nil {
						m.appendOutput(errorStyle.Render("✗ Failed to delete session: " + err.Error()))
					} else {
						m.appendOutput(lipgloss.NewStyle().Foreground(colorGreen).Render("🗑 Deleted session: " + selected.ID))
						m.selectionItems = append(m.selectionItems[:m.selectionIdx], m.selectionItems[m.selectionIdx+1:]...)
						if m.selectionIdx >= len(m.selectionItems) {
							m.selectionIdx = len(m.selectionItems) - 1
						}
						if m.selectionIdx < 0 {
							m.selectionIdx = 0
						}
						if len(m.selectionItems) == 0 {
							m.appState = stateInput
							m.textarea.Focus()
						}
					}
					m.refreshViewport()
					return m, nil
				}
			case tea.KeyEnter:
				if len(m.selectionItems) == 0 {
					m.appState = stateInput
					m.textarea.Focus()
					return m, nil
				}
				selected := m.selectionItems[m.selectionIdx]
				m.appState = stateInput
				m.textarea.Focus()
				return m.handleSelection(selected.ID)
			}
			return m, nil
		}

	// ── Agent turn events ──────────────────────────────────────────────────
	case agentEventMsg:
		return m.handleAgentEvent(msg)

	case summaryProgressMsg, summaryDoneMsg:
		return m.handleSummaryMsg(msg)

	case compactDoneMsg:
		return m.handleCompactDone(msg)

	case agentClosedMsg:
		// The channel closes right after DoneEvent; nothing else to do unless
		// the turn ended without one (shouldn't happen).
		if msg.turnID == m.turnID && m.events != nil {
			return m.finishTurn(agent.DoneEvent{})
		}
		return m, nil

	case renderTickMsg:
		if m.renderPending && m.appState == stateStreaming {
			m.refreshViewportStreaming()
		}
		m.renderPending = false
		return m, nil

	case modelListMsg:
		return m.showModelPicker(msg), nil

	case sessionTitleMsg:
		if string(msg) != "" && m.activeSession != nil {
			m.activeSession.Name = string(msg)
			m.saveSession()
			m.refreshViewport()
		}
		return m, nil
	}

	// ── Delegate to sub-components ─────────────────────────────────────────
	if m.appState == stateInput {
		var taCmd tea.Cmd
		m.textarea, taCmd = m.textarea.Update(msg)
		cmds = append(cmds, taCmd)
		if _, ok := msg.(tea.KeyMsg); ok && m.updateMentions() {
			m.recalculateDimensions()
		}
	} else if m.appState == stateInputSudoPassword {
		var tiCmd tea.Cmd
		m.sudoTextInput, tiCmd = m.sudoTextInput.Update(msg)
		cmds = append(cmds, tiCmd)
	}
	// Only delegate to the viewport when the textarea is NOT focused.
	// This prevents the viewport's built-in key bindings (arrows)
	// from stealing keystrokes meant for the text input.
	keyMsg, isKeyMsg := msg.(tea.KeyMsg)
	allowViewportUpdate := true
	if isKeyMsg && (m.appState == stateInput || m.appState == stateInputSudoPassword) {
		// In input states, block most keys from the viewport so arrow keys/typing don't scroll it.
		// However, allow PgUp/PgDn so the user can still scroll the chat history.
		if keyMsg.Type != tea.KeyPgUp && keyMsg.Type != tea.KeyPgDown {
			allowViewportUpdate = false
		}
	}

	if allowViewportUpdate {
		var vpCmd tea.Cmd
		m.viewport, vpCmd = m.viewport.Update(msg)
		cmds = append(cmds, vpCmd)
	}
	if m.appState == stateStreaming {
		var spCmd tea.Cmd
		m.spinner, spCmd = m.spinner.Update(msg)
		cmds = append(cmds, spCmd)
	}

	return m, tea.Batch(cmds...)
}

// handleSelection processes the interactive list selection.
func (m Model) handleSelection(id string) (Model, tea.Cmd) {
	switch m.selectionType {
	case "model":
		m.activeModel = id
		if m.activeSession != nil {
			m.activeSession.Model = id
			m.saveSession()
		}
		newProvider, err := m.buildProvider(m.activeProvider, id)
		if err != nil {
			m.appendOutput(errorStyle.Render("✗ Failed to switch model: " + err.Error()))
		} else {
			m.agent.SetProvider(newProvider)
			m.appendOutput(lipgloss.NewStyle().Foreground(colorGreen).Render(
				fmt.Sprintf("✓ Model switched to: %s", id)))
		}
		m.refreshViewport()
		return m, nil

	case "history":
		s, err := session.Load(id)
		if err != nil {
			m.appendOutput(errorStyle.Render("✗ Failed to load session: " + err.Error()))
			m.refreshViewport()
			return m, nil
		}
		m.activeSession = s
		m.syncCheckpointSession()
		if s.Provider != "" {
			m.activeProvider = s.Provider
		}
		if s.Model != "" {
			m.activeModel = s.Model
		}
		if newProvider, err := m.buildProvider(m.activeProvider, m.activeModel); err == nil {
			m.agent.SetProvider(newProvider)
		}
		m.loadHistory(s.History, false)
		m.titleGenerated = false
		m.appendOutput(lipgloss.NewStyle().Foreground(colorGreen).Render("Loaded session: " + s.ID))
		return m, nil

	case "provider":
		defaultModel := string(config.DefaultModel(config.ProviderName(id)))
		newProvider, err := m.buildProvider(id, defaultModel)
		if err != nil {
			m.appendOutput(errorStyle.Render("✗ Failed to switch provider: " + err.Error()))
		} else {
			m.agent.SetProvider(newProvider)
			m.activeProvider = id
			m.activeModel = defaultModel
			if m.activeSession != nil {
				m.activeSession.Provider = id
				m.activeSession.Model = defaultModel
				m.saveSession()
			}
			m.appendOutput(lipgloss.NewStyle().Foreground(colorGreen).Render(
				fmt.Sprintf("✓ Switched to provider: %s  model: %s", id, defaultModel)))
		}
		m.refreshViewport()
		return m, nil

	case "persona":
		prompt, ok := config.ResolvePersona(id, m.fileConfig)
		if !ok {
			m.appendOutput(errorStyle.Render(fmt.Sprintf("✗ Unknown persona %q.", id)))
			m.refreshViewport()
			return m, nil
		}
		m.defaultSystemPrompt = prompt
		m.activePersona = id
		if len(m.history) > 0 && m.history[0].Role == llm.RoleSystem {
			m.history[0].Content = prompt
		} else {
			m.history = append([]llm.Message{{Role: llm.RoleSystem, Content: prompt}}, m.history...)
		}
		m.saveSession()
		m.appendOutput(lipgloss.NewStyle().Foreground(colorGreen).Render(
			fmt.Sprintf("✓ Persona switched to %q (effective next message)", id)))
		m.refreshViewport()
		return m, nil
	}
	return m, nil
}

// generateSessionTitleCmd calls the LLM in the background to generate a summary title.
func (m Model) generateSessionTitleCmd() tea.Cmd {
	var userQuery string
	for _, msg := range m.history {
		if msg.Role == llm.RoleUser {
			userQuery, _ = mention.Split(msg.Content) // not the attached files
			break
		}
	}
	if userQuery == "" {
		return nil
	}

	provider := m.agent.Provider()
	if provider == nil {
		return nil
	}
	return func() tea.Msg {
		// Large hosted models (e.g. on NIM) can take well over 10s before the
		// first token, so allow a generous budget.
		ctx, cancel := context.WithTimeout(context.Background(), titleTimeout)
		defer cancel()

		prompt := fmt.Sprintf("Summarize the following topic/query into a short, concise conversation title of 3-5 words. Return ONLY the title, with no quotation marks, no markdown formatting, and no extra text:\n\n%s", userQuery)

		tokenChan := make(chan string, 100)
		errCh := make(chan error, 1)
		go func() {
			_, _, err := provider.StreamChat(ctx, []llm.Message{
				{Role: llm.RoleUser, Content: prompt},
			}, nil, tokenChan)
			errCh <- err
		}()

		var sb strings.Builder
		for token := range tokenChan {
			if !strings.HasPrefix(token, "\x00") { // skip native reasoning
				sb.WriteString(token)
			}
		}
		if err := <-errCh; err != nil {
			logger.L.Warn("title generation failed, using fallback title", "error", err)
		}

		title := cleanTitle(sb.String(), userQuery)
		logger.L.Info("generated session title", "title", title)
		return sessionTitleMsg(title)
	}
}

// titleTimeout bounds background session-title generation.
const titleTimeout = 60 * time.Second

// cleanTitle normalizes a generated title. If the model returned nothing
// usable (error, timeout, empty), it falls back to the first words of the
// user's query so sessions are never left "Untitled".
func cleanTitle(raw, query string) string {
	text, _, _ := llm.SplitReasoning(raw) // drop inline <think> blocks
	title := strings.TrimSpace(text)
	if i := strings.IndexByte(title, '\n'); i >= 0 {
		title = strings.TrimSpace(title[:i])
	}
	title = strings.Trim(title, "\"`'*#. ")
	if title == "" {
		words := strings.Fields(query)
		if len(words) > 6 {
			words = append(words[:6], "…")
		}
		title = strings.Join(words, " ")
	}
	if len(title) > 60 {
		title = tools.TruncateUTF8(title, 57) + "..."
	}
	if title == "" {
		title = "Untitled"
	}
	return title
}

// ── Internal helpers ──────────────────────────────────────────────────────────

// loadHistory updates the model's history and fully renders it to the viewport display.
// When keepScroll is true, the viewport's current scroll offset is preserved
// (used during window resize). When false, the viewport scrolls to the bottom
// (used when loading a new session).
func (m *Model) loadHistory(history []llm.Message, keepScroll bool) {
	m.outputLines = nil
	m.history = history
	m.plan = agent.PlanFromHistory(history)

	for i, msg := range m.history {
		if msg.Role == llm.RoleSystem {
			if i == 0 {
				// Initial system prompt is not rendered in output
				continue
			}
			if strings.HasPrefix(msg.Content, agent.TurnCompactedPrefix) {
				m.outputLines = append(m.outputLines,
					systemLabelStyle.Render("📦 Earlier progress on this request (compacted)"),
					lipgloss.NewStyle().Foreground(colorSubtext).Render(strings.TrimSpace(strings.TrimPrefix(msg.Content, agent.TurnCompactedPrefix))),
					m.convDivider(),
				)
				continue
			}
			// Command execution feedback from sandboxing
			m.outputLines = append(m.outputLines,
				systemLabelStyle.Render("⚙ System Context (Execution Feedback):"),
				systemContentStyle.Render(msg.Content),
				m.convDivider(),
			)
			continue
		}

		if msg.Role == llm.RoleUser && strings.HasPrefix(msg.Content, agent.CompactedPrefix) {
			m.outputLines = append(m.outputLines,
				systemLabelStyle.Render("📦 Earlier conversation (compacted)"),
				lipgloss.NewStyle().Foreground(colorSubtext).Render(strings.TrimSpace(strings.TrimPrefix(msg.Content, agent.CompactedPrefix))),
				m.convDivider(),
			)
			continue
		}
		if msg.Role == llm.RoleUser {
			text, attached := mention.Split(msg.Content)
			label := userLabelStyle.Render("You")
			m.outputLines = append(m.outputLines,
				label,
				lipgloss.NewStyle().Foreground(colorText).Render(text),
			)
			for _, p := range attached {
				m.outputLines = append(m.outputLines, lipgloss.NewStyle().Foreground(colorSubtext).Render("📎 "+p))
			}
			m.outputLines = append(m.outputLines, m.convDivider())
		} else if msg.Role == llm.RoleAssistant {
			if msg.Content != "" || msg.ReasoningContent != "" {
				label := assistantLabelStyle.Render("aig")
				m.outputLines = append(m.outputLines, label)

				if msg.ReasoningContent != "" {
					m.outputLines = append(m.outputLines, thinkingStyle.Render(strings.TrimSpace(msg.ReasoningContent)))
				}

				if msg.Content != "" {
					rendered, err := m.renderer.Render(msg.Content)
					if err != nil {
						rendered = msg.Content
					}
					m.outputLines = append(m.outputLines, strings.TrimRight(rendered, "\n"))
				}
				m.outputLines = append(m.outputLines, m.convDivider())
			}
			// If there are tool calls in this assistant turn, render the completed ones.
			for _, call := range msg.ToolCalls {
				// Look ahead in history to see if there is a corresponding tool result.
				hasResult := false
				for _, searchMsg := range history {
					if searchMsg.Role == llm.RoleTool && searchMsg.ToolCallID == call.ID {
						hasResult = true
						break
					}
				}
				if hasResult {
					// Render the tool call start line.
					if call.Name == "update_plan" {
						if items, ok := planArgs(call); ok {
							m.outputLines = append(m.outputLines, planLines(items, "")...)
						}
					} else if call.Name == "ask_user" {
						q, _ := call.Args["question"].(string)
						m.outputLines = append(m.outputLines, confirmStyle.Render("❓ "+q))
					} else if call.Name == "run_command" {
						command, _ := call.Args["command"].(string)
						m.outputLines = append(m.outputLines, lipgloss.NewStyle().Foreground(colorYellow).Render(
							fmt.Sprintf("⚙ Tool: run_command: %s", command)))
					} else if tools.IsWriteTool(call.Name) {
						path, _ := call.Args["path"].(string)
						icon := toolIcon(call.Name)
						m.outputLines = append(m.outputLines, lipgloss.NewStyle().Foreground(colorYellow).Bold(true).Render(
							fmt.Sprintf("%s %s: %s", icon, call.Name, path)))
					} else {
						m.outputLines = append(m.outputLines, lipgloss.NewStyle().Foreground(colorYellow).Render(
							fmt.Sprintf("⚙ Tool: %s", call.Name)))
					}
				}
			}
		} else if msg.Role == llm.RoleTool {
			if strings.HasPrefix(msg.Content, "Error:") {
				m.outputLines = append(m.outputLines, errorStyle.Render(fmt.Sprintf("✗ Tool %s failed", msg.ToolName)))
				if msg.ToolName == "run_command" || msg.ToolName == "run_checks" || msg.ToolName == "explore" {
					m.outputLines = append(m.outputLines, errorStyle.Render(msg.Content))
				}
			} else if msg.ToolName == "ask_user" {
				m.outputLines = append(m.outputLines, lipgloss.NewStyle().Foreground(colorSubtext).Render("  ↳ "+strings.TrimPrefix(msg.Content, "The user answered: ")))
			} else if msg.ToolName != "update_plan" {
				m.outputLines = append(m.outputLines, lipgloss.NewStyle().Foreground(colorGreen).Render(fmt.Sprintf("✓ Tool %s done", msg.ToolName)))
				if msg.ToolName == "run_command" || msg.ToolName == "run_checks" || msg.ToolName == "explore" {
					m.outputLines = append(m.outputLines, execResultStyle.Render(msg.Content))
				}
			}
			// Add divider if next is a user message
			nextIsUser := false
			if i+1 < len(m.history) && m.history[i+1].Role == llm.RoleUser {
				nextIsUser = true
			}
			if nextIsUser {
				m.outputLines = append(m.outputLines, m.convDivider())
			}
		}
	}

	// Re-add in-flight turn state (streaming text, pending approval, …).
	if m.restoreTurnOutput() {
		return
	}

	if keepScroll {
		m.refreshViewportKeepPosition()
	} else {
		m.refreshViewport()
	}
}

func (m *Model) getExecutionIndicator(cmd string) (string, string) {
	cmdLower := strings.ToLower(cmd)
	isSudo := sandbox.UsesSudo(cmd)
	isDownload := false
	downloadKeywords := []string{
		"curl", "wget", "git clone", "apt-get install", "apt install",
		"npm install", "npm i ", "pip install", "go get", "docker pull",
		"yarn add", "wget ", "download",
	}
	for _, kw := range downloadKeywords {
		if strings.Contains(cmdLower, kw) {
			isDownload = true
			break
		}
	}
	if isDownload {
		if isSudo {
			return "📥", "downloading & installing with sudo…"
		}
		return "📥", "downloading…"
	}
	if isSudo {
		return "🔒", "running with sudo…"
	}
	return "⚙", "running command…"
}

// saveSession persists the active session history to disk.
func (m *Model) saveSession() {
	m.updateTokenEstimate()
	if m.activeSession == nil {
		return
	}
	m.activeSession.History = m.history
	if err := session.Save(m.activeSession); err != nil {
		logger.L.Error("failed to auto-save session", "id", m.activeSession.ID, "error", err)
	}
}

// submitPrompt adds the user's message to history and renders it in the viewport.
func (m Model) submitPrompt(input string) Model {
	m.stickBottom = true
	cwd, _ := os.Getwd()
	content, atts := mention.Expand(input, cwd)
	m.history = append(m.history, llm.Message{
		Role:    llm.RoleUser,
		Content: content,
	})
	m.saveSession()

	label := userLabelStyle.Render("You")
	m.outputLines = append(m.outputLines,
		label,
		lipgloss.NewStyle().Foreground(colorText).Render(input),
	)
	for _, a := range atts {
		if a.Err != nil {
			m.outputLines = append(m.outputLines, lipgloss.NewStyle().Foreground(colorYellow).Render("⚠ "+a.Label()))
		} else {
			m.outputLines = append(m.outputLines, lipgloss.NewStyle().Foreground(colorSubtext).Render("📎 "+a.Label()))
		}
	}
	m.outputLines = append(m.outputLines, m.convDivider())
	return m
}

// refreshViewport re-renders all output lines into the viewport and scrolls
// to the bottom. Use this when new content has been appended (e.g. a new
// message, command output, tool result).
func (m *Model) refreshViewport() {
	follow := m.stickBottom || m.viewport.AtBottom()
	m.stickBottom = false
	m.viewport.SetContent(strings.Join(m.outputLines, "\n"))
	if follow {
		m.viewport.GotoBottom()
	}
}

// refreshViewportKeepPosition re-renders output lines into the viewport while
// preserving the current scroll offset. Use this during resize and re-layout
// operations where no new content was added, so the user's reading position
// is not lost.
func (m *Model) refreshViewportKeepPosition() {
	prevOffset := m.viewport.YOffset
	content := strings.Join(m.outputLines, "\n")
	m.viewport.SetContent(content)
	// Clamp the offset so it doesn't exceed the new content length.
	maxOffset := m.viewport.TotalLineCount() - m.viewport.VisibleLineCount()
	if maxOffset < 0 {
		maxOffset = 0
	}
	if prevOffset > maxOffset {
		prevOffset = maxOffset
	}
	m.viewport.SetYOffset(prevOffset)
}

// appendOutput adds a rendered line to the output history.
func (m *Model) appendOutput(line string) {
	m.outputLines = append(m.outputLines, line)
}

// helpOverlay returns a formatted help block to append to outputLines.
func helpOverlay() string {
	header := lipgloss.NewStyle().Foreground(colorAccent).Bold(true).Render("❓ Keyboard Shortcuts & Commands")
	lines := []string{
		header,
		lipgloss.NewStyle().Foreground(colorAccent).Render("  Shortcuts"),
		"  Enter           Send message",
		"  Shift+Enter     Insert newline",
		"  Ctrl+C          Cancel the running turn / quit",
		"  Ctrl+L          Clear viewport",
		"  ?               Toggle this help",
		lipgloss.NewStyle().Foreground(colorAccent).Render("  Session Commands"),
		"  /history        List saved sessions",
		"  /load <id>      Resume session",
		"  /save <name>    Rename session",
		"  /delete <id>    Delete session",
		"  /map [dir]      Map workspace into context",
		"  /new            Fresh session",
		"  /clear          Clear viewport",
		"  /quit           Exit",
		lipgloss.NewStyle().Foreground(colorAccent).Render("  Runtime Switching"),
		"  /model          List models for current provider",
		"  /model <id>     Switch model (keeps history)",
		"  /provider       List providers",
		"  /provider <n>   Switch provider (keeps history)",
		"  /persona        List personas",
		"  /persona <n>    Switch system prompt persona",
		lipgloss.NewStyle().Foreground(colorAccent).Render("  Agent Execution"),
		"  Shift+Tab           Cycle mode: ask → accept edits → auto → plan",
		"  /mode <m>           Set mode: ask | edits | auto | plan",
		lipgloss.NewStyle().Foreground(colorAccent).Render("  Safety net"),
		"  /undo, /redo        Revert / re-apply the last turn's changes",
		"  /diff               Changes since the session's first checkpoint",
		"  /checkpoints        List checkpoints",
		"  /plan [task]        Plan mode (read-only), then /go to implement",
		lipgloss.NewStyle().Foreground(colorAccent).Render("  Project index"),
		"  /summarize          Preview; /summarize go builds the project summary",
		"  /compact            Summarize older turns to free context",
		lipgloss.NewStyle().Foreground(colorAccent).Render("  Files"),
		"  @path           Attach a file / dir (@f.go:10-40 a range); Tab completes",
	}
	return strings.Join(lines, "\n")
}

// newRenderer builds the markdown renderer; wrap 0 disables wrapping.
func newRenderer(wrap int) (*glamour.TermRenderer, error) {
	return glamour.NewTermRenderer(
		glamour.WithStandardStyle("dark"),
		glamour.WithWordWrap(max(wrap, 0)),
	)
}
