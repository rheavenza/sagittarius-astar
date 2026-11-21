// Command aig — Sagittarius A* terminal AI agent.
//
// Usage:
//
//	aig [flags]
//	aig --provider gemini --model gemini-2.0-flash
//	aig --provider deepseek --model deepseek-chat
//	aig --provider nim --model meta/llama-3.3-70b-instruct
//	aig --persona sysadmin         # use custom prompt persona
//	aig --resume 20260521-120000   # resume previous session
//	aig --log-level debug          # verbose file logging
//	aig --version                  # print version and exit
package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/clitorhea/sagittarius-astar.git/internal/agent"
	"github.com/clitorhea/sagittarius-astar.git/internal/checkpoint"
	"github.com/clitorhea/sagittarius-astar.git/internal/config"
	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
	"github.com/clitorhea/sagittarius-astar.git/internal/logger"
	"github.com/clitorhea/sagittarius-astar.git/internal/mcp"
	"github.com/clitorhea/sagittarius-astar.git/internal/prompt"
	"github.com/clitorhea/sagittarius-astar.git/internal/repomap"
	"github.com/clitorhea/sagittarius-astar.git/internal/session"
	"github.com/clitorhea/sagittarius-astar.git/internal/tools"
	"github.com/clitorhea/sagittarius-astar.git/internal/tui"
)

var (
	flagProvider string
	flagModel    string
	flagPersona  string
	flagResume   string
	flagLogLevel string
	flagLogFile  string

	version = "dev" // Injected by LDFLAGS during build
)

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

var rootCmd = &cobra.Command{
	Use:   "aig",
	Short: "aig — terminal-based AI agent (Sagittarius A*)",
	Long: `aig is a cross-platform, terminal-based AI agent that supports
multiple LLM providers (Gemini, DeepSeek, NVIDIA NIM) and can securely execute
shell commands with your explicit consent.

Configuration file is located at: ~/.config/aig/config.json
Sessions are saved to: ~/.local/share/aig/sessions/
Logs are written to: ~/.cache/aig/aig.log

Examples:
  aig
  aig --provider deepseek
  aig --provider nim --model meta/llama-3.3-70b-instruct   # NVIDIA_API_KEY
  aig --persona sysadmin
  aig --resume 20260521-170000
  aig --log-level debug
  aig --version`,
	// aig takes no positional arguments. Without this, a typo or an unknown
	// subcommand (e.g. "aig foo") silently started the full TUI.
	Args: cobra.NoArgs,
	RunE: runChat,
	// Runtime errors are printed once by main(); usage only for flag errors.
	SilenceUsage:  true,
	SilenceErrors: true,
	// Enables --version; the value is injected by LDFLAGS (see Makefile),
	// "dev" otherwise. `aig version` is a subcommand below.
	Version: version,
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the aig version",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, _ []string) {
		fmt.Fprintf(cmd.OutOrStdout(), "aig version %s\n", version)
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
	rootCmd.PersistentFlags().StringVarP(
		&flagProvider, "provider", "p", "",
		"LLM provider to use (gemini | deepseek | nim)",
	)
	rootCmd.PersistentFlags().StringVarP(
		&flagModel, "model", "m", "",
		"Model name (defaults to provider's recommended model)",
	)
	rootCmd.PersistentFlags().StringVarP(
		&flagPersona, "persona", "s", "",
		"System prompt persona to use (defaults to coder, or default_persona from config)",
	)
	rootCmd.PersistentFlags().StringVarP(
		&flagResume, "resume", "r", "",
		"Resume a previous conversation by session ID (defaults to 'latest' if flag is present but no ID is provided)",
	)
	rootCmd.PersistentFlags().Lookup("resume").NoOptDefVal = "latest"
	rootCmd.PersistentFlags().StringVar(
		&flagLogLevel, "log-level", "info",
		"Log verbosity written to file (debug | info | warn | error)",
	)
	rootCmd.PersistentFlags().StringVar(
		&flagLogFile, "log-file", "",
		"Override log file path (default: ~/.cache/aig/aig.log)",
	)
}

