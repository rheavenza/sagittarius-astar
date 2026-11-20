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
package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/clitorhea/sagittarius-astar.git/internal/config"
	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
	"github.com/clitorhea/sagittarius-astar.git/internal/logger"
	"github.com/clitorhea/sagittarius-astar.git/internal/mcp"
	"github.com/clitorhea/sagittarius-astar.git/internal/session"
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
  aig --log-level debug`,
	RunE: runChat,
}

func init() {
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

	// ── TUI ─────────────────────────────────────────────────────────────────────────────────
	model, err := tui.NewModel(provider, activeSession, cfg.SystemPrompt, version, string(cfg.Provider), cfg.Model, cfg.Persona, mcpRegistry)
	if err != nil {
		logger.L.Error("TUI init failed", "error", err)
		return fmt.Errorf("failed to initialize TUI: %w", err)
	}

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
//
// Any repository could ship an aig.md, so it is never used silently: the
// first time (and whenever its content changes) the user is asked on the
// terminal before the TUI starts.
func resolveLocalPrompt() string {
	info, err := os.Stat(config.LocalPromptFile)
	if err != nil || info.IsDir() {
		return ""
	}
	if config.IsPromptTrusted(config.LocalPromptFile) {
		return config.LocalPromptFile
	}

	// Only ask when we can actually talk to a human.
	if st, err := os.Stdin.Stat(); err != nil || st.Mode()&os.ModeCharDevice == 0 {
		logger.L.Warn("ignoring untrusted ./aig.md (no terminal to ask for consent)")
		return ""
	}

	data, _ := os.ReadFile(config.LocalPromptFile)
	preview := strings.TrimSpace(string(data))
	if len(preview) > 400 {
		preview = strings.ToValidUTF8(preview[:400], "") + "\n…"
	}
	fmt.Fprintf(os.Stderr, "Found ./%s (%d bytes). It would replace your system prompt:\n\n%s\n\nUse it and remember this version? [y/N] ",
		config.LocalPromptFile, len(data), preview)

	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))
	if answer != "y" && answer != "yes" {
		fmt.Fprintln(os.Stderr, "→ ignoring ./aig.md for this run.")
		return ""
	}
	if err := config.TrustPrompt(config.LocalPromptFile); err != nil {
		logger.L.Warn("failed to record aig.md trust", "error", err)
	}
	return config.LocalPromptFile
}