func runChat(_ *cobra.Command, _ []string) error {
	// The TUI needs a real terminal. Without one (e.g. when aig itself is
	// run by an agent's shell tool) it would hang until killed.
	if !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		return fmt.Errorf("aig needs an interactive terminal (stdin and stdout must be a TTY); use 'aig --version' or 'aig --help' from scripts")
	}

	// ── Logging ──────────────────────────────────────────────────────────────
	logLevel, err := logger.ParseLevel(flagLogLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v — defaulting to info\n", err)
		logLevel = slog.LevelInfo
	}

	cleanupLog, err := logger.Init(logLevel, flagLogFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: logging disabled: %v\n", err)
	}
	defer cleanupLog()

	// Build the repo map in the background while config, consent prompts
	// and MCP servers start up.
	repoMapSection := startRepoMap()

	// ── Local ./aig.md prompt (requires one-time consent per content hash) ───
	if flagPersona == "" {
		flagPersona = resolveLocalPrompt()
	}

	// ── Configuration ─────────────────────────────────────────────────────────
	cfg, err := config.Load(config.ProviderName(flagProvider), flagModel, flagPersona)
	if err != nil {
		logger.L.Error("config load failed", "error", err)
		return fmt.Errorf("configuration error: %w\n\nRun 'aig --help' for usage", err)
	}

	// ── Session Resolution ───────────────────────────────────────────────────
	var activeSession *session.Session
	if flagResume != "" {
		if flagResume == "latest" {
			activeSession, err = session.LoadLatest()
			if err != nil {
				logger.L.Error("failed to load latest session", "error", err)
				return fmt.Errorf("failed to resume latest session: %w", err)
			}
		} else {
			activeSession, err = session.Load(flagResume)
			if err != nil {
				logger.L.Error("failed to load session to resume", "id", flagResume, "error", err)
				return fmt.Errorf("failed to resume session %q: %w", flagResume, err)
			}
		}
		logger.L.Info("resuming session", "id", activeSession.ID, "turns", len(activeSession.History))

		// Override provider and model from session if not explicitly requested on CLI
		if flagProvider == "" && activeSession.Provider != "" {
			flagProvider = activeSession.Provider
		}
		if flagModel == "" && activeSession.Model != "" {
			flagModel = activeSession.Model
		}
		// Re-load configuration with the updated provider/model
		cfg, err = config.Load(config.ProviderName(flagProvider), flagModel, flagPersona)
		if err != nil {
			logger.L.Error("config reload failed for resumed session", "error", err)
			return fmt.Errorf("configuration error: %w", err)
		}
	} else {
		activeSession = &session.Session{
			ID:        session.GenerateID(),
			Name:      "Untitled",
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
			Provider:  string(cfg.Provider),
			Model:     cfg.Model,
		}
		if cfg.SystemPrompt != "" {
			activeSession.History = []llm.Message{{
				Role:    llm.RoleSystem,
				Content: cfg.SystemPrompt,
			}}
		}
		logger.L.Info("created fresh session", "id", activeSession.ID)
	}

	logger.L.Info("aig starting",
		"provider", cfg.Provider,
		"model", cfg.Model,
		"persona", cfg.Persona,
		"log_file", logger.LogPath,
	)

	// ── Provider ──────────────────────────────────────────────────────────────
	provider, err := llm.NewProvider(string(cfg.Provider), cfg.APIKey, cfg.Model, cfg.BaseURL)
	if err != nil {
		logger.L.Error("provider init failed", "provider", cfg.Provider, "error", err)
		return fmt.Errorf("failed to initialize provider: %w", err)
	}
	logger.L.Debug("provider initialised", "provider", cfg.Provider, "model", cfg.Model)

	// ── MCP Server Bootstrap ─────────────────────────────────────────────────────────────────
	// Spawn MCP servers declared in the config's mcp_servers map.
	// Failures are non-fatal: aig degrades gracefully to native-tools-only mode.
	var mcpRegistry *mcp.Registry
	fc := config.LoadFileConfig()
	if len(fc.MCPServers) > 0 {
		mcpMgr := mcp.NewManager()

		// Convert config.MCPServerConfig → mcp.ServerConfig (identical fields; kept
		// separate to prevent a config → mcp import cycle in the config package).
		serverConfigs := make(map[string]mcp.ServerConfig, len(fc.MCPServers))
		for name, sc := range fc.MCPServers {
			serverConfigs[name] = mcp.ServerConfig{
				Command: sc.Command,
				Args:    sc.Args,
				Env:     sc.Env,
				Trusted: sc.Trusted,
			}
		}

		// Use context.Background() for subprocess lifetime — the processes must
		// live for the entire session, not just the 15-second discovery window.
		if err := mcpMgr.Start(context.Background(), serverConfigs); err != nil {
			logger.L.Warn("MCP: failed to start servers, continuing without MCP tools", "error", err)
		} else {
			mcpRegistry = mcp.NewRegistry(mcpMgr)

			// Short-lived context just for the RPC handshake + tools/list calls.
			discoverCtx, discoverCancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer discoverCancel()

			if err := mcpRegistry.Discover(discoverCtx); err != nil {
				logger.L.Warn("MCP: tool discovery failed, continuing without MCP tools", "error", err)
				mcpRegistry.Shutdown()
				mcpMgr.Shutdown(context.Background())
				mcpRegistry = nil
			} else {
				logger.L.Info("MCP: tools registered",
					"servers", len(fc.MCPServers),
					"tools", mcpRegistry.ToolCount(),
				)
				// Shut down registry clients before the manager closes the pipes.
				defer func() {
					mcpRegistry.Shutdown()
					shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer shutCancel()
					mcpMgr.Shutdown(shutCtx)
				}()
			}
		}
	} else {
		logger.L.Debug("MCP: no servers configured, skipping bootstrap")
	}

	// ── Agent ───────────────────────────────────────────────────────────────────────────────
	cwd, _ := os.Getwd()
	env := prompt.DetectEnv(cwd)
	projectChecks, checksSection := loadProjectChecks(cwd, env.GitRoot)
	checkpoints := checkpoint.New(cwd, activeSession.ID)
	logger.L.Info("checkpoints enabled", "mode", checkpoints.Mode())
	ag := agent.New(agent.Options{
		Checkpoints:    checkpoints,
		Provider:       provider,
		MCP:            mcpRegistry,
		WorkspaceRoot:  cwd,
		ContextBlock:   buildContextBlock(cwd, env, checksSection, repoMapSection()),
		PostEditChecks: fc.PostEditChecks,
		ProjectChecks:  projectChecks,
	})

	if fc.PermissionMode != "" {
		if strings.EqualFold(fc.PermissionMode, "plan") {
			ag.SetPlanMode(true)
		} else if mode, ok := agent.ParseMode(fc.PermissionMode); ok {
			ag.SetMode(mode)
		} else {
			logger.L.Warn("unknown permission_mode in config, using default", "value", fc.PermissionMode)
		}
	}

	// ── TUI ─────────────────────────────────────────────────────────────────────────────────
	model, err := tui.NewModel(ag, activeSession, cfg.SystemPrompt, version, string(cfg.Provider), cfg.Model, cfg.Persona)
	if err != nil {
		logger.L.Error("TUI init failed", "error", err)
		return fmt.Errorf("failed to initialize TUI: %w", err)
	}
	model.SetCheckpoints(checkpoints)

	p := tea.NewProgram(
		model,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)

	logger.L.Debug("entering TUI event loop")
	if _, err := p.Run(); err != nil {
		logger.L.Error("TUI exited with error", "error", err)
		return fmt.Errorf("TUI error: %w", err)
	}

	logger.L.Info("aig exited cleanly")
	return nil
}

// resolveLocalPrompt returns "aig.md" if a ./aig.md exists and the user has
// approved its current content (now or previously); otherwise "".
func resolveLocalPrompt() string {
	info, err := os.Stat(config.LocalPromptFile)
	if err != nil || info.IsDir() {
		return ""
	}
	if askTrust(config.LocalPromptFile, "It would REPLACE your system prompt") {
		return config.LocalPromptFile
	}
	return ""
}

// buildContextBlock loads instruction files (AGENTS.md, CLAUDE.md,
// .aig/instructions.md in cwd and the git root, plus the user's global
// ~/.config/aig/AGENTS.md) and joins them with the environment and the
// project checks section. Repo files need consent.
func buildContextBlock(cwd string, env prompt.Env, sections ...string) string {

	var files []prompt.InstructionFile
	if g := prompt.GlobalInstructionFile(filepath.Dir(config.Path())); g != "" {
		if f, err := prompt.LoadInstructionFile(g, true); err == nil {
			files = append(files, f)
		}
	}
	for _, p := range prompt.FindInstructionFiles(cwd, env.GitRoot) {
		if !askTrust(p, "It will be added to the agent's instructions") {
			continue
		}
		if f, err := prompt.LoadInstructionFile(p, false); err == nil {
			files = append(files, f)
		}
	}
	logger.L.Info("context block built", "git_root", env.GitRoot, "instruction_files", len(files))
	return prompt.BuildContextBlock(env, files, sections...)
}

// repoMapStartupTimeout bounds the startup repo map; on a huge repo the
// prompt simply goes without it (the repo_map tool still works).
const repoMapStartupTimeout = 3 * time.Second

// startRepoMap builds the system-prompt repository map in the background
// (git repos only, so aig started in $HOME doesn't crawl it) and returns a
// function that waits for the rendered section ("" if off, failed or slow).
func startRepoMap() func() string {
	budget := config.LoadFileConfig().RepoMapTokens
	if budget < 0 {
		return func() string { return "" }
	}
	if budget == 0 {
		budget = 1024
	}
	cwd, err := os.Getwd()
	if err != nil {
		return func() string { return "" }
	}
	ch := make(chan string, 1)
	start := time.Now()
	go func() {
		root := gitRoot(cwd)
		if root == "" {
			ch <- ""
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), repoMapStartupTimeout)
		defer cancel()
		m, err := repomap.Build(ctx, root)
		if err != nil {
			logger.L.Warn("repo map skipped", "root", root, "error", err)
			ch <- ""
			return
		}
		files, syms := m.Stats()
		logger.L.Info("repo map built", "root", root, "files", files, "symbols", syms, "took", time.Since(start))
		ch <- prompt.RenderRepoMap(root, m.Render(repomap.Options{MaxTokens: budget}))
	}()
	return func() string {
		select {
		case s := <-ch:
			return s
		case <-time.After(repoMapStartupTimeout + time.Second):
			return ""
		}
	}
}

func gitRoot(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// loadProjectChecks reads .aig/config.json (cwd, then git root) after
// consent and returns its checks plus the "Definition of done" prompt section.
// A broken file is reported on stderr and ignored.
func loadProjectChecks(cwd, gitRoot string) (tools.ProjectChecks, string) {
	path := config.FindProjectConfig(cwd, gitRoot)
	if path == "" {
		return tools.ProjectChecks{}, ""
	}
	pc, err := config.LoadProjectConfig(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aig: ignoring project config: %v\n", err)
		logger.L.Warn("invalid project config", "path", path, "error", err)
		return tools.ProjectChecks{}, ""
	}
	if len(pc.Checks) == 0 {
		return tools.ProjectChecks{}, ""
	}
	if !askTrust(path, "The agent will run its checks as shell commands") {
		return tools.ProjectChecks{}, ""
	}
	out := tools.ProjectChecks{Root: pc.Root, Timeout: time.Duration(pc.ChecksTimeoutSeconds) * time.Second}
	var section []prompt.Check
	for _, c := range pc.Checks {
		out.Checks = append(out.Checks, tools.ProjectCheck{Name: c.Name, Command: c.Command})
		section = append(section, prompt.Check{Name: c.Name, Command: c.Command})
	}
	logger.L.Info("project checks loaded", "path", path, "checks", len(out.Checks))
	return out, prompt.RenderChecks(path, section)
}

// askTrust returns true if path was previously approved with its current
// content, or the user approves it now on the terminal (before the TUI
// starts). Approval is remembered by content hash; edits invalidate it.
//
// Any repository can ship such files, so they are never used silently.
func askTrust(path, effect string) bool {
	if config.IsPromptTrusted(path) {
		return true
	}
	// Only ask when we can actually talk to a human.
	if st, err := os.Stdin.Stat(); err != nil || st.Mode()&os.ModeCharDevice == 0 {
		logger.L.Warn("ignoring untrusted prompt file (no terminal to ask for consent)", "path", path)
		return false
	}

	data, _ := os.ReadFile(path)
	preview := strings.TrimSpace(string(data))
	if len(preview) > 400 {
		preview = strings.ToValidUTF8(preview[:400], "") + "\n…"
	}
	fmt.Fprintf(os.Stderr, "\nFound %s (%d bytes). %s:\n\n%s\n\nUse it and remember this version? [y/N] ",
		path, len(data), effect, preview)

	answer, _ := stdinReader.ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))
	if answer != "y" && answer != "yes" {
		fmt.Fprintf(os.Stderr, "→ ignoring %s for this run.\n", path)
		return false
	}
	if err := config.TrustPrompt(path); err != nil {
		logger.L.Warn("failed to record prompt trust", "path", path, "error", err)
	}
	return true
}

// stdinReader is shared so consecutive prompts don't lose buffered input.
var stdinReader = bufio.NewReader(os.Stdin)

// isTerminal reports whether f is a character device (a TTY).
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
